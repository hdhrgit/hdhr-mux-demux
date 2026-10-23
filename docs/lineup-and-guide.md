# Lineup, playlist and guide

## The lineup: devices decide what exists and where

The multiplexer does **not** scan to build its channel list. The channel list is
the union of the physical devices' own `/lineup.json?tuning` (re-read every
`lineup.refresh`, default 10 min, and on a rescan), so it matches what the firmware keeps,
including quiet subchannels a scan can miss. Besides names, codecs and HD/DRM flags, a device
answers `?tuning` with where each channel is on the air (frequency, modulation, program number and
transport stream ID), and the lineup follows that: the first device that lists a channel decides, and
a stream seen on another RF channel does not move it, so a station carried on two RF channels is not
flipped back and forth. Nothing is tuned to learn it. The multiplexer's own `lineup.json?tuning`
answers the same way. The placement is remembered in `<state_dir>/lineup.json`. Where a device
does not say (older firmware), it is learned:

* **From a running stream** (only for a channel the devices did not place). A tuner's program list
  (`streaminfo`) names every
  channel of its multiplex, so watching one channel teaches the whole multiplex.
* **On first use.** For a channel not yet located, the multiplexer tunes it by
  name (`/tunerN/vX.Y`, which the device resolves from its own lineup) on a free
  tuner, reads the multiplex and lets the tuner go again; that costs one extra tune
  the first time a multiplex is used. If the first device does not know the channel
  the others are asked. Concurrent requests share one probe.
* **From an explicit scan** (below), which locates everything at once.

### Playlist (`/playlist.m3u8`)

For VLC, Kodi, Jellyfin, Emby, TiviMate and other players or IPTV apps that cannot use `lineup.json`
or the HDHomeRun protocol, the lineup is also served as an extended M3U playlist:

```
#EXTM3U url-tvg="http://MUX/epg" x-tvg-url="http://MUX/epg"
#EXTINF:-1 tvg-id="7.1" tvg-chno="7.1" tvg-name="KGO-HD" group-title="Antenna",KGO-HD
http://MUX:5004/auto/v7.1
```

`/playlist.m3u8`, `/playlist.m3u`, `/lineup.m3u` and `/lineup.m3u8` are the same UTF-8 file. It is a
playlist of channels, not HLS: each URL is the usual MPEG transport stream over HTTP, served from a
shared tuner. `tvg-id` is the guide number, which is also the channel id in the XMLTV guide on
`/epg` (the header points players at it), so the programme guide lines up. Channels with DRM are left out. Channels that are not HD carry `group-id="SD"`, which is what
Channels DVR looks for: without it every channel of an M3U source is labelled HD.

Options: `?hd=1` only HD, `?favorites=1` only favourites, `?atsc3=0` leave out ATSC 3.0 channels,
`?group=` sets `group-title`: `source` (default, e.g. "Antenna"), `rf` ("545 MHz"), `quality`
(HD / SD) or `none`.

### Programme guide (`/epg`)

ATSC 1.0 stations broadcast their schedule in every multiplex (PSIP: channel table, event
titles and times, descriptions). The multiplexer reads it from the streams it already pulls:

* **From running streams.** Any stream of a whole multiplex hands over the guide of every
  channel on it every few seconds, so watching one channel updates its neighbours.
* **From a pass over the idle multiplexes.** After start-up (90 s) and then every
  `guide.refresh` (default 1 h, 0 turns it off) it listens to each ATSC 1.0 multiplex in the lineup for
  `guide.hold` (45 s), `guide.parallel` (3) at a time, on spare tuners. A pass never keeps a
  tuner from a viewer: it gives it up at once. It also listens for a few seconds to the other broadcast RF channels (see the Advanced tab). `POST /epg/refresh` starts one now.
  On a new installation no channel's place on the air is known yet (the lineup comes from the devices
  and has no frequencies), so the first pass begins by finding out: it looks for one channel of each
  multiplex by name, which teaches every channel on that multiplex, about one tuning per multiplex and
  only once (the log says `guide: finding where the channels are on the air`).

It is served as **XMLTV** on `/epg` (also `/epg.xml`) and as JSON on `/epg.json`, for the
channels of the lineup. Options: `?hours=N` ahead (default 72), `?channel=7.1` (repeatable),
`?all=1` to list channels without a guide. XMLTV channel ids are the guide numbers (`7.1`);
times are UTC. The guide is kept in `<state_dir>/epg.json`.

**How far ahead?** That is up to each station, which fills as many 3-hour EIT tables as it likes.
On the test antenna the channels reach a median of 9 hours ahead (84 of 123 channels 6-12 h), 31
channels 12-48 h, and the big networks' stations about 1.5 to 3 days (the longest 70 h). A
client that reads the guide hourly therefore always has several hours in hand; the near
term is refreshed by the passes and by any running stream, the far end only changes slowly.

**Unlocked passes.** While a pass listens, the device holds the tuner locked for the HTTP
stream, so another application asking for that tuner is told it is in use. With
`guide.unlocked` (on by default) the pass drops that lock as soon as the stream runs (`lockkey force`, the
stream keeps flowing). Anyone may then take the tuner (checked on real devices: their request
succeeds and our stream stops), the pass notices within a second, lets go without touching what
the new user set up, and carries on with the next multiplex when a tuner is free. It still pulls
over HTTP, so it works through Docker NAT.

Limits: ATSC 3.0 services carry no PSIP, so they have no guide; Huffman-compressed text (rare)
is not decoded; the broadcasts usually cover a day or two ahead.

### Scan (explicit only)

`POST /lineup.post?scan=start` (what a client's "rescan" does; Channels DVR does it about
once a day) or `set /lineup/scan start` is **handed to one device's own scan** (`lineup.scan_mode:
"device"`, the default). The multiplexer picks a device nobody is using (none of its tuners has a
viewer of ours or another application; tuners only held for a quick return are let go), preferring
one with ATSC 3.0 tuners because it finds both kinds of channel, marks all its tuners as scanning
(they answer `804` and `/auto/` goes around them), starts `POST /lineup.post?scan=start` on the
device, follows its `lineup_status.json` (shown as the multiplexer's own progress), and when it ends
re-reads the devices' lineups. If every device is in use the request is refused (`409`): the client
tries again later. The firmware scan takes all of a device's tuners and cuts whatever they stream,
which is why it only ever runs on an idle device. Where a scan can change a device's lineup it
does so on that device, and the other devices keep theirs.

`lineup.scan_mode: "own"` keeps the multiplexer's own scan instead: it uses **every free tuner of
every device** at once, each taking the next unscanned RF channel, never evicts a viewer, is capped by
`lineup.scan_tuners_per_device`, teaches the multiplexer where channels are (and adds any the
devices do not list) and re-reads the lineups. `scan_on_start` is off by default.

