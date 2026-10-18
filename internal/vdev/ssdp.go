package vdev

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ssdpGroup4 = &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900}
	ssdpGroup6 = &net.UDPAddr{IP: net.ParseIP("ff02::c"), Port: 1900}
)

// Real HDHomeRun devices announce every 10 minutes, once over IPv4 and once
// over IPv6, each notification sent twice about 22 ms apart.
const (
	ssdpInterval  = 10 * time.Minute
	ssdpRepeats   = 2
	ssdpPacketGap = 22 * time.Millisecond
	ssdpCacheAge  = "max-age=1800"
	ssdpReadBuf   = 2048
)

// ssdpServer answers UPnP M-SEARCH queries and periodically announces the
// device as a DLNA media server, the way real HDHomeRun devices do, so TVs and
// other UPnP clients list it. Each IP family is independent.
type ssdpServer struct {
	d    *Device
	quit chan struct{}
	fams []*ssdpFamily
}

// ssdpFamily is SSDP over IPv4 or IPv6.
type ssdpFamily struct {
	s     *ssdpServer
	v6    bool
	conn  *net.UDPConn // multicast listener
	group *net.UDPAddr // destination of announcements
	iface *net.Interface
	self  net.IP // the address placed in Location
}

func newSSDP(d *Device) *ssdpServer { return &ssdpServer{d: d, quit: make(chan struct{})} }

func (s *ssdpServer) start() error {
	if !s.d.cfg.NoSSDP4 {
		if err := s.startFamily(false); err != nil {
			log.Printf("ssdp: ipv4 disabled: %v", err)
		}
	}
	if !s.d.cfg.NoSSDP6 {
		if err := s.startFamily(true); err != nil {
			log.Printf("ssdp: ipv6 disabled: %v", err)
		}
	}
	if len(s.fams) == 0 {
		return fmt.Errorf("no usable IP family")
	}
	go s.announce()
	return nil
}

// startFamily joins the multicast group on the interface that carries the
// default IPv4 route (the one clients reach us on) and starts answering.
func (s *ssdpServer) startFamily(v6 bool) error {
	iface, err := defaultInterface()
	if err != nil {
		return err
	}
	f := &ssdpFamily{s: s, v6: v6, iface: iface, group: ssdpGroup4}
	if v6 {
		f.group = &net.UDPAddr{IP: ssdpGroup6.IP, Port: ssdpGroup6.Port, Zone: iface.Name}
		if f.self = linkLocal6(iface); f.self == nil {
			return fmt.Errorf("%s has no link-local IPv6 address", iface.Name)
		}
		f.conn, err = net.ListenMulticastUDP("udp6", iface, ssdpGroup6)
	} else {
		f.conn, err = net.ListenMulticastUDP("udp4", nil, ssdpGroup4)
	}
	if err != nil {
		return err
	}
	s.fams = append(s.fams, f)
	go f.serve()
	return nil
}

