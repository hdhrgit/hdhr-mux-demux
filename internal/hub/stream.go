// Package hub shares physical tuners between any number of virtual tuners.
//
// A stream is one physical tuner locked to one RF channel, delivering the
// full MPEG transport stream. Any number of subscriptions attach to it; each
// has its own PID/program filter, so many virtual tuners watching programs of
// the same multiplex cost a single physical tuner.
package hub

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hdhrmux/internal/chanmap"
	"hdhrmux/internal/psip"
	"hdhrmux/internal/ts"
	"hdhrmux/internal/upstream"
)

type stream struct {
	h    *Hub
	key  string
	spec chanmap.Spec

	// service streams (ATSC 3.0) carry one device-selected service rather
	// than the whole multiplex: program is the service the device was told
	// to deliver. req is the program the first subscriber asked for.
	service bool
	program uint16
	req     uint16
	base    string // what the stream carries (RF channel [/service])
	// background streams exist only to collect the guide; they give way to
	// anyone who needs the tuner.
	background bool
	unlocked   bool            // lock-less (see Options.Unlocked)
	collector  *psip.Collector // PSIP guide tables (whole-multiplex streams only)
	lastGuide  time.Time
	tuner      int // physical tuner it is pinned to, -1 for any

	broken   chan struct{} // the source failed; wakes the monitor
	maskKick chan struct{} // demand for PIDs changed; wakes the masker

	// guarded by Hub.mu
	refs      int
	dead      bool
	idleTimer *time.Timer
	idleSince time.Time

	ready chan struct{} // closed once start() finished
	err   error

	lmu   sync.Mutex // guards lease
	lease *upstream.Lease

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	subs atomic.Pointer[[]*Sub]

	// PSI state; written by the reader goroutine under fmu, read under fmu.
	fmu      sync.Mutex
	tsid     uint16
	patVer   byte
	programs map[uint16]uint16 // program -> PMT PID
	pmts     map[uint16]*ts.PMT
	sections map[uint16]*ts.SectionReader // reader goroutine only
	lastSec  map[uint16][]byte            // last PAT/PMT section seen per PID, to skip repeats (reader goroutine only)
	isPSI    [ts.PIDMax + 1]bool          // reader goroutine only
	// PIDs of the guide tables, written by the reader goroutine under fmu
	guidePIDs ts.PIDSet

	lastData atomic.Int64 // unix nanos
	lost     atomic.Uint64
	rxBytes  atomic.Uint64

	smu         sync.Mutex // guards status & info
	status      upstream.Status
	statusAt    time.Time
	streamInfo  string
	infoAt      time.Time
	learnedInfo string

	rateAt    time.Time // bitrate sampling for Info
	rateBytes uint64
	rxBPS     int
}

func newStream(h *Hub, spec chanmap.Spec, base, key string, service bool, program, req uint16, tuner int) *stream {
	st := &stream{
		h: h, key: key, spec: spec, service: service, program: program, req: req, base: base, tuner: tuner,
		broken: make(chan struct{}, 1), maskKick: make(chan struct{}, 1),
		ready: make(chan struct{}), stop: make(chan struct{}),
		programs: map[uint16]uint16{}, pmts: map[uint16]*ts.PMT{},
		sections: map[uint16]*ts.SectionReader{}, lastSec: map[uint16][]byte{},
	}
	empty := []*Sub{}
	st.subs.Store(&empty)
	st.isPSI[ts.PIDPAT] = true
	if !service {
		st.collector = psip.NewCollector()
	}
	return st
}

// start takes a physical tuner for the stream and launches the workers.
func (st *stream) start(ctx context.Context) error {
	lease, err := st.h.pool.Open(ctx, st.request(nil))
	if err != nil {
		if errors.Is(err, upstream.ErrNeedsProgram) {
			// ATSC 3.0: the device only streams a selected service.
			st.h.markService(st.spec.Frequency)
			if st.req != 0 {
				return errServiceMode
			}
			return ErrNeedsProgram
		}
		return err
	}
	st.lmu.Lock()
	st.lease = lease
	st.lmu.Unlock()
	st.lastData.Store(time.Now().UnixNano())
	log.Printf("hub: channel %s on physical tuner %s (#%d)", st.key, lease.ID(), lease.Global())
	st.pollStatus(lease)
	st.wg.Add(3)
	go st.reader()
	go st.monitor()
	go st.masker(MaskGrace)
	return nil
}

