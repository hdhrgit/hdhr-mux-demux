// Package guide stores the programme guide collected from the broadcasts.
package guide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"hdhrmux/internal/psip"
)

// Store keeps the events of every channel, keyed by guide number ("7.1"), and
// persists them to disk.
type Store struct {
	mu     sync.Mutex
	path   string
	events map[string][]psip.Event
	rev    uint64
	saved  time.Time
	dirty  bool
}

// Open loads the store from path (a missing file is an empty guide). An empty
// path keeps it in memory only.
func Open(path string) (*Store, error) {
	s := &Store{path: path, events: map[string][]psip.Event{}}
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
	if err := json.Unmarshal(b, &s.events); err != nil {
		return nil, err
	}
	return s, nil
}

// retention is how long after it ended an event is kept.
const retention = 2 * time.Hour

// Merge adds what a multiplex's tables currently say. For each channel the
// new events replace any stored event they overlap; the rest are kept, and
// events that ended long ago are dropped.
func (s *Store) Merge(g psip.Guide, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, fresh := range g.Events {
		if len(fresh) == 0 {
			continue
		}
		merged := append([]psip.Event(nil), fresh...)
		for _, e := range s.events[ch] {
			replaced := false
			for _, f := range fresh {
				if e.Start.Before(f.End()) && f.Start.Before(e.End()) {
					replaced = true // the tables now say something else for this time
					break
				}
			}
			if !replaced {
				merged = append(merged, e)
			}
		}
		var keep []psip.Event
		for _, e := range merged {
			if e.End().After(now.Add(-retention)) && e.Title != "" {
				keep = append(keep, e)
			}
		}
		sort.Slice(keep, func(i, j int) bool { return keep[i].Start.Before(keep[j].Start) })
		s.events[ch] = keep
	}
	s.rev++
	s.dirty = true
	if time.Since(s.saved) > 30*time.Second {
		s.saveLocked()
	}
}

// Flush writes any unsaved changes.
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		s.saveLocked()
	}
}

func (s *Store) saveLocked() {
	s.saved, s.dirty = time.Now(), false
	if s.path == "" {
		return
	}
	b, err := json.Marshal(s.events)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.path)
	}
}

// Events returns a channel's events (a copy).
func (s *Store) Events(channel string) []psip.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]psip.Event(nil), s.events[channel]...)
}

// Summary reports how many channels have events and how many events there are.
func (s *Store) Summary() (channels, events int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evs := range s.events {
		if len(evs) > 0 {
			channels++
			events += len(evs)
		}
	}
	return
}

// Newest returns the end of the latest event of a channel (zero if none).
func (s *Store) Newest(channel string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t time.Time
	for _, e := range s.events[channel] {
		if e.End().After(t) {
			t = e.End()
		}
	}
	return t
}
