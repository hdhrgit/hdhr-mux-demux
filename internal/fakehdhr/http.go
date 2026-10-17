package fakehdhr

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hdhrmux/internal/ts"
)

var reHTTPCh = regexp.MustCompile(`^ch(\d+)(?:-(\d+))?$`)

// StreamPort is the port of the device's HTTP streaming service (the real
// one is 5004).
func (d *Device) StreamPort() int { return d.http.Addr().(*net.TCPAddr).Port }

func (d *Device) startHTTP() error {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	d.http = ln
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{tuner}/{channel}", d.serveHTTPStream)
	d.srv = &http.Server{Handler: mux}
	go d.srv.Serve(ln)
	return nil
}

func hdhrErr(w http.ResponseWriter, code int, hdhr string) {
	w.Header().Set("Server", "HDHomeRun/1.0")
	w.Header().Set("Connection", "close")
	if hdhr != "" {
		w.Header().Set("X-HDHomeRun-Error", hdhr)
	}
	w.WriteHeader(code)
}

// serveHTTPStream mimics GET /tunerN/ch<freq>[-<program>] on port 5004 of a
// real device, including its quirks: ATSC 3.0 needs a program and locks on
// tuners 0 and 1 only; a tuner in use answers 804; a dead channel is rejected
// after a delay with 807.
func (d *Device) serveHTTPStream(w http.ResponseWriter, r *http.Request) {
	which, chs := r.PathValue("tuner"), r.PathValue("channel")
	idx, err := strconv.Atoi(strings.TrimPrefix(which, "tuner"))
	m := reHTTPCh.FindStringSubmatch(chs)
	if err != nil || !strings.HasPrefix(which, "tuner") || idx < 0 || idx >= len(d.tuners) || m == nil {
		hdhrErr(w, 404, "")
		return
	}
	freq, _ := strconv.ParseUint(m[1], 10, 64)
	prog := m[2]

	d.mu.Lock()
	t := d.tuners[idx]
	if t.cancel != nil || t.lockkey != 0 {
		d.mu.Unlock()
		hdhrErr(w, 503, "804 Tuner In Use")
		return
	}
	fc := d.findChannel(freq)
	switch {
	case fc != nil && fc.ATSC3 && prog == "":
		d.mu.Unlock()
		hdhrErr(w, 404, "")
		return
	case fc != nil && fc.ATSC3 && idx > 1:
		d.mu.Unlock()
		hdhrErr(w, 503, "806 Tune Failed")
		return
	case fc == nil:
		d.mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		hdhrErr(w, 503, "807 No Video Data")
		return
	}
	ch := fc
	if prog != "" {
		var sel *Channel
		for _, p := range fc.Programs {
			if strconv.Itoa(int(p.Number)) == prog {
				c := *fc
				c.Programs = []Program{p}
				sel = &c
			}
		}
		if sel == nil {
			d.mu.Unlock()
			hdhrErr(w, 503, "806 Tune Failed")
			return
		}
		ch = sel
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	t.channel, t.freq = fmt.Sprintf("auto:%d", freq), freq
	t.lockkey, t.lockIP, t.target = 0xFFFFFFFE, host, "http://"+r.RemoteAddr
	cancel := make(chan struct{})
	t.cancel = cancel
	n := 0
	for _, o := range d.tuners {
		if o.cancel != nil {
			n++
		}
	}
	if n > d.peak {
		d.peak = n
	}
	d.tuneLog = append(d.tuneLog, t.channel)
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		if t.cancel == cancel {
			t.cancel = nil
		}
		t.channel, t.freq, t.lockkey, t.lockIP, t.target = "none", 0, 0, "", "none"
		d.mu.Unlock()
	}()

	h := w.Header()
	h.Set("Server", "HDHomeRun/1.0")
	h.Set("Content-Type", "video/mpeg")
	h.Set("Connection", "close")
	h["X-HDHomeRun-Resource"] = []string{fmt.Sprintf("tuner%d", idx)}
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	full := ts.FullSet()
	d.stream(t, ch, flushWriter{w, rc}, false, full, cancel)
}

type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if err == nil {
		err = f.rc.Flush()
	}
	return n, err
}
