package vdev

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"hdhrmux/internal/chanmap"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/ts"
	"hdhrmux/internal/upstream"
)

var (
	ErrBusy   = errors.New("tuner in use")
	ErrLocked = errors.New("tuner locked")
)

// VTuner is one virtual tuner: the state a real HDHomeRun tuner would hold
// (channel, filter, program, target, lock) backed by a shared-stream
// subscription instead of dedicated silicon.
type VTuner struct {
	d   *Device
	idx int

	op sync.Mutex // serialises state-changing operations (may be held across network I/O)
	mu sync.Mutex // guards the fields below (short critical sections only)

	channelmap string
	tuned      bool
	spec       chanmap.Spec
	chanStr    string
	vchannel   string
	sub        *hub.Sub
	program    uint16
	explicit   bool // an explicit PID filter is in force
	pids       ts.PIDSet
	targetStr  string
	snd        *sender
	httpUse    bool // reserved/used by an HTTP stream client
	// httpSubs are the HTTP viewers attached to this tuner. Viewers of the same
	// RF channel can share a busy tuner; v.sub is the first of them (or the
	// control client's subscription). httpOpened: the tuner was opened by HTTP
	// and is released with its last viewer.
	httpSubs   []*hub.Sub
	viewers    map[*hub.Sub]viewerMeta // who is behind each HTTP viewer
	httpOpened bool
	// autoOwned: the stream was opened by an /auto/ request, so other /auto/
	// requests may join it. A tuner somebody asked for directly (by number, or
	// over the control protocol) belongs to them, as on a real device.
	autoOwned bool
	lockkey   uint32
	lockIP    string
	lastUse   time.Time

	physStatus string // cached status of the physical counterpart
	physOK     bool
	physAt     time.Time
}

func newVTuner(d *Device, idx int) *VTuner {
	return &VTuner{d: d, idx: idx, channelmap: d.cfg.ChannelMap, chanStr: "none", targetStr: "none", lastUse: time.Now()}
}

func (v *VTuner) touch() { v.mu.Lock(); v.lastUse = time.Now(); v.mu.Unlock() }

// busyLocked: the tuner is tuned, streaming over HTTP, or reserved.
func (v *VTuner) busyLocked() bool {
	return v.tuned || v.sub != nil || v.httpUse || len(v.httpSubs) > 0
}

// lockedLocked reports whether a lock is currently in force (locks expire
// when their holder stops talking to the tuner and nothing streams).
func (v *VTuner) lockedLocked() bool {
	if v.lockkey == 0 {
		return false
	}
	if v.snd == nil && time.Since(v.lastUse) > v.d.cfg.LockTimeout {
		v.lockkey, v.lockIP = 0, ""
		return false
	}
	return true
}

// ---- tuning ------------------------------------------------------------

// tuneLocked (op held) tunes to spec, sharing a physical tuner via the hub.
func (v *VTuner) tuneLocked(ctx context.Context, spec chanmap.Spec, o hub.Options) error {
	p := v.opts(o.Program, o.Service)
	o.Pin, o.Tuner = p.Pin, p.Tuner
	key := v.d.hub.KeyFor(spec, o)
	v.mu.Lock()
	same := v.tuned && v.sub != nil && v.sub.Key() == key
	was := v.tuned
	v.mu.Unlock()
	if same {
		v.mu.Lock()
		v.spec, v.chanStr = spec, spec.String()
		v.mu.Unlock()
		return nil
	}
	v.stopStream()
	if o.Program == 0 && (o.Service || v.d.hub.IsService(spec.Frequency)) {
		return v.pendingLocked(spec, was)
	}
	sub, err := v.d.hub.Subscribe(ctx, spec, o)
	if errors.Is(err, hub.ErrNeedsProgram) {
		return v.pendingLocked(spec, was)
	}
	if err != nil {
		v.mu.Lock()
		v.tuned, v.chanStr = false, "none"
		v.mu.Unlock()
		return err
	}
	v.mu.Lock()
	v.sub, v.tuned, v.spec, v.chanStr = sub, true, spec, spec.String()
	if was { // retuning resets program and filter, as on a real tuner
		v.program, v.explicit = 0, false
	}
	v.applyFilterLocked()
	v.mu.Unlock()
	return v.maybeStartSender()
}

