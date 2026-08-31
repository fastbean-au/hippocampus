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

## Unattended soak runs (`demo/soak.sh`)

`run.sh` runs until you stop it. `soak.sh` wraps it in the thing item 20 actually asks for: a
bounded run that samples itself and writes a report with verdicts.

```sh
./demo/soak.sh --hours 4                    # SQLite + OpenSearch (the default profile)
./demo/soak.sh --hours 4 --profile sqlite   # no search backend
./demo/soak.sh --hours 4 -- --bursty_workers 6
```

It drives `run.sh` rather than duplicating it, and adds four things: the duration, a sample every
five minutes into a CSV, a disk-space floor that stops the run rather than filling the host, and
`demo/soak/report.py`, which turns the samples into a verdict per check. Everything lands in
`demo/soak-runs/<timestamp>-<profile>/` — the generated config, `samples.csv`, `run.log` and
`report.md`. The report is the artefact worth keeping; the directory also holds the run's whole
store, so it is gitignored.

**Why SQLite + OpenSearch is the default profile.** `demo/config.json` has OpenSearch off, so
before this there was no soak path that touched item 84's delete outbox or its reverse sweep at
all — and those exist to close a defect (the index leaking stale documents under sustained write
load, found at 20.7x more documents than rows on a live host) that only appears under exactly the
load a soak applies. That check is the one in the report that reads `Index divergence`.

What it checks, and why each one is there rather than left to a reading of the log:

| Check                              | Looking for                                                                                                                                                                                                                                                                                                  |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Goroutines                         | A leak. The failure a clean log will never show, and the one item 20 names first.                                                                                                                                                                                                                            |
| Resident memory / Go mapped memory | The same, in bytes; RSS is read from `ps`, since a Go process cannot report its own honestly.                                                                                                                                                                                                                |
| Sleep cycle                        | Degradation. The cycle has grown from one scan to roughly six, and the question is whether that shows over hours. Judged on the **mean** cycle time, never a quantile — at a 120s sleep period a window holds ~8 cycles, and a p95 over 8 observations is a histogram bucket edge rather than a measurement. |
| Eviction convergence               | `used_bytes` settling at the target rather than climbing past it or oscillating.                                                                                                                                                                                                                             |
| Index divergence                   | Item 84: OpenSearch documents against store rows. 1.0 is agreement.                                                                                                                                                                                                                                          |
| Outbox drain                       | Queued deletions draining rather than accumulating.                                                                                                                                                                                                                                                          |
| Index queue drops                  | Expected under load; it is the recovery, not the drop, that matters.                                                                                                                                                                                                                                         |
| Faults                             | Panics, failed sleep cycles, and the RED server-error rate against the shipped alert's own 1% threshold.                                                                                                                                                                                                     |
| Log                                | Error and warning lines, deduplicated with counts.                                                                                                                                                                                                                                                           |

A check whose data is missing reports `UNKNOWN` rather than passing, because a check that could not
run is not a check that passed. The exit code is non-zero only on a `FAIL`.

Three checks additionally refuse to reach a verdict the evidence cannot support, because a report
that cries wolf teaches its reader to ignore it: a growth series that **rose and then levelled off**
is a working set filling, not a leak; the sleep-cycle trend is measured **only once the store stops
growing**, since a cycle scanning a filling store gets slower by construction; and eviction is judged
on whether it **ever brings the store back under the target**, not on the final sample, because the
target is enforced once per cycle and the store sawtooths around it. `demo/soak/report_test.py`
(`python3 -m unittest discover -s demo/soak`, no dependencies) pairs each of those with a test
proving the genuine fault is still caught.

### Watching something already running

```sh
./demo/soak.sh --observe-only --hours 4 \
    --prometheus http://127.0.0.1:9090 \
    --opensearch http://127.0.0.1:9200 --index agent-memories
```

Samples a deployment instead of launching one — nothing is generated, started, or stopped, and the
index is never deleted. Same checks, same report. It exists because a deployment that has been up
for weeks is a better long-duration instrument than any four-hour run: both of the findings no test
produced (the search index leaking stale documents, a bridge wedged for hours) came from one.

