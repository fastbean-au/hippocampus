package db

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
)

// ErrContentSearchUnavailable is returned by SearchMemoryHits when this database cannot answer
// content searches - a read-only tool open, a store opened WithoutContentIndex, or a dialect
// carrying no index of its own. Callers map it to a FailedPrecondition rather than an empty result,
// so an operator who expected search to work is told it is not available instead of concluding
// their store is empty.
var ErrContentSearchUnavailable = errors.New("content search is not available on this store")

// Content search lets SearchMemories work without an OpenSearch cluster, on every dialect. It is a
// secondary index like the OpenSearch one - the memories table stays the sole system of record, and
// callers re-read every hit from it - but it differs from that one in two ways worth being precise
// about, because they change what can go wrong:
//
//   - It is maintained SYNCHRONOUSLY, on the same connection as the write it belongs to. There is
//     no queue, so there is no overflow to drop operations, and no worker, so there is no
//     ordering hazard. What it is not is transactional on the write path: the index statement runs
//     after the memories INSERT/UPDATE rather than inside it, so a failure between the two leaves
//     a memory that is not findable by content. That is logged and never fails the write - the
//     index is rebuildable (--backfill-search) and the primary row is what matters.
//   - Deletes ARE transactional, and cost nothing at any call site, because no call site performs
//     them: the storage engine does. On the embedded dialect that is an AFTER DELETE trigger keyed
//     on the memories rowid; on the server dialects it is a foreign key with ON DELETE CASCADE.
//     Either way one declaration covers every deletion path there is - consolidation, eviction,
//     DeleteMemories, DeleteEventMemories, Purge, Clear, and an import that replaces a row - so
//     unlike the OpenSearch index, which needs a delete observer plus RPC-layer hooks to stay in
//     step, this one cannot drift on deletion at all.
//
// The three dialects' index shapes and query languages are in search_dialect.go, which is the only
// part of this that knows which dialect it is on. Everything below is shared: what gets indexed
// (the plain body, from inside the storage boundary and before compressBody ever sees it), when,
// the backfill, and the scan.
//
// Nothing here indexes an event: only memory bodies are searchable by content, on every backend.

// contentSearchTable is the content-search index, and contentSearchTrigger the AFTER DELETE trigger
// the embedded dialect keeps it in step with. The server dialects need no trigger - their index
// table's foreign key cascades - so that constant is theirs alone.
const (
	contentSearchTable   = "memories_fts"
	contentSearchTrigger = "memories_fts_delete"
)

// contentIndexMaxBytes bounds the text handed to the index, on every dialect.
//
// It exists for one dialect and is applied on all three. Postgres's tsvector has a hard ceiling of
// one megabyte, and exceeding it is an ERROR rather than a truncation - so an unbounded body would
// not be partly searchable, it would fail to index at all, and would do so on exactly one backend.
// Capping every dialect at the same figure is what keeps "which memories are findable" a property
// of the store rather than of the driver it happens to run on.
//
// Half a megabyte of text is some eighty thousand words; a body that long is a document rather than
// a memory, and its opening is what a keyword search will find it by in any case.
const contentIndexMaxBytes = 512 << 10

// ContentQuery carries the parameters of one content search. Text is required; EventId and Group
// restrict matches when non-empty. It mirrors search.Query, but is declared here so the db package
// takes no dependency on the search package (which depends on this one).
type ContentQuery struct {
	Text    string
	EventId string
	Group   string
	Limit   int

	// Metadata restricts matches to memories carrying every one of these key/value pairs, applied
	// in the join to the primary table alongside the group filter and BEFORE the LIMIT - so it
	// narrows the candidates rather than the page.
	Metadata map[string]string

	// Groups is the caller's group scope (see MemoryFilter.Groups), distinct from the Group filter
	// above. Like Metadata it is applied inside the query, not to the results: post-filtering a
	// ranked page would silently return fewer matches than the caller asked for, and would let the
	// size of the shortfall report how much of the store the caller cannot see.
	Groups []string
}

// ContentHit is one match: the memory's id and its relevance, in the convention every backend
// settles on at its own boundary - higher is a better match. The magnitudes are not comparable
// between dialects (a bm25, a ts_rank and an InnoDB relevance are three different numbers); the
// ORDER is what a caller may rely on.
type ContentHit struct {
	Id    string
	Score float64
}

