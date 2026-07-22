# MCP server

`cmd/hippocampus-mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server that
gives an LLM host — Claude Desktop, Claude Code, or any other MCP client — tools for storing and
recalling memories in a running Hippocampus instance.

It is a thin **bridge**: every tool call is turned into a gRPC request against the Hippocampus
service named by `--address`. The bridge holds no state of its own, so the host can spawn, kill,
and restart it freely. Because it is a normal gRPC client, it works against any deployment topology
— an embedded per-tenant SQLite instance, a centralised Postgres/MySQL store, or a read/write
replica behind a load balancer.

## Install

Grab a pre-built binary for your platform from the
[releases page](https://github.com/fastbean-au/hippocampus/releases) — each release attaches
`hippocampus-mcp` archives for Linux, macOS, and Windows on amd64/arm64, with a `checksums.txt` to
verify them. This is the easiest path for the local stdio use case: no Go toolchain required.

Or build from source:

```sh
go build -o hippocampus-mcp ./cmd/hippocampus-mcp
```

Or, for the HTTP transport, pull the image (see [Running with Docker](#running-with-docker)):

```sh
docker pull ghcr.io/fastbean-au/hippocampus-mcp:latest
```

## Tools

The surface is deliberately the memory-and-event operations a model needs to give and retrieve
memories. Destructive and administrative RPCs (`Purge`, `Export`/`Import`/`Transfer`/`Clear`, event
deletion/merge) are **not** exposed, so a model cannot wipe or exfiltrate a store through this
bridge.

| Tool | Maps to | Notes |
| :--- | :--- | :--- |
| `store_memory` | `StoreMemory` | Store text with a significance; low-significance memories are forgotten over time. |
| `recall_memories` | `RecallMemories` | Fetch by id **and reinforce** — resets the decay clock, raises effective significance. |
| `search_memories` | `SearchMemories` | Content search (needs the service's OpenSearch index); `reinforce` off by default. |
| `list_memories` | `GetMemories` | Read-only browse by group/significance; does **not** reinforce. |
| `create_event` | `StoreEvent` | A named time span memories can be grouped under. |
| `list_events` | `GetEvents` | Read-only browse of events. |
| `get_summarization_candidates` | `GetSummarizationCandidates` | Events the last consolidation cycle flagged as worth condensing. |

Memories and events are returned as plain JSON objects (the read-only fields — `id`, `time_stamp`,
`recall_count`, `is_summary`, and so on — included) so the model can reason about them and feed ids
back into `recall_memories`.

## Transports

- **stdio** (default) — the host launches this binary as a subprocess and speaks MCP over its
  stdin/stdout. All logging goes to **stderr**; stdout carries only the MCP protocol stream.
- **streamable HTTP** (`--transport http --http-address :8090`) — serves the same tools over HTTP
  for a remote/hosted host instead.

## Connecting to the service

```sh
# plaintext, no auth (a locally-running service)
hippocampus-mcp --address localhost:50051

# with a bearer token (the service has auth.method hmac/idp)
hippocampus-mcp --address memory.internal:50051 --token "$HIPPO_TOKEN"

# over TLS, verifying against a private CA
hippocampus-mcp --address memory.internal:50051 --tls --tls-ca-cert /etc/ssl/hippo-ca.pem
```

The auth and TLS options mirror the service's own [authentication](configuration.md#authentication)
and [TLS](configuration.md#tls) surface. Mint a token with
`hippocampus --mint-token --client-id <id> --ttl 24h -c config.json`; the same token authenticates
the bridge. The token may also be supplied as `HIPPOCAMPUS_MCP_TOKEN` (any flag maps to
`HIPPOCAMPUS_MCP_<NAME>` with dashes as underscores), keeping the secret out of the argv the host
stores.

### Flags

| Flag | Default | Purpose |
| :--- | :--- | :--- |
| `-a`, `--address` | `localhost:50051` | Hippocampus gRPC address. |
| `--transport` | `stdio` | `stdio` or `http`. |
| `--http-address` | `:8090` | Listen address for `--transport http`. |
| `--token` | — | Bearer token (or `HIPPOCAMPUS_MCP_TOKEN`). |
| `--tls` | `false` | Dial over TLS. |
| `--tls-ca-cert` | — | PEM CA bundle in place of the system pool. |
| `--tls-cert` / `--tls-key` | — | Client certificate/key for mutual TLS. |
| `--tls-insecure-skip-verify` | `false` | Skip certificate verification (dev only). |
| `--call-timeout-seconds` | `30` | Per-tool-call timeout bounding each gRPC request. |
| `--log-level` | `info` | Logging level (written to stderr). |

## Host configuration

### Claude Desktop

Add an entry to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "hippocampus": {
      "command": "/path/to/hippocampus-mcp",
      "args": ["--address", "localhost:50051"],
      "env": {
        "HIPPOCAMPUS_MCP_TOKEN": "your-token-if-auth-is-enabled"
      }
    }
  }
}
```

### Claude Code

```sh
claude mcp add hippocampus -- /path/to/hippocampus-mcp --address localhost:50051
```

## Running with Docker

The bridge fits the **embedded** deployment naturally — one instance, one store, one mind — and there
are two ways to run it against a containerised Hippocampus.

### stdio, against a containerised service (the common local case)

Every compose file publishes the gRPC port (`50051`), so the stdio bridge — spawned by your MCP host
on the host machine — dials it exactly as it would a local process. Nothing extra runs in Docker:

```sh
docker compose up --build                       # Hippocampus in a container, :50051 published
go build -o hippocampus-mcp ./cmd/hippocampus-mcp
claude mcp add hippocampus -- ./hippocampus-mcp --address localhost:50051
```

The host launches the local binary over stdin/stdout; it reaches into the container over `:50051`.

### HTTP, as a compose service (a shared endpoint)

For a network-reachable MCP endpoint (a remote/hosted host, or several hosts sharing one), the SQLite
compose file carries an opt-in `mcp` service on the streamable-HTTP transport, behind a compose
profile so the default `up` is unchanged:

```sh
docker compose --profile mcp up --build
```

It builds the `mcp` image target (`Dockerfile`), dials the `hippocampus` service over the compose
network, and publishes the MCP endpoint on `:8090`. Point an HTTP-capable MCP host at
`http://localhost:8090`. This demo endpoint is **unauthenticated**, like the rest of that stack —
put it behind auth/TLS (or a proxy that terminates them) before exposing it beyond localhost.

## Notes

- The MCP host may dispatch tool calls concurrently. A `store_memory` immediately followed by a
  `list_memories` can therefore race — the store commits, but the list may run first and not see it.
  This is host batching behaviour, not a consistency gap in the store; a subsequent list reflects
  the write.
- `search_memories` requires the service to have its OpenSearch content-search index enabled
  (`opensearch.enabled`); without it the tool returns a `FAILED_PRECONDITION` error, surfaced to the
  model as a tool error.
