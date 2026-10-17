// Package upstream talks to the physical HDHomeRun devices: discovery,
// a stable global numbering of all their tuners, tuner allocation that
// understands ATSC 3.0 capability, and transport stream reception over HTTP
// (default) or UDP/RTP.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"hdhrmux/internal/hdhrproto"
)

// Config selects which physical devices the pool manages.
type Config struct {
	// Discover enables broadcast discovery on the local network.
	Discover bool
	// DiscoverTargets are UDP discovery destinations (default
	// 255.255.255.255:65001).
	DiscoverTargets []string
	// Devices are static device addresses ("host" or "host:port"); they are
	// probed with unicast discovery.
	Devices []string
	// AllowIDs restricts the pool to these device IDs (hex). Empty = all.
	AllowIDs []string
	// Tuners restricts the pool to the listed tuners: "DEVICEID" (all of a
	// device's tuners), "DEVICEID/N" or "DEVICEID/N-M". Empty = all. Only listed
	// tuners are used and numbered, so the virtual device has exactly those.
	Tuners []string
	// ExcludeTuners lists tuners the pool must never use, "DEVICEID/N".
	ExcludeTuners []string
	// IgnoreIDs are never used (the multiplexer's own device ID).
	IgnoreIDs []uint32
	// Mode is how transport streams are pulled from a device: "http"
	// (default; works through Docker NAT) or "rtp"/"udp" (the device pushes
	// to a local UDP port).
	Mode string
	// StreamPort is the device's HTTP streaming port (default 5004).
	StreamPort int
	// LocalIP overrides the address devices are told to stream to (rtp/udp).
	LocalIP string
	// OrderFile persists the device order so tuner numbers stay stable.
	OrderFile string
	// RefreshInterval between discovery passes (default 30s).
	RefreshInterval time.Duration
	// WaitFree is how long Open waits for a tuner to become free.
	WaitFree time.Duration
	// TuneTimeout bounds how long a device may take to start a stream
	// (a channel without signal takes about 10 s to be rejected).
	TuneTimeout time.Duration
}

var (
	ErrNoTuner = errors.New("no physical tuner available")
	// ErrTunerBusy: the pinned physical tuner is in use (typically by another
	// application sharing the device).
	ErrTunerBusy = errors.New("physical tuner in use")
	// ErrTunerUnsuitable: the pinned tuner cannot tune this channel (an
	// ATSC 1.0-only tuner asked for ATSC 3.0).
	ErrTunerUnsuitable = errors.New("physical tuner cannot tune this channel")
	// ErrNoSignal: the device could not deliver video on the channel.
	ErrNoSignal = errors.New("no signal")
	// ErrNeedsProgram: the channel (ATSC 3.0) cannot be streamed without
	// selecting a service.
	ErrNeedsProgram = errors.New("channel requires a program")
)

type Device struct {
	ID         uint32
	IP         net.IP
	Port       int // control port
	StreamPort int
	WebPort    int // HTTP API port (lineup.json), 80 on real devices
	TunerCount int
	Model      string // /sys/model, e.g. hdhomerun5_atsc3
	ctl        *hdhrproto.Control
}

func (d *Device) Addr() string { return net.JoinHostPort(d.IP.String(), strconv.Itoa(d.Port)) }
func (d *Device) StreamAddr() string {
	return net.JoinHostPort(d.IP.String(), strconv.Itoa(d.StreamPort))
}

type tkey struct {
	dev uint32
	idx int
}

func (k tkey) String() string { return fmt.Sprintf("%08X/%d", k.dev, k.idx) }

// capability of a tuner to lock ATSC 3.0.
type capability int8

const (
	capNo      capability = -1
	capUnknown capability = 0
	capYes     capability = 1
)

type orderEntry struct {
	ID     string `json:"id"`
	Tuners int    `json:"tuners"`
}

