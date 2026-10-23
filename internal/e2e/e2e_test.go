// Package e2e exercises the whole multiplexer against simulated HDHomeRun
// devices over real sockets.
package e2e

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"hdhrmux/internal/config"
	"hdhrmux/internal/fakehdhr"
	"hdhrmux/internal/guide"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/ts"
	"hdhrmux/internal/upstream"
	"hdhrmux/internal/vdev"
)

func init() { hub.GuideInterval = 200 * time.Millisecond } // streams hand over their guide quickly

var channels = []fakehdhr.Channel{
	{Frequency: 177000000, TSID: 1, Programs: []fakehdhr.Program{{Number: 1, VChannel: "7.1", Name: "ALPHA"}, {Number: 2, VChannel: "7.2", Name: "BETA"}}},
	{Frequency: 605000000, TSID: 3, ATSC3: true, Programs: []fakehdhr.Program{{Number: 1376, VChannel: "104.1", Name: "NEXT-A"}, {Number: 1380, VChannel: "107.1", Name: "NEXT-B"}, {Number: 1381, VChannel: "114.1", Name: "NEXT-C"}}},
	{Frequency: 617000000, TSID: 4, Programs: []fakehdhr.Program{{Number: 1, VChannel: "36.1", Name: "EPSILON"}}},
	{Frequency: 473000000, TSID: 2, Programs: []fakehdhr.Program{{Number: 1, VChannel: "14.1", Name: "GAMMA"}, {Number: 3, VChannel: "14.3", Name: "DELTA"}}},
}

var testLineup = []lineup.Entry{
	{GuideNumber: "7.1", GuideName: "ALPHA", Frequency: 177000000, Program: 1},
	{GuideNumber: "7.2", GuideName: "BETA", Frequency: 177000000, Program: 2},
	{GuideNumber: "14.1", GuideName: "GAMMA", Frequency: 473000000, Program: 1},
	{GuideNumber: "14.3", GuideName: "DELTA", Frequency: 473000000, Program: 3},
}

type env struct {
	t     *testing.T
	fakes []*fakehdhr.Device
	dev   *vdev.Device
	hub   *hub.Hub
	lu    *lineup.Lineup
	http  string // streaming base URL (port 5004 equivalent)
	api   string // web/API base URL (port 80 equivalent)
	ctl   string // host:port
}

// Knobs a test may set (and must restore) before calling setup.
var (
	testMode     = ""                        // upstream transport: "" = http, or "rtp"
	testLinger   = time.Duration(0)          // hub linger
	testBuffer   = 0                         // hub client buffer in bytes (0 = default)
	testTuners   func(ids []uint32) []string // upstream tuner list, built from the fake device IDs
	testChannels []fakehdhr.Channel          // what the fake devices broadcast (nil = channels)
	testMask     = false                     // ask the devices to send only the PIDs being watched
	testModels   []string                    // device models by index (default: ATSC 3.0 capable)
	testSEQRamp  time.Duration               // fake devices report a symbol quality of 0 this long after a tune
	testSettle   time.Duration               // hub.SetSignalSettle
	testNoTuning bool                        // fake devices ignore lineup.json?tuning (older firmware)
)

func model(i int) string {
	if i < len(testModels) {
		return testModels[i]
	}
	return ""
}

