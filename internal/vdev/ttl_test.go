package vdev

import (
	"net"
	"testing"
)

func TestSetTTL(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("tcp4", ln.Addr().String())
		if err == nil {
			defer c.Close()
			buf := make([]byte, 1)
			c.Read(buf)
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !setTTL(c, 100) || getTTL(c, false) != 8 {
		t.Fatalf("a TTL of 100 should be cut to 8, got %d", getTTL(c, false))
	}
	if !setTTL(c, 3) {
		t.Fatal("setTTL failed")
	}
	if got := getTTL(c, false); got != 3 {
		t.Fatalf("TTL %d, want 3", got)
	}
	u, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if !setTTL(u, 3) || getTTL(u, false) != 3 {
		t.Fatalf("UDP TTL %d", getTTL(u, false))
	}
}

func TestClampTTL(t *testing.T) {
	for in, want := range map[int]int{-1: 3, 0: 3, 1: 1, 3: 3, 8: 8, 9: 8, 64: 8, 255: 8} {
		if got := clampTTL(in); got != want {
			t.Errorf("clampTTL(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestPrivateTarget(t *testing.T) {
	for ip, want := range map[string]bool{
		"10.1.2.3": true, "172.16.0.9": true, "172.31.31.103": true, "192.168.1.5": true,
		"127.0.0.1": true, "169.254.1.1": true,
		"8.8.8.8": false, "172.32.0.1": false, "100.64.0.1": false, "224.0.0.251": false,
		"255.255.255.255": false, "::1": false, "fe80::1": false, "2001:db8::1": false,
	} {
		if got := privateTarget(net.ParseIP(ip)); got != want {
			t.Errorf("privateTarget(%s) = %v, want %v", ip, got, want)
		}
	}
	if _, err := parseTarget("udp://8.8.8.8:5000"); err == nil {
		t.Error("a public target was accepted")
	}
	if _, err := parseTarget("rtp://192.168.1.5:5000"); err != nil {
		t.Errorf("a private target was refused: %v", err)
	}
}
