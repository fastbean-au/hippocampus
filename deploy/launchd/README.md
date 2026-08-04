# Native (launchd) deployment

Run Hippocampus as a per-user macOS LaunchAgent — the macOS counterpart to the
[systemd unit](../systemd/) for the embedded-SQLite single-instance model, without a container
runtime. It starts at login and restarts on exit.

## Install

Paths in [`au.fastbean.hippocampus.plist`](au.fastbean.hippocampus.plist) follow the Homebrew layout
(`/usr/local` on Intel). On **Apple Silicon** substitute `/opt/homebrew`, or edit them to any
absolute paths you prefer (launchd does **not** expand `~` — use the full `$HOME` path if you want
the store under your home directory).

```sh
# Binary, config, and the directories the plist logs/stores into.
sudo install -m0755 dist/hippocampus /usr/local/bin/hippocampus
sudo install -Dm0644 deploy/launchd/config.json /usr/local/etc/hippocampus/config.json
sudo mkdir -p /usr/local/var/hippocampus /usr/local/var/log/hippocampus
sudo chown "$(id -u)" /usr/local/var/hippocampus /usr/local/var/log/hippocampus

# Review the config (storage.directory must match the plist's paths), then load the agent.
"${EDITOR:-vi}" /usr/local/etc/hippocampus/config.json
cp deploy/launchd/au.fastbean.hippocampus.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/"$(id -u)" ~/Library/LaunchAgents/au.fastbean.hippocampus.plist
```

## Manage

```sh
launchctl kickstart -k gui/"$(id -u)"/au.fastbean.hippocampus                     # restart after upgrade/config change
launchctl print gui/"$(id -u)"/au.fastbean.hippocampus                            # status
launchctl bootout gui/"$(id -u)" ~/Library/LaunchAgents/au.fastbean.hippocampus.plist  # stop + unload
```

`kickstart`/`bootout` send `SIGTERM`, which the service handles as a graceful shutdown (drains the
gateway then the gRPC server, bounded by `shutdown.timeoutSeconds`).

## Notes

- **Per-user, not system-wide.** A LaunchAgent runs as the logged-in user (no privilege separation
  like the systemd unit's `DynamicUser`). For a login-scoped personal instance that's the intent; a
  system `LaunchDaemon` under `/Library/LaunchDaemons` would run at boot as root and is out of scope
  here.
- **Other drivers.** The default config uses embedded SQLite. For a shared Postgres/MySQL set
  `storage.driver` + the DSN and `consolidation.enabled` in `config.json` exactly as the compose/k8s
  overlays do — the agent is driver-agnostic.

See also [Operations · Running as a service](../../docs/operations.md#running-as-a-service).
