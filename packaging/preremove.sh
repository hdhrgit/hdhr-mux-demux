#!/bin/sh
# on removal (not upgrade) stop and disable the service; the state directory is kept
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    case "$1" in
        remove|0) systemctl disable --now hdhr-mux >/dev/null 2>&1 || true ;;
    esac
fi
exit 0
