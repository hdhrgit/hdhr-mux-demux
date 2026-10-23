package e2e

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/vdev"
)

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
	// A tuner a control client has tuned is its own, as on a real device: an /auto/
	// viewer of the same RF channel gets another tuner rather than riding on it.
	h, _ := e.open("/auto/v7.1")
	defer h.close()
	h.read(t, 188*20)
	if e.activePhysical() != 2 {
		t.Fatalf("expected 2 physical tuners (the control client's and the /auto/ viewer's), got %d", e.activePhysical())
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
	// (a request sent to its own address, as opposed to a broadcast, is answered: see
	// the hdhrproto test for the broadcast)
	if infos, _ := hdhrproto.Discover([]string{e.ctl}, 600*time.Millisecond); len(infos) != 1 {
		t.Fatalf("a hidden multiplexer did not answer wildcard discovery sent to its address: %+v", infos)
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
	// a client that connected over TCP knows where we are: wildcard discovery is answered there
	wild := &hdhrproto.Packet{Type: hdhrproto.TypeDiscoverReq}
	wild.AddU32(hdhrproto.TagDeviceType, hdhrproto.DeviceTypeWildcard)
	wild.AddU32(hdhrproto.TagDeviceID, hdhrproto.DeviceIDWildcard)
	tc, err := net.Dial("tcp", e.ctl)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	tc.Write(wild.Encode())
	tc.SetReadDeadline(time.Now().Add(time.Second))
	n, err = tc.Read(buf)
	if err != nil {
		t.Fatalf("no answer to wildcard discovery over TCP: %v", err)
	}
	if p, err := hdhrproto.Decode(buf[:n]); err != nil || p.Type != hdhrproto.TypeDiscoverRpy {
		t.Fatalf("bad TCP answer: %v %v", p, err)
	}
	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	if v, err := ctl.Get("/sys/model"); err != nil || v == "" {
		t.Fatalf("direct control connection: %q %v", v, err)
	}
	var dj map[string]any
	getJSON(t, e.api+"/discover.json", &dj)
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

// A control request is unauthenticated and its source can be forged: video is
// only ever sent to addresses on a private IPv4 network (or this machine).
func TestTargetsMustBePrivate(t *testing.T) {
	e := setup(t, []int{1}, 1, nil)
	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	if _, err := ctl.Set("/tuner0/channel", "auto:177000000", 0); err != nil {
		t.Fatal(err)
	}
	for _, tgt := range []string{"udp://8.8.8.8:5000", "rtp://93.184.216.34:5000", "udp://224.0.0.251:5000"} {
		if _, err := ctl.Set("/tuner0/target", tgt, 0); err == nil {
			t.Errorf("target %s accepted", tgt)
		}
	}
	for _, tgt := range []string{"udp://127.0.0.1:5000", "rtp://192.168.1.50:5004", "udp://10.0.0.7:1234"} {
		if _, err := ctl.Set("/tuner0/target", tgt, 0); err != nil {
			t.Errorf("target %s refused: %v", tgt, err)
		}
	}
	ctl.Set("/tuner0/target", "none", 0)
}
