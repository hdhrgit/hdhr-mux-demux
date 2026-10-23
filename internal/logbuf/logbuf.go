// Package logbuf keeps the last lines the program logged, so that the web interface can
// show them and follow them live. It is an io.Writer for the log package.
package logbuf

import (
	"sync"
)

// maxLine cuts a line that is longer than any log line should be.
const maxLine = 2048

// Line is one log line with its sequence number (1 for the first, then increasing).
type Line struct {
	ID   uint64 `json:"id"`
	Text string `json:"text"`
}

// Buffer holds the most recent lines.
type Buffer struct {
	mu      sync.Mutex
	ring    []Line
	size    int
	count   int    // lines held, at most size
	last    uint64 // the number of the newest line (0: none yet)
	partial []byte // the start of a line whose end has not been written yet
	subs    map[chan struct{}]struct{}
}

// New returns a buffer that keeps the last capacity lines.
func New(capacity int) *Buffer {
	if capacity < 1 {
		capacity = 1
	}
	return &Buffer{ring: make([]Line, capacity), size: capacity, subs: map[chan struct{}]struct{}{}}
}

// Write splits what is written into lines (the log package writes one line per call).
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	data := append(b.partial, p...)
	b.partial = nil
	for {
		i := indexNL(data)
		if i < 0 {
			break
		}
		b.addLocked(string(trim(data[:i])))
		data = data[i+1:]
	}
	if len(data) > 0 {
		b.partial = append([]byte(nil), data...)
		if len(b.partial) > maxLine*4 { // never a line end: do not grow for ever
			b.addLocked(string(trim(b.partial)))
			b.partial = nil
		}
	}
	subs := make([]chan struct{}, 0, len(b.subs))
	for c := range b.subs {
		subs = append(subs, c)
	}
	b.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- struct{}{}:
		default: // already told
		}
	}
	return len(p), nil
}

func indexNL(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

func trim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	if len(b) > maxLine {
		b = b[:maxLine]
	}
	return b
}

func (b *Buffer) addLocked(text string) {
	b.last++
	b.ring[(b.last-1)%uint64(b.size)] = Line{ID: b.last, Text: text}
	if b.count < b.size {
		b.count++
	}
}

// Since returns the lines newer than id, oldest first, at most max of them. With id 0 it
// returns the newest max lines. A caller that has fallen further behind than the buffer
// holds gets what is still there.
func (b *Buffer) Since(id uint64, max int) []Line {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last == 0 || max < 1 {
		return nil
	}
	oldest := b.last - uint64(b.count) + 1
	from := id + 1
	if id == 0 && uint64(max) < uint64(b.count) {
		from = b.last - uint64(max) + 1
	}
	if from < oldest {
		from = oldest
	}
	var out []Line
	for n := from; n <= b.last && len(out) < max; n++ {
		out = append(out, b.ring[(n-1)%uint64(b.size)])
	}
	return out
}

// Subscribe returns a channel that is sent to when new lines have been written (it holds
// at most one notification: read the lines with Since) and a function that ends the
// subscription.
func (b *Buffer) Subscribe() (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[c] = struct{}{}
	b.mu.Unlock()
	return c, func() {
		b.mu.Lock()
		delete(b.subs, c)
		b.mu.Unlock()
	}
}