type Pool struct {
	cfg Config

	mu      sync.Mutex
	devices map[uint32]*Device
	order   []orderEntry
	busy    map[tkey]bool
	avoid   map[tkey]time.Time // temporarily skipped
	caps    map[tkey]capability
	scan    map[tkey]bool // tuners held by a channel scan
	changed chan struct{} // closed & replaced whenever availability may have changed
	allow   map[uint32]bool
	exclude map[tkey]bool
	use     map[tkey]bool   // explicitly listed tuners (Config.Tuners)
	useDev  map[uint32]bool // devices listed without a tuner number
	ignore  map[uint32]bool

	// The global numbering: the tuners the pool may use, in order. Rebuilt
	// whenever devices or the configuration change.
	number []tkey
	global map[tkey]int

	// Reclaim, if set, is asked to free a physical tuner held for no one
	// (a lingering stream) when all are taken. accept reports whether a given
	// tuner would satisfy the waiting request. It returns whether it freed one.
	Reclaim func(accept func(*Lease) bool) bool
}

func NewPool(cfg Config) (*Pool, error) {
	if cfg.Mode == "" {
		cfg.Mode = "http"
	}
	switch cfg.Mode {
	case "http", "rtp", "udp":
	default:
		return nil, fmt.Errorf("upstream mode must be http, rtp or udp, got %q", cfg.Mode)
	}
	if cfg.StreamPort == 0 {
		cfg.StreamPort = 5004
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 30 * time.Second
	}
	if cfg.TuneTimeout == 0 {
		cfg.TuneTimeout = 15 * time.Second
	}
	if len(cfg.DiscoverTargets) == 0 {
		cfg.DiscoverTargets = []string{"255.255.255.255:65001"}
	}
	p := &Pool{
		cfg: cfg, devices: map[uint32]*Device{}, busy: map[tkey]bool{}, avoid: map[tkey]time.Time{},
		caps: map[tkey]capability{}, scan: map[tkey]bool{}, changed: make(chan struct{}),
		allow: map[uint32]bool{}, exclude: map[tkey]bool{}, ignore: map[uint32]bool{},
		use: map[tkey]bool{}, useDev: map[uint32]bool{}, global: map[tkey]int{},
	}
	for _, s := range cfg.AllowIDs {
		id, err := hdhrproto.ParseDeviceID(strings.ToUpper(s))
		if err != nil {
			return nil, err
		}
		p.allow[id] = true
	}
	for _, s := range cfg.ExcludeTuners {
		dev, idx, ok := strings.Cut(s, "/")
		id, err := hdhrproto.ParseDeviceID(strings.ToUpper(dev))
		n, err2 := strconv.Atoi(idx)
		if !ok || err != nil || err2 != nil {
			return nil, fmt.Errorf("bad exclude_tuners entry %q (want DEVICEID/N)", s)
		}
		p.exclude[tkey{id, n}] = true
	}
	for _, s := range cfg.Tuners {
		dev, spec, hasIdx := strings.Cut(strings.TrimSpace(s), "/")
		id, err := hdhrproto.ParseDeviceID(strings.ToUpper(dev))
		if err != nil {
			return nil, fmt.Errorf("bad tuners entry %q: %v", s, err)
		}
		if !hasIdx {
			p.useDev[id] = true
			continue
		}
		lo, hi := 0, 0
		if a, b, isRange := strings.Cut(spec, "-"); isRange {
			lo, _ = strconv.Atoi(a)
			hi, err = strconv.Atoi(b)
		} else {
			lo, err = strconv.Atoi(spec)
			hi = lo
		}
		if err != nil || lo < 0 || hi < lo {
			return nil, fmt.Errorf("bad tuners entry %q (want DEVICEID, DEVICEID/N or DEVICEID/N-M)", s)
		}
		for n := lo; n <= hi; n++ {
			p.use[tkey{id, n}] = true
		}
	}
	for _, id := range cfg.IgnoreIDs {
		p.ignore[id] = true
	}
	if cfg.OrderFile != "" {
		if b, err := os.ReadFile(cfg.OrderFile); err == nil {
			if err := json.Unmarshal(b, &p.order); err != nil {
				return nil, fmt.Errorf("%s: %w", cfg.OrderFile, err)
			}
		}
	}
	p.rebuildLocked()
	return p, nil
}

