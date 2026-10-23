package e2e

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"hdhrmux/internal/chanmap"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/vdev"
)

func TestScan(t *testing.T) {
	withoutTuningInfo(t)
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
	withoutTuningInfo(t)
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
	withoutTuningInfo(t)
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
	// only the channels that are not HD are marked SD (the way Channels DVR wants it)
	if strings.Count(body, `group-id="SD"`) != 1 || !strings.Contains(lines[3], `group-id="SD"`) || strings.Contains(lines[1], "group-id") {
		t.Errorf("SD marking:\n%s", body)
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

// A rescan is handed to one device's own scan: an ATSC 3.0 capable one when
// there is a choice, never one that is in use, and the lineup is re-read from the
// devices afterwards. The device's tuners refuse streams while it scans.
func TestRescanIsHandedToADevice(t *testing.T) {
	old := testModels
	testModels = []string{"hdhomerun_atsc", "hdhomerun5_atsc3"} // device 0 cannot do ATSC 3.0, device 1 can
	t.Cleanup(func() { testModels = old })
	e := setup(t, []int{2, 2}, 4, func(c *vdev.Config) {
		c.ScanMode, c.ScanPollInterval = "device", 100*time.Millisecond
	})
	for _, f := range e.fakes {
		f.HideFromLineup("7.2") // the channel only a scan finds
	}
	ctx := context.Background()
	if err := e.dev.RefreshLineup(ctx); err != nil {
		t.Fatal(err)
	}
	has := func(n string) bool { _, ok := e.lu.Find(n); return ok }
	if has("7.2") {
		t.Fatal("7.2 should not be listed before the scan")
	}

	post := func() int {
		r, err := http.Post(e.api+"/lineup.post?scan=start&source=Antenna", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := post(); code != 200 {
		t.Fatalf("rescan: %d", code)
	}
	if n, src := e.fakes[1].ScanRequests(); n != 1 || src != "Antenna" {
		t.Fatalf("the ATSC 3.0 device should have been asked once, got %d (%q)", n, src)
	}
	if n, _ := e.fakes[0].ScanRequests(); n != 0 {
		t.Fatalf("the other device was asked to scan too: %d", n)
	}
	var st map[string]any
	getJSON(t, e.api+"/lineup_status.json", &st)
	if st["ScanInProgress"] != float64(1) {
		t.Fatalf("lineup_status while scanning: %v", st)
	}
	if code := post(); code != 409 {
		t.Errorf("a second rescan while one runs: %d, want 409", code)
	}
	// the scanning device's tuners (2 and 3) refuse streams; /auto/ goes to the other device
	if _, resp := e.open("/tuner2/v7.1"); resp.StatusCode != 503 || resp.Header.Get("X-HDHomeRun-Error") != "804 Tuner In Use" {
		t.Errorf("a tuner of the scanning device: %d %v", resp.StatusCode, resp.Header)
	}
	h, rh := e.open("/auto/v14.1")
	if h == nil {
		t.Fatalf("/auto/ refused during a scan: %d", rh.StatusCode)
	}
	if r := rh.Header.Get("X-HDHomeRun-Resource"); r != "tuner0" && r != "tuner1" {
		t.Errorf("/auto/ used %s, a tuner of the scanning device", r)
	}
	h.close()

	waitFor(t, "the scan to finish", 5*time.Second, func() bool {
		var s map[string]any
		getJSON(t, e.api+"/lineup_status.json", &s)
		return s["ScanInProgress"] == float64(0)
	})
	waitFor(t, "the channel the scan found", 3*time.Second, func() bool { return has("7.2") })
	c, _ := e.open("/tuner2/v14.1")
	if c == nil {
		t.Fatal("tuner 2 should be usable once the scan is over")
	}
	c.close()

	// busy devices are not scanned with: with a viewer on the ATSC 3.0 device the
	// other one is used, and with both busy the request is refused
	waitFor(t, "released", 4*time.Second, func() bool { return e.activePhysical() == 0 })
	v1, _ := e.open("/tuner2/v7.1")
	if v1 == nil {
		t.Fatal("viewer refused")
	}
	defer v1.close()
	v1.read(t, 188*20)
	if code := post(); code != 200 {
		t.Fatalf("rescan with one device busy: %d", code)
	}
	if n, _ := e.fakes[0].ScanRequests(); n != 1 {
		t.Fatalf("the idle device should have scanned: %d", n)
	}
	waitFor(t, "the scan to finish", 5*time.Second, func() bool {
		var s map[string]any
		getJSON(t, e.api+"/lineup_status.json", &s)
		return s["ScanInProgress"] == float64(0)
	})
	time.Sleep(2200 * time.Millisecond) // the tuners rest briefly after a scan
	v0, _ := e.open("/tuner0/v7.1")
	if v0 == nil {
		t.Fatal("viewer refused")
	}
	defer v0.close()
	v0.read(t, 188*20)
	if code := post(); code != 409 {
		t.Fatalf("rescan with every device busy: %d, want 409", code)
	}
	if n, _ := e.fakes[1].ScanRequests(); n != 1 {
		t.Errorf("a busy device was asked to scan: %d", n)
	}
}

// With lineup.atsc3 off the ATSC 3.0 channels are left out of the listings (lineup,
// playlist, guide) but a client that asks for one by number is still served.
func TestHidingATSC3LeavesThemUnlistedButTunable(t *testing.T) {
	e := setup(t, []int{3}, 3, func(c *vdev.Config) { c.HideATSC3 = true })
	e.lu.Replace(append(append([]lineup.Entry{}, testLineup...), atsc3Lineup...))
	for _, path := range []string{"/lineup.json", "/playlist.m3u8", "/epg"} {
		r, err := http.Get(e.api + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if strings.Contains(string(b), "104.1") || strings.Contains(string(b), "NEXT-A") {
			t.Errorf("%s lists an ATSC 3.0 channel", path)
		}
		if path != "/epg" && !strings.Contains(string(b), "7.1") {
			t.Errorf("%s lost the ATSC 1.0 channels", path)
		}
	}
	c, rc := e.open("/auto/v104.1")
	if c == nil {
		t.Fatalf("a hidden ATSC 3.0 channel was not served: %d %v", rc.StatusCode, rc.Header)
	}
	c.close()
}

// An own scan with no ATSC 3.0 tuner to hand a strong-but-unlockable channel to says
// so, instead of silently missing it.
func TestScanSaysWhenATSC3ChannelsWereNotScanned(t *testing.T) {
	e := setup(t, []int{4}, 4, nil)
	// viewers hold tuners 0 and 1, the ones that can lock ATSC 3.0, so the scan only gets 2 and 3
	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	for i, f := range []string{"auto:177000000", "auto:473000000"} {
		if _, err := ctl.Set(fmt.Sprintf("/tuner%d/channel", i), f, 0); err != nil {
			t.Fatal(err)
		}
	}
	var buf syncLog
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	_, err := e.hub.Scan(context.Background(), hub.ScanConfig{
		Channels: []chanmap.Channel{{Number: 1, Frequency: 605000000}, {Number: 2, Frequency: 177000000}},
		LockWait: 300 * time.Millisecond, InfoWait: 500 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := buf.String(); !strings.Contains(s, "need an ATSC 3.0 tuner and none was free") || !strings.Contains(s, "605000000") {
		t.Errorf("the scan did not say it missed ATSC 3.0 channels:\n%s", s)
	}
}

// The devices' lineups say where each channel is on the air (lineup.json?tuning), so
// the multiplexer knows every channel's place without tuning anything, and a stream
// seen elsewhere does not move it. Its own lineup.json answers ?tuning the same way.
func TestPlacementComesFromTheDevices(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	if err := e.dev.RefreshLineup(context.Background()); err != nil {
		t.Fatal(err)
	}
	for g, want := range map[string]struct {
		freq uint64
		prog uint16
		mod  string
	}{
		"7.1": {177000000, 1, "8vsb"}, "7.2": {177000000, 2, "8vsb"}, "14.3": {473000000, 3, "8vsb"},
		"36.1": {617000000, 1, "8vsb"}, "104.1": {605000000, 1376, "atsc3"},
	} {
		en, ok := e.lu.Find(g)
		if !ok || en.Frequency != want.freq || en.Program != want.prog || en.Modulation != want.mod {
			t.Errorf("%s: %+v, want %d Hz program %d %s", g, en, want.freq, want.prog, want.mod)
		}
	}
	if en, _ := e.lu.Find("7.1"); en.TSID != 1 {
		t.Errorf("7.1 transport stream id %d, want 1", en.TSID)
	}
	for _, f := range e.fakes {
		if f.Peak() != 0 {
			t.Errorf("a tuner was used to learn that (peak %d)", f.Peak())
		}
	}

	// a stream seen on another RF channel does not move a placed channel
	e.lu.LearnRF([]lineup.Entry{{GuideNumber: "7.1", Frequency: 999000000, Program: 9, Modulation: "8vsb"}}, false)
	if en, _ := e.lu.Find("7.1"); en.Frequency != 177000000 {
		t.Errorf("7.1 moved to %d Hz", en.Frequency)
	}

	// the multiplexer's own lineup answers ?tuning like a device
	get := func(q string) []map[string]any {
		var out []map[string]any
		getJSON(t, e.api+"/lineup.json"+q, &out)
		return out
	}
	plain, tuned := get(""), get("?tuning")
	find := func(l []map[string]any, g string) map[string]any {
		for _, m := range l {
			if m["GuideNumber"] == g {
				return m
			}
		}
		return nil
	}
	if m := find(plain, "7.1"); m == nil || m["Frequency"] != nil {
		t.Errorf("plain lineup.json should not carry tuning fields: %v", m)
	}
	m := find(tuned, "7.1")
	if m == nil || m["Frequency"] != float64(177000000) || m["ProgramNumber"] != float64(1) || m["Modulation"] != "8vsb" || m["TransportStreamID"] != float64(1) {
		t.Errorf("lineup.json?tuning for 7.1: %v", m)
	}
}

// A client flips a channel between enabled, favourite and disabled the way it does on a
// device (lineup.post?favorite=): only this lineup changes, and the web page does the same.
func TestChannelStatesFlipLikeOnADevice(t *testing.T) {
	e := setup(t, []int{1}, 1, nil)
	e.lu.Replace(testLineup)
	post := func(v string) {
		t.Helper()
		resp, err := http.Post(e.api+"/lineup.post?favorite="+v, "", nil)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("lineup.post %s: %v %v", v, resp, err)
		}
	}
	numbers := func(q string) map[string]map[string]any {
		t.Helper()
		var l []map[string]any
		getJSON(t, e.api+"/lineup.json"+q, &l)
		out := map[string]map[string]any{}
		for _, c := range l {
			out[c["GuideNumber"].(string)] = c
		}
		return out
	}
	post("%2B7.1") // +7.1
	if c := numbers("")["7.1"]; c == nil || c["Favorite"] != float64(1) {
		t.Fatalf("7.1 should be a favourite: %v", c)
	}
	post("x7.1")
	if numbers("")["7.1"] != nil {
		t.Fatal("a disabled channel is still in the lineup")
	}
	if c := numbers("?show=found")["7.1"]; c == nil || c["Enabled"] != float64(0) || c["Favorite"] != nil {
		t.Fatalf("a disabled channel is listed with Enabled 0 and no star in ?show=found: %v", c)
	}
	if r, err := http.Get(e.http + "/auto/v7.1"); err == nil {
		r.Body.Close()
		if r.StatusCode == 200 {
			t.Error("a disabled channel was tuned")
		}
	}

	// the web page: enable it again, then make another a favourite
	put := func(n, state string) int {
		req, _ := http.NewRequest("PUT", e.api+"/ui/api/lineup/"+n, strings.NewReader(`{"state":"`+state+`"}`))
		req.Header.Set("X-Requested-With", "hdhr-mux-ui")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := put("7.1", "normal"); c != 200 {
		t.Fatalf("PUT normal: %d", c)
	}
	if numbers("")["7.1"] == nil {
		t.Fatal("7.1 was not enabled again")
	}
	if c := put("7.2", "favorite"); c != 200 || numbers("")["7.2"]["Favorite"] != float64(1) {
		t.Fatalf("PUT favorite: %d", c)
	}
	if put("7.2", "bogus") != 400 || put("99.9", "normal") != 404 {
		t.Error("bad state or unknown channel should be refused")
	}
	var ui struct {
		Editable bool
		Channels []struct {
			Number   string
			Favorite bool
			Disabled bool
		}
	}
	post("x14.1")
	getJSON(t, e.api+"/ui/api/lineup", &ui)
	found := false
	for _, c := range ui.Channels {
		if c.Number == "14.1" {
			found = c.Disabled
		}
	}
	if !ui.Editable || !found {
		t.Errorf("the page lists a disabled channel (editable %v, found %v)", ui.Editable, found)
	}
}
