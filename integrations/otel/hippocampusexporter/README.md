# Hippocampus OpenTelemetry logs exporter

An [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) **logs exporter** that stores
log records as Hippocampus memories over the service's gRPC contract. Each log record becomes a
memory whose significance is derived from the record's severity, so the Hippocampus consolidation
(decay) cycle forgets routine `DEBUG`/`INFO` noise first and keeps `ERROR`/`FATAL` — significance-by-
severity survival becomes your retention policy. See the repository
[demonstrations](../../../docs/demonstrations.md) for the shape of that result.

This is its own Go module (the collector dependency tree is large and kept out of the root module).
It is a thin gRPC client of a running Hippocampus instance — it holds no state beyond the currently
open events.

## Mapping

| Log record | Hippocampus memory |
|---|---|
| `Body` (or an attribute named by `body_from`) | `body` |
| `SeverityNumber` (falling back to `SeverityText`) | `significance` via the `significance` table, jittered and clamped |
| `Timestamp` (falling back to `ObservedTimestamp`) | `time_stamp` (a future timestamp is clamped to now, so the service's clock-skew guard never rejects it) |
| attribute named by `group_from` (default `service.name`) | `group` (else `default_group`) |
| `TraceID`/`SpanID`, the attributes named by `metadata_from`, those carrying `metadata_prefix`, and the fixed `metadata` labels | `metadata` (see below) |
| — | `event_id`, when `create_events` is true (see below) |

### Events

With `create_events: true` (the default), records are also bucketed into **events**. The event a
record belongs to is decided by a **key** built from `event_key_from` — an ordered list of
resource/log attribute names whose values are joined — plus an optional `event_bucket` time suffix
(`none`, `hour`, or `day`). When the bucket rolls, the prior event is ended and a new one opened.
The human-readable event name is rendered from `event_name_template` (`{key}` and `{bucket}`).

Examples:

- `event_key_from: [service.name]`, `event_bucket: day` → one event per service per day (the
  `cmd/logs` demonstration scheme).
- `event_key_from: [service.name, k8s.pod.name]`, `event_bucket: hour` → one event per pod per hour.

Event bookkeeping is best-effort: if an event can't be created, the memory is still stored, just
without an `event_id`.

### Metadata

A memory's `metadata` is what makes it findable later: `GetMemories` and `SearchMemories` filter on
it, so an attribute not recorded at write time cannot be recovered afterwards. Four selections feed
it, applied in this order, each overriding the last on a shared key:

| Setting | What it records |
|---|---|
| `metadata` | fixed labels stamped on every memory (`{pipeline: logs}`) |
| `trace_metadata` (default **true**) | the record's `TraceID`/`SpanID` as `trace_id`/`span_id`, in the lower-case hex every other tool prints |
| `metadata_from` | the named attributes, resolved record → scope → resource, most specific winning |
| `metadata_prefix` | every attribute whose name carries the prefix, which is stripped from the key (`app.memory.tenant` → `tenant`) |

`trace_metadata` is on by default because correlating a memory that survived the decay cycle back to
the trace that produced it is the reason a log pipeline would choose this store, and no later query
can recover an id that was never written.

`metadata_from` and `metadata_prefix` are opt-in **selections** rather than "copy every attribute":
a record's attribute set is unbounded and mostly machinery, so copying it wholesale would fill each
memory's metadata budget with noise and hand the key space to whatever instrumented the application.

Keys are normalised to the service's metadata charset (lower-cased, anything outside
`[A-Za-z0-9._:/-]` replaced with `_`), so semantic-convention names such as `http.status_code` and
`k8s.pod.name` pass through as themselves. Anything exceeding the service's metadata bounds (32
keys, 512 bytes a value, 4 KiB in total) is dropped and logged at debug rather than sent and
refused — a label the store would reject must not fail the record carrying it. A **fixed** label
that could never fit is refused at startup instead, since it is the operator's own value and it is
on every memory.

## Configuration

```yaml
exporters:
  hippocampus:
    endpoint: localhost:50051     # Hippocampus gRPC address
    tls:
      insecure: true              # or configure ca_file / cert_file / key_file / insecure_skip_verify
    token: "<bearer token>"       # when the service runs auth.method hmac/idp
    create_events: true
    group_from: service.name
    default_group: otel-logs
    event_key_from: [service.name]
    event_bucket: day             # none | hour | day
    event_name_template: "{key} — {bucket}"
    event_significance: 12000
    body_from: body               # "body", or an attribute name
    prefix_severity: false        # prepend "[LEVEL] " to the body
    trace_metadata: true          # record trace_id/span_id as metadata labels
    metadata:                     # fixed labels on every memory
      pipeline: logs
    metadata_from: [http.status_code, k8s.pod.name, error.type]
    metadata_prefix: "app.memory."  # copy these attributes, prefix stripped from the key
    significance:
      trace: 1000
      debug: 2000
      info: 6000
      warn: 16000
      error: 28000
      fatal: 32000
      default: 6000
      jitter: 1500
      min: 1
      max: 32767
    # standard exporterhelper blocks also apply:
    timeout: 30s
    retry_on_failure: { enabled: true }
    sending_queue: { enabled: true }
```

The `endpoint`, `tls`, and static `headers` come from the collector's standard gRPC client config
(`configgrpc`); `token` is a convenience that stamps `authorization: Bearer <token>` onto every RPC,
mirroring the service's other bearer-token clients.

## Building it into a collector

The exporter is registered like any collector component, via the
[OpenTelemetry Collector Builder (OCB)](https://opentelemetry.io/docs/collector/custom-collector/).
A ready-to-build manifest and sample config live in [`../collector`](../collector); see its
[README](../collector/README.md) for the end-to-end walkthrough.

## Tests

```sh
go test ./...            # unit tests, ~97% statement coverage
go test -cover ./...
```

`exporter_test.go` drives `pushLogs` against a fake Hippocampus client, covering the
severity→significance mapping, future-timestamp clamping, group extraction, the memories-only and
memories+events paths, configurable composite event keys, and `rejected`/error handling. `batch_test.go` covers the batch write, its split at the service cap, the
`Unimplemented` fallback and the retry policy; `metadata_map_test.go` covers the four metadata
selections, key normalisation and the bounds.
`factory_test.go` and `extras_test.go` cover the factory/lifecycle (`start`/`shutdown`, including the
bearer-token and TLS wiring), `config.go` validation, and the severity/bucket helpers.

## Batching

One collector batch is one `StoreMemories` call (split at the service's 500-memory cap), not one
`StoreMemory` per record — `exporterhelper` has already batched these, and sending them singly spent
a round trip, an interceptor chain, a rate-limit token and a transaction on each. Every memory is
still validated, defaulted and significance-gated individually, so one unusable record does not cost
its neighbours.

A service predating `StoreMemories` answers `Unimplemented`; the exporter notices once, says so, and
falls back to the per-record path for the life of the process.

## Caveats

- Delivery is at-least-once, and the retry unit is a whole batch: a **transport** failure is handed
  back to `exporterhelper`, which re-sends every record in it, and a fresh memory mints a new id per
  call. A **per-memory** failure is therefore deliberately not retried — some of the batch landed,
  and re-sending it would store those twice. The one exception is a call in which nothing landed and
  every failure was transient, which is safe to send again.
- Binary log bodies aren't special-cased; bodies are sent as UTF-8 strings.
