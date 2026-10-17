// Package lineup stores the virtual device's channel lineup, produced by
// scanning the physical tuners (or supplied by hand), and persists it.
package lineup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Entry struct {
	GuideNumber   string `json:"GuideNumber"`
	GuideName     string `json:"GuideName"`
	Frequency     uint64 `json:"Frequency"`
	Program       uint16 `json:"Program"`
	Modulation    string `json:"Modulation,omitempty"`
	Source        string `json:"Source,omitempty"`
	DRM           bool   `json:"DRM,omitempty"`
	Favorite      bool   `json:"Favorite,omitempty"`
	HD            bool   `json:"HD,omitempty"`
	VideoCodec    string `json:"VideoCodec,omitempty"`
	AudioCodec    string `json:"AudioCodec,omitempty"`
	SignalQuality int    `json:"SignalQuality,omitempty"`
	// Disabled entries are kept (shown with ?show=found) but not served.
	Disabled bool `json:"Disabled,omitempty"`
}

type Lineup struct {
	mu      sync.RWMutex
	path    string
	entries []Entry
}

// Open loads the lineup from path (a missing file is an empty lineup). An
// empty path keeps the lineup in memory only.
func Open(path string) (*Lineup, error) {
	l := &Lineup{path: path}
	if path == "" {
		return l, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &l.entries); err != nil {
		return nil, err
	}
	return l, nil
}

func guideLess(a, b string) bool {
	af, bf := guideKey(a), guideKey(b)
	if af != bf {
		return af < bf
	}
	return a < b
}

func guideKey(g string) float64 {
	major, minor, _ := strings.Cut(g, ".")
	m, _ := strconv.Atoi(major)
	n, _ := strconv.Atoi(minor)
	return float64(m) + float64(n)/1000
}

// Replace swaps the whole lineup and persists it.
func (l *Lineup) Replace(es []Entry) error {
	es = append([]Entry(nil), es...)
	sort.SliceStable(es, func(i, j int) bool { return guideLess(es[i].GuideNumber, es[j].GuideNumber) })
	l.mu.Lock()
	l.entries = es
	l.mu.Unlock()
	return l.save()
}

func (l *Lineup) save() error {
	if l.path == "" {
		return nil
	}
	l.mu.RLock()
	b, err := json.MarshalIndent(l.entries, "", "  ")
	l.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Entries returns a copy of the lineup; disabled entries only if all.
func (l *Lineup) Entries(all bool) []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		if all || !e.Disabled {
			out = append(out, e)
		}
	}
	return out
}

// Find looks a channel up by its guide number ("8.1").
func (l *Lineup) Find(guide string) (Entry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range l.entries {
		if e.GuideNumber == guide && !e.Disabled {
			return e, true
		}
	}
	return Entry{}, false
}

// SetFavorite marks or clears a favourite and persists.
func (l *Lineup) SetFavorite(guide string, fav bool) {
	l.mu.Lock()
	for i := range l.entries {
		if l.entries[i].GuideNumber == guide {
			l.entries[i].Favorite = fav
		}
	}
	l.mu.Unlock()
	l.save()
}

// ProgramName implements hub's program namer.
func (l *Lineup) ProgramName(freq uint64, program uint16) (string, string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range l.entries {
		if e.Frequency == freq && e.Program == program {
			return e.GuideNumber, e.GuideName, true
		}
	}
	return "", "", false
}

// Modulation returns the modulation recorded for programs on freq ("" if the
// frequency is not in the lineup).
func (l *Lineup) Modulation(freq uint64) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range l.entries {
		if e.Frequency == freq && e.Modulation != "" {
			return e.Modulation
		}
	}
	return ""
}
