# Deploying the showcase to a GCP VM

A runbook for standing up the two [hosted showcase](showcase.md) stacks (book and logs) on a single
Google Compute Engine VM: the server side runs as Docker Compose, the two data generators run as
systemd services, and Caddy provisions TLS automatically. Read [Hosted showcase](showcase.md) first —
this only covers the VM.

> **Two ways to run this.** The runbook below (steps 1–7) is the full stack and needs a beefy VM
> (`e2-standard-4`+). If you only want _one_ showcase and can live without the content-search tab and
> the Grafana dashboards, jump to [A lite stack for an e2-micro](#a-lite-stack-for-an-e2-micro): the
> same console and Auth0 sign-in, trimmed to fit a 0.25 vCPU / 1 GiB machine.

> **One domain instead of two?** Steps 1–7 stand up the two stacks on two domains, each with its own
> Caddy and Keycloak. To serve **both examples under one parent domain** — one shared Caddy, Keycloak,
> and Grafana (slightly lighter, and no `:80`/`:443` contention between two Caddys) — use the merged
> stack instead. Two changes to this runbook: in [step 2](#2-dns) create **four** subdomains of one
> domain (`book.`, `logs.`, `auth.`, `grafana.`) or a single wildcard rather than the six records
> below; and in [step 5](#5-bring-up-the-stacks) run one command:
>
> ```sh
> BASE_DOMAIN=hippocampus.example ACME_EMAIL=you@example.com \
>   docker compose -f docker/docker-compose.showcase-combined.yaml up --build -d
> ```
>
> The generators (step 6) then both authenticate to the one shared issuer
> `https://auth.${BASE_DOMAIN}/realms/hippocampus`. See
> [Both examples on one domain](showcase.md#both-examples-on-one-domain-a-single-merged-stack).

## 1. Sizing

Each stack runs Postgres + OpenSearch (1 GiB heap) + Keycloak (JVM) + an otel-lgtm bundle
(Grafana/Prometheus/Tempo/Loki) + hippocampus + Caddy, and there are two of them. Budget ~10 GiB of
RAM in use.

|              | Recommendation                                                         |
| ------------ | ---------------------------------------------------------------------- |
| Machine type | `e2-standard-4` (4 vCPU / 16 GiB) minimum; `e2-standard-8` comfortable |
| Boot disk    | 50 GiB `pd-ssd` (OpenSearch + telemetry retention)                     |
| Image        | Ubuntu 24.04 LTS (simple Docker + Go install)                          |
| Region       | anywhere close to your viewers                                         |

## 2. DNS

Pick two domains, one per stack (e.g. `book.example` and `logs.example`). Each needs three A records
— the apex/console, `auth.`, and `grafana.` — all pointing at the VM's **external IP**. Six records
total:

```
book.example            A   <VM_IP>
auth.book.example       A   <VM_IP>
grafana.book.example    A   <VM_IP>
logs.example            A   <VM_IP>
auth.logs.example       A   <VM_IP>
grafana.logs.example    A   <VM_IP>
```

These **must resolve before first boot** of the stacks — Caddy's Let's Encrypt challenge fails
otherwise. (Reserve a static external IP so it survives a VM restart.)

## 3. Create the VM and firewall

```sh
gcloud compute instances create hippocampus-showcase \
  --machine-type=e2-standard-4 --boot-disk-size=50GB --boot-disk-type=pd-ssd \
  --image-family=ubuntu-2404-lts --image-project=ubuntu-os-cloud \
  --tags=hippocampus-showcase --address=<RESERVED_STATIC_IP>

# Only 80/443 are exposed. The gRPC ports (50051/50052) stay VM-local: the generators run on the
# VM and dial localhost, so there is no reason to open them to the internet.
gcloud compute firewall-rules create hippocampus-showcase-web \
  --allow=tcp:80,tcp:443 --target-tags=hippocampus-showcase --direction=INGRESS
```

(SSH is covered by GCP's default rule / IAP.)

## 4. Install Docker and Go

```sh
sudo apt-get update
sudo apt-get install -y docker.io docker-compose-v2 golang-go git
sudo usermod -aG docker "$USER"   # log out/in for this to take effect
```

## 5. Bring up the stacks

```sh
git clone https://github.com/fastbean-au/hippocampus.git
cd hippocampus
```

**Before first run, change the demo secrets** (see [showcase.md](showcase.md#keycloak-self-hosted)):
the `hippocampus-gen` client `secret` and the console `redirectUris` in
`docker/keycloak/realm-hippocampus.json`, and optionally the Keycloak admin / Postgres passwords in
the compose files. Then:

```sh
BOOK_DOMAIN=book.example ACME_EMAIL=you@example.com \
  docker compose -f docker/docker-compose.showcase-book.yaml up --build -d

LOGS_DOMAIN=logs.example ACME_EMAIL=you@example.com \
  docker compose -f docker/docker-compose.showcase-logs.yaml up --build -d
```

Watch the certificates arrive (`docker compose ... logs -f caddy`), then browse to
`https://book.example/ui` and sign in as `admin-demo` / `writer-demo` / `reader-demo`.

## 6. Run the generators as systemd services

The generators are a separate module ([`hippocampus-gen`](https://github.com/fastbean-au/hippocampus-gen));
they run as host processes against the VM-local gRPC ports. That module depends on the private
`hippocampus` module, so building on the VM needs Git credentials — set `GOPRIVATE` and provide a
token (a read-only deploy token is enough):

```sh
git clone https://github.com/fastbean-au/hippocampus-gen.git
cd hippocampus-gen
export GOPRIVATE=github.com/fastbean-au/*
git config --global url."https://<TOKEN>@github.com/".insteadOf "https://github.com/"
go build -o /usr/local/bin/hippocampus-gen-book ./cmd/book
go build -o /usr/local/bin/hippocampus-gen-logs ./cmd/logs
```

(Alternatively build the two binaries on a machine that already has access and `scp` them over — no
toolchain or credentials on the VM.)

Put the shared generator client secret in a root-only env file:

```sh
sudo install -d /etc/hippocampus-gen
echo "GEN_SECRET=<the hippocampus-gen client secret>" | sudo tee /etc/hippocampus-gen/showcase.env
sudo chmod 600 /etc/hippocampus-gen/showcase.env
```

`/etc/systemd/system/hippocampus-gen-book.service` — reloads and summarises the book daily:

```ini
[Unit]
Description=Hippocampus book showcase generator
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/hippocampus-gen/showcase.env
ExecStart=/usr/local/bin/hippocampus-gen-book -s localhost:50051 \
  --loop --period 24h --reset --pace-window 2h --live --summarise \
  --oidc-issuer https://auth.book.example/realms/hippocampus \
  --oidc-client-id hippocampus-gen --oidc-client-secret ${GEN_SECRET}
Restart=always
RestartSec=30
DynamicUser=yes

[Install]
WantedBy=multi-user.target
```

`/etc/systemd/system/hippocampus-gen-logs.service` — a steady trickle:

```ini
[Unit]
Description=Hippocampus logs showcase generator
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/hippocampus-gen/showcase.env
ExecStart=/usr/local/bin/hippocampus-gen-logs -s localhost:50052 --live --rate 120 \
  --oidc-issuer https://auth.logs.example/realms/hippocampus \
  --oidc-client-id hippocampus-gen --oidc-client-secret ${GEN_SECRET}
Restart=always
RestartSec=30
DynamicUser=yes

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now hippocampus-gen-book hippocampus-gen-logs
journalctl -u hippocampus-gen-book -f
```

> The client secret appears in the process command line (visible to `ps` on the VM). That is
> acceptable for a throwaway showcase; for anything more, teach the generator to read the secret from
> the environment instead of a flag.

## 7. Operate

- **Restart a stack:** `docker compose -f docker/docker-compose.showcase-book.yaml restart`.
- **Update:** `git pull`, then `docker compose ... up --build -d`, and rebuild the generator binaries.
  Keycloak keeps its realm (named volume); the book store is purged each cycle anyway.
- **Reset everything:** `docker compose ... down -v` drops the named volumes (Postgres, OpenSearch,
  Keycloak, Caddy certs) for a clean slate — the realm re-imports on next start.
- **Certificates** live in the `*-caddy-data` volume and renew automatically; keep 80/443 reachable.
- **Cost:** shut the VM down when not demoing (`gcloud compute instances stop hippocampus-showcase`);
  the static IP and disk persist.

## A lite stack for an e2-micro

The full stack above runs Postgres + OpenSearch + Keycloak + otel-lgtm + hippocampus + Caddy, twice —
budget ~10 GiB of RAM. The **lite stack** (`docker/docker-compose.showcase-lite.yaml`) is the same
console trimmed down to fit a single **`e2-micro` (0.25 vCPU / 1 GiB)**: it drops Postgres (SQLite
instead), OpenSearch (no content-search tab), and otel-lgtm (no Grafana dashboards), and it replaces
self-hosted Keycloak with **hosted Auth0** — so there is no JVM on the box at all. What remains is two
small containers (hippocampus on SQLite, and Caddy), plus the generator as a host process.

RAM is comfortable (~500 MiB in use); the **0.25 vCPU is the real limit**, so run the generator gently
(low pace, one loop) and treat this as a trickle demo, not a soak test.

|              | Recommendation                               |
| ------------ | -------------------------------------------- |
| Machine type | `e2-micro` (0.25 vCPU / 1 GiB)               |
| Boot disk    | 20 GiB `pd-standard` (SQLite + image layers) |
| Image        | Ubuntu 24.04 LTS                             |
| Auth         | An Auth0 tenant (free tier is fine)          |

The phases below are ordered so a mistake surfaces at the next **Checkpoint** rather than three steps
later. Two ordering rules matter: **DNS must resolve before you start the stack** (Caddy's Let's
Encrypt challenge fails otherwise), and **Auth0's callback URL must match your final domain** — so
Auth0 and DNS come first, before anything boots.

### Phase 0 — Gather your values

Fill these in once; every command references them. You supply a domain, a GCP project, and an Auth0
tenant.

| Placeholder                    | What it is                             | Example                             |
| ------------------------------ | -------------------------------------- | ----------------------------------- |
| `DOMAIN`                       | Public hostname for the console        | `demo.example.com`                  |
| `ACME_EMAIL`                   | Email for Let's Encrypt                | `you@example.com`                   |
| `GCP_PROJECT`                  | Your GCP project id                    | `my-project`                        |
| `GCP_ZONE`                     | Zone to run in                         | `us-central1-a`                     |
| `AUTH0_DOMAIN`                 | Your Auth0 tenant domain               | `your-tenant.us.auth0.com`          |
| `AUTH0_AUDIENCE`               | Auth0 API identifier (you choose it)   | `https://hippocampus.api`           |
| `AUTH0_ROLES_CLAIM`            | Namespaced roles claim (you choose it) | `https://hippocampus.example/roles` |
| `AUTH0_CLIENT_ID`              | SPA app client id (from Phase 1)       | _filled in Phase 1_                 |
| `GEN_CLIENT_ID` / `GEN_SECRET` | M2M app id + secret (from Phase 1)     | _filled in Phase 1_                 |

> `GCP_ZONE`'s region (the zone minus its trailing letter, e.g. `us-central1`) is used for the static
> IP below — keep them consistent.

### Phase 1 — Auth0 (browser, ~10 min)

In the [Auth0 dashboard](https://manage.auth0.com):

1. **Create the API.** Applications → APIs → **Create API**. Set **Identifier** = `AUTH0_AUDIENCE`;
   Signing Algorithm **RS256**. The audience is what makes Auth0 mint a verifiable _JWT_ rather than an
   opaque token — both the console and the generator must request it.
2. **Create the roles.** User Management → Roles → create `reader`, `writer`, `admin`, and assign them
   to your test users. Naming them exactly after the service's tiers lets `roleMapping` stay empty.
3. **Add the roles-claim Action.** Actions → Library → **Build from scratch** (trigger _Login / Post
   Login_), deploy it, then drag it into the **Login** flow. Add a second one on the **Machine to
   Machine** flow so the generator's token also carries a role. It stamps a **namespaced** claim
   (Auth0 rejects non-namespaced custom claims):

   ```js
   // Login flow:
   exports.onExecutePostLogin = async (event, api) => {
     const claim = "https://hippocampus.example/roles"; // == AUTH0_ROLES_CLAIM
     api.accessToken.setCustomClaim(claim, event.authorization?.roles ?? []);
   };
   // Machine to Machine flow — a fixed role for the generator's client:
   exports.onExecuteCredentialsExchange = async (event, api) => {
     const claim = "https://hippocampus.example/roles";
     api.accessToken.setCustomClaim(claim, ["writer"]);
   };
   ```

   That URI is what `AUTH0_ROLES_CLAIM` / `auth.roleClaim` must match. (The service reads a claim key
   literally, so the dots and slashes are fine — it is not treated as a nested path.)

4. **Console app (SPA).** Applications → **Create Application** → _Single Page Web Application_. Set
   _Allowed Callback URLs_ to `https://DOMAIN/ui`, and _Allowed Web Origins_ + _Allowed Logout URLs_
   to `https://DOMAIN`. Copy its **Client ID** → `AUTH0_CLIENT_ID`.
5. **Generator app (M2M).** Applications → **Create Application** → _Machine to Machine_, authorised
   for the API from step 1. Copy its **Client ID** + **Client Secret** → `GEN_CLIENT_ID` / `GEN_SECRET`.

> **Checkpoint:** you now hold `AUTH0_DOMAIN`, `AUTH0_AUDIENCE`, `AUTH0_ROLES_CLAIM`,
> `AUTH0_CLIENT_ID`, `GEN_CLIENT_ID`, and `GEN_SECRET`.

### Phase 2 — Static IP and DNS

Reserve an IP so it survives restarts, then point your domain at it. No `auth.` or `grafana.`
subdomains — Auth0 is the issuer and there is no Grafana.

```sh
gcloud compute addresses create hippocampus-lite-ip \
  --project=GCP_PROJECT --region=us-central1     # region = GCP_ZONE minus its trailing letter

gcloud compute addresses describe hippocampus-lite-ip \
  --project=GCP_PROJECT --region=us-central1 --format='value(address)'
```

Create **one A record** at your DNS provider: `DOMAIN` → that IP.

> **Checkpoint:** `dig +short DOMAIN` returns the reserved IP. **Do not proceed to Phase 5 until it
> does** — the ACME challenge needs it.

### Phase 3 — Create the VM and firewall

```sh
gcloud compute instances create hippocampus-lite \
  --project=GCP_PROJECT --zone=GCP_ZONE \
  --machine-type=e2-micro --boot-disk-size=20GB --boot-disk-type=pd-standard \
  --image-family=ubuntu-2404-lts --image-project=ubuntu-os-cloud \
  --tags=hippocampus-lite \
  --address=$(gcloud compute addresses describe hippocampus-lite-ip \
      --project=GCP_PROJECT --region=us-central1 --format='value(address)')

gcloud compute firewall-rules create hippocampus-lite-web \
  --project=GCP_PROJECT --allow=tcp:80,tcp:443 \
  --target-tags=hippocampus-lite --direction=INGRESS
```

Only 80/443 face the internet; gRPC stays VM-local. SSH in:

```sh
gcloud compute ssh hippocampus-lite --project=GCP_PROJECT --zone=GCP_ZONE
```

### Phase 4 — Install Docker (on the VM)

```sh
sudo apt-get update && sudo apt-get install -y docker.io docker-compose-v2 git
sudo usermod -aG docker "$USER"
```

Log out and back in (`exit`, then `gcloud compute ssh …` again) for the group to take effect.

> **Do not install Go on the box** — the compiler can OOM a 1 GiB machine. The generator is
> cross-compiled elsewhere in Phase 6.

### Phase 5 — Bring up the lite stack (on the VM)

```sh
git clone https://github.com/fastbean-au/hippocampus.git && cd hippocampus

LITE_DOMAIN=DOMAIN ACME_EMAIL=ACME_EMAIL \
  AUTH0_DOMAIN=AUTH0_DOMAIN \
  AUTH0_AUDIENCE=AUTH0_AUDIENCE \
  AUTH0_CLIENT_ID=AUTH0_CLIENT_ID \
  AUTH0_ROLES_CLAIM=AUTH0_ROLES_CLAIM \
  docker compose -f docker/docker-compose.showcase-lite.yaml up --build -d
```

The first build compiles the Go image and is slow (several minutes) on a quarter-core — that is
one-time. Watch Caddy obtain the certificate (this confirms DNS + 80/443 are right):

```sh
docker compose -f docker/docker-compose.showcase-lite.yaml logs -f caddy
# wait for "certificate obtained successfully" for DOMAIN, then Ctrl-C
```

> **Checkpoint:** `curl -s https://DOMAIN/healthz` is OK, and browsing to `https://DOMAIN/ui` loads
> the console and bounces you through Auth0 sign-in. Sign in as a user with the `admin` role — the
> write controls should appear. A sign-in loop or 401 is almost always a callback-URL (Phase 1.4),
> audience, or roles-claim mismatch. gRPC is published on `127.0.0.1:50051` only.

### Phase 6 — Build and deploy the generator (Auth0 M2M)

The generator is the private [`hippocampus-gen`](https://github.com/fastbean-au/hippocampus-gen)
module. **Cross-compile it on your workstation** (which has Go and repo access), then copy the binary
over — do not build it on the e2-micro:

```sh
# on your workstation, inside a hippocampus-gen checkout
export GOPRIVATE=github.com/fastbean-au/*
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o hippocampus-gen-book ./cmd/book

gcloud compute scp hippocampus-gen-book hippocampus-lite:/tmp/ \
  --project=GCP_PROJECT --zone=GCP_ZONE
```

Back **on the VM**, install it and stash the M2M secret in a root-only env file:

```sh
sudo install /tmp/hippocampus-gen-book /usr/local/bin/

sudo install -d /etc/hippocampus-gen
printf 'GEN_CLIENT_ID=%s\nGEN_SECRET=%s\n' 'GEN_CLIENT_ID' 'GEN_SECRET' \
  | sudo tee /etc/hippocampus-gen/lite.env >/dev/null
sudo chmod 600 /etc/hippocampus-gen/lite.env
```

Create `/etc/systemd/system/hippocampus-gen-lite.service` — note the **`--oidc-audience`** flag:
Auth0's client-credentials grant only returns a JWT for the API when the audience is requested.

```ini
[Unit]
Description=Hippocampus lite showcase generator
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/hippocampus-gen/lite.env
ExecStart=/usr/local/bin/hippocampus-gen-book -s localhost:50051 \
  --loop --period 24h --reset --pace-window 6h --live --summarise \
  --oidc-issuer https://AUTH0_DOMAIN/ \
  --oidc-audience AUTH0_AUDIENCE \
  --oidc-client-id ${GEN_CLIENT_ID} --oidc-client-secret ${GEN_SECRET}
Restart=always
RestartSec=30
DynamicUser=yes

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now hippocampus-gen-lite
journalctl -u hippocampus-gen-lite -f
```

> **Checkpoint:** the journal shows the generator obtaining a token and storing memories, and the
> console's list fills in.

> If your `hippocampus-gen` build predates Auth0 support and has no `--oidc-audience` flag, the
> client-credentials token comes back opaque and the service rejects it — add the audience parameter
> to the generator's OIDC client before deploying.

### Phase 7 — Operate

- **Logs:** `docker compose -f docker/docker-compose.showcase-lite.yaml logs -f hippocampus`.
- **Restart:** `docker compose -f docker/docker-compose.showcase-lite.yaml restart`.
- **Update:** `git pull`, `docker compose … up --build -d`, and re-`scp` the generator if it changed.
- **Wipe and reset:** `docker compose … down -v` drops the SQLite store and Caddy certs.
- **Cost:** `gcloud compute instances stop hippocampus-lite` when idle — the static IP and disk
  persist; `start` when you want it back.

## Terraform

This runbook is deliberately manual. If you deploy it often, the VM + firewall + static IP + a
startup script that performs steps 4–6 are straightforward to capture in Terraform; that is left as a
follow-up rather than shipped here.
