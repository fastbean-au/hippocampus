package db

import (
	"context"

	log "github.com/sirupsen/logrus"
)

// Group scoping, storage side.
//
// A caller may be restricted to a subset of the store's group labels (auth.Claims.Groups). This
// file holds the one predicate that restriction turns into, so every scoped query in the package
// spells it identically. Which queries are scoped, and what happens to the ones that cannot be
// (those addressing rows by id), is decided a layer up - see hippocampus/scope.go.
//
// There is deliberately no tenant column behind any of this. The scope is the existing group_name,
// which is why this whole mechanism needs no schema change, no migration, and no new index: the
// column, its covering-index membership, and its meaning in both search backends already exist.
// See the decision record at TODO item 60.1.

// groupScopeConditions returns the SQL predicate restricting a query to the caller's group scope,
// and its arguments. The alias prefixes the column for queries that join (search's "m."); pass ""
// where the table is unaliased.
//
// An empty scope returns no predicate at all - unrestricted. That is the correct default for every
// server-owned scan and the reason this can be threaded through shared query builders without
// touching the consolidation paths, but it does mean the CALLER is responsible for having decided
// that an empty scope means "unscoped" rather than "scoped to nothing". auth.GroupsFromContext
// makes that distinction, and it must not be re-derived from the slice's length here.
func groupScopeConditions(alias string, groups []string) (string, []any) {
	if len(groups) == 0 {
		return "", nil
	}

	args := make([]any, 0, len(groups))
	for _, group := range groups {
		args = append(args, group)
	}

	return ` AND ` + alias + `group_name IN (` + placeholders(len(groups)) + `)`, args
}

// appendGroupScope appends the group-scope predicate to a query being built, mirroring
// appendMetadataConditions so the two read the same at their call sites.
func appendGroupScope(query string, args []any, alias string, groups []string) (string, []any) {
	clause, clauseArgs := groupScopeConditions(alias, groups)

	if clause == "" {
		return query, args
	}

	return query + clause, append(args, clauseArgs...)
}

// MemoryIdsOutsideGroups and EventIdsOutsideGroups report which of the named ids fall outside the
// caller's group scope. They are the counterpart to the filter predicate above, for the RPCs that
// address rows by id and so have no filter to constrain: RecallMemories, UpdateMemory,
// DeleteMemories, the link RPCs, and the rest.
//
// They deliberately report the ids that are OUT of scope rather than the ones in it, mirroring
// MissingMemoryIds/MissingEventIds - which the link RPCs already call for exactly this shape of
// check, and whose result the caller turns into a NotFound. Reporting it the same way lets a handler
// treat "does not exist" and "exists but is not yours" identically, which is what keeps the error
// from being an existence oracle.
//
// An id that does not exist at all is NOT reported here: it is absent from the query's result and
// so indistinguishable from one that is out of scope, and both must produce the same NotFound
// anyway. Callers needing to tell them apart (none do) would have to ask MissingIds separately.
func (d *DB) MemoryIdsOutsideGroups(ctx context.Context, ids []string, groups []string) ([]string, error) {
	log.Trace("func() db.MemoryIdsOutsideGroups")

	return d.idsOutsideGroups(ctx, memoryGraph, ids, groups)
}

func (d *DB) EventIdsOutsideGroups(ctx context.Context, ids []string, groups []string) ([]string, error) {
	log.Trace("func() db.EventIdsOutsideGroups")

	return d.idsOutsideGroups(ctx, eventGraph, ids, groups)
}

// idsOutsideGroups is the shared implementation, chunked exactly as MissingIds is so a large id
// list cannot build an unbounded IN clause (see item 23.7, which is why that chunking exists).
func (d *DB) idsOutsideGroups(ctx context.Context, graph linkGraph, ids []string, groups []string) ([]string, error) {
	if len(ids) == 0 || len(groups) == 0 {
		return nil, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	inScope := make(map[string]bool, len(ids))

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		args := make([]any, 0, len(chunk)+len(groups))
		for _, id := range chunk {
			args = append(args, id)
		}

		query := `SELECT id FROM ` + graph.items + ` WHERE id IN (` + placeholders(len(chunk)) + `)`
		query, args = appendGroupScope(query, args, "", groups)

		rows, err := d.query(ctx, query, args...)
		if err != nil {
			log.Errorf("failed to scope-check %s ids: %s", graph.kind, err.Error())

			return nil, err
		}

		for rows.Next() {
			var id string

			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				log.Errorf("failed to scan %s id: %s", graph.kind, err.Error())

				return nil, err
			}

			inScope[id] = true
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()
			log.Errorf("failed to scope-check %s ids: %s", graph.kind, err.Error())

			return nil, err
		}

		_ = rows.Close()
	}

	var outside []string

	for _, id := range ids {
		if !inScope[id] {
			outside = append(outside, id)
		}
	}

	return outside, nil
}
