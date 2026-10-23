package vdev

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// The Logs page: what the program has logged lately, and the log followed live as
// Server-Sent Events (plain HTTP: no library, and it passes a proxy without special
// settings; the browser reconnects by itself and says where it was with Last-Event-ID).

const (
	logsFirst = 500  // lines sent first when a browser connects fresh
	logsMax   = 5000 // lines in one answer at most
	logsPing  = 15 * time.Second
)

func (d *Device) logsCheck(w http.ResponseWriter, r *http.Request) bool {
	if d.cfg.Logs == nil {
		http.NotFound(w, r)
		return false
	}
	return d.adminCheck(w, r, false) // log lines show clients' addresses: the local network only
}

// handleUILogs answers with lines as JSON: ?since=ID for the ones after that, ?n= for
// how many (the newest ones without since); ?download=1 as a text file.
func (d *Device) handleUILogs(w http.ResponseWriter, r *http.Request) {
	if !d.logsCheck(w, r) {
		return
	}
	q := r.URL.Query()
	since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
	n, _ := strconv.Atoi(q.Get("n"))
	if n < 1 || n > logsMax {
		n = logsFirst
	}
	lines := d.cfg.Logs.Since(since, n)
	w.Header().Set("Cache-Control", "no-store")
	if q.Get("download") == "1" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="hdhr-mux.log"`)
		for _, l := range d.cfg.Logs.Since(0, logsMax) {
			fmt.Fprintln(w, l.Text)
		}
		return
	}
	if lines == nil {
		writeJSON(w, map[string]any{"lines": []any{}})
		return
	}
	writeJSON(w, map[string]any{"lines": lines})
}

// handleUILogsStream sends the log as it grows. A browser that connects for the first
// time gets the newest lines, one that reconnects (Last-Event-ID, or ?since=) gets what
// it missed.
func (d *Device) handleUILogsStream(w http.ResponseWriter, r *http.Request) {
	if !d.logsCheck(w, r) {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not possible here", http.StatusInternalServerError)
		return
	}
	var last uint64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		last, _ = strconv.ParseUint(v, 10, 64)
	} else if v := r.URL.Query().Get("since"); v != "" {
		last, _ = strconv.ParseUint(v, 10, 64)
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: do not hold the events back
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	notify, cancel := d.cfg.Logs.Subscribe()
	defer cancel()
	tick := time.NewTicker(logsPing)
	defer tick.Stop()
	first := last == 0
	for {
		max := logsMax
		if first {
			max, first = logsFirst, false
		}
		for _, l := range d.cfg.Logs.Since(last, max) {
			// one line per event: a log line has no newline in it
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", l.ID, l.Text); err != nil {
				return
			}
			last = l.ID
		}
		fl.Flush()
		select {
		case <-notify:
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-r.Context().Done():
			return
		case <-d.quit:
			return
		}
	}
}