// includedLocked reports whether the pool may use (and number) a tuner.
func (p *Pool) includedLocked(k tkey) bool {
	if p.exclude[k] {
		return false
	}
	if len(p.allow) > 0 && !p.allow[k.dev] {
		return false
	}
	if len(p.use) > 0 || len(p.useDev) > 0 {
		return p.use[k] || p.useDev[k.dev]
	}
	return true
}

// rebuildLocked recomputes the global numbering: every usable tuner of every
// known device in device order, so excluded tuners leave no holes.
func (p *Pool) rebuildLocked() {
	p.number = p.number[:0]
	p.global = map[tkey]int{}
	for _, o := range p.order {
		id, err := hdhrproto.ParseDeviceID(o.ID)
		if err != nil {
			continue
		}
		for i := 0; i < o.Tuners; i++ {
			if k := (tkey{id, i}); p.includedLocked(k) {
				p.global[k] = len(p.number)
				p.number = append(p.number, k)
			}
		}
	}
}

func (p *Pool) saveOrderLocked() {
	if p.cfg.OrderFile == "" {
		return
	}
	b, _ := json.MarshalIndent(p.order, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p.cfg.OrderFile), 0o755); err == nil {
		os.WriteFile(p.cfg.OrderFile, b, 0o644)
	}
}

// SetWebPort overrides the HTTP API port of one device.
func (p *Pool) SetWebPort(id uint32, port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := p.devices[id]; d != nil {
		d.WebPort = port
	}
}

// SetStreamPort overrides the HTTP streaming port of one device.
func (p *Pool) SetStreamPort(id uint32, port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := p.devices[id]; d != nil {
		d.StreamPort = port
	}
}

// Refresh runs one discovery pass and merges the results into the inventory.
func (p *Pool) Refresh() {
	var targets []string
	if p.cfg.Discover {
		targets = append(targets, p.cfg.DiscoverTargets...)
	}
	for _, d := range p.cfg.Devices {
		if _, _, err := net.SplitHostPort(d); err != nil {
			d = net.JoinHostPort(d, strconv.Itoa(hdhrproto.ControlPort))
		}
		targets = append(targets, d)
	}
	if len(targets) == 0 {
		return
	}
	infos, err := hdhrproto.Discover(targets, 800*time.Millisecond)
	if err != nil {
		log.Printf("upstream: discovery: %v", err)
		return
	}
	// Stable numbering for devices seen together for the first time: by ID.
	sort.Slice(infos, func(i, j int) bool { return infos[i].DeviceID < infos[j].DeviceID })
	for _, inf := range infos {
		if inf.DeviceType != hdhrproto.DeviceTypeTuner || inf.TunerCount == 0 || p.ignore[inf.DeviceID] {
			continue
		}
		if len(p.allow) > 0 && !p.allow[inf.DeviceID] {
			continue
		}
		p.mu.Lock()
		old, known := p.devices[inf.DeviceID]
		p.mu.Unlock()
		if known && old.IP.Equal(inf.IP) && old.Port == inf.Port {
			continue
		}
		d := &Device{ID: inf.DeviceID, IP: inf.IP, Port: inf.Port, StreamPort: p.cfg.StreamPort, WebPort: 80, TunerCount: inf.TunerCount}
		d.ctl = hdhrproto.NewControl(d.Addr())
		d.Model, _ = d.ctl.Get("/sys/model")
		if d.Model == "hdhomerun_mux" {
			// Another multiplexer is not a physical device: using it would loop.
			d.ctl.Close()
			p.mu.Lock()
			p.ignore[inf.DeviceID] = true
			p.mu.Unlock()
			log.Printf("upstream: ignoring %s at %s: it is a multiplexer", hdhrproto.FormatDeviceID(inf.DeviceID), d.Addr())
			continue
		}

		p.mu.Lock()
		if known {
			old.ctl.Close()
			d.StreamPort, d.WebPort = old.StreamPort, old.WebPort
		}
		p.devices[d.ID] = d
		p.registerLocked(d)
		p.notifyLocked()
		usable, first := 0, -1
		for i := 0; i < d.TunerCount; i++ {
			if g, ok := p.global[tkey{d.ID, i}]; ok {
				usable++
				if first < 0 {
					first = g
				}
			}
		}
		p.mu.Unlock()
		if usable == 0 {
			log.Printf("upstream: device %s (%s) at %s: none of its tuners is in use", hdhrproto.FormatDeviceID(d.ID), d.Model, d.Addr())
		} else {
			log.Printf("upstream: device %s (%s) at %s with %d tuner(s), %d usable as tuners %d-%d",
				hdhrproto.FormatDeviceID(d.ID), d.Model, d.Addr(), d.TunerCount, usable, first, first+usable-1)
		}
	}
}

