# Tools, real-hardware checks and limitations

## Tools

* `cmd/hdhrctl` (a development tool, not in the packages: `go build ./cmd/hdhrctl`) – `discover`, `get`, `set` (`HDHR_LOCKKEY=n` sends a lockkey),
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

* Clients: tested with Channels DVR (discovery, lineup, live viewing of several
  channels at once, signal display), VLC and the official `hdhomerun_config`. Not
  tested with Plex or Jellyfin, nor with scheduled recordings in Channels DVR.
* SSDP/DLNA: the multiplexer answers searches and announces itself on the LAN, and other UPnP
  clients fetch its description. An LG webOS TV does not list it as a video source yet, although
  it lists the real tuners (open: the TV never requests the description).
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
