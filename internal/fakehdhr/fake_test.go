package fakehdhr

import (
	"net"
	"testing"
	"time"

	"hdhrmux/internal/hdhrproto"
)

func TestStreamsToTarget(t *testing.T) {
	d, err := Start(Config{Tuners: 1, Channels: []Channel{{Frequency: 177000000, TSID: 1, Programs: []Program{{Number: 1, VChannel: "7.1", Name: "A"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rx, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer rx.Close()
	c := hdhrproto.NewControl(d.Addr())
	for _, kv := range [][2]string{{"channel", "auto:177000000"}, {"target", "udp://" + rx.LocalAddr().String()}} {
		if _, err := c.Set("/tuner0/"+kv[0], kv[1], 0); err != nil {
			t.Fatal(kv, err)
		}
	}
	rx.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, err := rx.Read(buf)
	if err != nil || n%188 != 0 || buf[0] != 0x47 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
