# Web interface

Browsers that open the web port are sent to `/ui/` (the plain page real devices serve stays on
`/?classic=1` and for non-browser clients). It needs no setup and no internet access: the files
are embedded in the binary.

* **Activity** – who is watching what, live (every 2 s): per tuner the physical tuner behind it,
  RF channel, signal / quality / symbol meters, incoming bitrate history, and for every viewer
  the client address, the channel, the programme on now with a progress bar and what is next
  (from the guide), how long they have been watching, their bitrate, how full their buffer is
  and any dropped data. Tuners used by other applications are marked, free tuners listed, and
  background work (guide collection, tuners held for a quick return) shown separately.
  Each physical tuner id links to that device's own status page for the tuner
  (`http://DEVICE/tuners.html?page=tunerN`). Slots a station has only broadcast as "To Be
  Announced" are shown as such (they are real PSIP data, but not titles; DVR software with
  another guide source may know better).
  A **This machine** section shows how the multiplexer itself is doing: its CPU use over the last
  minute (of one core, and how many cores there are), resident memory (and the container or service
  limit, if any), the host load, the CPU temperature where the system reports it (and whether a
  Raspberry Pi says it is under-voltage or throttled), goroutines and open files. It only reports;
  there are no alerts. What counts as too much is for the person running it to decide, and the same
  figures, plus `hdhrmux_dropped_batches_total`, are in `/metrics` (`process_*`, `go_*`) for Prometheus
  or Netdata.
* **Lineup** – every channel with what is on now and next, search, filters (HD, being watched,
  favourites), and a button that copies the channel's stream URL. The star column
  flips a channel between enabled, favourite and disabled (as on a device); it changes this
  multiplexer's copy only, and a client doing the same (`lineup.post?favorite=+7.1`, `-7.1`, `x7.1`) has the same effect.
  Disabled channels are left out of the lineup, playlist and guide and are not tuned.
* **Advanced** – the channels that are received on more than one RF channel. Most are on one RF channel; some stations are carried
  on two or more (2.1 to 2.4 and 4.1 are on 545 and 575 MHz in the author's market, 20.1 on 515 and 581,
  24.1 on 473 and 533). The page lists those first, with a row for every place: the name the multiplex
  gives the channel there, its program number, how well it is received (signal strength, quality and
  symbol quality, with the device that measured it) and how many hours ahead the guide on that RF
  channel goes (or "none": 533 MHz sends no guide at all). The place the devices' lineups use is marked.
  It is learned from the streams and guide passes that tune the
  channels anyway and kept in `<state_dir>/rfmap.json`. The devices' lineups give every station one
  place, so each guide pass also listens for a few seconds to the broadcast RF channels 2-36 that no
  lineup channel is placed on (`us-bcast` only), which is how a station on a second RF channel is found;
  the ones that carry something are then collected like the rest. It fills in over the first guide pass
  after a new start, and a new transmitter shows up within `guide.refresh`.
  The page is also where the choices are made (saved in `<state_dir>/channels.json`; they apply at once,
  and need the same permission as the settings page):
  * **Video** (a checkbox per RF channel): where `/auto/` may tune the channel. Several can be on, but not none: whether a channel
    exists at all is decided in the Lineup (the star column). An RF channel that every channel on it has switched off is not tuned for the guide.
  * **First**: the place tried first. A request first rides a session that is already open at any allowed
    place, then tunes the first choice, then the others, one after the other until one works. Without a
    choice the place from the devices' lineups is first. Other places are used only when
    `auto_join_other_locations` is on (or the place is the chosen first).
  * **EPG**: which RF channel the guide comes from. Without a choice it is the one that goes furthest
    ahead. Guides of different RF channels are never merged.
  Each choice has a "use the lineup's place" / "use automatic" link to undo it.
  **Merged channels** (same tab, saved in `<state_dir>/merges.json`): groups of channels you say carry
  the same programme (11.1 and 11.3, say). For the tuners they are one: a request for any of them
  is served by a stream that already carries any of them, so fewer tuners are used; with none open it
  tunes the requested channel at its own place first, then the places of the others. Which of them are
  listed is the Lineup's business: you can disable all but one and requests for it still ride the others.
  Nothing is merged on its own, and the multiplexer does not check that they really are the same.
* **Appearance** (the half-moon button) – System / Light / Dark, and a choice of theme for each:
  Paper, Mist, Mint, Lilac (light) and Slate, Midnight, Dusk, Ember (dark). None of them is pure white
  or pure black. *System* follows the device and switches between your light and dark theme by
  itself; the choice is kept in the browser. `?theme=midnight` on the URL forces a theme.

The data behind it is plain JSON: `/ui/api/activity` and `/ui/api/lineup`.

![Activity, Midnight theme](docs/ui-activity-midnight.png)
![Activity, Paper theme](docs/ui-activity-paper.png)
![Lineup, Mist theme](docs/ui-lineup-mist.png)

`cmd/uidemo` runs the interface against simulated devices and viewers (no hardware needed);
`-ui-dir internal/vdev/ui` serves the files from disk so edits show on reload.

