# Installing and running

## Quick start

Needs Go 1.23, or just Docker (`scripts/go.sh` runs the Go toolchain in a
container).

```sh
scripts/go.sh build -o bin/hdhr-mux ./cmd/hdhr-mux
cp config.example.json config.json   # edit state_dir, etc.
sudo setcap cap_net_bind_service=+ep bin/hdhr-mux   # only for ports 80 / 5004 / 65001
bin/hdhr-mux -config config.json
curl -X POST 'http://HOST/lineup.post?scan=start&source=Antenna'   # or lineup.scan_on_start
```

**Docker.** Streams are *pulled* from the devices over HTTP, so no inbound UDP
port mapping is needed: publish `80`, `5004`, `65001/tcp` and `65001/udp`.
Broadcast discovery (both the multiplexer finding the devices and clients
finding the multiplexer) needs `--network host`; with a bridge network list the
devices in `upstream.devices` and add the multiplexer to the DVR by address.

```
docker run -d --name hdhr-mux --network host --restart unless-stopped \
  -v hdhr-mux-state:/var/lib/hdhr-mux \
  -v /path/to/etc:/etc/hdhr-mux:ro \
  hdhr-mux -config /etc/hdhr-mux/config.json
```

**Give the container a state directory.** The lineup, programme guide, the stable numbering of the
tuners and the settings saved from the web interface live in `state_dir` (`/var/lib/hdhr-mux` in the
image). Inside a container that directory disappears with the container unless you map a
directory or a named volume to it (`-v` above). The web interface's Settings page checks and warns
when it is not mapped (or is only an anonymous volume). `--restart unless-stopped` brings the multiplexer back after a reboot or a crash
(the Settings page's Restart button does not need it).

**On the same machine as the DVR.** The multiplexer can run on bare metal next to Channels DVR
(or Plex, Jellyfin, ...). Two ways, depending on whether the DVR should find it by itself:

* *Everything on the machine's addresses (default).* A discovery broadcast sent from this machine
  reaches the multiplexer like any other, and a client adding it by `127.0.0.1` or the machine's own
  address gets answers (`BaseURL` follows the address the client used). Checked with `hdhrctl` on
  the same host as the multiplexer.
* *Local only:* `"bind": "127.0.0.1"` (or Bind address on the Settings page) puts the web, streaming
  and control listeners on the loopback address only and turns SSDP off. Nothing on the network can
  reach it or discover it; add it to the DVR as a device at `127.0.0.1`. Use this when the real
  devices are already visible to the DVR and you only want the multiplexer's tuners for it. (Not yet
  tried with Channels DVR itself.)

The default ports are 80, 5004 and 65001: change `http_listen` if something else on the machine uses
port 80.


## Docker Compose and Portainer

The repository has two Compose files. `docker-compose.yml` is the one to use: host networking, the
published image and a named volume for the state.

```sh
curl -LO https://raw.githubusercontent.com/hdhrgit/hdhr-mux-demux/main/docker-compose.yml
docker compose up -d
```

Then open `http://HOST/ui/`. A new multiplexer is not discoverable until you turn **Discoverable** on
(Settings, General) or uncomment `HDHOMERUN_DISCOVERABLE` in the file; add it to a DVR by address until then.
`docker compose pull && docker compose up -d` updates it. The `latest` image follows the releases (not
the pre-releases); for a fixed version use a tag such as `ghcr.io/hdhrgit/hdhr-mux:0.99.3`.

`docker-compose.bridge.yml` is for hosts where host networking is not possible: it publishes 80, 5004
and 65001 (TCP and UDP), turns broadcast discovery off and lists the devices by address.

**Portainer.** Create a stack (Stacks, Add stack, Web editor) and paste the contents of
`docker-compose.yml`. Keep the volume: it is what survives recreating the container.


## Packages and images

| How | |
|---|---|
| Docker | `docker run -d --name hdhr-mux --network host --restart unless-stopped -v hdhr-mux-state:/var/lib/hdhr-mux ghcr.io/hdhrgit/hdhr-mux` (linux/amd64, arm64, arm/v7). Works with no config file: pass `-e HDHOMERUN_DEVICES=ip,ip` or use the Settings page. |
| Debian, Ubuntu, Raspberry Pi OS | `sudo apt install ./hdhr-mux_VERSION_ARCH.deb` (amd64, arm64, armhf) |
| Fedora, RHEL, Rocky, openSUSE | `sudo dnf install ./hdhr-mux-VERSION-1.ARCH.rpm` (x86_64, aarch64, armv7hl) |
| Any Linux | the tarball: the binary and its systemd unit |

A package is the binary (`/usr/bin/hdhr-mux`) and a systemd unit, nothing else. The unit runs the
service as a private user systemd creates for it (`DynamicUser`), so there is no account to create or
clean up, and keeps its state (lineup, guide, saved settings) in `/var/lib/hdhr-mux`. There is no config
file: everything has a default and the Settings page saves what you change. If you want one, create
`/etc/hdhr-mux/config.json` (it is read when it exists; see `config.example.json` in the repository).
Then: `sudo systemctl enable --now hdhr-mux` and open `http://HOST/ui/`. `scripts/release.sh VERSION` builds all of
them (tarballs, deb, rpm for the three architectures) with nothing but Docker; the release workflow
does that, and pushes the multi-architecture image, when a `vX.Y.Z` tag is pushed.

**In an LXC container** (Proxmox, LXD): install the .deb inside it and use the systemd unit, as on any
Debian. What matters is the container's network: it needs its own address on your LAN (bridged, for
example Proxmox's `vmbr0`, or macvlan), not a NATed container network, or HDHomeRun discovery
(UDP broadcast) and SSDP (multicast) do not reach it in either direction. Its disk is the container's own
disk, so the state directory is kept. Not yet tried on LXC.

