# Security

![Hippocampus](go-hippocampus.png)

Hippocampus ships with **authentication, TLS, and rate limiting all off**. That is deliberate — the
default install is an embedded store on `localhost` with no dependencies, and a first run should not
require minting a token. It also means that **every deployment reachable beyond localhost needs a
deliberate pass over this page**, because nothing here turns itself on.

This is the security guide: what protects what, what is off until you enable it, where memory content
can leave the process, and what the service deliberately does not do. The exhaustive key reference is
[Configurability](configuration.md); the operational context is the
[Operations guide](operations.md).

## The defaults, and what to turn on

| Control                | Key                                                       | Default    | Turn it on when                                                    |
| :--------------------- | :-------------------------------------------------------- | :--------- | :------------------------------------------------------------------ |
| Authentication         | [`auth.method`](configuration.md#authentication)          | `none`     | Anything reachable beyond localhost.                                |
| Authorisation (tiers)  | [`auth.roleMapping`](configuration.md#authorisation)      | —          | Automatically, once auth is on: it is **default-closed**.           |
| Group scoping          | [`auth.requireGroupScope`](configuration.md#group-scoping) | off        | Several teams or systems share one store.                           |
| TLS                    | [`tls.enabled`](configuration.md#tls)                     | off        | Always, unless a proxy or mesh terminates it for you.               |
| Rate limiting          | [`rateLimit.enabled`](configuration.md#rate-limiting)     | off        | Any caller you do not control. Set at least a global ceiling.       |
| Gateway body cap       | `gateway.maxRequestBytes`                                 | unset      | The HTTP gateway is reachable by untrusted callers.                 |
| gRPC stream/keepalive  | `maxConcurrentStreams`, `keepalive.*`                     | grpc-go's  | The gRPC port is exposed beyond trusted callers.                    |
| Listener binding       | `bindAddress`, `gateway.bindAddress`                      | all        | A sidecar or mesh fronts the service — bind loopback only.          |

Authentication and authorisation are one decision, not two: the authoriser is built only when auth is
enabled, and a token whose roles resolve to no tier is denied every RPC.

## Authentication

`auth.method` selects the scheme:

- **`hmac`** — HS256 against a shared secret. Tokens are minted by the service's own CLI
  (`--mint-token --client-id … --role … --ttl …`), so there is no issuance endpoint to attack.
  Use a long random `auth.signingSecret` — at least 32 bytes; a shorter one is brute-forceable and
  the service warns at startup.
- **`idp`** — RS256 against an identity provider's JWKS, discovered from `auth.jwksUrl` or by OIDC
  discovery from `auth.issuer`, with `iss`/`aud` enforced when configured. Keys are cached by `kid`
  and re-fetched on rotation. `--mint-token` refuses under `idp`, because the provider issues tokens.

Both verifiers pin a single algorithm, so a token can never select its own, and both **require an
`exp`** — an expiry-less token would otherwise verify forever.

**Rotation and revocation.** Under `hmac`, signing secrets rotate without a flag day via
`auth.signingKeys` (several `kid`-tagged secrets trusted at once, `auth.activeKid` selecting the one
that signs) — see [Key rotation](configuration.md#key-rotation-hmac). Individual tokens or whole
clients are revoked ahead of their TTL by a polled `auth.revocationFile`, by `jti`, by `client_id`,
or per-client before a cutoff timestamp — see [Revocation](configuration.md#revocation). The
revocation file applies under `idp` too, as a local override for when the provider's own revocation
lags.

The verified `client_id` is logged on every failing request (and, on the HTTP gateway, every
request), so a leaked or misbehaving token can be traced back to the client it was issued to.

## Authorisation: role tiers

Every RPC requires a minimum tier — `reader`, `writer`, or `admin`, nesting as
`reader ⊂ writer ⊂ admin` — carried in the token's `roles` claim and enforced identically on gRPC and
the HTTP gateway from one policy table. See [Authorisation](configuration.md#authorisation).

- Issue **`reader`** to read-only consumers. Whether a reader's recall actually reinforces is itself
  configurable (`auth.readerRecallReinforces`), so a dashboard polling the store need not keep
  resetting decay clocks.
- **`writer`** covers stores, updates, deletes and summaries — and `Import`/`ImportBatch`, which
  deliberately bypass write-path validation so an archive restores faithfully. Grant it only to
  loaders you trust.
- Reserve **`admin`**: it alone may `Purge`, `Sleep`, `Clear`, `Transfer`, `Export`, and preview a
  consolidation.

Authorisation is **default-closed**, so on upgrade any pre-existing token must be re-minted with a
`--role` or it will be denied everything.

## Group scoping and the trust boundary

**A shared store is a shared trust domain by default.** Every token that can read can read
everything, and `group` is a label — despite reading like ownership ("system, subsystem, owner"), it
carries no access-control meaning of its own. Do not stand up one store for two parties on the
assumption that `group` separates them.

There are two ways to actually separate them, and they are not interchangeable.

**Soft partitioning — [group scoping](configuration.md#group-scoping).** Bind tokens to group labels
with a `groups` claim. Each team or system then sees, writes, exports and links only its own records,
and the console, CLI and `WhoAmI` report the scope. `auth.requireGroupScope` refuses a token that
carries no scope at all — an unscoped token being the _most_ privileged shape there is, not the
least. This is the right answer for teams or systems **inside one trust boundary**, where the goal is
that people not trip over each other's data rather than that they be unable to reach it.

What it does **not** give you, because the partition is soft:

- **The decay dynamics are shared.** Capacity pressure, the eviction ranking, the significance
  registry and the sleep cadence are store-global. A group writing heavily raises the pressure that
  decides what _every_ group forgets, and eviction ranks across the whole store by value. There is no
  per-group capacity target and no per-group quota.
- **`link_significance` crosses the boundary.** It is a denormalised aggregate in the covering index,
  so a link to a record in another group raises this one's effective significance even though the
  other end can never be read. Spreading activation on recall crosses such a link too. Only an
  unscoped token can create one.
- **It is not a defence against a hostile caller with a valid token.** It is one predicate and one id
  check per path, guarded by a test rather than by a structural impossibility.
- **Operators still need unscoped tokens** for `Purge`, `Sleep`, `PreviewConsolidation`, the
  `MergeEvents` dangling-reference heal, and the `--backfill-search` CLI mode.

**Hard isolation — one instance per tenant.** Where bleed-through is unacceptable, or a tenant needs
its own capacity and decay tuning, run a separate instance and a separate store. It isolates the
memory dynamics perfectly, gives each tenant its own auth secrets, backup and restore, and makes
erasure a matter of dropping a volume. With the embedded SQLite driver that is one container and one
volume per tenant; on `postgres`/`mysql` it is one database per tenant.

The two compose: a fleet of single-tenant embedded instances can transfer into one centralised
multi-group store, with the centralised side stamping each ingestor's group from its
[`transfer.token`](configuration.md#transfer-and-archive) rather than from anything in the archive.

## Transport

When `tls.enabled`, both listeners share one certificate and enforce a **TLS 1.2 minimum**.

If auth is enabled without TLS the service only warns — it assumes TLS is terminated upstream by a
proxy or service mesh. **Never send bearer tokens in plaintext.** Behind such a sidecar, bind the
listeners to loopback only with `bindAddress`/`gateway.bindAddress` (`127.0.0.1`) so nothing reaches
them except the local proxy.

The same applies to the **Transfer client**: setting `transfer.token` without `transfer.tls` sends
the token in plaintext to the target, and the service warns at startup.

## Rate limiting and transport hardening

[Rate limiting](configuration.md#rate-limiting) (`rateLimit.enabled`, off by default) admits requests
through a hierarchy of token buckets — a **global** ceiling (the denial-of-service bound), a
per-**tier** allowance, and a per-**client** share so one caller cannot starve the rest. Set at least
the global ceiling on anything reachable beyond trusted callers.

If the gRPC port is exposed, cap the concurrent HTTP/2 streams one connection may open with
`maxConcurrentStreams`, and enforce a keepalive policy (`keepalive.minTimeSeconds`,
`keepalive.permitWithoutStream`) so an abusive client cannot ping the server into a resource spiral.
Both default to grpc-go's own defaults.

**Body-size limits on an exposed gateway.** `memory.limit.sizeBytes` caps a memory body; left unset
there is no cap. The native gRPC transport bounds a whole request at its 4 MiB default, but the HTTP
gateway does not by default — set `gateway.maxRequestBytes` when the gateway is reachable by
untrusted callers, keeping the ceiling above your largest legitimate `ImportBatch`/`Transfer` body.

## The web console (`/ui`)

The HTTP gateway serves an embedded single-page console at `/ui`. The static page loads without a
token, but when the deployment authenticates it opens on a **sign-in card in place of the console** —
a bearer-token box under `hmac`, a **Sign in** button under `idp` — and reveals the tabs only once a
session resolves. A pasted token is kept in the browser's `localStorage` and sent with each `/v1`
call; **Sign out** discards it (or ends the provider session).

**None of that is the security boundary.** Every action still goes through authentication,
[authorisation](configuration.md#authorisation), and the purge gate like any other request. The
console calls `GET /v1/whoami` and adapts what it offers to the effective role — hiding write
controls for a `reader` — but a hidden control is a convenience, not a boundary; the server enforces
the tier on every RPC.

Because the token lives in the browser, serve `/ui` **only over TLS**, treat it as a trusted-operator
tool rather than a public endpoint, and put it behind your ingress' access controls if the gateway is
internet-facing.

## Where memory content can leave the process

The service is deliberately blind to memory bodies — it never reads one during consolidation, and
the covering index exists partly so the decay scans cannot. Four features are the exceptions, and
each is a decision to let content out:

- **The embedded LLM summariser** (`ollama.enabled`, off by default) is the one component that reads
  memory content, and it sends the text bodies of an event's memories to the configured Ollama
  server. Run Ollama on the same host or a private network (`http://localhost:11434`), not a shared
  or third-party endpoint, and reach it over TLS if it is remote. `ollama.autoSummarise` rewrites
  stored memories automatically during sleep, so leave it off unless that is intended. See
  [Embedded LLM (Ollama)](consolidation.md#embedded-llm-ollama).
- **The OpenSearch index holds a copy of every indexed body**, so the cluster is a second store of
  the same data and needs the same access control — authentication, TLS
  ([`opensearch.tls`](configuration.md#content-search)), and network isolation. The built-in SQLite
  FTS5 backend does not: it is a **contentless** index, holding the inverted index and not a second
  copy of the text. Choosing the embedded backend is therefore one fewer place your data lives.
- **Export, Transfer and the object store** write full bodies into an archive. Anyone who can read an
  archive can read the memories in it; S3 credentials come from the standard AWS chain, and
  `Transfer` sends the archive to another instance under `transfer.token`.
- **The forgotten log** records ids, groups, sizes, values and thresholds — deliberately **never
  bodies** — so it is safe to keep after the memory itself is gone.

Two things that do _not_ leak content: **storage errors never reach a client verbatim** (the
`mapError` seam translates what a client can act on and masks the rest as `codes.Internal`, keeping
raw driver text and the schema it would describe out of responses), and the **deployment topology
view** redacts every address it reports — DSN credentials in all three dialect forms, cluster
passwords, signing material — which is what makes its default `reader` tier defensible. See
[Seeing the deployment](operations.md#seeing-the-deployment).

## Secrets

Any config key can be supplied as an environment variable (`HIPPOCAMPUS_<KEY>` with dots replaced by
underscores), and **that is the recommended way to supply secrets** — inject them as Docker or
Kubernetes secrets rather than committing them to `config.json`. The ones that matter:

| Secret                    | Env override                       |
| :------------------------ | :--------------------------------- |
| HMAC signing secret       | `HIPPOCAMPUS_AUTH_SIGNINGSECRET`   |
| Database DSN (with its password) | `HIPPOCAMPUS_STORAGE_POSTGRES_DSN` / `..._MYSQL_DSN` |
| OpenSearch password       | `HIPPOCAMPUS_OPENSEARCH_PASSWORD`  |
| Transfer token            | `HIPPOCAMPUS_TRANSFER_TOKEN`       |
| OAuth2 client secret      | `HIPPOCAMPUS_AUTH_OAUTH2_CLIENTSECRET` |

`auth.signingKeys` is a structured list and so is config-file-only — it cannot be injected through a
single environment variable, so a deployment rotating keys needs a mounted config file.

## What the service does not do

Stated plainly, because each is a thing an operator might otherwise assume:

- **It does not encrypt data at rest.** Bodies are compressed, not encrypted. Use filesystem or
  volume encryption, or the database's own encryption, and treat a SQLite file or a `pg_dump` as
  plaintext.
- **It has no per-record ACLs.** Access is tier plus group scope; there is nothing finer.
- **There is no server-side mutual TLS.** The listeners present a certificate; they do not verify
  client certificates. Client identity is the bearer token. (Outbound mTLS _is_ supported — to
  OpenSearch and to a transfer target.)
- **There is no separate audit log.** The request log carries the verified `client_id` and the RPC,
  which is the audit trail; ship it somewhere durable if you need one. For a record of what the
  service _forgot_, the [forgotten log](operations.md#what-was-forgotten--the-forgotten-log) is the
  purpose-built answer.
- **It registers no gRPC reflection service**, so a client needs the proto or the OpenAPI document
  rather than discovering the schema from a running instance.
- **A `client_id` is never used as a metric attribute** — it arrives in a token, so it is unbounded
  cardinality and, in the wrong dashboard, an inventory of your callers. It appears in logs and in
  the topology view's observed callers (capped at 32, least-recently-seen evicted), and nowhere else.

## Container and Kubernetes posture

The published image is statically linked with CGO disabled and **runs as non-root**. The Kubernetes
overlays run pods **non-root with a read-only root filesystem**, take secrets (DSN, signing key) as
`HIPPOCAMPUS_*` env overrides rather than baking them into the ConfigMap, and use a token-less
ServiceAccount. See [Containers and Kubernetes](operations.md#containers-and-kubernetes).

The Compose stacks under `deploy/compose/` are **demonstration stacks**: several run OpenSearch with
its security plugin disabled and no authentication at all. `docker-compose.opensearch-secured.yaml`
is the one that shows the secured shape. Do not lift a demo stack into production unmodified.

## A hardening pass

For anything beyond localhost, in the order they matter:

1. **`auth.method`** to `hmac` or `idp`, and re-mint every token with a `--role`.
2. **`tls.enabled`**, or terminate TLS upstream and bind both listeners to `127.0.0.1`.
3. **Role tiers per client** — `reader` for consumers, `admin` only for operators.
4. **`rateLimit.enabled`** with at least a global ceiling.
5. **`gateway.maxRequestBytes`** and `memory.limit.sizeBytes` if the gateway is exposed.
6. **`maxConcurrentStreams` and a keepalive policy** if gRPC is exposed.
7. **Group scoping** (with `auth.requireGroupScope`) if several parties share the store — having read
   [the trust boundary](#group-scoping-and-the-trust-boundary) above.
8. **Secrets as environment variables**, not in `config.json`.
9. **Volume or database encryption**, since the service provides none.
10. **`/ui` behind your ingress' access controls**, or not exposed at all.

## Reporting a vulnerability

Privately, through GitHub's private vulnerability reporting on the repository — never a public
issue. What to include, what is in scope, and which documented behaviours are not vulnerabilities
are in **[SECURITY.md](../SECURITY.md)**.