It cannot replace the per-driver runs above, which need a controlled workload against a known
starting state. And one caveat matters more than it looks: if several instances report to one
collector without an instance label, their metrics collapse into one series and the samples describe
no single process. The report detects that — a monotonic counter running backwards is unambiguous —
and reports `Metric attribution` as a failure, marking every cross-series check UNKNOWN rather than
producing a confident, false verdict. `--selector 'job="agent"'` scopes the run where the collector
does distinguish instances.

**The byte cap is tightened, deliberately.** A soak generates its config from `demo/config.json` but
overrides `consolidation.capacityBytes` to 70 MB (floor 63 MB). The demo's own 200 MB sits _above_
the equilibrium the generator settles at, so `evict()` never runs — the four-hour run on 2026-08-30
settled at 96–111 MiB and produced not one eviction line, leaving the whole eviction path untested
while capacity _pressure_ worked fine and reached 1.85. `--capacity-bytes 0` restores the demo's
value; any other number sets it.

The soak uses its own ports (gRPC 8400, gateway 8480, Grafana 3030), its own OpenSearch index
(`hippocampus-soak`, deleted at both ends of the run unless `--keep-index`), and its own container
names, so it can run beside a demo. `--profile postgres`/`--profile mysql` need `SOAK_POSTGRES_DSN`
or `SOAK_MYSQL_DSN` pointing at a disposable database; add `OPENSEARCH=1` to give either a search
backend as well.

## What the generator does

| Worker (count) | Behaviour                                                                        |
| -------------- | -------------------------------------------------------------------------------- |
| bursty (3)     | Creates a backdated event, then floods it with 20-200 memories in seconds        |
| slow (4)       | Creates a live event and trickles memories into it for 1-5 minutes               |
| loose (2)      | Stores backdated, low-significance memories with no event                        |
| query (3)      | Range queries over events/memories, lookups by id, and reinforcing recalls       |
| mutator (1)    | Significance updates, ending/merging/deleting events and memories, manual sleeps |

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
| ---------------- | ---------------------- | ----------------------- |
| 0                | 10                     | 5m 46s                  |
| 1                | 15                     | 8m 38s                  |
| 3                | 25                     | 14m 24s                 |
| 10               | 60                     | 34m 34s                 |

and each of those clocks restarts on the _next_ like. Within half an hour you see the flat mass of
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

### Related stories

`TOPIC_LINKS` (on by default) relates posts about the same story, and
`consolidation.linkRecallPropagation: 0.25` in the config is what makes those links _do_ something:
when a post is liked, its related posts have their decay clocks pulled a quarter of the way back
towards "just recalled" too. So a story several outlets covered survives as a cluster, while a lone
post with the same engagement does not — `linkSignificanceWeight` adds the second half of that, since
a well-connected memory carries more effective significance.

The terms come from the post's link-card URL: a news URL's path is a slug someone wrote by hand
(`/politics/2026/08/samuel-alito-ethics-conflicts-interest-fossil`), which is a keyword list already
tokenised on hyphens. No NLP, no model, no dependency. Posts with no link card fall back to their
own text.

This is **much better with `FEED` set**, where nearly every post carries a link card. On the open
firehose most posts do not, so the terms come from the text and the links are noisier.

### Using a curated feed instead

```sh
FEED='at://did:plc:kkf4naxqmweop7dv4l2iqqf5/app.bsky.feed.generator/news-2-0' ./demo/bluesky.sh
```

Posts then come from that feed generator — ~500 headlines from verified news organisations, seeded at
startup — while likes and reposts keep arriving on the firehose to reinforce them. Every memory is
something you can read, which makes the Decay tab far more legible than the open firehose.

It arrives at roughly 70 posts an hour rather than 70 a second, so the shipped decay clock is much too
fast for it: raise `unitsOfAgeInDays` (one age unit of a few hours rather than three minutes) and
lower the capacity caps to match, or the store will hold a handful of memories and turn over before
you can look at it. See [the feed section](../docs/eventsource.md#curated-feeds-instead-of-the-firehose).

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
