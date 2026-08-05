# Configurability

The exhaustive reference for every configuration key. All settings come from the JSON config
file (see [`config.json`](../config.json) for a complete example) loaded via viper. For a
guided first configuration, start with [Getting started](getting-started.md); for tuning in
production, see the [Operations guide](operations.md).

## Environment variable overrides

Any config key can be overridden by an environment variable named `HIPPOCAMPUS_<KEY>` with the
key's dots replaced by underscores and uppercased — so `auth.signingSecret` becomes
`HIPPOCAMPUS_AUTH_SIGNINGSECRET`, `storage.postgres.dsn` becomes `HIPPOCAMPUS_STORAGE_POSTGRES_DSN`,
`opensearch.password` becomes `HIPPOCAMPUS_OPENSEARCH_PASSWORD`, and `transfer.token` becomes
`HIPPOCAMPUS_TRANSFER_TOKEN`. This is the recommended way to supply secrets — inject them as
Docker/Kubernetes secrets rather than committing or baking them into `config.json`. Precedence is
**command-line flag > environment variable > config file > built-in default**, so an env var
overrides the file but an explicit flag still wins.

## Operational

### Logging

Logging is written to stdout via [logrus](https://github.com/sirupsen/logrus).

```json
"logging": {
    "level": "info",
    "json": false
}
```

- `logging.level` — the minimum severity emitted. One of `trace`, `debug`, `info`, `warn`,
  `error`, `fatal`, `panic` (case-insensitive; the first letter and a few aliases such as
  `verbose`→`debug` and `information`→`info` also work). Any unset or unrecognised value falls back
  to `info`. The service logs its effective level at startup. Most per-function entry/exit lines are
  at `trace`; a failing RPC is logged at `warn` (`info` for client-fault codes), so the default
  `info` level surfaces failures without the per-call noise.
- `logging.json` — when `true`, emit structured JSON (one object per line) instead of the default
  human-readable text formatter. Use JSON when shipping logs to a collector that parses fields
  (Loki, Elasticsearch, CloudWatch); leave it `false` for local development.

### Observability

OpenTelemetry tracing and metrics are optional and independently enabled through the
`observability` section of the configuration file. Both export over OTLP/gRPC.

```json
"observability": {
    "tracing": {
        "enabled": false,
        "samplingRatio": 1.0
    },
    "metrics": {
        "enabled": false,
        "exportIntervalSeconds": 60
    },
    "otlp": {
        "endpoint": "localhost:4317",
        "insecure": true
    }
}
```

- `tracing.samplingRatio` — the fraction of traces to sample (0.0–1.0). The sampler is
  parent-based, so sampling decisions propagated by callers are honoured; the ratio applies to
  traces started locally (each RPC without an incoming trace context, and each sleep cycle).
- `metrics.exportIntervalSeconds` — how often metrics are exported; 0 uses the SDK default.
- `otlp.endpoint` — the OTLP/gRPC collector endpoint. When empty, the standard
  `OTEL_EXPORTER_OTLP_*` environment variables apply, falling back to `localhost:4317`.
- `otlp.insecure` — use plaintext instead of TLS when connecting to the collector.

Every RPC is traced (via the `otelgrpc` stats handler, which also records the standard
low-cardinality RPC metrics), and each sleep cycle produces its own trace with span events
marking the consolidation passes. Domain metrics cover stored/rejected/recalled/deleted
counts for memories and events, consolidation deletions, capacity evictions (row counts and the
estimated bytes reclaimed), a histogram of stored memory-body sizes, sleep cycle count
and duration, capacity pressure, the store's used bytes, gauges of the current event and
memory counts, the number of summarisation candidates found by the most recent sleep cycle, and
memories/summaries created via `ReplaceMemoriesWithSummary`. All metric attributes are bounded
(booleans or small enumerations) to keep cardinality low.

To view any of this locally without standing up a collector, every compose stack carries an
optional all-in-one `grafana/otel-lgtm` backend behind a compose `observability` profile:
`OBSERVABILITY=true docker compose --profile observability up` brings up Grafana on
`http://localhost:3000` and ships to it; the demo soak harness has the same switch
(`OBSERVABILITY=1 ./demo/run.sh`). Both are off by default, so a stack run without the profile
never attempts an export. See [Observability](operations.md#observability) in the operations guide.

The event/memory count gauges and the periodic stats log line share one cached count reading rather
than each running a full-table `COUNT`. `stats.intervalSeconds` (default **300**) sets both how
often the stats line is logged and the maximum age of that cached reading, so the underlying counts
run at most once per interval no matter how often metrics are exported. Set it to `0` to disable the
log line (the gauges still serve a cached count).

```json
"stats": {
    "intervalSeconds": 300
}
```

### HTTP gateway

Every RPC is also reachable as a JSON/HTTP endpoint, for clients that would rather not speak
gRPC. The gateway is **off by default**; set `gateway.port` to a port to enable an in-process
[grpc-gateway](https://github.com/grpc-ecosystem/grpc-gateway) reverse proxy (0, the default,
disables it). **8080** is the conventional port, and the Docker configurations use it. The gateway
calls straight into the same server instance gRPC uses, so there is no extra network hop, dial, or
serialisation round trip between the two. An OpenAPI/Swagger description of the mapping below is
served at `/v1/openapi.json`.

```json
"gateway": {
    "port": 8080,
    "maxRequestBytes": 0
}
```

`gateway.maxRequestBytes` caps the size of a request body the gateway will read (0, the default,
leaves it unbounded). It is off by default because a legitimate `ImportBatch`/`Transfer` body can be
large; set a ceiling when the gateway is reachable by untrusted callers, so an oversized body is
rejected by the transport (with `413`) before a handler buffers it. The native gRPC transport
already bounds a request at its 4 MiB default. TLS pins a **TLS 1.2 floor** on both listeners (see
[TLS](#tls)).

The gRPC server port is `port` (default **50051**). Both ports can be set on the command line —
`--port` and `--gateway-port` — which takes precedence over the config file:

```sh
hippocampus -c config.json --gateway-port 8080   # enable the gateway on the conventional port
hippocampus -c config.json --port 40000          # gRPC on a custom port; gateway stays off
```

Both listeners bind all interfaces by default. `bindAddress` (gRPC) and `gateway.bindAddress`
(HTTP) restrict the interface each binds to — set them to `127.0.0.1` to accept only loopback
traffic, e.g. behind a sidecar/mesh that terminates TLS and forwards over the loopback. Empty (the
default) preserves the all-interfaces behaviour.

To run several independent instances on one host, give each its own `port`, `gateway.port`, and
`storage.directory`/DSN — the defaults collide and the second to start fails to bind. See
[Running more than one instance on one host](operations.md#running-more-than-one-instance-on-one-host).

```json
"bindAddress": "",
"maxConcurrentStreams": 0,
"maxRecvMsgBytes": 0,
"keepalive": {
    "minTimeSeconds": 0,
    "permitWithoutStream": false
}
```

The gRPC transport has a few hardening knobs, all defaulting to grpc-go's own defaults when
unset — tolerable for trusted callers, worth setting if the gRPC port is exposed further.
`maxConcurrentStreams` caps the concurrent HTTP/2 streams one connection may open (0 keeps the
default). `keepalive.minTimeSeconds` is the minimum interval the server tolerates between a
client's keepalive pings before it terminates the connection (0 leaves grpc-go's policy), and
`keepalive.permitWithoutStream` allows those pings on an idle connection. `maxRecvMsgBytes`
(0 → grpc-go's 4 MiB default) raises the maximum receive-message size for larger
`ImportBatch`/single-memory payloads.

Each RPC maps onto a REST-ish path under `/v1`; path segments in `{braces}` come from the URL,
`GET`/`DELETE` take their remaining fields as query parameters, and `POST`/`PATCH` take them as
a JSON body:

| RPC                          | Method | Path                              |
| ---------------------------- | ------ | --------------------------------- |
| `StoreEvent`                 | POST   | `/v1/events`                      |
| `GetEvents`                  | GET    | `/v1/events`                      |
| `GetEventById`               | GET    | `/v1/events/{id}`                 |
| `DeleteEvent`                | DELETE | `/v1/events/{id}`                 |
| `EndEvent`                   | POST   | `/v1/events/{id}/end`             |
| `UpdateEventSignificance`    | PATCH  | `/v1/events/{id}/significance`    |
| `MergeEvents`                | POST   | `/v1/events/merge`                |
| `ReplaceMemoriesWithSummary` | POST   | `/v1/events/{event_id}/summary`   |
| `StoreMemory`                | POST   | `/v1/memories`                    |
| `UpdateMemory`               | PATCH  | `/v1/memories/{id}`               |
| `GetMemories`                | GET    | `/v1/memories`                    |
| `DeleteMemories`             | POST   | `/v1/memories/delete`             |
| `RecallMemories`             | POST   | `/v1/memories/recall`             |
| `GetSummarisationCandidates` | GET    | `/v1/summarisation/candidates`    |
| `SummariseMemories`          | POST   | `/v1/events/{event_id}/summarise` |
| `Export`                     | POST   | `/v1/export`                      |
| `Import`                     | POST   | `/v1/import`                      |
| `ImportBatch`                | POST   | `/v1/import/batch`                |
| `Transfer`                   | POST   | `/v1/transfer`                    |
| `Clear`                      | POST   | `/v1/clear`                       |
| `Sleep`                      | POST   | `/v1/sleep`                       |
| `Purge`                      | POST   | `/v1/purge`                       |
| `WhoAmI`                     | GET    | `/v1/whoami`                      |

`ReplaceMemoriesWithSummary`'s body maps directly to its `summary` field (a `Memory`), rather
than the whole request, so a client posts a plain memory object to
`/v1/events/{event_id}/summary` without a wrapper. `/healthz` and `/readyz` are always reachable
without authentication, for liveness/readiness probes (see [Health and readiness](#health-and-readiness)); every other path, including
`/v1/openapi.json`, is subject to [Authentication](#authentication) when it is enabled.

The `GetEvents` and `GetMemories` list endpoints additionally accept a `significance_extremum`
query parameter (`SIGNIFICANCE_EXTREMUM_HIGHEST` or `SIGNIFICANCE_EXTREMUM_LOWEST`): in place of a
`significance_min`/`significance_max` range, it returns only the events/memories tied at the single
highest (or lowest) significance value among those matching the other filters (time range, group) —
computed dynamically, not against a caller-supplied bound. It is mutually exclusive with
`significance_min`/`significance_max`; supplying both is rejected with `InvalidArgument`. The
lowest-significance set is exactly what the next sleep cycle forgets first, which makes it a handy
lens on consolidation (see [Demonstrations](demonstrations.md)). The full field-level request and
response schema for every endpoint lives in the OpenAPI description at `/v1/openapi.json`.

### Health and readiness

The gateway exposes two probe endpoints, both always open (no token) so orchestrators can reach
them:

- `/healthz` — **liveness**: the process is up. It never touches the database, so a slow or
  unreachable store does not make it fail; point a probe that _restarts_ the container here, so a
  transient dependency outage does not trigger a restart loop that cannot fix it. Its JSON body
  carries the running build's `version` (the same value `--version` prints), so an operator can tell
  what is deployed.
- `/readyz` — **readiness**: the process is up _and_ the database answers a ping. Returns `200`
  when the store is reachable, `503` otherwise. Point load-balancer / service readiness probes
  here so an instance whose database has become unreachable is drained rather than kept in
  rotation while every RPC fails. The gRPC health service (`grpc.health.v1.Health`) reflects the
  same signal, flipping between `SERVING` and `NOT_SERVING`.

```json
"readiness": {
    "pingTimeoutSeconds": 0,
    "cacheSeconds": 0
}
```

`readiness.pingTimeoutSeconds` bounds each database ping and `readiness.cacheSeconds` caches the
result so a burst of probes collapses to at most one ping per window; both fall back to internal
defaults (2 s and 3 s) when left at 0. The Docker image's `HEALTHCHECK` targets `/readyz`.

### Version

`hippocampus --version` prints the build identification and exits (before the config file is read).
The value comes from the Go module version plus the VCS revision/time the toolchain embeds at build
time, so it identifies the exact commit even for a `go build` from a working tree. The same string
is logged at startup, reported in the `/healthz` body, and set as the OTEL `service.version`
resource attribute when observability is enabled. The Docker image also carries an
`org.opencontainers.image.version` label (`--build-arg VERSION=<tag>`).

### Authentication

Every RPC — on both the native gRPC service and the HTTP gateway — can require a signed bearer
token. `auth.method` selects the scheme:

- `none` (the default) — no authentication, preserving the previous no-auth behaviour.
- `hmac` — tokens are signed and verified with shared HS256 secrets, minted by the service binary
  itself via `--mint-token`. HS256 is keyed directly with the raw secret, so use long, random
  values — at least 32 bytes (256 bits, matching the hash output); a shorter secret is
  brute-forceable and logs a startup warning. A single secret (`auth.signingSecret`) is the simplest
  form; `auth.signingKeys` holds several `kid`-identified secrets at once so one can be rotated in
  while tokens signed by another still verify (see [Key rotation](#key-rotation-hmac)), and
  `auth.revocationFile` cuts off individual tokens or clients without waiting out their TTL (see
  [Revocation](#revocation)).
- `idp` — tokens are issued by an external identity provider and verified as RS256 against the
  provider's published JWKS. `auth.jwksUrl` names the key-set endpoint directly, or `auth.issuer`
  alone resolves it via OIDC discovery (`<issuer>/.well-known/openid-configuration`). When
  `auth.issuer` is set it is also enforced against every token's `iss` claim, and
  `auth.audience`, when set, against `aud`. Keys are cached by `kid` and re-fetched every
  `auth.jwksRefreshIntervalSeconds` (300 by default); a token presenting an unknown `kid` forces
  one rate-limited early re-fetch, so a provider-side key rotation is picked up on first sight
  without restarting the service. The provider must be reachable at startup (the initial key
  fetch failing fails startup), but a later outage only stops key refreshes — cached keys keep
  verifying requests.

```json
"auth": {
    "method": "none",
    "signingSecret": "",
    "signingKeys": [],
    "activeKid": "",
    "revocationFile": "",
    "revocationRefreshSeconds": 30,
    "jwksUrl": "",
    "issuer": "",
    "audience": "",
    "jwksRefreshIntervalSeconds": 300,
    "roleClaim": "roles",
    "roleMapping": {},
    "readerRecallReinforces": false,
    "ui": {
        "issuer": "",
        "clientId": "",
        "scopes": "openid profile",
        "audience": ""
    }
}
```

`auth.roleClaim`, `auth.roleMapping`, and `auth.readerRecallReinforces` drive
[authorisation](#authorisation) and are only consulted when authentication is enabled.

`auth.ui` is the **front-channel** configuration the embedded web console (`/ui`) reads — from the
unauthenticated `GET /ui/config` endpoint — to start a browser OIDC login under `auth.method: idp`.
It carries no secret: only the public single-page-app `clientId`, the `scopes` to request
(default `openid profile`), the `issuer` the browser runs OIDC discovery against (defaults to
`auth.issuer`), and an optional `audience`. The audience matters for providers whose access token is
opaque unless an API audience is requested — **Auth0** needs it set to the API identifier to mint a
verifiable JWT; **Keycloak** issues a JWT without it. The access token the flow returns is sent as
the `Authorization: Bearer` on every `/v1` call and verified exactly like any other `idp` token, so
the tier still comes from `auth.roleClaim`/`auth.roleMapping`. When `auth.method` is `none` or
`hmac`, `/ui/config` simply reports the method and the console falls back to its manual bearer-token
box.

#### Server-side sign-in (`auth.oauth2`)

`auth.ui` above runs the OIDC Authorisation Code + PKCE flow **in the browser** as a public client
(no secret; the token lives in the page's `sessionStorage`). `auth.oauth2` is the alternative: the
**service itself** runs the flow as a **confidential client** — the code-for-token exchange happens
on the server with a client secret, and the resulting session rides an `HttpOnly` cookie the page
can never read. Prefer it when the identity provider requires a confidential client, or to keep the
token out of page-readable storage (mitigating token theft via XSS). It is available only under
`auth.method: idp`, and it does not change how tokens are _verified_ — the cookie carries the same
IdP access token, checked by the same `idp` verifier as a bearer header.

```jsonc
"auth": {
  "method": "idp",
  "issuer": "https://idp.example.com/realms/hippocampus",
  "audience": "hippocampus-api",              // API audience for the access token (as for idp)
  "roleClaim": "realm_access.roles",
  "oauth2": {
    "enabled": true,
    "clientId": "hippocampus-console",         // a CONFIDENTIAL client at the provider
    "clientSecret": "…",                       // inject via HIPPOCAMPUS_AUTH_OAUTH2_CLIENTSECRET
    "redirectUrl": "https://hippo.example.com/auth/callback",  // must be registered at the provider
    "scopes": "openid profile email offline_access"           // offline_access → a refresh token
    // optional: issuer/audience (default to auth.issuer/auth.audience), cookieSecure (defaults to
    // the redirectUrl scheme), cookieDomain, successRedirectUrl (default /ui),
    // postLogoutRedirectUrl, sessionTTLSeconds, refreshTTLSeconds
  }
}
```

When enabled, the gateway serves four endpoints — `GET /auth/login` (start the flow),
`GET /auth/callback` (the provider's redirect target; exchanges the code, verifies the `id_token`
signature and `nonce`, sets the session cookie), `POST /auth/refresh` (swap the refresh cookie for a
fresh access token), and `GET /auth/logout` (clear the cookies and, when the provider advertises an
`end_session_endpoint`, RP-initiated logout) — all reachable without a token. The session cookie
(`hippo_session`) is `HttpOnly`, `SameSite=Lax`, and `Secure` (following the `redirectUrl` scheme
unless `cookieSecure` overrides); the refresh cookie is scoped to `/auth` so it is never sent to the
API. `/ui/config` reports `loginMode: "server"`, and the console shows a **Sign in** button that
navigates to `/auth/login` instead of running the in-page flow. The `redirectUrl` **must** point at
the service's own `…/auth/callback` and be registered as an allowed redirect on the provider's
client. Because the client secret is a secret, inject it via
`HIPPOCAMPUS_AUTH_OAUTH2_CLIENTSECRET` rather than committing it. Serve the console over HTTPS
(directly or via a TLS-terminating proxy) so the `Secure` session cookie is not dropped.

`auth.signingKeys` is a structured list (`[{ "kid": "...", "secret": "..." }]`) and so is
config-file-only — unlike `auth.signingSecret`, it cannot be injected through a single
`HIPPOCAMPUS_AUTH_*` environment variable.

The boolean `auth.enabled` from earlier releases remains as a deprecated alias: when
`auth.method` is unset, `auth.enabled: true` selects `hmac` (with a startup warning) and
`auth.enabled: false` selects `none`, so existing configs keep working unchanged.

Whichever method is active, present the token as `Authorization: Bearer <token>` over HTTP, or
as an `authorization: Bearer <token>` metadata entry over gRPC — the same header value works
either way. A missing or invalid token gets `codes.Unauthenticated` from gRPC, or `401` (with a
`WWW-Authenticate: Bearer` header) from the HTTP gateway. The gRPC health service and the
gateway's `/healthz` are always reachable without a token, regardless of method, so orchestrator
liveness/readiness probes are never blocked. Both verifiers pin their signing algorithm
(`jwt.WithValidMethods` — HS256 for `hmac`, RS256 for `idp`), so a token can never select its
own verification algorithm, and both require an `exp` claim (`jwt.WithExpirationRequired`), so a
token minted without an expiry is rejected rather than being valid forever (`--mint-token` always
sets one).

Under `hmac` there is no client registry or admin RPC — issuing a token is a CLI operation on
the service binary itself:

```sh
hippocampus --mint-token --client-id my-client --role writer --ttl 24h -c config.json
```

This prints a signed token to **stdout** and exits without starting the server (or touching the
database); it also prints the token's `jti` (its unique id), client, and roles to **stderr**, so
`token=$(hippocampus --mint-token ...)` still captures only the token while an operator can record
the `jti` for later [revocation](#revocation). `--role` is **required** and names one or more
[authorisation](#authorisation) tiers — `reader`, `writer`, and/or `admin` (repeatable, or
comma-separated: `--role reader,writer`); a role-less token authorises nothing under the
default-closed policy, so minting one is refused. `--ttl` accepts any Go duration (`1h`, `24h`,
`720h`, ...); `--signing-secret` can override the config's secret for minting on a host that only
has the secret and not the full deployed config. When `auth.signingKeys` is configured, the token
is signed with `auth.activeKid` (or the first listed key) and stamped with that `kid`; `--kid`
overrides which key to use. Under `idp` the identity provider owns issuance, expiry, and
revocation, and `--mint-token` refuses to run.

Verification is written behind an interface (`auth.Verifier`) — `hmac` and `idp` are its two
implementations, and the interceptor/middleware call sites are identical for both.

The verified `client_id` is logged on every failing request (and, on the HTTP gateway, every
request), so a leaked token can be traced to the client it was issued to. What each token may _do_
is governed by [authorisation](#authorisation) below.

### Authorisation

When authentication is enabled, each RPC also requires a minimum **role tier**, carried in the
token's `roles` claim. The tiers nest — `reader` ⊂ `writer` ⊂ `admin` — so a higher tier can do
everything a lower one can:

| Tier     | May call                                                                                                                                                                                                                                     |
| -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `reader` | `GetEvents`, `GetEventById`, `GetMemories`, `SearchMemories`, `RecallMemories`, `GetSummarisationCandidates`, `WhoAmI`                                                                                                                       |
| `writer` | everything `reader` can, plus `StoreEvent`, `EndEvent`, `UpdateEventSignificance`, `MergeEvents`, `DeleteEvent`, `StoreMemory`, `UpdateMemory`, `DeleteMemories`, `ReplaceMemoriesWithSummary`, `SummariseMemories`, `Import`, `ImportBatch` |
| `admin`  | everything `writer` can, plus `Purge`, `Sleep`, `Export`, `Transfer`, `Clear`                                                                                                                                                                |

`Export`/`Transfer` are `admin` because they read the whole store out; `Import`/`ImportBatch` are
`writer` because they deliberately bypass the write-path validation `StoreMemory`/`StoreEvent`
enforce (body-size limit, future-timestamp clock-skew guard, minimum-significance gate) to restore
an archive faithfully. A caller whose token grants a tier below the RPC's requirement gets
`codes.PermissionDenied` from gRPC, or `403` from the HTTP gateway (distinct from the `401` an
_unauthenticated_ request gets). The same policy governs both transports from one table, so gRPC
and the gateway can never diverge.

Authorisation is **default-closed**: a token whose roles resolve to no known tier is denied every
RPC. Because tokens minted before this release carry no `roles` claim, **enabling a newer build
against existing tokens requires re-minting them with a `--role`** (or, under `idp`, adding roles
to the provider's token). With `auth.method: none` there is no authenticated principal, so
authorisation is off and every RPC is reachable, exactly as before.

Under `hmac`, `--mint-token --role` stamps the tier names (`reader`/`writer`/`admin`) directly.
Under `idp`, the provider supplies them: by default they are read from a top-level `roles` claim;
`auth.roleClaim` points at a differently-named claim when the provider uses one. The claim name is
resolved literally first — so a provider that namespaces roles under a URI-shaped key (Auth0's
`https://your-domain/roles`) matches as a top-level key — and, when no literal key exists, as a
dotted path through nested objects, so Keycloak's `realm_access.roles` also resolves.
`auth.roleMapping` translates a
provider's own group names onto the tiers, e.g. `{ "hippo-ops": "admin", "hippo-app": "writer" }`;
the tier names always map to themselves, so a mapping is only needed for other names.

`RecallMemories` and `SearchMemories` are `reader`-tier: recall is a read as far as _access_ goes,
even though it normally reinforces the memory (resets its decay clock, raises its recall count).
Whether a **reader** actually triggers that reinforcement is controlled globally by
`auth.readerRecallReinforces` (default `false`): left off, a reader's recall/search returns the
memories without the write side effect; turned on, readers reinforce like anyone else. `writer` and
`admin` callers always reinforce.

#### Key rotation (hmac)

`auth.signingKeys` lets several HS256 secrets be trusted at once, each tagged with a `kid` that is
written into the header of every token it signs. Because the verifier trusts _every_ listed key, a
new secret can be introduced and start signing while tokens signed by the previous secret keep
verifying — so a rotation never has a flag day where outstanding tokens are abruptly rejected. The
procedure:

1. Add the new key to `auth.signingKeys` and point `auth.activeKid` at it (keep the old key in the
   list); restart. New tokens are now signed with the new key; old tokens still verify.
2. Re-mint client tokens against the new key as they expire, or all at once.
3. Once no outstanding tokens are signed by the old key (they have expired or been re-minted),
   remove it from `auth.signingKeys` and restart.

`auth.signingSecret` and `auth.signingKeys` can be set together: `signingSecret` verifies tokens
that carry no `kid` (every token minted before rotation existed), while `signingKeys` verifies
`kid`-tagged ones — a convenient way to migrate an existing single-secret deployment onto keyed
rotation without invalidating its live tokens. A token whose `kid` names no configured key is
rejected.

#### Revocation

Two mechanisms cut a credential off before its TTL expires:

- **Rotate the signing secret** — changing `auth.signingSecret` (or removing a key from
  `auth.signingKeys`) invalidates every token signed by it at once. Coarse, but immediate.
- **A revocation file** (`auth.revocationFile`) — a JSON file, polled for changes every
  `auth.revocationRefreshSeconds` (30 by default), that revokes named tokens or clients without a
  restart and without affecting anyone else:

  ```json
  {
    "jtis": ["9fdec40d972eb3a4be7297c29ed95061"],
    "clients": [
      { "clientId": "decommissioned-batch-loader" },
      {
        "clientId": "rotated-web-console",
        "issuedBefore": "2026-07-01T00:00:00Z"
      }
    ]
  }
  ```

  `jtis` revokes individual tokens by the `jti` printed at mint time. A `clients` entry revokes
  every token for that `client_id`; adding `issuedBefore` (an RFC 3339 timestamp) revokes only
  tokens issued before it — the per-client rotation move: set the cutoff to now, then mint the
  client a fresh token. Changes take effect within `auth.revocationRefreshSeconds`, on both the
  gRPC service and the HTTP gateway. A named-but-unreadable or malformed file **fails startup** (so
  a typo can't silently revoke nothing); a bad file written _after_ startup is ignored with an
  error log, keeping the last good list in force. The check runs _after_ signature verification and
  works in front of `idp` as well as `hmac`, so a provider-issued token can be revoked locally even
  when the provider's own revocation lags.

### TLS

`tls.enabled` (`false` by default) turns on TLS for both the gRPC service and the HTTP gateway,
sharing one certificate/key pair and enforcing a **TLS 1.2 minimum** on both listeners (weaker
legacy protocol versions are refused):

```json
"tls": {
    "enabled": false,
    "certFile": "",
    "keyFile": ""
}
```

Enabling authentication without `tls.enabled` logs a startup warning rather than refusing to
start — bearer tokens sent over plaintext are sniffable in transit, but some deployments
terminate TLS upstream (a reverse proxy or service mesh) rather than in the service itself, which
is a legitimate topology this doesn't try to prevent.

### Storage

`storage.driver` selects the storage backend. The default, `sqlite`, is the embedded,
zero-dependency database every prior release used, stored in `storage.directory`. Setting it to
`postgres` stores everything in the PostgreSQL database named by `storage.postgres.dsn` instead,
and `mysql` in the MySQL database named by `storage.mysql.dsn` (go-sql-driver format, e.g.
`user:password@tcp(host:3306)/dbname`; the DSN must name a database schema):

```json
"storage": {
    "driver": "sqlite",
    "directory": "./data",
    "queryTimeoutSeconds": 60,
    "compression": {
        "enabled": true,
        "minBytes": 512
    },
    "pool": {
        "maxOpenConns": 25,
        "maxIdleConns": 0
    },
    "postgres": {
        "dsn": ""
    },
    "mysql": {
        "dsn": ""
    }
}
```

`storage.queryTimeoutSeconds` bounds how long any single statement or transaction may run (default
60; 0 leaves them unbounded). The default is comfortably above a full consolidation scan at the
benchmarked sizes, so a hung or unreachable database fails an operation after a bounded time instead
of blocking the request goroutine — and its pooled connection — indefinitely. Raise it above the
longest legitimate operation on a larger store, notably a full consolidation scan, or a sleep cycle
could be aborted mid-scan; set it to 0 to disable the bound (reasonable for embedded SQLite).

#### Body compression

`storage.compression.enabled` (**default true**) stores memory bodies compressed. Storage is the
resource the whole service manages to, so what it buys is retention: the same
`consolidation.capacityBytes` target holds more memories, and forgetting starts later. What it costs
is CPU on every operation that touches a body — writes compress, and reads of the body
(`GetMemories`, `RecallMemories`, `SearchMemories`, export) decompress. Measured against the
database work it sits beside, that is about **2.4%** on a store-and-read round trip, against
savings of 4–8× on bodies that clear the size threshold; see
[Body compression](performance.md#body-compression) in the performance notes for the benchmarks.
The consolidation and eviction scans are unaffected: they read the covering index and never load a
body at all.

Set it to `false` for a workload that reads large pages of small, poorly-compressing bodies, where
the per-memory decompression multiplies without much storage to show for it.

The algorithm is gzip and is deliberately **not** configurable — a body written by one version of
the service has to stay readable by every later one, which a configurable algorithm would make a
matter of nobody ever changing the key. What is configurable is only whether new bodies are
compressed on the way in. (The compression _level_ is an encoder-side choice that any gzip reader
decodes regardless, so it carries no such commitment; the service uses the fastest one, which on
realistic bodies gives up almost no ratio.)

That setting can be changed at any point in a store's life. Each row records whether its own body
was compressed, and reads are driven entirely by that flag rather than by the current configuration,
so a store may hold a mix of both and every row still reads correctly. Turning compression on does
not rewrite the bodies already stored (they are compressed if and when they are next updated);
turning it off leaves every compressed row readable.

One consequence worth anticipating when it changes on an existing store: because the capacity target
counts stored bytes, the store's used figure — and with it the capacity pressure that scales the
deletion threshold — moves as new writes land under the new setting. Turning compression on makes a
store effectively larger, so it forgets **less** aggressively at the same `capacityBytes`; turning it
off does the reverse. Re-check `capacityBytes` against the behaviour you want after either change.

Three rules narrow what is actually compressed, so the feature cannot cost storage:

- Bodies below `storage.compression.minBytes` (default 512) are stored verbatim — small bodies
  compress poorly once gzip's own ~18-byte header and trailer are counted. A value below 64 is
  raised to it, since a smaller threshold only buys compression attempts that the next rule rejects.
- **Binary** memories are never compressed. Their bodies are client-encoded and as likely to be an
  already-compressed payload as anything else.
- A body that compression did not actually make smaller is stored verbatim. On incompressible
  content the worst case is therefore the CPU of the attempt, never a body that grew.

`UsedBytes` — and so the capacity target and eviction — accounts for what is really on disk, which
is the compressed size. Sizes seen elsewhere are the logical ones: the `memory.body_bytes` histogram
measures the body as the client sent it, and `memory.limit.sizeBytes` is likewise checked against the
body as submitted, not against its stored size.

Compression is confined to the storage layer. Everything above it — the RPCs, the OpenSearch index,
the summariser, and the export/archive format — sees plain bodies, so an archive is portable between
instances whatever either one's setting is; an import takes the receiving instance's policy.

`storage.pool.maxOpenConns` (default 25) and `storage.pool.maxIdleConns` (0 → defaults to
`maxOpenConns`) cap the connection pool on the `postgres`/`mysql` drivers, where `database/sql`
otherwise allows unlimited open connections and a burst of concurrent RPCs can exhaust the shared
database's connection slots. SQLite is always single-connection and ignores these. In a replicated
deployment keep the **sum** of `maxOpenConns` across every instance below the server's
`max_connections`, leaving headroom for the consolidator's instance-lock keepalive connection and
any superuser reserve — see [Operations](operations.md#connection-pool-sizing-server-drivers).

MySQL support requires MySQL 8.0.20 or later (the upserts use the `ON DUPLICATE KEY UPDATE` row
alias). One MySQL-specific bound: ids are `VARCHAR(255)` there (MySQL cannot index an unbounded
`TEXT` column), so client-supplied event and memory ids longer than 255 characters are rejected
by that driver; generated UUID ids are unaffected.

Exactly one instance may run consolidation against a given store. With SQLite that is one instance
full stop — the embedded database cannot be shared between processes. With PostgreSQL or MySQL the
_consolidating_ instance takes a session-scoped advisory lock at startup (MySQL: a `GET_LOCK` lock
scoped to the schema name); a second instance that also has `consolidation.enabled: true` refuses
to start, rather than silently running concurrent consolidation cycles against shared data.
Additional instances with `consolidation.enabled: false` skip the lock and run alongside it as
read/write replicas — see [Horizontal scaling](../README.md#horizontal-scaling).

Both capacity axes (`consolidation.capacityMemories` and `consolidation.capacityBytes`) work
with every driver, but the byte measure differs. SQLite reads the database's pages excluding
the freelist; the server drivers estimate the live rows directly (payload bytes plus the same
fixed per-row allowance eviction uses when estimating freed bytes), because no server file-size
measure ever shrinks after deletes — Postgres's autovacuum and InnoDB's purge make freed space
internally reusable without returning it to the filesystem, so a `pg_database_size()`-style
reading would sit at its high-water mark and keep eviction firing against a figure that cannot
drop. The estimate costs one scan of the two tables per sleep cycle, and only when a byte
capacity is configured. `consolidation.walTriggerBytes` remains SQLite-only (it measures the
on-disk WAL file, which has no server-driver counterpart) and is rejected at startup. There is
also no `Preserve` compaction step on the server drivers — their background reclamation already
handles dead rows continuously.

### Content search

Recall is normally by id, event, or time/significance range. `SearchMemories`
(`POST /v1/memories/search`) adds one more read path: it finds memories whose body matches a query,
most relevant first, optionally restricted to one event and/or one `group` label.

There are two backends, and **you get one without configuring anything**:

| Backend     | When it is used                          | Drivers       | Notes                                                                            |
| ----------- | ---------------------------------------- | ------------- | -------------------------------------------------------------------------------- |
| Store index | `opensearch.enabled` false (the default) | `sqlite` only | An FTS5 index inside the same database file. No cluster, no configuration.       |
| OpenSearch  | `opensearch.enabled` true                | all           | A separate cluster. Scales further and is the only option on `postgres`/`mysql`. |

On the `postgres` and `mysql` drivers with OpenSearch disabled there is no content search at all,
and `SearchMemories` returns `FAILED_PRECONDITION` saying so. A log line at startup reports which
backend was selected.

Both backends are strictly **secondary**: the primary store remains the system of record, results
are always re-read from it, and binary memories (`is_binary`) are never indexed by either.

#### The store's own index (SQLite)

The default. Ranking is FTS5's bm25, and matching is an OR over the query's words — the same
semantics as the OpenSearch backend's, so moving between the two changes scale, not results.

Query text is treated as **words, not as a query language**: FTS5 operators (`AND`, `NEAR`, `*`,
`column:`) and quotes are neutralised rather than honoured, so no input is a syntax error and none
can reach into the query's structure. Ranking still favours memories matching more of the words.

What differs from OpenSearch, and mostly in this backend's favour:

- **It cannot go stale.** The index is maintained inside the write itself, not by an asynchronous
  worker, so there is no propagation queue to overflow and no reconciliation sweep to wait for. A
  memory is findable as soon as `StoreMemory` returns.
- **Deletes cannot drift.** They are handled by a database trigger, so every path that removes a
  memory — consolidation, eviction, purge, summary replacement — is covered by construction.
- **No second copy of your content.** The index is contentless: it holds the inverted index and
  not the bodies, so unlike an OpenSearch deployment it neither duplicates your text nor gives
  back the storage [body compression](#body-compression) saves.
- **Upgrades populate it automatically.** A database written before this existed gains the index
  on the next startup, and it is filled from the memories already stored — no backfill step.
- It is bounded by the single SQLite instance, where OpenSearch scales independently and serves
  the `postgres`/`mysql` deployments as well.

The one drift it can suffer is an index write that failed and was logged rather than failing its
memory, leaving a memory stored but unfindable.
[`--backfill-search`](#backfill-and-reindex) rebuilds the index to repair that; with no OpenSearch
configured, that flag rebuilds this index instead. It **writes to the database**, so stop the
service first.

#### OpenSearch

**Enabling on a store that already has data:** the index starts empty and nothing already stored is
retroactively indexed, so existing memories stay unsearchable until they are re-indexed. Run a
one-time [`--backfill-search`](#backfill-and-reindex) to populate it immediately; on the
consolidating instance the [self-healing sweep](#self-healing-reconciliation) also fills it within
one `reconcileIntervalSeconds`, but backfill is instant.

```json
"opensearch": {
    "enabled": false,
    "addresses": ["http://localhost:9200"],
    "username": "",
    "password": "",
    "index": "hippocampus-memories",
    "queueSize": 1024,
    "reconcileIntervalSeconds": 3600,
    "reconcileBatchSize": 500,
    "applyTimeoutSeconds": 0,
    "applyMaxAttempts": 0,
    "applyRetryBaseBackoffMillis": 0,
    "closeDrainTimeoutSeconds": 0,
    "tls": {
        "caCertFile": "",
        "certFile": "",
        "keyFile": "",
        "insecureSkipVerify": false
    }
}
```

The index worker's retry/drain behaviour is tunable, each defaulting (0) to a built-in value:
`applyTimeoutSeconds` bounds one operation against the cluster (default 10s), `applyMaxAttempts`
caps retries of a transient failure before dropping (default 4), `applyRetryBaseBackoffMillis` is
the wait before the second attempt, doubling with jitter thereafter (default 250ms), and
`closeDrainTimeoutSeconds` bounds how long shutdown waits for the queue to drain (default 5s). Raise
them for a slower cluster where the defaults drop too many operations before the
[reconciliation sweep](#self-healing-reconciliation) heals them.

```sh
curl -X POST http://localhost:8080/v1/memories/search \
    -H 'Content-Type: application/json' \
    -d '{"query": "quarterly forecast", "limit": 5, "reinforce": true}'
```

OpenSearch is strictly a **secondary index** — the primary store remains the system of record
for every existence, consolidation, and recall decision:

- Writes and deletes (including the sleep cycle's consolidation and eviction, purges, event
  merges, and summarisation) propagate to the index asynchronously and best-effort: an
  unreachable or lagging cluster never fails or slows a primary operation. The worker retries a
  transient cluster failure a few times with backoff before giving up, so a brief blip does not
  lose a write. A full propagation queue (`opensearch.queueSize`) still drops operations with a
  warning rather than blocking — but see [Self-healing](#self-healing-reconciliation) below, which
  recovers anything dropped.
- Search results are always re-read from the primary store; ids the index returns that the
  primary no longer holds are silently dropped. A stale index can therefore miss recent writes
  (indexing is asynchronous on top of OpenSearch's ~1s near-real-time refresh) or carry leftover
  documents, but it can never fabricate a result.
- By default finding a memory does not reinforce it — exploratory search must not distort the
  decay model. Setting `reinforce` on the request routes the matches through recall instead,
  resetting their decay clocks and raising their effective significance, exactly as
  `RecallMemories` does.
- Binary memories (`is_binary`) are never indexed; their bodies are opaque.
- With `opensearch.enabled` false (the default) the `sqlite` driver falls back to
  [the store's own index](#the-stores-own-index-sqlite); `postgres` and `mysql` have no content
  search, and `SearchMemories` fails with `FAILED_PRECONDITION`.

The index is fully rebuildable from the primary store (re-storing memories re-indexes them), so
losing it costs search availability, nothing more.
Try it with `docker compose -f deploy/compose/docker-compose.opensearch.yaml up --build` (security disabled —
demo only). Integration tests run against any disposable cluster via
`HIPPOCAMPUS_TEST_OPENSEARCH_URL=http://localhost:9200 go test ./search`.

#### Securing the connection

The bodies of non-binary memories are indexed into OpenSearch as searchable text, so a
content-search deployment holds a **second plaintext copy** of that content outside the primary
store. Treat the cluster as needing the same protection as the primary store, not less.

- **Authentication.** Set `opensearch.username`/`opensearch.password` for a cluster running the
  security plugin. Inject the password as a secret rather than committing it: the environment
  variable `HIPPOCAMPUS_OPENSEARCH_PASSWORD` overrides `opensearch.password` (see
  [Environment variable overrides](#environment-variable-overrides)).
- **Transport encryption.** Give `opensearch.addresses` an `https://` URL. With no `tls` block the
  server certificate is verified against the host's system certificate pool. The optional
  `opensearch.tls` block configures the rest:
  - `caCertFile` — a PEM bundle of certificate authorities to trust for the server certificate, in
    place of the system pool. Set this to trust a cluster serving a certificate signed by a private
    CA, **including OpenSearch's own security plugin, which bootstraps self-signed certificates by
    default**. This is the production-correct alternative to disabling verification.
  - `certFile` / `keyFile` — a client certificate and key for mutual TLS; set both or neither.
  - `insecureSkipVerify` — disables server certificate verification entirely. It logs a startup
    warning and is a **development-only** escape hatch for self-signed certificates; prefer
    `caCertFile` in production, where an unverified connection offers no protection against
    interception.

  A malformed `tls` block (an unreadable or empty CA bundle, a half-configured client certificate
  pair, an unloadable key) fails startup rather than silently downgrading security. The same block
  applies to the `--backfill-search` CLI mode.

For a secured reference stack — security plugin enabled, HTTPS, credentials via the environment —
use `docker compose -f deploy/compose/docker-compose.opensearch-secured.yaml up --build` (it uses
`insecureSkipVerify` only because the demo image's certificates are self-signed; the file's header
comment explains the `caCertFile` swap for production).

#### Self-healing reconciliation

Because propagation is asynchronous, a document can still go missing — an operation dropped under
queue overflow, lost to a crash before the worker drained, or missed while the cluster was
unreachable long enough to exhaust the worker's retries. Rather than leave that gap open until an
operator runs a manual backfill, the **consolidating instance** runs a periodic reconciliation
sweep that re-indexes every non-binary memory from the primary store, keyed by id (idempotent, so
it only ever _adds back_ what was missing). It is controlled by two keys:

- `opensearch.reconcileIntervalSeconds` (default `3600`) — how often a sweep runs; the interval is
  measured from the end of one sweep to the start of the next. `0` (or negative) disables it.
- `opensearch.reconcileBatchSize` (default `500`) — how many memories each page reads; the sweep
  pauses briefly between pages so it trickles into the async index queue rather than flooding it.

The sweep runs only on the instance with `consolidation.enabled: true` (the single owner of index
maintenance), so replicas never duplicate it, and it starts a short while after launch so a sparse
index is healed soon after a restart rather than a whole interval later. It heals only _missing_
documents: a stale document (one the primary store no longer holds) is already harmless — search
results are re-verified against the primary store — and removing stale documents needs a full
enumeration of the index, which remains the job of `--backfill-search --reindex` below.

#### Backfill and reindex

The reconciliation sweep above heals a missing document on its own, but two cases still want an
immediate, synchronous rebuild: enabling `opensearch.enabled` on an existing database (the whole
index is empty and you do not want to wait for a sweep), and clearing _stale_ documents the sweep
deliberately leaves behind. The backfill CLI mode covers both:

```sh
hippocampus --backfill-search -c config.json            # index every non-binary memory
hippocampus --backfill-search --reindex -c config.json  # delete and recreate the index first
```

Like `--mint-token`, this runs and exits without starting the server. It streams every non-binary
memory from the primary store in id-keyed batches (`--backfill-batch-size`, default 500) and
indexes each one synchronously, keyed by id — runs are idempotent (each write overwrites the same
document) and abort on the first error, so a failed run is simply rerun. `--reindex` deletes and
recreates the index before backfilling, which also removes stale documents for memories the
primary store no longer holds; without it, existing documents are left in place and overwritten.

The tool may run alongside a live service instance: SQLite's WAL admits cross-process readers,
and the Postgres open skips the single-instance advisory lock (it only reads). The one caveat to
a live run is that a memory deleted by the service mid-backfill can be re-indexed after its
deletion propagated, leaving a stale document — harmless for reads (results are re-verified
against the primary store) and cleared by the next `--reindex` run.

**Without OpenSearch**, the same flag rebuilds
[the store's own index](#the-stores-own-index-sqlite) instead, and behaves differently in two ways
that matter. It is rarely needed — that index is populated automatically at startup and maintained
inside every write — so reach for it only when a memory is stored but not findable, which means an
index write failed and was logged. And it **writes to the service's own database**, so unlike the
OpenSearch backfill it must not be run beside a live instance: stop the service first. `--reindex`
and `--backfill-batch-size` do not apply; the rebuild always clears and repopulates.

### Summarisation (embedded LLM / Ollama)

By default summaries are authored by the client (`ReplaceMemoriesWithSummary`, see
[Summarisation](consolidation.md#summarisation)). Enabling the optional embedded LLM — an
[Ollama](https://github.com/ollama/ollama) server — lets the service author them itself: the
`SummariseMemories` RPC (`POST /v1/events/{event_id}/summarise`) reads an event's memories,
generates a summary via the model, and replaces them with it; and `ollama.autoSummarise` makes the
sleep cycle do the same for each scan candidate. Off by default; when disabled `SummariseMemories`
returns `FAILED_PRECONDITION` and auto-summarisation is a no-op. The summariser is the one component
that reads memory content, and it sends memory bodies to the Ollama server — see the
[operations security note](operations.md#security).

```json
"ollama": {
    "enabled": false,
    "address": "http://localhost:11434",
    "model": "llama3.2",
    "autoSummarise": false,
    "timeoutSeconds": 120,
    "maxMemories": 200,
    "promptCharLimit": 32000,
    "systemPrompt": "",
    "temperature": 0
}
```

`enabled`/`address`/`model` are the core settings; the rest have sensible defaults (`maxMemories`
and `promptCharLimit` bound the prompt so a large event cannot overrun a small model's context;
`systemPrompt` empty uses a built-in memory-consolidation instruction; `temperature` 0 uses the
model default). Binary memories are excluded from the prompt (their bodies are opaque). Deploy
Ollama with the optional `ollama` compose profile (see `docker-compose.yaml`), with the configured
model pulled (`docker compose exec ollama ollama pull <model>`). For the full behaviour, see
[Summarisation → Embedded LLM (Ollama)](consolidation.md#embedded-llm-ollama).

### Transfer and archive

An embedded (e.g. IoT) instance can periodically move its whole store to a centralised
instance, either directly over gRPC or through S3 when the two are never connected at the same
time. Both paths preserve full state — original timestamps, recall history, significance,
groups, summary flags, event links and relationships — so the centralised store's decay sees
true ages: this is a data migration, not a re-store. Both are move-semantics by manifest: each
run captures a point-in-time snapshot and records exactly what it captured (ids plus recall
state); the paired **clear** then deletes precisely those records, re-verified live, so a memory
written or recalled mid-run survives to the next run. No watermark state is kept — the store
itself is the watermark.

- `Export` (`POST /v1/export`) streams every event and memory into a gzip-compressed archive
  object in S3 and returns a `manifest_id` and the `object_key`; `{"clear": true}` deletes the
  captured records in the same call once the upload has succeeded.
- `Import` (`POST /v1/import`, body `{"object_key": "..."}`) streams an archive from S3 into the
  store, upserting by id — re-importing the same archive is idempotent. Imported non-binary
  memories are indexed into the optional content-search index.
- `Transfer` (`POST /v1/transfer`) sends the same records directly to the centralised instance's
  `ImportBatch` RPC (`transfer.targetAddress`), with the same `clear` flag and manifest.
- `Clear` (`POST /v1/clear`, body `{"manifest_id": "..."}`) is the deferred second half of a
  two-phase move: export/transfer first, verify at the receiving end, then clear. Manifests are
  held in memory only (the last 8) — after a restart the records are simply recaptured by the
  next run.

```json
"s3": {
    "bucket": "my-archive-bucket",
    "region": "ap-southeast-2",
    "endpoint": "",
    "usePathStyle": false,
    "keyPrefix": "hippocampus/"
},
"transfer": {
    "targetAddress": "central.example.com:50051",
    "token": "",
    "tls": {
        "enabled": false,
        "caCertFile": "",
        "certFile": "",
        "keyFile": "",
        "insecureSkipVerify": false
    },
    "batchSize": 500,
    "maxBatchBytes": 0,
    "maxManifestRows": 0
}
```

S3 credentials come from the standard AWS chain (environment variables, shared config, instance
roles); `s3.endpoint` and `s3.usePathStyle` support S3-compatible stores such as MinIO. With no
`s3.bucket` configured, `Export`/`Import` fail with `FAILED_PRECONDITION`; with no
`transfer.targetAddress`, so does `Transfer`. `transfer.token` is sent as the bearer token to
the centralised instance when its [authentication](#authentication) is enabled, and
`transfer.tls.enabled` dials it over TLS. `transfer.batchSize` sets both the pagination page size and
the archive/ImportBatch batch size.

`transfer.tls` accepts the same trust options as [`opensearch.tls`](#securing-the-connection), so a
transfer to a target serving a private-CA or mutual-TLS certificate can verify it:
`caCertFile` is a PEM CA bundle trusted in place of the system pool, `certFile`/`keyFile` are a
client certificate/key pair for mutual TLS (set both or neither), and `insecureSkipVerify` is a
dev-only escape hatch that disables verification. With `enabled: true` and none of the trust options
set, the connection verifies against the system certificate pool. The legacy scalar form
`"tls": true` still works as a shorthand for `{ "enabled": true }`.

`transfer.maxBatchBytes` (0 → an internal 3 MiB default) additionally bounds each `ImportBatch`
message's serialised size during a `Transfer`: a page of large memory bodies is split into
byte-bounded sub-batches so no message overflows the receiver's gRPC max-receive-message size
(otherwise the transfer fails permanently, every retry hitting the same oversized deterministic
page). A single memory larger than the budget is sent alone. If your bodies can exceed the default
4 MiB gRPC frame, raise the receiving instance's `maxRecvMsgBytes` (top-level, 0 → grpc-go's 4 MiB
default) so it accepts them.

`transfer.maxManifestRows` (0 → unlimited, the default) caps how many records a single
`Export`/`Transfer` may capture into its in-memory manifest. Each captured memory holds its id plus
its recall-state snapshot, so an unbounded manifest over a very large store can grow to gigabytes
and OOM the instance. When set, a store larger than the cap is refused with `FAILED_PRECONDITION`
_before_ any upload, and the operator segments the transfer (e.g. by `group`) or raises the cap. The
manifest exists so a later `Clear` can delete exactly what was captured; a deployment that never
uses `Clear` can leave the cap low.

The sleep cycle keeps running during a capture: a record consolidated mid-export simply doesn't
matter (its clear becomes a no-op), and one consolidated at the centralised end after import is
the destination's own decay policy at work.

## Functional
