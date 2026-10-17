package vdev

import (
	"strings"
	"testing"

	"hdhrmux/internal/hdhrproto"
)

func testDevice() *Device {
	return &Device{cfg: Config{DeviceID: hdhrproto.MakeDeviceID(0x10900000)}}
}

func TestSSDPAnswersLikeARealMediaServer(t *testing.T) {
	d := testDevice()
	all := d.ssdpMatch("ssdp:all")
	if len(all) != 5 || all[0] != "upnp:rootdevice" || all[1] != d.udn() {
		t.Fatalf("ssdp:all = %v", all)
	}
	for _, st := range []string{"upnp:rootdevice", "urn:schemas-upnp-org:device:MediaServer:1",
		"URN:SCHEMAS-UPNP-ORG:SERVICE:CONTENTDIRECTORY:1", "urn:schemas-upnp-org:service:ConnectionManager:1", d.udn()} {
		if got := d.ssdpMatch(st); len(got) != 1 || !strings.EqualFold(got[0], st) {
			t.Errorf("search for %q answered with %v", st, got)
		}
	}
	for _, st := range []string{"urn:schemas-upnp-org:device:MediaRenderer:1", "upnp:other", "uuid:someone-else"} {
		if got := d.ssdpMatch(st); got != nil {
			t.Errorf("search for %q must not be answered, got %v", st, got)
		}
	}
	if got := d.ssdpUSN(d.udn()); got != d.udn() {
		t.Errorf("USN of the UDN is %q", got)
	}
	if got, want := d.ssdpUSN("upnp:rootdevice"), d.udn()+"::upnp:rootdevice"; got != want {
		t.Errorf("USN %q, want %q", got, want)
	}
}
