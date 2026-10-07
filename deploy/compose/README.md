# Compose stacks

One Compose file per deployment shape, for local runs and evaluation. None of them is hardened: the
databases use trivial passwords, OpenSearch runs with its security plugin off except in the secured
stack, and the service runs without authentication. Use them to see the shapes work, then take the
configuration somewhere real with [Security](../../docs/security.md) beside you.

Run every command from the repository root, since each file builds the service image from it.

| Stack | Command | What it runs |
| :---- | :------ | :----------- |
| SQLite (the default) | `docker compose up --build` | the service on an embedded store in a named volume. The file is `docker-compose.yaml` at the repository root |
| PostgreSQL | `docker compose -f deploy/compose/docker-compose.postgres.yaml up --build` | the service on PostgreSQL |
| MySQL | `docker compose -f deploy/compose/docker-compose.mysql.yaml up --build` | the service on MySQL 8 |
| OpenSearch | `docker compose -f deploy/compose/docker-compose.opensearch.yaml up --build` | SQLite plus an OpenSearch content index, with OpenSearch's security plugin off |
| OpenSearch, secured | `docker compose -f deploy/compose/docker-compose.opensearch-secured.yaml up --build` | the same with HTTPS and basic auth to OpenSearch; set `OPENSEARCH_ADMIN_PASSWORD` first |
| Corporate | `docker compose -f deploy/compose/docker-compose.corporate.yaml up --build` | the centralised shape: PostgreSQL plus OpenSearch, with a stateless service container |

Each stack mounts the `config.*.json` beside its Compose file over the image's own configuration,
except the SQLite stack, which uses `config.sqlite.json` as the image bakes it in. Override
individual keys with `HIPPOCAMPUS_*` environment variables rather than editing the file. The
[configuration reference](../../docs/configuration.md) lists every key.

## Optional profiles

Each profile adds a service that is off by default. Enable it with `--profile <name>`.

| Profile | Stacks | Adds |
| :------ | :----- | :--- |
| `observability` | all | `grafana/otel-lgtm`: Grafana on `:3000` with the Hippocampus dashboard and the shipped alert rules, and an OTLP collector on `:4317`. Set `OBSERVABILITY=true` too, which is what turns the service's export on |
| `ollama` | SQLite, Corporate | Ollama, the model server behind the embedded summariser. Set `LLM=true` too, then pull the model once the stack is up |
| `swagger` | SQLite | Swagger UI on `:8082`, a browser form over the `/v1` API |
| `mcp` | SQLite | the MCP bridge over streamable HTTP on `127.0.0.1:8090` |

```sh
OBSERVABILITY=true docker compose --profile observability up --build
```

`observability/` holds what that profile mounts into Grafana: the dashboard, its provider, and
`alerting-rules.yaml`, which is the Grafana-managed copy of
[`deploy/observability/prometheus-alerts.yaml`](../observability/README.md).

## Ports

Every stack publishes on `127.0.0.1` unless `PUBLISH_ADDRESS` says otherwise. A published port
bypasses a host firewall, so exposing a stack beyond the host is a deliberate step:

```sh
PUBLISH_ADDRESS=0.0.0.0 docker compose up --build
```

The service publishes gRPC on `50051` and the gateway, the console (`/ui`) and the probes on `8080`.
The databases also publish their own ports, for local tooling. The
[port map](../../docs/operations.md#ports) covers every component.
