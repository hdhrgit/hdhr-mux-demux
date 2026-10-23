// Package rfmap remembers what the multiplexer has learned about each RF channel:
// which programmes it carries and under which channel numbers, how far ahead its
// guide goes, and how well it was received. It is learned passively from the streams
// and guide passes that tune the channels anyway, and kept in a small file so that
// it survives a restart. Nothing here tunes anything.
package rfmap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Guide says what a multiplex's own programme guide holds for one programme.
type Guide struct {
	Events int       `json:"events"`
	Until  time.Time `json:"until"` // when the last event ends
	Seen   time.Time `json:"seen"`
}

// Prog is one programme of a multiplex.
type Prog struct {
	Number  uint16   `json:"number"`            // program number in the PAT (the service id for ATSC 3.0)
	Channel string   `json:"channel,omitempty"` // virtual channel number from the multiplex's own table
	Name    string   `json:"name,omitempty"`    // its short name there
	PMTPID  uint16   `json:"pmtPid,omitempty"`
	PIDs    []uint16 `json:"pids,omitempty"` // PCR and elementary streams
	Guide   *Guide   `json:"guide,omitempty"`
}

// Signal is a reception measurement.
type Signal struct {
	SS  int       `json:"ss"`
	SNQ int       `json:"snq"`
	SEQ int       `json:"seq"`
	At  time.Time `json:"at"`
}

// RF is what is known about one RF channel.
type RF struct {
	Frequency  uint64             `json:"frequency"`
	Modulation string             `json:"modulation,omitempty"`
	TSID       uint16             `json:"tsid,omitempty"`
	Seen       time.Time          `json:"seen"`
	Programs   map[uint16]*Prog   `json:"programs"`
	Signal     map[string]*Signal `json:"signal,omitempty"` // by device id: devices can have different antennas
}

// Store is the table of RF channels.
type Store struct {
	mu    sync.Mutex
	path  string
	rf    map[uint64]*RF
	saved time.Time
	dirty bool
	rev   atomic.Uint64                // changes when the set of programmes of an RF channel does
	obs   atomic.Pointer[func(Change)] // told what changed about an RF channel
}

// Change is something about an RF channel that was different the last time it was read: a
// call sign or channel number, a programme that came or went, the transport stream id.
type Change struct {
	Kind      string // callsign, number, program-added, program-removed, tsid
	Frequency uint64
	Program   uint16
	Channel   string // the channel number concerned
	Old, New  string
}

// SetObserver sets the function told about the changes NoteMux finds. The first time an RF
// channel is read is not a change.
func (s *Store) SetObserver(f func(Change)) { s.obs.Store(&f) }

// saveEvery limits how often changes are written out.
const saveEvery = 30 * time.Second

// Open loads the table from path (a missing file is an empty table). An empty path
// keeps it in memory only.
func Open(path string) (*Store, error) {
	s := &Store{path: path, rf: map[uint64]*RF{}}
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
	var list []*RF
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, r := range list {
		if r.Programs == nil {
			r.Programs = map[uint16]*Prog{}
		}
		s.rf[r.Frequency] = r
	}
	return s, nil
}

func (s *Store) get(freq uint64) *RF {
	r := s.rf[freq]
	if r == nil {
		r = &RF{Frequency: freq, Programs: map[uint16]*Prog{}}
		s.rf[freq] = r
	}
	return r
}

