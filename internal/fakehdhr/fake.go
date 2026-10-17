// Package fakehdhr simulates a physical HDHomeRun tuner device: discovery,
// the TCP/UDP control protocol and UDP/RTP transport streams carrying
// synthetic programs. It exists so the multiplexer can be developed and
// tested without hardware.
package fakehdhr

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hdhrmux/internal/chanmap"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/ts"
)

type Program struct {
	Number   uint16
	VChannel string // e.g. "8.1"
	Name     string
}

type Channel struct {
	Frequency uint64
	TSID      uint16
	// ATSC3 channels behave like real ATSC 3.0 tuning: only tuners 0 and 1
	// lock, and the device streams just the service chosen with "program".
	ATSC3    bool
	Programs []Program
}

type Config struct {
	DeviceID   uint32
	Tuners     int
	Channels   []Channel
	Bitrate    int    // bits per second per active tuner (default 4 Mbps)
	ChannelMap string // default us-bcast
}

type Device struct {
	cfg  Config
	tcp  net.Listener
	udp  net.PacketConn
	port int
	http net.Listener
	srv  *http.Server

	mu       sync.Mutex
	tuners   []*tuner
	peak     int
	tuneLog  []string
	closed   chan struct{}
	locksOff atomic.Bool
}

type tuner struct {
	idx      int
	channel  string
	freq     uint64
	chmap    string
	filter   string
	program  string
	target   string
	lockkey  uint32
	lockIP   string
	vchannel string
	cancel   chan struct{}
	pkts     atomic.Uint64
}

// Start launches the device on 127.0.0.1 with the same (random) port for
// TCP and UDP control.
func Start(cfg Config) (*Device, error) {
	if cfg.Tuners == 0 {
		cfg.Tuners = 2
	}
	if cfg.Bitrate == 0 {
		cfg.Bitrate = 4_000_000
	}
	if cfg.ChannelMap == "" {
		cfg.ChannelMap = "us-bcast"
	}
	if cfg.DeviceID == 0 {
		cfg.DeviceID = hdhrproto.MakeDeviceID(0x10200000)
	}
	var d *Device
	for try := 0; try < 20; try++ {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		port := ln.Addr().(*net.TCPAddr).Port
		pc, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			ln.Close()
			continue
		}
		d = &Device{cfg: cfg, tcp: ln, udp: pc, port: port, closed: make(chan struct{})}
		break
	}
	if d == nil {
		return nil, fmt.Errorf("fakehdhr: no free port pair")
	}
	for i := 0; i < cfg.Tuners; i++ {
		d.tuners = append(d.tuners, &tuner{idx: i, channel: "none", filter: "0x0000-0x1FFF", program: "0", target: "none", chmap: cfg.ChannelMap})
	}
	if err := d.startHTTP(); err != nil {
		return nil, err
	}
	go hdhrproto.ServeTCP(d.tcp, d)
	go hdhrproto.ServeUDP(d.udp, d)
	return d, nil
}

func (d *Device) Addr() string { return fmt.Sprintf("127.0.0.1:%d", d.port) }
func (d *Device) ID() uint32   { return d.cfg.DeviceID }

func (d *Device) Close() {
	select {
	case <-d.closed:
		return
	default:
		close(d.closed)
	}
	d.tcp.Close()
	d.udp.Close()
	d.srv.Close()
	d.mu.Lock()
	for _, t := range d.tuners {
		d.stopStream(t)
	}
	d.mu.Unlock()
}

// Active returns the number of tuners currently streaming.
func (d *Device) Active() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, t := range d.tuners {
		if t.cancel != nil {
			n++
		}
	}
	return n
}

// Peak returns the highest number of simultaneously streaming tuners seen.
func (d *Device) Peak() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.peak
}

// TunedChannels lists the channel of every tuner ("none" when idle).
func (d *Device) TunedChannels() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, t := range d.tuners {
		out = append(out, t.channel)
	}
	return out
}

// TuneCount reports how many times any tuner has been tuned.
func (d *Device) TuneCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.tuneLog)
}

// KillStreams stops all UDP output without releasing tuners, simulating a
// signal/network failure.
func (d *Device) KillStreams() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range d.tuners {
		d.stopStream(t)
	}
}

// ---- hdhrproto.Handler -------------------------------------------------

