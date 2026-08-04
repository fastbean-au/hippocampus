# Native (systemd) deployment

Run Hippocampus as a hardened systemd service on a VM or bare metal — no container runtime, no
orchestrator. This is the fourth deployment pillar alongside the binary release, Docker Compose
(`deploy/compose/`), and Kubernetes (`deploy/k8s/`); it suits the embedded-SQLite single-instance
model (one binary + one data directory) that compose and k8s both wrap more heavily.

## Install from a package (recommended)

The `.deb`/`.rpm` built by [`deploy/nfpm/nfpm.yaml`](../nfpm/nfpm.yaml) install the binary at
`/usr/bin/hippocampus`, the unit at `/usr/lib/systemd/system/hippocampus.service`, and a default
config at `/etc/hippocampus/config.json`:

```sh
sudo dpkg -i hippocampus_<version>_amd64.deb      # Debian/Ubuntu
sudo rpm -i  hippocampus-<version>.x86_64.rpm     # RHEL/Fedora/SUSE

sudoedit /etc/hippocampus/config.json             # review before first start
sudo systemctl enable --now hippocampus
```

The package runs `daemon-reload` on install and restarts a running instance on upgrade, but never
auto-enables — you start it after reviewing the config. Config edits survive upgrades
(`config|noreplace`).

## Install by hand

```sh
sudo install -m0755 dist/hippocampus /usr/bin/hippocampus
sudo install -m0644 deploy/systemd/hippocampus.service /usr/lib/systemd/system/hippocampus.service
sudo install -Dm0644 deploy/systemd/config.json /etc/hippocampus/config.json
sudo systemctl daemon-reload
sudo systemctl enable --now hippocampus
```

## What the unit does

- **Unprivileged, isolated.** `DynamicUser=yes` runs it under a transient system user;
  `StateDirectory=hippocampus` owns `/var/lib/hippocampus` (where the SQLite file and its WAL live —
  keep `storage.directory` pointed there), `ConfigurationDirectory=hippocampus` owns
  `/etc/hippocampus`.
- **Hardened.** The systemd analogue of the k8s pod `securityContext`: dropped capabilities,
  `NoNewPrivileges`, `ProtectSystem=strict`, private tmp/devices, a `@system-service` syscall
  filter, and `MemoryDenyWriteExecute` (safe — the binary is pure Go, CGO disabled, no JIT).
- **Graceful shutdown.** `SIGTERM` drains the gateway then the gRPC server; `TimeoutStopSec=30`
  gives the `shutdown.timeoutSeconds` phases headroom.

## Other drivers

The default config uses the embedded SQLite driver. To run against a shared Postgres/MySQL
(centralised / horizontal-scaling model), set `storage.driver` + the DSN and
`consolidation.enabled` in `config.json` exactly as the compose/k8s overlays do — the unit is
driver-agnostic. Front the listeners with a TLS-terminating reverse proxy, or set `tls.enabled` and
`bindAddress`, for anything beyond localhost.
