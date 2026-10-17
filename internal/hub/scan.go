package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"hdhrmux/internal/chanmap"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/upstream"
)

// ScanConfig controls a channel scan.
type ScanConfig struct {
	Channels   []chanmap.Channel
	Modulation string // channel string prefix, default "auto"
	Source     string // "Antenna" or "Cable"
	// LockWait / InfoWait bound the time spent on each RF channel. Channels
	// with no signal energy are abandoned after one second; ATSC 3.0 can
	// take 1.5 s or more to lock, hence the long ceiling.
	LockWait time.Duration
	InfoWait time.Duration
	// PerDevice limits how many tuners of each device are used (0 = every
	// free one).
	PerDevice int
}

// Progress is reported after each RF channel is examined.
type Progress struct {
	Done, Total int
	Found       int
}

// Scan scans the channel list using one tuner on every device at once (each
// worker takes the next unscanned channel), then merges the results into one
// lineup: when a guide number shows up on several RF channels the best copy is
// kept, and codec/HD details are taken from the devices' own lineups.
func (h *Hub) Scan(ctx context.Context, cfg ScanConfig, progress func(Progress)) ([]lineup.Entry, error) {
	if cfg.Modulation == "" {
		cfg.Modulation = "auto"
	}
	if cfg.LockWait == 0 {
		cfg.LockWait = 4 * time.Second
	}
	if cfg.InfoWait == 0 {
		cfg.InfoWait = 6 * time.Second
	}
	devs := h.pool.Devices()
	if len(devs) == 0 {
		return nil, upstream.ErrNoTuner
	}

	// Take tuners: on every device, as many of the free ones as allowed
	// (all of them by default; scanning never evicts a viewer).
	type worker struct {
		lease   *upstream.Lease
		capable bool // proven to lock ATSC 3.0
	}
	var workers []worker
	anyCapable := false
	for _, d := range devs {
		for n := 0; cfg.PerDevice == 0 || n < cfg.PerDevice; n++ {
			lease, err := h.pool.AcquireControl(ctx, d.ID)
			if err != nil {
				if n == 0 {
					log.Printf("scan: no free tuner on %08X: %v", d.ID, err)
				}
				break
			}
			w := worker{lease, lease.ATSC3Capable()}
			anyCapable = anyCapable || w.capable
			workers = append(workers, w)
		}
	}
	if len(workers) == 0 {
		return nil, upstream.ErrNoTuner
	}

	// Two queues: everything starts in "general"; a channel that shows a
	// strong signal but does not lock on a tuner that cannot do ATSC 3.0 is
	// handed to the capable tuners instead.
	general := make(chan chanmap.Channel, len(cfg.Channels))
	capable := make(chan chanmap.Channel, len(cfg.Channels))
	var pending sync.WaitGroup
	for _, c := range cfg.Channels {
		pending.Add(1)
		general <- c
	}
	allDone := make(chan struct{})
	go func() { pending.Wait(); close(allDone) }()

	var (
		mu    sync.Mutex
		found []lineup.Entry
		done  int
		wg    sync.WaitGroup
	)
	// next hands a worker its next channel: capable tuners take the hand-offs
	// first; everyone else only draws from the general queue.
	next := func(capableWorker bool) (ch chanmap.Channel, ok bool) {
		if capableWorker {
			select {
			case ch = <-capable:
				return ch, true
			default:
			}
			select {
			case ch = <-capable:
				return ch, true
			case ch = <-general:
				return ch, true
			case <-allDone:
			case <-ctx.Done():
			}
			return ch, false
		}
		select {
		case ch = <-general:
			return ch, true
		case <-allDone:
		case <-ctx.Done():
		}
		return ch, false
	}
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer w.lease.Release()
			for {
				ch, ok := next(w.capable)
				if !ok {
					return
				}
				es, strong := scanChannel(ctx, w.lease, cfg, ch)
				if len(es) == 0 && strong {
					if !w.capable && anyCapable {
						capable <- ch // not counted yet: a capable tuner will finish it
						continue
					}
					es, _ = scanChannel(ctx, w.lease, cfg, ch) // ATSC 3.0 sometimes needs a second try
				}
				mu.Lock()
				found = append(found, es...)
				done++
				p := Progress{Done: done, Total: len(cfg.Channels), Found: len(found)}
				mu.Unlock()
				if progress != nil {
					progress(p)
				}
				pending.Done()
			}
		}()
	}
	log.Printf("scan: %d channel(s) on %d tuner(s) of %d device(s)", len(cfg.Channels), len(workers), len(devs))
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	out := mergeEntries(found)
	enrich(ctx, out, devs)
	return out, nil
}