// pendingLocked records the channel of a virtual tuner that sits on an
// ATSC 3.0 frequency without a program: the device cannot stream such a
// channel until a service is chosen, so no physical tuner is taken yet.
func (v *VTuner) pendingLocked(spec chanmap.Spec, was bool) error {
	v.mu.Lock()
	v.sub, v.tuned, v.spec, v.chanStr = nil, true, spec, spec.String()
	if was {
		v.program, v.explicit = 0, false
	}
	v.mu.Unlock()
	return nil
}

func (v *VTuner) applyFilterLocked() {
	if v.sub == nil {
		return
	}
	switch {
	case v.explicit:
		v.sub.SetFilter(v.pids)
	case v.program != 0:
		v.sub.SetProgram(v.program)
	default:
		v.sub.SetProgram(0)
	}
}

// stopStream stops the sender and detaches from the shared stream, keeping
// target/lock configuration.
func (v *VTuner) stopStream() {
	v.mu.Lock()
	snd, sub := v.snd, v.sub
	v.snd, v.sub = nil, nil
	v.mu.Unlock()
	if snd != nil {
		snd.stop()
	}
	if sub != nil {
		sub.Close()
	}
}

// releaseAll (op held) returns the tuner to its idle state.
func (v *VTuner) releaseAll(clearTarget bool) {
	v.stopStream()
	v.mu.Lock()
	v.tuned, v.chanStr, v.vchannel = false, "none", ""
	v.program, v.explicit, v.httpUse, v.autoOwned = 0, false, false, false
	v.httpSubs, v.httpOpened, v.viewers = nil, false, nil
	if clearTarget {
		v.targetStr = "none"
	}
	v.mu.Unlock()
}

func (v *VTuner) maybeStartSender() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.sub == nil || v.targetStr == "none" || v.snd != nil {
		return nil
	}
	snd, err := newSender(v, v.sub, v.targetStr)
	if err != nil {
		return err
	}
	v.snd = snd
	return nil
}

// ---- HTTP use ----------------------------------------------------------

// tryReserve claims the tuner for an HTTP client if it is idle.
func (v *VTuner) tryReserve() bool {
	if !v.op.TryLock() {
		return false
	}
	defer v.op.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.busyLocked() || v.lockedLocked() {
		return false
	}
	v.httpUse, v.lastUse = true, time.Now()
	return true
}

// startHTTP (after tryReserve) tunes and selects the program.
// startHTTP tunes the tuner for an HTTP viewer; auto says the request came to
// /auto/ (others like it may then join), not for this tuner by number.
func (v *VTuner) startHTTP(ctx context.Context, spec chanmap.Spec, program uint16, vchannel string, service, auto bool) (*hub.Sub, error) {
	v.op.Lock()
	defer v.op.Unlock()
	if err := v.tuneLocked(ctx, spec, hub.Options{Program: program, Service: service}); err != nil {
		v.releaseAll(false)
		return nil, err
	}
	v.mu.Lock()
	v.vchannel = vchannel
	v.program, v.explicit = program, false
	v.applyFilterLocked()
	sub := v.sub
	v.httpSubs, v.httpOpened = []*hub.Sub{sub}, true
	v.autoOwned = auto
	v.mu.Unlock()
	sub.Enable()
	return sub, nil
}

