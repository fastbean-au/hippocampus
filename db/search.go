package db

import (
	"context"
	"errors"
	"strings"
	"unicode"

	log "github.com/sirupsen/logrus"
)

// ErrContentSearchUnavailable is returned by SearchMemoryIds when this database cannot answer
// content searches - every driver but SQLite, and a read-only tool open. Callers map it to a
// FailedPrecondition rather than an empty result, so an operator who expected search to work is
// told it is not available on their driver instead of concluding their store is empty.
var ErrContentSearchUnavailable = errors.New("content search is not available on this storage driver")

// Content search lets SearchMemories work without an OpenSearch cluster. It is a secondary index
// like the OpenSearch one - the memories table stays the sole system of record, and callers
// re-read every hit from it - but it differs from that one in two ways worth being precise about,
// because they change what can go wrong:
//
//   - It is maintained SYNCHRONOUSLY, on the same connection as the write it belongs to. There is
//     no queue, so there is no overflow to drop operations, and no worker, so there is no
//     ordering hazard. What it is not is transactional on the write path: the index statement runs
//     after the memories INSERT/UPDATE rather than inside it, so a failure between the two leaves
//     a memory that is not findable by content. That is logged and never fails the write - the
//     index is rebuildable (--backfill-search) and the primary row is what matters.
//   - Deletes ARE transactional, and cost nothing at any call site, because they are handled by an
//     AFTER DELETE trigger on memories keyed on the rowid. That one trigger covers every deletion
//     path there is - consolidation, eviction, DeleteMemories, DeleteEventMemories, Purge, Clear,
//     and an import that replaces a row - so unlike the OpenSearch index, which needs a delete
//     observer plus RPC-layer hooks to stay in step, this one cannot drift on deletion at all.
//
// SQLite only, for now. The virtual table is FTS5, which modernc.org/sqlite is built with
// (SQLITE_ENABLE_FTS5), so this adds no dependency and does not need cgo. Postgres (tsvector) and
// MySQL (FULLTEXT) are deliberately not implemented yet: ContentSearchAvailable reports false on
// them and the RPC surfaces that as a clear FailedPrecondition rather than silently returning
// nothing. See TODO item 56.1.
//
// The index is CONTENTLESS (content='', contentless_delete=1): it holds the inverted index and
// not a second copy of the body. That is deliberate and not merely a size optimisation - storing
// the text again would give back much of what body compression was added to save, on a product
// whose whole purpose is managing a finite store. It also rules out the obvious alternative, an
// external-content table over memories.body, for a harder reason: since compression landed, that
// column can hold a gzip stream, so an index reading it directly would tokenise binary. Every
// write below therefore feeds the index the PLAIN body, from inside the storage boundary and
// before compressBody ever sees it.

// contentSearchTable is the FTS5 virtual table backing content search, and contentSearchTrigger
// the AFTER DELETE trigger that keeps it in step with the memories table. The virtual table's
// rowid is the memories row's own rowid, which is what lets the trigger work on OLD.rowid alone
// and never need the body.
const (
	contentSearchTable   = "memories_fts"
	contentSearchTrigger = "memories_fts_delete"
)

// contentSearchDDL creates the FTS5 index and its delete trigger. Both are IF NOT EXISTS, so this
// runs on every startup and only does work the first time - the same shape as the rest of
// initSchema.
//
// An existing store gains an EMPTY index here: the virtual table is created but holds nothing for
// the memories already in the table, so those memories are not findable by content until the
// index is populated. initSchema calls backfillContentSearch immediately after this for exactly
// that reason.
const contentSearchDDL = `
CREATE VIRTUAL TABLE IF NOT EXISTS ` + contentSearchTable + ` USING fts5(
	body,
	content='',
	contentless_delete=1
);

CREATE TRIGGER IF NOT EXISTS ` + contentSearchTrigger + ` AFTER DELETE ON memories BEGIN
	DELETE FROM ` + contentSearchTable + ` WHERE rowid = OLD.rowid;
END;
`

// ContentQuery carries the parameters of one content search. Text is required; EventId and Group
// restrict matches when non-empty. It mirrors search.Query, but is declared here so the db package
// takes no dependency on the search package (which depends on this one).
type ContentQuery struct {
	Text    string
	EventId string
	Group   string
	Limit   int
}

// ContentHit is one match: the memory's id and its relevance, with the sign flipped from FTS5's
// convention so that higher is more relevant (see SearchMemoryHits).
type ContentHit struct {
	Id    string
	Score float64
}

