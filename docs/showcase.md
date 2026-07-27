# Hosted showcase

A publicly reachable demonstration of Hippocampus — the web console, OpenSearch content search, and
the Grafana/OTEL telemetry stack — with the UI protected by an identity provider. It runs as **two
independent stacks**, each driven by the [`hippocampus-gen`](https://github.com/fastbean-au/hippocampus-gen)
generators:

| Stack    | Shape                                                                 | Generator                                                                   |
| -------- | --------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| **book** | _Great Expectations_ reloaded daily, summarised, decaying             | `cmd/book --loop --period 24h --reset --live --pace-window <w> --summarize` |
| **logs** | a continuous log trickle, reaped by consolidation + capacity eviction | `cmd/logs --live --rate <n>`                                                |

The service configs are [`docker/config.showcase-book.json`](../docker/config.showcase-book.json) and
[`docker/config.showcase-logs.json`](../docker/config.showcase-logs.json); the compose stacks are
[`docker/docker-compose.showcase-book.yaml`](../docker/docker-compose.showcase-book.yaml) and
[`…-logs.yaml`](../docker/docker-compose.showcase-logs.yaml). This document covers the
identity-provider setup and how to run the stacks; the GCP VM provisioning is a separate
[runbook](showcase-gcp.md).

> **Tight on resources?** There is also a **lite** single stack that trades OpenSearch content search
> and the Grafana/OTEL telemetry for a footprint that fits a 0.25 vCPU / 1 GiB VM — see
> [A lite single stack](#a-lite-single-stack-e2-micro) below.

## What the configs assume

Both configs use `auth.method: idp` and a **compressed decay clock**
(`consolidation.unitsOfAgeInDays: 0.002`, ≈ one age-unit per three minutes) so forgetting,
summarisation, and (for logs) capacity eviction all play out within a session rather than over real
days. They differ where the two shapes differ:

- **book** enables summarisation (`summarizationMinMemories: 20`, `summarizationMinAgeInDays: 0`) and
  leaves capacity uncapped — the store is small and purged each day.
- **logs** disables summarisation and caps the store (`capacityBytes`/`capacityMemories`) so eviction
  keeps the ever-growing trickle bounded.

Both enable OpenSearch and ship metrics/traces to `otel-lgtm` by default.

### The one issuer rule

`auth.issuer` (which the **service** uses to discover the JWKS and which it enforces against each
token's `iss`) and `auth.ui.issuer` (which the **browser** runs OIDC discovery against) **must be the
same canonical URL** — the one the identity provider stamps into `iss`. A split-horizon setup (the
browser using a public URL while the service dials an internal one) makes the `iss` claim mismatch
`auth.issuer` and every token is rejected. On a deployed VM this is the single public provider URL
that both the browser and the service containers reach. Override both per deployment:

```sh
HIPPOCAMPUS_AUTH_ISSUER=https://auth.example/realms/hippocampus
HIPPOCAMPUS_AUTH_UI_ISSUER=https://auth.example/realms/hippocampus
```

(All config keys are overridable as `HIPPOCAMPUS_<KEY>` with `.`→`_`.)

## Keycloak (self-hosted)

A ready-to-import realm lives at
[`docker/keycloak/realm-hippocampus.json`](../docker/keycloak/realm-hippocampus.json). It defines:

- realm roles `reader` / `writer` / `admin` (mapped straight onto Hippocampus's tiers);
- a **public SPA client** `hippocampus-console` (Authorization Code + PKCE, no secret) for the `/ui`
  console — set its `redirectUris` to your console URLs (the file ships localhost plus
  `https://book.hippocampus.example/ui` / `https://logs.hippocampus.example/ui` placeholders);
- a **confidential client** `hippocampus-gen` (client-credentials, `serviceAccountsEnabled`) with the
  `admin` role, for the generators — **change its `secret`** before deploying;
- demo users `admin-demo` / `writer-demo` / `reader-demo` (password = the role) so you can sign in and
  see the role-gated console.

Keycloak publishes roles under the nested `realm_access.roles` claim, which is why the configs set
`auth.roleClaim: "realm_access.roles"` (resolved via the dotted-path lookup — see
[Authorization](configuration.md#authorization)). Keycloak's access token has no `aud` for these
clients, so `auth.audience` is left empty (unenforced).

Run it (dev mode, importing the realm):

```sh
podman run -d --name keycloak -p 8092:8080 \
  -e KEYCLOAK_ADMIN=admin -e KEYCLOAK_ADMIN_PASSWORD=admin -e KC_HOSTNAME_STRICT=false \
  -v "$PWD/docker/keycloak/realm-hippocampus.json:/opt/keycloak/data/import/realm.json:ro,Z" \
  quay.io/keycloak/keycloak:26.0 start-dev --import-realm
```

Then point a stack at it (`issuer` = `http://localhost:8092/realms/hippocampus`). A machine token for
the generators:

```sh
curl -s -X POST http://localhost:8092/realms/hippocampus/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=hippocampus-gen \
  -d client_secret=<secret> | jq -r .access_token
```

or let the generator fetch it itself with `--oidc-issuer/--oidc-client-id/--oidc-client-secret` (see
the generator's [Authentication](https://github.com/fastbean-au/hippocampus-gen#authentication)).

## Auth0 (SaaS)

Auth0 is wired through the same `auth.method: idp` path; only the config values differ. In the Auth0
dashboard:

1. **APIs → Create API.** The **Identifier** you choose is the **audience**. Enable RS256. This is
   what makes Auth0 mint a _JWT_ access token — without an audience it returns an opaque token that
   cannot be verified.
2. **Applications → Create → Single Page Application** for the console. Add your console URL to
   _Allowed Callback URLs_ and _Allowed Web Origins_. PKCE is automatic for SPAs.
3. **Applications → Create → Machine to Machine**, authorise it for the API above, for the
   generators (client-credentials). Grant it the permission/role your `admin` tier maps to.
4. **Add roles to the token.** Auth0 does not put roles in a standard claim; add a Login/Client-
   Credentials **Action** that sets a namespaced claim, e.g.
   `api.accessToken.setCustomClaim("https://hippocampus.example/roles", ["admin"])` (or from the
   user's assigned roles). The namespace must be a URI Auth0 won't strip.

Then the config (via env overrides on the showcase config):

```sh
HIPPOCAMPUS_AUTH_ISSUER=https://YOUR_TENANT.us.auth0.com/
HIPPOCAMPUS_AUTH_UI_ISSUER=https://YOUR_TENANT.us.auth0.com/
HIPPOCAMPUS_AUTH_AUDIENCE=https://api.hippocampus.example        # the API Identifier
HIPPOCAMPUS_AUTH_UI_AUDIENCE=https://api.hippocampus.example     # so the browser gets a JWT
HIPPOCAMPUS_AUTH_UI_CLIENTID=<spa-client-id>
HIPPOCAMPUS_AUTH_ROLECLAIM=https://hippocampus.example/roles     # matched literally (top-level)
```

Because the role claim is a URI-shaped **top-level** key, the resolver matches it literally; the
nested dotted-path lookup is what makes Keycloak's `realm_access.roles` work. One resolver, both
providers — see [Authorization](configuration.md#authorization).

The generators authenticate to Auth0 with the same flags plus `--oidc-audience <API Identifier>`.

## Running the stacks

Each stack is a self-contained compose project: hippocampus (Postgres + OpenSearch), a Keycloak IdP,
and the otel-lgtm telemetry stack, all behind **Caddy**, which terminates TLS (automatic Let's
Encrypt) and routes by hostname. The two stacks are independent and run side by side on one host.

### The split-issuer fix

`auth.issuer` must be the one URL both the browser and the service reach (see [the one issuer
rule](#the-one-issuer-rule)). Caddy provides it: it joins the compose network with the public
hostnames (`${DOMAIN}`, `auth.${DOMAIN}`, `grafana.${DOMAIN}`) as **network aliases**, so the
hippocampus container resolves `auth.${DOMAIN}` to Caddy and reaches Keycloak at exactly the URL the
browser uses via public DNS. The compose sets `HIPPOCAMPUS_AUTH_ISSUER`/`_UI_ISSUER` and Keycloak's
`KC_HOSTNAME` to that same `https://auth.${DOMAIN}` value.

> Do **not** substitute a `*.localhost` hostname here: libc and Go resolve the `.localhost` TLD to
> loopback (RFC 6761) and never consult the compose DNS, so the container would dial itself instead
> of Caddy. Use a real domain (below), or `/etc/hosts` aliases for a non-`.localhost` name locally.

### Deploy (public domain)

Point DNS A/AAAA records for `${DOMAIN}`, `auth.${DOMAIN}`, and `grafana.${DOMAIN}` at the VM, open
ports 80 and 443, then before first run **change the two demo secrets**: Keycloak's admin password
and the `hippocampus-gen` client `secret` in
[`docker/keycloak/realm-hippocampus.json`](../docker/keycloak/realm-hippocampus.json) (the realm's
console `redirectUris` already list `https://book.hippocampus.example/ui` /
`https://logs.hippocampus.example/ui` — change these to your domains too).

```sh
BOOK_DOMAIN=book.example ACME_EMAIL=you@example.com \
  docker compose -f docker/docker-compose.showcase-book.yaml up --build -d

LOGS_DOMAIN=logs.example ACME_EMAIL=you@example.com \
  docker compose -f docker/docker-compose.showcase-logs.yaml up --build -d
```

Sign in to `https://book.example/ui` as `admin-demo` / `writer-demo` / `reader-demo` (password = the
role) and watch the console adapt to the tier.

### Drive it with the generators

The generators run as **host processes** (they are a separate Go module whose private dependency a
clean image build can't fetch without credentials — so they are not compose services). Build them
from the sibling [`hippocampus-gen`](https://github.com/fastbean-au/hippocampus-gen) checkout and
point them at the published gRPC port, authenticating to Keycloak as the `hippocampus-gen` client
(admin tier — the book path calls `Purge`/`Sleep`):

```sh
# book: reload + summarise every 24h, spread across 2h, ageing live
go run ./cmd/book -s <vm>:50051 \
  --loop --period 24h --reset --pace-window 2h --live --summarize \
  --oidc-issuer https://auth.book.example/realms/hippocampus \
  --oidc-client-id hippocampus-gen --oidc-client-secret "$GEN_SECRET"

# logs: a steady trickle the sleep cycle keeps reaping
go run ./cmd/logs -s <vm>:50052 --live --rate 120 \
  --oidc-issuer https://auth.logs.example/realms/hippocampus \
  --oidc-client-id hippocampus-gen --oidc-client-secret "$GEN_SECRET"
```

Running these unattended (a systemd unit per stack) is covered in the [GCP deployment
runbook](showcase-gcp.md).

### A lite single stack (e2-micro)

The book/logs stacks each run Postgres + OpenSearch + Keycloak + otel-lgtm behind Caddy — together
they want ~10 GiB of RAM. When that is too much (a single tiny VM, a throwaway demo), the **lite
stack** [`docker/docker-compose.showcase-lite.yaml`](../docker/docker-compose.showcase-lite.yaml)
(config [`docker/config.showcase-lite.json`](../docker/config.showcase-lite.json)) strips it to two
containers — hippocampus on **SQLite** plus Caddy — and moves auth to **hosted [Auth0](#auth0-saas)**,
so there is no JVM on the box. It fits a **0.25 vCPU / 1 GiB** machine (~500 MiB in use; the quarter
core, not RAM, is the limit). The trade-off is the two heavy sidecars: **no content-search tab**
(OpenSearch) and **no Grafana dashboards** (telemetry).

Because Auth0's issuer is a single public URL the browser and the container both reach, the lite stack
needs neither the [split-issuer](#the-split-issuer-fix) Caddy-alias trick nor an `auth.` subdomain —
just one A record for the console. Bring it up with your Auth0 tenant details:

```sh
LITE_DOMAIN=demo.example ACME_EMAIL=you@example.com \
  AUTH0_DOMAIN=your-tenant.us.auth0.com \
  AUTH0_AUDIENCE=https://hippocampus.api \
  AUTH0_CLIENT_ID=<console SPA client id> \
  AUTH0_ROLES_CLAIM=https://hippocampus.example/roles \
  docker compose -f docker/docker-compose.showcase-lite.yaml up --build -d
```

The full walkthrough — Auth0 setup, the machine-to-machine generator, and the systemd unit — is the
[lite section of the GCP runbook](showcase-gcp.md#a-lite-stack-for-an-e2-micro).

### Local evaluation without a public domain

Automatic HTTPS needs a real domain, so the full Caddy stack does not come up as-is on a laptop. To
try the pieces locally, run Keycloak directly (the [Keycloak](#keycloak-self-hosted) section's
`podman run`) and a SQLite instance with `auth.method: idp` pointed at
`http://localhost:8092/realms/hippocampus` — the arrangement the idp round-trip was verified against.
The `docker/docker-compose.corporate.yaml` stack remains the quick, unauthenticated local demo.
