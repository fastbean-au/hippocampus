# Demonstrations

Worked demonstrations that load real-shaped data into Hippocampus and show consolidation doing its
job. They cross two **data shapes** — narratives and logs — with the two **deployment modes**
(embedded SQLite, and centralised Postgres + OpenSearch), using the companion data generator.

Two others sit beside them, both self-contained in this repository. For **live data nobody staged**,
[`./demo/bluesky.sh`](../demo/README.md#the-bluesky-firehose-demo-demoblueskysh) points the
[Bluesky bridge](eventsource.md#bluesky-the-firehose-bridge) at the public firehose, where real
engagement decides what survives — that is the hosted demo below, and the most convincing of the
three. For a purely synthetic soak (no external data, bursty writers, live decay under a byte cap),
[`./demo/run.sh`](../demo/README.md) answers "does it stay healthy under sustained load"; the
demonstrations here answer "what does forgetting look like on data you recognise".

## The measured one — what forgetting costs you

Every demonstration below shows the **mechanism**: memories arrive, decay, and are forgotten in a
sensible order. None of them shows the **benefit**, and nobody adopts a store because it deletes
things. That question — _of everything it threw away, how much did you actually need later?_ — has
its own answer, and it is a number rather than a screenshot:

> At a store holding a fifth of what was written to it, **every access-based policy is
> statistically indistinguishable from random** at retaining the memories that matter but are not
> touched often. LRU scores 20.2% against random's 19.9%. Hippocampus scores 27.6%, and the margin
> widens with store size — **+11.1 points at a 42% store**.

The full method, the baselines it is measured against, the three checks that stop it being
circular, and its limitations are in **[Retention quality](retention.md)**. It is the demonstration
to read if you are deciding whether the model is worth anything; the ones below are the ones to
watch if you want to see it working.

That comparison also runs **live**, as a pair of hosted consoles: one writer feeds byte-for-byte
identical memories to two stores, and only one of them is told which memories matter. Every decay
method divides significance by a function of age, so with a constant significance the ordering
reduces to pure recency — the flat store is an LRU store arrived at by configuration rather than by a
different algorithm. Open [agent](https://agent.hippocampus-demo.com/ui) and
[agent-flat](https://agent-flat.hippocampus-demo.com/ui), search both for the same thing, and look
for something old that was written as significant.

## The hosted demo — [hippocampus-demo.com](https://hippocampus-demo.com)

Both demonstrations below run continuously at <https://hippocampus-demo.com>, alongside the Bluesky
one, if you would rather watch than load one yourself. Decay, recall reinforcement, and
consolidation are slow by design — they play out over days — so the hosted instances run the same
build with the decay clock compressed (as
[`demo/config.json`](../demo/config.json) does, via `consolidation.unitsOfAgeInDays`), and the whole
cycle happens in minutes. Every console takes a read-only sign-in: **`demo` / `demo`**.

| Site                                                                                                                | What it shows                                                                                                                                                                  |
| ------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| [Agent pair](https://agent.hippocampus-demo.com/ui) and [its flat twin](https://agent-flat.hippocampus-demo.com/ui) | **One workload, two stores.** Identical memories into both; only one is told which matter. Search each for something old and important — it is in one and gone from the other. |
| [Bluesky console](https://bluesky.hippocampus-demo.com/ui)                                                          | Verified news headlines arriving live, all equally significant; likes and reposts reinforce them, replies thread onto them, related coverage links them — the rest decays.     |
| [Book console](https://book.hippocampus-demo.com/ui)                                                                | _Great Expectations_ re-read daily: episodic detail distilled into semantic summaries as it ages, recalled passages holding on.                                                |
| [Logs console](https://logs.hippocampus-demo.com/ui)                                                                | A continuous log stream against a byte capacity target — consolidation and eviction under real storage pressure.                                                               |
| [Grafana dashboard](https://grafana.hippocampus-demo.com)                                                           | Live telemetry from the stacks (the same dashboard the `observability` Compose profile provisions).                                                                            |
| [Config builder](https://config-builder.hippocampus-demo.com)                                                       | The [configuration wizard](config-wizard.md), hosted — build a `config.json` and its deployment artefacts.                                                                     |

**The Bluesky one is the demonstration to open first**, because it is the only one running on data
nobody here controls: real posts, real attention, arriving in real time, with nothing staged. Every
post is stored at the same significance, so engagement is the _only_ differentiator — which makes it
the cleanest statement of what the store is for. It is the [`bluesky`
bridge](eventsource.md#bluesky-the-firehose-bridge) in **feed mode**, which
[`./demo/bluesky.sh`](../demo/README.md#the-bluesky-firehose-demo-demoblueskysh) runs locally with
`FEED` set to a news feed generator (bare, it consumes the open firehose instead). Neither needs an
account or a credential — Jetstream is public — but read that README's decay-clock note first: a
curated feed arrives at ~70 posts an hour rather than ~70 a second, and the shipped clock is tuned
for the latter.

The consoles are the same embedded web console (`/ui`) every instance serves; the **Now** and
**Decay** tabs are where a cycle is visible as it happens. The demo credential resolves to the
`reader` [tier](configuration.md#authorisation), which is why the write controls are absent.

## The data generator

The generators live in the companion repository
[`hippocampus-gen`](https://github.com/fastbean-au/hippocampus-gen), checked out beside this one
(`../hippocampus-gen`). It is a separate Go module with a `replace` directive pointing at this
project, so it always builds against your local contract. Three commands:

| Command      | Data shape | What it produces                                                                                                                                                                                       |
| ------------ | ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `cmd/book`   | Narrative  | Charles Dickens' _Great Expectations_: one **event per chapter** (I–LIX), one **memory per paragraph** (~3,850).                                                                                       |
| `cmd/logs`   | Logs       | Synthetic service logs: one **memory per log line**, significance derived from the line's **level**, tagged with its service via the **group** label, bucketed into one **event per service per day**. |
| `cmd/random` | Synthetic  | A wordlist-driven load generator (meaningless text) for throughput/load testing.                                                                                                                       |

Each takes `-s <host:port>` for the target gRPC address (default `localhost:50051`). They speak
plain gRPC with no auth, so point them at a demonstration instance, not a secured deployment.

## Embedded mode (SQLite)

Start a local instance with a minimal SQLite config (see
[Getting started](getting-started.md#a-minimal-configuration)); the examples below assume gRPC on
`:50051` and the HTTP gateway on `:8080`.

### Narrative — the book

```sh
cd ../hippocampus-gen
go run ./cmd/book -s localhost:50051
```

This streams the novel in reading order: each chapter becomes an event, each paragraph a memory,
with timestamps stepping forward across a ~2-year window (the book's internal timeline, not real
dates). A clean run stores **59 events and ~3,850 memories**.

Because the memories carry genuine prose, this is the demonstration to use when showing **content
search** (OpenSearch, below) or **summarisation** — an event's paragraphs are exactly the kind of
piled-up, gone-quiet detail the sleep cycle surfaces as a summarisation candidate.

**The recall-reinforcement wrinkle.** Recalling a memory reinforces it (resets its decay clock,
raises its effective significance), which is right for episodic/operational memory where "what you
keep returning to matters most". A narrative is the case where that intuition can _invert_: the
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
| ----- | ------ | ----- | -------- |
| DEBUG | 1209   | 167   | 14%      |
| INFO  | 1177   | 321   | 27%      |
| WARN  | 384    | 253   | 66%      |
| ERROR | 209    | 204   | 98%      |
| FATAL | 21     | 21    | 100%     |

(3,000 → 966 memories in one cycle.) The exact figures depend on the decay settings and the age
spread; the _shape_ — monotonic survival by significance — is the point. Trigger the cycle with the
`Sleep` RPC (`POST /v1/sleep`) or let the timed cycle run. Filter by service with the `group` field
on `GetMemories`/`GetEvents` (see [Grouping](configuration.md)).

To watch that shape at the edges, both list endpoints accept a `significance_extremum` parameter
(`SIGNIFICANCE_EXTREMUM_HIGHEST` / `SIGNIFICANCE_EXTREMUM_LOWEST`) that returns only the items tied
at the highest or lowest significance among those matching the other filters — the lowest set being
precisely what the next cycle forgets first, the highest set the most durable. The web console
(`/ui`) exposes it on both the **Memories** and **Events** tabs as a _Significance → Highest/Lowest
only_ selector, so the about-to-be-forgotten tier is one click away during a soak.

### Logs via the OpenTelemetry Collector

The `cmd/logs` generator above synthesises log lines directly. To ingest **real** logs — from files,
or from any OTel-instrumented application — Hippocampus ships an OpenTelemetry Collector **logs
exporter** (`integrations/otel/hippocampusexporter/`). Dropped into a collector pipeline
(`filelog`/`otlp` receiver → `batch` → `hippocampus`), it turns each log record into a memory:
severity (`SeverityNumber`, falling back to `SeverityText`) drives significance, `service.name`
becomes the `group`, and — with `create_events: true` — records are bucketed into events keyed by
configurable attributes (`event_key_from`, `event_bucket`). The default significance table matches
the one above, so the same **routine noise forgotten first, errors survive** result holds, now from a
live pipeline rather than the generator.

```sh
go install go.opentelemetry.io/collector/cmd/builder@v0.157.0
cd integrations/otel/collector
builder --config builder-config.yaml
./_build/hippocampus-otelcol --config config.yaml   # tails integrations/otel/collector/sample.log
```

Ingesting the bundled 12-line `sample.log` produces 12 memories (monotonic significance from `DEBUG`
to `FATAL`, one event for the day); a `Sleep` cycle with decay tuned to bite then forgets the
low-severity tiers first, leaving the `ERROR`/`FATAL` survivors. See
[`integrations/otel/collector/README.md`](../integrations/otel/collector/README.md) for the full walkthrough and
[`integrations/otel/hippocampusexporter/README.md`](../integrations/otel/hippocampusexporter/README.md) for the exporter's
configuration (auth/TLS, the significance table, and the event-keying options).

## Centralised mode (Postgres + OpenSearch)

The same generators drive a centralised deployment unchanged — only the target address differs. The
`corporate` compose stack runs the Postgres driver with the OpenSearch content-search index and an
OTEL collector:

```sh
docker compose -f deploy/compose/docker-compose.corporate.yaml up --build
```

It exposes gRPC on `:50051` and the gateway on `:8080`. Load either generator against it:

```sh
cd ../hippocampus-gen
go run ./cmd/logs -s localhost:50051 -n 20000 -d 60
```

With OpenSearch enabled, the book demonstration additionally exercises **content search**: after
loading, `POST /v1/memories/search` (or the console at `/ui`) finds paragraphs by content, always
re-reading hits from the primary store so consolidated memories drop out of results. See
[Content search](configuration.md#content-search). Grafana is on `:3000` for the
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
