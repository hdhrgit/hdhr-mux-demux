# hdhr-mux-demux

A virtual HDHomeRun that **combines several physical HDHomeRun devices into
one** (4 devices × 4 tuners = one 16-tuner device) and **shares a physical
tuner between every client watching the same RF channel**.

```
 Channels DVR / Plex / Jellyfin / hdhomerun_config / VLC ...
      |  discovery (UDP 65001)  control (TCP 65001)  web/API (80)  streams (5004)
      v
 +-----------------------------------------------------------+
 |  virtual HDHomeRun: tuners 0..15, one lineup              |
 |   each viewer has its own PID / program filter + buffer   |
 |   +----------------+  +----------------+                  |
 |   | shared stream  |  | shared stream  |   ...            |
 |   | RF 207 MHz     |  | RF 569 MHz     |                  |
 |   +-------+--------+  +-------+--------+                  |
 |   tuner pool: classes, failover, lingering, eviction      |
 +-----------|-------------------|---------------------------+
        HTTP GET :5004/tunerN/chFREQ  (pulled from the devices)
```


## What it does

* **One device from many.** Every tuner of every HomeRun on the network, numbered stably, behind one
  discovery announcement, one lineup and one web interface.
* **Shared tuners.** Everyone watching the same RF channel through `/auto/` (Channels DVR, the playlist,
  DLNA) shares one physical tuner, each with their own program filter. The devices are told to send only
  the PIDs being watched, not the whole 19 Mbit/s multiplex.
* **Direct clients keep their tuner.** tvheadend, `hdhomerun_config` and `/tunerN/` get exactly the tuner
  they ask for, as on a real HomeRun.
* **The devices decide the lineup.** Channels, names and where they are on the air come from the devices'
  own lineups; stations carried on two RF channels can be steered and merged on the Advanced tab.
* **Programme guide.** Collected from the broadcasts (PSIP) and served as XMLTV and JSON, plus an M3U
  playlist and a DLNA media server.
* **Web interface and metrics.** Activity, Lineup, Advanced and Settings pages; Prometheus `/metrics`
  (a Netdata profile is in `contrib/netdata`).

## Quick start

With Docker Compose (host networking; the state is kept in a named volume):

```sh
curl -LO https://raw.githubusercontent.com/hdhrgit/hdhr-mux-demux/main/docker-compose.yml
docker compose up -d
```

Or a package for Debian, Ubuntu and Raspberry Pi OS (`.deb`), Fedora, RHEL and Rocky (`.rpm`), or the
tarball, from the [releases page](https://github.com/hdhrgit/hdhr-mux-demux/releases):

```sh
sudo apt install ./hdhr-mux_VERSION_ARCH.deb
sudo systemctl enable --now hdhr-mux
```

Then open `http://HOST/ui/`. A new multiplexer finds the devices by itself but does not announce itself
until you turn **Discoverable** on (Settings, General); until then add it to a DVR by its address.

## Documentation

| | |
|---|---|
| [Installing and running](docs/installing.md) | Docker, Compose, Portainer, packages, running by hand, LXC, next to a DVR |
| [Configuration](docs/configuration.md) | where settings come from, the keys, the Settings page, environment variables |
| [How it works](docs/how-it-works.md) | how a request flows, bandwidth, tuner allocation, ATSC 3.0, what is emulated |
| [Lineup, playlist and guide](docs/lineup-and-guide.md) | the lineup, `/playlist.m3u8`, `/epg`, scans |
| [Web interface](docs/web-interface.md) | the pages, appearance |
| [Monitoring and load testing](docs/monitoring.md) | `/metrics`, alerts, `cmd/loadtest` |
| [Tools, hardware checks and limitations](docs/hardware.md) | what was checked on real devices, known limitations |
| [Release notes](docs/releases) | what changed in each version |
