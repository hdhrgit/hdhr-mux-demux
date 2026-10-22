// Package merges keeps the groups of channels a person says carry the same thing
// (11.1 and 11.3 broadcast the same programme, say). For the tuners they are one: a
// request for any of them is served by whichever of them is already being received,
// or, failing that, by the best place any of them has.
package merges

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Store is the set of groups, kept in a small JSON file.
type Store struct {
	mu     sync.Mutex
	path   string
	groups [][]string
	of     map[string]int // channel number -> index in groups
	rev    atomic.Uint64
}

type file struct {
	Groups [][]string `json:"groups"`
}

// Open loads the groups from path (a missing file means none). An empty path keeps
// them in memory only.
func Open(path string) (*Store, error) {
	s := &Store{path: path, of: map[string]int{}}
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
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	groups, err := normalise(f.Groups)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.setLocked(groups)
	return s, nil
}

// normalise trims, drops repeats and groups of fewer than two, and refuses a channel
// that is in two groups.
func normalise(in [][]string) ([][]string, error) {
	var out [][]string
	seen := map[string]bool{}
	for _, g := range in {
		var m []string
		mine := map[string]bool{}
		for _, n := range g {
			n = strings.TrimSpace(n)
			if n == "" || mine[n] {
				continue
			}
			if seen[n] {
				return nil, fmt.Errorf("channel %s is in more than one group", n)
			}
			mine[n] = true
			m = append(m, n)
		}
		for n := range mine {
			seen[n] = true
		}
		if len(m) >= 2 {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Store) setLocked(groups [][]string) {
	s.groups = groups
	s.of = map[string]int{}
	for i, g := range groups {
		for _, n := range g {
			s.of[n] = i
		}
	}
}

// Groups returns a copy of the groups.
func (s *Store) Groups() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.groups))
	for i, g := range s.groups {
		out[i] = append([]string(nil), g...)
	}
	return out
}

// GroupOf returns the channels merged with number, number included and first
// (nil if it is in no group).
func (s *Store) GroupOf(number string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.of[number]
	if !ok {
		return nil
	}
	out := []string{number}
	for _, n := range s.groups[i] {
		if n != number {
			out = append(out, n)
		}
	}
	return out
}

// Rev changes whenever the groups do.
func (s *Store) Rev() uint64 { return s.rev.Load() }

// Set replaces the groups and writes them out.
func (s *Store) Set(groups [][]string) error {
	g, err := normalise(groups)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.groups
	s.setLocked(g)
	if err := s.saveLocked(); err != nil {
		s.setLocked(old)
		return err
	}
	s.rev.Add(1)
	return nil
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(file{Groups: s.groups}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
