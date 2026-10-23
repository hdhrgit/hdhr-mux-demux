package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hdhrmux/internal/chanmap"
	"hdhrmux/internal/fakehdhr"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/vdev"
)

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

// A request for a specific tuner that is busy is first checked against what
// the tuner carries: the same RF channel (or ATSC 3.0 service) can be served
// by joining it, only a different channel gets "804 Tuner In Use".
// A tuner asked for by number belongs to the client that asked, as on a real
// device: busy is busy, even when the other client wants the same channel. Sharing
// is for /auto/ requests, which join each other but not a tuner somebody asked
// for directly.
func TestDirectRequestsAreNotShared(t *testing.T) {
	e := setup(t, []int{3}, 3, nil)
	e.lu.Replace(append(append(append([]lineup.Entry{}, testLineup...), extraLineup...), atsc3Lineup...))

	a, ra := e.open("/tuner0/v7.1")
	if a == nil {
		t.Fatal("first viewer refused")
	}
	defer a.close()
	a.read(t, 188*20)
	if ra.Header.Get("X-HDHomeRun-Resource") != "tuner0" {
		t.Fatalf("resource %q", ra.Header.Get("X-HDHomeRun-Resource"))
	}

	// the same RF channel (other programme, same programme) on the busy tuner: 804
	for _, ch := range []string{"v7.2", "v7.1"} {
		if _, resp := e.open("/tuner0/" + ch); resp.StatusCode != 503 || resp.Header.Get("X-HDHomeRun-Error") != "804 Tuner In Use" {
			t.Fatalf("/tuner0/%s on a busy tuner: %d %v", ch, resp.StatusCode, resp.Header)
		}
	}

	// an /auto/ request for that channel does not ride on tuner 0 either
	c, rc := e.open("/auto/v7.2")
	if c == nil {
		t.Fatal("/auto/ refused")
	}
	defer c.close()
	c.read(t, 188*20)
	if r := rc.Header.Get("X-HDHomeRun-Resource"); r == "tuner0" || r == "" {
		t.Fatalf("/auto/ joined the tuner somebody asked for by number: %q", r)
	}
	if n := e.activePhysical(); n != 2 {
		t.Fatalf("%d physical tuners, want 2 (tuner 0 for its owner, one for /auto/)", n)
	}

	// ... but /auto/ viewers share with each other
	d, rd := e.open("/auto/v7.1")
	if d == nil {
		t.Fatal("second /auto/ viewer refused")
	}
	defer d.close()
	if an := analyze(t, d.read(t, 188*100)); an.ccErrors != 0 || len(an.esProgs) != 1 || !an.esProgs[1] {
		t.Fatalf("shared /auto/ viewer got %v (cc=%d)", an.esProgs, an.ccErrors)
	}
	if rd.Header.Get("X-HDHomeRun-Resource") != rc.Header.Get("X-HDHomeRun-Resource") {
		t.Fatalf("the second /auto/ viewer should share tuner %q, got %q", rc.Header.Get("X-HDHomeRun-Resource"), rd.Header.Get("X-HDHomeRun-Resource"))
	}
	if n := e.activePhysical(); n != 2 {
		t.Fatalf("%d physical tuners after /auto/ sharing, want 2", n)
	}

	// the owner leaving frees its tuner
	a.close()
	waitFor(t, "tuner 0 released with its owner", 3*time.Second, func() bool {
		x, resp := e.open("/tuner0/v14.1")
		if x != nil {
			x.close()
		}
		return resp.StatusCode == 200
	})
	c.close()
	d.close()

	// ATSC 3.0: one tuner per service as before, and a busy one is just busy
	waitFor(t, "everything released", 4*time.Second, func() bool { return e.activePhysical() == 0 })
	d1, _ := e.open("/tuner1/v104.1")
	if d1 == nil {
		t.Fatal("ATSC 3.0 viewer refused")
	}
	defer d1.close()
	if _, resp := e.open("/tuner1/v104.1"); resp.StatusCode != 503 {
		t.Fatalf("the same ATSC 3.0 service by number on a busy tuner: %d", resp.StatusCode)
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
	// /auto/ does not ride on tuners that were asked for by number: it takes a third,
	// and a second /auto/ viewer then shares that one
	c, rc := e.open("/auto/v7.1")
	if c == nil {
		t.Fatal("auto refused")
	}
	defer c.close()
	r := rc.Header.Get("X-HDHomeRun-Resource")
	if r == "tuner0" || r == "tuner1" || r == "" {
		t.Fatalf("/auto/ rode on a tuner somebody asked for by number: %q", r)
	}
	c.read(t, 188*20)
	d, rd := e.open("/auto/v7.2")
	if d == nil {
		t.Fatal("auto refused")
	}
	defer d.close()
	if rd.Header.Get("X-HDHomeRun-Resource") != r {
		t.Fatalf("the second /auto/ viewer should share %q, got %q", r, rd.Header.Get("X-HDHomeRun-Resource"))
	}
	d.read(t, 188*20)
	if got := e.activePhysical(); got != 3 {
		t.Fatalf("%d physical tuners active, want 3 (two by number, one shared by two /auto/ viewers)", got)
	}
	var st []map[string]any
	getJSON(t, e.api+"/status.json", &st)
	if st[3]["VctNumber"] != nil {
		t.Fatalf("the unused virtual tuner shows a channel: %v", st[3])
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

// Channels DVR tells the tuner it was given to let go ("set channel none",
// "set target none") every time one of its streams ends. With /auto/ viewers
// sharing a tuner, that must not cut the viewers that are still watching.
func TestClearingASharedTunerKeepsTheOtherViewers(t *testing.T) {
	e := setup(t, []int{3}, 3, nil)
	a, ra := e.open("/auto/v7.1")
	b, rb := e.open("/auto/v7.2") // the same RF channel: shares the tuner
	if a == nil || b == nil {
		t.Fatal("streams refused")
	}
	defer a.close()
	defer b.close()
	res := ra.Header.Get("X-HDHomeRun-Resource")
	if res == "" || res != rb.Header.Get("X-HDHomeRun-Resource") {
		t.Fatalf("the viewers should share a tuner: %q %q", res, rb.Header.Get("X-HDHomeRun-Resource"))
	}
	a.read(t, 188*50)
	b.read(t, 188*50)

	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	if _, err := ctl.Set("/"+res+"/target", "none", 0); err != nil {
		t.Fatalf("target none: %v", err)
	}
	if _, err := ctl.Set("/"+res+"/channel", "none", 0); err != nil {
		t.Fatalf("channel none: %v", err)
	}
	// both streams carry on
	if an := analyze(t, a.read(t, 188*200)); !an.esProgs[1] || an.ccErrors != 0 {
		t.Fatalf("viewer A after the clear: %v cc=%d", an.esProgs, an.ccErrors)
	}
	if an := analyze(t, b.read(t, 188*200)); !an.esProgs[2] || an.ccErrors != 0 {
		t.Fatalf("viewer B after the clear: %v cc=%d", an.esProgs, an.ccErrors)
	}
	// retuning it to another channel is refused while viewers are on it
	if _, err := ctl.Set("/"+res+"/channel", "auto:473000000", 0); err == nil || !strings.Contains(err.Error(), "resource locked") {
		t.Fatalf("a retune under viewers: %v", err)
	}
	if n := e.activePhysical(); n != 1 {
		t.Fatalf("%d physical tuners", n)
	}

	// with nobody on it, clearing works as before
	a.close()
	b.close()
	waitFor(t, "released", 4*time.Second, func() bool { return e.activePhysical() == 0 })
	if _, err := ctl.Set("/"+res+"/channel", "none", 0); err != nil {
		t.Fatalf("channel none on an idle tuner: %v", err)
	}
}

// tvheadend (control protocol, picks PIDs) holds a tuner; an /auto/ request for a
// channel on it joins that tuner instead of taking another, when auto_join_direct
// is on. The joiner gets its own programme whatever the filter does, and keeps the
// tuner when tvheadend lets go.
func TestAutoJoinsADirectClient(t *testing.T) {
	for _, join := range []bool{true, false} {
		t.Run(fmt.Sprintf("join=%v", join), func(t *testing.T) {
			e := setup(t, []int{3}, 3, func(c *vdev.Config) { c.AutoJoinDirect = join })
			ctl := hdhrproto.NewControl(e.ctl)
			defer ctl.Close()
			rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer rx.Close()
			var udp atomic.Int64
			go func() {
				buf := make([]byte, 2048)
				for {
					n, _, err := rx.ReadFromUDP(buf)
					if err != nil {
						return
					}
					udp.Add(int64(n))
				}
			}()
			set := func(name, val string) {
				t.Helper()
				if _, err := ctl.Set(name, val, 0); err != nil {
					t.Fatalf("set %s %s: %v", name, val, err)
				}
			}
			set("/tuner0/channel", "auto:177000000")
			set("/tuner0/filter", "0x0000 0x0100-0x0102")
			set("/tuner0/target", "udp://"+rx.LocalAddr().String())
			waitFor(t, "tvheadend's stream", 4*time.Second, func() bool { return udp.Load() > 188*50 })

			controlSince := func() int64 {
				var a struct {
					Tuners []struct {
						Viewers []struct {
							Control bool
							Since   int64
						}
					}
				}
				getJSON(t, e.api+"/ui/api/activity", &a)
				for _, tn := range a.Tuners {
					for _, v := range tn.Viewers {
						if v.Control {
							return v.Since
						}
					}
				}
				return 0
			}
			since0 := controlSince()
			time.Sleep(1200 * time.Millisecond)

			c, rc := e.open("/auto/v7.2")
			if c == nil {
				t.Fatal("/auto/ refused")
			}
			defer c.close()
			if an := analyze(t, c.read(t, 188*100)); !an.esProgs[2] || an.esProgs[1] || an.ccErrors != 0 {
				t.Fatalf("the joiner got %v (cc=%d)", an.esProgs, an.ccErrors)
			}
			res := rc.Header.Get("X-HDHomeRun-Resource")
			if join {
				if res != "tuner0" || e.activePhysical() != 1 {
					t.Fatalf("/auto/ should ride on tuner0: %q, %d physical tuners", res, e.activePhysical())
				}
			} else {
				if res == "tuner0" || e.activePhysical() != 2 {
					t.Fatalf("/auto/ should take its own tuner: %q, %d physical tuners", res, e.activePhysical())
				}
				return
			}

			if s := controlSince(); since0 == 0 || s != since0 {
				t.Fatalf("tvheadend's time watched changed when the viewer joined: %d -> %d", since0, s)
			}
			// tvheadend goes on receiving, and changing its filter leaves the viewer alone
			before := udp.Load()
			set("/tuner0/filter", "0x0000 0x0100-0x0102 0x0110-0x0112")
			if an := analyze(t, c.read(t, 188*200)); !an.esProgs[2] || an.esProgs[1] || an.ccErrors != 0 {
				t.Fatalf("the joiner after the filter change: %v (cc=%d)", an.esProgs, an.ccErrors)
			}
			waitFor(t, "tvheadend still receiving", 3*time.Second, func() bool { return udp.Load() > before+188*50 })

			// tvheadend lets go: the viewer keeps the tuner, and it is freed with the viewer
			set("/tuner0/target", "none")
			set("/tuner0/channel", "none")
			if an := analyze(t, c.read(t, 188*200)); !an.esProgs[2] || an.ccErrors != 0 {
				t.Fatalf("the joiner after tvheadend let go: %v (cc=%d)", an.esProgs, an.ccErrors)
			}
			if n := e.activePhysical(); n != 1 {
				t.Fatalf("%d physical tuners after tvheadend let go, want 1", n)
			}
			c.close()
			waitFor(t, "the tuner freed with its last viewer", 4*time.Second, func() bool { return e.activePhysical() == 0 })
		})
	}
}

// The same station can be received on a second RF channel (another antenna, a
// feed) that the lineup does not list. A session that already carries it is ridden
// by /auto/ instead of taking a tuner, when auto_join_other_locations is on.
func TestAutoRidesASessionAtAnotherLocation(t *testing.T) {
	for _, ride := range []bool{true, false} {
		t.Run(fmt.Sprintf("ride=%v", ride), func(t *testing.T) {
			testChannels = append(append([]fakehdhr.Channel{}, channels...),
				fakehdhr.Channel{Frequency: 191000000, TSID: 9, Programs: []fakehdhr.Program{{Number: 5, VChannel: "7.1", Name: "ALPHA"}}})
			defer func() { testChannels = nil }()
			e := setup(t, []int{3}, 3, func(c *vdev.Config) { c.AutoJoinOtherLocations, c.AutoJoinDirect = ride, true })
			ctl := hdhrproto.NewControl(e.ctl)
			defer ctl.Close()
			rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer rx.Close()
			// somebody (tvheadend) is on 191 MHz, where the lineup does not put 7.1
			for name, val := range map[string]string{"channel": "auto:191000000", "target": "udp://" + rx.LocalAddr().String()} {
				if _, err := ctl.Set("/tuner0/"+name, val, 0); err != nil {
					t.Fatalf("set %s: %v", name, err)
				}
			}
			waitFor(t, "its channel table", 6*time.Second, func() bool { return len(e.hub.CarryingChannel("7.1")) > 0 })
			// streams teach the lineup where they saw a channel; the lineup's place is 177 MHz
			e.lu.Replace(testLineup)

			c, rc := e.open("/auto/v7.1")
			if c == nil {
				t.Fatal("/auto/ refused")
			}
			defer c.close()
			res := rc.Header.Get("X-HDHomeRun-Resource")
			an := analyze(t, c.read(t, 188*200))
			if an.ccErrors != 0 {
				t.Fatalf("continuity errors %d", an.ccErrors)
			}
			if ride {
				if res != "tuner0" || !an.esProgs[5] || e.activePhysical() != 1 {
					t.Fatalf("should ride tuner0 at 191 MHz: %q programmes %v, %d physical tuners", res, an.esProgs, e.activePhysical())
				}
			} else if res == "tuner0" || !an.esProgs[1] || e.activePhysical() != 2 {
				t.Fatalf("should open the lineup's own place: %q programmes %v, %d physical tuners", res, an.esProgs, e.activePhysical())
			}
		})
	}
}

// Every tuner busy: a control client that only collects tables (tvheadend's idle
// guide scan) gives its tuner up to a viewer; one that is watching a programme
// does not.
func TestEPGScansGiveWayToViewers(t *testing.T) {
	for _, evict := range []bool{true, false} {
		t.Run(fmt.Sprintf("evict=%v", evict), func(t *testing.T) {
			e := setup(t, []int{2}, 2, func(c *vdev.Config) { c.EvictScans, c.ScanGrace = evict, 300*time.Millisecond })
			e.lu.Replace(append(append([]lineup.Entry{}, testLineup...), extraLineup...))
			ctl := hdhrproto.NewControl(e.ctl)
			defer ctl.Close()
			rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer rx.Close()
			set := func(name, val string) {
				t.Helper()
				if _, err := ctl.Set(name, val, 0); err != nil {
					t.Fatalf("set %s %s: %v", name, val, err)
				}
			}
			tgt := "udp://" + rx.LocalAddr().String()
			set("/tuner0/channel", "auto:177000000") // collecting tables only
			set("/tuner0/filter", "0x0000 0x1ffb 0x1d00-0x1d0f")
			set("/tuner0/target", tgt)
			set("/tuner1/channel", "auto:473000000") // watching programme 1
			set("/tuner1/filter", "0x0000 0x0100-0x0102")
			set("/tuner1/target", tgt)
			time.Sleep(700 * time.Millisecond) // past the grace period

			c, rc := e.open("/auto/v36.1")
			if !evict {
				if c != nil {
					c.close()
					t.Fatal("served although every tuner is held")
				}
				if rc.StatusCode != 503 {
					t.Fatalf("status %d, want 503", rc.StatusCode)
				}
				return
			}
			if c == nil {
				t.Fatalf("the scan did not give way: %d %v", rc.StatusCode, rc.Header)
			}
			defer c.close()
			if r := rc.Header.Get("X-HDHomeRun-Resource"); r != "tuner0" {
				t.Fatalf("took %q, want the scanning tuner0", r)
			}
			if an := analyze(t, c.read(t, 188*100)); an.ccErrors != 0 || !an.esProgs[1] {
				t.Fatalf("viewer got %v (cc=%d) pids=%v", an.esProgs, an.ccErrors, an.pids)
			}
			// the one watching a programme is left alone
			d, rd := e.open("/auto/v14.3")
			if d != nil {
				d.close()
				t.Fatalf("a watched tuner was taken: %v", rd.Header)
			}
			if rd.StatusCode != 503 {
				t.Fatalf("status %d, want 503", rd.StatusCode)
			}
		})
	}
}

// A tuner's symbol quality reads 0 for a moment after it locks. Channels DVR counts
// every second of it as a signal error, so for signal_settle after a stream starts
// a zero is shown as the last good reading; the activity page keeps the truth.
func TestSignalQualityZeroIsHiddenAtStart(t *testing.T) {
	for _, settle := range []time.Duration{2 * time.Second, 0} {
		t.Run(fmt.Sprint("settle=", settle), func(t *testing.T) {
			old1, old2 := testSEQRamp, testSettle
			testSEQRamp, testSettle = 1500*time.Millisecond, settle
			defer func() { testSEQRamp, testSettle = old1, old2 }()
			e := setup(t, []int{2}, 2, nil)
			c, rc := e.open("/auto/v7.1")
			if c == nil {
				t.Fatalf("refused: %d", rc.StatusCode)
			}
			defer c.close()
			res := rc.Header.Get("X-HDHomeRun-Resource")
			ctl := hdhrproto.NewControl(e.ctl)
			defer ctl.Close()
			zeroSeen, readings := 0, 0
			for i := 0; i < 12; i++ { // the first 1.2 s, while the fake reports 0
				st, err := ctl.Get("/" + res + "/status")
				if err != nil {
					t.Fatal(err)
				}
				readings++
				if strings.Contains(st, "seq=0 ") {
					zeroSeen++
				}
				time.Sleep(100 * time.Millisecond)
			}
			if settle > 0 && zeroSeen != 0 {
				t.Errorf("a client saw a symbol quality of 0 in %d of %d readings with signal_settle on", zeroSeen, readings)
			}
			if settle == 0 && zeroSeen == 0 {
				t.Errorf("no zero readings at all with signal_settle off: the test is not testing anything")
			}
			time.Sleep(1500 * time.Millisecond)
			st, _ := ctl.Get("/" + res + "/status")
			if !strings.Contains(st, "seq=100 ") {
				t.Errorf("after the ramp: %q", st)
			}
		})
	}
}

// Ending a stream (the last viewer leaves) is not a failure of it: the closing connection
// must not be taken for a broken one and "failed over".
func TestEndingAStreamIsNotAFailover(t *testing.T) {
	e := setup(t, []int{4}, 4, nil)
	e.lu.Replace(testLineup)
	failovers := func() float64 {
		resp, err := http.Get(e.api + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		for _, ln := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(ln, "hdhrmux_stream_failovers_total "); ok {
				var f float64
				fmt.Sscan(v, &f)
				return f
			}
		}
		return -1
	}
	for i := 0; i < 40; i++ {
		c, rc := e.open("/auto/v7.1")
		if rc.StatusCode != 200 {
			t.Fatalf("round %d: %d", i, rc.StatusCode)
		}
		select {
		case <-c.data:
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: no data", i)
		}
		c.cancel()
		time.Sleep(60 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
	if n := failovers(); n != 0 {
		t.Errorf("%v failovers while only ending streams", n)
	}
}