func (d *Device) Discover(_ net.IP, devType, devID uint32) *hdhrproto.Packet {
	if devType != hdhrproto.DeviceTypeWildcard && devType != hdhrproto.DeviceTypeTuner {
		return nil
	}
	if devID != hdhrproto.DeviceIDWildcard && devID != d.cfg.DeviceID {
		return nil
	}
	r := &hdhrproto.Packet{Type: hdhrproto.TypeDiscoverRpy}
	r.AddU32(hdhrproto.TagDeviceType, hdhrproto.DeviceTypeTuner)
	r.AddU32(hdhrproto.TagDeviceID, d.cfg.DeviceID)
	r.Add(hdhrproto.TagTunerCount, []byte{byte(d.cfg.Tuners)})
	return r
}

func (d *Device) findChannel(freq uint64) *Channel {
	for i := range d.cfg.Channels {
		if d.cfg.Channels[i].Frequency == freq {
			return &d.cfg.Channels[i]
		}
	}
	return nil
}

func (d *Device) GetSet(remote net.IP, name string, value *string, lockkey *uint32) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	parts := strings.Split(strings.Trim(name, "/"), "/")
	switch parts[0] {
	case "sys":
		if len(parts) == 2 && value == nil {
			switch parts[1] {
			case "model":
				return "hdhomerun5_atsc3", nil
			case "hwmodel":
				return "FAKE-" + strconv.Itoa(d.cfg.Tuners), nil
			case "version":
				return "20240101", nil
			case "features":
				return "channelmap: us-bcast us-cable\nmodulation: 8vsb qam256 qam64\nauto-modulation: auto", nil
			}
		}
	}
	if strings.HasPrefix(parts[0], "tuner") && len(parts) == 2 {
		idx, err := strconv.Atoi(parts[0][5:])
		if err != nil || idx < 0 || idx >= len(d.tuners) {
			return "", fmt.Errorf("ERROR: unknown getset variable")
		}
		return d.tunerVar(d.tuners[idx], remote, parts[1], value, lockkey)
	}
	return "", fmt.Errorf("ERROR: unknown getset variable")
}

func (d *Device) tunerVar(t *tuner, remote net.IP, v string, value *string, lockkey *uint32) (string, error) {
	locked := t.lockkey != 0
	if value != nil && v != "lockkey" && locked && (lockkey == nil || *lockkey != t.lockkey) {
		return "", fmt.Errorf("ERROR: resource locked by %s", t.lockIP)
	}
	if value != nil && v != "lockkey" && !locked && lockkey != nil {
		return "", fmt.Errorf("ERROR: lock no longer held") // as seen on real hardware
	}
	switch v {
	case "lockkey":
		if value == nil {
			if !locked {
				return "none", nil
			}
			return t.lockIP, nil
		}
		switch *value {
		case "none":
			if locked && (lockkey == nil || *lockkey != t.lockkey) {
				return "", fmt.Errorf("ERROR: resource locked by %s", t.lockIP)
			}
			t.lockkey, t.lockIP = 0, ""
		case "force":
			// Real devices only drop the lock; tuning and streaming go on.
			t.lockkey, t.lockIP = 0, ""
		default:
			n, ok := hdhrproto.ParseLockkey(*value)
			if !ok || n == 0 {
				return "", fmt.Errorf("ERROR: invalid lockkey")
			}
			if locked && (lockkey == nil || *lockkey != t.lockkey) {
				return "", fmt.Errorf("ERROR: resource locked by %s", t.lockIP)
			}
			t.lockkey, t.lockIP = n, remote.String()
		}
		return *value, nil
	case "channel":
		if value == nil {
			return t.channel, nil
		}
		return *value, d.tune(t, *value)
	case "channelmap":
		if value != nil {
			t.chmap = *value
		}
		return t.chmap, nil
	case "filter":
		if value != nil {
			if _, err := ts.ParsePIDSet(*value); err != nil {
				return "", fmt.Errorf("ERROR: invalid set value")
			}
			t.filter = *value
		}
		return t.filter, nil
	case "program":
		if value != nil {
			t.program = *value
			if fc := d.findChannel(t.freq); fc != nil && fc.ATSC3 && t.target != "none" {
				d.stopStream(t)
				d.startStream(t)
			}
		}
		return t.program, nil
	case "vchannel":
		if value == nil {
			return t.vchannel, nil
		}
		for _, c := range d.cfg.Channels {
			for _, p := range c.Programs {
				if p.VChannel == *value {
					t.vchannel = *value
					t.program = strconv.Itoa(int(p.Number))
					return *value, d.tune(t, fmt.Sprintf("auto:%d", c.Frequency))
				}
			}
		}
		return "", fmt.Errorf("ERROR: invalid virtual channel")
	case "target":
		if value == nil {
			return t.target, nil
		}
		return *value, d.setTarget(t, *value)
	case "status":
		return d.status(t), nil
	case "streaminfo":
		return d.streaminfo(t), nil
	case "plpinfo":
		return "", nil
	}
	return "", fmt.Errorf("ERROR: unknown getset variable")
}