func setup(t *testing.T, physTuners []int, virtual int, mod func(*vdev.Config)) *env {
	t.Helper()
	e := &env{t: t}
	var addrs []string
	for i, n := range physTuners {
		f, err := fakehdhr.Start(fakehdhr.Config{DeviceID: hdhrproto.MakeDeviceID(0x10200000 + uint32(i)<<8), Tuners: n, Channels: pick(testChannels, channels), Model: model(i), SEQRamp: testSEQRamp, NoTuning: testNoTuning})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(f.Close)
		e.fakes = append(e.fakes, f)
		addrs = append(addrs, f.Addr())
	}
	var tuners []string
	if testTuners != nil {
		var ids []uint32
		for _, f := range e.fakes {
			ids = append(ids, f.ID())
		}
		tuners = testTuners(ids)
	}
	pool, err := upstream.NewPool(upstream.Config{Devices: addrs, WaitFree: 2 * time.Second, Mode: testMode, Tuners: tuners})
	if err != nil {
		t.Fatal(err)
	}
	pool.Refresh()
	if got, want := len(pool.Devices()), len(physTuners); got != want {
		t.Fatalf("discovered %d devices, want %d", got, want)
	}
	for _, f := range e.fakes {
		pool.SetStreamPort(f.ID(), f.StreamPort())
		pool.SetWebPort(f.ID(), f.WebPort())
	}
	e.hub = hub.New(pool, hub.Config{Linger: testLinger, ClientBuffer: testBuffer, Mask: testMask})
	t.Cleanup(e.hub.Close)
	e.hub.SetSignalSettle(testSettle)
	e.lu, _ = lineup.Open("")
	e.lu.Replace(testLineup)
	cfg := vdev.Config{
		DeviceID: hdhrproto.MakeDeviceID(0x10900000), Tuners: virtual,
		HTTPAddr: "127.0.0.1:0", StreamAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0",
		ScanLockWait: 100 * time.Millisecond, ScanInfoWait: 3 * time.Second,
	}
	if mod != nil {
		mod(&cfg)
	}
	e.dev, err = vdev.New(cfg, e.hub, e.lu)
	if err != nil {
		t.Fatal(err)
	}
	e.dev.SetGuide(func() *guide.Store { g, _ := guide.Open(""); return g }())
	if err := e.dev.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.dev.Close)
	e.http = fmt.Sprintf("http://127.0.0.1:%d", e.dev.StreamPort())
	e.api = fmt.Sprintf("http://127.0.0.1:%d", e.dev.HTTPPort())
	e.ctl = fmt.Sprintf("127.0.0.1:%d", e.dev.ControlPort())
	return e
}

func (e *env) activePhysical() int {
	n := 0
	for _, f := range e.fakes {
		n += f.Active()
	}
	return n
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// client is an HTTP stream consumer collecting raw TS bytes.
type client struct {
	cancel context.CancelFunc
	resp   *http.Response
	data   chan []byte
}

func (e *env) open(path string) (*client, *http.Response) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.http+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		cancel()
		return nil, resp
	}
	c := &client{cancel: cancel, resp: resp, data: make(chan []byte, 4096)}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				c.data <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				close(c.data)
				return
			}
		}
	}()
	return c, resp
}

func (c *client) close() { c.cancel(); c.resp.Body.Close() }

// read collects at least n bytes.
func (c *client) read(t *testing.T, n int) []byte {
	t.Helper()
	var out []byte
	deadline := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case b, ok := <-c.data:
			if !ok {
				t.Fatalf("stream ended after %d bytes", len(out))
			}
			out = append(out, b...)
		case <-deadline:
			t.Fatalf("timeout: got %d of %d bytes", len(out), n)
		}
	}
	return out
}

type analysis struct {
	pids     map[uint16]int
	patProgs map[uint16]uint16
	esProgs  map[uint16]bool // program numbers seen in ES payloads
	ccErrors int
}

// analyze checks sync, continuity and collects what the stream contains.
func analyze(t *testing.T, b []byte) analysis {
	t.Helper()
	a := analysis{pids: map[uint16]int{}, esProgs: map[uint16]bool{}}
	lastCC := map[uint16]int{}
	var patR ts.SectionReader
	// Skip any partial first packet (HTTP chunk boundaries are packet aligned
	// here, but be tolerant).
	for len(b) >= 188 {
		p := b[:188]
		b = b[188:]
		if p[0] != 0x47 {
			t.Fatalf("lost sync")
		}
		pid := ts.PID(p)
		a.pids[pid]++
		if c, ok := lastCC[pid]; ok && int(ts.CC(p)) != (c+1)&15 {
			a.ccErrors++
		}
		lastCC[pid] = int(ts.CC(p))
		switch {
		case pid == 0:
			for _, sec := range patR.Push(p) {
				if pat, err := ts.ParsePAT(sec); err == nil {
					a.patProgs = pat.Programs
				}
			}
		case pid >= 0x101 && pid&0xF != 0:
			a.esProgs[uint16(p[4])<<8|uint16(p[5])] = true
		}
	}
	return a
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("%s: %v", url, err)
	}
}

