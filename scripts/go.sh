#!/bin/sh
# Run the Go toolchain inside Docker (no local Go install required).
# Usage: scripts/go.sh test ./...   |   scripts/go.sh build -o bin/hdhr-mux ./cmd/hdhr-mux
exec docker run --rm -v "$PWD":/src -v hdhrmux-gocache:/root/.cache/go-build -w /src \
  -e GOFLAGS=-buildvcs=false -e CGO_ENABLED="${CGO_ENABLED:-0}" golang:1.23 go "$@"
