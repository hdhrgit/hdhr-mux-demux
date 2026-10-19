package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, json string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if json != "" {
		os.WriteFile(p, []byte(json), 0o644)
	} else {
		p = ""
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaults(t *testing.T) {
	c := load(t, "")
	if !*c.Upstream.Discover || !*c.Discoverable || !*c.SSDP {
		t.Fatal("discovery, discoverability and SSDP should default to on")
	}
}

func TestStreamTTL(t *testing.T) {
	if c := load(t, ""); *c.StreamTTL != 3 || !*c.AutoJoinDirect {
		t.Fatalf("stream_ttl %d, auto_join_direct %v: want 3 and true", *c.StreamTTL, *c.AutoJoinDirect)
	}
	for in, want := range map[int]int{0: 3, 1: 1, 5: 5, 8: 8, 9: 8, 255: 8, 100000: 8} {
		if c := load(t, fmt.Sprintf(`{"stream_ttl": %d}`, in)); *c.StreamTTL != want {
			t.Errorf("stream_ttl %d became %d, want %d", in, *c.StreamTTL, want)
		}
	}
}

func TestEnvironmentOverrides(t *testing.T) {
	t.Setenv("HDHOMERUN_DEVICES", "192.168.1.50, hdhr.local ,")
	t.Setenv("HDHOMERUN_DISABLE_DISCOVERY", "true")
	t.Setenv("HDHOMERUN_TUNERS", "10813750/0-1,10A22E9C")
	t.Setenv("HDHOMERUN_DISCOVERABLE", "false")
	c := load(t, `{"upstream":{"devices":["10.0.0.9"],"tuners":["FFFFFFFF/0"]}}`)
	if want := []string{"10.0.0.9", "192.168.1.50", "hdhr.local"}; !reflect.DeepEqual(c.Upstream.Devices, want) {
		t.Errorf("devices %v, want %v", c.Upstream.Devices, want)
	}
	if *c.Upstream.Discover {
		t.Error("HDHOMERUN_DISABLE_DISCOVERY=true should switch discovery off")
	}
	if want := []string{"10813750/0-1", "10A22E9C"}; !reflect.DeepEqual(c.Upstream.Tuners, want) {
		t.Errorf("tuners %v, want %v (the environment replaces the file)", c.Upstream.Tuners, want)
	}
	if *c.Discoverable {
		t.Error("HDHOMERUN_DISCOVERABLE=false should hide the multiplexer")
	}
}

func TestSSDPFamilies(t *testing.T) {
	c := load(t, "")
	if !*c.SSDPIPv4 || *c.SSDPIPv6 {
		t.Fatal("SSDP should default to IPv4 on, IPv6 off")
	}
	c = load(t, `{"ssdp_ipv6": true, "ssdp_ipv4": false}`)
	if *c.SSDPIPv4 || !*c.SSDPIPv6 {
		t.Fatalf("ipv4 %v ipv6 %v, want false/true", *c.SSDPIPv4, *c.SSDPIPv6)
	}
}

func TestSSDPATSC3Default(t *testing.T) {
	if *load(t, "").SSDPATSC3 {
		t.Fatal("the ATSC 3.0 device should not be announced by default")
	}
	if !*load(t, `{"ssdp_atsc3": true}`).SSDPATSC3 {
		t.Fatal("ssdp_atsc3: true ignored")
	}
}

func TestSettingsOverlayAndSave(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "c.json")
	state := filepath.Join(dir, "state")
	os.WriteFile(cfgPath, []byte(`{"state_dir":"`+state+`","friendly_name":"From File","guide":{"refresh":"6h","hold":"30s"}}`), 0o644)
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfgPath, c)
	if v := m.Values(); v["friendly_name"] != "From File" || v["guide.refresh"] != "6h0m0s" || v["upstream.mask"] != true || v["idle_release"] != "" {
		t.Fatalf("values %v", v)
	}

	// save: becomes visible in settings.json, config file untouched, merged over the file
	if err := m.Save(map[string]any{"friendly_name": "Web Name", "guide.parallel": "5", "upstream.tuners": "10A22E9C/0-1\n10813750", "ssdp_ipv6": true, "linger": "9s"}); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c2.FriendlyName != "Web Name" || c2.Guide.Parallel != 5 || !*c2.SSDPIPv6 || c2.Linger != Duration(9*time.Second) ||
		!reflect.DeepEqual(c2.Upstream.Tuners, []string{"10A22E9C/0-1", "10813750"}) {
		t.Fatalf("overlay not applied: %+v", c2)
	}
	if c2.Guide.Hold != Duration(30*time.Second) || c2.Guide.Refresh != Duration(6*time.Hour) {
		t.Fatalf("file values lost under the overlay: %+v", c2.Guide)
	}
	if b, _ := os.ReadFile(cfgPath); !strings.Contains(string(b), "From File") || strings.Contains(string(b), "Web Name") {
		t.Fatalf("config file was modified: %s", b)
	}
	if got := NewManager(cfgPath, c2).Saved(); len(got) != 5 {
		t.Fatalf("saved keys %v", got)
	}

	// resetting a key brings back the file's value
	if err := NewManager(cfgPath, c2).Save(map[string]any{"friendly_name": nil}); err != nil {
		t.Fatal(err)
	}
	if c3, _ := Load(cfgPath); c3.FriendlyName != "From File" {
		t.Fatalf("reset: %q", c3.FriendlyName)
	}

	// invalid input is refused and nothing changes
	before, _ := os.ReadFile(filepath.Join(state, "settings.json"))
	for name, ch := range map[string]map[string]any{
		"bad duration": {"linger": "soon"}, "bad tuner": {"upstream.tuners": "nonsense"}, "bad id": {"device_id": "12345678"},
		"bad listen": {"http_listen": "eighty"}, "bad choice": {"upstream.mode": "carrier-pigeon"}, "unknown": {"no.such": 1},
		"negative": {"guide.parallel": -2}, "not a bool": {"ssdp": "yes"},
	} {
		if err := NewManager(cfgPath, c2).Save(ch); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(state, "settings.json")); string(after) != string(before) {
		t.Errorf("settings.json changed by refused saves:\n%s\n%s", before, after)
	}

	// the way out of a bad saved setting
	os.WriteFile(filepath.Join(state, "settings.json"), []byte(`{"friendly_name":"Saved"}`), 0o644)
	if c4, _ := LoadWithoutSettings(cfgPath); c4.FriendlyName != "From File" {
		t.Errorf("LoadWithoutSettings used settings.json: %q", c4.FriendlyName)
	}
	// environment wins over saved settings
	t.Setenv("HDHOMERUN_DISCOVERABLE", "false")
	if c5, _ := Load(cfgPath); *c5.Discoverable {
		t.Error("environment did not override")
	}
	if got := NewManager(cfgPath, c2).OverriddenByEnv(); got["discoverable"] != "HDHOMERUN_DISCOVERABLE" {
		t.Errorf("overridden %v", got)
	}
}

