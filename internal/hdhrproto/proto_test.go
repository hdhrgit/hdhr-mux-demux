package hdhrproto

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := &Packet{Type: TypeGetSetReq}
	p.AddString(TagGetSetName, "/tuner0/status")
	p.AddString(TagGetSetValue, string(bytes.Repeat([]byte("x"), 300))) // 2-byte length
	p.AddU32(TagGetSetLock, 0xDEADBEEF)
	wire := p.Encode()
	got, err := Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := got.GetString(TagGetSetName); n != "/tuner0/status" {
		t.Fatal(n)
	}
	if v, _ := got.GetString(TagGetSetValue); len(v) != 300 {
		t.Fatal(len(v))
	}
	if k, _ := got.GetU32(TagGetSetLock); k != 0xDEADBEEF {
		t.Fatal(k)
	}
	wire[6] ^= 1
	if _, err := Decode(wire); err != ErrCRC {
		t.Fatalf("want crc error, got %v", err)
	}
}

// The widely documented wire form of a discovery request for any
// tuner device.
func TestKnownDiscoverFrame(t *testing.T) {
	p := &Packet{Type: TypeDiscoverReq}
	p.AddU32(TagDeviceType, DeviceTypeTuner)
	p.AddU32(TagDeviceID, DeviceIDWildcard)
	want := []byte{0x00, 0x02, 0x00, 0x0c, 0x01, 0x04, 0x00, 0x00, 0x00, 0x01, 0x02, 0x04, 0xff, 0xff, 0xff, 0xff, 0x4e, 0x50, 0x7f, 0x35}
	if got := p.Encode(); !bytes.Equal(got, want) {
		t.Fatalf("got % x\nwant % x", got, want)
	}
}

func TestDeviceID(t *testing.T) {
	for _, id := range []uint32{0x1020FC71, 0x10200000, 0x12345678} {
		m := MakeDeviceID(id)
		if !ValidDeviceID(m) || m&^0xF != id&^0xF {
			t.Fatalf("MakeDeviceID(%08X)=%08X", id, m)
		}
		if ValidDeviceID(m ^ 1) {
			t.Fatalf("%08X should be invalid", m^1)
		}
	}
}
