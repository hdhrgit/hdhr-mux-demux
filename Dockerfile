# syntax=docker/dockerfile:1
# Multi-architecture: the build always runs natively and cross-compiles, so
#   docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 ...
# needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.23 AS build
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
WORKDIR /src
COPY . .
RUN export GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} CGO_ENABLED=0 \
 && go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/hdhr-mux ./cmd/hdhr-mux \
 && go build -trimpath -ldflags="-s -w" -o /out/hdhrctl ./cmd/hdhrctl

FROM scratch
COPY --from=build /out/hdhr-mux /out/hdhrctl /usr/local/bin/
# The working directory is the state directory (state_dir defaults to "."), so
# the image runs with no configuration at all. Map a directory or a named volume
# to it, or the lineup, guide, tuner numbering and saved settings are lost when
# the container is removed:  -v hdhr-mux-state:/var/lib/hdhr-mux
WORKDIR /var/lib/hdhr-mux
VOLUME /var/lib/hdhr-mux
# Streams are pulled from the devices over HTTP, so only these ports need
# publishing. --network host is still easiest: HDHomeRun discovery is UDP broadcast.
EXPOSE 80 5004 65001/tcp 65001/udp
ENTRYPOINT ["/usr/local/bin/hdhr-mux"]
# Optionally: -config /etc/hdhr-mux/config.json with the file mounted there.
CMD []