func (st *stream) request(skip map[string]bool) upstream.Request {
	return upstream.Request{
		Frequency: st.spec.Frequency,
		Program:   st.program,
		Service:   st.service || st.h.isService(st.spec.Frequency),
		Tuner:     st.tuner,
		Skip:      skip,
		Unlocked:  st.unlocked,
	}
}

var (
	// ErrNoSignal means the device could not deliver video on the channel.
	ErrNoSignal = upstream.ErrNoSignal
	// ErrNeedsProgram: an ATSC 3.0 channel cannot be tuned without choosing a
	// service; the caller should wait for the program and subscribe again.
	ErrNeedsProgram = upstream.ErrNeedsProgram
	errServiceMode  = errors.New("channel needs service mode")
)

func (st *stream) currentLease() *upstream.Lease {
	st.lmu.Lock()
	defer st.lmu.Unlock()
	return st.lease
}

func (st *stream) shutdown() {
	st.stopOnce.Do(func() {
		close(st.stop)
		if l := st.currentLease(); l != nil {
			l.Release() // closes the receiver, which unblocks the reader
		}
		st.wg.Wait()
		// A failover may have swapped in a new tuner while we were stopping.
		if l := st.currentLease(); l != nil {
			l.Release()
		}
		log.Printf("hub: channel %s released", st.key)
	})
}

// ---- reader ------------------------------------------------------------

func (st *stream) reader() {
	defer st.wg.Done()
	defer st.publishGuide(true)
	buf := make([]byte, 64*1024)
	var tail []byte // partial packet carried over to the next read (HTTP)
	var join []byte
	for {
		select {
		case <-st.stop:
			return
		default:
		}
		lease := st.currentLease()
		b, err := lease.Read(buf, time.Now().Add(500*time.Millisecond))
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			// The source ended or broke. If this is still the live lease the
			// monitor must replace it right away; otherwise we were swapped.
			if st.currentLease() == lease {
				select {
				case st.broken <- struct{}{}:
				default:
				}
				select {
				case <-st.stop:
					return
				case <-time.After(200 * time.Millisecond):
				}
			}
			tail = nil
			continue
		}
		if len(b) == 0 {
			continue
		}
		st.lastData.Store(time.Now().UnixNano())
		st.rxBytes.Add(uint64(len(b)))
		st.lost.Store(lease.Lost())
		if len(tail) > 0 {
			join = append(append(join[:0], tail...), b...)
			b = join
		}
		tail = append(tail[:0], st.process(b)...)
		st.publishGuide(false)
	}
}

// publishGuide hands the guide collected so far to the hub, at most every
// GuideInterval unless forced. Called from the reader goroutine only.
func (st *stream) publishGuide(force bool) {
	if st.collector == nil || (!force && time.Since(st.lastGuide) < GuideInterval) {
		return
	}
	st.lastGuide = time.Now()
	if st.collector.Changed() {
		st.h.publishGuide(st.collector.Snapshot())
	}
}

// process feeds whole TS packets to the subscribers and returns the trailing
// partial packet, if any.
func (st *stream) process(b []byte) []byte {
	subs := *st.subs.Load()
	for len(b) >= ts.PacketSize {
		if b[0] != ts.SyncByte {
			i := bytes.IndexByte(b, ts.SyncByte)
			if i < 0 {
				b = nil
				break
			}
			b = b[i:]
			continue
		}
		p := b[:ts.PacketSize]
		b = b[ts.PacketSize:]
		pid := ts.PID(p)
		if st.isPSI[pid] {
			st.handlePSI(pid, p)
			subs = *st.subs.Load()
		}
		if st.collector != nil && st.collector.Wants(pid) {
			st.collector.Push(pid, p)
			st.noteGuidePIDs()
		}
		for _, s := range subs {
			s.offer(p, pid)
		}
	}
	for _, s := range subs {
		s.flush()
	}
	return b
}

// noteGuidePIDs passes new guide PIDs (found in the MGT) on to the masker.
func (st *stream) noteGuidePIDs() {
	if set, grown := st.collector.WantedPIDs(); grown {
		st.fmu.Lock()
		st.guidePIDs = set
		st.fmu.Unlock()
		st.kickMask()
	}
}

func (st *stream) handlePSI(pid uint16, p []byte) {
	r := st.sections[pid]
	if r == nil {
		r = &ts.SectionReader{}
		st.sections[pid] = r
	}
	for _, sec := range r.Push(p) {
		if sec[5]&1 == 0 { // not yet applicable
			continue
		}
		if bytes.Equal(st.lastSec[pid], sec) {
			continue // a repeat of what was already processed
		}
		st.lastSec[pid] = sec
		if pid == ts.PIDPAT {
			if pat, err := ts.ParsePAT(sec); err == nil {
				st.updatePAT(pat)
			}
		} else if pmt, err := ts.ParsePMT(sec); err == nil {
			st.updatePMT(pid, pmt)
		}
	}
}

