# Configuration

Nearly everything is on the Settings page, so a config file is optional (see [Installing](installing.md)). Where the
configuration comes from, each over the one before: built-in defaults, the config file
(`-config FILE`, or `/etc/hdhr-mux/config.json` if it exists; never written by the program), the
settings saved from the web page (`settings.json` in the state directory), environment variables, and
the command-line options. The log line `config: …` at start says which of them are in use. What can only
be set in the file: `state_dir` (where the state lives), `settings_locked` (makes
the page read-only) and `lineup.sources` (named channel maps for scans).

**Running it by hand.** `hdhr-mux` needs no arguments: the working directory is its state directory
and everything has a default. Port 80 needs privileges (the packaged service has them; run by hand
without them and the web interface moves to **8080** and says so in the log, as it also does if
something else holds port 80). `-http :8080` (or `127.0.0.1:8080`) chooses the web port on purpose;
the Settings page shows it as set by the command line, and `http_listen` there is the way to make it
permanent. Port 80 is not needed to stream (streams are on 5004); it is only the address clients use
when they add the multiplexer by IP, and discovery announces whichever port is in use.

**Not discoverable until chosen.** A new multiplexer does not answer discovery broadcasts or announce
itself over SSDP: it would otherwise show up in every DVR on the network the moment it starts, which is
rarely wanted when it is first being tried (and two multiplexers on one network confuse a DVR). The web
page, and any client that is given its address or device ID, still work. Turn on **Discoverable**
(Settings, General), set `"discoverable": true`, set `HDHOMERUN_DISCOVERABLE=true`, or start it
with `-discoverable` (for that run only). Until one of those is done the log and the Activity and
Settings pages say so.

**No devices found.** The program keeps running instead of exiting: it stays quiet on the network, the
web pages say "Nothing to serve yet" and point at the Settings page (add the devices' addresses under
Devices and press Restart), and it restarts itself when it finds a device.

See [config.example.json](../config.example.json). Notable keys:

* `virtual_tuners` – default one per physical tuner.
* `http_listen` / `stream_listen` / `control_listen` – 80 / 5004 / 65001.
* `upstream.devices` / `allow` / `exclude` – static addresses, an allow list of
  device IDs, and tuners (`DEVICEID/N`) never to touch. Devices shared with
  other applications are fine; busy tuners are skipped.
* `upstream.tuners` – the tuners to use, as `DEVICEID`, `DEVICEID/N` or
  `DEVICEID/N-M`. Only these are used **and numbered** (from 0, in device order), so the
  virtual device has exactly that many tuners.
* `upstream.discover: false` – no broadcast discovery; use only `upstream.devices`.
* `discoverable: false` – hide the multiplexer: it ignores wildcard UDP discovery broadcasts and
  does not announce over SSDP. It still answers requests naming its device ID and any
  discovery request addressed to its own address (over TCP, or over UDP on Linux: a client
  that sends to the address knows where it is), so add it to a client by address.