// registerLocked records the device in the persistent order and seeds the
// ATSC 3.0 capability table from its model.
func (p *Pool) registerLocked(d *Device) {
	id := hdhrproto.FormatDeviceID(d.ID)
	found := false
	for i := range p.order {
		if p.order[i].ID == id {
			found = true
			if p.order[i].Tuners != d.TunerCount {
				p.order[i].Tuners = d.TunerCount
				p.saveOrderLocked()
			}
		}
	}
	if !found {
		p.order = append(p.order, orderEntry{id, d.TunerCount})
		p.saveOrderLocked()
	}
	p.rebuildLocked()
	for i := 0; i < d.TunerCount; i++ {
		k := tkey{d.ID, i}
		switch {
		case !strings.Contains(d.Model, "atsc3"):
			p.caps[k] = capNo
		case i < 2:
			// Observed on HDHR5-4K / HDFX-4K: tuners 0 and 1 lock ATSC 3.0,
			// the others are ATSC 1.0 only. Others stay "unknown" and are
			// learned from the device's answers.
			p.caps[k] = capYes
		default:
			p.caps[k] = capUnknown
		}
	}
}

// Run keeps the inventory fresh until ctx is cancelled.
func (p *Pool) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Refresh()
		}
	}
}

// TunerRef identifies one tuner in the global numbering.
type TunerRef struct {
	Global   int
	DeviceID uint32
	Index    int
	Device   *Device // nil while the device is offline
}

// Tuners lists every tuner the pool may use, including those of devices
// that are offline right now (remembered from earlier runs), in global order.
func (p *Pool) Tuners() []TunerRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]TunerRef, len(p.number))
	for g, k := range p.number {
		out[g] = TunerRef{Global: g, DeviceID: k.dev, Index: k.idx, Device: p.devices[k.dev]}
	}
	return out
}

// Resolve maps a global tuner number to its device and tuner index.
func (p *Pool) Resolve(global int) (TunerRef, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if global < 0 || global >= len(p.number) {
		return TunerRef{}, false
	}
	k := p.number[global]
	return TunerRef{Global: global, DeviceID: k.dev, Index: k.idx, Device: p.devices[k.dev]}, true
}

// TunerCount is the number of usable physical tuners currently online.
func (p *Pool) TunerCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, k := range p.number {
		if p.devices[k.dev] != nil {
			n++
		}
	}
	return n
}

// TotalTuners is the size of the global numbering (online or not).
func (p *Pool) TotalTuners() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.number)
}

// Devices returns a snapshot of the online devices that have tuners in use,
// in global order.
func (p *Pool) Devices() []*Device {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*Device
	seen := map[uint32]bool{}
	for _, k := range p.number {
		if d := p.devices[k.dev]; d != nil && !seen[k.dev] {
			seen[k.dev] = true
			out = append(out, d)
		}
	}
	return out
}

// GlobalOf returns the global number of a device's tuner, or -1 if the pool
// does not use it.
func (p *Pool) GlobalOf(id uint32, idx int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if g, ok := p.global[tkey{id, idx}]; ok {
		return g
	}
	return -1
}

func (p *Pool) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// Request describes what a stream needs from a physical tuner.
type Request struct {
	Frequency uint64
	// Program selects one service on the device itself (ATSC 3.0); 0 asks for
	// the whole multiplex.
	Program uint16
	// Service marks an ATSC 3.0 channel: only capable tuners qualify.
	Service bool
	// Tuner pins the stream to one physical tuner by its global number
	// (virtual tuner N is physical tuner N); -1 lets the pool choose.
	Tuner int
	// Skip lists tuners ("DEVICEID/N") not to use.
	Skip map[string]bool
}

