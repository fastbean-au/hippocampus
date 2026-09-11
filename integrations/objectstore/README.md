# Hippocampus object-storage agents

Two client daemons that let a Hippocampus instance act as a **retention controller** over an S3
bucket it does not hold the contents of: the payloads stay in the bucket, the store holds one
pointer-memory per object and decides what survives, and these carry that decision out.

- **`object-gateway`** — the tap. Fronts the bucket; every object it serves reinforces the memory
  that points at it, so recall reinforcement still reaches a store that never sees the reads.
- **`object-reaper`** — the actuator. Deletes the object behind a forgotten memory, by three paths:
  the `memory_forgotten` callback, the forgotten log read back over a window, and a reverse sweep of
  the bucket.

Full documentation: **[`docs/objectstore.md`](../../docs/objectstore.md)**.

## Quick start

```bash
# The tap, in front of a bucket.
go run ./cmd/object-gateway --bucket payloads --address localhost:50051 --auth-token dev

# The actuator, in shadow mode - it reports what it would delete and deletes nothing.
go run ./cmd/object-reaper --bucket payloads --address localhost:50051 --sweep-now
```

The one thing to get right before either is useful: a pointer-memory's id must be `<bucket>/<key>`.
Both agents derive it that way and neither keeps any state, which only works because the mapping goes
both ways — the forgotten log carries ids and never bodies, so a one-way hash would leave an agent
catching up after an outage unable to name a single object.

## Layout

| Package     | What it is                                                                            |
| :---------- | :------------------------------------------------------------------------------------ |
| `keymap`    | The id derivation, and its reverse. The contract with whatever writes the memories    |
| `tap`       | Batched, deduplicated, speculative `RecallMemories` — the reinforcement core          |
| `gateway`   | The HTTP front door: redirect or proxy, plus a `POST /recall` for your own chokepoint |
| `reap`      | The actuator: the callback receiver, the forgotten-log catch-up, and the sweep        |
| `objects`   | The S3 surface (presign/get/delete/list/ping), plus an in-memory implementation       |
| `client`    | The gRPC dial, and the three RPCs this integration is allowed to make                 |
| `httpserve` | One HTTP listener with TLS and a drain, shared by both commands                       |

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

The tests need no bucket and no running service: the S3 driver is exercised against an `httptest`
server speaking just enough of the wire protocol, and everything above it against in-memory fakes.
This is a separate Go module (the AWS SDK dependency tree stays out of the root module) with a
`replace` back to the repo root, so it is built and tested by its own `objectstore-agents` CI job.