// ContentSearchAvailable reports whether this database can answer content searches. Every dialect
// carries an index, but a read-only tool open never runs the DDL that creates it, and a store
// opened WithoutContentIndex has had it dropped.
func (d *DB) ContentSearchAvailable() bool {
	return d.dialect().contentSearch && !d.readOnly && !d.contentIndexOff
}

// initContentSearch brings the content-search index into line with what this store is configured to
// carry: it creates the index and populates it if it is empty but the store is not - the upgrade
// case, where a database written by a version without content search gains the index on this
// startup and would otherwise answer every search with nothing - or, under WithoutContentIndex,
// drops it.
//
// It is the migration's apply function on two dialect gates (12 and 14), so it runs on every
// startup and settles the index in whichever direction the configuration has since moved. Both
// directions detect their own completion, which is what lets the key be changed on a live store and
// what keeps the ledger honest: the step is recorded either way, because what it records is that
// this build has HANDLED the content index, not that an index exists. Gating the migration itself
// on the key would instead move the store's schema version up and down as the key changed, and a
// build meeting the higher of the two would refuse to open a store it understands perfectly.
//
// Dropping rather than merely ceasing to write is the whole of the safety here. An index that stops
// being maintained does not become empty, it becomes WRONG - it keeps answering, from a subset that
// shrinks with every consolidation cycle - and a search that quietly returns some of the matches is
// worse than one that refuses. Dropped, ContentSearchAvailable is false and SearchMemories already
// answers FailedPrecondition naming what to enable.
func (d *DB) initContentSearch() error {
	log.Trace("func() db.initContentSearch")

	if !d.dialect().contentSearch {
		return nil
	}

	if d.contentIndexOff {
		return d.dropContentIndex()
	}

	if err := d.createContentIndex(); err != nil {
		return err
	}

	return d.backfillContentSearch()
}

