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

## How a request flows

`GET http://MUX:5004/auto/v2.4`

1. The lineup (built by the scan) says 2.4 is program 6 on 545 MHz, ATSC 1.0.
2. `/auto/` first looks for a tuner already carrying the **same RF channel** (or ATSC 3.0
   service) and joins it with its own program filter; only otherwise does it open a free
   tuner. `/tunerN/...` asks for that exact tuner (joining if it carries the same stream,
   else `804 Tuner In Use`). `805 All Tuners In Use` if nothing can serve the request.
3. If no tuner carries 545 MHz yet, the best free one is chosen and opened with
   `GET http://DEVICE:5004/tunerM/ch545000000` (the whole multiplex).
4. The viewer's filter keeps PAT (rewritten to just program 6), PMT, PCR and
   the elementary streams; everything else is dropped.
5. Data goes to the client from a per-client buffer (16 MiB by default); a slow
   client loses data, never holds the others up.
6. When the last viewer leaves, the tuner is released (after `linger`).

## Tuner numbering and allocation

* Tuners are numbered `0..N-1` across devices in a stable order (device ID
  order the first time, then remembered in `<state_dir>/devices.json`, so a
  device being offline or added later never renumbers the others). Device 0's
  tuners are 0-3, device 1's are 4-7, and so on.
* **Strict 1:1.** Virtual tuner *N* is physical tuner *N*: whatever it streams
  comes from that tuner, and status queries for it (control protocol or
  `status.json`) describe it. Several viewers of the same RF channel share one
  virtual tuner (each with its own program filter); that is how tuners are saved.
* **`/auto/` maximises free tuners.** It first reuses a tuner that already carries
  the channel (busy, or still lingering on it); only if there is none does it open
  a new one, choosing the one that hurts future requests least:
  * ATSC 1.0 goes to tuners that **cannot** do ATSC 3.0 first (tuners 2 and 3 on
    a 4K device); the 3.0-capable ones are the last resort.
  * ATSC 3.0 goes only to capable tuners (0 and 1 on a 4K device, learned from
    the devices' own `806 Tune Failed` answers).
  * Tuners used by other applications (a DVR sharing the devices) or holding a
    lingering stream come last. A tuner another application is using is never
    taken: `/tunerN/` for it answers `804` and its status shows what the other
    application is doing.
* `/tunerN/` asks for exactly that tuner. If it is busy but already carries the
  same stream, the request joins it; otherwise `804 Tuner In Use`.
* With `linger` (default 5 s) a tuner stays tuned after its last viewer leaves
  so flipping back is instant, but it is **given up immediately** when anyone
  needs a tuner, so channel flipping never runs out.
* If a physical tuner stops delivering (connection drops, stalls, is cleared,
  or the device disappears) its stream is re-opened on the same tuner, and only
  if that fails moved to another one, without viewers reconnecting.

### ATSC 3.0

ATSC 3.0 tuners do not deliver the whole multiplex: the device only streams
the one service you select (`/tunerN/chFREQ-SERVICE`), re-muxed into its own
small transport stream. This was checked on real hardware with both HTTP and
the control protocol (no whole-multiplex stream, no list of services). So on
3.0 channels **each service needs its own physical tuner** (identical requests
still share one) and the multiplexer does no demuxing there.

## What is emulated

| Area | Support |
|---|---|
| UDP discovery on 65001 (wildcard + specific device ID) | yes |
| TCP (and UDP) get/set control protocol, `help` | yes |
| `/tunerN/` `channel` `channelmap` `filter` `program` `target` `status` `streaminfo` `lockkey` `vchannel` `debug` | yes |
| `/sys/model` `hwmodel` `version` `features` `copyright`, `/lineup/location`, `/ir/target` | yes (static values) |
| `target` as `udp://` or `rtp://` (7 TS packets per datagram, as libhdhomerun requires) | yes |
| Lockkeys (`lockkey`, `force`, `lock no longer held`) | yes |
| Port 80: `discover.json`, `lineup.json`, `lineup.xml`, `lineup_status.json`, `lineup.post?scan=…`, `status.json`, `device.xml`, `tuners.html` (every tuner with its channels, clients and their IPs), `lineup.html` | yes |
| Port 5004: `/auto/vX.Y`, `/tunerN/vX.Y`, `/auto/chFREQ[-PROGRAM]`, `?duration=`, real error headers (`801`, `804`–`807`) | yes |
| SSDP | yes (default interface only) |
| Lineup from the devices' own lineups, RF locations learned from streams / first use; explicit scan (`us-bcast`, `us-cable`, `us-hrc`, ATSC 1.0 and 3.0) | yes |
| `/lineup/scan` control variable (`get` → `state=… progress=…% found=…`, `set start` / `abort`) | yes |
| Transcoding, other channel maps, firmware upgrade, DVR/storage | no |

## The lineup: devices decide what exists, streams teach where it is

The multiplexer does **not** scan to build its channel list. The channel list is
the union of the physical devices' own `/lineup.json` (names, codecs, HD/DRM
flags; re-read every `lineup.refresh`, default 10 min, and on a rescan), so it
matches what the firmware keeps, including quiet subchannels a scan can miss.
What the devices' lineups do not say is where a channel is on the air, which
sharing needs. That is learned, and remembered in `<state_dir>/lineup.json`:

