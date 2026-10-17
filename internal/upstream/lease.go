package upstream

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errInUse      = errors.New("tuner in use")
	errTuneFailed = errors.New("tune failed")
)

// Source delivers transport stream bytes from a device.
type Source interface {
	// Read returns the next bytes of the stream (a slice of buf).
	Read(buf []byte, deadline time.Time) ([]byte, error)
	// Lost reports packets known to be lost in transit (RTP only).
	Lost() uint64
	Close()
}

// Lease is exclusive use of one physical tuner.
type Lease struct {
	pool *Pool
	dev  *Device
	idx  int
	once sync.Once

	src  Source
	scan bool // held by a channel scan
	// an unlocked session holds no lock key; others may take the tuner over
	unlocked bool
	freq     uint64 // what we tuned, to notice a takeover
	target   string
	// control lock, used by rtp mode and scanning (HTTP streams are locked
	// by the device itself for as long as the connection lives)
	key atomic.Uint32

	// mu serialises SetFilter with Release, so a late filter change cannot
	// re-lock a tuner that has just been given back.
	mu       sync.Mutex
	released bool
	maskFail bool // taking the lock for masking failed once; do not try again
}

func (l *Lease) Device() *Device { return l.dev }
func (l *Lease) Index() int      { return l.idx }
func (l *Lease) ID() string      { return tkey{l.dev.ID, l.idx}.String() }

// Global is the tuner's number in the stable global numbering.
func (l *Lease) Global() int { return l.pool.GlobalOf(l.dev.ID, l.idx) }

func (l *Lease) name(v string) string { return fmt.Sprintf("/tuner%d/%s", l.idx, v) }

// Get reads a control variable of the tuner.
func (l *Lease) Get(v string) (string, error) { return l.dev.ctl.Get(l.name(v)) }

// Set writes a control variable, presenting the lock key if we hold one.
func (l *Lease) Set(v, val string) error {
	_, err := l.dev.ctl.Set(l.name(v), val, l.key.Load())
	return err
}

// Read implements Source on the lease's stream.
func (l *Lease) Read(buf []byte, deadline time.Time) ([]byte, error) {
	if l.src == nil {
		return nil, errors.New("lease has no stream")
	}
	return l.src.Read(buf, deadline)
}

// Lost reports transit losses of the underlying stream.
func (l *Lease) Lost() uint64 {
	if l.src == nil {
		return 0
	}
	return l.src.Lost()
}

// Status is a parsed /tunerN/status.
type Status struct {
	Channel string
	Lock    string
	SS      int // signal strength %
	SNQ     int // signal-to-noise quality %
	SEQ     int // symbol error quality %
	BPS     int
	PPS     int
}

func (s Status) Locked() bool { return s.Lock != "" && s.Lock != "none" }

func ParseStatus(v string) Status {
	var st Status
	for _, f := range strings.Fields(v) {
		k, val, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(val)
		switch k {
		case "ch":
			st.Channel = val
		case "lock":
			st.Lock = val
		case "ss":
			st.SS = n
		case "snq":
			st.SNQ = n
		case "seq":
			st.SEQ = n
		case "bps":
			st.BPS = n
		case "pps":
			st.PPS = n
		}
	}
	return st
}

func (l *Lease) Status() (Status, error) {
	v, err := l.Get("status")
	if err != nil {
		return Status{}, err
	}
	return ParseStatus(v), nil
}

// StatusOf reads a tuner's status without owning it.
func (p *Pool) StatusOf(deviceID uint32, idx int) (Status, error) {
	p.mu.Lock()
	d := p.devices[deviceID]
	p.mu.Unlock()
	if d == nil {
		return Status{}, errors.New("device offline")
	}
	v, err := d.ctl.Get(fmt.Sprintf("/tuner%d/status", idx))
	if err != nil {
		return Status{}, err
	}
	return ParseStatus(v), nil
}

// ---- starting a stream -------------------------------------------------

func (p *Pool) start(ctx context.Context, dev *Device, idx int, req Request) (*Lease, error) {
	l := &Lease{pool: p, dev: dev, idx: idx}
	var err error
	if p.cfg.Mode == "http" {
		err = l.startHTTP(ctx, req)
	} else {
		err = l.startPush(ctx, req)
	}
	if err != nil {
		l.once.Do(func() {}) // never release a lease that did not start
		return nil, err
	}
	if req.Unlocked {
		l.unlock(req.Frequency)
	}
	if req.Service {
		p.setCap(tkey{dev.ID, idx}, capYes)
	}
	return l, nil
}

