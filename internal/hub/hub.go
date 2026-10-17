package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hdhrmux/internal/chanmap"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/psip"
	"hdhrmux/internal/ts"
	"hdhrmux/internal/upstream"
)

type Config struct {
	// Linger keeps a physical tuner tuned this long after its last
	// subscriber leaves, so quick channel flips can reuse it.
	Linger time.Duration
	// ClientBuffer is how many bytes of stream each subscription may queue for
	// a slow consumer before data is dropped (default 16 MiB, about 6 s of a
	// full ATSC 1.0 channel).
	ClientBuffer int
}

type Hub struct {
	cfg  Config
	pool *upstream.Pool

	mu       sync.Mutex
	streams  map[string]*stream
	guide    func(psip.Guide)         // receives the programme guide found in streams
	learner  func([]lineup.Entry)     // told what streams and probes reveal about channels
	learning map[string]chan struct{} // virtual channels being probed right now
	service  map[uint64]bool          // RF channels known to be ATSC 3.0 (one device-selected service per tuner)
}

func New(pool *upstream.Pool, cfg Config) *Hub {
	if cfg.ClientBuffer == 0 {
		cfg.ClientBuffer = 16 << 20
	}
	h := &Hub{cfg: cfg, pool: pool, streams: map[string]*stream{}, service: map[uint64]bool{}, learning: map[string]chan struct{}{}}
	pool.Reclaim = h.reclaim
	return h
}

// Options refine a Subscribe call.
type Options struct {
	// Program is the service the subscriber wants (0 = whole multiplex).
	// It matters for ATSC 3.0 channels, where the device only delivers one
	// selected service per tuner.
	Program uint16
	// Service hints that the channel is ATSC 3.0 (known from a scan), which
	// avoids tuning a physical tuner just to discover that.
	Service bool
	// Background marks a stream opened only to collect the guide: it is given
	// up as soon as anyone else needs its tuner.
	Background bool
	// Pin makes the stream use physical tuner number Tuner (virtual tuner N
	// is physical tuner N). Without it the pool picks any suitable tuner.
	Pin   bool
	Tuner int
}

// base is the identity of what a stream carries: the RF channel, plus the
// service on ATSC 3.0.
func (h *Hub) base(spec chanmap.Spec, o Options) (key string, service bool, program uint16) {
	key = spec.Key()
	if (o.Service || h.isService(spec.Frequency)) && o.Program != 0 {
		return fmt.Sprintf("%s/%d", key, o.Program), true, o.Program
	}
	return key, false, 0
}

func streamKey(base string, o Options) string {
	if o.Pin {
		return fmt.Sprintf("%s@%d", base, o.Tuner)
	}
	return base + "@any"
}

// KeyFor returns the identity of the stream a subscription would share.
func (h *Hub) KeyFor(spec chanmap.Spec, o Options) string {
	b, _, _ := h.base(spec, o)
	return streamKey(b, o)
}

// Carrier is a physical tuner that already carries a stream.
type Carrier struct {
	Tuner int  // global tuner number
	Idle  bool // lingering: no viewers right now
}

// Carrying lists the pinned tuners that already carry what a subscription to
// spec would receive, so a request can be served by reusing one of them
// instead of opening another tuner.
func (h *Hub) Carrying(spec chanmap.Spec, o Options) []Carrier {
	b, _, _ := h.base(spec, o)
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Carrier
	for _, st := range h.streams {
		if st.base == b && st.tuner >= 0 && !st.dead {
			out = append(out, Carrier{Tuner: st.tuner, Idle: st.refs == 0})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tuner < out[j].Tuner })
	return out
}

// IdleTuners are the tuners held only by lingering streams.
func (h *Hub) IdleTuners() map[int]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[int]bool{}
	for _, st := range h.streams {
		if st.refs == 0 && !st.dead && st.tuner >= 0 {
			out[st.tuner] = true
		}
	}
	return out
}

// IsService reports whether freq is known to be an ATSC 3.0 channel.
func (h *Hub) IsService(freq uint64) bool { return h.isService(freq) }

func (h *Hub) isService(freq uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.service[freq]
}

func (h *Hub) markService(freq uint64) {
	h.mu.Lock()
	h.service[freq] = true
	h.mu.Unlock()
}

// Subscribe attaches a new subscription to the stream for spec, tuning a
// physical tuner if no stream exists for that RF channel yet. The
// subscription delivers nothing until Enable is called.
func (h *Hub) Subscribe(ctx context.Context, spec chanmap.Spec, o Options) (*Sub, error) {
	for {
		s, err := h.subscribe(ctx, spec, o)
		if errors.Is(err, errServiceMode) {
			continue // the channel turned out to be ATSC 3.0; retry in service mode
		}
		return s, err
	}
}

