package vdev

import (
	"net"
	"testing"
)

func TestNoSSDPWhenTheWebInterfaceIsLoopbackOnly(t *testing.T) {
	lo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lo.Close()
	all, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer all.Close()
	for _, c := range []struct {
		name string
		cfg  Config
		ln   net.Listener
		want bool
	}{
		{"all addresses", Config{SSDP: true}, all, true},
		{"loopback only", Config{SSDP: true}, lo, false},
		{"switched off", Config{SSDP: false}, all, false},
		{"hidden", Config{SSDP: true, Hidden: true}, all, false},
	} {
		if got := ssdpWanted(c.cfg, c.ln); got != c.want {
			t.Errorf("%s: ssdpWanted = %v, want %v", c.name, got, c.want)
		}
	}
}