func (l *Lease) startHTTP(ctx context.Context, req Request) error {
	path := fmt.Sprintf("/tuner%d/ch%d", l.idx, req.Frequency)
	if req.Program != 0 {
		path += fmt.Sprintf("-%d", req.Program)
	}
	src, err := openHTTP(ctx, l.dev.StreamAddr(), path, l.pool.cfg.TuneTimeout)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			switch {
			case he.Code == 404 && req.Program == 0:
				return ErrNeedsProgram
			case he.HDHRCode == 804 || he.HDHRCode == 805:
				return errInUse
			case he.HDHRCode == 806:
				return errTuneFailed
			case he.HDHRCode == 807:
				return fmt.Errorf("%w: %v", ErrNoSignal, he)
			}
		}
		return err
	}
	l.src = src
	return nil
}

// ---- HTTP source -------------------------------------------------------

// HTTPError is a non-200 answer from a device's streaming port.
type HTTPError struct {
	Code     int
	HDHRCode int    // numeric part of X-HDHomeRun-Error, 0 if absent
	HDHR     string // full X-HDHomeRun-Error text
}

func (e *HTTPError) Error() string {
	if e.HDHR != "" {
		return fmt.Sprintf("http %d (%s)", e.Code, e.HDHR)
	}
	return fmt.Sprintf("http %d", e.Code)
}

type httpSource struct {
	conn net.Conn
	br   *bufio.Reader
}

func openHTTP(ctx context.Context, addr, path string, timeout time.Duration) (*httpSource, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: hdhr-mux\r\nConnection: close\r\n\r\n", path, addr); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReaderSize(conn, 64*1024)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	f := strings.Fields(status)
	if len(f) < 2 {
		conn.Close()
		return nil, fmt.Errorf("bad http status line %q", strings.TrimSpace(status))
	}
	code, _ := strconv.Atoi(f[1])
	he := &HTTPError{Code: code}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "X-HDHomeRun-Error") {
			he.HDHR = strings.TrimSpace(v)
			he.HDHRCode, _ = strconv.Atoi(strings.Fields(he.HDHR + " 0")[0])
		}
	}
	if code != 200 {
		conn.Close()
		return nil, he
	}
	conn.SetDeadline(time.Time{})
	return &httpSource{conn: conn, br: br}, nil
}

// readCoalesce is how long the reader lets data pile up in the socket before
// each read. A device sends in segments of about a kilobyte; waking up for every
// one of them cost more CPU than everything else in the data path.
const readCoalesce = 4 * time.Millisecond

func (s *httpSource) Read(buf []byte, deadline time.Time) ([]byte, error) {
	time.Sleep(readCoalesce)
	s.conn.SetReadDeadline(deadline)
	n, err := s.br.Read(buf)
	if n > 0 {
		return buf[:n], nil
	}
	return nil, err
}

func (s *httpSource) Lost() uint64 { return 0 }
func (s *httpSource) Close()       { s.conn.Close() }

// ---- push (rtp/udp) mode and control locks ------------------------------

