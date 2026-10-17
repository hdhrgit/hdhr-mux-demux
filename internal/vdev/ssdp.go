package vdev

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

var ssdpGroup = &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900}

// ssdpServer answers UPnP M-SEARCH queries and periodically announces the
// device as a DLNA media server, the way real HDHomeRun devices do, so TVs and
// other UPnP clients list it.
type ssdpServer struct {
	d    *Device
	conn *net.UDPConn
	quit chan struct{}
}

func newSSDP(d *Device) *ssdpServer { return &ssdpServer{d: d, quit: make(chan struct{})} }

func (s *ssdpServer) start() error {
	c, err := net.ListenMulticastUDP("udp4", nil, ssdpGroup)
	if err != nil {
		return err
	}
	s.conn = c
	go s.serve()
	go s.announce()
	return nil
}

func (s *ssdpServer) stop() {
	close(s.quit)
	s.byebye()
	s.conn.Close()
}

// ssdpTypes are the notification/search targets of the media server: the root
// device, its UDN, the device type and its two services.
func (d *Device) ssdpTypes() []string {
	return []string{"upnp:rootdevice", d.udn(), dmsDeviceType, cmType, cdType}
}

func (d *Device) ssdpUSN(nt string) string {
	if nt == d.udn() {
		return nt
	}
	return d.udn() + "::" + nt
}

// ssdpMatch returns the targets a search for st must be answered with.
func (d *Device) ssdpMatch(st string) []string {
	if st == "ssdp:all" {
		return d.ssdpTypes()
	}
	for _, t := range d.ssdpTypes() {
		if strings.EqualFold(st, t) {
			return []string{t}
		}
	}
	return nil
}

func (s *ssdpServer) location(remote net.IP) string {
	return s.d.baseURL(localFor(remote)) + "/dms/device.xml"
}

func (s *ssdpServer) response(st string, remote net.IP) []byte {
	return []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nServer: %s\r\nST: %s\r\nLocation: %s\r\nCache-Control: max-age=1800\r\nUSN: %s\r\nExt:\r\nDate: %s\r\nContent-Length: 0\r\n\r\n",
		s.d.dmsServer(), st, s.location(remote), s.d.ssdpUSN(st), time.Now().UTC().Format(http.TimeFormat)))
}

func (s *ssdpServer) serve() {
	buf := make([]byte, 2048)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(buf[:n])))
		if err != nil || req.Method != "M-SEARCH" {
			continue
		}
		for _, t := range s.d.ssdpMatch(req.Header.Get("St")) {
			s.conn.WriteToUDP(s.response(t, from.IP), from)
		}
	}
}

func (s *ssdpServer) notify(nts string) {
	out, err := net.DialUDP("udp4", nil, ssdpGroup)
	if err != nil {
		return
	}
	defer out.Close()
	loc := s.d.baseURL(localFor(ssdpGroup.IP)) + "/dms/device.xml"
	for _, nt := range s.d.ssdpTypes() {
		fmt.Fprintf(out, "NOTIFY * HTTP/1.1\r\nHost: 239.255.255.250:1900\r\nCache-Control: max-age=1800\r\nLocation: %s\r\nNT: %s\r\nNTS: %s\r\nServer: %s\r\nUSN: %s\r\n\r\n",
			loc, nt, nts, s.d.dmsServer(), s.d.ssdpUSN(nt))
	}
}

func (s *ssdpServer) byebye() { s.notify("ssdp:byebye") }

func (s *ssdpServer) announce() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		s.notify("ssdp:alive")
		select {
		case <-s.quit:
			return
		case <-t.C:
		}
	}
}
