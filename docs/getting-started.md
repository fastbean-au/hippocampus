# Getting started

![Hippocampus](go-hippocampus.png)

This walks through building Hippocampus, running it with a minimal SQLite configuration, and making
your first requests over the HTTP/JSON gateway. For production concerns (driver choice, tuning,
backup, security) see the [Operations guide](operations.md); for the full configuration reference see
[Configurability](configuration.md#configurability).

## Prerequisites

- **Go 1.25+** to build from source, **or** Docker to run the prebuilt image.
- Nothing else for the default SQLite driver — it is embedded and has no external dependencies.

## Build

```sh
go build -o hippocampus ./cmd/hippocampus
```

Or use Docker (statically linked, CGO disabled, runs as non-root):

```sh
docker compose up --build         # SQLite, database persisted in a named volume
```

The compose file exposes `50051` (gRPC) and `8080` (HTTP gateway). If you build from source, use the
configuration below.

## A minimal configuration

Create `config.json`:

```json
{
    "port": 50051,
    "gateway": { "port": 8080 },
    "storage": { "directory": "./data" },
    "sleep": { "periodSeconds": 60 },
    "consolidation": {
        "method": 1,
        "aggressiveness": 1.0,
        "unitsOfAgeInDays": 1.0,
        "deletionThreshold": 5,
        "minimumAgeInDays": 0
    }
}
```

This runs the gRPC service on 50051 and the JSON gateway on 8080, stores the SQLite database under
`./data`, and runs a consolidation ("sleep") cycle every 60 seconds using the power-law decay
algorithm. `unitsOfAgeInDays`, `method` (1–6), and `aggressiveness` must all be set to valid values
or the service refuses to start — a guard against a misconfiguration that would silently forget
everything. See [Memory consolidation](consolidation.md#memory-consolidation) for what these mean and how
to tune them.

## Run

```sh
./hippocampus -c config.json
```

The gRPC port defaults to **50051**. The HTTP gateway is **off by default**; the config above enables
it on **8080** (the conventional port) via `gateway.port`. Both ports can also be set on the command
line, which takes precedence over the config file:

```sh
./hippocampus -c config.json --gateway-port 8080   # enable the gateway on the conventional port
./hippocampus -c config.json --port 40000          # gRPC on a custom port
```

You should see it initialise and log `hippocampus started`. Check liveness (unauthenticated):

```sh
curl -s localhost:8080/healthz            # 200 OK
curl -s localhost:8080/v1/openapi.json    # the OpenAPI description of every endpoint
```

The gateway also serves a self-contained browser console at [`/ui`](http://localhost:8080/ui) for
browsing, searching, and editing memories and events — it drives the same `/v1` endpoints the curl
examples below use, so it is the quickest way to explore a running instance without writing a
client. (When `auth.method` is enabled, paste a bearer token into the field at the top right.)

## First requests (HTTP gateway)

Every RPC is reachable as JSON under `/v1`. Field names are lowerCamelCase.

**Store a memory** — `significance` (> 0) and `body` are required; the response carries the id:

```sh
curl -s -X POST localhost:8080/v1/memories \
  -H 'Content-Type: application/json' \
  -d '{"significance": 50, "body": "the deploy at 14:03 rolled back cleanly"}'
# {"id":"6f1c…","rejected":false}
```

A memory below `memory.minimumSignificance` is *quietly forgotten*: no error, empty id,
`"rejected":true` — a design choice echoing how a brain drops the insignificant.

**List memories** (most significant first):

```sh
curl -s 'localhost:8080/v1/memories?limit=10'
```

**Recall memories** — reinforces them (resets the decay clock, raises effective significance) and
returns them:

```sh
curl -s -X POST localhost:8080/v1/memories/recall \
  -H 'Content-Type: application/json' \
  -d '{"ids": ["6f1c…"]}'
```

**Store an event with memories** — an event groups memories and carries its own significance;
memories attached to a significant event are harder to forget:

```sh
curl -s -X POST localhost:8080/v1/events \
  -H 'Content-Type: application/json' \
  -d '{"name": "release-2.4", "significance": 80,
       "memories": [{"significance": 40, "body": "canary healthy for 30m"}]}'
```

**Trigger a consolidation cycle now** (rather than waiting for the timer):

```sh
curl -s -X POST localhost:8080/v1/sleep
```

Watch the service log: each cycle reports how many memories/events it consolidated, the capacity
pressure, and any evictions.

## Using gRPC directly

The gateway calls straight into the same in-process server, so gRPC and HTTP are equivalent. The
contract is [`contract/hippocampus.proto`](../contract/hippocampus.proto); generated Go client stubs
live in `contract/`. With [`grpcurl`](https://github.com/fullstorydev/grpcurl) and the proto file:

```sh
grpcurl -plaintext -proto contract/hippocampus.proto \
  -d '{"significance": 50, "body": "hello"}' \
  localhost:50051 proto.Hippocampus/StoreMemory
```

## Enabling authentication

Auth and TLS are off by default. To require a bearer token, set `auth.method` to `hmac` and mint a
token from the shared secret:

```sh
go run ./cmd/hippocampus --mint-token --client-id my-client --ttl 24h -c config.json
# prints a token; pass it as: -H 'Authorization: Bearer <token>'
```

See [Authentication](configuration.md#authentication) and [TLS](configuration.md#tls), and the
[Operations guide](operations.md#security) for CLI-only issuance, signing-key
[rotation](configuration.md#key-rotation-hmac) and token/client
[revocation](configuration.md#revocation), and the `idp` (RS256/JWKS) alternative.

## Next steps

- [Operations & deployment guide](operations.md) — driver choice, sizing/tuning, backup, shutdown,
  observability, security.
- [Use cases & deployment modes](use-cases.md) — embedded vs. centralised topologies.
- [Configurability](configuration.md#configurability) — the exhaustive configuration reference.
- [Memory consolidation](consolidation.md#memory-consolidation) — the decay algorithms, capacity target,
  and summarization.
