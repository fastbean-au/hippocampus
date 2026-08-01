# A collector with the Hippocampus logs exporter

This directory builds a small OpenTelemetry Collector that ships the
[`hippocampusexporter`](../hippocampusexporter), so logs collected by the standard collector
pipeline (a `filelog` receiver tailing files, an `otlp` receiver for instrumented apps) land in
Hippocampus as memories. Severity drives significance, so the decay cycle forgets routine noise
first and keeps errors.

## Prebuilt image

Each tagged release publishes this collector to GHCR, so you can run it without building anything:

```sh
docker run --rm --network host \
  -v "$(pwd)/config.yaml:/etc/otelcol/config.yaml" \
  ghcr.io/fastbean-au/hippocampus-otel-collector:latest
```

The bundled config's `endpoint` is `localhost:50051`, so the collector must be able to reach the
Hippocampus instance on the host. `--network host` (Linux) is the simplest way; on Docker Desktop
(macOS/Windows) drop `--network host` and instead point `exporters.hippocampus.endpoint` at
`host.docker.internal:50051` in your mounted `config.yaml`. Against a containerised Hippocampus on a
shared compose/user-defined network, use its service name (and drop the mount to keep the demo
config, or set the endpoint accordingly).

The image bundles a runnable demo `config.yaml`/`sample.log` at `/etc/otelcol/`; mount your own
`config.yaml` over it for a real deployment. It is tagged with the release version, the rolling
`major.minor`, and `latest` (see [`.github/workflows/release.yaml`](../../../.github/workflows/release.yaml)).
Build it yourself from source instead with the steps below.

## Build

The collector is assembled with the
[OpenTelemetry Collector Builder (OCB)](https://opentelemetry.io/docs/collector/custom-collector/)
from [`builder-config.yaml`](builder-config.yaml):

```sh
go install go.opentelemetry.io/collector/cmd/builder@v0.157.0
cd integrations/otel/collector
builder --config builder-config.yaml
```

This generates the collector sources and binary under `./_build/` (git-ignored). The manifest
points OCB at the local exporter (and the repo-root contract) via `replaces:`, so it builds against
your working tree.

## Run

Point [`config.yaml`](config.yaml) at your logs (`receivers.filelog.include`) and at a running
Hippocampus instance (`exporters.hippocampus.endpoint`), then:

```sh
./_build/hippocampus-otelcol --config config.yaml
```

A sample multi-severity log (`sample.log`) and a `filelog` parser that reads `[LEVEL] message`
lines are included, alongside a `debug` exporter so you can watch records flow.

## End-to-end demonstration

With a local SQLite Hippocampus running (gRPC `:50051`, gateway `:8080` — see
[getting started](../../../docs/getting-started.md)) and decay tuned to bite:

1. Run the collector over `sample.log`; the 12 lines become 12 memories, one event for the day.
   Confirm via the gateway:

   ```sh
   curl -s 'http://localhost:8080/v1/memories?page_size=100'
   ```

   Significance rises monotonically with severity (`DEBUG` lowest … `FATAL` highest), the `group`
   is taken from `service.name` (or `default_group`), and each memory carries the day's `eventId`.

2. Trigger a consolidation cycle (`curl -s -X POST http://localhost:8080/v1/sleep -d '{}'`) and
   list again: the lowest-severity memories are forgotten first while the errors and fatals
   survive — significance-by-severity survival, driven by a real OTel pipeline. (In a live
   deployment this plays out over days as logs age; the demonstration compresses the decay clock —
   see [demonstrations](../../../docs/demonstrations.md).)

## Notes

- The bundled `config.yaml` also opens an `otlp` receiver on `:4317` for instrumented apps; drop it
  from the `logs` pipeline if you only want file tailing (or if that port is taken locally).
- For a secured Hippocampus, set `exporters.hippocampus.token` and the `tls` block (see the
  [exporter README](../hippocampusexporter/README.md#configuration)).