// joinHTTP lets another HTTP client use a tuner that is already busy when it
// asks for the same stream (same RF channel, or same ATSC 3.0 service): the
// request can be satisfied without a second tuner, so it must not be refused.
// It returns ErrBusy if the tuner is carrying something else. With wait set it
// queues behind a tune in progress instead of giving up.
func (v *VTuner) joinHTTP(ctx context.Context, spec chanmap.Spec, program uint16, service, wait bool) (*hub.Sub, error) {
	if wait {
		v.op.Lock()
	} else if !v.op.TryLock() {
		return nil, ErrBusy
	}
	o := v.opts(program, service)
	v.mu.Lock()
	ok := v.tuned && v.sub != nil && v.autoOwned && !v.lockedLocked() && v.sub.Key() == v.d.hub.KeyFor(spec, o)
	v.mu.Unlock()
	v.op.Unlock()
	if !ok {
		return nil, ErrBusy
	}
	sub, err := v.d.hub.Subscribe(ctx, spec, o)
	if err != nil {
		return nil, err
	}
	sub.SetProgram(program)
	sub.Enable()
	v.mu.Lock()
	v.httpSubs = append(v.httpSubs, sub)
	v.lastUse = time.Now()
	v.mu.Unlock()
	return sub, nil
}

// viewerMeta says who an HTTP viewer is.
type viewerMeta struct {
	client string // IP address
	label  string // channel as requested: a guide number or a /chFREQ path
	since  time.Time
}

// noteViewer records the client behind a viewer for the status page.
func (v *VTuner) noteViewer(sub *hub.Sub, m viewerMeta) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.viewers == nil {
		v.viewers = map[*hub.Sub]viewerMeta{}
	}
	v.viewers[sub] = m
}

// ViewerRow is one client of a tuner as shown on the status page.
type ViewerRow struct {
	Client  string
	Label   string
	Since   time.Time
	BPS     int
	Queue   int // % of the client buffer in use
	Drops   uint64
	Control bool // a control-protocol client (target=...) rather than HTTP
}

// viewerRows lists the clients currently attached to the tuner.
func (v *VTuner) viewerRows() []ViewerRow {
	v.mu.Lock()
	type pair struct {
		sub *hub.Sub
		m   viewerMeta
	}
	var hs []pair
	for _, s := range v.httpSubs {
		hs = append(hs, pair{s, v.viewers[s]})
	}
	sub, tgt, vch, spec := v.sub, v.targetStr, v.vchannel, v.spec
	controlled := v.snd != nil
	since := v.lastUse
	v.mu.Unlock()

	var rows []ViewerRow
	for _, h := range hs {
		bps, _ := h.sub.Rate()
		q := 0
		if c := cap(h.sub.C); c > 0 {
			q = h.sub.Buffered() * 100 / c
		}
		rows = append(rows, ViewerRow{Client: h.m.client, Label: h.m.label, Since: h.m.since, BPS: bps, Queue: q, Drops: h.sub.Drops()})
	}
	if controlled && sub != nil {
		host := ""
		if i := strings.Index(tgt, "://"); i >= 0 {
			host, _, _ = net.SplitHostPort(tgt[i+3:])
		}
		label := vch
		if label == "" {
			label = spec.String()
		}
		bps, _ := sub.Rate()
		q := 0
		if c := cap(sub.C); c > 0 {
			q = sub.Buffered() * 100 / c
		}
		rows = append(rows, ViewerRow{Client: host, Label: label, Since: since, BPS: bps, Queue: q, Drops: sub.Drops(), Control: true})
	}
	return rows
}

// endHTTP detaches one HTTP viewer; the tuner is released with the last one
// unless a control-protocol client opened it.
func (v *VTuner) endHTTP(sub *hub.Sub) {
	v.op.Lock()
	defer v.op.Unlock()
	v.mu.Lock()
	delete(v.viewers, sub)
	for i, s := range v.httpSubs {
		if s == sub {
			v.httpSubs = append(v.httpSubs[:i], v.httpSubs[i+1:]...)
			break
		}
	}
	remaining, opened := len(v.httpSubs), v.httpOpened
	if v.sub == sub && remaining > 0 {
		v.sub = v.httpSubs[0] // another viewer now represents the tuner
	}
	v.mu.Unlock()
	if remaining == 0 && opened {
		v.releaseAll(false)
	}
	sub.Close()
}

// ---- reaper ------------------------------------------------------------

// reap releases tuners left tuned with nobody using them.
func (v *VTuner) reap(idle time.Duration) {
	v.mu.Lock()
	stale := v.tuned && v.snd == nil && !v.httpUse && time.Since(v.lastUse) > idle
	v.mu.Unlock()
	if !stale || !v.op.TryLock() {
		return
	}
	defer v.op.Unlock()
	v.mu.Lock()
	stale = v.tuned && v.snd == nil && !v.httpUse && time.Since(v.lastUse) > idle
	v.mu.Unlock()
	if stale {
		v.releaseAll(false)
	}
}

