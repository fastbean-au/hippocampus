# Event-sourcing bridges

Bridge services that consume from a message broker and store each message as a Hippocampus
**memory**, so a stream of events decays and consolidates under the same significance/recall dynamics
as everything else in the store. One bridge exists for each of five sources:

| Source   | Command            | Client library                       | Delivery semantics                          |
| -------- | ------------------ | ------------------------------------ | ------------------------------------------- |
| NATS     | `cmd/nats`         | `github.com/nats-io/nats.go`         | at-most-once (core NATS has no ack)         |
| MQTT     | `cmd/mqtt`         | `github.com/eclipse/paho.mqtt.golang`| at-least-once (QoS ≥ 1, manual ack)         |
| RabbitMQ | `cmd/rabbitmq`     | `github.com/rabbitmq/amqp091-go`     | at-least-once (manual ack, nack-with-requeue)|
| Kafka    | `cmd/kafka`        | `github.com/segmentio/kafka-go`      | at-least-once (offset committed after store)|
| Bluesky  | `cmd/bluesky`      | `github.com/gorilla/websocket`       | at-least-once, cursor-gated                 |

This is its own Go module (`github.com/fastbean-au/hippocampus/integrations/eventsource`), separate
from the root so its broker-client dependencies never reach the main service build — the same
arrangement the OTEL exporter uses.

> The full guide — how it works, message shaping, delivery semantics, custom transformers, and a
> worked end-to-end walkthrough — lives in **[docs/eventsource.md](../../docs/eventsource.md)**.

