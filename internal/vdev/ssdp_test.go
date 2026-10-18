package vdev

import (
	"net"
	"strconv"
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

func TestSSDPAnnouncementsMatchRealDevices(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d := testDevice()
	d.httpLn = ln
	s := &ssdpServer{d: d}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	v4 := &ssdpFamily{s: s, group: ssdpGroup4}
	v6 := &ssdpFamily{s: s, v6: true, self: net.ParseIP("fe80::218:ddff:fe0a:31d0"), group: &net.UDPAddr{IP: ssdpGroup6.IP, Port: 1900, Zone: "eth0"}}

	for _, f := range []*ssdpFamily{v4, v6} {
		pk := f.notifyPackets("ssdp:alive")
		// every target twice, as real devices send them
		if want := 2 * len(d.ssdpTypes()); len(pk) != want {
			t.Fatalf("v6=%v: %d packets, want %d", f.v6, len(pk), want)
		}
		seen := map[string]int{}
		for _, p := range pk {
			txt := string(p)
			if !strings.HasPrefix(txt, "NOTIFY * HTTP/1.1\r\n") || !strings.Contains(txt, "NTS: ssdp:alive\r\n") ||
				!strings.Contains(txt, "Cache-Control: max-age=1800\r\n") || !strings.Contains(txt, "Server: hdhr-mux/1.0 UPnP/1.0\r\n") {
				t.Fatalf("v6=%v: bad packet %q", f.v6, txt)
			}
			for _, line := range strings.Split(txt, "\r\n") {
				if strings.HasPrefix(line, "NT: ") {
					seen[line[4:]]++
				}
			}
			if f.v6 {
				if !strings.Contains(txt, "Host: [ff02::c]:1900\r\n") || !strings.Contains(txt, "Location: http://[fe80::218:ddff:fe0a:31d0]:"+port+"/dms/device.xml\r\n") {
					t.Fatalf("ipv6 packet %q", txt)
				}
			} else if !strings.Contains(txt, "Host: 239.255.255.250:1900\r\n") {
				t.Fatalf("ipv4 packet %q", txt)
			}
		}
		for _, nt := range d.ssdpTypes() {
			if seen[nt] != 2 {
				t.Errorf("v6=%v: %s announced %d times, want 2", f.v6, nt, seen[nt])
			}
		}
	}
}
