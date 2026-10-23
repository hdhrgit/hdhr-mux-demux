package e2e

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"hdhrmux/internal/hdhrproto"
)

// /metrics speaks the Prometheus text format and reflects what is happening.
func TestMetrics(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	c1, _ := e.open("/auto/v7.1")
	c2, _ := e.open("/auto/v7.2")
	if c1 == nil || c2 == nil {
		t.Fatal("stream refused")
	}
	defer c1.close()
	defer c2.close()
	c1.read(t, 188*200)
	_, resp := e.open("/tuner1/v14.1/nonsense") // a request that is refused
	_ = resp

	get := func() map[string]float64 {
		r, err := http.Get(e.api + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("content type %q", ct)
		}
		b, _ := io.ReadAll(r.Body)
		out := map[string]float64{}
		helped := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.HasPrefix(line, "# HELP ") {
				helped[strings.Fields(line)[2]] = true
				continue
			}
			if strings.HasPrefix(line, "#") {
				continue
			}
			i := strings.LastIndex(line, " ")
			if i < 0 {
				t.Fatalf("bad line %q", line)
			}
			v, err := strconv.ParseFloat(line[i+1:], 64)
			if err != nil {
				t.Fatalf("bad value in %q", line)
			}
			out[line[:i]] = v
			name := line[:i]
			if j := strings.Index(name, "{"); j >= 0 {
				name = name[:j]
			}
			if !helped[name] {
				t.Fatalf("no HELP before %q", line)
			}
		}
		return out
	}
	waitFor(t, "stream bitrate in metrics", 5*time.Second, func() bool {
		return get()[`hdhrmux_stream_received_bits_per_second{tuner="`+hdhrproto.FormatDeviceID(e.fakes[0].ID())+`/0",channel="177000000"}`] > 0
	})
	m := get()
	for k, want := range map[string]float64{
		"hdhrmux_physical_tuners":                   2,
		"hdhrmux_physical_tuners_in_use":            1,
		"hdhrmux_virtual_tuners":                    2,
		"hdhrmux_viewers":                           2,
		"hdhrmux_streams_opened_total":              1,
		`hdhrmux_stream_requests_total{code="200"}`: 2,
	} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	if m["process_cpu_seconds_total"] <= 0 || m["go_goroutines"] <= 0 {
		t.Errorf("process metrics missing: cpu %v goroutines %v", m["process_cpu_seconds_total"], m["go_goroutines"])
	}
}

// The web interface: its pages, the activity feed with who watches what and the
// programme on now, and the redirect for browsers.
func TestWebInterface(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	c, _ := e.open("/auto/v7.1")
	if c == nil {
		t.Fatal("stream refused")
	}
	defer c.close()
	c.read(t, 188*100)

	type activity struct {
		Device  struct{ ID, Name string }
		Summary struct{ Viewers, TunersInUse, Tuners, Physical int }
		Tuners  []struct {
			Index       int
			State       string
			Physical    string
			RxBps       int
			Signal      *struct{ Strength, Quality int }
			PhysicalURL string
			Viewers     []struct {
				Client, Channel, Name string
				Now, Next             *struct{ Title string }
				Since                 int64
			}
		}
		Counters map[string]uint64
	}
	var a activity
	waitFor(t, "guide titles in the activity feed", 10*time.Second, func() bool {
		a = activity{}
		getJSON(t, e.api+"/ui/api/activity", &a)
		return len(a.Tuners) > 0 && len(a.Tuners[0].Viewers) > 0 && a.Tuners[0].Viewers[0].Now != nil
	})
	if a.Summary.Viewers != 1 || a.Summary.TunersInUse != 1 || a.Summary.Tuners != 2 || a.Device.Name == "" {
		t.Errorf("summary %+v device %+v", a.Summary, a.Device)
	}
	t0 := a.Tuners[0]
	v := t0.Viewers[0]
	if t0.State != "watching" || v.Client != "127.0.0.1" || v.Channel != "7.1" || v.Name != "ALPHA" ||
		v.Now.Title != "Now Show ALPHA" || v.Next == nil || v.Next.Title != "Next Show ALPHA" || v.Since == 0 {
		t.Errorf("tuner 0: %+v", t0)
	}
	if !strings.HasSuffix(t0.PhysicalURL, "/tuners.html?page=tuner0") {
		t.Errorf("physical tuner link %q (physical %q)", t0.PhysicalURL, t0.Physical)
	}
	if t0.Signal == nil || t0.Signal.Strength == 0 {
		t.Errorf("no signal reported: %+v", t0.Signal)
	}
	if a.Tuners[1].State != "idle" || len(a.Tuners[1].Viewers) != 0 {
		t.Errorf("tuner 1: %+v", a.Tuners[1])
	}
	if a.Counters["streamsOpened"] != 1 {
		t.Errorf("counters %v", a.Counters)
	}

	var lu struct {
		Channels []struct {
			Number, Name string
			Watching     int
			Now          *struct{ Title string }
			URL          string
		}
	}
	getJSON(t, e.api+"/ui/api/lineup", &lu)
	found := false
	for _, ch := range lu.Channels {
		if ch.Number == "7.1" {
			found = true
			if ch.Watching != 1 || ch.Now == nil || !strings.HasSuffix(ch.URL, "/auto/v7.1") {
				t.Errorf("7.1: %+v", ch)
			}
		}
	}
	if !found || len(lu.Channels) != len(testLineup) {
		t.Errorf("lineup api: %d channels, found 7.1: %v", len(lu.Channels), found)
	}

	for path, want := range map[string]string{"/ui/": "text/html", "/ui/app.js": "text/javascript", "/ui/app.css": "text/css", "/ui/favicon.svg": "image/svg+xml"} {
		r, err := http.Get(e.api + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), want) || len(b) < 100 {
			t.Errorf("%s: %d %q (%d bytes)", path, r.StatusCode, r.Header.Get("Content-Type"), len(b))
		}
		if path == "/ui/" && (strings.Contains(string(b), "{{NAME}}") || !strings.Contains(string(b), a.Device.Name)) {
			t.Errorf("index did not get the device name: %.200s", b)
		}
	}

	// browsers go to the interface; plain clients still get the classic page
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", e.api+"/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	r, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 302 || r.Header.Get("Location") != "/ui/" {
		t.Errorf("browser root: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	r, err = noFollow.Get(e.api + "/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Errorf("plain root: %d", r.StatusCode)
	}
}
