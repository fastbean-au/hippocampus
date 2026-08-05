# Configuration wizard

A guided, browser-based builder for a Hippocampus `config.json` and the deployment artefacts that
carry it. It asks what you are building and where it will run, shows what each answer will actually
do — including how long a memory of a given significance survives under the forgetting curve you
chose — and hands back a config file plus a Compose file, Kubernetes manifests, a systemd unit, a
launchd agent, or a runbook, whichever fits.

It is aimed at the first configuration. [Configurability](configuration.md) remains the exhaustive
reference, and everything the wizard writes is ordinary config you can keep editing by hand.

**Hosted:** <https://config-builder.hippocampus-demo.com>

## Running it yourself

The wizard is a static page. Nothing you type is transmitted anywhere — there is no server side to
transmit it to — and the hosted copy is the same artefact as the one you can run locally. Prefer
the local copy anyway if you would rather your DSNs and signing secrets never reach a page you did
not serve yourself.

```sh
# From a checkout
go run ./cmd/config-wizard                  # http://localhost:8091

# Or the published image
docker run --rm -p 8091:8091 ghcr.io/fastbean-au/hippocampus-config-wizard:latest
```

Each GitHub release also carries a `hippocampus-config-wizard` binary for every OS/arch; the assets
are embedded in it, so the binary is the whole application.

| Flag             | Default | Purpose                                                  |
| ---------------- | ------- | -------------------------------------------------------- |
| `--port`         | `8091`  | HTTP listen port.                                        |
| `--bind-address` | (all)   | Interface to bind; `127.0.0.1` restricts it to loopback. |
| `--log-level`    | `info`  | `trace`, `debug`, `info`, `warn`, `error`.               |
| `--version`      |         | Print the build version and exit.                        |

Every flag is also settable as `HIPPOCAMPUS_WIZARD_<FLAG>` (for example
`HIPPOCAMPUS_WIZARD_PORT=9000`). `/healthz` answers for container and load-balancer probes.

## What it produces

| Artefact                                  | When                                                    |
| ----------------------------------------- | ------------------------------------------------------- |
| `config.json`                             | Always.                                                 |
| `.env` / `hippocampus.env`                | When any secret is set — see [Secrets](#secrets) below. |
| `docker-compose.yaml`                     | Deployment target **Docker / Podman Compose**.          |
| `hippocampus.yaml` (namespace → workload) | Target **Kubernetes**.                                  |
| `hippocampus.service`                     | Target **Linux systemd**.                               |
| `au.fastbean.hippocampus.plist`           | Target **macOS launchd**.                               |
| `DEPLOY.md`                               | Always — the runbook for the chosen target.             |

The deployment artefacts follow the shapes the repo already ships: the Compose file mirrors
[`deploy/compose/`](../deploy/compose/) (adding a Postgres, MySQL, OpenSearch, or Ollama service only
when the configuration needs one), the manifests mirror [`deploy/k8s/`](../deploy/k8s/) — a
`StatefulSet` for SQLite, a single consolidator `Deployment` plus replicas and a
`PodDisruptionBudget` for the shared-database drivers — and the unit and plist mirror
[`deploy/systemd/`](../deploy/systemd/) and [`deploy/launchd/`](../deploy/launchd/), hardening and
all.

## The forgetting preview

The **Memory & forgetting** step charts the decay curve for the chosen algorithm, aggressiveness,
threshold, and age unit, and tabulates how long a memory of a given effective significance survives.
The maths is the service's own (`calculateValue` in `hippocampus/sleep.go`), including the minimum
age and hard retention floors, so the numbers are what the sleep cycle will actually do — the one
caveat being that they assume no capacity pressure, which only ever makes forgetting sooner. It is
the fastest way to find out that the aggressiveness you picked would delete everything inside a day,
before it does.

## Checks

The wizard applies the service's own startup validation as you type — the rules in `validateConfig`
and the storage-driver switch — so a configuration that passes here starts. That covers the
outright refusals (`consolidation.unitsOfAgeInDays` at 0, method 3's aggressiveness floor of 1/e, a
`walTriggerBytes` on a server driver, an empty `storage.directory` under SQLite, an `idp` method
without an issuer or JWKS) as well as the things the service only warns about at startup or does not
mention at all: a short HMAC secret, auth without TLS, a capacity target with no eviction floor, both
capacity axes disabled, a disabled gateway under a target whose probes need it.

## Secrets

Signing secrets, database DSNs, an OpenSearch password, a transfer token, and an OAuth2 client
secret are treated as secrets. They stay out of `config.json` unless you ask otherwise, and are
written to a separate environment file as `HIPPOCAMPUS_*` overrides — the mechanism
[Configurability](configuration.md#environment-variable-overrides) recommends for exactly this. They
are also kept out of the browser's `localStorage`, so a reload keeps your answers but never your
credentials.

The generated Kubernetes `Secret` carries `CHANGE-ME` placeholders rather than the values, so a
manifest can be committed and the real values injected from Sealed Secrets, External Secrets, SOPS,
or whatever your cluster already uses.

## Editing an existing config

**Import config.json** reads a file you already have back into the wizard, so you can review it
against the current validation rules, see what has been changed from the service defaults, and
regenerate the deployment artefacts around it. Keys the wizard does not manage
(`auth.signingKeys`, for instance) are reported and dropped rather than silently rewritten — keep
the original if it carries any.

## Full or minimal config

By default the wizard writes every key it knows about, which produces a file that reads like the
repo's own [`config.json`](../config.json) — useful as documentation of what a deployment decided.
The **only keys the service does not already default** toggle writes the smaller form instead: every
key that would actually change behaviour, and nothing else.

The distinction matters more than it looks. The service applies a built-in default to only a handful
of keys; every other absent key reads as its zero value, and several of those are fatal
(`consolidation.method`, `aggressiveness`, and `unitsOfAgeInDays` all refuse to start at 0). The
minimal form therefore still writes those out. A test in `cmd/config-wizard` keeps the wizard's list
of service defaults in step with `viper.SetDefault` in `cmd/hippocampus/main.go`, so the two cannot
drift into producing a config that looks right and will not start.
