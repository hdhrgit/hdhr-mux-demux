# How it works

## How a request flows

`GET http://MUX:5004/auto/v2.4`

1. The lineup (built by the scan) says 2.4 is program 6 on 545 MHz, ATSC 1.0.
2. `/auto/` first looks for a tuner an earlier `/auto/` request opened on the **same RF channel**
   (or ATSC 3.0 service) and joins it with its own program filter; only otherwise does it open a
   free tuner. `/tunerN/...` asks for that exact tuner and gets it, or `804 Tuner In Use` if it is busy,
   even with the same channel (as on a real device). `805 All Tuners In Use` if nothing can serve
   an `/auto/` request.
3. If no tuner carries 545 MHz yet, the best free one is chosen and opened with
   `GET http://DEVICE:5004/tunerM/ch545000000` (the whole multiplex).
4. The viewer's filter keeps PAT (rewritten to just program 6), PMT, PCR and
   the elementary streams; everything else is dropped.
5. Once the multiplex's tables are known, the device is told (through the tuner's PID
   filter) to send only what is needed: see *Bandwidth* below.
6. Data goes to the client from a per-client buffer (16 MiB by default); a slow
   client loses data, never holds the others up.
7. When the last viewer leaves, the tuner is released (after `linger`).

### Bandwidth

A tuner delivers its whole multiplex, about 19 Mbit/s, however few programs are watched.
After the HTTP request has started, the mux takes the tuner's lock over (`lockkey force`, then
a lock key of its own; the stream keeps flowing, checked on real devices) and sets the tuner's
`filter` to the PAT, the PMTs, the PSIP guide tables and the audio, video and PCR PIDs of
programs somebody watches. With one HD program on a multiplex that is roughly 6-9 Mbit/s
over the LAN instead of 19; a lingering stream nobody watches drops to the tables alone.

* Until the PAT is known (a few seconds) and whenever a viewer wants the whole multiplex
  (`hdhomerun_config save` without a program, a raw PID filter covering everything), the
  filter stays open.
* A new viewer's PIDs are added at once; PIDs nobody needs any more are dropped after 5 s, so
  flipping back and forth does not make the filter flap.
* Guide collection is unaffected: the PSIP PIDs always stay in the filter, so the programmes of
  unwatched channels keep arriving.
* When the stream ends the mux puts the filter back to everything and releases the lock; a
  lock left behind by a crash expires on the device by itself (under a minute, measured).
* Not applied to ATSC 3.0 (the device already sends one service) and to `guide.unlocked`
  passes (whoever takes such a tuner over would inherit the filter). A device that refuses the
  lock swap is left unmasked. `upstream.mask: false` switches the feature off.

## Tuner numbering and allocation

* Tuners are numbered `0..N-1` across devices in a stable order (device ID
  order the first time, then remembered in `<state_dir>/devices.json`, so a
  device being offline or added later never renumbers the others). Device 0's
  tuners are 0-3, device 1's are 4-7, and so on.
* **Strict 1:1.** Virtual tuner *N* is physical tuner *N*: whatever it streams
  comes from that tuner, and status queries for it (control protocol or
  `status.json`) describe it. Several `/auto/` viewers of the same RF channel share one
  virtual tuner (each with its own program filter); that is how tuners are saved.
* **Direct requests are trusted.** A client that asks for a tuner by number (HTTP `/tunerN/`, or the
  control protocol: tvheadend, `hdhomerun_config`, which lock it and may receive over UDP/RTP) gets
  that tuner and keeps it, as on a real HomeRun; nobody else is attached to it. A direct client
  never joins anybody: it is refused (`804`, or "resource locked") when its tuner is busy.
  `/auto/` requests (Channels DVR, the playlist, DLNA) join each other, and with
  **`auto_join_direct`** (default `true`) they also join a tuner a direct client already holds on
  the same RF channel instead of taking another one. The viewer gets its own programme whatever
  the direct client's PID filter does, and keeps the tuner if that client lets go of it (then it is
  released with the last viewer). While `/auto/` viewers are on it the direct client cannot retune
  it (it is told the resource is locked), as on a real device. Set it to `false` to keep direct
  clients' tuners strictly to themselves.
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
* `/tunerN/` asks for exactly that tuner: it gets it or `804 Tuner In Use`.
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
| M3U/M3U8 playlist of the lineup on `/playlist.m3u8` (also `/playlist.m3u`, `/lineup.m3u`, `/lineup.m3u8`), for players that cannot use `lineup.json` or the HDHomeRun protocol | yes (extension, see [Playlist](lineup-and-guide.md#playlist-playlistm3u8)) |
| Port 5004: `/auto/vX.Y`, `/tunerN/vX.Y`, `/auto/chFREQ[-PROGRAM]`, `?duration=`, real error headers (`801`, `804`–`807`) | yes |
| DLNA/UPnP media server on `/dms` (SSDP announcements and search answers, `device.xml`, ContentDirectory with the `Favorites` and `Channels` containers, ConnectionManager), so TVs list the multiplexer as a video source and play its channels | yes (default interface only) |
| Programme guide from the broadcasts (PSIP), XMLTV on `/epg`, JSON on `/epg.json` | yes (ATSC 1.0 only) |
| Lineup from the devices' own lineups, RF locations learned from streams / first use; explicit scan (`us-bcast`, `us-cable`, `us-hrc`, ATSC 1.0 and 3.0) | yes |
| `/lineup/scan` control variable (`get` → `state=… progress=…% found=…`, `set start` / `abort`) | yes |
| Transcoding, other channel maps, firmware upgrade, DVR/storage | no |

