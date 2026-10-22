package vdev

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// The merged channels: groups of channels a person says carry the same thing, so that a
// request for one of them can be served by whichever is already being received (see
// placesOf and the merges package).

type mergeMember struct {
	Number   string `json:"number"`
	Name     string `json:"name"`
	HD       bool   `json:"hd,omitempty"`
	Disabled bool   `json:"disabled,omitempty"` // not listed in the lineup (still counts as a place)
	Places   int    `json:"places"`             // how many RF channels it is received on, as far as known
}

func (d *Device) handleUIMergesGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	groups := [][]mergeMember{}
	for _, g := range d.merges.Groups() {
		var out []mergeMember
		for _, n := range g {
			m := mergeMember{Number: n, Name: "(not in the lineup any more)"}
			if e, ok := d.entryAny(n); ok {
				m.Name, m.HD, m.Disabled = e.GuideName, e.HD, e.Disabled
				m.Places = len(d.ownPlaces(e))
			}
			out = append(out, m)
		}
		groups = append(groups, out)
	}
	writeJSON(w, map[string]any{"groups": groups, "editable": d.adminCheck(nil, r, false) && !d.cfg.SettingsLocked})
}

// handleUIMergesPut replaces the groups. The body is {"groups": [["11.1","11.3"], ...]}.
func (d *Device) handleUIMergesPut(w http.ResponseWriter, r *http.Request) {
	if !d.adminCheck(w, r, true) {
		return
	}
	var p struct{ Groups [][]string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&p); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	for _, g := range p.Groups {
		for _, n := range g {
			if _, ok := d.entryAny(n); !ok {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("there is no channel %s", n)})
				return
			}
		}
	}
	if err := d.merges.Set(p.Groups); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