func TestClassifyStorage(t *testing.T) {
	root := "100 1 0:50 / / rw - overlay overlay rw\n"
	bind := root + "300 100 259:2 /home/debian/hdhr-mux-run/state /var/lib/hdhr-mux rw - ext4 /dev/nvme0n1p2 rw\n"
	anon := root + "301 100 259:2 /var/lib/docker/volumes/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef/_data /var/lib/hdhr-mux rw - ext4 /dev/sda rw\n"
	named := root + "302 100 259:2 /var/lib/docker/volumes/hdhr-mux-state/_data /var/lib/hdhr-mux rw - ext4 /dev/sda rw\n"
	mem := root + "303 100 0:60 / /var/lib/hdhr-mux rw - tmpfs tmpfs rw\n"
	for name, c := range map[string]struct {
		info, kind string
		container  bool
		warn       bool
	}{
		"bind mount":         {bind, "mounted", true, false},
		"named volume":       {named, "mounted", true, false},
		"anonymous volume":   {anon, "anonymous-volume", true, true},
		"container layer":    {root, "container-layer", true, true},
		"tmpfs":              {mem, "memory", true, true},
		"plain host":         {root, "disk", false, false},
		"plain host on disk": {bind, "disk", false, false},
	} {
		kind, warn := classifyStorage(c.info, "/var/lib/hdhr-mux", c.container)
		if kind != c.kind || (warn != "") != c.warn {
			t.Errorf("%s: kind %q warning %q, want %q (warning %v)", name, kind, warn, c.kind, c.warn)
		}
	}
}

func TestBind(t *testing.T) {
	c := load(t, `{"bind":"127.0.0.1"}`)
	if c.HTTPListen != "127.0.0.1:80" || c.StreamListen != "127.0.0.1:5004" || c.ControlListen != "127.0.0.1:65001" || !c.Loopback() {
		t.Errorf("listeners %q %q %q loopback %v", c.HTTPListen, c.StreamListen, c.ControlListen, c.Loopback())
	}
	// explicit hosts and the disabled stream port are left alone
	c = load(t, `{"bind":"10.1.2.3","http_listen":"0.0.0.0:8080","stream_listen":"-"}`)
	if c.HTTPListen != "0.0.0.0:8080" || c.StreamListen != "-" || c.ControlListen != "10.1.2.3:65001" || c.Loopback() {
		t.Errorf("listeners %q %q %q", c.HTTPListen, c.StreamListen, c.ControlListen)
	}
	if c := load(t, ""); c.HTTPListen != ":80" || c.Loopback() {
		t.Errorf("default %q", c.HTTPListen)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	os.WriteFile(p, []byte(`{"bind":"not-an-ip"}`), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("a bind that is not an IP address was accepted")
	}
}

// The settings added with direct clients are on the page and take effect.
func TestSettingsCoverClientSharing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "c.json")
	os.WriteFile(cfgPath, []byte(`{"state_dir":"`+filepath.Join(dir, "state")+`"}`), 0o644)
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfgPath, c)
	v := m.Values()
	for _, k := range []string{"auto_join_direct", "auto_join_other_locations", "evict_scans"} {
		if v[k] != true {
			t.Errorf("%s = %v, want true by default", k, v[k])
		}
	}
	if v["lineup.scan_mode"] != "device" {
		t.Errorf("scan_mode %v", v["lineup.scan_mode"])
	}
	if err := m.Save(map[string]any{"auto_join_direct": false, "evict_scans": false, "stream_ttl": "50", "lineup.scan_mode": "own"}); err != nil {
		t.Fatal(err)
	}
	c2, _ := Load(cfgPath)
	if *c2.AutoJoinDirect || *c2.EvictScans || !*c2.AutoJoinOtherLocations || *c2.StreamTTL != 8 || c2.Lineup.ScanMode != "own" {
		t.Fatalf("saved values not applied: %v %v %v %v %s", *c2.AutoJoinDirect, *c2.EvictScans, *c2.AutoJoinOtherLocations, *c2.StreamTTL, c2.Lineup.ScanMode)
	}
}
