# Performance under sustained high throughput

![Hippocampus](go-hippocampus.png)

This note reports how Hippocampus behaves when writes arrive far faster than a comfortable
steady state — driving each storage backend from a modest fraction of its byte capacity per second
up to and beyond the capacity per second — and how the sleep cycle (consolidation + eviction, the
"reaping") copes. It complements the 48-hour stability soak and the regression benchmarks noted in
the project history; those answer "is it stable over days" and "did the sleep scan regress", while
this one answers "where does each backend saturate, and does forgetting keep the store bounded when
it does". For deployment sizing and tuning see [Operations](operations.md); for the capacity target
and decay model see [Consolidation](consolidation.md#capacity-target).

> **One real bug fell out of this exercise** — under a configured `walTriggerBytes` the timed sleep
> cycle could stop firing, so a store under sustained load would eventually stop forgetting. It is
> fixed; see [The livelock this surfaced](#the-livelock-this-surfaced-fixed).

## Method

A **throughput ladder**: for step _k_ = 1…10 the load generator drives a target accepted-write rate
of _k_ × 10 % of the store's byte capacity per second, enforced by a byte token-bucket
(`demo/generator`, `--target_bytes_per_sec`). The throttle is an **upper bound** — comparing the
achieved rate against the target is exactly how saturation shows up. Bodies are a fixed ~64 KB
(`--body_bytes`) so a byte target is met at a sane request rate rather than being dominated by
per-RPC overhead. Each step runs for 180 s — many 15 s sleep cycles — so reaping reaches steady
state. A population sampler (`--sample_interval_seconds`) records the store's shape (event/memory
counts and the memory-significance distribution) each interval; the service's own
`capacity pressure: P (N memories, B bytes used)` log line supplies the driver-independent reaping
signal.

The numbers below use a **reduced 50 MB byte capacity** (`consolidation.capacityBytes`, floor
45 MB) so the whole sweep fits on a disk-constrained test host; percentages of capacity are the
portable quantity, and the shape of every result carries over to a larger cap. Re-run at
`capacityBytes: 200000000` (or higher) for full-scale absolute numbers. SQLite runs against a local
file with `walTriggerBytes` enabled and the generator's `MAX_BYTES` file-size backstop providing
flow control; Postgres and MySQL run in throwaway containers on the default image configuration with
the 25-connection pool, no WAL trigger (they have no client-visible WAL file), and no file-size
backstop (the store is bounded only by eviction). Harness: `demo/generator` plus a small sweep
runner; see [Reproducing](#reproducing).

## Results by backend

Columns: **achieved MB/s** and **achieved %/s** are the mean over the second half of the step (the
`%/s` is against the 50 MB cap); **steady/peak used MB** is the service's live-row byte estimate
(what eviction targets), oscillating around the 45 MB floor; **write p99** is the client-measured
write latency for that step; **pauses** counts generator backpressure pauses.

### SQLite (single connection)

| step | target MB/s | achieved MB/s | achieved %/s | steady used MB | peak used MB | evictions | write p99 ms | pauses |
| ---: | ----------: | ------------: | -----------: | -------------: | -----------: | --------: | -----------: | -----: |
|    1 |           5 |           3.5 |          7.1 |             66 |          102 |        11 |          182 |      0 |
|    2 |          10 |           8.7 |         17.5 |             76 |          151 |         6 |          180 |      0 |
|    3 |          15 |          11.7 |         23.3 |            114 |          216 |         5 |           84 |      5 |
|    4 |          20 |          15.7 |         31.3 |            115 |          307 |         5 |           98 |      4 |
|    5 |          25 |          18.3 |         36.7 |            165 |          350 |         4 |           55 |      6 |
|    6 |          30 |          19.0 |         38.0 |            226 |          444 |         4 |           61 |      8 |
|    7 |          35 |          17.7 |         35.3 |            226 |          500 |         4 |           47 |      7 |
|    8 |          40 |          14.7 |         29.3 |            191 |          404 |         4 |           38 |      7 |
|    9 |          45 |          18.7 |         37.3 |            299 |          464 |         3 |           30 |      7 |
|   10 |          50 |          17.7 |         35.3 |            347 |          521 |         3 |           32 |      6 |

- **Throughput ceiling ~18–19 MB/s.** Achieved tracks target through steps 1–2, then plateaus from
  step 3 on. The single writer connection is the bottleneck: every write serialises against the
  consolidation scan and the WAL checkpoint on the same connection.
- **Reaping stays bounded but overshoots with load.** Steady used-bytes climbs from ~66 MB to
  ~347 MB (up to ~7× the 50 MB cap) as the offered rate rises — eviction claws the store back each
  cycle, but between cycles it climbs further the harder you push. It never runs away.
- **Backpressure engages** (pauses 0 → 8): once the WAL/file hits the generator's cap, writes pause
  until a checkpoint reclaims space. Write p99 is highest at the low steps (the 15 s sleep + eviction
  briefly blocks the one connection) and falls at high steps as backpressure paces the writers.
- SQLite's WAL does not truncate while readers are active, so the file grows to a high-water mark
  under sustained writes; here it is bounded by the backpressure pause plus `walTriggerBytes`. Size
  the disk for the peak, not the steady state (see [Operations](operations.md#choosing-and-sizing-a-storage-backend)).

### PostgreSQL (25-connection pool)

| step | target MB/s | achieved MB/s | achieved %/s | steady used MB | peak used MB | evictions | write p99 ms | pauses |
| ---: | ----------: | ------------: | -----------: | -------------: | -----------: | --------: | -----------: | -----: |
|    1 |           5 |           4.3 |          8.5 |             67 |           88 |        12 |         12.9 |      0 |
|    2 |          10 |           8.5 |         17.0 |             70 |          151 |         6 |         10.1 |      0 |
|    3 |          15 |          13.3 |         26.7 |            100 |          218 |         6 |          8.3 |      0 |
|    4 |          20 |          18.3 |         36.7 |            132 |          268 |         6 |          7.2 |      0 |
|    5 |          25 |          23.0 |         46.0 |            164 |          350 |         6 |         14.4 |      0 |
|    6 |          30 |          27.7 |         55.3 |            194 |          423 |         6 |         14.5 |      0 |
|    7 |          35 |          33.0 |         66.0 |            241 |          499 |         6 |         13.7 |      0 |
|    8 |          40 |          39.0 |         78.0 |            256 |          591 |         7 |         33.4 |      0 |
|    9 |          45 |          44.0 |         88.0 |            303 |          648 |         8 |         51.6 |      0 |
|   10 |          50 |          49.0 |         98.0 |            256 |          701 |         9 |         82.5 |      0 |

- **Tracks the target almost linearly to 49 MB/s** (92–98 % of target at every step) — **no
  saturation in the tested range, ~2.6× SQLite's ceiling.** The connection pool lets writes proceed
  on one connection while the consolidation scan runs on another, so they no longer serialise.
- **Write p99 stays 7–15 ms** through step 7, rising to 33/52/82 ms only at steps 8–10 as it
  approaches ~40–50 MB/s. **Zero backpressure pauses** — eviction keeps up throughout.
- The same reaping overshoot as SQLite (used-bytes to ~6× cap between cycles), reclaimed each cycle;
  evictions delete up to ~46 k memories per step and the store stays bounded.
- Postgres retains a lot of WAL/dead-tuple space on disk under this load (autovacuum lags a
  sustained delete stream), so **plan headroom well above the logical `used_bytes`** and rely on
  autovacuum plus routine maintenance rather than immediate file shrinkage.

### MySQL (25-connection pool, default image config)

Steps 1–6 (higher steps were disk-limited on the test host; see [Caveats](#caveats)):

| step | target MB/s | achieved MB/s | achieved %/s | steady used MB | peak used MB | evictions | write p99 ms | pauses |
| ---: | ----------: | ------------: | -----------: | -------------: | -----------: | --------: | -----------: | -----: |
|    1 |           5 |           4.4 |          8.7 |             66 |          109 |         8 |         11.4 |      0 |
|    2 |          10 |           9.8 |         19.7 |             91 |          169 |         6 |          148 |      0 |
|    3 |          15 |          15.0 |         30.0 |            151 |          240 |         6 |           58 |      0 |
|    4 |          20 |          15.3 |         30.7 |            255 |          263 |         2 |          399 |      0 |
|    5 |          25 |          12.3 |         24.7 |            456 |          514 |         2 |          492 |      0 |
|    6 |          30 |          13.0 |         26.0 |            443 |          443 |         1 |          518 |      0 |

- **Tracks target to ~15 MB/s (step 3), then plateaus and degrades** to ~12–15 MB/s — a ceiling
  closer to SQLite than to Postgres.
- **Write p99 climbs sharply under load: ~400–520 ms at steps 4–6** (vs Postgres's 7–82 ms), and
  **eviction starves** — the sleep cycle's large `DELETE` contends with the write flood, evictions
  drop to 1–2 per step and used-bytes climbs to ~9× the cap.
- This is largely **stock `mysql:8.0` configuration** — a 128 MB InnoDB buffer pool,
  `innodb_flush_log_at_trx_commit=1`, and the doublewrite buffer make each commit fsync-heavy. It is
  exactly the tuning called out in [Operations](operations.md#choosing-and-sizing-a-storage-backend);
  a larger buffer pool and relaxed flush settings move MySQL much closer to Postgres. Both server
  backends here ran their default configs, so this is a fair default-vs-default comparison, not a
  tuned one.
- Under high write concurrency MySQL also throws the occasional InnoDB deadlock (see
  [MySQL InnoDB deadlocks](#mysql-innodb-deadlocks-open)).

## Cross-backend comparison

Achieved MB/s at each target (— = disk-limited on the test host):

| target MB/s |   5 |  10 |   15 |   20 |   25 |   30 |   35 |   40 |   45 |   50 |
| ----------- | --: | --: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| SQLite      | 3.5 | 8.7 | 11.7 | 15.7 | 18.3 | 19.0 | 17.7 | 14.7 | 18.7 | 17.7 |
| PostgreSQL  | 4.3 | 8.5 | 13.3 | 18.3 | 23.0 | 27.7 | 33.0 | 39.0 | 44.0 | 49.0 |
| MySQL       | 4.4 | 9.8 | 15.0 | 15.3 | 12.3 | 13.0 |    — |    — |    — |    — |

Three distinct profiles emerge, all with reaping keeping the store bounded:

- **SQLite** — one connection; writes serialise with the sleep scan and checkpoint, so it saturates
  at **~18–19 MB/s** with write p99 up to ~180 ms and uses client backpressure (pauses) as flow
  control. Ideal for embedded/edge single-writer deployments; not for centralised high write fan-in.
- **PostgreSQL** — the pool decouples writes from consolidation; it **tracks demand to ~49 MB/s with
  no saturation and low latency**, the clear choice for a centralised high-throughput store.
- **MySQL** — same pooled architecture but, at stock config, **fsync-bound around ~15 MB/s with
  latency that degrades badly under load**; tune the InnoDB buffer pool and flush settings before
  relying on it at rate.

## Reaping and the shape of the store through sleep cycles

Across every backend, the sleep cycle held the store bounded even when the write rate exceeded the
byte capacity per second: `used_bytes` oscillates around the eviction floor, spiking between the
15 s cycles by an amount that grows with the offered rate (the "overshoot" columns) and being
reclaimed each cycle. The overshoot is the headroom to budget for: at the top SQLite/Postgres steps
the store briefly reached ~6–7× the configured cap before eviction caught up. Evictions per step
stayed roughly constant for SQLite/Postgres and _fell_ for MySQL as its sleep cycle starved — a
useful signal that a backend can no longer consolidate fast enough to match the write rate.

The **population shape** shifts as forgetting bites. The memory-significance distribution (four
bands, 1–100) tilts upward over a step as consolidation deletes the least-significant memories
first — e.g. one Postgres step moved the lowest band from 34.6 % → 25.2 % of survivors while the top
band rose 15.2 % → 24.2 %, and the SQLite low band fell 32.8 % → 29.6 % as the top rose 18.5 % →
20.2 %. Recall reinforcement keeps a slice of otherwise low-value memories alive, so the shift is a
tilt, not a hard cut, and it is noisier at the highest steps where the surviving population is small
and turns over fastest. The mechanism is the capacity-pressure multiplier: as the store fills,
pressure scales the deletion threshold up (raised to `capacityPressureExponent`), so forgetting
becomes more aggressive exactly when it needs to — visible as the pressure values climbing into the
tens and hundreds in the service log at the high steps.

## Issues found

### The livelock this surfaced (fixed)

With `consolidation.walTriggerBytes > 0` **and** `sleep.periodSeconds` greater than the 5 s WAL
poll interval, the **timed sleep cycle never fired**. `autoSleep`'s select recreated the period
timer (`time.After`) on every loop iteration, and the WAL-poll ticker — firing every 5 s, more often
than the period — restarted that countdown before it could elapse. Consolidation then ran _only_
when the WAL crossed its trigger; when the write rate dropped or writes paused under backpressure,
the WAL fell below the threshold, nothing triggered a cycle, and **the store stopped being
consolidated or evicted entirely** despite a configured sleep period. Combined with size-based
client backpressure this is a livelock: the store never shrinks, so writers never resume. A
goroutine dump of a wedged instance confirmed the service was otherwise healthy and idle — the
timer case simply never fired.

Fixed by using a single long-lived timer, reset after each fire, instead of a fresh `time.After` per
iteration. Regression test `TestAutoSleep_TimedCycleFiresWithWALTriggerEnabled`. Latent before now
because the long-running soak and the default demo did not combine `walTriggerBytes` with a longer
period.

### MySQL InnoDB deadlocks (open)

Under high write concurrency MySQL occasionally aborts a `StoreMemory` transaction with
`Error 1213 (40001): Deadlock found` — about 2 in ~12,700 writes (~0.02 %) at the lowest step. The
service currently surfaces it as gRPC `Unknown`, so the write is simply lost. InnoDB deadlocks are
expected under concurrency and the standard remedy is to retry the transaction; the recommended fix
is a bounded retry-on-1213 in the MySQL write path, or mapping 1213/serialization failures to
`codes.Aborted` so callers retry. Left open pending a decision, as it is rare and arguably a
client-retryable condition.

Two unrelated issues found earlier in the same effort — `GetEventById` returning `Unknown` instead
of `NotFound`, and the demo generator's burst timestamps overflowing the future-timestamp
validation window — are already fixed.

## Reproducing

The generator gained three flags for this: `--target_bytes_per_sec` (byte-rate throttle),
`--body_bytes` (fixed body size), and `--sample_interval_seconds` (population sampler). A single
step, by hand:

```sh
go build -o /tmp/hippo ./cmd/hippocampus
go build -o /tmp/gen   ./demo/generator
# config.json: storage.driver + consolidation.capacityBytes/capacityBytesFloor + sleep.periodSeconds
/tmp/hippo -c config.json &
# target 60% of a 50 MB cap per second = 30 MB/s, ~64 KB bodies, sample every 10 s
/tmp/gen --address localhost:8300 --target_bytes_per_sec 30000000 --body_bytes 65536 \
         --sample_interval_seconds 10 --bursty_workers 12 --loose_workers 6
```

Watch `capacity pressure: … bytes used` in the service log and `population sample` /
`generator statistics` in the generator log. For a live dashboard use `OBSERVABILITY=1
./demo/run.sh` (see [demo/README](../demo/README.md)).

## Caveats

- **Reduced scale.** Capacity was 50 MB so the sweep fit a disk-constrained host; percentages of
  capacity and the shape of the results are what transfer. Absolute MB/s ceilings will differ on
  other hardware and at a larger cap.
- **Default configs.** Postgres and MySQL ran stock container defaults; MySQL in particular is
  expected to improve substantially with InnoDB tuning.
- **Host-limited high steps.** Postgres and MySQL grow their on-disk footprint fast under sustained
  writes (WAL, dead tuples, InnoDB redo/doublewrite); on the test host MySQL's steps beyond 6 could
  not complete for lack of disk, not for any service-level reason. On adequate disk the ladder runs
  to completion.
- **Single instance.** This measures one consolidating instance, the supported model. Read/write
  replicas (server drivers) scale reads independently; see
  [Horizontal scaling](operations.md#horizontal-scaling-with-replicas).
