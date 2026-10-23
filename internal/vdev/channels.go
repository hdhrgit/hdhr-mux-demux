package vdev

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"hdhrmux/internal/chanprefs"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/rfmap"
)

// The Channels page: for every channel, where on the air it is received. Most
// channels are on one RF channel; some stations are carried on two or more, and
// then there is a choice to make, which a person makes here: which RF channels the
// channel may be tuned on, which one to try first, and which one's programme guide
// to use. Each place shows what is known about it: the name the multiplex gives the
// channel there, how well it is received and how far ahead its guide goes.

// place is one RF channel a channel can be tuned on.
type place struct {
	freq    uint64
	program uint16
	service bool // ATSC 3.0
}

// placesOf lists where a channel may be tuned, in the order to try: the one chosen
// as first, then the one the devices' lineups use, then the rest by signal quality.
// RF channels switched off for the channel are left out, unless that leaves none (whether
// a channel exists at all is decided in the Lineup, not here). An ATSC 3.0 service is
// only ever at its own place.
func (d *Device) placesOf(e lineup.Entry) []place {
	ps := d.ownPlaces(e)
	// channels merged with this one carry the same thing: their places are as good
	for _, n := range d.merges.GroupOf(e.GuideNumber) {
		if n == e.GuideNumber {
			continue
		}
		m, ok := d.entryAny(n) // a merged channel that is not listed still counts
		if !ok {
			continue
		}
		for _, p := range d.ownPlaces(m) {
			if !slices.Contains(ps, p) {
				ps = append(ps, p)
			}
		}
	}
	return ps
}

// ownPlaces is placesOf without the merged channels.
func (d *Device) ownPlaces(e lineup.Entry) []place {
	if ps := d.placesWith(e, true); len(ps) > 0 {
		return ps
	}
	return d.placesWith(e, false)
}

func (d *Device) placesWith(e lineup.Entry, useOff bool) []place {
	pref := d.prefs.Get(e.GuideNumber)
	type cand struct {
		place
		snq    int
		lineup bool
	}
	var cs []cand
	seen := map[uint64]bool{}
	add := func(c cand) {
		if c.freq != 0 && !seen[c.freq] && !(useOff && pref.IsOff(c.freq)) {
			seen[c.freq] = true
			cs = append(cs, c)
		}
	}
	atsc3 := e.Modulation == "atsc3"
	if e.Frequency != 0 {
		add(cand{place: place{e.Frequency, e.Program, atsc3}, lineup: true})
	}
	// the other places the channel is received on are used unless auto_join_other_locations is
	// off (a place chosen as first is always used)
	if !atsc3 {
		others := d.lv().AutoJoinOtherLocations
		for _, r := range d.rf.Snapshot() {
			if !others && r.Frequency != pref.Video && !pref.IsOn(r.Frequency) {
				continue
			}
			if r.Modulation == "atsc3" {
				continue
			}
			for _, p := range r.Programs {
				if p.Channel == e.GuideNumber {
					snq := -1
					for _, s := range r.Signal {
						if s.SNQ > snq {
							snq = s.SNQ
						}
					}
					add(cand{place: place{r.Frequency, p.Number, false}, snq: snq})
				}
			}
		}
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if (a.freq == pref.Video) != (b.freq == pref.Video) {
			return a.freq == pref.Video
		}
		if a.lineup != b.lineup {
			return a.lineup
		}
		return a.snq > b.snq
	})
	out := make([]place, len(cs))
	for i, c := range cs {
		out[i] = c.place
	}
	return out
}

type cpSignal struct {
	SS     int    `json:"ss"`
	SNQ    int    `json:"snq"`
	SEQ    int    `json:"seq"`
	Device string `json:"device"` // the device that measured it
	At     int64  `json:"at"`     // unix seconds
}

type cpLocation struct {
	Frequency  uint64    `json:"frequency"`
	Modulation string    `json:"modulation,omitempty"`
	Program    uint16    `json:"program"`
	TSID       uint16    `json:"tsid,omitempty"`
	Name       string    `json:"name,omitempty"`   // the name the multiplex gives it
	Chosen     bool      `json:"chosen,omitempty"` // the place the devices' lineups use
	Signal     *cpSignal `json:"signal,omitempty"`
	// Guide says whether anything is known about the multiplex's guide for this channel
	// at all (a multiplex that was never read has no answer, one that sends no guide
	// has zero events); GuideHours is how far ahead it goes.
	Guide       bool    `json:"guide"`
	GuideHours  float64 `json:"guideHours"`
	GuideEvents int     `json:"guideEvents"`
	Seen        int64   `json:"seen,omitempty"` // when the multiplex was last read, unix seconds
	// The choices, as they stand.
	Off   bool `json:"off,omitempty"`   // not to be used for video
	First bool `json:"first,omitempty"` // tried first for video
	Used  bool `json:"used,omitempty"`  // the guide in use is read from here
}