// NoteMux records the programmes a multiplex carries right now. Programmes that
// are no longer in its PAT are forgotten; guide information about the ones that
// stay is kept.
func (s *Store) NoteMux(freq uint64, modulation string, tsid uint16, progs []Prog, now time.Time) {
	if freq == 0 || len(progs) == 0 {
		return
	}
	var changes []Change
	defer func() { // runs after the unlock
		if f := s.obs.Load(); f != nil {
			for _, c := range changes {
				(*f)(c)
			}
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(freq)
	if modulation != "" {
		r.Modulation = modulation
	}
	had := len(r.Programs) > 0
	if had && tsid != 0 && r.TSID != 0 && r.TSID != tsid {
		changes = append(changes, Change{Kind: "tsid", Frequency: freq, Old: strconv.Itoa(int(r.TSID)), New: strconv.Itoa(int(tsid))})
	}
	if tsid != 0 || r.TSID == 0 {
		r.TSID = tsid
	}
	r.Seen = now
	next := make(map[uint16]*Prog, len(progs))
	for i := range progs {
		p := progs[i]
		if old := r.Programs[p.Number]; old != nil {
			if p.Channel == "" {
				p.Channel, p.Name = old.Channel, old.Name
			}
			if old.Channel == p.Channel {
				p.Guide = old.Guide
			}
		}
		next[p.Number] = &p
	}
	if had {
		for n, p := range next {
			o := r.Programs[n]
			switch {
			case o == nil:
				changes = append(changes, Change{Kind: "program-added", Frequency: freq, Program: n, Channel: p.Channel, New: p.Name})
			case o.Name != "" && p.Name != "" && o.Name != p.Name:
				changes = append(changes, Change{Kind: "callsign", Frequency: freq, Program: n, Channel: p.Channel, Old: o.Name, New: p.Name})
			}
			if o != nil && o.Channel != "" && p.Channel != "" && o.Channel != p.Channel {
				changes = append(changes, Change{Kind: "number", Frequency: freq, Program: n, Old: o.Channel, New: p.Channel})
			}
		}
		for n, o := range r.Programs {
			if next[n] == nil {
				changes = append(changes, Change{Kind: "program-removed", Frequency: freq, Program: n, Channel: o.Channel, Old: o.Name})
			}
		}
	}
	r.Programs = next
	s.rev.Add(1)
	s.touchLocked(now)
}

// NoteGuide records how much guide a multiplex's tables hold for a programme.
func (s *Store) NoteGuide(freq uint64, program uint16, channel, name string, events int, until time.Time, now time.Time) {
	if freq == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(freq)
	p := r.Programs[program]
	if p == nil {
		p = &Prog{Number: program}
		r.Programs[program] = p
	}
	if channel != "" {
		p.Channel = channel
	}
	if name != "" {
		p.Name = name
	}
	p.Guide = &Guide{Events: events, Until: until, Seen: now}
	s.touchLocked(now)
}

// NoteSignal records a reception measurement made through a device.
func (s *Store) NoteSignal(freq uint64, device string, ss, snq, seq int, now time.Time) {
	if freq == 0 || device == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(freq)
	if r.Signal == nil {
		r.Signal = map[string]*Signal{}
	}
	old := r.Signal[device]
	r.Signal[device] = &Signal{SS: ss, SNQ: snq, SEQ: seq, At: now}
	if old == nil || now.Sub(old.At) > time.Minute {
		s.touchLocked(now) // in memory always; written out only now and then
	}
}

func (s *Store) touchLocked(now time.Time) {
	s.dirty = true
	if s.path != "" && now.Sub(s.saved) > saveEvery {
		s.saveLocked(now)
	}
}

// Flush writes any unsaved changes.
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		s.saveLocked(time.Now())
	}
}

func (s *Store) saveLocked(now time.Time) {
	s.saved = now
	if s.path == "" {
		return
	}
	b, err := json.Marshal(s.listLocked())
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil && os.Rename(tmp, s.path) == nil {
		s.dirty = false
	}
}

func (s *Store) listLocked() []*RF {
	out := make([]*RF, 0, len(s.rf))
	for _, r := range s.rf {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Frequency < out[j].Frequency })
	return out
}

// Snapshot returns a copy of the table, in frequency order.
func (s *Store) Snapshot() []RF {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RF, 0, len(s.rf))
	for _, r := range s.listLocked() {
		c := RF{Frequency: r.Frequency, Modulation: r.Modulation, TSID: r.TSID, Seen: r.Seen, Programs: map[uint16]*Prog{}}
		for n, p := range r.Programs {
			q := *p
			if p.Guide != nil {
				g := *p.Guide
				q.Guide = &g
			}
			c.Programs[n] = &q
		}
		if len(r.Signal) > 0 {
			c.Signal = map[string]*Signal{}
			for d, sg := range r.Signal {
				x := *sg
				c.Signal[d] = &x
			}
		}
		out = append(out, c)
	}
	return out
}

// Rev changes whenever the programmes of an RF channel change, so that what is
// derived from them can be recomputed.
func (s *Store) Rev() uint64 { return s.rev.Load() }
