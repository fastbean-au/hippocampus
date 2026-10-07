# The contract

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `contract/`

`contract/` — the gRPC contract (`hippocampus.proto`) and generated code. RPCs cover
event/memory CRUD plus `StoreMemories` (the **write-path** batch, and the counterpart to
`ImportBatch` rather than a variant of it: up to 500 unrelated memories, each through exactly
`StoreMemory`'s validation/defaulting/minimum-significance gate in its own transaction, and each
answered by its own positional result carrying the gRPC code that memory alone earned. Nothing is
upserted — an id the store holds fails `ALREADY_EXISTS` in its result — and the call fails only for
a batch-level fault. Per memory rather than per batch because a partial success is the useful
answer to a producer that cannot re-author a record it did not write, and one transaction would
hold a write lock across 500 significance resolutions; the cap is the page size `Transfer` already
sends `ImportBatch` in, so a producer learns one number. Deliberately off the MCP surface, on the
same rule that excludes `ImportBatch`), `Sleep`, `Purge`, `MergeEvents`, `RecallMemories`,
`ReplaceMemoriesWithSummary`, `GetSummarisationCandidates`, `SummariseMemories` (the embedded-LLM
generate-and-replace), `PreviewConsolidation`/`ExplainConsolidation`/`GetConsolidationStatus` (the
forgetting-transparency set: what a cycle would forget, where an individual memory stands, and
when the next cycle is due plus what the last one did — the last of these being the only one that
answers "when", and the only one that does NOT refuse on a replica, since reporting
`consolidation_enabled: false` is the answer there), `WhoAmI` (reports the caller's
effective authorisation tier plus the deployment's capability flags and its `version` — the only
gRPC-reachable place the build is reported, since `--version`, the startup log and `/healthz` are
all unreachable from a gRPC client and `GetTopology` is optional, tier-configurable and refused to
a scoped caller), `GetSignificanceLevels` (the distinct significance values in use: the registry
`SignificancePlacement` positions against, which a client had no way to see - `reader`,
`scopeNone`, and answered in full to a group-scoped caller because there is no per-group scale),
the transfer/archive surface
(`Export`, `Import`, `ImportBatch`, `Transfer`, `Clear`), and the predicate deletions
(`DeleteMemoriesByFilter`/`DeleteEventsByFilter` - see `hippocampus/predicate.go`). The event surface mirrors the memory one
since item 94: `UpdateEvent` is `UpdateMemory`'s counterpart (`patch /v1/events/{id}`, the full
partial update `db.UpdateEvent` always carried, with `EndEvent`/`UpdateEventSignificance` kept as
the convenience routes), and `GetEvents` carries `ended`, `name_contains`, `linked_to` and `links`
beside `GetEventById.links`. `ended` arrived with the fix to `time_end_max`, which matched every
open event because an open event stores `time_end = 0` - the same pairing, and the same reasoning,
as `recalled` and `time_recalled_max`. Each RPC carries a
`google.api.http` annotation mapping it onto a REST-ish `/v1/...` path (see
[the API reference](docs/api.md#routes) for the full mapping); `go generate
./contract` (directive in `generate.go`) turns those into `hippocampus.pb.gw.go` (the gateway)
and `hippocampus.swagger.json` (the OpenAPI description, embedded via `swagger.go`).
`contract/google/api/{annotations,http}.proto`
are vendored copies of the googleapis definitions the annotations depend on. Every message and
field carries a comment - they reach every generated client and the OpenAPI document - and
`contract/comments_test.go` refuses a new one without, through an allow-list that may only shrink
(empty since TODO-3 item 174) and a line scan held to the compiled descriptor.

## `types/`

`types/` — request/response validation and conversion between proto messages and DB rows.