// ---- variables ---------------------------------------------------------

func (v *VTuner) statusString() string {
	v.mu.Lock()
	sub, chs := v.sub, v.chanStr
	v.mu.Unlock()
	if sub == nil {
		if !v.isTuned() {
			if s, ok := v.foreignStatus(); ok {
				return s
			}
		}
		return fmt.Sprintf("ch=%s lock=none ss=0 snq=0 seq=0 bps=0 pps=0", chs)
	}
	info := sub.Info()
	st := info.Status
	ch := st.Channel
	if ch == "" {
		ch = chs
	}
	lock := st.Lock
	if lock == "" {
		lock = "none"
	}
	bps, pps := sub.Rate()
	return fmt.Sprintf("ch=%s lock=%s ss=%d snq=%d seq=%d bps=%d pps=%d", ch, lock, st.SS, st.SNQ, st.SEQ, bps, pps)
}

// debugString renders /tunerN/debug in the four-line form real devices use
// (tun / dev / ts / net). Channels DVR polls it about twice a second per
// tuner for its signal display. When another application is using this
// tuner's physical counterpart, that tuner's own reply is passed through.
func (v *VTuner) debugString() string {
	v.mu.Lock()
	sub, spec := v.sub, v.spec
	v.mu.Unlock()
	if sub == nil {
		if !v.isTuned() {
			if s, ok := v.foreignDebug(); ok {
				return s
			}
		}
		return "tun: ch=none lock=none ss=0 snq=0 seq=0 dbg=0\ndev: bps=0 resync=0 overflow=0\nts:  bps=0 te=0 crc=0\nnet: bps=0 pps=0 err=0 stop=0\n"
	}
	info := sub.Info()
	st := info.Status
	lock := st.Lock
	if lock == "" {
		lock = "none"
	} else {
		lock = fmt.Sprintf("%s:%d", lock, spec.Frequency)
	}
	ch := st.Channel
	if ch == "" {
		ch = spec.String()
	}
	bps, _ := sub.Rate()
	return fmt.Sprintf("tun: ch=%s lock=%s ss=%d snq=%d seq=%d dbg=0\n"+
		"dev: bps=%d resync=0 overflow=0\n"+
		"ts:  bps=%d te=0 crc=0\n"+
		"net: bps=%d pps=%d err=%d stop=0\n",
		ch, lock, st.SS, st.SNQ, st.SEQ, info.RxBPS, bps, bps, bps/8/1470, info.Lost)
}

// foreignDebug is foreignStatus for the debug variable.
func (v *VTuner) foreignDebug() (string, bool) {
	if _, foreign := v.foreignStatus(); !foreign {
		return "", false
	}
	pool := v.d.hub.Pool()
	ref, ok := pool.Resolve(v.idx)
	if !ok || ref.Device == nil {
		return "", false
	}
	s, err := pool.RawGet(ref.DeviceID, ref.Index, "debug")
	return s, err == nil
}

// foreignStatus answers a status query for an idle virtual tuner from its
// physical counterpart (same number in the global numbering), but only when
// somebody else is using that tuner: this is how a DVR sharing the devices
// with the multiplexer is seen. Tuners the multiplexer itself holds for other
// viewers read as idle, as the virtual tuner is.
func (v *VTuner) foreignStatus() (string, bool) {
	v.mu.Lock()
	if time.Since(v.physAt) < time.Second {
		s, ok := v.physStatus, v.physOK
		v.mu.Unlock()
		return s, ok
	}
	v.physAt = time.Now()
	v.mu.Unlock()
	var s string
	ok := false
	pool := v.d.hub.Pool()
	if ref, found := pool.Resolve(v.idx); found && ref.Device != nil && !pool.HeldByUs(ref.DeviceID, ref.Index) {
		if raw, err := pool.RawStatus(ref.DeviceID, ref.Index); err == nil && !strings.HasPrefix(raw, "ch=none") {
			s, ok = raw, true
		}
	}
	v.mu.Lock()
	v.physStatus, v.physOK = s, ok
	v.mu.Unlock()
	return s, ok
}