func (h *Hub) subscribe(ctx context.Context, spec chanmap.Spec, o Options) (*Sub, error) {
	base, service, program := h.base(spec, o)
	key := streamKey(base, o)
	tuner := -1
	if o.Pin {
		tuner = o.Tuner
	}
	h.mu.Lock()
	st := h.streams[key]
	creator := st == nil
	if creator {
		st = newStream(h, spec, base, key, service, program, o.Program, tuner)
		st.background = o.Background
		st.idleSince = time.Now()
		h.streams[key] = st
	}
	st.refs++
	if st.idleTimer != nil {
		st.idleTimer.Stop()
		st.idleTimer = nil
	}
	h.mu.Unlock()

	if creator {
		st.err = st.start(ctx)
		if st.err != nil {
			h.mu.Lock()
			st.dead = true
			delete(h.streams, key)
			h.mu.Unlock()
		}
		close(st.ready)
	} else {
		select {
		case <-st.ready:
		case <-ctx.Done():
			h.unref(st)
			return nil, ctx.Err()
		}
	}
	if st.err != nil {
		return nil, st.err
	}
	s := &Sub{st: st, C: make(chan []byte, h.cfg.ClientBuffer/(7*ts.PacketSize)), done: make(chan struct{}), mode: modeAll, started: time.Now()}
	s.pids = ts.FullSet()
	st.addSub(s)
	return s, nil
}

func (h *Hub) unref(st *stream) {
	h.mu.Lock()
	st.refs--
	if st.refs > 0 || st.dead {
		h.mu.Unlock()
		return
	}
	finish := func() {
		h.mu.Lock()
		if st.refs > 0 || st.dead {
			h.mu.Unlock()
			return
		}
		st.dead = true
		delete(h.streams, st.key)
		h.mu.Unlock()
		st.shutdown()
	}
	if h.cfg.Linger <= 0 {
		h.mu.Unlock()
		go finish()
		return
	}
	st.idleSince = time.Now()
	st.idleTimer = time.AfterFunc(h.cfg.Linger, finish)
	h.mu.Unlock()
}

// reclaim gives up the longest-idle lingering stream whose tuner satisfies
// accept, so a request that would otherwise fail for lack of a tuner (typically
// a channel flip) can proceed immediately.
func (h *Hub) reclaim(accept func(*upstream.Lease) bool) bool {
	h.mu.Lock()
	var victim *stream
	for _, st := range h.streams {
		lingering := st.refs == 0 && st.idleTimer != nil
		if st.dead || !(lingering || (st.background && st.refs <= 1)) {
			continue
		}
		l := st.currentLease()
		if l == nil || !accept(l) {
			continue
		}
		if victim == nil || st.idleSince.Before(victim.idleSince) {
			victim = st
		}
	}
	if victim == nil {
		h.mu.Unlock()
		return false
	}
	victim.dead = true
	if victim.idleTimer != nil {
		victim.idleTimer.Stop()
	}
	delete(h.streams, victim.key)
	h.mu.Unlock()
	log.Printf("hub: giving up idle channel %s to make room", victim.key)
	victim.shutdown()
	return true
}

// Streams reports every active stream.
func (h *Hub) Streams() []Info {
	h.mu.Lock()
	list := make([]*stream, 0, len(h.streams))
	for _, st := range h.streams {
		list = append(list, st)
	}
	h.mu.Unlock()
	out := make([]Info, 0, len(list))
	for _, st := range list {
		out = append(out, st.Info())
	}
	return out
}

// Close shuts every stream down.
func (h *Hub) Close() {
	h.mu.Lock()
	list := make([]*stream, 0, len(h.streams))
	for _, st := range h.streams {
		st.dead = true
		list = append(list, st)
	}
	h.streams = map[string]*stream{}
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, st := range list {
		wg.Add(1)
		go func() { defer wg.Done(); st.shutdown() }()
	}
	wg.Wait()
}

// ---- subscription ------------------------------------------------------

type filterMode int

const (
	modeAll filterMode = iota
	modePIDs
	modeProgram
)

type filterState struct {
	pass   ts.PIDSet
	rewrit bool   // replace the PAT with patSec
	patSec []byte // single-program PAT section
}

