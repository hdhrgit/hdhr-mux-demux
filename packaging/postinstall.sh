#!/bin/sh
set -e
if ! getent passwd hdhr-mux >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --no-create-home --home-dir /var/lib/hdhr-mux --shell /usr/sbin/nologin hdhr-mux
    else
        adduser --system --no-create-home --home /var/lib/hdhr-mux --shell /usr/sbin/nologin hdhr-mux
    fi
fi
mkdir -p /var/lib/hdhr-mux
chown hdhr-mux:hdhr-mux /var/lib/hdhr-mux 2>/dev/null || chown hdhr-mux /var/lib/hdhr-mux
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
    echo "hdhr-mux installed. Start it with:  systemctl enable --now hdhr-mux"
    echo "Then open  http://$(hostname)/ui/  (settings are on the Settings page)."
fi