func (d *Device) tune(t *tuner, v string) error {
	d.stopStream(t)
	if v == "none" {
		t.channel, t.freq = "none", 0
		return nil
	}
	spec, ok, err := chanmap.Parse(v, t.chmap)
	if err != nil || !ok {
		return fmt.Errorf("ERROR: invalid channel")
	}
	t.channel, t.freq = v, spec.Frequency
	d.tuneLog = append(d.tuneLog, v)
	if t.target != "none" {
		d.startStream(t)
	}
	return nil
}

func (d *Device) setTarget(t *tuner, v string) error {
	d.stopStream(t)
	if v == "none" {
		t.target = "none"
		return nil
	}
	if !strings.HasPrefix(v, "udp://") && !strings.HasPrefix(v, "rtp://") {
		return fmt.Errorf("ERROR: invalid target")
	}
	if _, err := net.ResolveUDPAddr("udp4", v[6:]); err != nil {
		return fmt.Errorf("ERROR: invalid target")
	}
	t.target = v
	d.startStream(t)
	return nil
}

func (d *Device) status(t *tuner) string {
	fc := d.findChannel(t.freq)
	if t.channel == "none" || fc == nil || (fc.ATSC3 && t.idx > 1) {
		ch := t.channel
		return fmt.Sprintf("ch=%s lock=none ss=0 snq=0 seq=0 bps=0 pps=0", ch)
	}
	if fc.ATSC3 {
		bps, pps := 0, 0
		if t.cancel != nil {
			bps, pps = d.cfg.Bitrate, d.cfg.Bitrate/8/188
		}
		return fmt.Sprintf("ch=auto:%d lock=atsc3 ss=60 snq=70 seq=100 bps=%d pps=%d", t.freq, bps, pps)
	}
	bps, pps := 0, 0
	if t.cancel != nil {
		bps, pps = d.cfg.Bitrate, d.cfg.Bitrate/8/188
	}
	return fmt.Sprintf("ch=8vsb:%d lock=8vsb ss=87 snq=92 seq=100 bps=%d pps=%d", t.freq, bps, pps)
}

func (d *Device) streaminfo(t *tuner) string {
	c := d.findChannel(t.freq)
	if c == nil || t.channel == "none" {
		return ""
	}
	var sb strings.Builder
	for _, p := range c.Programs {
		fmt.Fprintf(&sb, "%d: %s %s\n", p.Number, p.VChannel, p.Name)
	}
	fmt.Fprintf(&sb, "tsid=0x%04X", c.TSID)
	return sb.String()
}

// ---- streaming ---------------------------------------------------------

func (d *Device) stopStream(t *tuner) {
	if t.cancel != nil {
		close(t.cancel)
		t.cancel = nil
	}
}

func (d *Device) startStream(t *tuner) {
	ch := d.findChannel(t.freq)
	if ch == nil || t.channel == "none" || t.target == "none" || t.cancel != nil {
		return
	}
	if ch.ATSC3 {
		// Only the selected service is delivered, as its own small transport
		// stream; without a selection (or without lock) nothing flows.
		if t.idx > 1 || t.program == "0" {
			return
		}
		var svc *Channel
		for _, p := range ch.Programs {
			if strconv.Itoa(int(p.Number)) == t.program {
				c := *ch
				c.Programs = []Program{p}
				svc = &c
			}
		}
		if svc == nil {
			return
		}
		ch = svc
	}
	rtp := strings.HasPrefix(t.target, "rtp://")
	addr, _ := net.ResolveUDPAddr("udp4", t.target[6:])
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return
	}
	filter, _ := ts.ParsePIDSet(t.filter)
	t.cancel = make(chan struct{})
	n := 0
	for _, o := range d.tuners {
		if o.cancel != nil {
			n++
		}
	}
	if n > d.peak {
		d.peak = n
	}
	go func(cancel chan struct{}) {
		d.stream(t, ch, conn, rtp, filter, cancel)
		conn.Close()
	}(t.cancel)
}