type cpChannel struct {
	Number    string       `json:"number"`
	Name      string       `json:"name"`
	HD        bool         `json:"hd,omitempty"`
	DRM       bool         `json:"drm,omitempty"`
	Locations []cpLocation `json:"locations"`
	// What was chosen (0 = not chosen: the lineup's place for video, the guide that
	// goes furthest ahead for the guide).
	Video uint64 `json:"video,omitempty"`
	Guide uint64 `json:"guide,omitempty"`
}

type cpView struct {
	Time     int64       `json:"time"` // unix milliseconds
	Channels []cpChannel `json:"channels"`
	// Duplicates counts the channels that are on more than one RF channel.
	Duplicates int `json:"duplicates"`
	// Known counts the RF channels the multiplexer has read so far.
	Known int `json:"known"`
	// Editable says whether choices can be saved from this address.
	Editable bool `json:"editable"`
}

// channelsView builds the page from the lineup, what the RF channel table has
// learned and the choices that were made.
func channelsView(entries []lineup.Entry, rf []rfmap.RF, prefs map[string]chanprefs.Pref, guideSource func(string) uint64, others bool, now time.Time) cpView {
	byFreq := map[uint64]*rfmap.RF{}
	for i := range rf {
		byFreq[rf[i].Frequency] = &rf[i]
	}
	// where each channel number is carried, according to the multiplexes themselves
	places := map[string][]cpLocation{}
	for i := range rf {
		r := &rf[i]
		for _, p := range r.Programs {
			if p.Channel == "" {
				continue
			}
			places[p.Channel] = append(places[p.Channel], locationOf(r, p, now))
		}
	}
	out := cpView{Time: now.UnixMilli(), Known: len(rf), Channels: []cpChannel{}}
	for _, e := range entries {
		c := cpChannel{Number: e.GuideNumber, Name: e.GuideName, HD: e.HD, DRM: e.DRM}
		locs := places[e.GuideNumber]
		// the lineup's own place always shows, even before the multiplex has been read
		found := false
		for i := range locs {
			if locs[i].Frequency == e.Frequency && (e.Program == 0 || locs[i].Program == e.Program || len(locs) == 1) {
				locs[i].Chosen, found = true, true
			}
		}
		if !found && e.Frequency != 0 {
			l := cpLocation{Frequency: e.Frequency, Modulation: e.Modulation, Program: e.Program, TSID: e.TSID, Name: e.GuideName, Chosen: true}
			if r := byFreq[e.Frequency]; r != nil {
				if p := r.Programs[e.Program]; p != nil {
					l = locationOf(r, p, now)
					l.Chosen = true
				}
			}
			locs = append(locs, l)
		}
		sort.Slice(locs, func(i, j int) bool { return locs[i].Frequency < locs[j].Frequency })
		pref := prefs[e.GuideNumber]
		c.Video, c.Guide = pref.Video, pref.Guide
		// what may be used: the lineup's place, and the others only when they were allowed (or
		// the multiplexer is set to use them all); never what was switched off
		allowed := func(l cpLocation) bool {
			if pref.IsOff(l.Frequency) {
				return false
			}
			return l.Chosen || others || pref.IsOn(l.Frequency) || pref.Video == l.Frequency
		}
		// none allowed (an old choice from when that hid the channel): the lineup's place is used
		allOff := len(locs) > 0
		for i := range locs {
			if allowed(locs[i]) {
				allOff = false
			}
		}
		used := guideSource(e.GuideNumber)
		first := false
		for i := range locs {
			l := &locs[i]
			l.Off = !allowed(*l) && !(allOff && l.Chosen)
			l.Used = used != 0 && l.Frequency == used
			// the place tried first: the chosen one, else the lineup's, among those switched on
			if !first && !l.Off && (pref.Video == l.Frequency || (pref.Video == 0 && l.Chosen)) {
				l.First, first = true, true
			}
		}
		if !first { // the chosen place is off or unknown: the first one that is on
			for i := range locs {
				if !locs[i].Off {
					locs[i].First = true
					break
				}
			}
		}
		c.Locations = locs
		if len(locs) > 1 {
			out.Duplicates++
		}
		out.Channels = append(out.Channels, c)
	}
	return out
}

