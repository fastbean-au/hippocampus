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
OBSERVABILITY=1 ./demo/run.sh
```

With `OBSERVABILITY` set, `run.sh` launches an all-in-one `grafana/otel-lgtm` collector (needs
`docker` or `podman`), enables the service's OTLP metrics and traces (via `HIPPOCAMPUS_*` env
overrides, so `demo/config.json` is untouched), and points them at the collector. Grafana comes up
at [http://localhost:3000](http://localhost:3000) with a pre-built **Hippocampus** dashboard already
provisioned as the home page (`docker/observability/`); the collector is stopped on Ctrl-C. Left
unset, metrics stay off and nothing is exported. This is the recommended way to watch a soak run —
the consolidation and eviction volume, `hippocampus.sleep.duration`, `hippocampus.used_bytes`, and
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