// rank returns the free tuners that qualify for req, best first. ATSC 1.0
// requests go to tuners that cannot do ATSC 3.0 before touching the ones that
// can, so those stay free for ATSC 3.0; ATSC 3.0 requests go to proven-capable
// tuners before unproven ones. Ties go to the less loaded device.
func (p *Pool) rank(req Request) []tkey {
	p.mu.Lock()
	defer p.mu.Unlock()
	load := map[uint32]int{}
	for k := range p.busy {
		load[k.dev]++
	}
	type cand struct {
		k      tkey
		class  int
		load   int
		global int
	}
	var cs []cand
	now := time.Now()
	for g, k := range p.number {
		if p.devices[k.dev] == nil {
			continue
		}
		if req.Tuner >= 0 && g != req.Tuner {
			continue
		}
		if p.busy[k] || req.Skip[k.String()] || (req.Tuner < 0 && now.Before(p.avoid[k])) {
			continue
		}
		c := p.caps[k]
		cl := 0
		if req.Service {
			if c == capNo {
				continue
			}
			if c == capUnknown {
				cl = 1
			}
		} else {
			cl = int(c) + 1 // no=0, unknown=1, yes=2
		}
		cs = append(cs, cand{k, cl, load[k.dev], g})
	}
	sort.Slice(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.class != b.class {
			return a.class < b.class
		}
		if a.load != b.load {
			return a.load < b.load
		}
		return a.global < b.global
	})
	out := make([]tkey, len(cs))
	for i, c := range cs {
		out[i] = c.k
	}
	return out
}

// compatible reports whether freeing tuner k would let req proceed.
func (p *Pool) compatible(k tkey, req Request) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	g, ok := p.global[k]
	if !ok || req.Skip[k.String()] {
		return false
	}
	if req.Tuner >= 0 && g != req.Tuner {
		return false
	}
	return !(req.Service && p.caps[k] == capNo)
}

