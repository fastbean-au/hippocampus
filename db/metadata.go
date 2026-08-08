package db

import (
	"sort"
	"strings"
)

// This file holds the two dialect-dependent things metadata needs from SQL: how many bytes a row's
// metadata occupies, and how to ask whether a key equals a value. Both differ per driver in ways
// that are easy to get subtly wrong, so both live here rather than inline at their call sites.

// metadataBytesExpr is the byte length of a row's metadata column, for the store's byte accounting.
//
// It is a per-dialect expression rather than a shared length() for two reasons. SQLite's length()
// counts CHARACTERS on a text value and bytes only on a blob - length(body) is byte-exact purely
// because body is a BLOB - so a metadata column holding multi-byte UTF-8 would be under-counted
// without the cast. And Postgres' octet_length has no definition for jsonb at all, so the value
// must
// be rendered to text first.
//
// alias is the table alias including its trailing dot ("m."), or empty for an unaliased query.
//
// Every site that measures a memory's bytes must use this. UsedBytes and EvictMemories' freed-bytes
// estimate are required to be exact complements of one another (see CLAUDE.md); if one counts
// metadata and the other does not, eviction chases a figure it can never reach.
func (d *DB) metadataBytesExpr(alias string) string {
	column := alias + "metadata"

	switch d.driver {

	case driverPostgres:
		return `COALESCE(OCTET_LENGTH(` + column + `::text), 0)`

	case driverMySQL:
		// MySQL's LENGTH is already a byte count, and a JSON column renders to its text form here.
		return `COALESCE(LENGTH(` + column + `), 0)`

	default:
		return `COALESCE(LENGTH(CAST(` + column + ` AS BLOB)), 0)`

	}
}

// metadataConditions builds the WHERE fragments and args matching every one of a filter's key/value
// pairs - a conjunction, so an item must carry all of them.
//
// The key is always a BOUND PARAMETER, never concatenated into the SQL. That is what makes it safe,
// and it is only possible because types.validMetadataKey excludes the characters ('"', '$', '[')
// that would let a key escape the JSON path it is interpolated into on SQLite and MySQL. A key that
// reached here unvalidated would be a driver error - MySQL raises ER_INVALID_JSON_PATH - surfacing
// as Internal rather than InvalidArgument, which is why the RPC layer validates filter keys with
// the
// same rule it validates written ones.
//
// Keys are emitted in sorted order so two identical requests produce byte-identical SQL, which
// keeps
// any statement cache effective and makes the tests deterministic.
//
// A row with no metadata is NULL here, and NULL never equals anything on any of the three dialects,
// so it is correctly excluded by any key predicate. This is the property that makes the whole
// feature safe to add to an existing store, and it holds only because the column is NULL-able: a
// column defaulting to the empty string would make SQLite's json_extract raise "malformed JSON"
// instead.
func (d *DB) metadataConditions(alias string, metadata map[string]string) ([]string, []any) {
	if len(metadata) == 0 {
		return nil, nil
	}

	column := alias + "metadata"

	keys := make([]string, 0, len(metadata))

	for k := range metadata {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	clauses := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)*2)

	for _, k := range keys {
		switch d.driver {

		case driverPostgres:
			// ->> takes the key itself rather than a path, and yields text.
			clauses = append(clauses, column+` ->> ? = ?`)
			args = append(args, k, metadata[k])

		case driverMySQL:
			// The COLLATE is not optional. JSON_UNQUOTE yields a string under the server's default
			// collation, which is case- and accent-insensitive (utf8mb4_0900_ai_ci) - so without it
			// MySQL would match values SQLite and Postgres would not, and the same store would answer
			// the same filter differently depending on its driver. This is the same drift utf8mb4_bin
			// was introduced to kill for id/event_id/group_name.
			clauses = append(clauses, `JSON_UNQUOTE(JSON_EXTRACT(`+column+`, ?)) COLLATE utf8mb4_bin = ?`)
			args = append(args, metadataJSONPath(k), metadata[k])

		default:
			clauses = append(clauses, `json_extract(`+column+`, ?) = ?`)
			args = append(args, metadataJSONPath(k), metadata[k])

		}
	}

	return clauses, args
}

// metadataJSONPath renders a key as the JSON path SQLite and MySQL address a top-level member by.
// The key is always quoted, so one containing '.', ':', '/' or '-' addresses the member of that
// literal name rather than being read as a path expression - json_extract('{"a.b":"c"}', '$."a.b"')
// is "c", where the unquoted $.a.b would be NULL.
func metadataJSONPath(key string) string {
	return `$."` + key + `"`
}

// triStateArg turns a tri-state filter into the bound argument for a boolean column, reporting
// false when the filter is unset and no predicate should be emitted at all.
//
// The value is bound as a Go bool rather than written as a 0/1 literal because the columns are
// INTEGER on SQLite but BOOLEAN on Postgres and MySQL - the same reason EvictMemories binds its
// memories_consolidated fallback.
func triStateArg(state TriState) (bool, bool) {
	switch state {

	case TriStateTrue:
		return true, true

	case TriStateFalse:
		return false, true

	default:
		return false, false

	}
}

// appendMetadataConditions appends the metadata predicates to a query being built with " AND "
// separators, the shape memoryFilterConditions and eventFilterConditions use.
func (d *DB) appendMetadataConditions(query string, args []any, alias string, metadata map[string]string) (string, []any) {
	clauses, clauseArgs := d.metadataConditions(alias, metadata)

	if len(clauses) == 0 {
		return query, args
	}

	query += ` AND ` + strings.Join(clauses, ` AND `)

	return query, append(args, clauseArgs...)
}
