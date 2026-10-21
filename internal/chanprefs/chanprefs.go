// Package chanprefs keeps the choices a person makes on the Channels page for
// channels that are received on more than one RF channel: which places may be
// used for video, which one to try first, and which one the programme guide is
// taken from. Nothing is stored for a channel nobody has touched.
package chanprefs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
)

// Pref is the choices for one channel, by its guide number ("24.1").
type Pref struct {
	// Off lists RF channels (Hz) the channel must not be tuned on.
	Off []uint64 `json:"off,omitempty"`
	// Video is the RF channel to try first (0 = the devices' lineup's place).
	Video uint64 `json:"video,omitempty"`
	// Guide is the RF channel whose programme guide is used (0 = automatic: the one
	// that goes furthest ahead).
	Guide uint64 `json:"guide,omitempty"`
}

func (p Pref) empty() bool { return len(p.Off) == 0 && p.Video == 0 && p.Guide == 0 }

// IsOff reports whether freq is switched off for the channel.
func (p Pref) IsOff(freq uint64) bool {
	for _, f := range p.Off {
		if f == freq {
			return true
		}
	}
	return false
}

// Store is the set of choices, kept in a small JSON file.
type Store struct {
	mu   sync.Mutex
	path string
	m    map[string]Pref
	rev  atomic.Uint64
}

// Open loads the choices from path (a missing file means none). An empty path
// keeps them in memory only.
func Open(path string) (*Store, error) {
	s := &Store{path: path, m: map[string]Pref{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.m); err != nil {
		return nil, err
	}
	return s, nil
}

// Get returns the choices for a channel (none if untouched).
func (s *Store) Get(number string) Pref {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[number]
}

// Rev changes whenever any choice does, so that things derived from the choices
// can be recomputed.
func (s *Store) Rev() uint64 { return s.rev.Load() }

// Set replaces the choices for a channel and writes them out. Empty choices remove
// the channel from the file.
func (s *Store) Set(number string, p Pref) error {
	sort.Slice(p.Off, func(i, j int) bool { return p.Off[i] < p.Off[j] })
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.m[number]
	if p.empty() {
		delete(s.m, number)
	} else {
		s.m[number] = p
	}
	if err := s.saveLocked(); err != nil {
		if old.empty() {
			delete(s.m, number)
		} else {
			s.m[number] = old
		}
		return err
	}
	s.rev.Add(1)
	return nil
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// All returns a copy of every stored choice.
func (s *Store) All() map[string]Pref {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Pref, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}