func (v *VTuner) streamInfoString() string {
	v.mu.Lock()
	sub := v.sub
	v.mu.Unlock()
	if sub == nil {
		return ""
	}
	return sub.Info().StreamInfo
}

func (v *VTuner) filterString() string {
	v.mu.Lock()
	sub, explicit, pids := v.sub, v.explicit, v.pids
	v.mu.Unlock()
	switch {
	case sub != nil:
		return sub.FilterString()
	case explicit:
		return pids.String()
	}
	return "0x0000-0x1fff"
}

// get returns a variable's value.
func (v *VTuner) get(name string) (string, error) {
	switch name {
	case "status":
		return v.statusString(), nil
	case "streaminfo":
		return v.streamInfoString(), nil
	case "filter":
		return v.filterString(), nil
	case "debug":
		return v.debugString(), nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	switch name {
	case "channel":
		return v.chanStr, nil
	case "channelmap":
		return v.channelmap, nil
	case "vchannel":
		if v.vchannel == "" {
			return "none", nil
		}
		return v.vchannel, nil
	case "program":
		return fmt.Sprint(v.program), nil
	case "target":
		return v.targetStr, nil
	case "lockkey":
		if !v.lockedLocked() {
			return "none", nil
		}
		return v.lockIP, nil
	case "name":
		return fmt.Sprintf("tuner%d", v.idx), nil
	case "plpinfo", "ccerror", "tqerror":
		return "", nil
	}
	return "", errors.New("ERROR: unknown getset variable")
}

// set assigns a variable. remote and key identify the requester for lock
// checks.
func (v *VTuner) set(remote net.IP, name, value string, key *uint32) (string, error) {
	v.touch()
	v.mu.Lock()
	locked := v.lockedLocked()
	holder := v.lockIP
	match := key != nil && *key == v.lockkey
	v.mu.Unlock()

	if name == "lockkey" {
		return v.setLock(remote, value, key, locked, match, holder)
	}
	if locked && !match {
		return "", fmt.Errorf("ERROR: resource locked by %s", holder)
	}
	if !locked && key != nil {
		return "", errors.New("ERROR: lock no longer held")
	}

	v.op.Lock()
	defer v.op.Unlock()
	switch name {
	case "channel":
		spec, ok, err := chanmap.Parse(value, v.channelmapNow())
		if err != nil {
			return "", errors.New("ERROR: invalid channel")
		}
		if !ok {
			v.stopStreamKeepTarget()
			return value, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		v.mu.Lock()
		var pre uint16
		if !v.tuned { // a program chosen before the channel is kept
			pre = v.program
		}
		v.mu.Unlock()
		o := hub.Options{Program: pre, Service: v.d.lineup.Modulation(spec.Frequency) == "atsc3"}
		if err := v.tuneLocked(ctx, spec, o); err != nil {
			return "", errors.New("ERROR: " + userError(err))
		}
		v.mu.Lock()
		v.vchannel = ""
		v.mu.Unlock()
	case "channelmap":
		if _, err := chanmap.Channels(value); err != nil {
			return "", errors.New("ERROR: invalid set value")
		}
		v.mu.Lock()
		v.channelmap = value
		v.mu.Unlock()
	case "vchannel":
		return v.setVChannel(value)
	case "program":
		var n uint16
		if value != "none" {
			if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
				return "", errors.New("ERROR: invalid set value")
			}
		}
		v.mu.Lock()
		v.program, v.explicit = n, false
		sub, spec := v.sub, v.spec
		v.mu.Unlock()
		if (sub != nil && sub.NeedsRetarget(n)) || (sub == nil && n != 0 && v.isTuned()) {
			// ATSC 3.0: each service has its own physical tuner.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := v.retarget(ctx, spec, n); err != nil {
				return "", errors.New("ERROR: " + userError(err))
			}
		} else {
			v.mu.Lock()
			v.applyFilterLocked()
			v.mu.Unlock()
		}
	case "filter":
		set, err := ts.ParsePIDSet(value)
		if err != nil {
			return "", errors.New("ERROR: invalid set value")
		}
		v.mu.Lock()
		v.pids, v.explicit, v.program = set, true, 0
		v.applyFilterLocked()
		v.mu.Unlock()
	case "target":
		return v.setTarget(value)
	case "name", "debug":
	default:
		return "", errors.New("ERROR: unknown getset variable")
	}
	return value, nil
}