// ContentSearchAvailable reports whether this database can answer content searches. Only the
// SQLite driver can, and only when it is not a read-only tool open (which never runs the DDL that
// creates the index).
func (d *DB) ContentSearchAvailable() bool {
	return d.driver == driverSQLite && !d.readOnly
}

// initContentSearch creates the content-search index and populates it if it is empty but the store
// is not - the upgrade case, where a database written by a version without content search gains
// the index on this startup and would otherwise answer every search with nothing.
func (d *DB) initContentSearch() error {
	log.Trace("func() db.initContentSearch")

	if d.driver != driverSQLite {
		return nil
	}

	if _, err := d.sql.Exec(contentSearchDDL); err != nil {
		log.Errorf("failed to initialise the content search index: %s", err.Error())

		return err
	}

	return d.backfillContentSearch()
}

// backfillContentSearch populates the content-search index from the memories table when the index
// is empty and the table is not. It is deliberately narrow: it does nothing at all on a store
// whose index already has rows, so an ordinary restart pays one COUNT and moves on, and it never
// tries to repair a partially populated index (--backfill-search --reindex is the tool for that).
//
// The guard is "index is empty", not "index is smaller than the table", because those two are
// legitimately different: binary memories are never indexed, so a healthy index is always smaller
// than the table by however many binary memories the store holds.
func (d *DB) backfillContentSearch() error {
	log.Trace("func() db.backfillContentSearch")

	var indexed int

	if err := d.sql.QueryRow(`SELECT count(*) FROM ` + contentSearchTable).Scan(&indexed); err != nil {
		log.Errorf("failed to count the content search index: %s", err.Error())

		return err
	}

	if indexed > 0 {
		return nil
	}

	var stored int

	if err := d.sql.QueryRow(`SELECT count(*) FROM memories WHERE is_binary = 0`).Scan(&stored); err != nil {
		log.Errorf("failed to count indexable memories: %s", err.Error())

		return err
	}

	if stored == 0 {
		return nil
	}

	log.Infof("populating the content search index from %d existing memories", stored)

	if err := d.RebuildContentSearch(context.Background()); err != nil {
		return err
	}

	log.Info("content search index populated")

	return nil
}

// RebuildContentSearch empties the content-search index and repopulates it from the memories
// table. It is what --backfill-search runs on the SQLite driver, and what initContentSearch uses
// to populate a newly created index on an existing store.
//
// Bodies are read (and so decompressed) a page at a time rather than all at once, because the
// whole point of this store is that it may hold more memories than fit comfortably in memory.
func (d *DB) RebuildContentSearch(ctx context.Context) error {
	log.Trace("func() db.RebuildContentSearch")

	if d.driver != driverSQLite {
		return nil
	}

	if _, err := d.exec(ctx, `DELETE FROM `+contentSearchTable); err != nil {
		log.Errorf("failed to clear the content search index: %s", err.Error())

		return err
	}

	const pageSize = 500

	var afterId string

	for {
		memories, err := d.GetIndexableMemoriesPage(ctx, afterId, pageSize)
		if err != nil {
			return err
		}

		if len(memories) == 0 {
			return nil
		}

		for _, memory := range memories {
			if err := d.indexMemoryContent(ctx, memory.Id, memory.Body, memory.IsBinary); err != nil {
				return err
			}
		}

		afterId = memories[len(memories)-1].Id
	}
}

// indexMemoryContent adds a memory's body to the content-search index. It is called by the write
// helpers with the plain body, before compression.
//
// The rowid is resolved by the INSERT ... SELECT itself rather than by LastInsertId, so this works
// unchanged for a create (where the row was just inserted), an update, and an import upsert -
// none of which need to tell it which of those they are.
//
// A binary memory is never indexed: its body is client-encoded and opaque, exactly as on the
// OpenSearch path. That is a skip, not an error.
func (d *DB) indexMemoryContent(ctx context.Context, id string, body string, isBinary bool) error {
	if !d.ContentSearchAvailable() || isBinary {
		return nil
	}

	_, err := d.exec(ctx,
		`INSERT INTO `+contentSearchTable+` (rowid, body) SELECT rowid, ? FROM memories WHERE id = ?`,
		body,
		id,
	)
	if err != nil {
		log.Errorf("failed to index the body of memory '%s' for content search: %s", id, err.Error())
	}

	return err
}

