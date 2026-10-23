// Package changelog keeps a history of what changed about the channels: a station's call
// sign, the number it goes by, where on the air it is, which programmes an RF channel
// carries. It is a file of its own in the state directory (one JSON object per line), so
// that the history survives restarts and is not lost among the ordinary log; channels that
// are disabled are watched as much as the others.
package changelog

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	keep     = 5000    // events held in memory (the newest)
	maxBytes = 4 << 20 // the file is rotated at this size (one older file is kept)
	dedupe   = 6 * time.Hour
)

// Event is one change.
type Event struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`                // callsign, number, moved, added, removed, tsid, program-added, program-removed
	Channel   string    `json:"channel,omitempty"`   // the guide number it concerns ("7.1"), if any
	Frequency uint64    `json:"frequency,omitempty"` // the RF channel (Hz) it was seen on, if any
	Old       string    `json:"old,omitempty"`
	New       string    `json:"new,omitempty"`
	Text      string    `json:"text"` // the change in a sentence
}

// Log is the history.
type Log struct {
	mu     sync.Mutex
	path   string
	events []Event
	size   int64
}

// Open loads the history from path (a missing file is none). An empty path keeps it in
// memory only.
func Open(path string) (*Log, error) {
	l := &Log{path: path}
	if path == "" {
		return l, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Kind != "" {
			l.events = append(l.events, e)
		}
	}
	if len(l.events) > keep {
		l.events = l.events[len(l.events)-keep:]
	}
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
	return l, nil
}

// Path is the file the history is kept in ("" when it is only in memory).
func (l *Log) Path() string { return l.path }

// Add records an event: in the file, in memory and, as a line starting "changes:", in the
// ordinary log. The same change seen again within a few hours (two sources report one
// station's new name) is recorded once.
func (l *Log) Add(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	l.mu.Lock()
	for i := len(l.events) - 1; i >= 0 && e.Time.Sub(l.events[i].Time) < dedupe; i-- {
		o := l.events[i]
		if o.Kind == e.Kind && o.Channel == e.Channel && o.Old == e.Old && o.New == e.New && (o.Frequency == e.Frequency || o.Frequency == 0 || e.Frequency == 0) {
			l.mu.Unlock()
			return
		}
	}
	l.events = append(l.events, e)
	if len(l.events) > keep {
		l.events = l.events[len(l.events)-keep:]
	}
	if l.path != "" {
		l.appendLocked(e)
	}
	l.mu.Unlock()
	log.Printf("changes: %s", e.Text)
}

func (l *Log) appendLocked(e Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	if l.size+int64(len(b)) > maxBytes {
		os.Rename(l.path, l.path+".1")
		l.size = 0
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("changes: cannot write %s: %v", l.path, err)
		return
	}
	defer f.Close()
	if n, err := f.Write(b); err == nil {
		l.size += int64(n)
	}
}

// List returns the newest n events, oldest first (all of them for n <= 0).
func (l *Log) List(n int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	ev := l.events
	if n > 0 && len(ev) > n {
		ev = ev[len(ev)-n:]
	}
	return append([]Event(nil), ev...)
}
