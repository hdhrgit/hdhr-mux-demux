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
// device so clients that discover via SSDP (rather than the HDHomeRun UDP
// protocol) find it.
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
	s.conn.Close()
}

var ssdpTargets = []string{"upnp:rootdevice", "urn:schemas-upnp-org:device:MediaServer:1"}

func (s *ssdpServer) location(remote net.IP) string {
	return s.d.baseURL(localFor(remote)) + "/device.xml"
}

func (s *ssdpServer) response(st string, remote net.IP) []byte {
	usn := "uuid:" + s.d.uuid()
	if st != "uuid:"+s.d.uuid() {
		usn += "::" + st
	}
	return []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nEXT:\r\nLOCATION: %s\r\nSERVER: Linux/1.0 UPnP/1.0 hdhr-mux/1.0\r\nST: %s\r\nUSN: %s\r\n\r\n",
		s.location(remote), st, usn))
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
		st := req.Header.Get("St")
		var replies []string
		switch {
		case st == "ssdp:all":
			replies = append(replies, ssdpTargets...)
		case st == "uuid:"+s.d.uuid():
			replies = append(replies, st)
		default:
			for _, t := range ssdpTargets {
				if strings.EqualFold(st, t) {
					replies = append(replies, t)
				}
			}
		}
		for _, r := range replies {
			s.conn.WriteToUDP(s.response(r, from.IP), from)
		}
	}
}

func (s *ssdpServer) announce() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		out, err := net.DialUDP("udp4", nil, ssdpGroup)
		if err == nil {
			for _, nt := range ssdpTargets {
				fmt.Fprintf(out, "NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nCACHE-CONTROL: max-age=1800\r\nLOCATION: %s\r\nNT: %s\r\nNTS: ssdp:alive\r\nSERVER: Linux/1.0 UPnP/1.0 hdhr-mux/1.0\r\nUSN: uuid:%s::%s\r\n\r\n",
					s.d.baseURL(localFor(ssdpGroup.IP))+"/device.xml", nt, s.d.uuid(), nt)
			}
			out.Close()
		}
		select {
		case <-s.quit:
			return
		case <-t.C:
		}
	}
}
