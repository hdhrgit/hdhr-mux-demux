#!/bin/sh
# Build the release artifacts into dist/:
#   hdhr-mux_VERSION_linux_{amd64,arm64,armv7}.tar.gz   the binary and its systemd unit
#   hdhr-mux_VERSION_{amd64,arm64,armhf}.deb  and  hdhr-mux-VERSION.{x86_64,aarch64,armv7hl}.rpm
#   SHA256SUMS
# Needs only Docker (Go and nfpm run in containers):  scripts/release.sh 0.99.0
set -e
VERSION=${1:?usage: scripts/release.sh VERSION}
VERSION=${VERSION#v}
cd "$(dirname "$0")/.."
rm -rf dist && mkdir -p dist/bin

# GOARCH GOARM NFPM_ARCH TARBALL_ARCH
TARGETS="amd64 - amd64 amd64
arm64 - arm64 arm64
arm 7 arm7 armv7"

echo "$TARGETS" | while read goarch goarm nfarch tarch; do
    [ "$goarm" = "-" ] && goarm=""
    out=dist/bin/linux-$goarch$goarm
    mkdir -p "$out"
    docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 \
        -e GOOS=linux -e GOARCH="$goarch" -e GOARM="$goarm" golang:1.23 sh -c "
        go build -trimpath -ldflags='-s -w -X main.version=$VERSION' -o $out/hdhr-mux ./cmd/hdhr-mux"
    # tarball
    stage=dist/stage/hdhr-mux_${VERSION}_linux_$tarch
    mkdir -p "$stage"
    cp "$out/hdhr-mux" packaging/hdhr-mux.service "$stage/"
    tar -C dist/stage -czf "dist/hdhr-mux_${VERSION}_linux_$tarch.tar.gz" "hdhr-mux_${VERSION}_linux_$tarch"
    # deb and rpm (nfpm does not expand variables inside file paths, so do that here)
    sed -e "s|\${GOARCH}|$goarch|g" -e "s|\${GOARM}|$goarm|g" packaging/nfpm.yaml > dist/stage/nfpm.yaml
    for fmt in deb rpm; do
        docker run --rm -v "$PWD":/src -w /src -e VERSION="$VERSION" -e ARCH="$nfarch" \
            goreleaser/nfpm:latest package --config dist/stage/nfpm.yaml --packager $fmt --target dist/
    done
done
rm -rf dist/stage dist/bin
(cd dist && sha256sum * > SHA256SUMS)
ls -l dist