var extraLineup = []lineup.Entry{{GuideNumber: "36.1", GuideName: "EPSILON", Frequency: 617000000, Program: 1}}

var atsc3Lineup = []lineup.Entry{
	{GuideNumber: "104.1", GuideName: "NEXT-A", Frequency: 605000000, Program: 1376, Modulation: "atsc3"},
	{GuideNumber: "107.1", GuideName: "NEXT-B", Frequency: 605000000, Program: 1380, Modulation: "atsc3"},
	{GuideNumber: "114.1", GuideName: "NEXT-C", Frequency: 605000000, Program: 1381, Modulation: "atsc3"},
}

func lockedTuners(t *testing.T, f *fakehdhr.Device, n int) int {
	t.Helper()
	raw := hdhrproto.NewControl(f.Addr())
	defer raw.Close()
	locked := 0
	for i := 0; i < n; i++ {
		if v, err := raw.Get(fmt.Sprintf("/tuner%d/lockkey", i)); err == nil && v != "none" {
			locked++
		}
	}
	return locked
}

func soapCall(t *testing.T, base, path, service, action, args string) (int, string) {
	t.Helper()
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:` +
		action + ` xmlns:u="` + service + `">` + args + `</u:` + action + `></s:Body></s:Envelope>`
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", `"`+service+`#`+action+`"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func unesc(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#34;", `"`, "&amp;", "&").Replace(s)
}

type xmltv struct {
	Channels []struct {
		ID    string   `xml:"id,attr"`
		Names []string `xml:"display-name"`
	} `xml:"channel"`
	Programmes []struct {
		Start   string `xml:"start,attr"`
		Stop    string `xml:"stop,attr"`
		Channel string `xml:"channel,attr"`
		Title   string `xml:"title"`
		Desc    string `xml:"desc"`
	} `xml:"programme"`
}

func fetchEPG(t *testing.T, e *env, query string) xmltv {
	t.Helper()
	resp, err := http.Get(e.api + "/epg" + query)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /epg: %v %v", resp, err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Fatalf("content type %q", ct)
	}
	var x xmltv
	if err := xml.NewDecoder(resp.Body).Decode(&x); err != nil {
		t.Fatalf("XMLTV does not parse: %v", err)
	}
	return x
}

// pidsOf renders a PID set for failure messages.
func pidsOf(s ts.PIDSet) string { return s.String() }

// physFilter finds the (single) streaming tuner of the first fake device and
// returns its filter.
func physFilter(e *env) (ts.PIDSet, bool) {
	for i := 0; i < 2; i++ {
		if e.fakes[0].TunedChannels()[i] != "none" {
			return e.fakes[0].TunerFilter(i)
		}
	}
	return ts.FullSet(), false
}

func pick(a, b []fakehdhr.Channel) []fakehdhr.Channel {
	if a != nil {
		return a
	}
	return b
}

// mgr is a settings manager that can read its configuration again, as in main.
func mgr(path string, cfg *config.Config) *config.Manager {
	m := config.NewManager(path, cfg)
	m.SetLoader(func() (*config.Config, error) { return config.Load(path) })
	return m
}

type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncLog) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }

func (s *syncLog) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// withoutTuningInfo makes the simulated devices answer lineup.json?tuning as older
// firmware does (no frequencies): the multiplexer then has to learn where channels
// are by tuning them.
func withoutTuningInfo(t *testing.T) {
	t.Helper()
	old := testNoTuning
	testNoTuning = true
	t.Cleanup(func() { testNoTuning = old })
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
