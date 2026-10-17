FROM golang:1.23 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hdhr-mux ./cmd/hdhr-mux \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fakehdhr ./cmd/fakehdhr

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/hdhr-mux /out/fakehdhr /usr/local/bin/
VOLUME /var/lib/hdhr-mux
# Streams are pulled from the devices over HTTP, so only these ports need publishing.
# --network host is still easiest: HDHomeRun discovery is UDP broadcast.
EXPOSE 80 5004 65001/tcp 65001/udp
ENTRYPOINT ["/usr/local/bin/hdhr-mux"]
CMD ["-config", "/etc/hdhr-mux/config.json"]
