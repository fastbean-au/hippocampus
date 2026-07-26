# Hosted showcase

A publicly reachable demonstration of Hippocampus — the web console, OpenSearch content search, and
the Grafana/OTEL telemetry stack — with the UI protected by an identity provider. It runs as **two
independent stacks**, each driven by the [`hippocampus-gen`](https://github.com/fastbean-au/hippocampus-gen)
generators:

| Stack | Shape | Generator |
|---|---|---|
| **book** | *Great Expectations* reloaded daily, summarised, decaying | `cmd/book --loop --period 24h --reset --live --pace-window <w> --summarize` |
| **logs** | a continuous log trickle, reaped by consolidation + capacity eviction | `cmd/logs --live --rate <n>` |

The service configs are [`docker/config.showcase-book.json`](../docker/config.showcase-book.json) and
[`docker/config.showcase-logs.json`](../docker/config.showcase-logs.json). This document covers the
identity-provider setup those configs assume; the compose stacks and the GCP deployment are covered
separately.

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
   what makes Auth0 mint a *JWT* access token — without an audience it returns an opaque token that
   cannot be verified.
2. **Applications → Create → Single Page Application** for the console. Add your console URL to
   *Allowed Callback URLs* and *Allowed Web Origins*. PKCE is automatic for SPAs.
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