func (st *stream) updatePAT(pat *ts.PAT) {
	st.fmu.Lock()
	defer st.fmu.Unlock()
	same := st.tsid == pat.TSID && st.patVer == pat.Version && len(st.programs) == len(pat.Programs)
	for k, v := range pat.Programs {
		if st.programs[k] != v {
			same = false
		}
	}
	if same {
		return
	}
	for pid := range st.lastSec { // PMTs seen before this PAT may have been ignored
		if pid != ts.PIDPAT {
			delete(st.lastSec, pid)
		}
	}
	for _, pid := range st.programs {
		st.isPSI[pid] = false
	}
	st.tsid, st.patVer, st.programs = pat.TSID, pat.Version, pat.Programs
	for _, pid := range pat.Programs {
		st.isPSI[pid] = true
	}
	for prog := range st.pmts {
		if _, ok := pat.Programs[prog]; !ok {
			delete(st.pmts, prog)
		}
	}
	st.recomputeLocked()
}

func (st *stream) updatePMT(pid uint16, pmt *ts.PMT) {
	st.fmu.Lock()
	defer st.fmu.Unlock()
	if st.programs[pmt.Program] != pid {
		return
	}
	if old := st.pmts[pmt.Program]; old != nil && bytes.Equal(old.Raw, pmt.Raw) {
		return
	}
	st.pmts[pmt.Program] = pmt
	st.recomputeLocked()
}

func (st *stream) recomputeLocked() {
	for _, s := range *st.subs.Load() {
		s.filter.Store(st.buildFilterLocked(s))
	}
	st.kickMask()
}

func (st *stream) addSub(s *Sub) {
	st.fmu.Lock()
	defer st.fmu.Unlock()
	old := *st.subs.Load()
	next := make([]*Sub, len(old), len(old)+1)
	copy(next, old)
	next = append(next, s)
	s.filter.Store(st.buildFilterLocked(s))
	st.subs.Store(&next)
}

func (st *stream) removeSub(s *Sub) {
	st.fmu.Lock()
	defer st.fmu.Unlock()
	old := *st.subs.Load()
	next := make([]*Sub, 0, len(old))
	for _, o := range old {
		if o != s {
			next = append(next, o)
		}
	}
	st.subs.Store(&next)
	st.kickMask()
}

