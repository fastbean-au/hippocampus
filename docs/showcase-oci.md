# Deploying the showcase to an OCI VM

A runbook for standing up the two [hosted showcase](showcase.md) stacks (book and logs) on a single
Oracle Cloud Infrastructure (OCI) compute instance: the server side runs as Docker Compose, the two
data generators run as systemd services, and Caddy provisions TLS automatically. Read [Hosted
showcase](showcase.md) first — this only covers the OCI instance. It is the sibling of the [GCP
runbook](showcase-gcp.md); everything above the cloud (Compose, Caddy, the IdP, the generators) is
identical, so this document concentrates on the OCI-specific plumbing.

> **Two ways to run this.** The runbook below (steps 1–7) is the full stack and needs a mid-size
> instance (4 OCPUs / ~24 GiB). If you only want _one_ showcase and can live without the
> content-search tab and the Grafana dashboards, jump to [A lite stack on an Always Free
> micro](#a-lite-stack-on-an-always-free-micro): the same console and Auth0 sign-in, trimmed to fit a
> 1 OCPU / 1 GiB machine.
>
> **The OCI sweetener.** OCI's [Always Free](https://www.oracle.com/cloud/free/) tier includes an Arm
> Ampere allocation of **up to 4 OCPUs and 24 GiB of memory** (`VM.Standard.A1.Flex`). That is enough
> to run the _full_ stack **at no cost** — the one cloud where the whole showcase fits inside the free
> tier. The only catch is architecture: A1 is `arm64`, so the generator must be cross-compiled for
> Arm (noted in step 6). Prefer x86? The two Always Free `VM.Standard.E2.1.Micro` instances (1 OCPU /
> 1 GiB each) host the lite stack instead.

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

## 0. Prerequisites and OCI vocabulary

- The [OCI CLI](https://docs.oracle.com/en-us/iaas/Content/API/SDKDocs/cliinstall.htm) installed and
  configured (`oci setup config`), or use the Console — the fiddly bits (the VCN wizard) are easier
  there and called out below.
- An SSH key pair; the public key is handed to the instance at launch (OCI has no equivalent of GCP's
  managed SSH — you connect with your own key as the `ubuntu` user).
- A few OCIDs you will reuse: your **compartment** OCID (`-c`/`--compartment-id` everywhere), and an
  **availability domain** (AD) name from `oci iam availability-domain list`.

Two OCI quirks worth internalising before you size anything:

- **OCPU ≠ vCPU.** On the x86 flex shapes 1 OCPU = 2 vCPUs (a full hyper-threaded core); on Arm
  (`A1.Flex`) 1 OCPU = 1 core. So a 4-OCPU `E4.Flex` is 8 vCPUs — comfortably more than the GCP
  `e2-standard-4` the sibling runbook uses.
- **Flex shapes let you dial OCPUs and memory independently** via `--shape-config`, so you are not
  boxed into fixed machine types.

## 1. Sizing

Each stack runs Postgres + OpenSearch (1 GiB heap) + Keycloak (JVM) + an otel-lgtm bundle
(Grafana/Prometheus/Tempo/Loki) + hippocampus + Caddy, and there are two of them. Budget ~10 GiB of
RAM in use.

|             | Recommendation                                                                                                       |
| ----------- | -------------------------------------------------------------------------------------------------------------------- |
| Shape       | `VM.Standard.E4.Flex` at **4 OCPUs / 24 GiB** (8 vCPUs); Arm `VM.Standard.A1.Flex` at 4/24 is the Always Free option |
| Boot volume | 100 GiB, Balanced performance (OpenSearch + telemetry retention)                                                     |
| Image       | Canonical Ubuntu 24.04 (simple Docker + Go install)                                                                  |
| Region / AD | any region close to your viewers; pick one AD                                                                        |

> On Arm (`A1.Flex`) every container image must be `arm64`. Hippocampus and the sidecars all build or
> pull multi-arch, so the only Arm-specific step is cross-compiling the generator (step 6).

## 2. DNS

Pick two domains, one per stack (e.g. `book.example` and `logs.example`). Each needs three A records
— the apex/console, `auth.`, and `grafana.` — all pointing at the instance's **public IP**. Six
records total:

```text
book.example            A   <VM_IP>
auth.book.example       A   <VM_IP>
grafana.book.example    A   <VM_IP>
logs.example            A   <VM_IP>
auth.logs.example       A   <VM_IP>
grafana.logs.example    A   <VM_IP>
```

These **must resolve before first boot** of the stacks — Caddy's Let's Encrypt challenge fails
otherwise. On OCI an **ephemeral** public IP survives a stop/start (it is only released on
_terminate_), so unlike GCP you often do not need to reserve one; reserve a **public IP** (below)
only if you may terminate and rebuild.

## 3. Create the network, instance, and security

OCI has no default network, so there is a VCN to stand up first. The quickest path is the Console:
**Networking → Virtual Cloud Networks → Create VCN → _Create VCN with Internet Connectivity_** (the
wizard), which gives you a VCN, a public subnet, an internet gateway, and a route table in one shot.
Note the **subnet OCID** it creates. (The CLI equivalent is `oci network vcn create` plus an internet
gateway, route rule, and subnet — four calls; the wizard is genuinely the pragmatic choice here.)

Rather than editing the subnet's default security list, attach a **Network Security Group** (NSG) to
the instance — its rules are self-contained and easy to script:

```sh
NSG_ID=$(oci network nsg create -c "$COMPARTMENT" --vcn-id "$VCN_ID" \
  --display-name hippocampus-showcase --query 'data.id' --raw-output)

# Only 80/443 are exposed. The gRPC ports (50051/50052) stay instance-local: the generators run on
# the VM and dial localhost, so there is no reason to open them to the internet.
for PORT in 80 443; do
  oci network nsg rules add --nsg-id "$NSG_ID" --security-rules '[{
    "direction":"INGRESS","protocol":"6","source":"0.0.0.0/0","sourceType":"CIDR_BLOCK",
    "tcpOptions":{"destinationPortRange":{"min":'"$PORT"',"max":'"$PORT"'}}}]'
done
```

(SSH on 22 is already allowed by the wizard's default security list.)

Launch the instance, handing it the subnet, the NSG, and your SSH public key:

```sh
IMAGE_ID=$(oci compute image list -c "$COMPARTMENT" \
  --operating-system "Canonical Ubuntu" --operating-system-version 24.04 \
  --shape VM.Standard.E4.Flex --query 'data[0].id' --raw-output)

oci compute instance launch -c "$COMPARTMENT" \
  --availability-domain "$AD" --display-name hippocampus-showcase \
  --shape VM.Standard.E4.Flex --shape-config '{"ocpus":4,"memoryInGBs":24}' \
  --image-id "$IMAGE_ID" --subnet-id "$SUBNET_ID" --nsg-ids '["'"$NSG_ID"'"]' \
  --assign-public-ip true --boot-volume-size-in-gbs 100 \
  --ssh-authorized-keys-file ~/.ssh/id_ed25519.pub
```

For Always Free Arm, swap `--shape VM.Standard.A1.Flex` (and the same `--shape-config`) and select
the Arm build of the image (drop `--shape` from the `image list` filter, or pick the `aarch64` image).

Find the public IP and SSH in as `ubuntu`:

```sh
oci compute instance list-vnics --instance-id "$INSTANCE_ID" \
  --query 'data[0]."public-ip"' --raw-output
ssh ubuntu@<VM_IP>
```

> **The OCI host-firewall gotcha.** Canonical's OCI Ubuntu images ship with host `iptables` rules
> that allow only SSH and reject the rest — so even with the NSG open, 80/443 are blocked _on the
> box_ until you open them there too, and Caddy's ACME challenge will hang. Do this on the instance
> **before** bringing up the stacks:
>
> ```sh
> sudo iptables -I INPUT -p tcp --dport 80 -j ACCEPT
> sudo iptables -I INPUT -p tcp --dport 443 -j ACCEPT
> sudo netfilter-persistent save
> ```
>
> (This is the single most common reason an OCI Caddy stack never gets a certificate.)

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
`https://book.example/ui` and sign in as `admin-demo` / `writer-demo` / `reader-demo`. A challenge
that never completes almost always means the host `iptables` rules (step 3) or the NSG ingress
weren't opened.

## 6. Run the generators as systemd services

The generators are a separate module ([`hippocampus-gen`](https://github.com/fastbean-au/hippocampus-gen));
they run as host processes against the instance-local gRPC ports. That module depends on the private
`hippocampus` module, so building on the instance needs Git credentials — set `GOPRIVATE` and provide
a token (a read-only deploy token is enough):

```sh
git clone https://github.com/fastbean-au/hippocampus-gen.git
cd hippocampus-gen
export GOPRIVATE=github.com/fastbean-au/*
git config --global url."https://<TOKEN>@github.com/".insteadOf "https://github.com/"
go build -o /usr/local/bin/hippocampus-gen-book ./cmd/book
go build -o /usr/local/bin/hippocampus-gen-logs ./cmd/logs
```

(Alternatively build the two binaries on a machine that already has access and `scp` them over — no
toolchain or credentials on the instance.)

> **On Arm (`A1.Flex`):** if you build the binaries elsewhere, cross-compile them for Arm —
> `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build …` — or an `x86-64` binary will `exec format
error` on the instance. Building _on_ the box picks the right arch automatically.

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

> The client secret appears in the process command line (visible to `ps` on the box). That is
> acceptable for a throwaway showcase; for anything more, teach the generator to read the secret from
> the environment instead of a flag.

## 7. Operate

- **Restart a stack:** `docker compose -f docker/docker-compose.showcase-book.yaml restart`.
- **Update:** `git pull`, then `docker compose ... up --build -d`, and rebuild the generator binaries.
  Keycloak keeps its realm (named volume); the book store is purged each cycle anyway.
- **Reset everything:** `docker compose ... down -v` drops the named volumes (Postgres, OpenSearch,
  Keycloak, Caddy certs) for a clean slate — the realm re-imports on next start.
- **Certificates** live in the `*-caddy-data` volume and renew automatically; keep 80/443 reachable
  (both in the NSG _and_ the host `iptables`).
- **Cost:** on the Always Free Arm shape the compute is free; otherwise stop the instance when not
  demoing (`oci compute instance action --instance-id <id> --action STOP`) — the boot volume persists
  and the ephemeral public IP is retained across stop/start.

## A lite stack on an Always Free micro

The full stack above runs Postgres + OpenSearch + Keycloak + otel-lgtm + hippocampus + Caddy, twice —
budget ~10 GiB of RAM. The **lite stack** (`docker/docker-compose.showcase-lite.yaml`) is the same
console trimmed down to fit a single **Always Free `VM.Standard.E2.1.Micro` (1 OCPU / 1 GiB)**: it
drops Postgres (SQLite instead), OpenSearch (no content-search tab), and otel-lgtm (no Grafana
dashboards), and it replaces self-hosted Keycloak with **hosted Auth0** — so there is no JVM on the
box at all. What remains is two small containers (hippocampus on SQLite, and Caddy), plus the
generator as a host process.

RAM is comfortable (~500 MiB in use); the **1 OCPU is the real limit**, so run the generator gently
(low pace, one loop) and treat this as a trickle demo, not a soak test.

|             | Recommendation                                                        |
| ----------- | --------------------------------------------------------------------- |
| Shape       | `VM.Standard.E2.1.Micro` (1 OCPU / 1 GiB, Always Free — two included) |
| Boot volume | 50 GiB (the Always Free minimum; SQLite + image layers fit easily)    |
| Image       | Canonical Ubuntu 24.04                                                |
| Auth        | An Auth0 tenant (free tier is fine)                                   |

> Prefer the Always Free **Arm** shape (`VM.Standard.A1.Flex`, up to 4 OCPUs / 24 GiB)? It runs the
> lite stack with room to spare — or the _full_ stack, as noted at the top. Just cross-compile the
> generator for `arm64`.

The phases below are ordered so a mistake surfaces at the next **Checkpoint** rather than three steps
later. Two ordering rules matter: **DNS must resolve before you start the stack** (Caddy's Let's
Encrypt challenge fails otherwise), and **Auth0's callback URL must match your final domain** — so
Auth0 and DNS come first, before anything boots.

### Phase 0 — Gather your values

Fill these in once; every command references them. You supply a domain, an OCI compartment + subnet,
and an Auth0 tenant.

| Placeholder                    | What it is                             | Example                             |
| ------------------------------ | -------------------------------------- | ----------------------------------- |
| `DOMAIN`                       | Public hostname for the console        | `demo.example.com`                  |
| `ACME_EMAIL`                   | Email for Let's Encrypt                | `you@example.com`                   |
| `COMPARTMENT`                  | Your OCI compartment OCID              | `ocid1.compartment.oc1..…`          |
| `AD`                           | Availability domain name               | `Uocm:PHX-AD-1`                     |
| `SUBNET_ID`                    | Public subnet OCID (from Phase 2)      | `ocid1.subnet.oc1.phx.…`            |
| `AUTH0_DOMAIN`                 | Your Auth0 tenant domain               | `your-tenant.us.auth0.com`          |
| `AUTH0_AUDIENCE`               | Auth0 API identifier (you choose it)   | `https://hippocampus.api`           |
| `AUTH0_ROLES_CLAIM`            | Namespaced roles claim (you choose it) | `https://hippocampus.example/roles` |
| `AUTH0_CLIENT_ID`              | SPA app client id (from Phase 1)       | _filled in Phase 1_                 |
| `GEN_CLIENT_ID` / `GEN_SECRET` | M2M app id + secret (from Phase 1)     | _filled in Phase 1_                 |

### Phase 1 — Auth0 (browser, ~10 min)

Identical to the GCP runbook — the identity provider is cloud-agnostic. Follow [Phase 1 of the GCP
runbook](showcase-gcp.md#phase-1--auth0-browser-10-min) to create the **API** (audience, RS256), the
`reader`/`writer`/`admin` **roles**, the namespaced **roles-claim Action** (Login _and_ Machine to
Machine flows), the **console SPA app** (callback `https://DOMAIN/ui`), and the **generator M2M app**.
The [showcase.md Auth0 section](showcase.md#auth0-saas) covers the same ground in prose.

> **Checkpoint:** you now hold `AUTH0_DOMAIN`, `AUTH0_AUDIENCE`, `AUTH0_ROLES_CLAIM`,
> `AUTH0_CLIENT_ID`, `GEN_CLIENT_ID`, and `GEN_SECRET`.

### Phase 2 — Network and DNS

Stand up the VCN as in [step 3](#3-create-the-network-instance-and-security) (the _Create VCN with
Internet Connectivity_ wizard is easiest) and note its public `SUBNET_ID`. There is no `auth.` or
`grafana.` subdomain here — Auth0 is the issuer and there is no Grafana — so create **one A record**:
`DOMAIN` → the instance's public IP.

You will not have the IP until the instance launches (Phase 3), so either launch first and then add
the record, or reserve a public IP up front:

```sh
oci network public-ip create -c "$COMPARTMENT" --lifetime RESERVED \
  --display-name hippocampus-lite-ip --query 'data."ip-address"' --raw-output
```

and associate it with the instance's primary VNIC after launch (`oci network public-ip update
--public-ip-id <id> --private-ip-id <primary-private-ip-id>`).

> **Checkpoint:** `dig +short DOMAIN` returns the instance's IP. **Do not proceed to Phase 5 until it
> does** — the ACME challenge needs it.

### Phase 3 — Create the instance and security

Attach an NSG opening only 80/443 (as in [step 3](#3-create-the-network-instance-and-security)), then
launch the micro:

```sh
IMAGE_ID=$(oci compute image list -c "$COMPARTMENT" \
  --operating-system "Canonical Ubuntu" --operating-system-version 24.04 \
  --shape VM.Standard.E2.1.Micro --query 'data[0].id' --raw-output)

oci compute instance launch -c "$COMPARTMENT" \
  --availability-domain "$AD" --display-name hippocampus-lite \
  --shape VM.Standard.E2.1.Micro \
  --image-id "$IMAGE_ID" --subnet-id "$SUBNET_ID" --nsg-ids '["'"$NSG_ID"'"]' \
  --assign-public-ip true --boot-volume-size-in-gbs 50 \
  --ssh-authorized-keys-file ~/.ssh/id_ed25519.pub
```

`VM.Standard.E2.1.Micro` is a fixed shape — no `--shape-config`. SSH in and **open the host
firewall** (the same OCI Ubuntu gotcha as the full stack):

```sh
ssh ubuntu@<VM_IP>
sudo iptables -I INPUT -p tcp --dport 80 -j ACCEPT
sudo iptables -I INPUT -p tcp --dport 443 -j ACCEPT
sudo netfilter-persistent save
```

### Phase 4 — Install Docker (on the VM)

```sh
sudo apt-get update && sudo apt-get install -y docker.io docker-compose-v2 git
sudo usermod -aG docker "$USER"
```

Log out and back in (`exit`, then `ssh …` again) for the group to take effect.

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

The first build compiles the Go image and is slow (several minutes) on a single OCPU — that is
one-time. Watch Caddy obtain the certificate (this confirms DNS + 80/443 are right, at both the NSG
_and_ host-firewall layers):

```sh
docker compose -f docker/docker-compose.showcase-lite.yaml logs -f caddy
# wait for "certificate obtained successfully" for DOMAIN, then Ctrl-C
```

> **Checkpoint:** `curl -s https://DOMAIN/healthz` is OK, and browsing to `https://DOMAIN/ui` loads
> the console and bounces you through Auth0 sign-in. Sign in as a user with the `admin` role — the
> write controls should appear. A sign-in loop or 401 is almost always a callback-URL (Phase 1),
> audience, or roles-claim mismatch. gRPC is published on `127.0.0.1:50051` only.

### Phase 6 — Build and deploy the generator (Auth0 M2M)

The generator is the private [`hippocampus-gen`](https://github.com/fastbean-au/hippocampus-gen)
module. **Cross-compile it on your workstation** (which has Go and repo access), then copy the binary
over — do not build it on the micro:

```sh
# on your workstation, inside a hippocampus-gen checkout
export GOPRIVATE=github.com/fastbean-au/*
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o hippocampus-gen-book ./cmd/book
#           ^^^^^^^^^^^^ use GOARCH=arm64 for an A1.Flex (Arm) instance

scp hippocampus-gen-book ubuntu@<VM_IP>:/tmp/
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
- **Cost:** the Always Free micro is free to leave running; if you used a paid shape,
  `oci compute instance action --instance-id <id> --action STOP` when idle — the boot volume persists
  and the ephemeral public IP is retained across stop/start.

## Terraform / Resource Manager

This runbook is deliberately manual. If you deploy it often, the VCN + subnet + internet gateway +
NSG + instance + optional reserved public IP + a `cloud-init` script that performs steps 4–6 are
straightforward to capture in Terraform (the `oci` provider), which OCI's **Resource Manager** can
run as a managed stack. That is left as a follow-up rather than shipped here.
