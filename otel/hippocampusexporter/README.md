# Hippocampus OpenTelemetry logs exporter

An [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) **logs exporter** that stores
log records as Hippocampus memories over the service's gRPC contract. Each log record becomes a
memory whose significance is derived from the record's severity, so the Hippocampus consolidation
(decay) cycle forgets routine `DEBUG`/`INFO` noise first and keeps `ERROR`/`FATAL` — significance-by-
severity survival becomes your retention policy. See the repository
[demonstrations](../../docs/demonstrations.md) for the shape of that result.

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
memories+events paths, configurable composite event keys, and `rejected`/error handling.
`factory_test.go` and `extras_test.go` cover the factory/lifecycle (`start`/`shutdown`, including the
bearer-token and TLS wiring), `config.go` validation, and the severity/bucket helpers.

## Caveats

- Delivery is at-least-once: a retried batch re-issues `StoreMemory`, which mints a fresh id each
  call, so a transient failure can duplicate the records that already succeeded in that batch.
- Binary log bodies aren't special-cased; bodies are sent as UTF-8 strings.