// ProgramPIDs returns the PIDs used by program index i: PMT, video, audio.
func ProgramPIDs(i int) (pmt, video, audio uint16) {
	return 0x0100 + uint16(i)*0x10, 0x0101 + uint16(i)*0x10, 0x0102 + uint16(i)*0x10
}

func (d *Device) stream(t *tuner, ch *Channel, conn io.Writer, rtp bool, filter ts.PIDSet, stop chan struct{}) {
	failed := false
	cc := map[uint16]byte{}
	next := func(pid uint16) byte { c := cc[pid]; cc[pid] = (c + 1) & 15; return c }

	patSec := patBuild(ch)
	var progs [][2]uint16
	var pmts [][]byte
	for i, p := range ch.Programs {
		_, v, a := ProgramPIDs(i)
		pmts = append(pmts, ts.BuildPMT(p.Number, 0, v, []ts.Stream{{Type: 0x02, PID: v}, {Type: 0x81, PID: a}}))
		progs = append(progs, [2]uint16{v, a})
	}

	var seq uint16
	var ssrc uint32 = 0x48445248
	counter := make([]uint32, len(ch.Programs)*2)
	perTick := d.cfg.Bitrate / 8 / 188 / 100 // packets per 10 ms
	if perTick < 7 {
		perTick = 7
	}
	tick := time.NewTicker(time.Duration(float64(time.Second) * float64(perTick) * 188 * 8 / float64(d.cfg.Bitrate)))
	defer tick.Stop()
	var batch []byte
	flush := func() {
		if len(batch) == 0 {
			return
		}
		out := batch
		if rtp {
			hdr := make([]byte, 12, 12+len(batch))
			hdr[0], hdr[1] = 0x80, 33
			hdr[2], hdr[3] = byte(seq>>8), byte(seq)
			ts90 := uint32(time.Now().UnixNano() / 11111)
			hdr[4], hdr[5], hdr[6], hdr[7] = byte(ts90>>24), byte(ts90>>16), byte(ts90>>8), byte(ts90)
			hdr[8], hdr[9], hdr[10], hdr[11] = byte(ssrc>>24), byte(ssrc>>16), byte(ssrc>>8), byte(ssrc)
			seq++
			out = append(hdr, batch...)
		}
		if _, err := conn.Write(out); err != nil {
			failed = true
		}
		batch = batch[:0]
	}
	emit := func(p []byte) {
		if filter.Has(ts.PID(p)) {
			batch = append(batch, p...)
			t.pkts.Add(1)
			if len(batch) >= 7*188 {
				flush()
			}
		}
	}
	n := 0
	rr := 0
	for {
		select {
		case <-stop:
			return
		case <-d.closed:
			return
		case <-tick.C:
		}
		if failed {
			return
		}
		for i := 0; i < perTick; i++ {
			switch {
			case n%40 == 0:
				emit(ts.PacketizeSection(ts.PIDPAT, next(ts.PIDPAT), patSec))
			case n%40 == 1:
				for k := range pmts {
					pid, _, _ := ProgramPIDs(k)
					emit(ts.PacketizeSection(pid, next(pid), pmts[k]))
				}
			default:
				k := rr % (len(progs) * 2)
				rr++
				pid := progs[k/2][k%2]
				p := make([]byte, 188)
				p[0], p[1], p[2], p[3] = 0x47, byte(pid>>8)&0x1F, byte(pid), 0x10|next(pid)
				if counter[k] == 0 {
					p[1] |= 0x40
				}
				// payload: program number, running counter, filler
				p[4] = byte(ch.Programs[k/2].Number >> 8)
				p[5] = byte(ch.Programs[k/2].Number)
				c := counter[k]
				counter[k]++
				p[6], p[7], p[8], p[9] = byte(c>>24), byte(c>>16), byte(c>>8), byte(c)
				for j := 10; j < 188; j++ {
					p[j] = byte(j)
				}
				emit(p)
			}
			n++
		}
		flush()
	}
}

func patBuild(ch *Channel) []byte {
	sec := []byte{0x00, 0xB0, 0, byte(ch.TSID >> 8), byte(ch.TSID), 0xC1, 0, 0}
	for i, p := range ch.Programs {
		pmt, _, _ := ProgramPIDs(i)
		sec = append(sec, byte(p.Number>>8), byte(p.Number), 0xE0|byte(pmt>>8), byte(pmt))
	}
	sec[2] = byte(len(sec) - 3 + 4)
	crc := ts.CRC32MPEG(sec)
	return append(sec, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}
