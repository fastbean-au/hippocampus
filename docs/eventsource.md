# Event sourcing — broker bridges

`integrations/eventsource` bridges a message broker into Hippocampus: it consumes from a broker and
stores every message as a **memory**, so a stream of events decays and consolidates under the same
significance/recall dynamics as everything else in the store. Routine, low-significance events fade;
the ones that matter (or that you keep recalling) survive.

There is one bridge for each of four brokers:

| Broker   | Command        | Delivery semantics                              |
| -------- | -------------- | ----------------------------------------------- |
| NATS     | `cmd/nats`     | at-most-once (core NATS has no per-message ack) |
| MQTT     | `cmd/mqtt`     | at-least-once (QoS ≥ 1, manual ack)             |
| RabbitMQ | `cmd/rabbitmq` | at-least-once (manual ack, nack-with-requeue)   |
| Kafka    | `cmd/kafka`    | at-least-once (offset committed after store)    |

Like the [OpenTelemetry exporter](../integrations/otel/hippocampusexporter/README.md), this is its
own Go module (`github.com/fastbean-au/hippocampus/integrations/eventsource`), separate from the root
so its broker-client dependencies never reach the main service build. Each bridge is a normal gRPC
client, so it works against any deployment topology — an embedded per-tenant SQLite instance, a
centralised Postgres/MySQL store, or a read/write replica behind a load balancer.

> For turning application **logs** into memories instead of broker messages, see the
> [OpenTelemetry log ingestion](../integrations/otel/collector/README.md) integration; for an LLM
> host, see the [MCP server](mcp.md).

## How it works

Every bridge is the same two pieces: a broker **adapter** (the consume loop) on top of a shared
**`bridge` core** (the transform-and-store logic). The adapter normalises its native delivery onto a
broker-agnostic `bridge.Message`, and the core turns that into one or more memories via a
`Transformer` and writes them over gRPC.

```text
broker ─▶ adapter (nats/mqtt/rabbitmq/kafka) ─▶ bridge.Store ─▶ Transformer ─▶ StoreMemory RPC ─▶ Hippocampus
```

A delivery that fails to store (a transform error or a gRPC transport failure) is treated as failed
so the adapter can redeliver it — NATS drops it (no ack exists), MQTT leaves it unacked, RabbitMQ
nacks with requeue, and Kafka leaves the offset uncommitted. A memory dropped for significance below
the service's threshold is a _success_, not a failure.

## Install