func anyOn(locs []cpLocation) bool {
	for _, l := range locs {
		if !l.Off {
			return true
		}
	}
	return false
}

func locationOf(r *rfmap.RF, p *rfmap.Prog, now time.Time) cpLocation {
	l := cpLocation{Frequency: r.Frequency, Modulation: r.Modulation, Program: p.Number, TSID: r.TSID, Name: p.Name, Seen: r.Seen.Unix()}
	if g := p.Guide; g != nil {
		l.Guide, l.GuideEvents = true, g.Events
		if g.Until.After(now) {
			l.GuideHours = g.Until.Sub(now).Hours()
		}
	}
	var latest *rfmap.Signal
	device := ""
	for d, s := range r.Signal {
		if latest == nil || s.At.After(latest.At) {
			latest, device = s, d
		}
	}
	if latest != nil {
		l.Signal = &cpSignal{SS: latest.SS, SNQ: latest.SNQ, SEQ: latest.SEQ, Device: strings.ToUpper(device), At: latest.At.Unix()}
	}
	return l
}

func (d *Device) guideSource(number string) uint64 {
	if s := d.guideStore(); s != nil {
		return s.SourceOf(number)
	}
	return 0
}

func (d *Device) handleUIChannels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	v := channelsView(d.lineup.Entries(true), d.rf.Snapshot(), d.prefs.All(), d.guideSource, d.lv().AutoJoinOtherLocations, time.Now())
	v.Editable = d.adminCheck(nil, r, false) && !d.cfg.SettingsLocked
	writeJSON(w, v)
}

// handleUIChannelPut saves the choices for one channel. The body is
// {"off": [Hz...], "video": Hz, "guide": Hz}; zero means "not chosen".
func (d *Device) handleUIChannelPut(w http.ResponseWriter, r *http.Request) {
	if !d.adminCheck(w, r, true) {
		return
	}
	number := r.PathValue("number")
	e, ok := d.lineup.Find(number)
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no such channel"})
		return
	}
	var p chanprefs.Pref
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&p); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	// only places the channel is known to have can be chosen
	known := map[uint64]bool{e.Frequency: e.Frequency != 0}
	for _, rf := range d.rf.Snapshot() {
		for _, pr := range rf.Programs {
			if pr.Channel == number {
				known[rf.Frequency] = true
			}
		}
	}
	for _, f := range append(append([]uint64{}, p.Off...), p.Video, p.Guide) {
		if f != 0 && !known[f] {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("%d Hz is not a place %s is received on", f, number)})
			return
		}
	}
	offAll := len(known) > 0
	for f, ok := range known {
		if ok && !(chanprefs.Pref{Off: p.Off}).IsOff(f) {
			offAll = false
		}
	}
	if offAll {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "at least one RF channel has to stay on; to switch the channel off, disable it in the Lineup"})
		return
	}
	// what is not switched off is allowed: the page shows these places as ticked
	p.On = nil
	for f, ok := range known {
		if ok && !(chanprefs.Pref{Off: p.Off}).IsOff(f) && f != e.Frequency {
			p.On = append(p.On, f)
		}
	}
	if p.Video != 0 && (chanprefs.Pref{Off: p.Off}).IsOff(p.Video) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "the first choice for video cannot be switched off"})
		return
	}
	if err := d.prefs.Set(number, p); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// rfAllOff reports whether the RF channel carries channels (as far as is known) and
// every one of them is switched off there: nothing then needs it tuned.
func (d *Device) rfAllOff(freq uint64) bool {
	n := 0
	for _, r := range d.rf.Snapshot() {
		if r.Frequency != freq {
			continue
		}
		for _, p := range r.Programs {
			if p.Channel == "" {
				continue
			}
			n++
			if !d.prefs.Get(p.Channel).IsOff(freq) {
				return false
			}
		}
	}
	return n > 0
}

// entryAny finds a channel of the lineup whether or not it is switched on there.
func (d *Device) entryAny(number string) (lineup.Entry, bool) {
	for _, e := range d.lineup.Entries(true) {
		if e.GuideNumber == number {
			return e, true
		}
	}
	return lineup.Entry{}, false
}
