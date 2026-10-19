package main

import "testing"

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