// backfillContentSearch populates the content-search index from the memories table when the index
// is empty and the table is not. It is deliberately narrow: it does nothing at all on a store
// whose index already has rows, so an ordinary restart pays one COUNT and moves on, and it never
// tries to repair a partially populated index (--backfill-search is the tool for that).
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

	if err := d.sql.QueryRow(`SELECT count(*) FROM memories WHERE NOT is_binary`).Scan(&stored); err != nil {
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
// table. It is what --backfill-search runs when there is no OpenSearch cluster, and what
// initContentSearch uses to populate a newly created index on an existing store.
//
// Bodies are read (and so decompressed) a page at a time rather than all at once, because the
// whole point of this store is that it may hold more memories than fit comfortably in memory.
func (d *DB) RebuildContentSearch(ctx context.Context) error {
	log.Trace("func() db.RebuildContentSearch")

	if !d.dialect().contentSearch {
		return nil
	}

	// A store opened WithoutContentIndex has no table to rebuild into, and the first statement below
	// would fail against it with whichever "no such table" the dialect spells. Refusing by name
	// instead is what lets --backfill-search say which setting is in the way.
	if d.contentIndexOff {
		return ErrContentSearchUnavailable
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
// The write is an add-or-replace on every dialect (see writeContentIndexEntry), so this works
// unchanged for a create, an update, and an import upsert - none of which need to tell it which of
// those they are.
//
// A binary memory is never indexed: its body is client-encoded and opaque, exactly as on the
// OpenSearch path. That is a skip, not an error.
func (d *DB) indexMemoryContent(ctx context.Context, id string, body string, isBinary bool) error {
	if !d.ContentSearchAvailable() || isBinary {
		return nil
	}

	if err := d.writeContentIndexEntry(ctx, id, truncateForIndex(body)); err != nil {
		log.Errorf("failed to index the body of memory '%s' for content search: %s", id, err.Error())

		return err
	}

	return nil
}

// reindexMemoryContent replaces a memory's entry in the content-search index after its body
// changes.
//
// The delete is unconditional, and runs even for a binary memory: a memory whose body is replaced
// must not keep matching on its old text, and the cheapest way to be sure of that is not to
// special-case it. It is redundant where the write above is an upsert and necessary where it is
// not, which is not worth branching on.
func (d *DB) reindexMemoryContent(ctx context.Context, id string, body string, isBinary bool) error {
	if !d.ContentSearchAvailable() {
		return nil
	}

	if err := d.deleteContentIndexEntry(ctx, id); err != nil {
		log.Errorf("failed to clear the content search entry for memory '%s': %s", id, err.Error())

		return err
	}

	return d.indexMemoryContent(ctx, id, body, isBinary)
}

// SearchMemoryHits returns the memories whose body matches the query, most relevant first. Like
// the OpenSearch path it returns ids and relevance only, never bodies: the caller re-reads the
// rows from the primary store, which is what keeps the store authoritative.
//
// The shape of the query is one shared statement - the memories table joined to the index, filtered,
// ordered by relevance and limited - into which the active dialect supplies its join, its match
// predicate and its scoring expression. Ranking is each engine's own relevance measure, all of them
// bm25-descended and all normalised here to higher-is-better, so result order is comparable with
// the OpenSearch backend's rather than arbitrarily different.
func (d *DB) SearchMemoryHits(ctx context.Context, query ContentQuery) ([]ContentHit, error) {
	log.Trace("func() db.SearchMemoryHits")

	if !d.ContentSearchAvailable() {
		return nil, ErrContentSearchUnavailable
	}

	match := d.contentMatchExpression(contentTokens(query.Text))

	// Every token was punctuation or otherwise dropped by the tokeniser, so there is nothing that
	// could match. Returning empty is right, and it avoids handing an engine an empty query, which
	// on at least one of them is a syntax error rather than an empty result.
	if match == "" {
		return nil, nil
	}

	terms := d.contentSearchTerms(match)

	clauses := []string{terms.predicate}

	args := append([]any{}, terms.scoreArgs...)
	args = append(args, terms.predicateArgs...)

	if query.EventId != "" {
		clauses = append(clauses, `m.event_id = ?`)
		args = append(args, query.EventId)
	}

	if query.Group != "" {
		clauses = append(clauses, `m.group_name = ?`)
		args = append(args, query.Group)
	}

	// The caller's scope, conjoined with the Group filter above. Built through the same helper the
	// filter predicates use so the two paths cannot disagree about what a scope means.
	if clause, clauseArgs := groupScopeConditions("m.", query.Groups); clause != "" {
		clauses = append(clauses, strings.TrimPrefix(clause, ` AND `))
		args = append(args, clauseArgs...)
	}

	// Metadata narrows the candidates inside the query rather than filtering the results, so the
	// LIMIT below still returns a full page when one exists. Through the shared builder so the two
	// backends cannot drift on what a filter means.
	metadataClauses, metadataArgs := d.metadataConditions("m.", query.Metadata)
	clauses = append(clauses, metadataClauses...)
	args = append(args, metadataArgs...)

	limit := query.Limit
	if limit <= 0 {
		limit = 10
	}

	args = append(args, limit)

	rows, err := d.query(ctx,
		`SELECT m.id, `+terms.score+` AS score FROM memories m
		JOIN `+terms.join+`
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY score DESC
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

		if err := rows.Scan(&hit.Id, &hit.Score); err != nil {
			log.Errorf("failed to scan a content search result: %s", err.Error())

			return nil, err
		}

		hits = append(hits, hit)
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to read the content search results: %s", err.Error())

		return nil, err
	}

	return hits, nil
}

// contentTokens splits a user's raw query text into the bare tokens every dialect's query language
// is then assembled from.
//
// It does NOT pass the text through, on any backend. Each engine's match argument is a query
// language of its own - FTS5 has AND/OR/NOT/NEAR, prefix stars and column filters, tsquery has
// &/|/!/<->, MySQL's boolean mode has +/-/*/~/() - so raw input can be a syntax error (an
// unbalanced quote, a trailing operator) or can reach past the caller's intent into the query's
// structure. Neither is acceptable from an RPC argument, and a search that returns an error because
// someone typed a hyphen is not a search.
//
// Splitting on anything that is not a letter or a number is what disarms all three at once: not one
// operator character survives it. What each dialect then does with the tokens is
// contentMatchExpression's business.
func contentTokens(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// truncateForIndex bounds a body to contentIndexMaxBytes, cutting on a rune boundary.
//
// The boundary matters: a body is a proto3 string and so valid UTF-8, and half a rune is not - it
// would be rejected on the wire by one driver and stored as replacement characters by another. The
// same cut the embedding path already makes for the same reason.
func truncateForIndex(body string) string {
	if len(body) <= contentIndexMaxBytes {
		return body
	}

	cut := contentIndexMaxBytes

	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}

	return body[:cut]
}
