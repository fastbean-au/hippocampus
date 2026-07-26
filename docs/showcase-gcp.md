# Deploying the showcase to a GCP VM

A runbook for standing up the two [hosted showcase](showcase.md) stacks (book and logs) on a single
Google Compute Engine VM: the server side runs as Docker Compose, the two data generators run as
systemd services, and Caddy provisions TLS automatically. Read [Hosted showcase](showcase.md) first —
this only covers the VM.

## 1. Sizing

Each stack runs Postgres + OpenSearch (1 GiB heap) + Keycloak (JVM) + an otel-lgtm bundle
(Grafana/Prometheus/Tempo/Loki) + hippocampus + Caddy, and there are two of them. Budget ~10 GiB of
RAM in use.

| | Recommendation |
|---|---|
| Machine type | `e2-standard-4` (4 vCPU / 16 GiB) minimum; `e2-standard-8` comfortable |
| Boot disk | 50 GiB `pd-ssd` (OpenSearch + telemetry retention) |
| Image | Ubuntu 24.04 LTS (simple Docker + Go install) |
| Region | anywhere close to your viewers |

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
  --loop --period 24h --reset --pace-window 2h --live --summarize \
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

## Terraform

This runbook is deliberately manual. If you deploy it often, the VM + firewall + static IP + a
startup script that performs steps 4–6 are straightforward to capture in Terraform; that is left as a
follow-up rather than shipped here.
