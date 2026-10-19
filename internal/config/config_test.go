package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
	if c := load(t, `{"stream_ttl": 0}`); *c.StreamTTL != 0 {
		t.Fatal("stream_ttl 0 (the system default) was overridden")
	}
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"stream_ttl": 300}`), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("stream_ttl 300 accepted")
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
