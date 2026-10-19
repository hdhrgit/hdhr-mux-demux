#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
    echo "hdhr-mux installed. Start it with:  systemctl enable --now hdhr-mux"
    echo "Then open  http://$(hostname)/ui/  (settings are on the Settings page)."
fi
