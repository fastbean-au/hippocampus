package db

import (
	"context"
	"strings"

	log "github.com/sirupsen/logrus"
)

// The three content-search implementations, and the only file besides dialect.go and metadata.go
// permitted to know which dialect is active.
//
// It is here rather than in the table because none of this reduces to a fragment. Each dialect
// carries a different INDEX SHAPE and a different QUERY LANGUAGE, and the two are not independent:
//
//   - The embedded dialect keeps an FTS5 virtual table keyed on the memories rowid, matched with
//     MATCH, ranked by a bm25 the sign of which runs backwards.
//   - One server dialect keeps a table of tsvectors keyed on the memory id, matched with @@ against
//     a tsquery, ranked by ts_rank.
//   - The other keeps a table of plain bodies under a FULLTEXT index, matched with AGAINST in
//     boolean mode, which is both the predicate and the score.
//
// What is shared - when to index, what to index (the plain body, never the stored bytes), the
// backfill, the rebuild, the filters, the scope, and the rule that a higher score is a better match
// - is in search.go, which knows nothing about which of the three it is running on. A fourth
// dialect is an arm in each of the four switches below plus two fields in the dialect table.
//
// The two shapes differ in one structural way, which dialect.contentIndexCascades names: the
// embedded index is a virtual table kept in step with memories by an AFTER DELETE trigger, and the
// server indexes are ordinary tables kept in step by a foreign key with ON DELETE CASCADE. Both are
// transactional and cost nothing at any call site, which is why deletion - the one thing a
// secondary index really cannot afford to get wrong - is the part of this that cannot drift on any
// of the three.
//
// One cost is worth stating plainly rather than discovering. On MySQL a FULLTEXT index is an index
// ON A COLUMN, so the index table holds a second, UNCOMPRESSED copy of every indexed body. That is
// what a FULLTEXT index is there, and the alternatives are worse: OpenSearch holds a copy too and
// wants a JVM cluster around it, and no search at all was the previous answer. The other two hold
// an inverted index rather than the text - FTS5 contentless, and a tsvector, which is lexemes and
// positions. That is a privacy property and not much of a size one: for short bodies a tsvector is
// about as large as the text it came from, so all three want disk budgeted for them.
//
// None of the three counts towards UsedBytes: the index is derived, and letting the record of what
// is searchable raise capacity pressure would evict live memories to make room for it (the lesson
// the forgotten log and the delete outbox both record). On the embedded dialect that is not quite
// true - its page accounting cannot exclude a table in its own file - and docs/operations.md says
// so, since it is the one dialect where the index is inside the capacity target.

// contentSearchBodyIndex is the name of the index over the indexed text on the dialects whose
// content index is an ordinary table. The embedded dialect needs no such name: its virtual table IS
// the index.
const contentSearchBodyIndex = "memories_fts_body"

// contentSearchDDL creates the FTS5 index and its delete trigger. Both are IF NOT EXISTS, so this
// runs on every startup and only does work the first time - the same shape as the rest of
// initSchema.
//
// An existing store gains an EMPTY index here: the virtual table is created but holds nothing for
// the memories already in the table, so those memories are not findable by content until the
// index is populated. initContentSearch calls backfillContentSearch immediately after this for
// exactly that reason, on every dialect.
//
// The index is CONTENTLESS - an empty content= option, with contentless_delete - so it holds the
// inverted index and not a second copy of the body. That is deliberate and not merely a size
// optimisation: storing the text again would give back much of what body compression saves, on a
// product whose whole purpose is managing a finite store. It also rules out the obvious
// alternative, an
// external-content table over memories.body, for a harder reason: since compression landed, that
// column can hold a gzip stream, so an index reading it directly would tokenise binary.
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

