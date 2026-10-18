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

func nts(ts []ssdpTarget) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.nt)
	}
	return out
}

func TestSSDPAnswersLikeARealMediaServer(t *testing.T) {
	d := testDevice()
	all := d.ssdpMatch("ssdp:all")
	// the media server (root, UDN, device, two services) and the ATSC 3.0 primary device (root, UDN, device)
	if len(all) != 8 || all[0].nt != "upnp:rootdevice" || all[1].nt != d.udn() {
		t.Fatalf("ssdp:all = %v", nts(all))
	}
	for _, st := range []string{"urn:schemas-upnp-org:device:MediaServer:1",
		"URN:SCHEMAS-UPNP-ORG:SERVICE:CONTENTDIRECTORY:1", "urn:schemas-upnp-org:service:ConnectionManager:1", d.udn(),
		atscDeviceType, d.atscUDN()} {
		if got := d.ssdpMatch(st); len(got) != 1 || !strings.EqualFold(got[0].nt, st) {
			t.Errorf("search for %q answered with %v", st, nts(got))
		}
	}
	// both devices answer a search for root devices, each with its own description
	root := d.ssdpMatch("upnp:rootdevice")
	if len(root) != 2 || root[0].path != "/dms/device.xml" || root[1].path != "/atsc3/device.xml" ||
		root[0].usn != d.udn()+"::upnp:rootdevice" || root[1].usn != d.atscUDN()+"::upnp:rootdevice" {
		t.Errorf("rootdevice search: %+v", root)
	}
	for _, st := range []string{"urn:schemas-upnp-org:device:MediaRenderer:1", "upnp:other", "uuid:someone-else"} {
		if got := d.ssdpMatch(st); len(got) != 0 {
			t.Errorf("search for %q must not be answered, got %v", st, nts(got))
		}
	}
	if d.udn() == d.atscUDN() {
		t.Error("the media server and the ATSC 3.0 device need distinct UDNs")
	}

	// leaving the ATSC 3.0 device out
	d.cfg.NoATSC3SSDP = true
	if got := d.ssdpMatch("ssdp:all"); len(got) != 5 {
		t.Errorf("without ATSC 3.0: %v", nts(got))
	}
	if got := d.ssdpMatch(atscDeviceType); len(got) != 0 {
		t.Errorf("ATSC 3.0 device answered although switched off: %v", got)
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
		// 8 targets, each twice: the 16 packets a real 4K device sends per family
		if len(pk) != 16 {
			t.Fatalf("v6=%v: %d packets, want 16", f.v6, len(pk))
		}
		seen := map[string]int{}
		for _, p := range pk {
			txt := string(p)
			if !strings.HasPrefix(txt, "NOTIFY * HTTP/1.1\r\n") || !strings.Contains(txt, "NTS: ssdp:alive\r\n") ||
				!strings.Contains(txt, "Cache-Control: max-age=1800\r\n") || !strings.Contains(txt, "Server: hdhr-mux/1.0 UPnP/1.0\r\n") {
				t.Fatalf("v6=%v: bad packet %q", f.v6, txt)
			}
			var usn, loc string
			for _, line := range strings.Split(txt, "\r\n") {
				switch {
				case strings.HasPrefix(line, "USN: "):
					usn = line[5:]
					seen[usn]++
				case strings.HasPrefix(line, "Location: "):
					loc = line[10:]
				}
			}
			// the ATSC 3.0 device points at its own description
			wantPath := "/dms/device.xml"
			if strings.HasPrefix(usn, d.atscUDN()) {
				wantPath = "/atsc3/device.xml"
			}
			if !strings.HasSuffix(loc, ":"+port+wantPath) {
				t.Fatalf("v6=%v: %s has Location %s, want ...:%s%s", f.v6, usn, loc, port, wantPath)
			}
			if f.v6 {
				if !strings.Contains(txt, "Host: [ff02::c]:1900\r\n") || !strings.Contains(loc, "http://[fe80::218:ddff:fe0a:31d0]:") {
					t.Fatalf("ipv6 packet %q", txt)
				}
			} else if !strings.Contains(txt, "Host: 239.255.255.250:1900\r\n") {
				t.Fatalf("ipv4 packet %q", txt)
			}
		}
		for _, tg := range d.ssdpTargets() {
			if seen[tg.usn] != 2 {
				t.Errorf("v6=%v: %s announced %d times, want 2", f.v6, tg.usn, seen[tg.usn])
			}
		}
	}
}
