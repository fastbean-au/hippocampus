# Installing Hippocampus

Five ways to get a running instance, in roughly the order of how little work each is. The service is
a single statically-linked binary with no runtime dependencies on the default SQLite driver, so most
of these are one command.

Once it's up, [Getting started](getting-started.md) walks through the first requests over gRPC and
the JSON gateway, and [Operations & deployment](operations.md) covers supervision, driver choice and
sizing in detail.

## Homebrew (macOS and Linux)

The quickest supervised install. The formula generates and manages the launchd or systemd definition
for you, and installs a default embedded-SQLite config that's preserved across upgrades:

```sh
brew install fastbean-au/tap/hippocampus
brew services start hippocampus
```

The tap also carries the two client binaries, which need no supervision:

```sh
brew install fastbean-au/tap/hippocampus-cli   # the `hippo` CLI
brew install fastbean-au/tap/hippocampus-mcp   # the MCP bridge
```

See the [tap repo](https://github.com/fastbean-au/homebrew-tap) for what each formula installs and
where.

## Linux packages (`.deb` / `.rpm`)

Every [release](https://github.com/fastbean-au/hippocampus/releases) publishes packages carrying the
binary, a hardened systemd unit and a default config. They deliberately don't auto-enable the
service, so you get to review the config first:

```sh
sudo dpkg -i hippocampus_<version>_amd64.deb      # Debian/Ubuntu
sudo rpm -i  hippocampus-<version>.x86_64.rpm     # RHEL/Fedora/SUSE

sudoedit /etc/hippocampus/config.json
sudo systemctl enable --now hippocampus
```

The config is marked a conffile, so your edits survive upgrades. The shipped unit runs under a
transient unprivileged user (`DynamicUser`, with `StateDirectory` owning `/var/lib/hippocampus`) and
carries the full sandbox — dropped capabilities, `NoNewPrivileges`, `ProtectSystem=strict`, private
tmp and devices, a `@system-service` syscall filter. See
[`deploy/systemd/README.md`](../deploy/systemd/README.md), and
[Running as a service](operations.md#running-as-a-service) for the hand-rolled equivalents on a
distro without the packages.

## Containers

Multi-arch images are published to GHCR on every release. The image carries a default
embedded-SQLite config, serves gRPC on `50051` and the JSON gateway — plus the browser console at
`/ui` — on `8080`, and keeps the store in `/data`:

```sh
docker run -p 50051:50051 -p 8080:8080 -v hippocampus:/data ghcr.io/fastbean-au/hippocampus:latest
```

Mount your own config over `/etc/hippocampus/config.json` to replace the baked one, or override
individual keys with `HIPPOCAMPUS_*` environment variables.

Compose stacks per driver live in [`deploy/compose/`](../deploy/compose/) — `docker compose up` from
a clone brings up the SQLite one — and Kustomize overlays in
[`deploy/k8s/`](../deploy/k8s/README.md):

```sh
kubectl apply -k deploy/k8s/overlays/sqlite      # embedded: one StatefulSet plus a PVC
kubectl apply -k deploy/k8s/overlays/postgres    # centralised: one consolidator plus N replicas
```

The MCP bridge, the [configuration wizard](config-wizard.md), the [ingestor](ingestor.md) and the
five [broker bridges](eventsource.md) each ship their own image alongside the service. Details of
both paths are in [Containers and Kubernetes](operations.md#containers-and-kubernetes).

## Release binaries

Every release attaches an archive per binary, per OS and architecture — Linux, macOS and Windows on
both `amd64` and `arm64`. They're built with CGO disabled, so there's nothing to install alongside
them:

```sh
VERSION=v0.46.0
curl -fsSLO "https://github.com/fastbean-au/hippocampus/releases/download/${VERSION}/hippocampus_${VERSION}_linux_amd64.tar.gz"
tar -xzf "hippocampus_${VERSION}_linux_amd64.tar.gz"
./hippocampus --version
```

Archives are named `<binary>_<tag>_<os>_<arch>` — `.tar.gz` everywhere but Windows, which gets a
`.zip` — and each bundles one binary plus the licence.

The same release carries `hippo` (the [CLI](cli.md)), `hippocampus-mcp` (the [MCP
bridge](mcp.md)), `hippocampus-config-wizard`, `hippocampus-ingestor` and one bridge binary per
broker.

## From source

You'll need Go 1.27+ and nothing else:

```sh
git clone https://github.com/fastbean-au/hippocampus.git
cd hippocampus
go build -o hippocampus ./cmd/hippocampus
```

## Checking it works

Whichever route you took, the service answers on the HTTP gateway once it's up:

```sh
curl -s localhost:8080/healthz     # 200, with the build version in the body
open http://localhost:8080/ui      # the browser console
```

If you're running the binary by hand rather than under a supervisor, note that the gateway is off
unless it's given a port — `./hippocampus --gateway-port 8080` is what gets you the JSON API, the
probes and the console. The packaged and containerised installs set it in their shipped config
already.

With no `config.json` on the default path the service starts on its built-in defaults — SQLite in
`./data`, gRPC on `50051`, the power-law decay algorithm, no authentication — and logs a warning
naming them. That's enough to make requests against, but it's a starting point rather than a
deployment: there's no authentication, no TLS, no capacity target, and no automatic consolidation
cycle, so nothing is forgotten until you ask for it.

[Getting started](getting-started.md) picks up from here with the first requests and a configuration
worth growing from, and [Security](security.md) is the pass anything reachable beyond localhost
needs.
