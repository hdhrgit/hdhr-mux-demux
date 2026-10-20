package main

import (
	"testing"

	"hdhrmux/internal/config"
)

func TestPickConfig(t *testing.T) {
	yes, no := func(string) bool { return true }, func(string) bool { return false }
	for _, c := range []struct {
		flag   string
		exists func(string) bool
		want   string
	}{
		{"", no, ""},                    // no file: defaults and the settings page
		{"", yes, defaultConfig},        // somebody made /etc/hdhr-mux/config.json
		{"/x/c.json", yes, "/x/c.json"}, // an explicit -config always wins
		{"/x/c.json", no, "/x/c.json"},
	} {
		if got := pickConfig(c.flag, c.exists); got != c.want {
			t.Errorf("pickConfig(%q) = %q, want %q", c.flag, got, c.want)
		}
	}
}

func TestHTTPOverridesAndFallback(t *testing.T) {
	c := &config.Config{HTTPListen: ":80", HTTPListenDefault: true}
	if got := httpFallback(c); got != ":8080" {
		t.Errorf("fallback %q, want :8080", got)
	}
	c.Bind = "127.0.0.1"
	if got := httpFallback(c); got != "127.0.0.1:8080" {
		t.Errorf("fallback with bind %q", got)
	}
	applyOverrides(c, ":9090")
	if c.HTTPListen != ":9090" || c.HTTPListenDefault {
		t.Errorf("-http not applied: %q default=%v", c.HTTPListen, c.HTTPListenDefault)
	}
	if got := httpFallback(c); got != "" {
		t.Errorf("a port chosen on purpose must not fall back: %q", got)
	}
	d := &config.Config{HTTPListen: ":80", HTTPListenDefault: true}
	applyOverrides(d, "")
	if d.HTTPListen != ":80" || !d.HTTPListenDefault {
		t.Errorf("no -http changed the config: %+v", d)
	}
}
