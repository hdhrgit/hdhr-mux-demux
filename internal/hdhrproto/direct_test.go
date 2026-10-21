//go:build linux

package hdhrproto

import (
	"net"
	"testing"
	"time"
)

type directHandler struct{ viaDirect, viaPlain chan bool }

func (h directHandler) Discover(net.IP, uint32, uint32) *Packet {
	h.viaPlain <- true
	return nil
}
func (h directHandler) DiscoverDirect(net.IP, uint32, uint32) *Packet {
	h.viaDirect <- true
	return nil
}
func (h directHandler) GetSet(net.IP, string, *string, *uint32) (string, error) { return "", nil }

// A discovery request sent to one of the machine's own addresses is told from a
// broadcast one.
func TestUDPUnicastIsDirectBroadcastIsNot(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	h := directHandler{make(chan bool, 4), make(chan bool, 4)}
	go ServeUDP(pc, h)
	port := pc.LocalAddr().(*net.UDPAddr).Port

	req := &Packet{Type: TypeDiscoverReq}
	req.AddU32(TagDeviceType, DeviceTypeWildcard)
	req.AddU32(TagDeviceID, DeviceIDWildcard)
	send := func(dst string) {
		c, err := net.Dial("udp4", net.JoinHostPort(dst, itoa(port)))
		if err != nil {
			t.Skipf("cannot send to %s: %v", dst, err)
		}
		defer c.Close()
		if _, err := c.Write(req.Encode()); err != nil {
			t.Skipf("cannot send to %s: %v", dst, err)
		}
	}
	send("127.0.0.1")
	select {
	case <-h.viaDirect:
	case <-h.viaPlain:
		t.Fatal("a request to the machine's own address was treated as a broadcast")
	case <-time.After(time.Second):
		t.Fatal("no request arrived")
	}
	send("127.255.255.255")
	select {
	case <-h.viaPlain:
	case <-h.viaDirect:
		t.Fatal("a broadcast was treated as addressed to the machine")
	case <-time.After(time.Second):
		t.Skip("the system did not deliver a loopback broadcast")
	}
}

func itoa(n int) string {
	b := []byte{}
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