// pinnedProblem says up front whether the pinned tuner can ever serve req.
func (p *Pool) pinnedProblem(req Request) error {
	if req.Tuner < 0 {
		return nil
	}
	ref, ok := p.Resolve(req.Tuner)
	if !ok || ref.Device == nil {
		return ErrTunerBusy // offline or nonexistent: unusable right now
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := tkey{ref.DeviceID, ref.Index}
	if p.scan[k] {
		return ErrTunerBusy // a scan has it: do not wait for the scan to end
	}
	if req.Service && p.caps[k] == capNo {
		return ErrTunerUnsuitable
	}
	return nil
}

// HeldByScan reports whether a channel scan is using physical tuner global.
func (p *Pool) HeldByScan(global int) bool {
	ref, ok := p.Resolve(global)
	if !ok {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scan[tkey{ref.DeviceID, ref.Index}]
}

// ATSC3Capable reports whether the leased tuner is proven to lock ATSC 3.0.
func (l *Lease) ATSC3Capable() bool {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	return l.pool.caps[tkey{l.dev.ID, l.idx}] == capYes
}

func (p *Pool) setCap(k tkey, c capability) {
	p.mu.Lock()
	p.caps[k] = c
	p.mu.Unlock()
}

func (p *Pool) markBusy(k tkey) (*Device, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy[k] {
		return nil, false
	}
	p.busy[k] = true
	return p.devices[k.dev], true
}

func (p *Pool) unbusy(k tkey, avoidFor time.Duration) {
	p.mu.Lock()
	delete(p.busy, k)
	if avoidFor > 0 {
		p.avoid[k] = time.Now().Add(avoidFor)
	}
	p.notifyLocked()
	p.mu.Unlock()
}

// strictWait bounds how long a pinned request waits for its tuner: a
// tuner that is not ours to free (another application, or a device that has
// not yet noticed a closed stream) should not stall the caller for long.
const strictWait = 1500 * time.Millisecond

// Open takes a physical tuner, tunes it as req describes and begins
// delivering the transport stream. A pinned request (req.Tuner >= 0) uses
// exactly that tuner and fails with ErrTunerBusy / ErrTunerUnsuitable;
// otherwise it waits up to WaitFree for any suitable tuner.
func (p *Pool) Open(ctx context.Context, req Request) (*Lease, error) {
	strict := req.Tuner >= 0
	if err := p.pinnedProblem(req); err != nil {
		return nil, err
	}
	wait := p.cfg.WaitFree
	if strict && wait > strictWait {
		wait = strictWait
	}
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		p.mu.Lock()
		wake := p.changed
		p.mu.Unlock()
		for _, k := range p.rank(req) {
			dev, ok := p.markBusy(k)
			if !ok || dev == nil {
				continue
			}
			l, err := p.start(ctx, dev, k.idx, req)
			if err == nil {
				return l, nil
			}
			switch {
			case errors.Is(err, ErrNoSignal), errors.Is(err, ErrNeedsProgram), ctx.Err() != nil:
				p.unbusy(k, 0)
				return nil, err
			case errors.Is(err, errTuneFailed):
				if req.Service {
					p.setCap(k, capNo)
					log.Printf("upstream: tuner %s cannot lock ATSC 3.0", k)
				}
				p.unbusy(k, 5*time.Second)
				if strict {
					return nil, ErrTunerUnsuitable
				}
			case errors.Is(err, errInUse):
				if strict {
					p.unbusy(k, 0)
				} else {
					p.unbusy(k, time.Second)
				}
			default:
				log.Printf("upstream: tuner %s: %v", k, err)
				p.unbusy(k, 5*time.Second)
			}
			lastErr = err
		}
		// Nothing free: ask for a lingering stream to be given up.
		if p.Reclaim != nil && p.Reclaim(func(l *Lease) bool { return p.compatible(tkey{l.dev.ID, l.idx}, req) }) {
			continue
		}
		left := time.Until(deadline)
		if left <= 0 {
			if strict {
				return nil, ErrTunerBusy
			}
			if lastErr != nil && !errors.Is(lastErr, errInUse) {
				return nil, fmt.Errorf("%w (last error: %v)", ErrNoTuner, lastErr)
			}
			return nil, ErrNoTuner
		}
		if left > 300*time.Millisecond {
			left = 300 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
		case <-time.After(left):
		}
	}
}

// HeldByUs reports whether the pool itself is using the tuner.
func (p *Pool) HeldByUs(deviceID uint32, idx int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busy[tkey{deviceID, idx}]
}

// RawStatus returns the status line of a physical tuner exactly as the
// device reports it.
func (p *Pool) RawStatus(deviceID uint32, idx int) (string, error) {
	p.mu.Lock()
	d := p.devices[deviceID]
	p.mu.Unlock()
	if d == nil {
		return "", errors.New("device offline")
	}
	return d.ctl.Get(fmt.Sprintf("/tuner%d/status", idx))
}

// Suitability says how good physical tuner global is for a new stream, lower
// being better; ok is false when it cannot serve the kind of stream at all.
// ATSC 1.0 streams prefer tuners that cannot do ATSC 3.0 (keeping the capable
// ones free); ATSC 3.0 streams prefer tuners proven capable.
func (p *Pool) Suitability(global int, service bool) (class int, ok bool) {
	ref, found := p.Resolve(global)
	if !found || ref.Device == nil {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := tkey{ref.DeviceID, ref.Index}
	c := p.caps[k]
	if service {
		switch c {
		case capYes:
			return 0, true
		case capUnknown:
			return 1, true
		}
		return 0, false
	}
	return int(c) + 1, true
}

// RawGet reads any control variable of a physical tuner as the device
// reports it.
func (p *Pool) RawGet(deviceID uint32, idx int, name string) (string, error) {
	p.mu.Lock()
	d := p.devices[deviceID]
	p.mu.Unlock()
	if d == nil {
		return "", errors.New("device offline")
	}
	return d.ctl.Get(fmt.Sprintf("/tuner%d/%s", idx, name))
}
