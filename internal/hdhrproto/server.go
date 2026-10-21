package hdhrproto

import (
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"time"
)

// Handler implements the device side of the control protocol.
type Handler interface {
	// Discover returns the reply for a discovery request, or nil to ignore it.
	Discover(remote net.IP, deviceType, deviceID uint32) *Packet
	// GetSet services one get (value == nil) or set request. The returned
	// string is the variable's value; a non-nil error is reported to the
	// client verbatim as an ERROR_MESSAGE tag.
	GetSet(remote net.IP, name string, value *string, lockkey *uint32) (string, error)
}

// DirectDiscoverer is implemented by a Handler that answers a discovery request
// differently when it arrived on a connection the client made to it on purpose (TCP):
// such a client knows where it is, so a device that keeps out of broadcast discovery
// still answers it.
type DirectDiscoverer interface {
	DiscoverDirect(remote net.IP, deviceType, deviceID uint32) *Packet
}

func (p *Packet) getSetArgs() (name string, value *string, lock *uint32, ok bool) {
	name, ok = p.GetString(TagGetSetName)
	if !ok {
		return
	}
	if v, has := p.GetString(TagGetSetValue); has {
		value = &v
	}
	if k, has := p.GetU32(TagGetSetLock); has {
		lock = &k
	}
	return
}

func dispatch(h Handler, remote net.IP, req *Packet, direct bool) *Packet {
	switch req.Type {
	case TypeDiscoverReq:
		dt, _ := req.GetU32(TagDeviceType)
		di, _ := req.GetU32(TagDeviceID)
		if _, ok := req.Get(TagDeviceType); !ok {
			dt = DeviceTypeWildcard
		}
		if _, ok := req.Get(TagDeviceID); !ok {
			di = DeviceIDWildcard
		}
		if dd, ok := h.(DirectDiscoverer); ok && direct {
			return dd.DiscoverDirect(remote, dt, di)
		}
		return h.Discover(remote, dt, di)
	case TypeGetSetReq:
		name, value, lock, ok := req.getSetArgs()
		if !ok {
			return nil
		}
		rep := &Packet{Type: TypeGetSetRpy}
		rep.AddString(TagGetSetName, name)
		val, err := h.GetSet(remote, name, value, lock)
		if err != nil {
			rep.AddString(TagErrorMessage, err.Error())
		} else {
			rep.AddString(TagGetSetValue, val)
		}
		return rep
	}
	return nil
}

// ServeUDP answers discovery and get/set requests received on pc until it is
// closed. Where the system says which address a request was sent to (Linux), a
// request addressed to one of this machine's own addresses counts as made on purpose,
// like one over TCP, and a broadcast does not.
func ServeUDP(pc net.PacketConn, h Handler) {
	if uc, ok := pc.(*net.UDPConn); ok && enablePktinfo(uc) {
		serveUDPAddressed(uc, h)
		return
	}
	buf := make([]byte, 2048)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		req, err := Decode(buf[:n])
		if err != nil {
			continue
		}
		var ip net.IP
		if ua, ok := addr.(*net.UDPAddr); ok {
			ip = ua.IP
		}
		if rep := dispatch(h, ip, req, false); rep != nil {
			pc.WriteTo(rep.Encode(), addr)
		}
	}
}

// serveUDPAddressed is ServeUDP for a socket that reports each datagram's destination.
func serveUDPAddressed(uc *net.UDPConn, h Handler) {
	buf := make([]byte, 2048)
	oob := make([]byte, 128)
	for {
		n, oobn, _, addr, err := uc.ReadMsgUDP(buf, oob)
		if err != nil {
			return
		}
		req, err := Decode(buf[:n])
		if err != nil {
			continue
		}
		if rep := dispatch(h, addr.IP, req, isOwnAddress(destinationOf(oob[:oobn]))); rep != nil {
			uc.WriteTo(rep.Encode(), addr)
		}
	}
}

// isOwnAddress reports whether ip is one of this machine's own unicast addresses (so
// not a broadcast or multicast one).
func isOwnAddress(ip net.IP) bool {
	if ip == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// ServeTCP accepts control connections until ln is closed.
func ServeTCP(ln net.Listener, h Handler) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go serveConn(c, h)
	}
}

func serveConn(c net.Conn, h Handler) {
	defer c.Close()
	var ip net.IP
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		ip = ta.IP
	}
	hdr := make([]byte, 4)
	for {
		c.SetReadDeadline(time.Now().Add(5 * time.Minute))
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		n := FrameLen(hdr)
		if n > MaxFrame {
			return
		}
		frame := make([]byte, n)
		copy(frame, hdr)
		if _, err := io.ReadFull(c, frame[4:]); err != nil {
			return
		}
		req, err := Decode(frame)
		if err != nil {
			log.Printf("hdhrproto: bad frame from %v: %v", ip, err)
			return
		}
		if rep := dispatch(h, ip, req, true); rep != nil {
			c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := c.Write(rep.Encode()); err != nil {
				return
			}
		}
	}
}