* **From every running stream.** A tuner's program list (`streaminfo`) names every
  channel of its multiplex, so watching one channel teaches the whole multiplex.
* **On first use.** For a channel not yet located, the multiplexer tunes it by
  name (`/tunerN/vX.Y`, which the device resolves from its own lineup) on a free
  tuner, reads the multiplex and lets the tuner go again; that costs one extra tune
  the first time a multiplex is used. If the first device does not know the channel
  the others are asked. Concurrent requests share one probe.
* **From an explicit scan** (below), which locates everything at once.

### Scan (explicit only)

`POST /lineup.post?scan=start` (what a client's "rescan" does) or
`set /lineup/scan start` runs a scan that teaches the locations of channels (and adds
any the devices do not list) and re-reads the devices' lineups. It does not trigger the
firmware's own scan, which takes all of a device's tuners and cuts running streams. It uses
**every free tuner of every device** at once, each taking the next unscanned RF channel; it
never evicts a viewer, and `lineup.scan_tuners_per_device` caps it. Tuners that cannot do
ATSC 3.0 hand channels that show signal but do not lock to the capable ones. Tuners held by
the scan refuse streams at once (`804`) and `/auto/` goes around them. `scan_on_start`
is off by default.

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

### Configuration

See [config.example.json](config.example.json). Notable keys:

* `virtual_tuners` – default one per physical tuner.
* `http_listen` / `stream_listen` / `control_listen` – 80 / 5004 / 65001.
* `upstream.devices` / `allow` / `exclude` – static addresses, an allow list of
  device IDs, and tuners (`DEVICEID/N`) never to touch. Devices shared with
  other applications are fine; busy tuners are skipped.
* `upstream.tuners` – the tuners to use, as `DEVICEID`, `DEVICEID/N` or
  `DEVICEID/N-M`. Only these are used **and numbered** (from 0, in device order), so the
  virtual device has exactly that many tuners.
* `upstream.discover: false` – no broadcast discovery; use only `upstream.devices`.
* `discoverable: false` – hide the multiplexer: it ignores wildcard UDP discovery and
  does not announce over SSDP. It still answers requests naming its device ID and direct
  connections, so add it to a client by address.
* `upstream.mode` – `http` (default) or `rtp`/`udp` (device pushes to this
  host; needs inbound UDP).
* `linger` (5 s, `-1s` to disable), `client_buffer_mb` (16), `idle_release`.

### Environment variables

These override the file and use the same names as the `hdhomerunsignal` tool where the
meaning is the same:

| Variable | Effect |
|---|---|
| `HDHOMERUN_DEVICES` | comma-separated IPs/hostnames of devices, added to `upstream.devices` |
| `HDHOMERUN_DISABLE_DISCOVERY=true` | no auto-discovery; only the listed devices |
| `HDHOMERUN_TUNERS` | comma-separated tuners to use (`DEVICEID`, `DEVICEID/N`, `DEVICEID/N-M`); replaces `upstream.tuners` |
| `HDHOMERUN_DISCOVERABLE=false` | do not let clients discover the multiplexer |

Changing the tuner list renumbers the virtual tuners.

## Tools

* `cmd/hdhrctl` – `discover`, `get`, `set` (`HDHR_LOCKKEY=n` sends a lockkey),
  `save N SECONDS` against a physical device or the multiplexer.
* `cmd/tscheck` – reads a transport stream on stdin and reports PIDs,
  continuity errors and the programs in the PAT/PMT.
* `cmd/fakehdhr` / `internal/fakehdhr` – a simulated device (control protocol,
  port 5004 HTTP streaming, RTP) that mimics the real quirks listed below. The
  end-to-end tests in `internal/e2e` run the whole stack against it:

```sh
CGO_ENABLED=1 scripts/go.sh test -race ./...
```

## Verified on real hardware

Tested against an HDHR5-4K and two HDFX-4K (firmware 20260326, ATSC 1.0 + 3.0
antenna) that share the network with a DVR, using `curl`, `hdhrctl`, `tscheck`
and the official `hdhomerun_config` (Debian package, run in a container):

* A full 68-channel scan on 14 free tuners of four devices takes about 12 s. The devices' own lineups hold 164 channels, more than that scan found (it dropped quiet subchannels and a multiplex without a tsid, now fixed), which is why the lineup comes from the devices.
* Viewers of several programs per RF channel share one tuner; ATSC 1.0 streams
  land on tuners 2/3, 3.0 services on tuners 0/1; minutes-long soaks show no
  sync or continuity errors; tuners are released cleanly.
* The official tool can discover the multiplexer, read/set tuners 0-11, and
  `save` from it with no network errors (same result as from a real device).

A device's own `/lineup/scan` (also what `lineup.post?scan=start` triggers) takes all its tuners:
every `/tunerN/…` variable answers `ERROR: tuner busy` and running streams are cut
(the multiplexer fails them over to another device). It takes about 80 s, and
`libhdhomerun`'s `hdhomerun_config scan` is the one-tuner alternative. The control
syntax is `get /lineup/scan` → `state=complete progress=100% found=161`, `set /lineup/scan start|abort`.

What the hardware taught us, all handled: streaming is on port **5004**, not 80;
unknown channels answer `801 Unknown Channel`, busy tuners `804`, tune failures
`806`, no signal `807` after 10 s; ATSC 3.0 needs a program and only tuners
0/1 lock; `lockkey force` only drops the lock and a stale key gives `lock no
longer held`; libhdhomerun discards UDP datagrams that are not exactly 7 TS
packets; scanning needs about 1.5 s for ATSC 3.0 to lock.

## Known limitations

* Not tested with Plex, Jellyfin or Channels DVR themselves, nor SSDP on the LAN.
* Setting `channel` through the control protocol waits for the stream to start
  (up to about 10 s on a channel with no signal) instead of returning at once
  like a real tuner, so scanning *through* the control protocol is slow; use
  the HTTP `lineup.post?scan=start` scan. HTTP streaming is unaffected.
* New devices that appear after start-up get tuner numbers but are only used
  after a restart to be exposed as virtual tuners.
* Only the `us-bcast`, `us-cable` and `us-hrc` channel maps are built in.
  Frequencies can always be tuned directly (`/auto/ch545000000-6`).
* Program filtering passes PAT (rewritten), PMT, PCR and elementary streams;
  PSIP/EPG tables are not forwarded to program-filtered viewers.
* Encrypted (DRM) ATSC 3.0 services are listed but flagged `DRM`.
