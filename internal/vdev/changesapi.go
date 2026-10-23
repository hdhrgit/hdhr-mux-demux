package vdev

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"hdhrmux/internal/changelog"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/rfmap"
)

// The history of changes: a station's call sign or number, where on the air it is, which
// programmes an RF channel carries. It is watched for every channel in the lineup, the
// disabled ones too, and for every RF channel the multiplexer reads, and kept in a file of
// its own (changes.log in the state directory).

func mhzText(hz uint64) string {
	if hz%1000000 == 0 {
		return strconv.FormatUint(hz/1000000, 10) + " MHz"
	}
	return strconv.FormatFloat(float64(hz)/1e6, 'f', -1, 64) + " MHz"
}

func (d *Device) noteLineupChange(c lineup.Change) {
	e := changelog.Event{Kind: c.Kind, Channel: c.Channel, Old: c.Old, New: c.New}
	switch c.Kind {
	case "callsign":
		e.Text = fmt.Sprintf("%s: call sign %s → %s", c.Channel, c.Old, c.New)
	case "moved":
		e.Frequency = c.Frequency
		e.Old, e.New = mhzText(c.OldFrequency), mhzText(c.Frequency)
		e.Text = fmt.Sprintf("%s: moved from %s to %s", c.Channel, e.Old, e.New)
	case "added":
		e.Text = fmt.Sprintf("%s %s added to the lineup", c.Channel, c.New)
	case "removed":
		e.Text = fmt.Sprintf("%s %s removed from the lineup", c.Channel, c.Old)
	default:
		return
	}
	d.changes.Add(e)
}

func (d *Device) noteRFChange(c rfmap.Change) {
	e := changelog.Event{Kind: c.Kind, Channel: c.Channel, Frequency: c.Frequency, Old: c.Old, New: c.New}
	at := mhzText(c.Frequency)
	switch c.Kind {
	case "callsign":
		e.Text = fmt.Sprintf("%s, program %d (%s): call sign %s → %s", at, c.Program, c.Channel, c.Old, c.New)
	case "number":
		e.Text = fmt.Sprintf("%s, program %d: channel number %s → %s", at, c.Program, c.Old, c.New)
	case "program-added":
		e.Text = fmt.Sprintf("%s: program %d added (%s %s)", at, c.Program, c.Channel, c.New)
	case "program-removed":
		e.Text = fmt.Sprintf("%s: program %d removed (%s %s)", at, c.Program, c.Channel, c.Old)
	case "tsid":
		e.Text = fmt.Sprintf("%s: transport stream id %s → %s", at, c.Old, c.New)
	default:
		return
	}
	d.changes.Add(e)
}

// handleUIChanges lists the history, newest first (?n= how many, default 500); with
// ?download=1 it is the file itself, one JSON object per line.
func (d *Device) handleUIChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Cache-Control", "no-store")
	if q.Get("download") == "1" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="changes.log"`)
		if p := d.changes.Path(); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				w.Write(b)
				return
			}
		}
		for _, e := range d.changes.List(0) {
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "%s\n", b)
		}
		return
	}
	n, _ := strconv.Atoi(q.Get("n"))
	if n < 1 || n > 5000 {
		n = 500
	}
	ev := d.changes.List(n)
	for i, j := 0, len(ev)-1; i < j; i, j = i+1, j-1 {
		ev[i], ev[j] = ev[j], ev[i]
	}
	if ev == nil {
		ev = []changelog.Event{}
	}
	writeJSON(w, map[string]any{"events": ev, "file": d.changes.Path()})
}