// mergeEntries keeps one entry per guide number: not encrypted, then ATSC 1.0
// (works on every tuner) over ATSC 3.0, then the better signal.
func mergeEntries(es []lineup.Entry) []lineup.Entry {
	better := func(a, b lineup.Entry) bool {
		if a.DRM != b.DRM {
			return !a.DRM
		}
		if (a.Modulation == "atsc3") != (b.Modulation == "atsc3") {
			return a.Modulation != "atsc3"
		}
		if a.SignalQuality != b.SignalQuality {
			return a.SignalQuality > b.SignalQuality
		}
		return a.Frequency < b.Frequency
	}
	best := map[string]lineup.Entry{}
	for _, e := range es {
		if old, ok := best[e.GuideNumber]; !ok || better(e, old) {
			if ok {
				log.Printf("scan: guide number %s on %d Hz preferred over %d Hz", e.GuideNumber, e.Frequency, old.Frequency)
			}
			best[e.GuideNumber] = e
		} else {
			log.Printf("scan: guide number %s on %d Hz ignored in favour of %d Hz", e.GuideNumber, e.Frequency, old.Frequency)
		}
	}
	out := make([]lineup.Entry, 0, len(best))
	for _, e := range best {
		out = append(out, e)
	}
	return out
}

// enrich fills codec and HD details from the physical devices' own lineups.
func enrich(ctx context.Context, es []lineup.Entry, devs []*upstream.Device) {
	type devEntry struct {
		GuideNumber string
		VideoCodec  string
		AudioCodec  string
		HD          int
	}
	meta := map[string]devEntry{}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, d := range devs {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+net.JoinHostPort(d.IP.String(), "80")+"/lineup.json", nil)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var list []devEntry
		err = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if err != nil {
			continue
		}
		for _, e := range list {
			if _, ok := meta[e.GuideNumber]; !ok {
				meta[e.GuideNumber] = e
			}
		}
	}
	for i := range es {
		if m, ok := meta[es[i].GuideNumber]; ok {
			es[i].VideoCodec, es[i].AudioCodec, es[i].HD = m.VideoCodec, m.AudioCodec, m.HD == 1
		}
	}
}

// minSignal is the signal strength (%) below which a channel is considered
// empty (measured: empty channels read 21-28, real ones 50 and up).
const (
	minSignal    = 35
	strongSignal = 45 // enough signal that a failure to lock deserves a retry
)

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// scanChannel examines one RF channel. strong reports a clear signal that
// nevertheless did not lock (worth a retry).
func scanChannel(ctx context.Context, lease *upstream.Lease, cfg ScanConfig, ch chanmap.Channel) (entries []lineup.Entry, strong bool) {
	if err := lease.Retune(fmt.Sprintf("%s:%d", cfg.Modulation, ch.Frequency)); err != nil {
		log.Printf("scan: tune %d Hz: %v", ch.Frequency, err)
		return nil, false
	}
	var st upstream.Status
	began := time.Now()
	for {
		s, err := lease.Status()
		if err == nil && s.Locked() {
			st = s
			break
		}
		waited := time.Since(began)
		if waited > cfg.LockWait || (err == nil && s.SS < minSignal && waited > time.Second) || !sleepCtx(ctx, 100*time.Millisecond) {
			if err == nil && s.SS >= minSignal {
				log.Printf("scan: %d Hz on %s: signal %d%% but no lock", ch.Frequency, lease.ID(), s.SS)
			} else if err != nil {
				log.Printf("scan: %d Hz on %s: %v", ch.Frequency, lease.ID(), err)
			}
			return nil, err == nil && s.SS >= strongSignal
		}
	}
	// Wait for the program table (PSIP/SDT names arrive after the PAT).
	deadline := time.Now().Add(cfg.InfoWait)
	var info, prev string
	for {
		v, err := lease.Get("streaminfo")
		// Some multiplexes never report a tsid, so a program line is what counts.
		if err == nil && strings.Contains(v, ":") {
			info = v
			if v == prev {
				break
			}
			prev = v
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, 400*time.Millisecond) {
			break
		}
	}
	if s, err := lease.Status(); err == nil && s.Locked() {
		st = s
	}
	es := ParseStreamInfo(info, ch.Frequency, st.Lock, cfg.Source)
	for i := range es {
		es[i].SignalQuality = st.SNQ
	}
	return es, false
}

// ParseStreamInfo converts a /tunerN/streaminfo reply into lineup entries.
func ParseStreamInfo(info string, freq uint64, modulation, source string) []lineup.Entry {
	var out []lineup.Entry
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		num, rest, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, "tsid") {
			continue
		}
		prog, err := strconv.Atoi(strings.TrimSpace(num))
		if err != nil {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 || fields[0] == "0" {
			continue
		}
		vch := fields[0]
		if _, err := strconv.ParseFloat(vch, 64); err != nil {
			continue
		}
		name := strings.Join(fields[1:], " ")
		e := lineup.Entry{GuideNumber: vch, Frequency: freq, Program: uint16(prog), Modulation: modulation, Source: source}
		switch {
		case strings.HasSuffix(name, "(control)"):
			continue
		case strings.HasSuffix(name, "(no data)"):
			// Low-rate subchannels that happen to be silent at that moment;
			// the devices' own lineups keep them and they stream fine.
			name = strings.TrimSpace(strings.TrimSuffix(name, "(no data)"))
		case strings.HasSuffix(name, "(encrypted)"):
			e.DRM = true
			name = strings.TrimSpace(strings.TrimSuffix(name, "(encrypted)"))
		}
		if name == "" {
			name = vch
		}
		e.GuideName = name
		out = append(out, e)
	}
	return out
}
