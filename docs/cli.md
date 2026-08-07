# Command-line client (`hippo`)

`hippo` is a command-line client for a running Hippocampus service. It exposes the full RPC surface
as noun-verb subcommands and can talk to the service over **either** transport:

- **gRPC** (the default) — dials the service's gRPC port (`port`, default `50051`).
- **HTTP** (`--transport http`) — calls the JSON/HTTP `/v1` grpc-gateway (`gateway.port`, default
  `8080`).

Both transports share one client interface, so every command behaves identically whichever is
selected — including the error codes and messages, which are surfaced as gRPC status codes on both.
`hippo` holds no state; it dials the service named by `--address` and turns each command into one
RPC. What a token is actually allowed to do is enforced by the service's [auth
tiers](configuration.md#authorisation), not by this tool.

It lives in its own Go module under `integrations/cli/` (its client dependency tree stays out of the
root build), and ships as a cross-compiled binary on each GitHub release.

## Build and run

```sh
cd integrations/cli
go build -o hippo .

# gRPC (default)
./hippo --address localhost:50051 whoami

# HTTP /v1 gateway
./hippo --transport http --address localhost:8080 whoami
```

If `--address` is omitted it defaults to `localhost:50051` for gRPC and `localhost:8080` for HTTP.
For HTTP, a bare `host:port` gains an `http://` (or `https://` under `--tls`) scheme automatically.

## Global flags

These apply to every command and may appear on either side of the subcommand:

| Flag                         | Default         | Description                                             |
| ---------------------------- | --------------- | ------------------------------------------------------- |
| `-t`, `--transport`          | `grpc`          | transport to the service: `grpc` or `http`              |
| `-a`, `--address`            | (per transport) | service address                                         |
| `--token`                    |                 | bearer token sent on every request                      |
| `--tls`                      | `false`         | connect over TLS                                        |
| `--tls-ca-cert`              |                 | PEM CA bundle to verify the service certificate against |
| `--tls-cert` / `--tls-key`   |                 | client certificate/key for mutual TLS                   |
| `--tls-insecure-skip-verify` | `false`         | skip certificate verification (dev only)                |
| `--timeout-seconds`          | `30`            | per-request timeout (`0` disables)                      |
| `-o`, `--output`             | `text`          | output format: `text` or `json`                         |
| `--log-level`                | `warn`          | logging level written to stderr                         |

Every global flag can also be set via a `HIPPOCAMPUS_<FLAG>` environment variable, so a secret need
not appear in the argv:

```sh
export HIPPOCAMPUS_TOKEN="$(hippocampus --mint-token --client-id ci --role writer --ttl 24h -c config.json)"
hippo memory store --body "authenticated write"
```

## Authentication

Whether a token is needed depends on the service's `auth.method`
([authentication](configuration.md#authentication)):

- **`none`** (the default) — no token; omit `--token` entirely.
- **`hmac`** — mint one with the service's own CLI, signed by the configured secret:

  ```sh
  hippo_token=$(hippocampus --mint-token --client-id ci --role writer --ttl 24h -c config.json)
  hippo --token "$hippo_token" memory store --body "authenticated write"
  ```

  `--role` stamps the [tier](configuration.md#authorisation) the token carries; give it the tier the
  commands you run require (`reader` for the read-only ones, `writer` to store/update, `admin` for
  `purge`/`sleep`/data movement). `--mint-token` is HMAC-only.

- **`idp`** — the service verifies tokens issued by your identity provider, so obtain the bearer
  token from the IdP (the OAuth2/OIDC flow your organisation uses); the service cannot mint it.

Pass the token with `--token`, or as `HIPPOCAMPUS_TOKEN` to keep the secret out of the argv (shown
above). The TLS trust options mirror the service's own [Transfer client and the MCP
bridge](mcp.md): a private-CA bundle, an optional client certificate for mutual TLS, and an
`insecureSkipVerify` escape hatch. `--tls` is required for `https`/TLS endpoints; without it the
client connects in plaintext.

## Output

- `--output text` (default) — a compact human-readable rendering, one field per line. Binary memory
  bodies are shown as a `<binary, N bytes>` placeholder rather than dumped.
- `--output json` — indented [protojson](https://protobuf.dev/programming-guides/json/) using the
  wire field names. This is the stable, machine-parseable form for scripting:

  ```sh
  hippo -o json memory list --group svc-a | jq '.memories[].id'
  ```

Errors are written to stderr as `hippo: <message>` and the process exits non-zero.

## Shell completion

`hippo completion <shell>` prints a completion script for `bash`, `zsh`, or `fish`. The script wires
the shell to `hippo` itself at completion time, so subcommands (`memory store`, `event list`, ...),
flags, and the closed-set flag values (`--transport grpc|http`, `--output text|json`,
`--place-mode`, `--extremum`, `--order-by`, `--log-level`) always match the installed binary.

```sh
# bash — for the current shell, or add to ~/.bashrc
source <(hippo completion bash)

# zsh — requires compinit (usually already run by your setup)
source <(hippo completion zsh)

# fish
hippo completion fish | source
```

To install permanently, write the script into your shell's completions directory (e.g.
`hippo completion bash | sudo tee /etc/bash_completion.d/hippo`).

## Commands

Run `hippo --help` for the list and `hippo <command> --help` for a single command's flags.

### Memories

| Command          | Purpose                                                                                                                                      |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `memory store`   | store a new memory (`--body`/`--body-file`, `--significance`, `--event-id`, `--group`, `--timestamp`, `--binary`)                            |
| `memory update`  | partial update of an existing memory (`--id` plus any content fields)                                                                        |
| `memory delete`  | delete memories by id (`--id` repeatable, or positional ids)                                                                                 |
| `memory list`    | list memories with filters (`--group`, `--significance-min/-max`, `--timestamp-min/-max`, `--order-by`, `--limit`, `--offset`, `--extremum`) |
| `memory recall`  | recall memories by id (reinforces them)                                                                                                      |
| `memory link`    | link a memory to others (`--id`, `--link memoryID:sig` repeatable)                                                                           |
| `memory unlink`  | remove links between a memory and others (`--id`, `--target` repeatable or positional)                                                       |
| `memory links`   | list a memory's links (`--id`, `--direction both\|outbound\|inbound`)                                                                        |
| `memory search`  | content-search the index (`--query`, `--mode keyword\|semantic\|hybrid`, `--limit`, `--event-id`, `--group`, `--reinforce`)                  |
| `memory explain` | where memories stand against consolidation (`--id` repeatable or positional, `--curve-significance`, `--curve-days`, `--curve-points`)       |

`memory explain` reports each memory's computed value, the threshold it is measured against, and how
long it has before it is forgotten; with `--curve-significance` it also returns the decay curve of
the current configuration. Both halves stand alone — a curve with no ids asks only what the
configuration does. See [Where a memory stands](operations.md#where-a-memory-stands).

### Events

| Command              | Purpose                                                                                                                      |
| -------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `event link`         | link an event to others (`--id`, `--link eventID:sig` repeatable)                                                            |
| `event unlink`       | remove links between an event and others (`--id`, `--target` repeatable or positional)                                       |
| `event links`        | list an event's links (`--id`, `--direction both\|outbound\|inbound`)                                                        |
| `event create`       | create an event (`--name`, `--description`, `--significance`, `--group`, `--time-start`, `--time-end`, `--link eventID:sig`) |
| `event end`          | set an event's end time (`--id`, `--time-end`)                                                                               |
| `event significance` | change an event's significance (`--id`, `--significance` or placement)                                                       |
| `event merge`        | re-point one event's memories onto another (`--from`, `--to`)                                                                |
| `event delete`       | delete an event, optionally its memories (`--id`, `--memories`)                                                              |
| `event get`          | fetch a single event (`--id`, `--memories`)                                                                                  |
| `event list`         | list events with filters (same shape as `memory list`, plus `--time-start-min/-max`, `--time-end-min/-max`, `--memories`)    |

### Summarisation

| Command              | Purpose                                                                                       |
| -------------------- | --------------------------------------------------------------------------------------------- |
| `summary candidates` | list events the last sleep cycle flagged as worth condensing                                  |
| `summary replace`    | replace an event's memories with a caller-supplied summary (`--event-id` plus content fields) |
| `summary summarise`  | generate and store a summary with the embedded LLM (`--event-id`, `--significance`)           |

### Admin

| Command  | Purpose                                                         |
| -------- | --------------------------------------------------------------- |
| `whoami` | report the caller's identity and effective tier                 |
| `sleep`  | trigger a consolidation cycle now, or preview one (`--dry-run`) |
| `purge`  | delete every event and memory (requires `--yes`)                |

`sleep --dry-run` reports what a cycle would forget and deletes nothing — see
[Previewing what would be forgotten](operations.md#previewing-what-would-be-forgotten). It calls a
separate read-only RPC (`PreviewConsolidation`), so it cannot trigger a cycle by accident;
`--limit` bounds how many individual memories are detailed (default 100, max 1000) without
affecting the counts, which are always complete.

### Data movement

| Command        | Purpose                                                                                |
| -------------- | -------------------------------------------------------------------------------------- |
| `export`       | snapshot the store into an S3 archive object (`--clear`)                               |
| `import`       | import an archive object from S3 (`--object-key`)                                      |
| `import-batch` | upsert full-state rows from a JSON `ImportBatchRequest` file (`--file`, `-` for stdin) |
| `transfer`     | stream the whole store into a centralised instance (`--clear`)                         |
| `clear`        | delete exactly the records captured by an export/transfer run (`--manifest-id`)        |

## Timestamps and significance placement

- Time flags (`--timestamp`, `--time-start`, `--time-end`, and the `*-min`/`*-max` filters) take
  **RFC3339** (e.g. `2026-08-03T09:30:00Z`); an empty value means "unset" (the server defaults to
  now on create, or applies no bound on a filter).
- Significance can be set relative to existing values instead of absolutely, via the shared
  placement flags: `--place-mode above|below|between`, `--place-anchor`/`--place-anchor-id`, and
  (for `between`) `--place-upper`/`--place-upper-id`. See
  [SignificancePlacement](../contract/hippocampus.proto).

## Examples

```sh
# Store an event and a memory under it, over gRPC.
EV=$(hippo -o json event create --name "release 1.4" --significance 8 --group deploy | jq -r .id)
hippo memory store --body "canary healthy" --significance 6 --event-id "$EV" --group deploy

# The same over the HTTP gateway, with the token from the environment.
export HIPPOCAMPUS_TOKEN=... HIPPOCAMPUS_TRANSPORT=http HIPPOCAMPUS_ADDRESS=localhost:8080
hippo memory list --group deploy --significance-min 5 --limit 50

# Ingest full-state rows exported elsewhere.
hippo import-batch --file batch.json

# Destructive operations are gated.
hippo purge --yes
```
