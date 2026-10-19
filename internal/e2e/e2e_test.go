// Package e2e exercises the whole multiplexer against simulated HDHomeRun
// devices over real sockets.
package e2e

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"hdhrmux/internal/chanmap"
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
	testMode   = ""                        // upstream transport: "" = http, or "rtp"
	testLinger = time.Duration(0)          // hub linger
	testBuffer = 0                         // hub client buffer in bytes (0 = default)
	testTuners func(ids []uint32) []string // upstream tuner list, built from the fake device IDs
	testMask   = false                     // ask the devices to send only the PIDs being watched
)

func setup(t *testing.T, physTuners []int, virtual int, mod func(*vdev.Config)) *env {
	t.Helper()
	e := &env{t: t}
	var addrs []string
	for i, n := range physTuners {
		f, err := fakehdhr.Start(fakehdhr.Config{DeviceID: hdhrproto.MakeDeviceID(0x10200000 + uint32(i)<<8), Tuners: n, Channels: channels})
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

func TestDiscoveryAndDescriptors(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	infos, err := hdhrproto.Discover([]string{e.ctl}, time.Second)
	if err != nil || len(infos) != 1 {
		t.Fatalf("discover: %v %v", infos, err)
	}
	inf := infos[0]
	if inf.DeviceID != e.dev.ID() || inf.TunerCount != 2 || inf.DeviceType != hdhrproto.DeviceTypeTuner {
		t.Fatalf("bad discover reply %+v", inf)
	}
	if want := e.api; inf.BaseURL != want {
		t.Fatalf("BaseURL %q want %q", inf.BaseURL, want)
	}

	var dj map[string]any
	getJSON(t, e.api+"/discover.json", &dj)
	if dj["DeviceID"] != hdhrproto.FormatDeviceID(e.dev.ID()) || dj["TunerCount"].(float64) != 2 {
		t.Fatalf("discover.json %v", dj)
	}
	var lj []map[string]any
	getJSON(t, e.api+"/lineup.json", &lj)
	if len(lj) != 4 || lj[0]["GuideNumber"] != "7.1" || lj[0]["URL"] != e.http+"/auto/v7.1" {
		t.Fatalf("lineup.json %v", lj)
	}
	resp, err := http.Get(e.api + "/lineup.xml")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<GuideNumber>14.3</GuideNumber>") {
		t.Fatalf("lineup.xml: %s", body)
	}
	var ls map[string]any
	getJSON(t, e.api+"/lineup_status.json", &ls)
	if ls["ScanPossible"].(float64) != 1 {
		t.Fatalf("lineup_status %v", ls)
	}
	// wrong device id must not be answered
	req := &hdhrproto.Packet{Type: hdhrproto.TypeDiscoverReq}
	req.AddU32(hdhrproto.TagDeviceType, hdhrproto.DeviceTypeTuner)
	req.AddU32(hdhrproto.TagDeviceID, 0x10000001)
	c, _ := net.Dial("udp", e.ctl)
	c.Write(req.Encode())
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := c.Read(make([]byte, 100)); err == nil {
		t.Fatalf("unexpected reply of %d bytes to foreign device id", n)
	}
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

// Many viewers of programs on the same RF channel must share one physical
// tuner, and each must receive only its own program.
func TestSharedTunerDemux(t *testing.T) {
	e := setup(t, []int{2}, 4, nil)

	c71, _ := e.open("/auto/v7.1")
	c72, _ := e.open("/auto/v7.2")
	c141, _ := e.open("/auto/v14.1")
	c143, _ := e.open("/auto/ch473000000-3") // raw frequency-program form
	for _, c := range []*client{c71, c72, c141, c143} {
		if c == nil {
			t.Fatal("stream refused")
		}
		defer c.close()
	}

	check := func(name string, c *client, prog uint16, base uint16) {
		a := analyze(t, c.read(t, 188*400))
		if a.ccErrors != 0 {
			t.Errorf("%s: %d continuity errors", name, a.ccErrors)
		}
		if len(a.patProgs) != 1 || a.patProgs[prog] != base {
			t.Errorf("%s: PAT %v, want only program %d -> pmt 0x%x", name, a.patProgs, prog, base)
		}
		for pid := range a.pids {
			if pid != 0 && (pid < base || pid > base+2) {
				t.Errorf("%s: leaked PID 0x%x", name, pid)
			}
		}
		if len(a.esProgs) != 1 || !a.esProgs[prog] {
			t.Errorf("%s: payload programs %v want only %d", name, a.esProgs, prog)
		}
		if a.pids[base+1] == 0 || a.pids[base+2] == 0 || a.pids[base] == 0 {
			t.Errorf("%s: missing PMT/video/audio: %v", name, a.pids)
		}
	}
	check("7.1", c71, 1, 0x100)
	check("7.2", c72, 2, 0x110)
	check("14.1", c141, 1, 0x100)
	check("14.3", c143, 3, 0x110)

	if got := e.activePhysical(); got != 2 {
		t.Fatalf("4 viewers on 2 RF channels used %d physical tuners, want 2", got)
	}
	if got := e.fakes[0].TuneCount(); got != 2 {
		t.Fatalf("tuned %d times, want 2", got)
	}

	// /auto/ reuses tuners: four viewers of two RF channels occupy only two
	// of the four virtual tuners, the rest stay free.
	var st []map[string]any
	getJSON(t, e.api+"/status.json", &st)
	busyTuners := 0
	for _, s := range st {
		if s["VctNumber"] != nil {
			busyTuners++
		}
	}
	if busyTuners != 2 {
		t.Fatalf("%d virtual tuners busy for 2 RF channels, want 2: %v", busyTuners, st)
	}

	// 177 MHz is already streaming, so one more viewer of it joins the tuner
	// that carries it.
	c5, resp := e.open("/auto/v7.1")
	if c5 == nil {
		t.Fatalf("5th viewer of a streaming channel refused: %d %v", resp.StatusCode, resp.Header)
	}
	defer c5.close()
	if an := analyze(t, c5.read(t, 188*100)); an.ccErrors != 0 || !an.esProgs[1] || len(an.esProgs) != 1 {
		t.Fatalf("joined viewer content: %v cc=%d", an.esProgs, an.ccErrors)
	}
	// ...but a channel nobody streams cannot be served: no physical tuner left.
	e.lu.Replace(append(append([]lineup.Entry{}, testLineup...), extraLineup...))
	_, resp = e.open("/auto/v36.1")
	if resp.StatusCode != 503 || resp.Header.Get("X-HDHomeRun-Error") != "805 All Tuners In Use" {
		t.Fatalf("new channel with everything busy: %d %v", resp.StatusCode, resp.Header)
	}
	// explicit busy tuner carrying a different channel (tuner1 has 473 MHz)
	_, resp = e.open("/tuner1/v7.1")
	if resp.StatusCode != 503 || resp.Header.Get("X-HDHomeRun-Error") != "804 Tuner In Use" {
		t.Fatalf("busy tuner1: %d %v", resp.StatusCode, resp.Header)
	}
	c5.close()

	c71.close()
	c72.close()
	waitFor(t, "RF 177 released", 3*time.Second, func() bool { return e.activePhysical() == 1 })
	c141.close()
	c143.close()
	waitFor(t, "all released", 3*time.Second, func() bool { return e.activePhysical() == 0 })
}

// Combining devices: viewers on distinct RF channels spread over devices.
func TestMultipleDevicesPooled(t *testing.T) {
	e := setup(t, []int{1, 1}, 2, nil)
	a, _ := e.open("/auto/v7.1")
	b, _ := e.open("/auto/v14.1")
	if a == nil || b == nil {
		t.Fatal("expected both streams")
	}
	defer a.close()
	defer b.close()
	a.read(t, 188*50)
	b.read(t, 188*50)
	if e.fakes[0].Active() != 1 || e.fakes[1].Active() != 1 {
		t.Fatalf("expected one active tuner on each device, got %d and %d", e.fakes[0].Active(), e.fakes[1].Active())
	}
	var st []map[string]any
	getJSON(t, e.api+"/status.json", &st)
	if len(st) != 2 || st[0]["VctNumber"] == nil {
		t.Fatalf("status.json %v", st)
	}
}

// With every physical tuner busy a new RF channel is refused; once a tuner
// frees up (even moments later) the request succeeds.
func TestPhysicalExhaustionAndHandover(t *testing.T) {
	e := setup(t, []int{1}, 3, nil)
	a, _ := e.open("/auto/v7.1")
	defer a.close()
	a.read(t, 188*20)

	start := time.Now()
	_, resp := e.open("/auto/v14.1")
	if resp.StatusCode != 503 || !strings.HasPrefix(resp.Header.Get("X-HDHomeRun-Error"), "805") {
		t.Fatalf("expected 805, got %d %v", resp.StatusCode, resp.Header)
	}
	if time.Since(start) < time.Second {
		t.Fatalf("refusal returned in %v; expected to wait for a free tuner first", time.Since(start))
	}
	// Releasing A while B is waiting hands the tuner over.
	done := make(chan *client, 1)
	go func() { c, _ := e.open("/auto/v14.1"); done <- c }()
	time.Sleep(300 * time.Millisecond)
	a.close()
	select {
	case c := <-done:
		if c == nil {
			t.Fatal("handover request failed")
		}
		defer c.close()
		c.read(t, 188*20)
	case <-time.After(5 * time.Second):
		t.Fatal("handover timed out")
	}
}

func TestUnknownChannelAndTuner(t *testing.T) {
	e := setup(t, []int{1}, 1, nil)
	for path, want := range map[string]int{
		"/auto/v99.9":   404,
		"/tuner5/v7.1":  404,
		"/auto/garbage": 404,
		"/bogus/v7.1":   404,
		"/nothere.json": 404,
	} {
		_, resp := e.open(path)
		if resp.StatusCode != want {
			t.Errorf("%s: %d want %d", path, resp.StatusCode, want)
		}
	}
}

func TestControlProtocolAndRTP(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	c := hdhrproto.NewControl(e.ctl)
	defer c.Close()

	mustGet := func(name string) string {
		t.Helper()
		v, err := c.Get(name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return v
	}
	mustSet := func(name, val string) {
		t.Helper()
		if _, err := c.Set(name, val, 0); err != nil {
			t.Fatalf("set %s %s: %v", name, val, err)
		}
	}
	if v := mustGet("/sys/hwmodel"); v != "HDHR-MUX" {
		t.Fatalf("hwmodel %q", v)
	}
	if v := mustGet("/tuner0/status"); !strings.HasPrefix(v, "ch=none lock=none") {
		t.Fatalf("idle status %q", v)
	}
	if _, err := c.Get("/nonsense"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected unknown variable error, got %v", err)
	}
	if v := mustGet("help"); !strings.Contains(v, "/tuner<n>/streaminfo") {
		t.Fatalf("help: %q", v)
	}

	rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()

	mustSet("/tuner0/channel", "auto:177000000")
	mustSet("/tuner0/program", "2")
	mustSet("/tuner0/target", "rtp://"+rx.LocalAddr().String())

	var got []byte
	var lastSeq uint16
	haveSeq := false
	rx.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	for len(got) < 188*300 {
		n, err := rx.Read(buf)
		if err != nil {
			t.Fatalf("rtp read: %v (got %d bytes)", err, len(got))
		}
		if n != 12+7*188 { // libhdhomerun clients reject any other datagram size
			t.Fatalf("rtp datagram of %d bytes, want %d", n, 12+7*188)
		}
		if buf[0]>>6 != 2 || buf[1]&0x7F != 33 {
			t.Fatalf("not an MP2T RTP packet: % x", buf[:12])
		}
		seq := uint16(buf[2])<<8 | uint16(buf[3])
		if haveSeq && seq != lastSeq+1 {
			t.Fatalf("rtp sequence jump %d -> %d", lastSeq, seq)
		}
		lastSeq, haveSeq = seq, true
		got = append(got, buf[12:n]...)
	}
	a := analyze(t, got)
	if len(a.esProgs) != 1 || !a.esProgs[2] || len(a.patProgs) != 1 {
		t.Fatalf("rtp content wrong: progs=%v pat=%v", a.esProgs, a.patProgs)
	}

	st := mustGet("/tuner0/status")
	if !strings.Contains(st, "ch=8vsb:177000000 lock=8vsb") || strings.Contains(st, "bps=0 ") {
		t.Fatalf("status %q", st)
	}
	if v := mustGet("/tuner0/program"); v != "2" {
		t.Fatalf("program %q", v)
	}
	if v := mustGet("/tuner0/filter"); v != "0x0000 0x0110-0x0112" {
		t.Fatalf("filter for program 2 = %q", v)
	}
	if info := mustGet("/tuner0/streaminfo"); !strings.Contains(info, "2: 7.2 BETA") || !strings.Contains(info, "tsid=0x0001") {
		t.Fatalf("streaminfo %q", info)
	}
	if v := mustGet("/tuner0/target"); !strings.HasPrefix(v, "rtp://") {
		t.Fatalf("target %q", v)
	}
	// An HTTP viewer on the same RF channel joins the same physical tuner.
	h, _ := e.open("/auto/v7.1")
	defer h.close()
	h.read(t, 188*20)
	if e.activePhysical() != 1 {
		t.Fatalf("expected 1 physical tuner, got %d", e.activePhysical())
	}
	h.close()

	// PID filter mode (no PAT rewriting)
	mustSet("/tuner0/filter", "0x0000 0x0100-0x0102")
	mustSet("/tuner0/target", "none")
	for { // discard stale RTP datagrams
		rx.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		if _, err := rx.Read(buf); err != nil {
			break
		}
	}
	mustSet("/tuner0/target", "udp://"+rx.LocalAddr().String())
	rx.SetReadDeadline(time.Now().Add(5 * time.Second))
	got = got[:0]
	for len(got) < 188*200 {
		n, err := rx.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n != 7*188 {
			t.Fatalf("udp datagram of %d bytes, want %d", n, 7*188)
		}
		got = append(got, buf[:n]...) // udp: no RTP header
	}
	a = analyze(t, got[:len(got)/188*188])
	for pid := range a.pids {
		if pid != 0 && (pid < 0x100 || pid > 0x102) {
			t.Fatalf("filter leaked pid 0x%x", pid)
		}
	}
	if len(a.patProgs) != 2 {
		t.Fatalf("explicit filter should pass the original PAT, got %v", a.patProgs)
	}

	mustSet("/tuner0/target", "none")
	mustSet("/tuner0/channel", "none")
	waitFor(t, "physical release", 2*time.Second, func() bool { return e.activePhysical() == 0 })
	if v := mustGet("/tuner0/channel"); v != "none" {
		t.Fatalf("channel %q", v)
	}
}

func TestLockkey(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	owner, other := hdhrproto.NewControl(e.ctl), hdhrproto.NewControl(e.ctl)
	defer owner.Close()
	defer other.Close()

	if v, _ := other.Get("/tuner0/lockkey"); v != "none" {
		t.Fatalf("lockkey %q", v)
	}
	if _, err := owner.Set("/tuner0/lockkey", "12345", 0); err != nil {
		t.Fatal(err)
	}
	if v, _ := other.Get("/tuner0/lockkey"); v != "127.0.0.1" {
		t.Fatalf("lockkey holder %q", v)
	}
	if _, err := other.Set("/tuner0/channel", "auto:177000000", 0); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("expected locked error, got %v", err)
	}
	if _, err := other.Set("/tuner0/lockkey", "999", 0); err == nil {
		t.Fatal("stealing lock should fail")
	}
	// reading is always allowed
	if _, err := other.Get("/tuner0/status"); err != nil {
		t.Fatal(err)
	}
	// HTTP cannot take a locked tuner, but /auto skips it
	_, resp := e.open("/tuner0/v7.1")
	if resp.StatusCode != 503 {
		t.Fatalf("locked tuner via http: %d", resp.StatusCode)
	}
	c, _ := e.open("/auto/v7.1")
	if c == nil {
		t.Fatal("auto should use the unlocked tuner")
	}
	c.close()
	if _, err := owner.Set("/tuner0/channel", "auto:177000000", 12345); err != nil {
		t.Fatalf("owner set: %v", err)
	}
	if _, err := other.Set("/tuner0/lockkey", "force", 0); err != nil {
		t.Fatal(err)
	}
	if v, _ := other.Get("/tuner0/lockkey"); v != "none" {
		t.Fatalf("after force: %q", v)
	}
	// force only drops the lock, as on real hardware: the tuner stays tuned,
	// and the old key is now stale.
	waitFor(t, "only the force-unlocked tuner left running", 3*time.Second, func() bool { return e.activePhysical() == 1 })
	if _, err := owner.Set("/tuner0/channel", "none", 12345); err == nil || !strings.Contains(err.Error(), "no longer held") {
		t.Fatalf("stale key: %v", err)
	}
	if _, err := other.Set("/tuner0/channel", "none", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tuner released", 2*time.Second, func() bool { return e.activePhysical() == 0 })
}

func TestIdleTunerReaped(t *testing.T) {
	e := setup(t, []int{1}, 1, func(c *vdev.Config) { c.IdleRelease = 200 * time.Millisecond })
	c := hdhrproto.NewControl(e.ctl)
	defer c.Close()
	if _, err := c.Set("/tuner0/channel", "auto:177000000", 0); err != nil {
		t.Fatal(err)
	}
	// tuned but no target: physical tuner is held without streaming
	waitFor(t, "reaper", 8*time.Second, func() bool {
		v, _ := c.Get("/tuner0/channel")
		return v == "none"
	})
}

func TestScan(t *testing.T) {
	e := setup(t, []int{1}, 1, func(c *vdev.Config) { c.ScanFrom, c.ScanTo = 5, 14 })
	e.lu.Replace(nil)
	resp, err := http.Post(e.api+"/lineup.post?scan=start&source=Antenna", "", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("scan start: %v %v", resp, err)
	}
	var ls map[string]any
	getJSON(t, e.api+"/lineup_status.json", &ls)
	if ls["ScanInProgress"].(float64) != 1 {
		t.Fatalf("expected scan in progress: %v", ls)
	}
	waitFor(t, "scan to finish", 30*time.Second, func() bool {
		getJSON(t, e.api+"/lineup_status.json", &ls)
		return ls["ScanInProgress"].(float64) == 0
	})
	var lj []map[string]any
	getJSON(t, e.api+"/lineup.json", &lj)
	var names []string
	for _, l := range lj {
		names = append(names, l["GuideNumber"].(string)+"="+l["GuideName"].(string))
	}
	// The channel list is the devices' (every program they list); the scan
	// taught where the ones inside its range are.
	if got := strings.Join(names, ","); got != "7.1=ALPHA,7.2=BETA,14.1=GAMMA,14.3=DELTA,36.1=EPSILON,104.1=NEXT-A,107.1=NEXT-B,114.1=NEXT-C" {
		t.Fatalf("lineup after scan %q", got)
	}
	for g, want := range map[string]uint64{"7.1": 177000000, "7.2": 177000000, "14.1": 473000000, "14.3": 473000000, "36.1": 0, "104.1": 0} {
		if en, _ := e.lu.Find(g); en.Frequency != want {
			t.Errorf("%s: frequency %d, want %d", g, en.Frequency, want)
		}
	}
	// scan released its tuner, and the scanned lineup is streamable
	c, _ := e.open("/auto/v14.3")
	if c == nil {
		t.Fatal("cannot stream scanned channel")
	}
	defer c.close()
	a := analyze(t, c.read(t, 188*100))
	if !a.esProgs[3] || len(a.esProgs) != 1 {
		t.Fatalf("scanned channel content %v", a.esProgs)
	}
}

// If a physical tuner stops delivering data, viewers are moved to another
// physical tuner without reconnecting.
func TestFailover(t *testing.T) {
	old := hub.StallLimit
	hub.StallLimit = 1500 * time.Millisecond
	defer func() { hub.StallLimit = old }()

	e := setup(t, []int{2}, 1, nil)
	c, _ := e.open("/auto/v7.1")
	defer c.close()
	c.read(t, 188*50)
	before := e.fakes[0].TunedChannels()

	e.fakes[0].KillStreams()
	// drain anything in flight, then expect data to resume on its own
	time.Sleep(200 * time.Millisecond)
loop:
	for {
		select {
		case <-c.data:
		default:
			break loop
		}
	}
	a := analyze(t, c.read(t, 188*100))
	if !a.esProgs[1] {
		t.Fatalf("no data after failover: %v", a.pids)
	}
	after := e.fakes[0].TunedChannels()
	t.Logf("tuners before %v after %v", before, after)
	if e.activePhysical() != 1 {
		t.Fatalf("active physical %d, want 1", e.activePhysical())
	}
}

var extraLineup = []lineup.Entry{{GuideNumber: "36.1", GuideName: "EPSILON", Frequency: 617000000, Program: 1}}

var atsc3Lineup = []lineup.Entry{
	{GuideNumber: "104.1", GuideName: "NEXT-A", Frequency: 605000000, Program: 1376, Modulation: "atsc3"},
	{GuideNumber: "107.1", GuideName: "NEXT-B", Frequency: 605000000, Program: 1380, Modulation: "atsc3"},
	{GuideNumber: "114.1", GuideName: "NEXT-C", Frequency: 605000000, Program: 1381, Modulation: "atsc3"},
}

// ATSC 3.0 devices only deliver one selected service per tuner, so services
// of the same RF channel cannot share a tuner (but identical ones still do),
// and only some tuners can lock at all.
func TestATSC3ServiceMode(t *testing.T) {
	e := setup(t, []int{4}, 6, nil)
	e.lu.Replace(append(append([]lineup.Entry{}, testLineup...), atsc3Lineup...))

	a, _ := e.open("/auto/v104.1")
	b, _ := e.open("/auto/v107.1")
	a2, _ := e.open("/auto/v104.1") // same service: shares
	if a == nil || b == nil || a2 == nil {
		t.Fatal("streams refused")
	}
	defer a.close()
	defer b.close()
	defer a2.close()
	for name, c := range map[string]struct {
		c    *client
		prog uint16
	}{"104.1": {a, 1376}, "107.1": {b, 1380}, "104.1b": {a2, 1376}} {
		an := analyze(t, c.c.read(t, 188*200))
		if an.ccErrors != 0 || len(an.esProgs) != 1 || !an.esProgs[c.prog] {
			t.Errorf("%s: cc=%d progs=%v want only %d", name, an.ccErrors, an.esProgs, c.prog)
		}
	}
	if got := e.activePhysical(); got != 2 {
		t.Fatalf("2 services used %d tuners, want 2", got)
	}
	// A third service needs a tuner that can lock ATSC 3.0, but only tuners
	// 0 and 1 can, and both are busy: refused as no signal, not hung.
	start := time.Now()
	_, resp := e.open("/auto/v114.1")
	if resp.StatusCode != 503 || !strings.HasPrefix(resp.Header.Get("X-HDHomeRun-Error"), "805") {
		t.Fatalf("3rd service: %d %v", resp.StatusCode, resp.Header)
	}
	t.Logf("refusal took %v", time.Since(start))
	if got := e.activePhysical(); got != 2 {
		t.Fatalf("failed attempt leaked tuners: %d active", got)
	}
}

// Without lineup knowledge (raw frequency + program, or the control protocol
// selecting the program after tuning) service mode is discovered on the fly.
func TestATSC3Discovery(t *testing.T) {
	e := setup(t, []int{4}, 4, nil)

	c, _ := e.open("/auto/ch605000000-1380")
	if c == nil {
		t.Fatal("raw ATSC 3.0 stream refused")
	}
	an := analyze(t, c.read(t, 188*200))
	c.close()
	if an.ccErrors != 0 || !an.esProgs[1380] || len(an.esProgs) != 1 {
		t.Fatalf("raw: cc=%d progs=%v", an.ccErrors, an.esProgs)
	}
	waitFor(t, "release", 3*time.Second, func() bool { return e.activePhysical() == 0 })

	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	rx, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer rx.Close()
	for _, kv := range [][2]string{{"channel", "auto:605000000"}, {"program", "1376"}, {"target", "udp://" + rx.LocalAddr().String()}} {
		if _, err := ctl.Set("/tuner0/"+kv[0], kv[1], 0); err != nil {
			t.Fatalf("set %s: %v", kv[0], err)
		}
	}
	read := func(want uint16) {
		t.Helper()
		var got []byte
		buf := make([]byte, 2048)
		for len(got) < 188*200 {
			rx.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := rx.Read(buf)
			if err != nil {
				t.Fatalf("program %d: %v", want, err)
			}
			got = append(got, buf[:n]...)
		}
		// skip datagrams from before the switch
		an := analyze(t, got[len(got)/2/188*188:])
		if !an.esProgs[want] || len(an.esProgs) != 1 {
			t.Fatalf("want only program %d, got %v", want, an.esProgs)
		}
	}
	read(1376)
	// switching service on the same RF channel moves to another tuner
	if _, err := ctl.Set("/tuner0/program", "1380", 0); err != nil {
		t.Fatal(err)
	}
	read(1380)
	if got := e.activePhysical(); got != 1 {
		t.Fatalf("active %d, want 1 (old service must be released)", got)
	}
}

// Someone force-releasing the lock on a physical tuner (real devices keep
// streaming) must not leave it permanently unmanaged; and if the tuner is
// also cleared, viewers must be moved rather than stranded.
func TestForcedUnlockRecovery(t *testing.T) {
	testMode = "rtp" // control locks only exist in push mode
	defer func() { testMode = "" }()
	e := setup(t, []int{2}, 1, nil)
	c, _ := e.open("/auto/v7.1")
	defer c.close()
	c.read(t, 188*50)

	raw := hdhrproto.NewControl(e.fakes[0].Addr())
	defer raw.Close()
	held := func() int {
		for i, ch := range e.fakes[0].TunedChannels() {
			if ch != "none" {
				return i
			}
		}
		return -1
	}
	idx := held()
	lockvar := fmt.Sprintf("/tuner%d/lockkey", idx)

	// 1. lock released externally: stream continues, mux re-takes the lock
	if _, err := raw.Set(lockvar, "force", 0); err != nil {
		t.Fatal(err)
	}
	if v, _ := raw.Get(lockvar); v != "none" {
		t.Fatalf("force did not release: %q", v)
	}
	waitFor(t, "lock re-taken", 8*time.Second, func() bool { v, _ := raw.Get(lockvar); return v != "none" })
	c.read(t, 188*50)

	// 2. lock released and tuner cleared: viewers move to the other tuner
	raw.Set(lockvar, "force", 0)
	if _, err := raw.Set(fmt.Sprintf("/tuner%d/channel", idx), "none", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
loop:
	for {
		select {
		case <-c.data:
		default:
			break loop
		}
	}
	an := analyze(t, c.read(t, 188*100))
	if !an.esProgs[1] {
		t.Fatalf("stream did not recover: %v", an.pids)
	}

	// 3. when the viewer leaves, every physical tuner is clean again
	c.close()
	waitFor(t, "all tuners idle", 3*time.Second, func() bool {
		for i := 0; i < 2; i++ {
			l, _ := raw.Get(fmt.Sprintf("/tuner%d/lockkey", i))
			ch, _ := raw.Get(fmt.Sprintf("/tuner%d/channel", i))
			tg, _ := raw.Get(fmt.Sprintf("/tuner%d/target", i))
			if l != "none" || ch != "none" || tg != "none" {
				return false
			}
		}
		return true
	})
}

// ATSC 1.0 streams must not occupy the ATSC 3.0-capable tuners (0 and 1 of a
// 4K device) while other tuners are free.
func TestAvoidsATSC3TunersForATSC1(t *testing.T) {
	e := setup(t, []int{4}, 4, nil)
	e.lu.Replace(append(append([]lineup.Entry{}, testLineup...), extraLineup...))
	busy := func() []bool {
		var out []bool
		for _, ch := range e.fakes[0].TunedChannels() {
			out = append(out, ch != "none")
		}
		return out
	}
	// /auto/ keeps the 3.0-capable tuners (0 and 1) free while others exist
	a, ra := e.open("/auto/v7.1")
	b, rb := e.open("/auto/v14.1")
	if a == nil || b == nil {
		t.Fatal("streams refused")
	}
	for _, r := range []*http.Response{ra, rb} {
		if res := r.Header.Get("X-HDHomeRun-Resource"); res != "tuner2" && res != "tuner3" {
			t.Fatalf("resource header %q, want tuner2 or tuner3", res)
		}
	}
	defer a.close()
	defer b.close()
	a.read(t, 188*20)
	b.read(t, 188*20)
	if got := busy(); got[0] || got[1] || !got[2] || !got[3] {
		t.Fatalf("ATSC 1.0 streams on tuners %v, want 2 and 3 only", got)
	}
	// with 2 and 3 taken the capable tuners are the last resort
	c, _ := e.open("/auto/v36.1")
	if c == nil {
		t.Fatal("third ATSC 1.0 stream refused although tuners 0/1 are free")
	}
	defer c.close()
	c.read(t, 188*20)
	if got := busy(); !got[0] && !got[1] {
		t.Fatalf("third stream not on a 3.0 tuner: %v", got)
	}
	// ...and an ATSC 3.0 service now has to wait for / be refused a tuner
	e.lu.Replace(append(append(append([]lineup.Entry{}, testLineup...), extraLineup...), atsc3Lineup...))
	d, _ := e.open("/auto/v104.1")
	if d == nil {
		t.Fatal("ATSC 3.0 service should take the remaining capable tuner")
	}
	d.close()
}

// Virtual tuner N is physical tuner N of the stable global numbering
// (device 0 = tuners 0..1, device 1 = tuners 2..3) when it is free to be.
func TestFixedTunerMapping(t *testing.T) {
	e := setup(t, []int{2, 2}, 4, nil)
	c3, _ := e.open("/tuner3/v7.1")
	c0, _ := e.open("/tuner0/v14.1")
	if c3 == nil || c0 == nil {
		t.Fatal("streams refused")
	}
	defer c3.close()
	defer c0.close()
	c3.read(t, 188*20)
	c0.read(t, 188*20)
	if got := e.fakes[1].TunedChannels(); got[1] == "none" || got[0] != "none" {
		t.Fatalf("tuner3 should be device 1 tuner 1, device 1 = %v", got)
	}
	if got := e.fakes[0].TunedChannels(); got[0] == "none" || got[1] != "none" {
		t.Fatalf("tuner0 should be device 0 tuner 0, device 0 = %v", got)
	}
	var st []map[string]any
	getJSON(t, e.api+"/status.json", &st)
	if len(st) != 4 || st[3]["Resource"] != "tuner3" || st[3]["VctNumber"] != "7.1" || st[0]["VctNumber"] != "14.1" {
		t.Fatalf("status.json %v", st)
	}
}

// A lingering tuner is kept for quick flips back, but is given up the moment
// someone needs it.
func TestLingerAndEviction(t *testing.T) {
	testLinger = 30 * time.Second
	defer func() { testLinger = 0 }()
	e := setup(t, []int{1}, 2, nil)

	a, _ := e.open("/auto/v7.1")
	a.read(t, 188*20)
	a.close()
	waitFor(t, "viewer gone", time.Second, func() bool { return len(e.hub.Streams()) == 1 && e.hub.Streams()[0].Subs == 0 })
	// flip back: reuses the tuned tuner, no new tune
	a2, _ := e.open("/auto/v7.2")
	a2.read(t, 188*20)
	if n := e.fakes[0].TuneCount(); n != 1 {
		t.Fatalf("flip back re-tuned (%d tunes)", n)
	}
	a2.close()
	waitFor(t, "viewer gone", time.Second, func() bool { return e.hub.Streams()[0].Subs == 0 })

	// the only physical tuner is held by the lingering 177 MHz stream; a
	// different channel must get it immediately rather than after the linger
	start := time.Now()
	b, _ := e.open("/auto/v14.1")
	if b == nil {
		t.Fatal("channel flip refused although the only tuner was merely lingering")
	}
	defer b.close()
	b.read(t, 188*20)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("flip took %v", d)
	}
}

// Slow consumers get a bounded queue; beyond it data is dropped and counted,
// and the shared stream is never held up.
func TestSlowClientBuffer(t *testing.T) {
	testBuffer = 64 << 10
	defer func() { testBuffer = 0 }()
	e := setup(t, []int{2}, 2, nil)
	spec := chanmap.Spec{Modulation: "auto", Frequency: 177000000}

	slow, err := e.hub.Subscribe(context.Background(), spec, hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	slow.Enable()
	fast, err := e.hub.Subscribe(context.Background(), spec, hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	fast.Enable()

	got := 0
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case b := <-fast.C:
			got += len(b)
		case <-deadline:
			break loop
		}
	}
	if got < 500000 {
		t.Fatalf("fast client starved by a slow one: %d bytes", got)
	}
	if slow.Drops() == 0 || slow.Buffered() == 0 || slow.Buffered() > cap(slow.C) {
		t.Fatalf("slow client: drops=%d buffered=%d cap=%d", slow.Drops(), slow.Buffered(), cap(slow.C))
	}
	if fast.Drops() != 0 {
		t.Fatalf("fast client dropped %d", fast.Drops())
	}
}

// Real devices answer with an empty body and "X-HDHomeRun-Error: NNN Text".
func TestErrorHeadersLikeRealDevice(t *testing.T) {
	e := setup(t, []int{1}, 2, nil)
	a, _ := e.open("/tuner0/v7.1")
	defer a.close()
	for path, want := range map[string]string{
		"/tuner0/v14.1": "804 Tuner In Use",
		"/auto/v14.1":   "805 All Tuners In Use", // tuner1 is free but the only physical tuner is busy
		"/auto/v99.9":   "801 Unknown Channel",
	} {
		_, resp := e.open(path)
		if got := resp.Header.Get("X-HDHomeRun-Error"); got != want {
			t.Errorf("%s: X-HDHomeRun-Error %q, want %q", path, got, want)
		}
		if resp.Header.Get("Server") != "HDHomeRun/1.0" {
			t.Errorf("%s: Server %q", path, resp.Header.Get("Server"))
		}
	}
	a.read(t, 188*10)
}

// A request for a specific tuner that is busy is first checked against what
// the tuner carries: the same RF channel (or ATSC 3.0 service) can be served
// by joining it, only a different channel gets "804 Tuner In Use".
func TestSpecificBusyTunerJoins(t *testing.T) {
	e := setup(t, []int{2}, 2, func(c *vdev.Config) {})
	e.lu.Replace(append(append(append([]lineup.Entry{}, testLineup...), extraLineup...), atsc3Lineup...))

	a, _ := e.open("/tuner0/v7.1")
	if a == nil {
		t.Fatal("first viewer refused")
	}
	defer a.close()
	a.read(t, 188*20)

	b, resp := e.open("/tuner0/v7.2") // same RF channel, other program
	if b == nil {
		t.Fatalf("same-channel request on busy tuner0 refused: %d %v", resp.StatusCode, resp.Header)
	}
	defer b.close()
	an := analyze(t, b.read(t, 188*100))
	if an.ccErrors != 0 || len(an.esProgs) != 1 || !an.esProgs[2] {
		t.Fatalf("joined viewer got %v (cc=%d), want only program 2", an.esProgs, an.ccErrors)
	}
	if e.activePhysical() != 1 {
		t.Fatalf("joining used %d physical tuners", e.activePhysical())
	}

	_, resp = e.open("/tuner0/v14.1") // different RF channel
	if resp.StatusCode != 503 || resp.Header.Get("X-HDHomeRun-Error") != "804 Tuner In Use" {
		t.Fatalf("different channel on busy tuner: %d %v", resp.StatusCode, resp.Header)
	}

	// the first viewer leaving must not take the tuner away from the second
	a.close()
	time.Sleep(300 * time.Millisecond)
	b.read(t, 188*50)
	_, resp = e.open("/tuner0/v14.1")
	if resp.StatusCode != 503 {
		t.Fatalf("tuner0 was freed while a viewer remains: %d", resp.StatusCode)
	}
	b.close()
	waitFor(t, "tuner released with the last viewer", 3*time.Second, func() bool { return e.activePhysical() == 0 })
	c, resp := e.open("/tuner0/v14.1")
	if c == nil {
		t.Fatalf("tuner0 still busy after everyone left: %d", resp.StatusCode)
	}
	c.close()

	// ATSC 3.0: only the identical service joins
	d1, _ := e.open("/tuner1/v104.1")
	if d1 == nil {
		t.Fatal("ATSC 3.0 viewer refused")
	}
	defer d1.close()
	d2, resp := e.open("/tuner1/v104.1")
	if d2 == nil {
		t.Fatalf("same ATSC 3.0 service refused: %d", resp.StatusCode)
	}
	defer d2.close()
	_, resp = e.open("/tuner1/v107.1")
	if resp.StatusCode != 503 || resp.Header.Get("X-HDHomeRun-Error") != "804 Tuner In Use" {
		t.Fatalf("other ATSC 3.0 service on a busy tuner: %d %v", resp.StatusCode, resp.Header)
	}
}

// Virtual tuner N is physical tuner N, strictly: asking for two specific
// tuners means two physical tuners, while /auto/ reuses what already carries
// the channel.
func TestStrictMappingAndReuse(t *testing.T) {
	e := setup(t, []int{4}, 4, nil)
	a, ra := e.open("/tuner0/v7.1")
	b, rb := e.open("/tuner1/v7.2") // same RF channel, but tuner1 is its own tuner
	if a == nil || b == nil {
		t.Fatal("streams refused")
	}
	if ra.Header.Get("X-HDHomeRun-Resource") != "tuner0" || rb.Header.Get("X-HDHomeRun-Resource") != "tuner1" {
		t.Fatalf("resource headers %q %q", ra.Header.Get("X-HDHomeRun-Resource"), rb.Header.Get("X-HDHomeRun-Resource"))
	}
	defer a.close()
	defer b.close()
	a.read(t, 188*20)
	b.read(t, 188*20)
	if got := e.activePhysical(); got != 2 {
		t.Fatalf("two explicit tuners used %d physical tuners, want 2", got)
	}
	if got := e.fakes[0].TunedChannels(); got[0] == "none" || got[1] == "none" || got[2] != "none" || got[3] != "none" {
		t.Fatalf("physical tuners in use %v, want exactly 0 and 1", got)
	}
	// /auto/ for the same channel joins an existing tuner instead of a third
	c, rc := e.open("/auto/v7.1")
	if c == nil {
		t.Fatal("auto refused")
	}
	defer c.close()
	if r := rc.Header.Get("X-HDHomeRun-Resource"); r != "tuner0" && r != "tuner1" {
		t.Fatalf("a joined viewer must be told the tuner it shares, got %q", r)
	}
	c.read(t, 188*20)
	if got := e.activePhysical(); got != 2 {
		t.Fatalf("/auto/ opened another tuner (%d active) instead of reusing", got)
	}
	var st []map[string]any
	getJSON(t, e.api+"/status.json", &st)
	if st[2]["VctNumber"] != nil || st[3]["VctNumber"] != nil {
		t.Fatalf("/auto/ occupied a free virtual tuner: %v", st)
	}
}

// A physical tuner used by another application is left alone: /auto/ goes
// elsewhere and a request for that very tuner is refused with 804.
func TestForeignUseOfPhysicalTuner(t *testing.T) {
	e := setup(t, []int{4}, 4, nil)
	foreign, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/tuner2/ch473000000", e.fakes[0].StreamPort()))
	if err != nil || foreign.StatusCode != 200 {
		t.Fatalf("foreign client: %v %v", foreign, err)
	}
	defer foreign.Body.Close()

	a, _ := e.open("/auto/v7.1")
	if a == nil {
		t.Fatal("auto refused")
	}
	defer a.close()
	a.read(t, 188*20)
	got := e.fakes[0].TunedChannels()
	if got[3] == "none" {
		t.Fatalf("/auto/ should have taken the free ATSC 1.0 tuner 3, tuners: %v", got)
	}
	if got[0] != "none" || got[1] != "none" {
		t.Fatalf("/auto/ used a 3.0-capable tuner: %v", got)
	}
	_, resp := e.open("/tuner2/v7.1")
	if resp.StatusCode != 503 || resp.Header.Get("X-HDHomeRun-Error") != "804 Tuner In Use" {
		t.Fatalf("tuner used by someone else: %d %v", resp.StatusCode, resp.Header)
	}
	// and its status shows what the other application is doing
	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	if v, _ := ctl.Get("/tuner2/status"); !strings.Contains(v, "473000000") {
		t.Fatalf("status of a foreign-used tuner: %q", v)
	}
}

// Channels DVR polls /tunerN/debug for its signal display and expects the
// four-line form real devices return.
func TestDebugVariableFormat(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	idle, err := ctl.Get("/tuner0/debug")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"tun: ch=none", "dev: ", "ts:  ", "net: "} {
		if !strings.Contains(idle, p) {
			t.Fatalf("idle debug lacks %q:\n%s", p, idle)
		}
	}
	c, _ := e.open("/tuner0/v7.1")
	defer c.close()
	c.read(t, 188*400)
	time.Sleep(700 * time.Millisecond)
	c.read(t, 188*400)
	dbg, _ := ctl.Get("/tuner0/debug")
	lines := strings.Split(strings.TrimSpace(dbg), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "tun: ch=") || !strings.Contains(lines[0], " lock=8vsb:177000000 ss=") ||
		!strings.HasPrefix(lines[1], "dev: bps=") || !strings.HasPrefix(lines[2], "ts:  bps=") || !strings.HasPrefix(lines[3], "net: bps=") {
		t.Fatalf("debug while streaming:\n%s", dbg)
	}
	if strings.Contains(lines[1], "bps=0 ") || strings.Contains(lines[3], "bps=0 ") {
		t.Fatalf("rates missing:\n%s", dbg)
	}
	if _, err := ctl.Get("/tuner/debug"); err == nil {
		t.Fatal("/tuner/debug does not exist on real devices either")
	}
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

// /lineup/scan works like a real device's: get shows state/progress/found,
// set start|abort controls it. A scan takes every free tuner (never evicting a
// viewer), the tuners it holds refuse streams at once with 804, and /auto/ goes
// around them.
func TestLineupScanVariable(t *testing.T) {
	e := setup(t, []int{3}, 3, func(c *vdev.Config) { c.ScanFrom, c.ScanTo = 2, 36 })
	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	e.lu.Replace(nil)

	if v, _ := ctl.Get("/lineup/scan"); v != "state=idle progress=0% found=0" {
		t.Fatalf("before any scan: %q", v)
	}
	if _, err := ctl.Set("/lineup/scan", "bogus", 0); err == nil || !strings.Contains(err.Error(), "unknown scan command") {
		t.Fatalf("bogus command: %v", err)
	}
	e.lu.Replace(testLineup)
	if _, err := ctl.Set("/lineup/scan", "start", 0); err != nil {
		t.Fatal(err)
	}
	if v, _ := ctl.Get("/lineup/scan"); !strings.HasPrefix(v, "state=running progress=") {
		t.Fatalf("while scanning: %q", v)
	}
	waitFor(t, "scan tuners locked", 3*time.Second, func() bool { return lockedTuners(t, e.fakes[0], 3) == 3 })
	if _, err := ctl.Set("/lineup/scan", "start", 0); err == nil {
		t.Fatal("second scan should be refused while one is running")
	}
	// a tuner held by the scan is refused immediately, not after a wait
	began := time.Now()
	_, resp := e.open("/tuner0/v7.1")
	if resp.StatusCode != 503 || resp.Header.Get("X-HDHomeRun-Error") != "804 Tuner In Use" || time.Since(began) > time.Second {
		t.Fatalf("stream on a scan tuner: %d %v after %v", resp.StatusCode, resp.Header, time.Since(began))
	}
	waitFor(t, "scan to finish", 40*time.Second, func() bool { v, _ := ctl.Get("/lineup/scan"); return strings.HasPrefix(v, "state=complete") })
	if v, _ := ctl.Get("/lineup/scan"); v != "state=complete progress=100% found=7" {
		t.Fatalf("after the scan: %q", v)
	}
	if n := lockedTuners(t, e.fakes[0], 3); n != 0 {
		t.Fatalf("%d tuners still locked after the scan", n)
	}
	c, _ := e.open("/tuner0/v7.1")
	if c == nil {
		t.Fatal("tuner 0 should be usable again")
	}
	c.close()

	// abort
	ctl.Set("/lineup/scan", "start", 0)
	time.Sleep(300 * time.Millisecond)
	if _, err := ctl.Set("/lineup/scan", "abort", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "abort", 5*time.Second, func() bool { v, _ := ctl.Get("/lineup/scan"); return strings.HasPrefix(v, "state=aborted") })
	waitFor(t, "tuners released", 3*time.Second, func() bool { return lockedTuners(t, e.fakes[0], 3) == 0 })
}

// With the number of scan tuners limited, the others stay available.
func TestScanTunerLimit(t *testing.T) {
	e := setup(t, []int{3}, 3, func(c *vdev.Config) { c.ScanFrom, c.ScanTo, c.ScanTuners = 2, 36, 1 })
	e.lu.Replace(testLineup)
	if err := e.dev.StartScan(""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "scan tuner locked", 3*time.Second, func() bool { return lockedTuners(t, e.fakes[0], 3) >= 1 })
	if n := lockedTuners(t, e.fakes[0], 3); n != 1 {
		t.Fatalf("scan holds %d tuners, want 1", n)
	}
	c, _ := e.open("/auto/v7.1") // goes to a tuner the scan does not hold
	if c == nil {
		t.Fatal("/auto/ refused although two tuners are free")
	}
	c.read(t, 188*50)
	c.close()
	e.dev.AbortScan()
}

// The channel list comes from the devices' own lineups; where a channel is on
// the air is not known until it is learned.
func TestLineupFromDevices(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	e.lu.Replace(nil)
	if err := e.dev.RefreshLineup(context.Background()); err != nil {
		t.Fatal(err)
	}
	var lj []map[string]any
	getJSON(t, e.api+"/lineup.json", &lj)
	if len(lj) != 8 {
		t.Fatalf("%d channels from the devices, want 8: %v", len(lj), lj)
	}
	if lj[0]["GuideNumber"] != "7.1" || lj[0]["GuideName"] != "ALPHA" || lj[0]["VideoCodec"] != "MPEG2" || lj[0]["HD"].(float64) != 1 {
		t.Fatalf("first entry %v", lj[0])
	}
	if en, _ := e.lu.Find("7.1"); en.Frequency != 0 {
		t.Fatalf("nothing should be known about the RF channel yet: %+v", en)
	}
	e.lu.LearnRF([]lineup.Entry{{GuideNumber: "7.2", Frequency: 177000000, Program: 2, Modulation: "8vsb"}}, false)

	// a channel the devices stop listing disappears; what was learned stays
	e.fakes[0].HideFromLineup("14.3")
	if err := e.dev.RefreshLineup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.lu.Find("14.3"); ok {
		t.Fatal("14.3 should be gone")
	}
	if en, _ := e.lu.Find("7.2"); en.Frequency != 177000000 || en.Program != 2 {
		t.Fatalf("learned RF lost on refresh: %+v", en)
	}
}

// A channel of unknown location is found by tuning it by name once; the whole
// multiplex is learned from that, so its neighbours need no further probe.
func TestLazyLearning(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	e.lu.Replace(nil)
	if err := e.dev.RefreshLineup(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, resp := e.open("/auto/v14.3")
	if c == nil {
		t.Fatalf("unlearned channel refused: %d %v", resp.StatusCode, resp.Header)
	}
	defer c.close()
	an := analyze(t, c.read(t, 188*200))
	if an.ccErrors != 0 || len(an.esProgs) != 1 || !an.esProgs[3] {
		t.Fatalf("content: %v cc=%d", an.esProgs, an.ccErrors)
	}
	for g, prog := range map[string]uint16{"14.1": 1, "14.3": 3} {
		if en, _ := e.lu.Find(g); en.Frequency != 473000000 || en.Program != prog {
			t.Errorf("%s not learned from the probe: %+v", g, en)
		}
	}
	if en, _ := e.lu.Find("7.1"); en.Frequency != 0 {
		t.Errorf("7.1 is on another multiplex and must still be unknown: %+v", en)
	}
	tunes := e.fakes[0].TuneCount()
	// its neighbour needs no probe, and joins the tuner that already has it
	c2, _ := e.open("/auto/v14.1")
	if c2 == nil {
		t.Fatal("neighbour refused")
	}
	defer c2.close()
	c2.read(t, 188*50)
	if n := e.fakes[0].TuneCount(); n != tunes {
		t.Fatalf("the neighbour caused %d extra tunes", n-tunes)
	}
	if e.activePhysical() != 1 {
		t.Fatalf("%d tuners active, want 1", e.activePhysical())
	}
	// a channel the devices do not know cannot be located
	e.lu.LearnRF([]lineup.Entry{{GuideNumber: "55.5", GuideName: "GHOST"}}, true)
	_, resp = e.open("/auto/v55.5")
	if resp.StatusCode != 404 || resp.Header.Get("X-HDHomeRun-Error") != "801 Unknown Channel" {
		t.Fatalf("unknown to every device: %d %v", resp.StatusCode, resp.Header)
	}
}

// Watching any channel teaches the lineup where the rest of its multiplex is.
func TestLearnsFromRunningStream(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	e.lu.Replace(nil)
	if err := e.dev.RefreshLineup(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, resp := e.open("/auto/ch177000000-1") // by frequency: no lineup lookup at all
	if c == nil {
		t.Fatalf("refused: %d", resp.StatusCode)
	}
	defer c.close()
	waitFor(t, "7.2 learned from the running stream", 5*time.Second, func() bool {
		en, _ := e.lu.Find("7.2")
		return en.Frequency == 177000000 && en.Program == 2
	})
}

// The tuners page lists, for every tuner, the channels being watched, how many
// clients there are and where they are.
func TestTunersPageListsClients(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	a, _ := e.open("/auto/v7.1")
	b, _ := e.open("/auto/v7.2") // joins the same tuner
	c, _ := e.open("/auto/v14.1")
	for _, x := range []*client{a, b, c} {
		if x == nil {
			t.Fatal("stream refused")
		}
		defer x.close()
		x.read(t, 188*50)
	}
	resp, err := http.Get(e.api + "/tuners.html")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	for _, want := range []string{"3 client(s) on 2 of 2 tuner(s)", "7.1 ALPHA", "7.2 BETA", "14.1 GAMMA", "127.0.0.1"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Count(page, "127.0.0.1 &mdash;") != 3 {
		t.Errorf("want three client lines:\n%s", page)
	}
	var dj map[string]any
	getJSON(t, e.api+"/discover.json", &dj)
	if m := fmt.Sprint(dj["Manufacturer"]); strings.Contains(strings.ToLower(m), "silicon") {
		t.Errorf("Manufacturer %q", m)
	}
	xml, _ := http.Get(e.api + "/device.xml")
	xb, _ := io.ReadAll(xml.Body)
	if strings.Contains(strings.ToLower(string(xb)), "silicon") {
		t.Errorf("device.xml mentions the vendor:\n%s", xb)
	}
}

// With a tuner list the multiplexer is made of exactly those tuners: they are
// the only ones used, they are numbered from 0, and nothing else is touched.
func TestTunerListDefinesTheVirtualDevice(t *testing.T) {
	testTuners = func(ids []uint32) []string { return []string{hdhrproto.FormatDeviceID(ids[0]) + "/2-3"} }
	defer func() { testTuners = nil }()
	e := setup(t, []int{4}, 2, nil)

	var dj map[string]any
	getJSON(t, e.api+"/discover.json", &dj)
	if dj["TunerCount"].(float64) != 2 {
		t.Fatalf("TunerCount %v, want 2", dj["TunerCount"])
	}
	a, ra := e.open("/auto/v7.1")
	b, _ := e.open("/auto/v14.1")
	if a == nil || b == nil {
		t.Fatal("streams refused")
	}
	defer a.close()
	defer b.close()
	a.read(t, 188*20)
	b.read(t, 188*20)
	got := e.fakes[0].TunedChannels()
	if got[0] != "none" || got[1] != "none" || got[2] == "none" || got[3] == "none" {
		t.Fatalf("physical tuners in use %v, want only 2 and 3", got)
	}
	if r := ra.Header.Get("X-HDHomeRun-Resource"); r != "tuner0" && r != "tuner1" {
		t.Fatalf("resource %q: the listed tuners are numbered from 0", r)
	}
	e.lu.Replace(append(append([]lineup.Entry{}, testLineup...), extraLineup...))
	_, resp := e.open("/auto/v36.1") // a third channel: both listed tuners are taken
	if resp.StatusCode != 503 {
		t.Fatalf("third channel: %d", resp.StatusCode)
	}
	if n := e.fakes[0].Peak(); n > 2 {
		t.Fatalf("peak %d physical tuners in use", n)
	}
}

// Hidden: not discoverable by wildcard, still reachable by device ID or address.
func TestNotDiscoverable(t *testing.T) {
	e := setup(t, []int{1}, 1, func(c *vdev.Config) { c.Hidden = true })
	if infos, _ := hdhrproto.Discover([]string{e.ctl}, 600*time.Millisecond); len(infos) != 0 {
		t.Fatalf("a hidden multiplexer answered wildcard discovery: %+v", infos)
	}
	req := &hdhrproto.Packet{Type: hdhrproto.TypeDiscoverReq}
	req.AddU32(hdhrproto.TagDeviceType, hdhrproto.DeviceTypeTuner)
	req.AddU32(hdhrproto.TagDeviceID, e.dev.ID())
	c, _ := net.Dial("udp", e.ctl)
	defer c.Close()
	c.Write(req.Encode())
	c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1024)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("no answer to a request naming its device ID: %v", err)
	}
	if p, err := hdhrproto.Decode(buf[:n]); err != nil || p.Type != hdhrproto.TypeDiscoverRpy {
		t.Fatalf("bad answer: %v %v", p, err)
	}
	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	if v, err := ctl.Get("/sys/model"); err != nil || v == "" {
		t.Fatalf("direct control connection: %q %v", v, err)
	}
	var dj map[string]any
	getJSON(t, e.api+"/discover.json", &dj)
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

// The multiplexer is a DLNA media server like the real devices, so TVs list it
// as a video source and can browse and play its channels.
func TestDLNAMediaServer(t *testing.T) {
	const cd = "urn:schemas-upnp-org:service:ContentDirectory:1"
	e := setup(t, []int{1}, 1, nil)
	e.lu.Replace(append(append([]lineup.Entry{}, testLineup...), lineup.Entry{GuideNumber: "5.9", GuideName: "SECRET", DRM: true, Frequency: 1}))

	get := func(p string) string {
		resp, err := http.Get(e.api + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", p, resp, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	desc := get("/dms/device.xml")
	for _, want := range []string{"urn:schemas-upnp-org:device:MediaServer:1", "DMS-1.50", "<UDN>uuid:", e.api + "/dms/ContentDirectory</controlURL>", "/dms/ConnectionManager.xml"} {
		if !strings.Contains(desc, want) {
			t.Errorf("device description lacks %q:\n%s", want, desc)
		}
	}
	if strings.Contains(strings.ToLower(desc), "silicon") {
		t.Error("description mentions the vendor")
	}
	for _, p := range []string{"/dms/ContentDirectory.xml", "/dms/ConnectionManager.xml"} {
		if !strings.Contains(get(p), "<scpd") {
			t.Errorf("%s is not a service description", p)
		}
	}
	if !strings.Contains(get("/device.xml"), "MediaServer:1") {
		t.Error("/device.xml should describe the media server too")
	}

	_, root := soapCall(t, e.api, "/dms/ContentDirectory", cd, "Browse",
		"<ObjectID>0</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount><SortCriteria></SortCriteria>")
	root = unesc(root)
	for _, want := range []string{`id="Favorites"`, `id="Channels"`, "<dlna:containerType>Tuner_1_0</dlna:containerType>", "<NumberReturned>2</NumberReturned>"} {
		if !strings.Contains(root, want) {
			t.Errorf("root browse lacks %q:\n%s", want, root)
		}
	}

	_, ch := soapCall(t, e.api, "/dms/ContentDirectory", cd, "Browse",
		"<ObjectID>Channels</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><Filter>*</Filter><StartingIndex>1</StartingIndex><RequestedCount>2</RequestedCount><SortCriteria></SortCriteria>")
	ch = unesc(ch)
	for _, want := range []string{`id="Channels/v7.2"`, `id="Channels/v14.1"`, "<dc:title>7.2 BETA</dc:title>", "object.item.videoItem.videoBroadcast",
		"<upnp:channelNr>7.2</upnp:channelNr>", fmt.Sprintf(">http://127.0.0.1:%d/auto/v7.2</res>", e.dev.StreamPort()),
		"DLNA.ORG_PN=MPEG_TS_SD_NA_ISO", "<NumberReturned>2</NumberReturned>", "<TotalMatches>4</TotalMatches>"} {
		if !strings.Contains(ch, want) {
			t.Errorf("channel browse lacks %q:\n%s", want, ch)
		}
	}
	if strings.Contains(ch, "SECRET") {
		t.Error("an encrypted channel must not be offered")
	}

	// favourites follow the lineup's favourite flags
	if resp, err := http.Post(e.api+"/lineup.post?favorite=%2B14.1", "", nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("favorite: %v %v", resp, err)
	}
	_, fav := soapCall(t, e.api, "/dms/ContentDirectory", cd, "Browse",
		"<ObjectID>Favorites</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount><SortCriteria></SortCriteria>")
	if fav = unesc(fav); !strings.Contains(fav, `id="Channels/v14.1"`) || !strings.Contains(fav, "<TotalMatches>1</TotalMatches>") {
		t.Errorf("favourites:\n%s", fav)
	}

	_, meta := soapCall(t, e.api, "/dms/ContentDirectory", cd, "Browse",
		"<ObjectID>Channels/v7.1</ObjectID><BrowseFlag>BrowseMetadata</BrowseFlag><Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>1</RequestedCount><SortCriteria></SortCriteria>")
	if !strings.Contains(unesc(meta), "<dc:title>7.1 ALPHA</dc:title>") {
		t.Errorf("item metadata:\n%s", meta)
	}
	if code, body := soapCall(t, e.api, "/dms/ContentDirectory", cd, "Browse",
		"<ObjectID>nope</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>1</RequestedCount><SortCriteria></SortCriteria>"); code != 500 || !strings.Contains(body, "<errorCode>701</errorCode>") {
		t.Errorf("unknown object: %d %s", code, body)
	}
	if _, id := soapCall(t, e.api, "/dms/ContentDirectory", cd, "GetSystemUpdateID", ""); !strings.Contains(id, "<Id>") {
		t.Errorf("GetSystemUpdateID: %s", id)
	}
	_, pi := soapCall(t, e.api, "/dms/ConnectionManager", "urn:schemas-upnp-org:service:ConnectionManager:1", "GetProtocolInfo", "")
	if !strings.Contains(pi, "MPEG_TS_SD_NA_ISO") {
		t.Errorf("protocol info: %s", pi)
	}

	req, _ := http.NewRequest("SUBSCRIBE", e.api+"/dms/ContentDirectory", nil)
	req.Header.Set("CALLBACK", "<http://127.0.0.1:9/cb>")
	req.Header.Set("NT", "upnp:event")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("SID"), "uuid:") {
		t.Fatalf("SUBSCRIBE: %v %v", resp, err)
	}
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

// Watching a channel collects the guide of its whole multiplex, and /epg
// serves it as XMLTV (and JSON).
func TestGuideFromRunningStream(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	if x := fetchEPG(t, e, ""); len(x.Programmes) != 0 {
		t.Fatalf("a guide before anything was heard: %+v", x.Programmes)
	}
	c, _ := e.open("/auto/v7.1")
	defer c.close()
	c.read(t, 188*50)
	waitFor(t, "guide collected from the stream", 10*time.Second, func() bool {
		return len(fetchEPG(t, e, "?channel=7.2").Programmes) >= 2
	})

	x := fetchEPG(t, e, "")
	titles := map[string]string{}
	descs := map[string]string{}
	for _, p := range x.Programmes {
		titles[p.Channel+"/"+p.Title] = p.Start
		descs[p.Channel+"/"+p.Title] = p.Desc
		if p.Start >= p.Stop || !strings.HasSuffix(p.Start, " +0000") {
			t.Errorf("bad times %q-%q", p.Start, p.Stop)
		}
	}
	for _, want := range []string{"7.1/Now Show ALPHA", "7.1/Next Show ALPHA", "7.2/Now Show BETA"} {
		if _, ok := titles[want]; !ok {
			t.Errorf("missing %q in %v", want, titles)
		}
	}
	if descs["7.2/Now Show BETA"] != "About Now Show BETA" {
		t.Errorf("description %q", descs["7.2/Now Show BETA"])
	}
	if len(x.Channels) < 2 || x.Channels[0].ID != "7.1" || len(x.Channels[0].Names) != 3 || x.Channels[0].Names[0] != "7.1 ALPHA" {
		t.Errorf("channels %+v", x.Channels)
	}
	for _, p := range fetchEPG(t, e, "?channel=7.2").Programmes {
		if p.Channel != "7.2" {
			t.Errorf("channel filter leaked %q", p.Channel)
		}
	}
	for _, p := range x.Programmes {
		if p.Channel == "14.1" {
			t.Error("a multiplex nobody listened to has no guide yet")
		}
	}
	var js []map[string]any
	getJSON(t, e.api+"/epg.json?channel=7.1", &js)
	if len(js) != 1 || js[0]["GuideNumber"] != "7.1" || len(js[0]["Events"].([]any)) < 2 {
		t.Fatalf("epg.json: %v", js)
	}
}

// A pass listens to the multiplexes nobody is watching, using spare tuners,
// and a viewer who needs the tuner gets it at once.
func TestGuidePassAndYielding(t *testing.T) {
	e := setup(t, []int{1}, 2, func(c *vdev.Config) { c.GuideHold = 4 * time.Second })
	e.lu.Replace(append(append([]lineup.Entry{}, testLineup...), atsc3Lineup...))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if n, total, err := e.dev.GuidePass(context.Background()); err != nil || n != 2 || total != 2 {
			t.Errorf("pass: %d/%d %v (the ATSC 3.0 channel has no PSIP and is not visited)", n, total, err)
		}
	}()
	time.Sleep(700 * time.Millisecond)
	// the single tuner is held by the pass; a viewer must not have to wait for it
	began := time.Now()
	c, resp := e.open("/auto/v14.1")
	if c == nil {
		t.Fatalf("viewer refused during a guide pass: %d %v", resp.StatusCode, resp.Header)
	}
	c.read(t, 188*50)
	if d := time.Since(began); d > 1500*time.Millisecond {
		t.Fatalf("viewer waited %v for the guide pass", d)
	}
	c.close()
	<-done

	got := fetchEPG(t, e, "")
	seen := map[string]bool{}
	for _, p := range got.Programmes {
		seen[p.Channel] = true
	}
	for _, ch := range []string{"7.1", "7.2", "14.1", "14.3"} {
		if !seen[ch] {
			t.Errorf("no guide for %s after the pass (have %v)", ch, seen)
		}
	}
	if seen["104.1"] {
		t.Error("ATSC 3.0 services carry no PSIP guide")
	}
	if resp, err := http.Post(e.api+"/epg/refresh", "", nil); err != nil || resp.StatusCode != 202 {
		t.Fatalf("POST /epg/refresh: %v %v", resp, err)
	}
}

// An unlocked guide pass holds no lock: another application can take the
// tuner at any moment, the pass lets go without touching what the new user set
// up, and the new user's stream carries on.
func TestUnlockedGuidePassYieldsToOutsiders(t *testing.T) {
	e := setup(t, []int{1}, 1, func(c *vdev.Config) { c.GuideHold = 8 * time.Second; c.GuideUnlocked = true })
	e.lu.Replace(append([]lineup.Entry(nil), testLineup[2:]...)) // 14.1, 14.3 on 473 MHz only
	raw := hdhrproto.NewControl(e.fakes[0].Addr())
	defer raw.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.dev.GuidePass(context.Background())
	}()
	waitFor(t, "pass listening without a lock", 5*time.Second, func() bool {
		tg, _ := raw.Get("/tuner0/target")
		lk, _ := raw.Get("/tuner0/lockkey")
		return strings.HasPrefix(tg, "http://") && lk == "none"
	})
	if lk, _ := raw.Get("/tuner0/lockkey"); lk != "none" {
		t.Fatalf("the guide pass holds a lock (%q)", lk)
	}

	// an outsider takes the tuner
	began := time.Now()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/tuner0/ch177000000", e.fakes[0].StreamPort()))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("an outsider could not take an unlocked tuner: %v %v", resp, err)
	}
	defer resp.Body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass did not give up the tuner")
	}
	if time.Since(began) > 4*time.Second {
		t.Fatalf("the pass took %v to notice", time.Since(began))
	}
	// the outsider's stream goes on, and nothing of theirs was cleared
	buf := make([]byte, 188*100)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("the outsider's stream was disturbed: %v", err)
	}
	if ch, _ := raw.Get("/tuner0/channel"); !strings.Contains(ch, "177000000") {
		t.Fatalf("the pass cleared the new user's channel: %q", ch)
	}
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

// The device is asked for only the PIDs somebody watches (plus tables and
// guide): extra viewers widen the filter at once, leaving viewers narrow it
// again after a grace period, and letting go restores the tuner.
func TestMaskNarrowsTheStream(t *testing.T) {
	// restored by a cleanup registered before the hub's, so it runs after the streams stop
	m, l, g := testMask, testLinger, hub.MaskGrace
	t.Cleanup(func() { testMask, testLinger, hub.MaskGrace = m, l, g })
	testMask, testLinger, hub.MaskGrace = true, 3500*time.Millisecond, 200*time.Millisecond
	e := setup(t, []int{2}, 2, nil)

	has := func(s ts.PIDSet, pids ...uint16) bool {
		for _, p := range pids {
			if !s.Has(p) {
				return false
			}
		}
		return true
	}
	hasNone := func(s ts.PIDSet, pids ...uint16) bool {
		for _, p := range pids {
			if s.Has(p) {
				return false
			}
		}
		return true
	}

	c1, _ := e.open("/auto/v7.1") // program 1: PMT 0x100, video 0x101, audio 0x102
	if c1 == nil {
		t.Fatal("stream refused")
	}
	defer c1.close()
	waitFor(t, "filter narrowed to program 1", 5*time.Second, func() bool {
		f, locked := physFilter(e)
		return locked && has(f, 0, 0x100, 0x101, 0x102, 0x110, 0x1ffb) && hasNone(f, 0x111, 0x112)
	})
	f, _ := physFilter(e)
	t.Logf("filter with one viewer: %s", pidsOf(f))
	if a := analyze(t, c1.read(t, 188*400)); !a.esProgs[1] || len(a.esProgs) != 1 || a.patProgs[1] != 0x100 {
		t.Fatalf("viewer 1 content: programs %v pat %v", a.esProgs, a.patProgs)
	}

	// A second program of the same multiplex widens the filter.
	c2, _ := e.open("/auto/v7.2")
	if c2 == nil {
		t.Fatal("second viewer refused")
	}
	defer c2.close()
	waitFor(t, "filter widened to program 2", 5*time.Second, func() bool {
		f, _ := physFilter(e)
		return has(f, 0x101, 0x102, 0x111, 0x112)
	})
	if a := analyze(t, c2.read(t, 188*400)); !a.esProgs[2] || len(a.esProgs) != 1 {
		t.Fatalf("viewer 2 content: programs %v", a.esProgs)
	}
	if got := e.activePhysical(); got != 1 {
		t.Fatalf("%d physical tuners busy, want 1", got)
	}

	// Program 1's viewer leaves: after the grace period its PIDs stop flowing
	// while program 2 carries on.
	c1.close()
	waitFor(t, "program 1 dropped from the filter", 5*time.Second, func() bool {
		f, _ := physFilter(e)
		return hasNone(f, 0x101, 0x102) && has(f, 0x111, 0x112, 0x100, 0x110, 0)
	})
	if a := analyze(t, c2.read(t, 188*200)); !a.esProgs[2] || len(a.esProgs) != 1 {
		t.Fatalf("viewer 2 disturbed: %v", a.esProgs)
	}

	// Nobody watching (the stream lingers): tables and guide only.
	c2.close()
	waitFor(t, "tables only", 5*time.Second, func() bool {
		f, _ := physFilter(e)
		return hasNone(f, 0x101, 0x102, 0x111, 0x112) && has(f, 0, 0x100, 0x110, 0x1ffb)
	})

	// Linger over: the tuner is given back unfiltered and unlocked.
	waitFor(t, "released", 5*time.Second, func() bool { return e.activePhysical() == 0 })
	waitFor(t, "unlocked and unfiltered", 3*time.Second, func() bool {
		for i := 0; i < 2; i++ {
			f, _ := e.fakes[0].TunerFilter(i)
			if e.fakes[0].TunerLocked(i) || f != ts.FullSet() {
				return false
			}
		}
		return true
	})
}

// A viewer that wants the whole multiplex (no program) keeps the filter open.
func TestMaskStaysOpenForWholeMultiplex(t *testing.T) {
	m := testMask
	t.Cleanup(func() { testMask = m })
	testMask = true
	e := setup(t, []int{2}, 2, nil)
	c, _ := e.open("/auto/ch177000000")
	if c == nil {
		t.Fatal("stream refused")
	}
	defer c.close()
	time.Sleep(2500 * time.Millisecond) // several masker ticks
	f, _ := physFilter(e)
	if f != ts.FullSet() {
		t.Fatalf("filter narrowed for a whole-multiplex viewer: %s", pidsOf(f))
	}
	if a := analyze(t, c.read(t, 188*400)); a.pids[0x101] == 0 || a.pids[0x111] == 0 {
		t.Fatalf("whole multiplex viewer saw %v, want both programs", a.pids)
	}
}

// Masking can be switched off: the device then keeps its own lock.
func TestMaskOff(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	c, _ := e.open("/auto/v7.1")
	if c == nil {
		t.Fatal("stream refused")
	}
	defer c.close()
	time.Sleep(2500 * time.Millisecond)
	f, keyed := physFilter(e)
	if f != ts.FullSet() || keyed {
		t.Fatalf("masking happened although off: filter %s keyed %v", pidsOf(f), keyed)
	}
}

// With masking on, the guide of the programs nobody watches is still
// collected, and after a failover the new session is narrowed again.
func TestMaskKeepsGuideAndSurvivesFailover(t *testing.T) {
	m, l, g, sl := testMask, testLinger, hub.MaskGrace, hub.StallLimit
	t.Cleanup(func() { testMask, testLinger, hub.MaskGrace, hub.StallLimit = m, l, g, sl })
	testMask, hub.MaskGrace, hub.StallLimit = true, 200*time.Millisecond, 1500*time.Millisecond
	e := setup(t, []int{2}, 1, nil)
	c, _ := e.open("/auto/v7.1")
	if c == nil {
		t.Fatal("stream refused")
	}
	defer c.close()
	c.read(t, 188*50)
	waitFor(t, "narrowed", 5*time.Second, func() bool {
		f, _ := physFilter(e)
		return !f.Has(0x112) && f.Has(0x102)
	})
	waitFor(t, "guide of the unwatched program", 10*time.Second, func() bool {
		return len(fetchEPG(t, e, "?channel=7.2").Programmes) >= 2
	})

	e.fakes[0].KillStreams()
	time.Sleep(200 * time.Millisecond)
loop:
	for {
		select {
		case <-c.data:
		default:
			break loop
		}
	}
	if a := analyze(t, c.read(t, 188*100)); !a.esProgs[1] {
		t.Fatalf("no data after failover: %v", a.pids)
	}
	waitFor(t, "narrowed again after failover", 5*time.Second, func() bool {
		f, keyed := physFilter(e)
		return keyed && !f.Has(0x112) && f.Has(0x102)
	})
	c.close()
	waitFor(t, "released", 5*time.Second, func() bool { return e.activePhysical() == 0 })
}

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

// The lineup as an M3U playlist for players that cannot use lineup.json.
func TestPlaylistM3U(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	e.lu.Replace([]lineup.Entry{
		{GuideNumber: "7.1", GuideName: "ALPHA", Frequency: 177000000, Program: 1, HD: true, Favorite: true},
		{GuideNumber: "7.2", GuideName: `BE"TA, the second`, Frequency: 177000000, Program: 2},
		{GuideNumber: "104.1", GuideName: "NEXT-A", Frequency: 605000000, Program: 1376, Modulation: "atsc3", HD: true},
		{GuideNumber: "9.1", GuideName: "SCRAMBLED", Frequency: 473000000, Program: 1, DRM: true},
	})
	get := func(path string) (string, string) {
		r, err := http.Get(e.api + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d", path, r.StatusCode)
		}
		return string(b), r.Header.Get("Content-Type")
	}

	body, ctype := get("/playlist.m3u8")
	if !strings.HasPrefix(ctype, "audio/x-mpegurl") {
		t.Errorf("content type %q", ctype)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if !strings.HasPrefix(lines[0], "#EXTM3U ") || !strings.Contains(lines[0], `url-tvg="`+e.api+`/epg"`) {
		t.Errorf("header %q", lines[0])
	}
	if len(lines) != 1+2*3 { // three channels; the DRM one is left out
		t.Fatalf("%d lines:\n%s", len(lines), body)
	}
	if lines[1] != `#EXTINF:-1 tvg-id="7.1" tvg-chno="7.1" tvg-name="ALPHA" group-title="Antenna",ALPHA` ||
		lines[2] != e.http+"/auto/v7.1" {
		t.Errorf("first entry:\n%s\n%s", lines[1], lines[2])
	}
	if !strings.Contains(lines[3], `tvg-name="BE'TA, the second"`) || !strings.HasSuffix(lines[3], `,BE"TA, the second`) {
		t.Errorf("quoting: %s", lines[3])
	}
	if strings.Contains(body, "SCRAMBLED") {
		t.Error("DRM channel listed")
	}
	for _, p := range []string{"/lineup.m3u", "/lineup.m3u8", "/playlist.m3u"} {
		if b, _ := get(p); b != body {
			t.Errorf("%s differs from /playlist.m3u8", p)
		}
	}

	count := func(path string) (int, string) {
		b, _ := get(path)
		return strings.Count(b, "#EXTINF"), b
	}
	if n, _ := count("/playlist.m3u?hd=1"); n != 2 {
		t.Errorf("hd=1: %d channels, want 2", n)
	}
	if n, b := count("/playlist.m3u?favorites=1"); n != 1 || !strings.Contains(b, "ALPHA") {
		t.Errorf("favorites=1: %d channels", n)
	}
	if n, b := count("/playlist.m3u?atsc3=0"); n != 2 || strings.Contains(b, "NEXT-A") {
		t.Errorf("atsc3=0: %d channels", n)
	}
	if _, b := count("/playlist.m3u?group=rf"); !strings.Contains(b, `group-title="177 MHz"`) {
		t.Errorf("group=rf: %s", b)
	}
	if _, b := count("/playlist.m3u?group=quality"); !strings.Contains(b, `group-title="HD"`) || !strings.Contains(b, `group-title="SD"`) {
		t.Errorf("group=quality: %s", b)
	}
	if _, b := count("/playlist.m3u?group=none"); strings.Contains(b, "group-title") {
		t.Errorf("group=none: %s", b)
	}
}
