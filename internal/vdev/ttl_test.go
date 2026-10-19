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
	if setTTL(c, 0) || getTTL(c, false) == 3 {
		t.Fatal("a TTL of 0 should leave the default alone")
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