// opts builds the hub options for a stream on this virtual tuner: virtual
// tuner N always uses physical tuner N. (Extra virtual tuners beyond the
// physical count, if configured, draw from the pool instead.)
func (v *VTuner) opts(program uint16, service bool) hub.Options {
	o := hub.Options{Program: program, Service: service}
	if v.idx < v.d.hub.Pool().TotalTuners() {
		o.Pin, o.Tuner = true, v.idx
	}
	return o
}

// pinned reports whether this virtual tuner has a physical tuner of its own.
func (v *VTuner) pinned() bool { return v.idx < v.d.hub.Pool().TotalTuners() }

// idleNow reports whether the tuner looks free to take a new stream.
func (v *VTuner) idleNow() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return !v.busyLocked() && !v.lockedLocked()
}

func (v *VTuner) currentSub() *hub.Sub {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.sub
}

func (v *VTuner) isTuned() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.tuned
}

func (v *VTuner) channelmapNow() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.channelmap
}

// stopStreamKeepTarget implements "set channel none".
func (v *VTuner) stopStreamKeepTarget() {
	v.releaseAll(false)
}

func (v *VTuner) setTarget(value string) (string, error) {
	if value == "none" {
		v.mu.Lock()
		snd := v.snd
		v.snd, v.targetStr = nil, "none"
		v.mu.Unlock()
		if snd != nil {
			snd.stop()
		}
		return value, nil
	}
	if _, err := parseTarget(value); err != nil {
		return "", errors.New("ERROR: invalid set value")
	}
	v.mu.Lock()
	old := v.snd
	v.snd, v.targetStr = nil, value
	v.mu.Unlock()
	if old != nil {
		old.stop()
	}
	if err := v.maybeStartSender(); err != nil {
		return "", errors.New("ERROR: invalid set value")
	}
	return value, nil
}

// retarget (op held) moves to the stream delivering program on the current
// RF channel.
func (v *VTuner) retarget(ctx context.Context, spec chanmap.Spec, program uint16) error {
	v.stopStream()
	sub, err := v.d.hub.Subscribe(ctx, spec, v.opts(program, true))
	if err != nil {
		v.mu.Lock()
		v.tuned, v.chanStr = false, "none"
		v.mu.Unlock()
		return err
	}
	v.mu.Lock()
	v.sub = sub
	v.applyFilterLocked()
	v.mu.Unlock()
	return v.maybeStartSender()
}

func (v *VTuner) setVChannel(value string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	e, err := v.d.resolve(ctx, value)
	if errors.Is(err, errUnknownChannel) {
		return "", errors.New("ERROR: invalid virtual channel")
	} else if err != nil {
		return "", errors.New("ERROR: " + userError(err))
	}
	o := hub.Options{Program: e.Program, Service: e.Modulation == "atsc3"}
	if err := v.tuneLocked(ctx, chanmap.Spec{Modulation: "auto", Frequency: e.Frequency}, o); err != nil {
		return "", errors.New("ERROR: " + userError(err))
	}
	v.mu.Lock()
	v.vchannel, v.program, v.explicit = value, e.Program, false
	v.applyFilterLocked()
	v.mu.Unlock()
	return value, nil
}