// defaultInterface is the interface the default IPv4 route leaves by.
func defaultInterface() (*net.Interface, error) {
	local := net.ParseIP(localFor(ssdpGroup4.IP))
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range ifs {
		addrs, _ := ifs[i].Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(local) {
				return &ifs[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no interface owns %v", local)
}

func linkLocal6(iface *net.Interface) net.IP {
	addrs, _ := iface.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() == nil && ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP
		}
	}
	return nil
}

func (s *ssdpServer) stop() {
	close(s.quit)
	s.byebye()
	for _, f := range s.fams {
		f.conn.Close()
	}
}

// ssdpTarget is one notification/search target: a device, a service or a root
// device announcement, with the description document it points to.
type ssdpTarget struct {
	nt   string // NT / ST
	usn  string
	path string // the description, relative to the host
}

// ssdpTargets are the targets of the media server (root device, UDN, device
// type, two services) and, unless switched off, of the ATSC 3.0 primary device
// real HDHomeRun 4K devices announce next to it.
func (d *Device) ssdpTargets() []ssdpTarget {
	dms, atsc := d.udn(), d.atscUDN()
	out := []ssdpTarget{
		{"upnp:rootdevice", dms + "::upnp:rootdevice", "/dms/device.xml"},
		{dms, dms, "/dms/device.xml"},
		{dmsDeviceType, dms + "::" + dmsDeviceType, "/dms/device.xml"},
		{cmType, dms + "::" + cmType, "/dms/device.xml"},
		{cdType, dms + "::" + cdType, "/dms/device.xml"},
	}
	if !d.cfg.NoATSC3SSDP {
		out = append(out,
			ssdpTarget{"upnp:rootdevice", atsc + "::upnp:rootdevice", "/atsc3/device.xml"},
			ssdpTarget{atsc, atsc, "/atsc3/device.xml"},
			ssdpTarget{atscDeviceType, atsc + "::" + atscDeviceType, "/atsc3/device.xml"})
	}
	return out
}

// ssdpMatch returns the targets a search for st must be answered with.
func (d *Device) ssdpMatch(st string) []ssdpTarget {
	if st == "ssdp:all" {
		return d.ssdpTargets()
	}
	var out []ssdpTarget
	for _, t := range d.ssdpTargets() {
		if strings.EqualFold(st, t.nt) {
			out = append(out, t)
		}
	}
	return out
}

// location is the description URL given to a client at remote.
func (f *ssdpFamily) location(path string, remote net.IP) string {
	if f.v6 {
		return "http://" + net.JoinHostPort(f.self.String(), strconv.Itoa(f.s.d.HTTPPort())) + path
	}
	return f.s.d.baseURL(localFor(remote)) + path
}

func (f *ssdpFamily) response(t ssdpTarget, remote net.IP) []byte {
	return []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nServer: %s\r\nST: %s\r\nLocation: %s\r\nCache-Control: %s\r\nUSN: %s\r\nExt:\r\nDate: %s\r\nContent-Length: 0\r\n\r\n",
		f.s.d.dmsServer(), t.nt, f.location(t.path, remote), ssdpCacheAge, t.usn, time.Now().UTC().Format(http.TimeFormat)))
}

func (f *ssdpFamily) serve() {
	buf := make([]byte, ssdpReadBuf)
	for {
		n, from, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(buf[:n])))
		if err != nil || req.Method != "M-SEARCH" {
			continue
		}
		for _, t := range f.s.d.ssdpMatch(req.Header.Get("St")) {
			f.conn.WriteToUDP(f.response(t, from.IP), from)
		}
	}
}

// hostHeader is the Host header of announcements.
func (f *ssdpFamily) hostHeader() string {
	if f.v6 {
		return "[ff02::c]:1900"
	}
	return "239.255.255.250:1900"
}

// notifyPackets builds the announcements of one burst: every target,
// ssdpRepeats times over.
func (f *ssdpFamily) notifyPackets(nts string) [][]byte {
	d := f.s.d
	var out [][]byte
	for r := 0; r < ssdpRepeats; r++ {
		for _, t := range d.ssdpTargets() {
			out = append(out, []byte(fmt.Sprintf("NOTIFY * HTTP/1.1\r\nHost: %s\r\nCache-Control: %s\r\nLocation: %s\r\nNT: %s\r\nNTS: %s\r\nServer: %s\r\nUSN: %s\r\n\r\n",
				f.hostHeader(), ssdpCacheAge, f.location(t.path, f.group.IP), t.nt, nts, d.dmsServer(), t.usn)))
		}
	}
	return out
}

func (f *ssdpFamily) notify(nts string) {
	var local *net.UDPAddr
	network := "udp4"
	if f.v6 {
		network = "udp6"
		local = &net.UDPAddr{IP: f.self, Zone: f.iface.Name}
	}
	out, err := net.DialUDP(network, local, f.group)
	if err != nil {
		log.Printf("ssdp: announce: %v", err)
		return
	}
	defer out.Close()
	for i, p := range f.notifyPackets(nts) {
		if i > 0 {
			time.Sleep(ssdpPacketGap)
		}
		out.Write(p)
	}
}

func (s *ssdpServer) notify(nts string) {
	var wg sync.WaitGroup
	for _, f := range s.fams {
		wg.Add(1)
		go func() { defer wg.Done(); f.notify(nts) }()
	}
	wg.Wait()
}

func (s *ssdpServer) byebye() { s.notify("ssdp:byebye") }

func (s *ssdpServer) announce() {
	t := time.NewTicker(ssdpInterval)
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