// ---- client side -------------------------------------------------------

// ErrDevice is returned when the device replies with an ERROR_MESSAGE.
type ErrDevice string

func (e ErrDevice) Error() string { return string(e) }

// Control is a persistent TCP control connection to a device. Calls are
// serialised; the connection is re-established on failure.
type Control struct {
	Addr    string // host:port
	Timeout time.Duration

	mu   chan struct{}
	conn net.Conn
}

func NewControl(addr string) *Control {
	c := &Control{Addr: addr, Timeout: 3 * time.Second, mu: make(chan struct{}, 1)}
	return c
}

func (c *Control) Close() {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// Get reads a variable.
func (c *Control) Get(name string) (string, error) { return c.GetSet(name, nil, 0) }

// Set writes a variable; lockkey of 0 means "no lockkey tag".
func (c *Control) Set(name, value string, lockkey uint32) (string, error) {
	return c.GetSet(name, &value, lockkey)
}

func (c *Control) GetSet(name string, value *string, lockkey uint32) (string, error) {
	req := &Packet{Type: TypeGetSetReq}
	req.AddString(TagGetSetName, name)
	if value != nil {
		req.AddString(TagGetSetValue, *value)
	}
	if lockkey != 0 {
		req.AddU32(TagGetSetLock, lockkey)
	}
	wire := req.Encode()

	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if c.conn == nil {
			conn, err := net.DialTimeout("tcp", c.Addr, c.Timeout)
			if err != nil {
				return "", err
			}
			c.conn = conn
		}
		rep, err := c.roundTrip(wire)
		if err == nil {
			if msg, bad := rep.GetString(TagErrorMessage); bad {
				return "", ErrDevice(msg)
			}
			v, _ := rep.GetString(TagGetSetValue)
			return v, nil
		}
		lastErr = err
		c.conn.Close()
		c.conn = nil
	}
	return "", lastErr
}

func (c *Control) roundTrip(wire []byte) (*Packet, error) {
	c.conn.SetDeadline(time.Now().Add(c.Timeout))
	if _, err := c.conn.Write(wire); err != nil {
		return nil, err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		return nil, err
	}
	n := FrameLen(hdr)
	if n > MaxFrame {
		return nil, errors.New("hdhrproto: oversized reply")
	}
	frame := make([]byte, n)
	copy(frame, hdr)
	if _, err := io.ReadFull(c.conn, frame[4:]); err != nil {
		return nil, err
	}
	return Decode(frame)
}

// Info is the result of a discovery reply.
type Info struct {
	DeviceID   uint32
	DeviceType uint32
	TunerCount int
	BaseURL    string
	LineupURL  string
	IP         net.IP
	Port       int
}

func parseDiscoverReply(p *Packet, from net.Addr) (Info, bool) {
	if p.Type != TypeDiscoverRpy {
		return Info{}, false
	}
	var inf Info
	inf.DeviceType, _ = p.GetU32(TagDeviceType)
	inf.DeviceID, _ = p.GetU32(TagDeviceID)
	if v, ok := p.Get(TagTunerCount); ok && len(v) == 1 {
		inf.TunerCount = int(v[0])
	}
	inf.BaseURL, _ = p.GetString(TagBaseURL)
	inf.LineupURL, _ = p.GetString(TagLineupURL)
	if ua, ok := from.(*net.UDPAddr); ok {
		inf.IP, inf.Port = ua.IP, ua.Port
	}
	return inf, true
}

// Discover sends a wildcard discovery request to each target (host:port,
// broadcast addresses allowed) and collects replies for wait.
func Discover(targets []string, wait time.Duration) ([]Info, error) {
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	req := &Packet{Type: TypeDiscoverReq}
	req.AddU32(TagDeviceType, DeviceTypeTuner)
	req.AddU32(TagDeviceID, DeviceIDWildcard)
	wire := req.Encode()
	for _, t := range targets {
		a, err := net.ResolveUDPAddr("udp4", t)
		if err != nil {
			continue
		}
		pc.WriteTo(wire, a)
	}
	var out []Info
	seen := map[uint32]bool{}
	pc.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			break
		}
		p, err := Decode(buf[:n])
		if err != nil {
			continue
		}
		if inf, ok := parseDiscoverReply(p, from); ok && !seen[inf.DeviceID] {
			seen[inf.DeviceID] = true
			out = append(out, inf)
		}
	}
	return out, nil
}

// ParseLockkey converts a lockkey variable value to a number.
func ParseLockkey(s string) (uint32, bool) {
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil
}