func (v *VTuner) setLock(remote net.IP, value string, key *uint32, locked, match bool, holder string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch value {
	case "none":
		if locked && !match {
			return "", fmt.Errorf("ERROR: resource locked by %s", holder)
		}
		v.lockkey, v.lockIP = 0, ""
	case "force":
		// As on a real device: only the lock is dropped.
		v.lockkey, v.lockIP = 0, ""
	default:
		var n uint32
		if _, err := fmt.Sscanf(value, "%d", &n); err != nil || n == 0 {
			return "", errors.New("ERROR: invalid set value")
		}
		if locked && !match {
			return "", fmt.Errorf("ERROR: resource locked by %s", holder)
		}
		v.lockkey, v.lockIP = n, remote.String()
	}
	return value, nil
}

func userError(err error) string {
	switch {
	case errors.Is(err, upstream.ErrTunerBusy):
		return "resource locked"
	case errors.Is(err, upstream.ErrTunerUnsuitable):
		return "invalid channel"
	case strings.Contains(err.Error(), "no physical tuner"):
		return "all tuners in use"
	}
	return err.Error()
}

// ---- transport sender (control-protocol "target") ----------------------

type sender struct {
	v    *VTuner
	conn *net.UDPConn
	rtp  bool
	quit chan struct{}
	done chan struct{}
	once sync.Once
}

type target struct {
	rtp  bool
	addr *net.UDPAddr
}

func parseTarget(s string) (target, error) {
	var t target
	switch {
	case strings.HasPrefix(s, "rtp://"):
		t.rtp = true
	case strings.HasPrefix(s, "udp://"):
	default:
		return t, fmt.Errorf("bad target %q", s)
	}
	a, err := net.ResolveUDPAddr("udp4", s[6:])
	if err != nil || a.Port == 0 {
		return t, fmt.Errorf("bad target %q", s)
	}
	t.addr = a
	return t, nil
}

func newSender(v *VTuner, sub *hub.Sub, tgt string) (*sender, error) {
	t, err := parseTarget(tgt)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp4", nil, t.addr)
	if err != nil {
		return nil, err
	}
	s := &sender{v: v, conn: conn, rtp: t.rtp, quit: make(chan struct{}), done: make(chan struct{})}
	sub.Enable()
	go s.run(sub)
	return s, nil
}

func (s *sender) stop() {
	s.once.Do(func() { close(s.quit) })
	<-s.done
}

// datagramSize is what real devices put in every UDP/RTP datagram: exactly
// seven TS packets. libhdhomerun clients count anything else as a network
// error and discard it, so batches are re-packed to this size.
const datagramSize = 7 * 188

func (s *sender) run(sub *hub.Sub) {
	defer close(s.done)
	defer s.conn.Close()
	var seq uint16
	start := time.Now()
	refused := 0
	var acc []byte
	send := func(payload []byte) bool {
		out := payload
		if s.rtp {
			ts90 := uint32(time.Since(start).Seconds() * 90000)
			out = make([]byte, 12+len(payload))
			out[0], out[1] = 0x80, 33
			out[2], out[3] = byte(seq>>8), byte(seq)
			out[4], out[5], out[6], out[7] = byte(ts90>>24), byte(ts90>>16), byte(ts90>>8), byte(ts90)
			out[8], out[9], out[10], out[11] = 'M', 'U', 'X', byte(s.v.idx)
			copy(out[12:], payload)
			seq++
		}
		if _, err := s.conn.Write(out); err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				refused++
				if refused >= 5 { // nobody is listening any more
					go s.v.targetGone(s)
					return false
				}
			}
			return true
		}
		refused = 0
		return true
	}
	for {
		select {
		case <-s.quit:
			return
		case <-sub.Done():
			return
		case b := <-sub.C:
			acc = append(acc, b...)
			for len(acc) >= datagramSize {
				if !send(acc[:datagramSize]) {
					return
				}
				acc = acc[datagramSize:]
			}
			if len(acc) == 0 {
				acc = nil // let the buffer be reused from the start
			}
		}
	}
}

// targetGone clears the target and frees the tuner after the receiver
// vanished (ICMP port unreachable), as a real HDHomeRun does.
func (v *VTuner) targetGone(s *sender) {
	v.op.Lock()
	defer v.op.Unlock()
	v.mu.Lock()
	cur := v.snd == s
	v.mu.Unlock()
	if cur {
		v.releaseAll(true)
	}
}