* `upstream.mask` – `false` to pull whole multiplexes (default `true`: only the PIDs being
  watched, see [Bandwidth](how-it-works.md#bandwidth)).
* `ssdp` – `false` switches SSDP/DLNA announcements and search answers off. `ssdp_ipv4`
  (default `true`) and `ssdp_ipv6` (default `false`) switch one IP family on or off. Like real
  HomeRuns the mux announces every 10 minutes on `239.255.255.250` and, with IPv6 on, on
  `ff02::c`, sending every notification twice (about 22 ms apart), and answers searches on
  each enabled family. The IPv6 side uses the link-local
  address of the interface carrying the default IPv4 route, and is skipped (with a log line)
  when that interface has none. Some TVs (an LG webOS one) only
  list the mux after hearing the startup announcement.
  `ssdp_atsc3` (default `false`) also announces the ATSC 3.0 primary device
  (`urn:schemas-atsc.org:device:primaryDevice:1.0`, description on `/atsc3/device.xml`: identity
  only, no services, as on the real 4K devices), which makes a burst 8 targets x 2 = 16 packets
  per IP family, like theirs (5 x 2 = 10 without it). The conservative defaults keep what is
  known to work: IPv4 and the media server only.
* `upstream.mode` – `http` (default) or `rtp`/`udp` (device pushes to this
  host; needs inbound UDP).
* `linger` (5 s, `0` to disable), `client_buffer_mb` (16), `idle_release`.
* `signal_settle` (2 s, `0` = off): a tuner's symbol quality starts at 0 and climbs for a second or two after it locks, and Channels DVR counts each such second as a signal error (`sigerr` in its log). For this long after a stream starts, a zero is shown to clients (`status`, `debug`, `status.json`) as that tuner's last good reading; the activity page and `/metrics` keep the device's own figures. The status of a new stream is also refreshed every 200 ms for its first 8 s, so what clients see tracks the device.
* `log_requests` (false) writes every client request to the log; `log_requests_on` (empty = all) limits that to some of the ports: `web` (the web / API port), `udp` and `tcp` (the control port, 65001) and `stream` (5004), separated by commas, e.g. `"web,tcp"`. Both apply while running (Settings, Diagnostics).
* `lineup.atsc3` (true): list the ATSC 3.0 channels in the lineup, the playlist and the guide. `false` leaves them out of those listings only: a client that asks for one by number is still served. Many are encrypted (DRM) and clients skip those themselves. An own scan (`lineup.scan_mode: own`) says in the log when ATSC 3.0 channels could not be scanned because no 3.0-capable tuner (0 and 1 of a 4K device) was free; the device scan (the default) covers both kinds.
* `auto_join_direct` (true): an `/auto/` request joins a tuner a direct client (tvheadend, `/tunerN/`) already holds on the same channel.
* `auto_join_other_locations` (false): the same station can be received on more than one RF channel (a second antenna or feed, a translator) while a device's lineup lists only one of them. An `/auto/` request for a channel that a session already open carries on another RF channel (found from that multiplex's own channel table) rides that session instead of taking a tuner. Never opens a tuner somewhere other than the lineup's place. Off by default: only the lineup's RF channel is used until you tick another on the Advanced tab; `true` uses every place a channel is received on.
* `evict_scans` (true): when every tuner is busy, an `/auto/` request takes one from a control client that is only collecting tables (tvheadend's idle guide scan): its PID filter has completed no programme for 10 s. The scanner finds its data stopped and its lock gone. A client that is watching a programme, or has a viewer on its tuner, is never touched. Set to `false` to leave direct clients' tuners alone even then.
* `stream_ttl` (3, at most 8): the IP TTL of the video the multiplexer sends (HTTP streams and UDP/RTP targets). Real devices send their streams with a TTL of 3 so they cannot travel beyond a couple of routers; the multiplexer does the same, so a client has to be within two router hops of it. Larger values are quietly cut to 8 and `0` means the default: a device pushes a lot of bandwidth and must not be usable from afar, so use a VPN for remote viewing. For the same reason a UDP/RTP `target` (a control request cannot be authenticated, and its source can be forged) must be an address on a private IPv4 network (RFC 1918), this machine or link-local; any other target is refused.

## Settings page (`/ui/#/settings`)

The common settings can be changed from the web interface instead of editing JSON: name, device ID,
listen addresses, SSDP, which devices and tuners to use, streaming and guide timing, diagnostics.
Changes are saved to `settings.json` in the state directory, in the same shape as the config file, and
laid over it: **config file < `settings.json` < environment variables** (a setting an environment
variable controls is shown as such and cannot be edited on the page). The config file itself is never
written, so it may be read-only or managed by something else. **Apply** saves the changes and puts
into effect everything that can change while running (sharing options, the guide and lineup timing,
linger, the client buffer, request logging, the TTL, the rescan mode); the button is inactive until
something has changed. A few settings are read once, at start (name, device ID, listen addresses,
SSDP, which devices and tuners to use): they are marked "restart" on the page, and after applying
them the page says they are waiting. **Restart** is a separate button: the multiplexer stops cleanly
and starts itself again in the same process, so it works in a container started without
`--restart` and under any supervisor (if that is not possible it exits with status 3 instead).

The page also shows where the state directory is and warns when it will not survive (see [Docker](installing.md)).

Because it can reconfigure the multiplexer, saving is only accepted from private-network, loopback or
link-local addresses, requires a custom request header (so another web site cannot send it from a
browser on your network) and can be switched off with `"settings_locked": true` in the config file.
There is no password: do not expose the web port to the internet.

If a saved setting stops the multiplexer from starting, delete `settings.json` from the state directory
or start with `-ignore-settings`.

## Environment variables

These override the file and use the same names as the `hdhomerunsignal` tool where the
meaning is the same:

| Variable | Effect |
|---|---|
| `HDHOMERUN_DEVICES` | comma-separated IPs/hostnames of devices, added to `upstream.devices` |
| `HDHOMERUN_DISABLE_DISCOVERY=true` | no auto-discovery; only the listed devices |
| `HDHOMERUN_TUNERS` | comma-separated tuners to use (`DEVICEID`, `DEVICEID/N`, `DEVICEID/N-M`); replaces `upstream.tuners` |
| `HDHOMERUN_DISCOVERABLE=true` | answer discovery and announce over SSDP (off until chosen; `false` makes the choice explicit) |

Changing the tuner list renumbers the virtual tuners.

