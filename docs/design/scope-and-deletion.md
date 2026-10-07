# Group scoping, predicate deletion and purge

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## The RPC half

- **Deletion by predicate** (`hippocampus/predicate.go` + `hippocampus/selection.go` +
  `db/predicate.go`, TODO 98.2) is `DeleteMemoriesByFilter`/`DeleteEventsByFilter`: what
  offboarding a group actually needs, the deletion surface having been by-id, one-event,
  everything (`Purge`) or whatever-a-manifest-captured (`Clear`) with no predicate anywhere. Five
  things carry it. (1) **The storage layer refuses to offer a `DELETE ... WHERE`**: every memory
  deletion here has to prune the link graph, queue the search index's delete inside the same
  transaction, capture a callback delivery from columns readable only while the rows exist, and
  record a tombstone - a predicate DELETE reaches none of that and the divergence is silent. So the
  RPC resolves the filter to ids in batches (`MemoryIdsMatching`) and feeds the existing by-id
  chokepoint; the deletion of a batch is what makes the next selection return the next one, which
  is exactly what a client-side page-and-delete loop cannot do while the store moves under it.
  (2) **An empty filter is refused**, on `DeleteForgottenMemories`' precedent - "everything" is
  `Purge`, and no defaulted field should reach it - and the check is `reflect.DeepEqual` against the
  BUILT filter's zero value, so a selecting field added later is covered without anybody extending
  a list. (3) **The listing is the dry run, provably**: `selection.go`'s `memorySelectionFilter`/
  `eventSelectionFilter` build the predicate for the deletion AND for `GetMemories`/`GetEvents`,
  off `memorySelector`/`eventSelector` interfaces both proto messages satisfy because the fields
  are named identically. There is deliberately no `dry_run` FLAG, on the reasoning that made
  `PreviewConsolidation` a separate RPC rather than a flag on `Sleep` - authorisation is per-RPC, so
  a flag could never be tiered apart from the destructive call it rode on. (4) **`linked_to` is
  absent** from both requests although the listings carry it: deleting a memory removes its edges,
  so that predicate narrows as a consequence of its own deletions and no care on the caller's part
  could make the listing and the deletion agree. (5) **`admin` and `scopeFilter`**, not
  `scopeUnbound` like `Purge` - draining one partition is precisely what a bound token should be
  able to do, and the predicate is what confines it. `max_deletions` bounds one call and `complete`
  reports whether the filter is exhausted; `delete_empty_events` reuses `DeleteEventIfEmpty` so an
  event that still holds a memory is never taken. Off the MCP surface, which lets a model act on
  records it can name and never on a set it can only describe.
- `Purge` deletes everything; while it runs, `InterceptorBlockWhenPurgeInProgress` (registered in
  main.go, `codes.Unavailable`) rejects all Hippocampus RPCs on gRPC, and its HTTP counterpart
  `HTTPMiddlewareBlockWhenPurgeInProgress` (503) rejects them on the gateway.
- `scope.go` is the RPC half of **group scoping** (`auth/groups.go` is the other; the decision
  record is TODO 60.1). It cannot be one chokepoint, because the RPCs do not reach the store the
  same way: a listing carries the scope as a predicate (`MemoryFilter.Groups`), an id-addressing
  RPC has no predicate and checks the ids instead (`scopeMemoryIds`/`scopeEventIds`), a store walk
  threads it into pagination, and `Purge`/`Sleep`/`PreviewConsolidation` cannot be scoped at all
  and are refused (`requireUnbound`). Four mechanisms means four places to forget one, so the
  `scopes` table declares which each RPC uses and `TestScopesCoverEveryRPC` requires every
  descriptor method to appear — **a new RPC must add an entry**, and a subtest in
  `scope_isolation_test.go`, whose own descriptor check is the reminder. The table is
  documentation and a checklist, never consulted at request time; what verifies the handlers is
  `TestGroupScopeIsolation*`, which drives every RPC as a caller bound to one group (disabling
  `scopedGroups` fails 46 of its subtests). Four rules the code holds to: an out-of-scope id the
  caller **named** reports `NotFound`, never `PermissionDenied`, which would confirm it exists;
  an id the caller did **not** name (a link's far end, an unlink target) is dropped silently, since
  refusing would reveal the crossing; and `writeGroup` stamps a scoped caller's sole group on a
  write naming none, so a bound writer never creates a record it cannot read back; and an
  **upsert** (`Import`/`ImportBatch`) is checked against the row it would replace as well as the
  group it writes to (`scopeImport`), since stamping the incoming row says nothing about whose row
  an id already names (TODO-3 item 136). An event-wide operation (`DeleteEvent`, the predicate
  event delete, summary replacement) acts on the caller's own memories of an in-scope event only,
  detaching another group's from a deleted event rather than refusing - a refusal would reveal
  them (`clearEventMemories`, item 139; `DeleteEvent` does the same inside one transaction through
  `db.DeleteEventCascade`, item 165, whose `if_empty` likewise counts only the caller's memories). Two things
  deliberately cross the boundary, both consequences of the partition being _soft_:
  `link_significance` is scope-blind (it is the denormalised aggregate in the covering index, and
  recomputing per-scope would mean joining the link tables in the consolidation scans), and the
  decay dynamics stay store-global.
