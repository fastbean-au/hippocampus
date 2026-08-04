#!/bin/sh
# Stop and disable the service on a genuine removal. The argument distinguishes remove from upgrade:
# deb passes "remove"/"upgrade"; rpm passes "0" (final removal) / "1" (upgrade). On an upgrade we
# leave the running unit alone — postinstall restarts it onto the new binary.
set -e

case "$1" in
    remove | 0)
        if command -v systemctl >/dev/null 2>&1; then
            systemctl disable --now hippocampus || true
        fi
        ;;
esac

exit 0
