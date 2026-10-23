package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hdhrmux/internal/config"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/vdev"
)

// The settings page's API: reading, saving, refusing bad input and cross-site
// requests, locking, and the restart button.
func TestSettingsAPI(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	state := filepath.Join(dir, "state")
	os.WriteFile(cfgPath, []byte(`{"state_dir":"`+state+`","friendly_name":"From File"}`), 0o644)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	restarted := make(chan struct{}, 1)
	e := setup(t, []int{2}, 2, func(c *vdev.Config) {
		c.StateDir = state
		c.Settings = mgr(cfgPath, cfg)
		c.Restart = func() { restarted <- struct{}{} }
	})

	call := func(method, path, body string, hdr map[string]string) (int, map[string]any) {
		req, _ := http.NewRequest(method, e.api+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		return r.StatusCode, m
	}
	admin := map[string]string{"X-Requested-With": "hdhr-mux-ui", "Content-Type": "application/json"}

	code, m := call("GET", "/ui/api/settings", "", nil)
	if code != 200 || len(m["schema"].([]any)) < 20 || m["values"].(map[string]any)["friendly_name"] != "From File" {
		t.Fatalf("GET: %d %v", code, m)
	}
	if st := m["storage"].(map[string]any); st["path"] == "" || st["writable"] != true {
		t.Errorf("storage %v", st)
	}
	if m["restartable"] != true || m["locked"] != false {
		t.Errorf("flags %v", m)
	}

	put := `{"changes":{"friendly_name":"Living Room","guide.parallel":"4","upstream.tuners":["10A22E9C/0-1"]}}`
	if code, _ := call("PUT", "/ui/api/settings", put, map[string]string{"Content-Type": "application/json"}); code != 403 {
		t.Errorf("PUT without the admin header: %d", code)
	}
	cross := map[string]string{"X-Requested-With": "hdhr-mux-ui", "Origin": "http://evil.example"}
	if code, _ := call("PUT", "/ui/api/settings", put, cross); code != 403 {
		t.Errorf("cross-site PUT: %d", code)
	}
	if _, err := os.Stat(filepath.Join(state, "settings.json")); err == nil {
		t.Fatal("a refused request wrote settings")
	}
	code, m = call("PUT", "/ui/api/settings", put, admin)
	if code != 200 || m["ok"] != true {
		t.Fatalf("PUT: %d %v", code, m)
	}
	// the two that are read at start wait for a restart; the guide setting is live
	if p := fmt.Sprint(m["pendingRestart"]); p != "[friendly_name upstream.tuners]" {
		t.Errorf("pending restart %s, want [friendly_name upstream.tuners]", p)
	}
	if v := m["values"].(map[string]any); v["friendly_name"] != "From File" || v["guide.parallel"] != float64(4) {
		t.Errorf("values after saving: friendly_name %v (still running with the old), guide.parallel %v (live)", v["friendly_name"], v["guide.parallel"])
	}
	b, _ := os.ReadFile(filepath.Join(state, "settings.json"))
	if !strings.Contains(string(b), `"friendly_name": "Living Room"`) || !strings.Contains(string(b), `"parallel": 4`) || !strings.Contains(string(b), "10A22E9C/0-1") {
		t.Errorf("settings.json:\n%s", b)
	}
	if got, _ := os.ReadFile(cfgPath); strings.Contains(string(got), "Living Room") {
		t.Error("the config file was modified")
	}
	if c2, err := config.Load(cfgPath); err != nil || c2.FriendlyName != "Living Room" {
		t.Errorf("reload: %v %v", err, c2)
	}
	code, m = call("PUT", "/ui/api/settings", `{"changes":{"linger":"soon"}}`, admin)
	if code != 400 || !strings.Contains(m["error"].(string), "duration") {
		t.Errorf("bad value: %d %v", code, m)
	}
	code, m = call("PUT", "/ui/api/settings", `{"changes":{"friendly_name":null}}`, admin)
	if code != 200 {
		t.Fatalf("reset: %d %v", code, m)
	}
	if c3, _ := config.Load(cfgPath); c3.FriendlyName != "From File" {
		t.Errorf("after reset: %q", c3.FriendlyName)
	}

	if code, _ := call("POST", "/ui/api/restart", "", nil); code != 403 {
		t.Errorf("restart without the admin header: %d", code)
	}
	if code, _ := call("POST", "/ui/api/restart", "", admin); code != 200 {
		t.Fatalf("restart: %d", code)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Error("restart was not triggered")
	}
}

func TestSettingsLockedAndAbsent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"state_dir":"`+dir+`"}`), 0o644)
	cfg, _ := config.Load(cfgPath)
	e := setup(t, []int{1}, 1, func(c *vdev.Config) {
		c.StateDir, c.Settings, c.SettingsLocked = dir, mgr(cfgPath, cfg), true
	})
	req, _ := http.NewRequest("PUT", e.api+"/ui/api/settings", strings.NewReader(`{"changes":{"friendly_name":"x"}}`))
	req.Header.Set("X-Requested-With", "hdhr-mux-ui")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 403 {
		t.Errorf("locked PUT: %v %v", err, r)
	}
	if r, err := http.Get(e.api + "/ui/api/settings"); err != nil || r.StatusCode != 200 {
		t.Errorf("a locked page still shows its values: %v %v", err, r)
	}

	e2 := setup(t, []int{1}, 1, nil)
	if r, err := http.Get(e2.api + "/ui/api/settings"); err != nil || r.StatusCode != 404 {
		t.Errorf("without a manager the settings are absent: %v %v", err, r)
	}
}

// A setting that does not need a restart takes effect when it is saved.
func TestSettingsApplyWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	state := filepath.Join(dir, "state")
	os.WriteFile(cfgPath, []byte(`{"state_dir":"`+state+`","auto_join_direct":false}`), 0o644)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	e := setup(t, []int{3}, 3, func(c *vdev.Config) {
		c.StateDir, c.Settings, c.AutoJoinDirect = state, mgr(cfgPath, cfg), false
	})
	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	for name, val := range map[string]string{"channel": "auto:177000000", "target": "udp://" + rx.LocalAddr().String()} {
		if _, err := ctl.Set("/tuner0/"+name, val, 0); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	resource := func() string {
		c, rc := e.open("/auto/v7.1")
		if c == nil {
			t.Fatalf("/auto/ refused: %d", rc.StatusCode)
		}
		defer c.close()
		c.read(t, 188*20)
		return rc.Header.Get("X-HDHomeRun-Resource")
	}
	if r := resource(); r == "tuner0" {
		t.Fatalf("joined a direct client's tuner with auto_join_direct off: %s", r)
	}
	req, _ := http.NewRequest("PUT", e.api+"/ui/api/settings", strings.NewReader(`{"changes":{"auto_join_direct":true}}`))
	req.Header.Set("X-Requested-With", "hdhr-mux-ui")
	req.Header.Set("Content-Type", "application/json")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("PUT: %v %v", err, r)
	}
	waitFor(t, "the tuner of the direct client is shared once the setting is on", 4*time.Second, func() bool { return resource() == "tuner0" })
}

// When the web port is the default and cannot be used (privileged, or taken by
// another program) the multiplexer moves to the fallback and says so, instead of
// failing to start; a port chosen on purpose is never changed.
func TestWebPortFallsBack(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	e := setup(t, []int{1}, 1, func(c *vdev.Config) {
		c.HTTPAddr, c.HTTPFallback = busy.Addr().String(), "127.0.0.1:0"
	})
	if e.dev.HTTPPort() == busy.Addr().(*net.TCPAddr).Port {
		t.Fatal("did not move off the busy port")
	}
	if r, err := http.Get(e.api + "/discover.json"); err != nil || r.StatusCode != 200 {
		t.Errorf("not serving on the fallback: %v %v", err, r)
	}
}

// With no devices the multiplexer still starts, keeps quiet on the network and says
// why on its web pages, so the settings page can be used to fix it.
func TestNoDevicesStartsInSetupMode(t *testing.T) {
	msg := "No HDHomeRun devices were found."
	e := setup(t, []int{}, 1, func(c *vdev.Config) { c.Setup = msg })
	var a struct{ Setup string }
	getJSON(t, e.api+"/ui/api/activity", &a)
	if a.Setup != msg {
		t.Errorf("activity setup %q", a.Setup)
	}
	infos, _ := hdhrproto.Discover([]string{e.ctl}, 600*time.Millisecond)
	if len(infos) != 0 {
		t.Errorf("a multiplexer with nothing to serve answered discovery: %v", infos)
	}
}

// A list setting can be typed one per line, or separated by commas or spaces.
func TestListSettingsAcceptCommasAndSpaces(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	state := filepath.Join(dir, "state")
	os.WriteFile(cfgPath, []byte(`{"state_dir":"`+state+`"}`), 0o644)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	e := setup(t, []int{1}, 1, func(c *vdev.Config) { c.StateDir, c.Settings = state, mgr(cfgPath, cfg) })
	req, _ := http.NewRequest("PUT", e.api+"/ui/api/settings", strings.NewReader(`{"changes":{"upstream.devices":["10.0.0.1, 10.0.0.2 10.0.0.3\n10.0.0.4"]}}`))
	req.Header.Set("X-Requested-With", "hdhr-mux-ui")
	req.Header.Set("Content-Type", "application/json")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("PUT: %v %v", err, r)
	}
	b, err := os.ReadFile(filepath.Join(state, "settings.json")) // read at start, so the running values wait for a restart
	if err != nil {
		t.Fatal(err)
	}
	var saved struct{ Upstream struct{ Devices []string } }
	json.Unmarshal(b, &saved)
	got := saved.Upstream.Devices
	if len(got) != 4 || got[0] != "10.0.0.1" || got[3] != "10.0.0.4" {
		t.Errorf("devices %v", got)
	}
}