// reindexMemoryContent replaces a memory's entry in the content-search index after its body
// changes. FTS5 has no update for a contentless table, so this is a delete followed by an insert.
//
// The delete is unconditional, and runs even for a binary memory: a memory whose body is replaced
// must not keep matching on its old text, and the cheapest way to be sure of that is not to
// special-case it.
func (d *DB) reindexMemoryContent(ctx context.Context, id string, body string, isBinary bool) error {
	if !d.ContentSearchAvailable() {
		return nil
	}

	_, err := d.exec(ctx,
		`DELETE FROM `+contentSearchTable+` WHERE rowid = (SELECT rowid FROM memories WHERE id = ?)`,
		id,
	)
	if err != nil {
		log.Errorf("failed to clear the content search entry for memory '%s': %s", id, err.Error())

		return err
	}

	return d.indexMemoryContent(ctx, id, body, isBinary)
}

// SearchMemoryHits returns the memories whose body matches the query, most relevant first. Like
// the OpenSearch path it returns ids and relevance only, never bodies: the caller re-reads the
// rows from the primary store, which is what keeps the store authoritative.
//
// Ranking is FTS5's bm25, the same family as the OpenSearch index's, so result order is comparable
// between the two backends rather than arbitrarily different. The score's SIGN is flipped here:
// FTS5's rank is negative and more negative is better, which is the opposite of every other
// scoring convention including OpenSearch's, so the backend boundary is the right place to settle
// it once (see search.Hit).
func (d *DB) SearchMemoryHits(ctx context.Context, query ContentQuery) ([]ContentHit, error) {
	log.Trace("func() db.SearchMemoryHits")

	if !d.ContentSearchAvailable() {
		return nil, ErrContentSearchUnavailable
	}

	match := ftsMatchExpression(query.Text)

	// Every token was punctuation or otherwise dropped by the tokeniser, so there is nothing that
	// could match. Returning empty is right, and it avoids handing FTS5 an empty MATCH, which is a
	// syntax error rather than an empty result.
	if match == "" {
		return nil, nil
	}

	clauses := []string{contentSearchTable + ` MATCH ?`}
	args := []any{match}

	if query.EventId != "" {
		clauses = append(clauses, `m.event_id = ?`)
		args = append(args, query.EventId)
	}

	if query.Group != "" {
		clauses = append(clauses, `m.group_name = ?`)
		args = append(args, query.Group)
	}

	limit := query.Limit
	if limit <= 0 {
		limit = 10
	}

	args = append(args, limit)

	rows, err := d.query(ctx,
		`SELECT m.id, `+contentSearchTable+`.rank FROM `+contentSearchTable+`
		JOIN memories m ON m.rowid = `+contentSearchTable+`.rowid
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY `+contentSearchTable+`.rank
		LIMIT ?`,
		args...,
	)
	if err != nil {
		log.Errorf("failed to run content search: %s", err.Error())

		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var hits []ContentHit

	for rows.Next() {
		var hit ContentHit
		var rank float64

		if err := rows.Scan(&hit.Id, &rank); err != nil {
			log.Errorf("failed to scan a content search result: %s", err.Error())

			return nil, err
		}

		hit.Score = -rank

		hits = append(hits, hit)
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to read the content search results: %s", err.Error())

		return nil, err
	}

	return hits, nil
}

// ftsMatchExpression turns a user's raw query text into an FTS5 MATCH expression.
//
// It does NOT pass the text through. FTS5's MATCH argument is a query language - it has operators
// (AND, OR, NOT, NEAR), prefix stars, column filters, and quoting - so raw input can be a syntax
// error ("cats AND", an unbalanced quote) or can reach past the caller's intent into the query's
// structure. Neither is acceptable from an RPC argument, and a search that returns an error
// because someone typed a hyphen is not a search.
//
// So the text is split into bare alphanumeric tokens and reassembled as quoted phrases joined by
// OR. Quoting makes each token a literal, which is what disarms the operators; OR is not an
// arbitrary choice but the semantics of the OpenSearch backend's "match" query, so the two
// backends agree on which memories match and not just on how they are ranked. Documents matching
// more of the tokens still rank higher, which is bm25 doing the work that AND would otherwise be
// approximating.
func ftsMatchExpression(text string) string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})

	if len(fields) == 0 {
		return ""
	}

	quoted := make([]string, 0, len(fields))

	for _, field := range fields {
		quoted = append(quoted, `"`+field+`"`)
	}

	return strings.Join(quoted, " OR ")
}