Grab a pre-built binary for your platform from the
[releases page](https://github.com/fastbean-au/hippocampus/releases) — each release attaches
`hippocampus-<broker>-bridge` archives for Linux, macOS, and Windows on amd64/arm64, with a
`checksums.txt` to verify them.

Or build from source (the bridges are a separate module, so build from its directory):

```sh
cd integrations/eventsource
go build -o hippocampus-nats-bridge ./cmd/nats
# ...and ./cmd/mqtt, ./cmd/rabbitmq, ./cmd/kafka
```

### Container image

Each tagged release publishes one image per broker to GHCR, so you can run a bridge without a Go
toolchain:

```sh
docker run --rm ghcr.io/fastbean-au/hippocampus-nats-bridge:latest \
  --nats-url nats://nats:4222 --subject 'events.>' --address hippocampus:50051
```

Images: `hippocampus-nats-bridge`, `hippocampus-mqtt-bridge`, `hippocampus-rabbitmq-bridge`,
`hippocampus-kafka-bridge` (tagged with the release version, the rolling `major.minor`, and
`latest`; `linux/amd64` and `linux/arm64`). A bridge is an outbound client — it dials the broker and
the Hippocampus service and listens on no port — so the image exposes nothing and takes the bridge's
flags after the image name. It must be able to reach both endpoints: on a shared compose/Kubernetes
network use the service names (as above); with `docker run` on the host, `--network host` (Linux) or
`host.docker.internal` for the broker/service addresses (Docker Desktop). All four are built from the
one parameterised `integrations/eventsource/Dockerfile` (the `BROKER` build-arg):

```sh
docker build -f integrations/eventsource/Dockerfile --build-arg BROKER=kafka \
  -t hippocampus-kafka-bridge .    # built from the repo root (the module's replace reaches it)
```

## Running

Each command shares a common set of flags — how to reach the Hippocampus service (`--address`,
`--token`, and the `--tls*` trust options) and how the default transformer shapes each message — plus
its own broker flags. Secrets can be injected via `HIPPOCAMPUS_<BROKER>_*` environment variables
(e.g. `HIPPOCAMPUS_NATS_TOKEN`, `HIPPOCAMPUS_MQTT_PASSWORD`) instead of argv. Run `--help` on any
command for the full list, or `--version` to print the build version.

```sh
# NATS: store everything published on "events.>" as memories grouped by subject
go run ./cmd/nats --nats-url nats://localhost:4222 --subject 'events.>' --address localhost:50051

# MQTT: sensor readings, QoS 1, significance 3
go run ./cmd/mqtt --broker tcp://localhost:1883 --topic 'sensors/#' --significance 3

# RabbitMQ: consume a queue with manual ack
go run ./cmd/rabbitmq --amqp-url amqp://guest:guest@localhost:5672/ --queue events

# Kafka: consume a topic as a group member
go run ./cmd/kafka --brokers localhost:9092 --topic events --consumer-group hippocampus
```

### Shaping messages into memories

The default transformer maps one message to one memory. Its behaviour is controlled by the shared
flags:

| Flag                       | Effect                                                                                           |
| -------------------------- | ------------------------------------------------------------------------------------------------ |
| `--significance`           | Significance stamped on each memory (default 1).                                                 |
| `--significance-header`    | Message header whose integer value overrides `--significance` per message.                       |
| `--group`                  | Group label for every memory.                                                                    |
| `--group-from-subject`     | When `--group` is empty, use the subject/topic as the group (default on).                        |
| `--group-header`           | Message header whose value overrides the group per message.                                      |
| `--binary`                 | Base64-encode the payload and mark the memory `is_binary` (never content-indexed).               |
| `--max-body-bytes`         | Truncate an over-long payload before it becomes a body (0 = unlimited).                          |
| `--metadata`               | Metadata label as `key=value`, stamped on every memory (repeatable).                             |
| `--metadata-header`        | Message header to copy onto each memory's metadata (repeatable).                                 |
| `--metadata-header-prefix` | Copy every header carrying this prefix onto the metadata, with the prefix stripped from the key. |
| `--subject-metadata-key`   | Record the subject/topic as a metadata label under this key, as well as (or instead of) as the group. |

Header selection is an **allowlist or a prefix, never "copy every header"**. Broker headers are
unbounded and mostly machinery — trace context, delivery counts, redelivery flags — so copying them
all would fill each memory's metadata budget with noise, and the keys would be infrastructure's
rather than the operator's choice.

> **If the bridge's token is [group-scoped](configuration.md#group-scoping), turn
> `--group-from-subject` off.** It is on by default, so each memory would be stamped with its
> subject as the group — and a scoped token may only write the groups it holds, so **every message
> would be refused** with `PermissionDenied`.
>
> ```bash
> go run ./cmd/nats --subject 'events.>' --token "$SCOPED_TOKEN" \
>   --group team-alpha --group-from-subject=false --subject-metadata-key subject
> ```
>
> `--group` names the label the token carries (or leave it unset and let the server stamp the
> token's sole group), and `--subject-metadata-key` keeps the subject as **metadata** — which is
> where multi-dimensional classification belongs now that `group` can also be an access boundary.
> Nothing is lost: filter on it with `?metadata=subject%3Devents.orders.created` exactly as you
> would have filtered on the group. An unscoped token is unaffected and the default stands.

Selected header names are normalised to the service's metadata key charset (lowercased, anything
outside `[A-Za-z0-9._:/-]` replaced with `_`), since header names routinely contain spaces and
capitals the service would reject. Anything that still will not fit — an over-long value, or a
selection past the 32-key or 4 KiB caps — is **dropped with a warning rather than failing the
delivery**: the message is not at fault, and on an at-least-once broker a nack would redeliver it
forever.

```sh
hippocampus-nats-bridge --subject 'events.>' \
  --metadata source=nats --metadata env=prod \
  --metadata-header-prefix 'hippo-'
```

The broker-provided message timestamp is used when available (a future timestamp is clamped to now so
the service's clock-skew guard never rejects the write), otherwise the current time.

## End-to-end walkthrough (NATS)

With a local SQLite Hippocampus running (gRPC `:50051`, gateway `:8080` — see
[getting started](getting-started.md)):

1. Start a broker — for NATS:

   ```sh
   docker run --rm -p 4222:4222 nats:latest
   ```

2. Run the bridge, storing everything on `events.>` as memories grouped by subject, at
   significance 5:

   ```sh
   cd integrations/eventsource
   go run ./cmd/nats --nats-url nats://localhost:4222 --subject 'events.>' \
     --address localhost:50051 --significance 5
   ```

   It logs `NATS bridge subscribed`.

3. Publish a message (any NATS client; here the `nats` CLI):

   ```sh
   nats pub events.orders.created 'order 42 created for acme'
   ```

4. Confirm it landed, via the gateway:

   ```sh
   curl -s http://localhost:8080/v1/memories | jq '.memories[] | {body, significance, group}'
   # { "body": "order 42 created for acme", "significance": 5, "group": "events.orders.created" }
   ```

The memory now decays and consolidates like any other: publish a stream of events, trigger a
consolidation cycle (`curl -s -X POST http://localhost:8080/v1/sleep -d '{}'`), and the
lower-significance ones are forgotten first while the ones you keep recalling survive.

## Delivery semantics and scaling

- **NATS** core delivery is at-most-once; a failed store is logged and dropped. Run several bridges
  sharing a `--queue` group to load-balance a subject. Front the bridge with JetStream for durable
  replay.
- **MQTT** uses manual acknowledgement (`AutoAckDisabled`): the PUBACK is sent only after the store
  succeeds, so with `--qos 1` (or `2`) and a persistent session (`--clean-session=false`, the
  default, plus a stable `--client-id`) an unstored message is redelivered on reconnect.
- **RabbitMQ** acks on success and nacks on failure — with requeue by default, or
  `--requeue-on-error=false` to dead-letter instead of risking a hot redelivery loop on a poison
  message. `--prefetch` bounds in-flight deliveries; keep it at 1 for strict ordering. Scale by
  running multiple bridges on the same queue.
- **Kafka** commits the offset only after a successful store. Run multiple bridges sharing
  `--consumer-group` to split a topic's partitions between them; a store failure backs off
  (`--error-backoff-seconds`) and re-reads rather than skipping.

## Observability

Every bridge serves `/healthz` and `/readyz` on `--health-port` (**8090 by default**; 0 disables) and
exports OTEL metrics over OTLP/gRPC with `--metrics`.

`/healthz` is process liveness and never fails while the process runs; `/readyz` reports whether the
Hippocampus instance the bridge writes to can actually serve:

```json
{"component":"hippocampus-nats-bridge","dependencies":{"hippocampus":"ok"},"status":"ready"}
```

**The broker is deliberately not part of readiness.** Both of its failure modes are already handled:
a broker unreachable at startup exits the process before the consume loop begins — the supervisor's
problem, and visible as a restart — while a mid-run disconnect is the adapter's own to retry. What
nothing else would notice is the Hippocampus end going away, because a bridge with no traffic and a
bridge that cannot write look identical from outside. That is the gap the probe closes.

| Metric | Type | Attributes |
| --- | --- | --- |
| `hippocampus.bridge.messages` | counter | `broker`, `outcome` (stored/rejected/filtered/failed) |
| `hippocampus.bridge.memories` | counter | `broker`, `outcome` |
| `hippocampus.bridge.message.duration` | histogram (s) | `broker`, `outcome` |
| `hippocampus.bridge.body_bytes` | histogram | `broker` |
| `hippocampus.client.rpc.requests` | counter | `endpoint`, `rpc`, `code`, `outcome` |
| `hippocampus.client.rpc.duration` | histogram (s) | as above |

`outcome` is four-valued rather than a success bool, because the three non-failures are
operationally different and an SLO has to separate them: a memory the **service** declined for
insignificance (`rejected`) is the decay model working, a message a Transformer chose to yield
nothing for (`filtered`) was dropped on purpose, and only `failed` is the bridge not doing its job. A
message yielding several memories reports the **worst** of their outcomes, since the adapter is about
to redeliver the whole message if any of them failed.

**Tenancy** is `--metrics-group`, defaulting to `--group` when that is set. It is a **per-process**
label, never the per-message group — with `--group-from-subject` (the default) that value is the
message subject, so on a wildcard subscription it would be one metric stream per subject. Set once
per process it costs no extra cardinality at all.

```promql
sum by (broker, outcome) (rate(hippocampus_bridge_messages_total[5m]))
```

Running several bridges on one host means giving each its own `--health-port`.

## Custom transformations

The `Transformer` is the extension point for anything beyond one-message-one-memory:

```go
type Transformer interface {
    Transform(msg bridge.Message) ([]*contract.Memory, error)
}
```

A program can embed an adapter and supply its own transform — parse a JSON envelope, derive
significance from a field, split a batch message into several memories, or drop messages that don't
match a filter (return an empty slice). Wire a `bridge.TransformerFunc` into `bridge.NewStore` and
hand that store to the adapter's `New`:

```go
store := bridge.NewStore(client, bridge.TransformerFunc(func(msg bridge.Message) ([]*contract.Memory, error) {
    // ...shape msg.Data into one or more *contract.Memory...
}), callTimeout)

b := nats.New(nats.Config{URL: url, Subject: "events.>"}, store)
_ = b.Run(ctx)
```

## Testing

```sh
cd integrations/eventsource
go test ./...        # unit tests (transform, store, and each adapter's message/ack routing)
go test -race ./...
```

The unit tests cover the pure logic without a live broker. Two adapters additionally have
integration tests that exercise the real connect path; they skip unless the matching environment
variable points at a broker:

- NATS runs an embedded in-process server, so it always runs — no external broker needed.
- MQTT: set `HIPPOCAMPUS_TEST_MQTT_BROKER` (e.g. `tcp://localhost:1883`).
- RabbitMQ: set `HIPPOCAMPUS_TEST_RABBITMQ_URL` (e.g. `amqp://guest:guest@localhost:5672/`).

CI starts mosquitto and RabbitMQ and runs the full suite with those variables set, so the adapters'
real-connect paths are exercised on every push.