// Sub is one consumer of a stream: a virtual tuner's data path.
type Sub struct {
	st *stream
	// C delivers batches of 1..7 whole TS packets. It is never closed; watch
	// Done instead. Batches are dropped (and counted) if the consumer is slow.
	C    chan []byte
	done chan struct{}
	once sync.Once

	// guarded by st.fmu
	mode    filterMode
	pids    ts.PIDSet
	program uint16

	filter atomic.Pointer[filterState]
	active atomic.Bool // set once a consumer is attached; until then nothing is queued

	// reader goroutine only
	pending []byte
	patCC   byte

	bytes   atomic.Uint64
	packets atomic.Uint64
	drops   atomic.Uint64

	rmu       sync.Mutex
	rateAt    time.Time
	rateBytes uint64
	ratePkts  uint64
	bps, pps  int
	started   time.Time
}

func (s *Sub) Done() <-chan struct{} { return s.done }

func (st *stream) buildFilterLocked(s *Sub) *filterState {
	f := &filterState{}
	switch s.mode {
	case modeAll:
		f.pass = ts.FullSet()
	case modePIDs:
		f.pass = s.pids
	case modeProgram:
		if st.service {
			// The device already delivers just this service; nothing to demux.
			f.pass = ts.FullSet()
			break
		}
		pmtPID, ok := st.programs[s.program]
		pmt := st.pmts[s.program]
		if !ok || pmt == nil {
			break // nothing passes until the program's PMT has been seen
		}
		f.pass.Add(pmtPID)
		for _, pid := range pmt.PIDs() {
			f.pass.Add(pid)
		}
		f.rewrit = true
		f.patSec = ts.BuildPAT(st.tsid, st.patVer, s.program, pmtPID)
	}
	return f
}

// SetProgram restricts the subscription to one program (0 = everything).
func (s *Sub) SetProgram(program uint16) {
	s.st.fmu.Lock()
	defer s.st.fmu.Unlock()
	if program == 0 {
		s.mode = modeAll
	} else {
		s.mode, s.program = modeProgram, program
	}
	s.filter.Store(s.st.buildFilterLocked(s))
}

// SetFilter passes exactly the given PIDs.
func (s *Sub) SetFilter(set ts.PIDSet) {
	s.st.fmu.Lock()
	defer s.st.fmu.Unlock()
	s.mode, s.pids, s.program = modePIDs, set, 0
	s.filter.Store(s.st.buildFilterLocked(s))
}

// Program returns the selected program (0 if none).
func (s *Sub) Program() uint16 {
	s.st.fmu.Lock()
	defer s.st.fmu.Unlock()
	if s.mode == modeProgram {
		return s.program
	}
	return 0
}

// FilterString renders the effective PID filter as HDHomeRun does.
func (s *Sub) FilterString() string {
	f := s.filter.Load()
	set := f.pass
	if f.rewrit {
		set.Add(ts.PIDPAT) // the rewritten PAT is delivered too
	}
	return set.String()
}

// Info returns the physical-side information for the subscription's stream.
func (s *Sub) Info() Info { return s.st.Info() }

// Programs lists the program numbers present in the stream.
func (s *Sub) Programs() []uint16 { return s.st.Programs() }

// Rate returns the delivered bitrate and packet rate, refreshed at most
// twice a second.
func (s *Sub) Rate() (bps, pps int) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	now := time.Now()
	if dt := now.Sub(s.rateAt); dt >= 500*time.Millisecond {
		if s.rateAt.IsZero() {
			dt = now.Sub(s.started)
		}
		b, p := s.bytes.Load(), s.packets.Load()
		if sec := dt.Seconds(); sec > 0 {
			s.bps = int(float64(b-s.rateBytes) * 8 / sec)
			s.pps = int(float64(p-s.ratePkts) / sec)
		}
		s.rateAt, s.rateBytes, s.ratePkts = now, b, p
	}
	return s.bps, s.pps
}

// Drops returns the number of batches discarded because the consumer lagged.
func (s *Sub) Drops() uint64 { return s.drops.Load() }

// Enable starts delivery to C, discarding anything queued earlier. Call it
// once the consumer is attached and the filter is set.
func (s *Sub) Enable() {
	for {
		select {
		case <-s.C:
			continue
		default:
		}
		break
	}
	s.rmu.Lock()
	s.started = time.Now()
	s.rateAt = time.Time{}
	s.rmu.Unlock()
	s.active.Store(true)
}

// Close detaches the subscription; the physical tuner is released when the
// last subscription of a stream closes.
func (s *Sub) Close() {
	s.once.Do(func() {
		close(s.done)
		s.st.removeSub(s)
		s.st.h.unref(s.st)
	})
}

