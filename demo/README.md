# Demo

A long-running exerciser for the hippocampus service: a load generator that stores, queries,
recalls, mutates, and deletes events and memories against a live instance, capped at 1 GiB of
on-disk data. The front-facing part of the demo is the **web console** — a single-page UI at
[http://localhost:8080/ui](http://localhost:8080/ui) for browsing and searching the memories and
events the generator is churning through in real time.

## Running

```sh
./demo/run.sh
```

The script builds the service and the generator, starts the service with `demo/config.json`
(gRPC on port 8300, the HTTP/JSON gateway and web console on 8080, database under `demo/data`),
launches an OpenSearch container so content search works (see below), waits for the service to
listen, then starts the generator. **Open [http://localhost:8080/ui](http://localhost:8080/ui)**
to watch and drive it. Ctrl-C stops everything. The database persists between runs; delete
`demo/data` to start fresh. `MAX_BYTES=<bytes>` overrides the generator's pause cap, and any
arguments passed to the script are forwarded to the generator (e.g. `./demo/run.sh --bursty_workers 8`).

## The web console (the demo UI)

The service's HTTP/JSON gateway (port 8080) serves a self-contained single-page console at
[http://localhost:8080/ui](http://localhost:8080/ui) — no build step, no external assets. It has
three tabs, all driving the same `/v1` JSON endpoints the gateway exposes:

- **Search** — free-text content search over memory bodies (`POST /v1/memories/search`, backed by
  OpenSearch). Event ids in the results are clickable and open the whole event; a `Reinforce` toggle
  routes matches through recall so you can watch decay clocks reset.
- **Memories** — create, edit, recall, and delete memories, with significance/group/timestamp
  filters and paging.
- **Events** — create, edit, end, and delete events, optionally listing their memories.

Auth is off in the demo config, so the token field can be left blank. Because the generator is
constantly writing, the console shows live data — and the sleep cycle forgetting and evicting it.

By default `run.sh` provisions OpenSearch for the Search tab: if something is already serving on
`http://localhost:9200` (e.g. a standing test cluster) it reuses that; otherwise it starts an
`opensearchproject/opensearch:3.1.0` container (needs `docker` or `podman`) and stops it again on
exit. Either way the service's secondary content-search index is enabled against it. Set
`SEARCH=0 ./demo/run.sh` to skip search entirely; if no cluster is reachable and no container
runtime is found the demo still runs — the Memories and Events tabs work fully, only the Search tab
is inactive.

## Watching a soak in Grafana

```sh
./demo/run.sh
```

By default `run.sh` also launches an all-in-one `grafana/otel-lgtm` collector (needs `docker` or
`podman`), enables the service's OTLP metrics and traces (via `HIPPOCAMPUS_*` env overrides, so
`demo/config.json` is untouched), and points them at the collector. Grafana comes up at
[http://localhost:3000](http://localhost:3000) with a pre-built **Hippocampus** dashboard already
provisioned as the home page (`deploy/compose/observability/`) and the shipped
[alert rules](../deploy/observability/README.md) loaded alongside it — the demo runs its store at
the capacity cap by design, so expect the capacity alerts to fire during a soak, which is them
working. The collector is stopped on Ctrl-C. Set
`OBSERVABILITY=0 ./demo/run.sh` to skip it; if no container runtime is found the demo still runs
without metrics/traces. This is the recommended way to watch a soak run — the consolidation and
eviction volume, `hippocampus.sleep.duration`, `hippocampus.used_bytes`, and
`hippocampus.bytes.evicted` all become visible in real time alongside the generator's own latency
log lines.

## What the generator does

| Worker (count)  | Behaviour                                                                     |
| --------------- | ----------------------------------------------------------------------------- |
| bursty (3)      | Creates a backdated event, then floods it with 20-200 memories in seconds     |
| slow (4)        | Creates a live event and trickles memories into it for 1-5 minutes            |
| loose (2)       | Stores backdated, low-significance memories with no event                     |
| query (3)       | Range queries over events/memories, lookups by id, and reinforcing recalls    |
| mutator (1)     | Significance updates, ending/merging/deleting events and memories, manual sleeps |

The demo config compresses time: `consolidation.unitsOfAgeInDays` is 0.002, making one age unit
roughly three minutes, so decay that would take days in production plays out within a session.
Bursty and loose data is backdated by up to 30 minutes (~10 age units) to spread the initial
ages, the two-minute sleep cycle forgets the less significant material as it decays, and
recalled memories have their decay clock reset — the recall workers visibly keep a slice of
older data alive. The service's own byte capacity target (`consolidation.capacityBytes`,
200 MB in the demo config) evicts the least valuable memories each sleep cycle once the store
exceeds it — reclaiming down to the 180 MB floor (`consolidation.capacityBytesFloor`) so
evictions are spaced out rather than trimming a sliver every cycle — and the store oscillates
around that bound while the generator's 1 GiB pause acts only as a backstop.

Memory bodies are mostly small text, with occasional blobs up to ~512 KiB (some stored as
base64 "binary" bodies). A watcher checks the database size (including the WAL) every five
seconds and pauses all writers at 1 GiB; querying and recalling continue, and writing resumes
once consolidation shrinks the database below 90% of the cap.

Every RPC the generator issues is timed, and each 30-second statistics tick logs per-class
latency lines (`rpc latency`: write/read/recall/sleep, with p50/p95/p99/max covering just that
interval). The interval scoping is the point: the service's single database connection means a
long consolidation scan queues RPCs behind it, so a sleep cycle at scale shows up as a spike in
that tick's percentiles — the `sleep` class itself is the manual `Sleep` RPC, whose latency is
the cycle's duration.

## Tuning

Generator flags (see `demo/generator/main.go`): `--address`, `--data_dir`, `--max_bytes`,
`--seed` (0 seeds from the clock; set it for a reproducible run), `--log_level`, and per-type
worker counts (`--bursty_workers`, `--slow_workers`, `--loose_workers`, `--query_workers`,
`--mutator_workers`).

Service behaviour is tuned in `demo/config.json` — notably `sleep.periodSeconds` (how often
consolidation runs) and the `consolidation` block (how aggressively it forgets).

## The Bluesky firehose demo (`./demo/bluesky.sh`)

A second, quite different demo: instead of a synthetic generator, it consumes the **live public
Bluesky firehose** and lets the decay model run on real data arriving in real time.

```sh
./demo/bluesky.sh                 # the whole network
LANGS=en ./demo/bluesky.sh        # only posts tagged English
DIDS=did:plc:xxxx ./demo/bluesky.sh   # follow specific accounts instead
```

It builds the service plus the `bluesky` bridge from `integrations/eventsource`, runs the service on
`demo/config.bluesky.json` (port 8300, gateway 8080, store under `demo/data-bluesky`), and points
the bridge at Jetstream. `SEARCH` and `OBSERVABILITY` behave exactly as in `run.sh`.

### What it demonstrates

Every post arrives with the **same significance** (`SIGNIFICANCE`, default 10). What differentiates
them afterwards is engagement: a like, a repost or a reply is turned into a `RecallMemories` call
against that post, which resets its decay clock and raises its effective significance. So the store
sorts itself purely by what people came back to.

That mapping needs **no state at all**: a memory's id is the post's `at://` URI and a like names its
target by that same URI, so the bridge holds no map and does no lookup. A like for a post it never
ingested, or one the store has already forgotten, costs one `UPDATE` that matches no rows.

With the shipped settings (`unitsOfAgeInDays: 0.002`, so one age unit is ~2.9 minutes, power-law
decay, threshold 5, `recallSignificanceWeight: 5`) a post's lifetime is about
`34.6 x effective-significance / capacity-pressure` seconds:

| likes since last | effective significance | lifetime at pressure x1 |
|---|---|---|
| 0 | 10 | 5m 46s |
| 1 | 15 | 8m 38s |
| 3 | 25 | 14m 24s |
| 10 | 60 | 34m 34s |

and each of those clocks restarts on the *next* like. Within half an hour you see the flat mass of
unengaged posts turning over every few minutes, a visible tail of once- or twice-liked posts, and a
handful that simply never leave.

### What to look at

- **Decay tab** (`http://localhost:8080/ui`) — the capacity pressure and scaled threshold tiles,
  then the "what would be forgotten now" dry run: it fills with unengaged posts and stays empty of
  liked ones. That is the whole demo in one screenshot.
- **Memories tab** — click the value cell of a post with `recall_count > 5` and one with 0, and put
  the two decay curves side by side.
- **Search tab** — free-text over live posts. The `Reinforce` toggle makes searching a trending term
  a second, manual demonstration of the same mechanism.
- **Grafana** (`:3000`) — capacity pressure, used bytes, consolidation and eviction rates. The
  bridge's own metrics have no shipped panels; the one worth adding by hand in Explore is the hit
  rate, which is what says the decay model is doing anything:

  ```promql
  sum(rate(hippocampus_bridge_recalls_total{outcome="reinforced"}[5m]))
    / sum(rate(hippocampus_bridge_recalls_total[5m]))
  ```

### Notes from running it

- **The capacity defaults are measured, not calculated.** A run on the open firehose settles around
  1,270 memories using ~1.9 MB (about 1,500 bytes each — `UsedBytes` counts live SQLite pages
  including indexes). The caps sit just under that so eviction and pressure actually engage; set
  them well above the equilibrium and the Decay tab shows a flat `x1.00` all session. Narrowing the
  stream with `LANGS`/`DIDS` lowers the arrival rate and so the equilibrium too — the script prints
  the observed ratio five minutes in.
- **Threads are sparse on the open firehose.** `EVENTS=thread` opens an event per thread root, but
  sampling the whole network means you rarely see both a root post and its replies, so most events
  end up holding one memory. `DIDS=...` is where threading gets interesting, because following a few
  accounts gives you whole conversations. `EVENTS=none` is also markedly faster — thread mode costs
  a second RPC per post.
- **Deletions are honoured** (`--honour-deletes`, on by default). Decay is about significance;
  deletion is about consent, and on Bluesky deleting a post is the only withdrawal a person has.
- **The data is other people's public posts**, kept locally in `demo/data-bluesky` (gitignored) and
  published nowhere. `rm -rf demo/data-bluesky` removes it.