// createContentIndex creates the content-search index for the active dialect, idempotently. Like
// every other migration it detects its own completion and so may be re-run on every startup; a
// store whose index was dropped gets it back (and, through the backfill above this, repopulated).
func (d *DB) createContentIndex() error {
	log.Trace("func() db.createContentIndex")

	switch d.driver {

	case driverPostgres:
		// The tsvector rather than the text: what Postgres needs to answer a search is the inverted
		// form, and storing the body again beside the memories table would give back what
		// compression exists to save. memory_id takes the id column's own type so the foreign key
		// below is accepted, and the cascade is the whole of the delete story.
		statement := `CREATE TABLE IF NOT EXISTS ` + contentSearchTable + ` (
			memory_id   ` + d.dialect().idType + ` PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
			body_search TSVECTOR NOT NULL
		)`

		if _, err := d.sql.Exec(statement); err != nil {
			log.Errorf("failed to create the content search index: %s", err.Error())

			return err
		}

		// GIN rather than GiST: this index is read far more often than it is written, and GIN is
		// the faster of the two to search at the cost of being slower to build.
		return d.ensureIndex(contentSearchTable, contentSearchBodyIndex, `USING GIN (body_search)`)

	case driverMySQL:
		// A FULLTEXT index indexes a COLUMN, so unlike the other two this table holds the body
		// itself. See the file comment: that copy is what a full text search costs on MySQL.
		statement := `CREATE TABLE IF NOT EXISTS ` + contentSearchTable + ` (
			memory_id ` + d.dialect().idType + ` NOT NULL PRIMARY KEY,
			body      LONGTEXT NOT NULL,
			CONSTRAINT memories_fts_memory FOREIGN KEY (memory_id) REFERENCES memories(id) ON DELETE CASCADE
		)`

		if _, err := d.sql.Exec(statement); err != nil {
			log.Errorf("failed to create the content search index: %s", err.Error())

			return err
		}

		// Added after the table rather than declared inside it, and probed for every time, so that
		// an index somebody dropped comes back on the next startup exactly as the other two
		// dialects' do. ensureIndex cannot be used: a FULLTEXT index is not spelled CREATE INDEX.
		exists, err := d.indexExists(contentSearchTable, contentSearchBodyIndex)
		if err != nil {
			return err
		}

		if exists {
			return nil
		}

		if _, err := d.sql.Exec(`ALTER TABLE ` + contentSearchTable + ` ADD FULLTEXT INDEX ` + contentSearchBodyIndex + ` (body)`); err != nil {
			log.Errorf("failed to create the content search index: %s", err.Error())

			return err
		}

		return nil

	default:
		if _, err := d.sql.Exec(contentSearchDDL); err != nil {
			log.Errorf("failed to create the content search index: %s", err.Error())

			return err
		}

		return nil

	}
}

// dropContentIndex removes the content-search index for the active dialect, idempotently. The
// counterpart to createContentIndex above, and the WithoutContentIndex path through
// initContentSearch: an index nothing reads is the largest non-body cost this store carries, and
// the way to stop paying it is to not have it.
//
// Idempotent in the same sense the creation is - IF EXISTS throughout - so it runs on every startup
// while the key says so and does work only the first time.
//
// The embedded dialect's trigger goes FIRST and is not optional. SQLite resolves a trigger's body
// when the trigger fires rather than when it is created, so a trigger left pointing at a dropped
// table does not become inert: it turns every subsequent DELETE from memories into an error, which
// is consolidation, eviction, Clear and Purge all failing at once. The server dialects need no such
// care - their index is an ordinary table whose foreign key is dropped along with it.
func (d *DB) dropContentIndex() error {
	log.Trace("func() db.dropContentIndex")

	statements := []string{`DROP TABLE IF EXISTS ` + contentSearchTable}

	switch d.driver {

	case driverPostgres, driverMySQL:
		// Nothing else: the GIN and FULLTEXT indexes belong to the table and go with it.

	default:
		statements = append([]string{`DROP TRIGGER IF EXISTS ` + contentSearchTrigger}, statements...)

	}

	for _, statement := range statements {
		if _, err := d.sql.Exec(statement); err != nil {
			log.Errorf("failed to drop the content search index: %s", err.Error())

			return err
		}
	}

	return nil
}

// writeContentIndexEntry adds or replaces one memory's body in the index. The body arrives plain -
// the caller reads it from inside the storage boundary, before compression - and already bounded
// (see indexMemoryContent).
//
// On the dialects whose index is keyed on the memory id this is an upsert, so it is correct for a
// create, an update and an import upsert alike without being told which it is. The embedded
// dialect's contentless FTS5 table has no upsert, so it resolves the rowid through the INSERT's own
// SELECT, which achieves the same thing for the same reason: none of the three call sites has to
// know whether a row is already there.
func (d *DB) writeContentIndexEntry(ctx context.Context, id string, body string) error {
	switch d.driver {

	case driverPostgres:
		// 'simple' rather than 'english': it neither stems nor drops stopwords, which is what FTS5's
		// unicode61 tokeniser and OpenSearch's standard analyser both do, so the three backends
		// agree on which memories match rather than one of them quietly matching "run" for
		// "running".
		_, err := d.exec(ctx,
			`INSERT INTO `+contentSearchTable+` (memory_id, body_search) VALUES (?, to_tsvector('simple', ?))
			ON CONFLICT (memory_id) DO UPDATE SET body_search = excluded.body_search`,
			id,
			body,
		)

		return err

	case driverMySQL:
		_, err := d.exec(ctx,
			`INSERT INTO `+contentSearchTable+` (memory_id, body) VALUES (?, ?) AS new
			ON DUPLICATE KEY UPDATE body = new.body`,
			id,
			body,
		)

		return err

	default:
		_, err := d.exec(ctx,
			`INSERT INTO `+contentSearchTable+` (rowid, body) SELECT rowid, ? FROM memories WHERE id = ?`,
			body,
			id,
		)

		return err

	}
}