// Programs returns the programs currently seen in the transport stream.
func (st *stream) Programs() []uint16 {
	st.fmu.Lock()
	defer st.fmu.Unlock()
	out := make([]uint16, 0, len(st.programs))
	for p := range st.programs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---- monitor -----------------------------------------------------------

// StallLimit is how long a locked stream may deliver no data before the hub
// moves it to another physical tuner.
var StallLimit = 8 * time.Second

func (st *stream) monitor() {
	stallLimit := StallLimit // read once: tests change it between streams
	defer st.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	fails := 0
	for {
		reset, broken := false, false
		select {
		case <-st.stop:
			return
		case <-st.broken:
			broken = true
		case <-t.C:
		}
		lease := st.currentLease()
		if lease.Unlocked() && lease.Claimed() {
			// Somebody else took the tuner: that is what unlocked is for. End quietly.
			log.Printf("hub: %s on %s was taken over; letting it go", st.key, lease.ID())
			go st.h.dropStream(st)
			return
		}
		stalled := false
		if !broken {
			s, ok := st.pollStatus(lease)
			if ok {
				fails = 0
			} else {
				fails++
			}
			idleOK := st.h.isService(st.spec.Frequency) && st.program == 0 // ATSC 3.0 without a selected service carries no data
			stalled = ok && !idleOK && s.Locked() && time.Since(time.Unix(0, st.lastData.Load())) > stallLimit
			reset = ok && (s.Channel == "none" || s.Channel == "") // someone cleared the tuner
		}
		if fails >= 3 || stalled || reset || broken {
			log.Printf("hub: channel %s on %s failing (status errors=%d stalled=%v reset=%v broken=%v); retuning", st.key, lease.ID(), fails, stalled, reset, broken)
			if st.failover(lease) {
				fails = 0
			}
		}
	}
}

// pollStatus refreshes the cached physical status and, until it is known and
// then every few seconds, the stream info.
func (st *stream) pollStatus(lease *upstream.Lease) (upstream.Status, bool) {
	s, err := lease.Status()
	if err != nil {
		return s, false
	}
	st.Info() // keeps the bitrate sample fresh even if nobody asks
	st.smu.Lock()
	st.status, st.statusAt = s, time.Now()
	need := !strings.Contains(st.streamInfo, "tsid=") || time.Since(st.infoAt) > 5*time.Second
	st.smu.Unlock()
	if need {
		defer st.learnFromInfo()
		if lease.Reclaim() {
			log.Printf("hub: lock on %s was released externally; re-taken", lease.ID())
		}
		if info, err := lease.Get("streaminfo"); err == nil {
			st.smu.Lock()
			st.streamInfo, st.infoAt = info, time.Now()
			st.smu.Unlock()
		}
	}
	return s, true
}

// learnFromInfo tells the hub which channels the stream's multiplex carries
// (once per change of the program list).
func (st *stream) learnFromInfo() {
	st.smu.Lock()
	info, lock := st.streamInfo, st.status.Lock
	changed := info != st.learnedInfo
	st.learnedInfo = info
	st.smu.Unlock()
	if changed && lock != "" && lock != "none" {
		st.h.learn(ParseStreamInfo(info, st.spec.Frequency, lock, ""))
	}
}

// failover moves the stream to a different physical tuner (or re-tunes the
// same one when no other is free).
func (st *stream) failover(old *upstream.Lease) bool {
	st.h.stats.failovers.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		select {
		case <-st.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	if st.tuner >= 0 {
		// Strict mapping: first put the stream back on its own tuner...
		old.Release()
		if nl, err := st.h.pool.Open(ctx, st.request(nil)); err == nil {
			st.swap(nl)
			log.Printf("hub: channel %s re-tuned on %s", st.key, nl.ID())
			return true
		}
		// ...and only if that tuner is unusable, move to any other one.
		req := st.request(map[string]bool{old.ID(): true})
		req.Tuner = -1
		nl, err := st.h.pool.Open(ctx, req)
		if err != nil {
			log.Printf("hub: retune %s failed: %v", st.key, err)
			return false
		}
		st.swap(nl)
		log.Printf("hub: channel %s moved off its own tuner %s to %s", st.key, old.ID(), nl.ID())
		return true
	}
	nl, err := st.h.pool.Open(ctx, st.request(map[string]bool{old.ID(): true}))
	if err != nil {
		// No alternative: let go of the suspect tuner and try again from scratch.
		old.Release()
		nl, err = st.h.pool.Open(ctx, st.request(nil))
		if err != nil {
			log.Printf("hub: retune %s failed: %v", st.key, err)
			return false
		}
		st.swap(nl)
		log.Printf("hub: channel %s re-tuned on %s", st.key, nl.ID())
		return true
	}
	old.Quarantine(20 * time.Second)
	st.swap(nl)
	go old.Release()
	log.Printf("hub: channel %s moved %s -> %s", st.key, old.ID(), nl.ID())
	return true
}

func (st *stream) swap(nl *upstream.Lease) {
	st.lmu.Lock()
	st.lease = nl
	st.lmu.Unlock()
	st.kickMask()
	st.lastData.Store(time.Now().UnixNano())
	select { // drop a stale failure notice
	case <-st.broken:
	default:
	}
	select {
	case <-st.stop: // shutting down: nobody will release this lease otherwise
		nl.Release()
	default:
	}
}

// Info is a snapshot of the physical side of a stream.
type Info struct {
	Spec       chanmap.Spec
	TunerID    string
	Status     upstream.Status
	StreamInfo string
	Subs       int
	RxBytes    uint64
	Lost       uint64
	RxBPS      int // bitrate pulled from the physical tuner
	// set by Hub.Streams only
	Background bool // opened just to collect the guide
	Lingering  bool // nobody is watching; held for a quick return
}

func (st *stream) Info() Info {
	st.smu.Lock()
	defer st.smu.Unlock()
	id := ""
	if l := st.currentLease(); l != nil {
		id = l.ID()
	}
	// bitrate of the stream, refreshed at most twice a second
	now := time.Now()
	rx := st.rxBytes.Load()
	if dt := now.Sub(st.rateAt); dt >= 500*time.Millisecond {
		if !st.rateAt.IsZero() {
			st.rxBPS = int(float64(rx-st.rateBytes) * 8 / dt.Seconds())
		}
		st.rateAt, st.rateBytes = now, rx
	}
	return Info{
		Spec: st.spec, TunerID: id, Status: st.status, StreamInfo: st.streamInfo,
		Subs: len(*st.subs.Load()), RxBytes: rx, Lost: st.lost.Load(), RxBPS: st.rxBPS,
	}
}
