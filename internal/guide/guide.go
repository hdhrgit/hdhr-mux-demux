// Package guide stores the programme guide collected from the broadcasts.
//
// A channel can be carried on more than one RF channel, each with a guide of its own
// (or none), so events are kept per channel and per RF channel they were read from.
// Which one is used for a channel is a choice: made by a person on the Channels page,
// or automatically the one that goes furthest ahead.
package guide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"hdhrmux/internal/psip"
)

// Store keeps the events of every channel, by guide number ("7.1") and the RF
// channel they were read from, and persists them to disk.
type Store struct {
	mu     sync.Mutex
	path   string
	events map[string]map[uint64][]psip.Event // channel -> RF channel (Hz, 0 = unknown) -> events
	choose func(channel string) uint64
	rev    uint64
	saved  time.Time
	dirty  bool
}

// onDisk is the file format. Older files were a plain map of channel to events;
// those events are loaded as coming from an unknown RF channel (0).
type onDisk struct {
	Sources map[string]map[string][]psip.Event `json:"sources"`
}

// Open loads the store from path (a missing file is an empty guide). An empty
// path keeps it in memory only.
func Open(path string) (*Store, error) {
	s := &Store{path: path, events: map[string]map[uint64][]psip.Event{}}
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
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	if _, ok := raw["sources"]; ok {
		var d onDisk
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, err
		}
		for ch, by := range d.Sources {
			m := map[uint64][]psip.Event{}
			for f, evs := range by {
				n, err := strconv.ParseUint(f, 10, 64)
				if err != nil {
					continue
				}
				m[n] = evs
			}
			s.events[ch] = m
		}
		return s, nil
	}
	var old map[string][]psip.Event
	if err := json.Unmarshal(b, &old); err != nil {
		return nil, err
	}
	for ch, evs := range old {
		s.events[ch] = map[uint64][]psip.Event{0: evs}
	}
	return s, nil
}

// SetChooser says which RF channel's guide to use for a channel: a frequency, or 0
// to let the store pick the one that goes furthest ahead.
func (s *Store) SetChooser(f func(channel string) uint64) {
	s.mu.Lock()
	s.choose = f
	s.mu.Unlock()
}

// retention is how long after it ended an event is kept.
const retention = 2 * time.Hour

// Merge adds what a multiplex's tables currently say (g.Frequency is the RF
// channel they were read from). For each channel the new events replace any stored
// event of that RF channel they overlap; the rest are kept, and events that ended
// long ago are dropped.
func (s *Store) Merge(g psip.Guide, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, fresh := range g.Events {
		if len(fresh) == 0 {
			continue
		}
		by := s.events[ch]
		if by == nil {
			by = map[uint64][]psip.Event{}
			s.events[ch] = by
		}
		merged := append([]psip.Event(nil), fresh...)
		for _, e := range by[g.Frequency] {
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
		by[g.Frequency] = keep
		if g.Frequency != 0 {
			delete(by, 0) // events from before the RF channel was recorded are replaced
		}
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
	d := onDisk{Sources: map[string]map[string][]psip.Event{}}
	for ch, by := range s.events {
		m := map[string][]psip.Event{}
		for f, evs := range by {
			if len(evs) > 0 {
				m[strconv.FormatUint(f, 10)] = evs
			}
		}
		if len(m) > 0 {
			d.Sources[ch] = m
		}
	}
	b, err := json.Marshal(d)
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

// pickLocked chooses the RF channel whose events are used for a channel: the one
// asked for if it has any, otherwise the one that goes furthest ahead.
func (s *Store) pickLocked(channel string) []psip.Event {
	by := s.events[channel]
	if len(by) == 0 {
		return nil
	}
	if s.choose != nil {
		if f := s.choose(channel); f != 0 {
			if evs := by[f]; len(evs) > 0 {
				return evs
			}
		}
	}
	var best []psip.Event
	var bestEnd time.Time
	var bestFreq uint64
	for f, evs := range by {
		if len(evs) == 0 {
			continue
		}
		end := evs[len(evs)-1].End() // sorted by start; the last one ends last in practice
		for _, e := range evs {
			if e.End().After(end) {
				end = e.End()
			}
		}
		if best == nil || end.After(bestEnd) || (end.Equal(bestEnd) && f < bestFreq) {
			best, bestEnd, bestFreq = evs, end, f
		}
	}
	return best
}

// Events returns a channel's events (a copy), from the RF channel chosen for it.
func (s *Store) Events(channel string) []psip.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]psip.Event(nil), s.pickLocked(channel)...)
}

// Summary reports how many channels have events and how many events there are.
func (s *Store) Summary() (channels, events int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.events {
		if evs := s.pickLocked(ch); len(evs) > 0 {
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
	for _, e := range s.pickLocked(channel) {
		if e.End().After(t) {
			t = e.End()
		}
	}
	return t
}

// SourceOf says which RF channel the events used for a channel were read from (0
// when there are none, or only ones from before the RF channel was recorded).
func (s *Store) SourceOf(channel string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs := s.pickLocked(channel)
	if len(evs) == 0 {
		return 0
	}
	for f, by := range s.events[channel] {
		if len(by) > 0 && &by[0] == &evs[0] {
			return f
		}
	}
	return 0
}
