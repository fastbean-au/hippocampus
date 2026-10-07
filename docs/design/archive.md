# Export, import and transfer

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `archive/`

`archive/` — the export/import wire format and object storage:
protodelim+gzip codec over `ArchiveRecord` protos (versioned header first) and the
`ObjectStore` interface (Put/Get) with **two** implementations, selected in `main.go` and
mutually exclusive (`configProblems` refuses both being set): an aws-sdk-go-v2 S3 one
(`s3.bucket`; `s3.endpoint`/`s3.usePathStyle` for MinIO; credentials from the standard AWS chain)
and a filesystem one (`archive.directory`, `archive/file.go`). The filesystem backend exists
because the archive format is the only representation preserving a store's full state
(timestamps, recall history, groups, summary flags, links) and requiring a bucket put the offline
backup of a store that is _designed to forget_ behind infrastructure a one-binary-and-a-directory
deployment does not otherwise need. Two things carry it. `Put` writes to a temporary file beside
its destination and **renames it into place**, so an interrupted export leaves nothing rather than
a truncated file — which `Import` would accept as an archive and fail part way through, having
already upserted what it read. And a key is **refused, not sanitised**: `Import` takes its
`object_key` straight from the request, so over a filesystem it is a caller-supplied path, and
anchoring-and-cleaning (the usual trick) would answer a request for `../../etc/passwd` with some
other file rather than with an error — `resolve` therefore rejects an absolute key and any
`.`/`..`/empty segment, which also keeps the two backends naming the same object for any key
either accepts. Containment is lexical, so the directory is assumed server-owned. The
transfer RPCs live in `hippocampus/transfer.go`: `Transfer` dials `transfer.targetAddress` with
credentials from `Transfer.clientCredentials`, which honours the same TLS trust-option block as
`opensearch.tls` (`transfer.tls.{caCertFile,certFile,keyFile,insecureSkipVerify}`); TLS is
toggled by `transferTLSEnabled` (`hippocampus/server.go`), accepting both the block form
`transfer.tls.enabled` and the legacy scalar `transfer.tls: true`. Export/Transfer walk the store via
`db.GetMemoriesPage`/`db.GetEventsPage` keyset pagination, record an in-memory manifest (ids +
recall-state snapshots, last 8 kept — `transfer.maxManifestRows`, 0/default unlimited, bounds one
run's capture: `walkStore` pre-flights the count and re-checks during the walk, refusing over-cap
with `FailedPrecondition` before any upload), and `Clear` (or the RPCs' `clear` flag) deletes exactly
the captured records via `db.ClearMemories` (the exported wrapper over the race-safe
`deleteMemoriesIfUnrecalled`, so recalls landing mid-run protect their memory) and
`DeleteEventIfEmpty`. The one-shot `clear` flag clears the manifest in-place (never via a
store-then-take round trip, which could return a nil manifest under concurrent runs and panic);
on a successful clear the manifest is not cached, and on a _failed_ clear it is cached so the
returned `manifest_id` can retry via `Clear` (the error message says so). Import/ImportBatch upsert full rows by id (`db.ImportMemories`/
`db.ImportEvents` — no defaulting, no minimum-significance gate, idempotent) and index
non-binary memories into the optional search index. Bodies are proto3 strings and therefore
UTF-8 everywhere — "binary" memory bodies are client-encoded — so the archive needs no special
binary handling.