// deleteContentIndexEntry removes one memory's entry from the index, by id. Only the reindex path
// calls it: every actual DELETION of a memory is handled by the trigger or the foreign key.
func (d *DB) deleteContentIndexEntry(ctx context.Context, id string) error {
	if d.dialect().contentIndexCascades {
		_, err := d.exec(ctx, `DELETE FROM `+contentSearchTable+` WHERE memory_id = ?`, id)

		return err
	}

	_, err := d.exec(ctx,
		`DELETE FROM `+contentSearchTable+` WHERE rowid = (SELECT rowid FROM memories WHERE id = ?)`,
		id,
	)

	return err
}

// contentMatchExpression assembles the tokeniser's output into this dialect's query language.
//
// The tokens are already bare alphanumerics (see contentTokens), so none of the three can carry an
// operator, a quote or a column filter. Each dialect quotes them anyway, which is defence in depth
// rather than necessity: the property that keeps a search from becoming a syntax error or reaching
// past the caller's intent should not rest on a tokeniser somewhere else staying strict.
//
// All three OR their tokens, which is the semantics of the OpenSearch backend's "match" query, so
// the backends agree on which memories match and not merely on how they rank them. A memory
// matching more of the tokens still ranks higher on every one of them, which is what an AND would
// otherwise be approximating.
func (d *DB) contentMatchExpression(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}

	quoted := make([]string, 0, len(tokens))

	switch d.driver {

	case driverPostgres:
		// tsquery's OR is |, and a single-quoted token is a literal lexeme.
		for _, token := range tokens {
			quoted = append(quoted, `'`+token+`'`)
		}

		return strings.Join(quoted, " | ")

	case driverMySQL:
		// In boolean mode a term carrying neither + nor - is optional, so juxtaposition IS OR; a
		// double-quoted term is a literal phrase. Two of MySQL's own limits show through here and
		// have no counterpart on the other two: innodb_ft_min_token_size (3 by default) means a
		// one- or two-character token matches nothing, and InnoDB's built-in stopword list drops
		// about three dozen common English words. Both are server settings rather than anything
		// this schema can state, so they are documented rather than worked around.
		for _, token := range tokens {
			quoted = append(quoted, `"`+token+`"`)
		}

		return strings.Join(quoted, " ")

	default:
		for _, token := range tokens {
			quoted = append(quoted, `"`+token+`"`)
		}

		return strings.Join(quoted, " OR ")

	}
}

// contentSearchTerms carries the three dialect-specific pieces of a content search, with the
// arguments each consumes. The shared scan in SearchMemoryHits assembles them into
//
//	SELECT m.id, <score> AS score FROM memories m JOIN <join> WHERE <predicate> AND ... ORDER BY score DESC
//
// so the arguments are consumed in that order: the score's first (it is in the select list), then
// the predicate's, then the filters', then the limit.
type contentSearchTerms struct {
	// join is the index table and its join condition. Two of the three alias it f; the embedded one
	// deliberately does not, because its MATCH operator takes the TABLE on its left and does not
	// accept an alias there.
	join string

	// score is the relevance expression, in the convention every backend settles on at its own
	// boundary: HIGHER IS A BETTER MATCH.
	score string

	// predicate is the match condition.
	predicate string

	scoreArgs     []any
	predicateArgs []any
}

// contentSearchTerms returns the active dialect's pieces for one already-assembled match
// expression.
func (d *DB) contentSearchTerms(match string) contentSearchTerms {
	switch d.driver {

	case driverPostgres:
		return contentSearchTerms{
			join:          contentSearchTable + ` f ON f.memory_id = m.id`,
			score:         `ts_rank(f.body_search, to_tsquery('simple', ?))`,
			predicate:     `f.body_search @@ to_tsquery('simple', ?)`,
			scoreArgs:     []any{match},
			predicateArgs: []any{match},
		}

	case driverMySQL:
		// The same expression twice: MySQL evaluates a repeated MATCH ... AGAINST once, so this
		// costs one search rather than two, and the score is simply its value.
		return contentSearchTerms{
			join:          contentSearchTable + ` f ON f.memory_id = m.id`,
			score:         `MATCH(f.body) AGAINST (? IN BOOLEAN MODE)`,
			predicate:     `MATCH(f.body) AGAINST (? IN BOOLEAN MODE)`,
			scoreArgs:     []any{match},
			predicateArgs: []any{match},
		}

	default:
		// FTS5's rank is negative and more negative is better, which is the opposite of every other
		// scoring convention including OpenSearch's. The sign is settled here, at the backend
		// boundary, so that nothing above this has to know which backend answered.
		return contentSearchTerms{
			join:          contentSearchTable + ` ON ` + contentSearchTable + `.rowid = m.rowid`,
			score:         `-` + contentSearchTable + `.rank`,
			predicate:     contentSearchTable + ` MATCH ?`,
			predicateArgs: []any{match},
		}

	}
}