// startPush locks the tuner over the control protocol, tunes it and has the
// device push the stream to a local UDP port.
func (l *Lease) startPush(ctx context.Context, req Request) error {
	if err := l.lockControl(); err != nil {
		return errInUse
	}
	fail := func(err error) error { l.releaseControl(); return err }
	channel := fmt.Sprintf("auto:%d", req.Frequency)
	if req.Program == 0 {
		if err := l.Set("filter", "0x0000-0x1FFF"); err != nil {
			return fail(err)
		}
		if err := l.Set("program", "0"); err != nil {
			return fail(err)
		}
	}
	if err := l.Set("channel", channel); err != nil {
		return fail(err)
	}
	if req.Program != 0 {
		if err := l.Set("program", strconv.Itoa(int(req.Program))); err != nil {
			return fail(err)
		}
	}
	rx, err := l.pool.newReceiver(l.dev)
	if err != nil {
		return fail(err)
	}
	l.src = rx
	if err := l.Set("target", rx.Target()); err != nil {
		rx.Close()
		l.src = nil
		return fail(err)
	}
	// A push-mode tuner reports its lock; a missing signal looks like silence.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s, err := l.Status(); err == nil && s.Locked() {
			return nil
		}
		if time.Now().After(deadline) {
			l.Set("target", "none")
			rx.Close()
			l.src = nil
			return fail(errTuneFailed)
		}
		select {
		case <-ctx.Done():
			rx.Close()
			l.src = nil
			return fail(ctx.Err())
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// checkFree reports an error unless the tuner has no lock and no stream.
func (l *Lease) checkFree() error {
	if v, err := l.dev.ctl.Get(l.name("lockkey")); err != nil {
		return err
	} else if v != "none" {
		return fmt.Errorf("locked by %s", v)
	}
	if v, err := l.dev.ctl.Get(l.name("target")); err != nil {
		return err
	} else if v != "none" && v != "" {
		return fmt.Errorf("in use (target %s)", v)
	}
	return nil
}

// unlock drops the lock the device took for our stream (or we took for it)
// but leaves the stream running. From then on anybody may take the tuner over;
// Claimed tells when that has happened.
func (l *Lease) unlock(freq uint64) {
	l.dev.ctl.Set(l.name("lockkey"), "force", 0)
	l.key.Store(0)
	l.unlocked, l.freq = true, freq
	l.target, _ = l.Get("target")
}

// releaseUnlocked clears the tuner, but only if it is still ours: after a
// takeover whatever the new user set up must be left alone. A stream we pull
// over HTTP frees the tuner when our connection closes, so there is nothing
// to clear.
func (l *Lease) releaseUnlocked() {
	if _, pulled := l.src.(*httpSource); pulled || l.Claimed() {
		return
	}
	l.dev.ctl.Set(l.name("target"), "none", 0)
	l.dev.ctl.Set(l.name("channel"), "none", 0)
}

// Unlocked reports whether this is a lock-less session.
func (l *Lease) Unlocked() bool { return l.unlocked }

// Claimed reports whether somebody else has taken an unlocked tuner over:
// it is no longer tuned to our channel or no longer streaming to us.
func (l *Lease) Claimed() bool {
	if !l.unlocked {
		return false
	}
	tg, err := l.Get("target")
	if err != nil {
		return false // cannot tell; the stall watchdog handles a dead device
	}
	if tg != l.target {
		return true
	}
	s, err := l.Status()
	return err == nil && FrequencyOf(s.Channel) != l.freq
}

func (l *Lease) lockControl() error {
	name := l.name("lockkey")
	if err := l.checkFree(); err != nil {
		return err
	}
	var kb [4]byte
	rand.Read(kb[:])
	key := binary.BigEndian.Uint32(kb[:]) | 1
	if _, err := l.dev.ctl.Set(name, strconv.FormatUint(uint64(key), 10), 0); err != nil {
		return err
	}
	l.key.Store(key)
	return nil
}

// releaseControl clears and unlocks the tuner. If the lock was force-released
// behind our back the stale key would be rejected, so it is dropped first.
func (l *Lease) releaseControl() {
	if l.key.Load() == 0 {
		return
	}
	key := l.key.Load()
	if v, err := l.Get("lockkey"); err == nil && v == "none" {
		key = 0
	}
	set := func(name, val string) { l.dev.ctl.Set(l.name(name), val, key) }
	set("target", "none")
	set("channel", "none")
	set("filter", "0x0000-0x1FFF")
	set("program", "0")
	set("lockkey", "none")
	l.key.Store(0)
}

// Reclaim re-takes the control lock if it was released behind our back (for
// example by "lockkey force"). It reports whether the lock had been lost.
func (l *Lease) Reclaim() bool {
	if l.key.Load() == 0 {
		return false
	}
	v, err := l.Get("lockkey")
	if err != nil || v != "none" {
		return false
	}
	_, err = l.dev.ctl.Set(l.name("lockkey"), strconv.FormatUint(uint64(l.key.Load()), 10), 0)
	return err == nil
}

// AcquireControl locks a free tuner of the given device (preferring ones that
// can do ATSC 3.0) for tuning without streaming, as the scanner does.
func (p *Pool) AcquireControl(ctx context.Context, deviceID uint32) (*Lease, error) {
	p.mu.Lock()
	d := p.devices[deviceID]
	p.mu.Unlock()
	if d == nil {
		return nil, errors.New("device offline")
	}
	req := Request{Service: strings.Contains(d.Model, "atsc3"), Tuner: -1}
	for _, k := range p.rank(req) {
		if k.dev != deviceID {
			continue
		}
		if _, ok := p.markBusy(k); !ok {
			continue
		}
		l := &Lease{pool: p, dev: d, idx: k.idx, scan: true}
		if err := l.lockControl(); err != nil {
			p.unbusy(k, time.Second)
			continue
		}
		p.mu.Lock()
		p.scan[k] = true
		p.mu.Unlock()
		return l, nil
	}
	return nil, ErrNoTuner
}

// ErrMaskUnavailable means the stream cannot be masked (an unlocked session,
// or the lock could not be taken); it delivers the whole multiplex.
var ErrMaskUnavailable = errors.New("stream cannot be masked")

// SetFilter limits the PIDs the device sends on our stream to the given
// filter value ("0x0000 0x0100-0x0102"). A stream we pull over HTTP is held by
// the device's own lock, which only lets the client who took it change the
// tuner; so the first call trades that lock for one with a key of our own (the
// stream keeps flowing). From then on the tuner stays locked until Release
// puts the filter back and lets go. Unlocked sessions are never masked: whoever
// takes the tuner over would inherit our filter.
func (l *Lease) SetFilter(filter string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.unlocked || l.maskFail {
		return ErrMaskUnavailable
	}
	if l.key.Load() == 0 {
		var kb [4]byte
		rand.Read(kb[:])
		key := binary.BigEndian.Uint32(kb[:]) | 1
		// Our own stream: drop the lock the device took for it and take it
		// again with a key.
		l.dev.ctl.Set(l.name("lockkey"), "force", 0)
		if _, err := l.dev.ctl.Set(l.name("lockkey"), strconv.FormatUint(uint64(key), 10), 0); err != nil {
			l.maskFail = true
			return fmt.Errorf("%w: %v", ErrMaskUnavailable, err)
		}
		l.key.Store(key)
	}
	return l.Set("filter", filter)
}

// Retune sets the channel on a control lease (scanner use).
func (l *Lease) Retune(channel string) error { return l.Set("channel", channel) }

// Release stops the stream and gives the tuner back. Safe to call more than
// once.
func (l *Lease) Release() {
	l.once.Do(func() {
		if l.src != nil {
			l.src.Close()
		}
		l.mu.Lock()
		l.released = true
		l.mu.Unlock()
		if l.unlocked {
			l.releaseUnlocked()
		} else {
			l.releaseControl()
		}
		if l.scan {
			l.pool.mu.Lock()
			delete(l.pool.scan, tkey{l.dev.ID, l.idx})
			l.pool.mu.Unlock()
		}
		l.pool.unbusy(tkey{l.dev.ID, l.idx}, 0)
	})
}

// Abandon gives the tuner back without talking to it (device unreachable)
// and keeps it out of rotation for a while.
func (l *Lease) Abandon(quarantine time.Duration) {
	l.once.Do(func() {
		if l.src != nil {
			l.src.Close()
		}
		l.pool.unbusy(tkey{l.dev.ID, l.idx}, quarantine)
	})
}

// Quarantine keeps the tuner out of rotation for d once it is released.
func (l *Lease) Quarantine(d time.Duration) {
	l.pool.mu.Lock()
	l.pool.avoid[tkey{l.dev.ID, l.idx}] = time.Now().Add(d)
	l.pool.mu.Unlock()
}

// ---- RTP/UDP receiver ----------------------------------------------------

type Receiver struct {
	conn    *net.UDPConn
	rtp     bool
	target  string
	lastSeq uint16
	haveSeq bool
	lost    uint64
}

func (p *Pool) newReceiver(dev *Device) (*Receiver, error) {
	local := net.ParseIP(p.cfg.LocalIP)
	if local == nil {
		c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: dev.IP, Port: dev.Port})
		if err != nil {
			return nil, err
		}
		local = c.LocalAddr().(*net.UDPAddr).IP
		c.Close()
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: local})
	if err != nil {
		return nil, err
	}
	conn.SetReadBuffer(8 << 20)
	scheme := "udp"
	if p.cfg.Mode == "rtp" {
		scheme = "rtp"
	}
	return &Receiver{
		conn: conn, rtp: p.cfg.Mode == "rtp",
		target: fmt.Sprintf("%s://%s", scheme, conn.LocalAddr().String()),
	}, nil
}

func (r *Receiver) Target() string { return r.target }
func (r *Receiver) Close()         { r.conn.Close() }
func (r *Receiver) Lost() uint64   { return r.lost }

func (r *Receiver) Read(buf []byte, deadline time.Time) ([]byte, error) {
	r.conn.SetReadDeadline(deadline)
	n, err := r.conn.Read(buf)
	if err != nil {
		return nil, err
	}
	b := buf[:n]
	if !r.rtp {
		return b, nil
	}
	if n < 12 || b[0]>>6 != 2 {
		return nil, nil
	}
	seq := binary.BigEndian.Uint16(b[2:4])
	if r.haveSeq {
		if gap := seq - r.lastSeq - 1; gap != 0 && gap < 0x8000 {
			r.lost += uint64(gap)
		}
	}
	r.lastSeq, r.haveSeq = seq, true
	off := 12 + int(b[0]&0x0F)*4
	if b[0]&0x10 != 0 && n >= off+4 {
		off += 4 + int(binary.BigEndian.Uint16(b[off+2:off+4]))*4
	}
	if off > n {
		return nil, nil
	}
	return b[off:], nil
}