**Authenticating.** `--token` for a hand run; `--oidc-issuer` + `--oidc-client-id`/`--oidc-client-secret`
for anything long-running against an IdP-backed service, since a static token expires and the bridge
then fails every write silently for as long as it runs. See
[Authenticating to the service](../../docs/eventsource.md#authenticating-to-the-service).

**Bluesky is the odd one out.** It is the only bridge that *reinforces* as well as writes: a post
becomes a memory, and the likes, reposts and replies that follow it become `RecallMemories` calls
against that post. Because a memory's id **is** the post's `at://` URI and a like names its target by
that same URI, reinforcing needs no state and no lookup — a like for a post the store never held, or
has already forgotten, costs one `UPDATE` that matches no rows. See
[Bluesky](../../docs/eventsource.md#bluesky-the-firehose-bridge).

## Install

Each command is a standalone binary; pick whichever broker you need.

**Prebuilt binary (no Go toolchain).** Every tagged release attaches
`hippocampus-<broker>-bridge` archives (`nats`/`mqtt`/`rabbitmq`/`kafka`/`bluesky`) for Linux, macOS, and
Windows on amd64/arm64, with a `checksums.txt` to verify them — grab one from the
[releases page](https://github.com/fastbean-au/hippocampus/releases).

**Container image.** Each release publishes one image per broker to GHCR
(`ghcr.io/fastbean-au/hippocampus-<broker>-bridge`, multi-arch), so you can run a bridge without a Go
toolchain — flags go after the image name:

```sh
docker run --rm ghcr.io/fastbean-au/hippocampus-nats-bridge:latest \
  --nats-url nats://nats:4222 --subject 'events.>' --address hippocampus:50051
```

All four come from the one parameterised [`Dockerfile`](Dockerfile) (the `BROKER` build-arg), built
with the repo root as context. A bridge listens on no port; see
[docs/eventsource.md](../../docs/eventsource.md#container-image) for the networking notes.

**Build from source** (the bridges are a separate module, so build from this directory):

```sh
cd integrations/eventsource
go build -o hippocampus-nats-bridge ./cmd/nats
# ...and ./cmd/mqtt, ./cmd/rabbitmq, ./cmd/kafka
```

## How it fits together

Every bridge is the same two pieces: a broker **adapter** (the consume loop) on top of a shared
**`bridge` core** (the transform-and-store logic). The core is broker-agnostic — each adapter
normalises its native delivery onto a `bridge.Message`, and the core turns that into one or more
`contract.Memory` values via a `Transformer` and writes them over gRPC.

```text
broker ── adapter (nats/mqtt/rabbitmq/kafka/bluesky) ── bridge.Store ── Transformer ── StoreMemory RPC ─▶ Hippocampus
```

The `Transformer` is the extension point:

```go
type Transformer interface {
    Transform(msg bridge.Message) ([]*contract.Memory, error)
}
```

- The **default transformer** (`bridge.NewDefaultTransformer`) maps one message to one memory: the
  payload becomes the body, the subject/topic becomes the group, and the significance is a fixed
  value (optionally overridden per message by a header). Every `cmd/*` ships this, driven by flags.
- A **custom transformer** lets a program embed an adapter and shape memories however it likes —
  parse a JSON envelope, derive significance from a field, split a batch message into several
  memories, or drop messages that don't match a filter (return an empty slice). Pass a
  `bridge.TransformerFunc` to `bridge.NewStore` and hand that store to the adapter's `New`.

Returning an error from `Transform` (or a transport failure storing a memory) makes the adapter
treat the delivery as failed: NATS drops it (no ack exists), MQTT leaves it unacked, RabbitMQ nacks
with requeue, and Kafka leaves the offset uncommitted so it is re-read.

## Running a bridge

Each command shares a common set of flags (service address, auth token, TLS, and the default
transformer's significance/group/binary options — see `bridge.RegisterCommonFlags`) plus its own
broker flags. Secrets can be supplied via environment variables (`HIPPOCAMPUS_<BROKER>_*`, e.g.
`HIPPOCAMPUS_NATS_TOKEN`) instead of argv.

```sh
# NATS: store everything published on "events.>" as memories grouped by subject
go run ./cmd/nats --nats-url nats://localhost:4222 --subject 'events.>' \
  --address localhost:50051

# MQTT: sensor readings, QoS 1, significance 3
go run ./cmd/mqtt --broker tcp://localhost:1883 --topic 'sensors/#' --significance 3

# RabbitMQ: consume a queue with manual ack
go run ./cmd/rabbitmq --amqp-url amqp://guest:guest@localhost:5672/ --queue events

# Kafka: consume a topic as a group member
go run ./cmd/kafka --brokers localhost:9092 --topic events --consumer-group hippocampus
```

Run `--help` on any command for the full flag list, or `--version` to print the build version.

## Delivery semantics and scaling

- **NATS** core delivery is at-most-once; a failed store is logged and dropped. Run several
  bridges sharing a `--queue` group to load-balance a subject. Use JetStream in front of the bridge
  if you need durable replay.
- **MQTT** uses manual acknowledgement with `AutoAckDisabled`: the PUBACK is sent only after the
  store succeeds, so with `--qos 1` (or 2) and a persistent session (`--clean-session=false`, the
  default, plus a stable `--client-id`) an unstored message is redelivered on reconnect.
- **RabbitMQ** acks on success and nacks (with requeue by default; `--requeue-on-error=false` to
  dead-letter instead) on failure. `--prefetch` bounds in-flight deliveries; keep it at 1 for
  strict ordering. Scale by running multiple bridges on the same queue.
- **Kafka** commits the offset only after a successful store, giving at-least-once. Run multiple
  bridges sharing `--consumer-group` to split partitions between them; a store failure backs off
  (`--error-backoff-seconds`) and re-reads rather than skipping.

## Observability

Every bridge serves `/healthz` and `/readyz` on `--health-port` (8090 by default, 0 disables) and
exports OTEL metrics with `--metrics`. Readiness reports whether the Hippocampus instance the bridge
writes to can serve — not the broker, whose failures are already visible as a restart or handled by
the adapter's own reconnect. See
[docs/eventsource.md](../../docs/eventsource.md#observability).

## Development

```sh
go build ./...
go test ./...          # unit tests (transform, store, and each adapter's message/ack routing)
go test -race ./...
```

The unit tests cover the pure logic — message normalisation, the default transformer, the store
loop, and each adapter's ack/commit routing (driven by fakes) — without needing a live broker, and
the NATS adapter additionally runs against an **embedded in-process NATS server**. The MQTT and
RabbitMQ adapters' real-connect paths are covered by integration tests that skip unless the matching
environment variable points at a broker:

```sh
HIPPOCAMPUS_TEST_MQTT_BROKER=tcp://localhost:1883 \
HIPPOCAMPUS_TEST_RABBITMQ_URL=amqp://guest:guest@localhost:5672/ \
go test ./...
```

CI (`.github/workflows/ci.yaml`, the `eventsource-bridges` job) starts mosquitto and RabbitMQ and
runs the whole suite with those variables set, so every package stays ≥95% covered on every push.
The release workflow cross-compiles all four `cmd` binaries onto the GitHub release.
