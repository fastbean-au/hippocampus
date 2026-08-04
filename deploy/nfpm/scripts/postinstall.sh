#!/bin/sh
# Reload systemd so the freshly installed/updated unit is visible. Deliberately does not enable or
# start the service — the operator reviews /etc/hippocampus/config.json first, then runs
# `systemctl enable --now hippocampus`. On an upgrade of an already-running instance, restart it so
# the new binary is picked up.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true

    if systemctl is-active --quiet hippocampus 2>/dev/null; then
        systemctl restart hippocampus || true
    fi
fi

exit 0
