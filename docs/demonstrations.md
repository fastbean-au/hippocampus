# Demonstrations

Worked demonstrations that load real-shaped data into Hippocampus and show consolidation doing its
job. They cross two **data shapes** — narratives and logs — with the two **deployment modes**
(embedded SQLite, and centralised Postgres + OpenSearch), using the companion data generator.

For a purely synthetic, self-contained soak (no external data, bursty writers, live decay under a
byte cap) see the built-in [`demo/`](../demo/README.md) harness (`./demo/run.sh`) instead — that
answers "does it stay healthy under sustained load"; the demonstrations here answer "what does
forgetting look like on data you recognise".

## The data generator

The generators live in the companion repository
[`hippocampus-gen`](https://github.com/fastbean-au/hippocampus-gen), checked out beside this one
(`../hippocampus-gen`). It is a separate Go module with a `replace` directive pointing at this
project, so it always builds against your local contract. Three commands:

| Command | Data shape | What it produces |
|---|---|---|
| `cmd/book` | Narrative | Charles Dickens' *Great Expectations*: one **event per chapter** (I–LIX), one **memory per paragraph** (~3,850). |
| `cmd/logs` | Logs | Synthetic service logs: one **memory per log line**, significance derived from the line's **level**, tagged with its service via the **group** label, bucketed into one **event per service per day**. |
| `cmd/random` | Synthetic | A wordlist-driven load generator (meaningless text) for throughput/load testing. |

Each takes `-s <host:port>` for the target gRPC address (default `localhost:50051`). They speak
plain gRPC with no auth, so point them at a demonstration instance, not a secured deployment.

## Embedded mode (SQLite)

Start a local instance with a minimal SQLite config (see
[Getting started](getting-started.md#a-minimal-configuration)); the examples below assume gRPC on
`:50051` and the HTTP gateway on `:8081`.

### Narrative — the book

```sh
cd ../hippocampus-gen
go run ./cmd/book -s localhost:50051
```

This streams the novel in reading order: each chapter becomes an event, each paragraph a memory,
with timestamps stepping forward across a ~2-year window (the book's internal timeline, not real
dates). A clean run stores **59 events and ~3,850 memories**.

Because the memories carry genuine prose, this is the demonstration to use when showing **content
search** (OpenSearch, below) or **summarization** — an event's paragraphs are exactly the kind of
piled-up, gone-quiet detail the sleep cycle surfaces as a summarization candidate.

**The recall-reinforcement wrinkle.** Recalling a memory reinforces it (resets its decay clock,
raises its effective significance), which is right for episodic/operational memory where "what you
keep returning to matters most". A narrative is the case where that intuition can *invert*: the
paragraphs a reader has already revisited are the ones they no longer need surfaced, while the
un-recalled passages are the ones still worth keeping available. If you are modelling
consumption rather than importance, consider leaving `RecallMemories`' reinforcement out of that
path (read without recalling), or even inverting it — consolidate the most-retrieved and keep the
unread. Hippocampus does not bake in a stance here; the demonstration just makes the tension visible.

### Logs — significance-driven forgetting

```sh
cd ../hippocampus-gen
go run ./cmd/logs -s localhost:50051 -n 3000 -d 20   # 3,000 lines across 20 days
```

Every line's significance is set from its level (`DEBUG` lowest … `FATAL` highest) and the emitting
service goes into the `group` label. This is the demonstration that makes the core value
proposition concrete: **routine noise is forgotten first, errors survive**. After loading 3,000
lines and running one sleep cycle with decay tuned to bite within the 20-day window
(`minimumAgeInDays: 1`, `deletionThreshold: 2000`, `method: 1`), survival ranks cleanly by severity:

| Level | Before | After | Survived |
|---|---|---|---|
| DEBUG | 1209 | 167 | 14% |
| INFO | 1177 | 321 | 27% |
| WARN | 384 | 253 | 66% |
| ERROR | 209 | 204 | 98% |
| FATAL | 21 | 21 | 100% |

(3,000 → 966 memories in one cycle.) The exact figures depend on the decay settings and the age
spread; the *shape* — monotonic survival by significance — is the point. Trigger the cycle with the
`Sleep` RPC (`POST /v1/sleep`) or let the timed cycle run. Filter by service with the `group` field
on `GetMemories`/`GetEvents` (see [Grouping](configuration.md)).

To watch that shape at the edges, both list endpoints accept a `significance_extremum` parameter
(`SIGNIFICANCE_EXTREMUM_HIGHEST` / `SIGNIFICANCE_EXTREMUM_LOWEST`) that returns only the items tied
at the highest or lowest significance among those matching the other filters — the lowest set being
precisely what the next cycle forgets first, the highest set the most durable. The web console
(`/ui`) exposes it on both the **Memories** and **Events** tabs as a *Significance → Highest/Lowest
only* selector, so the about-to-be-forgotten tier is one click away during a soak.

## Centralised mode (Postgres + OpenSearch)

The same generators drive a centralised deployment unchanged — only the target address differs. The
`corporate` compose stack runs the Postgres driver with the OpenSearch content-search index and an
OTEL collector:

```sh
docker compose -f docker/docker-compose.corporate.yaml up --build
```

It exposes gRPC on `:50051` and the gateway on `:8080`. Load either generator against it:

```sh
cd ../hippocampus-gen
go run ./cmd/logs -s localhost:50051 -n 20000 -d 60
```

With OpenSearch enabled, the book demonstration additionally exercises **content search**: after
loading, `POST /v1/memories/search` (or the console at `/ui`) finds paragraphs by content, always
re-reading hits from the primary store so consolidated memories drop out of results. See
[Content search](configuration.md#content-search-opensearch). Grafana is on `:3000` for the
consolidation/eviction metrics while the sleep cycle runs.

## What to look at

- **Counts before/after a sleep cycle**, sliced by level (logs) or by chapter/event (book) — the
  histogram of what survived is the clearest read on the decay model.
- **Capacity eviction** — set `consolidation.capacityBytes` (or `capacityMemories`) below the loaded
  size and watch eviction remove the least-valuable memories to hit the target
  (see [Capacity target](consolidation.md#capacity-target)).
- **Observability** — with the `corporate` stack (or `./demo/run.sh`, which launches it by default),
  the provisioned Grafana dashboard shows `memories.consolidated`, `memories.evicted`, `used_bytes`,
  and `capacity_pressure` per cycle.
