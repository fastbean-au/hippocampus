# Getting started

![Hippocampus](go-hippocampus.png)

This walks through building Hippocampus, running it with a minimal SQLite configuration, and making
your first requests over the HTTP/JSON gateway. For production concerns (driver choice, tuning,
backup, security) see the [Operations guide](operations.md); for the full configuration reference see
[Configurability](configuration.md#configurability).

## Prerequisites

- **Go 1.25+** to build from source, **or** Docker to run the prebuilt image.
- Nothing else for the default SQLite driver — it is embedded and has no external dependencies.

## The fastest look: the demo stack

Decay, recall reinforcement and consolidation play out over days, which makes a fresh install a poor
place to watch them. The demo stack builds the service and a load generator and runs them against
the embedded SQLite driver **with the decay clock compressed**, so forgetting is visible in minutes.
If a container runtime is present it also starts OpenSearch and a Grafana/OTEL collector; without
one it simply runs without them.

```sh
git clone https://github.com/fastbean-au/hippocampus.git
cd hippocampus
./demo/run.sh
```

Then open the embedded web console at [`localhost:8080/ui`](http://localhost:8080/ui) — its **Now**
and **Decay** tabs show what the last cycle forgot and where each memory stands — and Grafana at
[`localhost:3000`](http://localhost:3000). The demo serves its HTTP/JSON gateway on `8080` and gRPC
on `8300`. `SEARCH=0` and `OBSERVABILITY=0` skip the OpenSearch and collector containers; see
[`demo/README.md`](../demo/README.md) and [Demonstrations](demonstrations.md).

It is a demonstration, not a template: the compressed clock, the generator, and the wide-open
listeners all belong to it. The rest of this page is the instance you keep.

## Install or build

Homebrew is the quickest supervised install on macOS and Linux — it manages the launchd/systemd
definition and installs a default embedded-SQLite config that survives upgrades:

```sh
brew install fastbean-au/tap/hippocampus
brew services start hippocampus
```

From a clone, build it:

```sh
go build -o hippocampus ./cmd/hippocampus
```

Or use Docker (statically linked, CGO disabled, runs as non-root):

```sh
docker compose up --build         # SQLite, database persisted in a named volume
```

The compose file exposes `50051` (gRPC) and `8080` (HTTP gateway). If you build from source, use the
configuration below. Packages (`.deb`/`.rpm`), the Kubernetes overlays, and the service-supervision
options are in the [Operations guide](operations.md#running-as-a-service).

## Run it with no configuration at all

```sh
./hippocampus
```

With no `config.json` on the default path, the service starts on its built-in defaults — SQLite in
`./data`, gRPC on `50051`, the power-law decay algorithm, no authentication — and logs a warning
saying so. That is enough to make requests against, and nothing to write first. The HTTP gateway is
off unless a port is given, so add `--gateway-port 8080` to get the JSON API and the browser console
as well.

It is a starting point, not a deployment: there is no authentication, no TLS, no capacity target,
and — because `sleep.periodSeconds` has no default, deliberately — no automatic consolidation cycle,
so nothing is forgotten until you ask for it with the `Sleep` RPC. The configuration below is the
one to grow from.

## A minimal configuration

The repository ships a `config.json` that already runs both listeners, so `./hippocampus -c
config.json` from a clone works as-is. To write your own:

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
algorithm. `unitsOfAgeInDays`, `method` (1–6), and `aggressiveness` each have a default, but a value
that is present and invalid — a zero, a negative, a `method` outside 1–6 — makes the service refuse
to start rather than run: at zero they would silently forget everything. See [Memory
consolidation](consolidation.md#memory-consolidation) for what these mean and how to tune them.

## Run

```sh
./hippocampus -c config.json
```

The gRPC port defaults to **50051**. The HTTP gateway is **off unless a port is configured**; the
config above (and the repository's own `config.json`) enables it on **8080**, the conventional port.
When it is off the service says so at startup, since the console and the HTTP probes go with it.
Both ports can also be set on the command line, which takes precedence over the config file:

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
client. (When authentication is enabled, a sign-in card stands in place of the console until a
session resolves: a bearer-token box under `auth.method: hmac`, a provider button under `idp`. The
console then adapts to the token's [role](configuration.md#authorisation), hiding write controls for
a `reader`.) The [console guide](console.md) covers each tab.

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
  localhost:50051 hippocampus.v1.Hippocampus/StoreMemory
```

The `-proto` flag is not optional: the service registers no gRPC reflection service, so a tool
cannot discover the schema from a running instance. To generate stubs for a language other than Go —
from either the proto or the OpenAPI document — see [Clients in other languages](clients.md).

## Enabling authentication

Auth and TLS are off by default. To require a bearer token, set `auth.method` to `hmac` and mint a
token from the shared secret:

```sh
go run ./cmd/hippocampus --mint-token --client-id my-client --ttl 24h -c config.json
# prints a token; pass it as: -H 'Authorization: Bearer <token>'
```

See [Authentication](configuration.md#authentication) and [TLS](configuration.md#tls), and the
[security guide](security.md) for CLI-only issuance, signing-key
[rotation](configuration.md#key-rotation-hmac) and token/client
[revocation](configuration.md#revocation), and the `idp` (RS256/JWKS) alternative.

## Next steps

- [Clients in other languages](clients.md) — generate a Python, TypeScript, or any-language client
  from the proto or the OpenAPI document.
- [Operations & deployment guide](operations.md) — driver choice, sizing/tuning, backup, shutdown,
  observability, security.
- [Use cases & deployment modes](use-cases.md) — embedded vs. centralised topologies.
- [Configurability](configuration.md#configurability) — the exhaustive configuration reference.
- [Memory consolidation](consolidation.md#memory-consolidation) — the decay algorithms, capacity target,
  and summarisation.
- [MCP server](mcp.md) — expose the store to an LLM host (Claude Desktop/Code) as memory tools.
