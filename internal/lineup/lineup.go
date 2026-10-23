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
	"sync/atomic"
)

type Entry struct {
	GuideNumber   string `json:"GuideNumber"`
	GuideName     string `json:"GuideName"`
	Frequency     uint64 `json:"Frequency"`
	Program       uint16 `json:"Program"`
	Modulation    string `json:"Modulation,omitempty"`
	TSID          uint16 `json:"TSID,omitempty"` // the multiplex's transport stream id
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
	mu        sync.RWMutex
	path      string
	entries   []Entry
	rev       atomic.Uint64
	hideATSC3 atomic.Bool                  // leave ATSC 3.0 channels out of listings (they still tune)
	obs       atomic.Pointer[func(Change)] // told what changed in a merge (see SetObserver)
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
	l.rev.Add(1)
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
	hide := l.hideATSC3.Load()
	for _, e := range l.entries {
		if all || !e.Disabled && !(hide && e.Modulation == "atsc3") {
			out = append(out, e)
		}
	}
	return out
}

// SetHideATSC3 leaves ATSC 3.0 channels out of the listings Entries(false) gives (the
// lineup, the playlist, the guide). Find and Entries(true) still have them, so a
// client that asks for one by number is still served.
func (l *Lineup) SetHideATSC3(hide bool) { l.hideATSC3.Store(hide) }

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

// SetFavorite marks or clears a favourite and persists. A favourite is enabled.
func (l *Lineup) SetFavorite(guide string, fav bool) {
	l.mu.Lock()
	for i := range l.entries {
		if l.entries[i].GuideNumber == guide {
			l.entries[i].Favorite = fav
			if fav {
				l.entries[i].Disabled = false
			}
		}
	}
	l.mu.Unlock()
	l.save()
}

// SetDisabled switches a channel off or on in this lineup only (the devices are not told)
// and persists. A disabled channel is not a favourite and is not served.
func (l *Lineup) SetDisabled(guide string, off bool) {
	l.mu.Lock()
	for i := range l.entries {
		if l.entries[i].GuideNumber == guide {
			l.entries[i].Disabled = off
			if off {
				l.entries[i].Favorite = false
			}
		}
	}
	l.mu.Unlock()
	l.save()
}

