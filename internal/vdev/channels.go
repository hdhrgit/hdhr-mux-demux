package vdev

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"hdhrmux/internal/lineup"
	"hdhrmux/internal/rfmap"
)

// The Channels page: for every channel, where on the air it is received. Most
// channels are on one RF channel; some stations are carried on two or more, and
// then the multiplexer has a choice to make. Each place shows what is known about
// it: the name the multiplex gives the channel there, how well it is received and
// how far ahead its programme guide goes.

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
	Name       string    `json:"name,omitempty"` // the name the multiplex gives it
	Chosen     bool      `json:"chosen,omitempty"`
	Signal     *cpSignal `json:"signal,omitempty"`
	// GuideHours is how far ahead the multiplex's own guide goes; Guide says whether
	// anything is known about it at all (a multiplex whose guide was never read has
	// no answer, one that sends no guide has zero events).
	Guide       bool    `json:"guide"`
	GuideHours  float64 `json:"guideHours"`
	GuideEvents int     `json:"guideEvents"`
	Seen        int64   `json:"seen,omitempty"` // when the multiplex was last read, unix seconds
}

type cpChannel struct {
	Number    string       `json:"number"`
	Name      string       `json:"name"`
	HD        bool         `json:"hd,omitempty"`
	DRM       bool         `json:"drm,omitempty"`
	Locations []cpLocation `json:"locations"`
}

type cpView struct {
	Time     int64       `json:"time"` // unix milliseconds
	Channels []cpChannel `json:"channels"`
	// Duplicates counts the channels that are on more than one RF channel.
	Duplicates int `json:"duplicates"`
	// Known counts the RF channels the multiplexer has read so far.
	Known int `json:"known"`
}

// channelsView builds the page from the lineup and what the RF table has learned.
func channelsView(entries []lineup.Entry, rf []rfmap.RF, now time.Time) cpView {
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
		c.Locations = locs
		if len(locs) > 1 {
			out.Duplicates++
		}
		out.Channels = append(out.Channels, c)
	}
	return out
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
		l.Signal = &cpSignal{SS: latest.SS, SNQ: latest.SNQ, SEQ: latest.SEQ, Device: deviceLabel(device), At: latest.At.Unix()}
	}
	return l
}

// deviceLabel is how a device's id reads on the page.
func deviceLabel(id string) string {
	return strings.ToUpper(id)
}

func (d *Device) handleUIChannels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, channelsView(d.lineup.Entries(true), d.rf.Snapshot(), time.Now()))
}
