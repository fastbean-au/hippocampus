# Object storage: decay as a retention controller

Two small agents that let Hippocampus govern a bucket it does not hold the contents of. The payloads
stay in S3 (or MinIO, or anything S3-compatible); Hippocampus holds one **pointer-memory** per object
and decides what survives; these carry the decision out.

They are the working half of the [retention-controller deployment
mode](use-cases.md#retention-controller-the-payload-stays-where-it-is), and there are two of them
because the loop has two ends:

- **`object-gateway`** — the **tap**. It fronts the bucket, and every object it serves reinforces the
  memory that points at it. Without it the controller has significance, age, links and capacity
  pressure to work with, but not recall — the one input no expiry policy anywhere has.
- **`object-reaper`** — the **actuator**. It deletes the object behind a memory the store has
  forgotten, by three paths that back each other up.

Both live in [`integrations/objectstore`](../integrations/objectstore/README.md), a separate Go
module, and both are clients: they hold no state, and losing either loses no data.

## Before anything else: the id is the contract

A pointer-memory's id **must** be `<bucket>/<key>`.

Everything here follows from that. The gateway reinforces `keymap.MemoryId(bucket, key)` without
asking whether such a memory exists, and the reaper turns a forgotten id back into the object it
named. Neither keeps a lookup table, a cursor or a cache, and that is only possible because the
mapping goes both ways.

So whatever writes your pointer-memories — a producer, a Lambda, an
[event-source bridge](eventsource.md) — has to use the same id:

```python
client.store_memory(
    id=f"{bucket}/{key}",              # the contract
    body=f"s3://{bucket}/{key}",       # or a summary; the agents never read it
    significance=3,
    external_bytes=object_size,        # what forgetting this releases, out there
    group="traces",
)
```

Two consequences worth knowing up front.

**A mismatch is silent.** `RecallMemories` is an `UPDATE ... WHERE id IN (...)` that matches nothing
when the id is absent, which is exactly what lets the tap be stateless — and it means a producer
using a different id scheme produces no error, no warning and no reinforcement. The gateway watches
its own hit rate for that; see [when the tap reinforces
nothing](#when-the-tap-reinforces-nothing).

**A key over 255 characters cannot be managed.** The store's id column is `VARCHAR(255)` on MySQL, so
an id that does not fit is refused rather than hashed. Such objects are skipped by the tap and — this
is the important half — are **never** deleted by the sweep, which cannot tell an object it failed to
map from one nobody asked it to manage.

## The gateway

```bash
hippocampus-object-gateway \
  --bucket payloads \
  --address localhost:50051 \
  --token "$WRITER_TOKEN" \
  --auth-token "$GATEWAY_TOKEN"
```

`GET /o/<key>` reinforces the pointer-memory and then answers the read. Two modes:

| `--mode`             | What happens                                               | Cost                                               |
| :------------------- | :--------------------------------------------------------- | :------------------------------------------------- |
| `redirect` (default) | Presigns the object and answers `302`                      | None — no payload byte passes through the process  |
| `proxy`              | Streams the object through the gateway, forwarding `Range` | Bandwidth, and the gateway is now on the data path |

Prefer `redirect`. `proxy` exists for a deployment that cannot expose the store's own hostname to
readers at all; it forwards range requests, because answering one with a whole body and a `200` is a
wrong answer rather than a degraded one.

`HEAD` deliberately does **not** reinforce: asking for an object's metadata is not reading it, and a
client that HEADs before every GET would otherwise double every recall it makes.

**The gateway is not an authorisation layer for the bucket.** A presigned URL grants a read to
whoever holds it, so anybody who may call the gateway may read anything in the bucket it fronts.
`--auth-token` is therefore required unless you pass `--allow-anonymous`, which is a legitimate
choice for public assets and should not be reachable by leaving a flag unset. `--url-ttl-seconds`
(default 300) bounds how long an issued URL lives.

### If you already have a chokepoint

You may already have a fetch proxy, a signed-URL issuer, or an application that knows what it just
served. Then you want the tap without the gateway:

```bash
curl -X POST http://gateway:8088/recall \
  -H 'Authorization: Bearer '"$GATEWAY_TOKEN" \
  -d '{"keys":["traces/2026/09/abc.json","traces/2026/09/def.json"]}'
```

It answers `202` with what it accepted. Coarse signals count — appearing in any result set, being
referenced by an alert, being linked from an incident is enough to move a decay clock — so this does
not have to be per-view accurate to be worth wiring.

Reads are batched: `--recall-batch-size` (default 100) distinct ids, flushed at
`--recall-batch-window-ms` (default 2000) or when full. A read counts as tapped once it is
**buffered**, so a crash inside the window loses at most one window of reinforcement. That is
deliberate: a lost read decays a memory slightly sooner, it does not make it wrong. `--recall-batch-size 0`
recalls synchronously per read if you disagree.

## The reaper

```bash
hippocampus-object-reaper \
  --bucket payloads \
  --address localhost:50051 \
  --token "$READER_TOKEN" \
  --callback-secret "$CALLBACK_SECRET" \
  --sweep-interval 6h
  # note: no --delete. See below.
```

### It does not delete by default

Without `--delete` the reaper runs in **shadow mode**: every deletion is selected, counted and logged
at Info, and none is carried out. That is the state to run it in first, and the metrics it produces
in shadow (`hippocampus.objectstore.deletions{outcome="shadow"}`) are exactly what you compare
against what your existing flat expiry would have dropped, before making a decay model authoritative
over data the store cannot see.

`--sweep-now` runs one sweep, reports, and exits — the dry run you can put in a terminal.

### The three paths

A forget-instruction reaches the reaper three ways, and it needs all three because at-least-once
delivery against a process that can be down is still lossy — and the loss here is not a missed
notification but an orphaned payload: permanent, silent, and growing precisely with how well the
decay cycle is working.

| Path      | What it is                                                | What it covers                                    | Flag               |
| :-------- | :-------------------------------------------------------- | :------------------------------------------------ | :----------------- |
| **Push**  | `memory_forgotten` callbacks, posted as the cycle deletes | The normal case, within seconds                   | `--listen-port`    |
| **Pull**  | The forgotten log, read back over a window at startup     | What was missed while the process was not running | `--catch-up` (1h)  |
| **Sweep** | The bucket enumerated, each object's memory asked after   | Anything both of the others lost                  | `--sweep-interval` |

Deleting an object that is already gone is a no-op, which is what makes the three safe to overlap and
safe to replay.

Configure the service side to match:

```json
{
  "callbacks": {
    "enabled": true,
    "url": "http://reaper:8089/callbacks",
    "signingSecret": "...",
    "backlogPolicy": "retain"
  },
  "consolidation": {
    "tombstones": { "enabled": true },
    "capacityExternalBytes": 536870912000
  }
}
```

`backlogPolicy` matters: here a `memory_forgotten` delivery is an **instruction**, and the default
`abandon` discards deletions at the queue's caps, orphaning a payload behind each one permanently.
`retain` or `stall` are the two honest choices, and
[`docs/configuration.md`](configuration.md#outbound-callbacks) sets out who pays under each. The
forgotten log is the pull path's source, so keep it on.

The reaper verifies deliveries with the same bearer token and HMAC signature the sink sends
(`--callback-token`, `--callback-secret`); with a secret configured it also refuses a delivery whose
timestamp is outside `--callback-max-age-seconds` in either direction. It answers `5xx` for anything
it could not carry out, which is what makes the service's queue replay it, and `2xx` for anything it
deliberately did not act on, which is what stops the queue replaying that forever.

### What it refuses to delete

- **An id that is not an object reference** — a UUID from another producer sharing the store.
- **An id naming another bucket** — an agent pointed at the wrong one.
- **A key too long to have been minted here.**
- **A cause outside `--causes`** (default `consolidation,eviction,cascade`).

That last one is worth reading carefully, because each omission is a way to destroy data that is
still wanted:

| Cause             | Default | Why                                                                                               |
| :---------------- | :------ | :------------------------------------------------------------------------------------------------ |
| `consolidation`   | acts    | The decay pass. This is the point.                                                                |
| `eviction`        | acts    | The capacity pass. Likewise.                                                                      |
| `cascade`         | acts    | A memory going because its event did.                                                             |
| `client`          | ignores | An explicit `DeleteMemories`. Defensible to act on; enable it deliberately.                       |
| `clear`           | ignores | The second half of an Export or Transfer, which is a **move** — the payload is still wanted.      |
| `summary_replace` | ignores | Memories folded into a summary; whether their payloads go too is your judgement.                  |
| `purge`           | ignores | An operator resetting the store. Obeying it would empty the bucket on one administrative command. |

### The sweep, and its one assumption

The sweep enumerates the bucket, derives each object's id, asks the store which of them it still
holds ([`ExplainConsolidation`](operations.md), 200 ids per call), and reaps the rest.

It assumes the bucket — or `--sweep-prefix` of it — is managed solely by this controller. An object
with no pointer-memory is, under that assumption, an orphan. Three things hold that to something
safe:

- **`--sweep-min-age` (default 24h) cannot be disabled.** An object is never judged before its
  producer has had time to write the memory for it; without a grace period the sweep would race every
  write.
- **Shadow mode**, as everywhere else here.
- **`--sweep-prefix`**, which is how a shared bucket is narrowed to the part this controller owns.

And one thing it refuses to do: if the store cannot be asked which ids it holds, the sweep **stops**.
Reading "cannot ask" as "not held" would empty the bucket the first time the service was unreachable.

The sweep needs an instance with `consolidation.enabled` — a replica refuses `ExplainConsolidation`,
since the decay policy it would describe is not the one being carried out. Point the reaper at the
consolidating instance; it is the one sending the callbacks anyway.

## Running the pair

Ports, so the two can share a host:

| Process          | Serves             | Probes and `/metrics` |
| :--------------- | :----------------- | :-------------------- |
| `object-gateway` | `8088` (objects)   | `8090`                |
| `object-reaper`  | `8089` (callbacks) | `8091`                |

Every flag is also an environment variable — `HIPPOCAMPUS_OBJECT_GATEWAY_*` and
`HIPPOCAMPUS_OBJECT_REAPER_*`, dashes becoming underscores — which is how tokens are injected without
appearing in `argv`. Bucket credentials come from the standard AWS chain and never from a flag.

Tokens differ by agent, and minimally: the gateway needs **writer** tier (it reinforces), the reaper
needs only **reader**. Both want an **unscoped** token — a group-scoped one turns an id the store
does not hold into `NotFound` for the whole batch, which the tap would see as every read missing.

Images are published per agent:

```bash
docker run ghcr.io/fastbean-au/hippocampus-object-gateway:latest --bucket payloads --allow-anonymous
docker run ghcr.io/fastbean-au/hippocampus-object-reaper:latest --bucket payloads --sweep-now
```

### Readiness is different for the two, deliberately

The gateway's `/readyz` covers the **bucket only**. Its job is serving objects, and reinforcement is
secondary — taking it out of a load balancer because a memory store is down would turn a lost
decay-clock update into an outage for every reader. The reaper's covers **both** ends, because its
job needs both.

## When the tap reinforces nothing

This is the failure worth knowing about in advance, because nothing errors.

The gateway counts `hippocampus.objectstore.recalls` by outcome, and `missing` versus `reinforced` is
the hit rate. If a meaningful number of reads reinforce nothing at all over five minutes, it says so
at Warn, and `HippocampusObjectTapNotReinforcing` alerts on the same condition. Either:

- the ids do not match — the producer is not writing `<bucket>/<key>`; or
- the store really has forgotten everything being served, which on a store whose job is forgetting is
  worth knowing either way.

## Metrics

| Metric                                      | What it says                                                                         |
| :------------------------------------------ | :----------------------------------------------------------------------------------- |
| `hippocampus.objectstore.recalls`           | Ids reinforced, by outcome (`reinforced`/`missing`/`failed`) — the hit rate          |
| `hippocampus.objectstore.recall.batch_size` | Ids per call, for tuning the window against the RPC rate it saves                    |
| `hippocampus.objectstore.requests`          | Object reads, by mode and outcome                                                    |
| `hippocampus.objectstore.request.duration`  | Time to serve one read                                                               |
| `hippocampus.objectstore.deletions`         | Objects acted on, by path and outcome — `shadow` is what a dry run reports           |
| `hippocampus.objectstore.deliveries`        | Callbacks received, by kind and outcome                                              |
| `hippocampus.objectstore.sweep.examined`    | Objects enumerated; compare with deletions — swept millions, deleted none is healthy |
| `hippocampus.objectstore.sweep.duration`    | Time for one pass                                                                    |

Plus the shared client RED metrics (`hippocampus.client.rpc.*`) for everything dialled.

## What this does not do yet

- **Nothing here writes the pointer-memories.** Registration is the producer's, deliberately: it is
  the only party that knows an object's significance, its group and what it relates to.
- **No OIDC client-credentials grant.** Both agents take a static `--token`; against an IdP-backed
  service that token expires and then fails every call. The [broker bridges](eventsource.md) have the
  grant and these should grow it.
- **S3 only.** The `objects.Store` interface is four operations wide, so another object store is a
  new implementation rather than a new design.