// Lookup finds a channel whether or not it is disabled.
func (l *Lineup) Lookup(guide string) (Entry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range l.entries {
		if e.GuideNumber == guide {
			return e, true
		}
	}
	return Entry{}, false
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

// Change is something a merge of the devices' lineups found different: a channel's name, the RF
// channel the devices put it on, a channel that appeared or went away.
type Change struct {
	Kind         string // callsign, moved, added, removed
	Channel      string
	Old, New     string // the names, for callsign, added and removed
	OldFrequency uint64
	Frequency    uint64
}

// SetObserver sets the function told about the changes each Merge finds (a first fill of an
// empty lineup is not a change). It runs after the lineup is unlocked.
func (l *Lineup) SetObserver(f func(Change)) { l.obs.Store(&f) }

// DeviceEntry is a channel as listed by a physical device's own lineup.
type DeviceEntry struct {
	GuideNumber string
	GuideName   string
	VideoCodec  string
	AudioCodec  string
	HD          bool
	Favorite    bool
	DRM         bool
	// Where the device found the channel on the air (from lineup.json?tuning), if it says.
	Frequency  uint64
	Modulation string
	Program    uint16
	TSID       uint16
}

// Merge makes the devices' lineups the channel list: names and codec, HD and
// DRM details come from them, RF details already learned for a channel are
// kept, and channels no device lists any more are dropped. It reports how many
// channels were added and removed.
func (l *Lineup) Merge(devs []DeviceEntry) (added, removed int, err error) {
	l.mu.Lock()
	old := map[string]Entry{}
	for _, e := range l.entries {
		old[e.GuideNumber] = e
	}
	var out []Entry
	var changes []Change
	seen := map[string]bool{}
	for _, d := range devs {
		if seen[d.GuideNumber] {
			continue
		}
		seen[d.GuideNumber] = true
		e, known := old[d.GuideNumber]
		if !known {
			added++
			if len(old) > 0 {
				changes = append(changes, Change{Kind: "added", Channel: d.GuideNumber, New: d.GuideName})
			}
			e = Entry{GuideNumber: d.GuideNumber, Favorite: d.Favorite}
		}
		if known {
			if e.GuideName != "" && d.GuideName != "" && e.GuideName != d.GuideName {
				changes = append(changes, Change{Kind: "callsign", Channel: d.GuideNumber, Old: e.GuideName, New: d.GuideName})
			}
			if d.Frequency != 0 && e.Frequency != 0 && d.Frequency != e.Frequency {
				changes = append(changes, Change{Kind: "moved", Channel: d.GuideNumber, OldFrequency: e.Frequency, Frequency: d.Frequency})
			}
		}
		e.GuideName, e.VideoCodec, e.AudioCodec, e.HD = d.GuideName, d.VideoCodec, d.AudioCodec, d.HD
		e.DRM = e.DRM || d.DRM
		// The devices say where each channel is on the air, and that is what the lineup
		// follows: a station carried on two RF channels is not moved back and forth by
		// whichever stream was last seen.
		if d.Frequency != 0 {
			e.Frequency, e.Program = d.Frequency, d.Program
			if d.Modulation != "" {
				e.Modulation = d.Modulation
			}
			if d.TSID != 0 {
				e.TSID = d.TSID
			}
		}
		out = append(out, e)
	}
	for g := range old {
		if !seen[g] {
			removed++
			if len(devs) > 0 {
				changes = append(changes, Change{Kind: "removed", Channel: g, Old: old[g].GuideName})
			}
		}
	}
	sortEntries(out)
	l.entries = out
	l.mu.Unlock()
	if f := l.obs.Load(); f != nil {
		for _, c := range changes {
			(*f)(c)
		}
	}
	return added, removed, l.save()
}

func sortEntries(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool { return guideLess(es[i].GuideNumber, es[j].GuideNumber) })
}

// LearnRF records where channels live on the air (frequency, program,
// modulation). Known channels are updated; unknown ones are added only if
// addUnknown is set (an explicit scan). It reports how many entries changed.
func (l *Lineup) LearnRF(es []Entry, addUnknown bool) (changed int) {
	l.mu.Lock()
	idx := map[string]int{}
	for i, e := range l.entries {
		idx[e.GuideNumber] = i
	}
	for _, n := range es {
		i, ok := idx[n.GuideNumber]
		if !ok {
			if !addUnknown {
				continue
			}
			l.entries = append(l.entries, n)
			idx[n.GuideNumber] = len(l.entries) - 1
			changed++
			continue
		}
		e := &l.entries[i]
		if !addUnknown && e.Frequency != 0 {
			// Already placed (by the devices' lineups): a stream seen on another RF channel
			// does not move it. Only what is missing about it is filled in.
			if n.Frequency == e.Frequency {
				if e.TSID == 0 && n.TSID != 0 {
					e.TSID = n.TSID
					changed++
				}
				if n.DRM && !e.DRM {
					e.DRM = true
					changed++
				}
			}
			continue
		}
		if e.Frequency == n.Frequency && e.Program == n.Program && e.Modulation == n.Modulation {
			continue
		}
		e.Frequency, e.Program, e.Modulation = n.Frequency, n.Program, n.Modulation
		if e.Source == "" {
			e.Source = n.Source
		}
		if n.SignalQuality != 0 {
			e.SignalQuality = n.SignalQuality
		}
		e.DRM = e.DRM || n.DRM
		changed++
	}
	if changed > 0 {
		sortEntries(l.entries)
	}
	l.mu.Unlock()
	if changed > 0 {
		l.save()
	}
	return changed
}

// Rev counts changes to the lineup (used as the DLNA UpdateID).
func (l *Lineup) Rev() uint64 { return l.rev.Load() }
