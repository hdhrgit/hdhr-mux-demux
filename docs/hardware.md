# Tools, device behaviour and limitations

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

## Device behaviour

The lineup comes from the devices' own lineups rather than from a scan: they list quiet subchannels
and multiplexes that a scan of the air can miss.

Viewers of several programs per RF channel share one tuner; ATSC 1.0 streams land on tuners 2 and 3
of a 4K device and ATSC 3.0 services on tuners 0 and 1.

A device's own `/lineup/scan` (also what `lineup.post?scan=start` triggers) takes all its tuners:
every `/tunerN/…` variable answers `ERROR: tuner busy` and running streams are cut
(the multiplexer fails them over to another device). It takes about 80 s, and
`libhdhomerun`'s `hdhomerun_config scan` is the one-tuner alternative. The control
syntax is `get /lineup/scan` → `state=complete progress=100% found=161`, `set /lineup/scan start|abort`.

The devices' quirks, all handled: streaming is on port **5004**, not 80;
unknown channels answer `801 Unknown Channel`, busy tuners `804`, tune failures
`806`, no signal `807` after 10 s; ATSC 3.0 needs a program and only tuners
0/1 lock; `lockkey force` only drops the lock and a stale key gives `lock no
longer held`; libhdhomerun discards UDP datagrams that are not exactly 7 TS
packets; scanning needs about 1.5 s for ATSC 3.0 to lock.

## Known limitations

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