// offer is called by the stream reader for each packet.
func (s *Sub) offer(p []byte, pid uint16) {
	if !s.active.Load() {
		return
	}
	f := s.filter.Load()
	if f == nil {
		return
	}
	if f.rewrit && pid == ts.PIDPAT {
		if ts.PUSI(p) {
			s.append(ts.PacketizeSection(ts.PIDPAT, s.patCC, f.patSec))
			s.patCC = (s.patCC + 1) & 15
		}
		return
	}
	if f.pass.Has(pid) {
		s.append(p)
	}
}

func (s *Sub) append(p []byte) {
	s.pending = append(s.pending, p...)
	if len(s.pending) >= 7*ts.PacketSize {
		s.flush()
	}
}

func (s *Sub) flush() {
	if len(s.pending) == 0 {
		return
	}
	b := make([]byte, len(s.pending))
	copy(b, s.pending)
	s.pending = s.pending[:0]
	select {
	case s.C <- b:
		s.bytes.Add(uint64(len(b)))
		s.packets.Add(uint64(len(b) / ts.PacketSize))
	default:
		s.drops.Add(1)
	}
}

// Key identifies the shared stream the subscription is attached to.
func (s *Sub) Key() string { return s.st.key }

// NeedsRetarget reports whether selecting program on this subscription
// requires moving to a different stream: true on ATSC 3.0 channels, where
// each service is delivered by its own physical tuner.
func (s *Sub) NeedsRetarget(program uint16) bool {
	return s.st.h.isService(s.st.spec.Frequency) && program != s.st.program
}

// GuideInterval is how often a running stream hands over guide data it has
// collected since the last time.
var GuideInterval = 10 * time.Second

// SetGuideSink registers the function that receives the programme guide
// found in the PSIP tables of running streams.
func (h *Hub) SetGuideSink(f func(psip.Guide)) {
	h.mu.Lock()
	h.guide = f
	h.mu.Unlock()
}

func (h *Hub) publishGuide(g psip.Guide) {
	h.mu.Lock()
	f := h.guide
	h.mu.Unlock()
	if f != nil {
		f(g)
	}
}

// CollectGuide tunes a multiplex on any free tuner and listens to its guide
// tables for hold. It gives the tuner up at once if someone else needs it.
func (h *Hub) CollectGuide(ctx context.Context, spec chanmap.Spec, hold time.Duration) error {
	sub, err := h.Subscribe(ctx, spec, Options{Background: true})
	if err != nil {
		return err
	}
	defer sub.Close()
	select {
	case <-time.After(hold):
	case <-sub.st.stop: // taken away
	case <-ctx.Done():
	}
	return nil
}

// SetLearner registers the function that is told which RF channel, program
// and modulation channels were seen on, so the lineup can remember them.
func (h *Hub) SetLearner(f func([]lineup.Entry)) {
	h.mu.Lock()
	h.learner = f
	h.mu.Unlock()
}

func (h *Hub) learn(es []lineup.Entry) {
	h.mu.Lock()
	f := h.learner
	h.mu.Unlock()
	if f != nil && len(es) > 0 {
		f(es)
	}
}

// LearnVChannel finds out where a virtual channel lives by tuning it by name
// on a free tuner (the device resolves it from its own lineup) and reading the
// multiplex's program list. Concurrent requests for the same channel share one
// probe. Everything on the same multiplex is learned at once.
func (h *Hub) LearnVChannel(ctx context.Context, vchannel string) error {
	h.mu.Lock()
	if wait, ok := h.learning[vchannel]; ok {
		h.mu.Unlock()
		select {
		case <-wait:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	h.learning[vchannel] = done
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.learning, vchannel)
		h.mu.Unlock()
		close(done)
	}()

	res, err := h.pool.Probe(ctx, vchannel)
	if err != nil {
		return err
	}
	es := ParseStreamInfo(res.StreamInfo, res.Frequency, res.Lock, "")
	if strings.HasPrefix(res.Lock, "atsc3") {
		h.markService(res.Frequency)
	}
	h.learn(es)
	log.Printf("hub: learned %d channel(s) on %d Hz (%s) while looking for %s", len(es), res.Frequency, res.Lock, vchannel)
	return nil
}

// Pool returns the physical tuner pool the hub allocates from.
func (h *Hub) Pool() *upstream.Pool { return h.pool }

// Buffered returns how many batches are queued for the consumer.
func (s *Sub) Buffered() int { return len(s.C) }
