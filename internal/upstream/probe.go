package upstream

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrUnknownChannel: no device has this virtual channel in its lineup.
var ErrUnknownChannel = errors.New("unknown channel")

// ProbeResult is what a device reports about the multiplex a channel is on.
type ProbeResult struct {
	Frequency  uint64
	Lock       string // modulation the tuner locked, e.g. "8vsb", "atsc3"
	StreamInfo string // /tunerN/streaminfo: every program of the multiplex
}

// FrequencyOf extracts the frequency from a status channel such as
// "8vsb:545000000" or "atsc3:177000000:0".
func FrequencyOf(channel string) uint64 {
	parts := strings.Split(channel, ":")
	if len(parts) < 2 {
		return 0
	}
	f, _ := strconv.ParseUint(parts[1], 10, 64)
	return f
}

// Probe tunes a virtual channel by name on any free tuner (the device resolves
// it from its own lineup), reads which RF channel it is on and what the
// multiplex carries, and lets the tuner go again. It is how the multiplexer
// learns channels it has not seen yet without scanning.
func (p *Pool) Probe(ctx context.Context, vchannel string) (ProbeResult, error) {
	unknownOn := map[uint32]bool{}
	deadline := time.Now().Add(p.cfg.WaitFree)
	var lastErr error
	for {
		p.mu.Lock()
		wake := p.changed
		p.mu.Unlock()
		for _, k := range p.rank(Request{Tuner: -1}) {
			if unknownOn[k.dev] {
				continue
			}
			dev, ok := p.markBusy(k)
			if !ok || dev == nil {
				continue
			}
			res, err := p.probeOn(ctx, dev, k.idx, vchannel)
			p.unbusy(k, 0)
			switch {
			case err == nil:
				return res, nil
			case errors.Is(err, ErrUnknownChannel):
				unknownOn[k.dev] = true
			case errors.Is(err, ErrNoSignal), ctx.Err() != nil:
				return res, err
			}
			lastErr = err
		}
		// every online device was asked and none knows it
		known := false
		for _, d := range p.Devices() {
			if !unknownOn[d.ID] {
				known = true
			}
		}
		if !known {
			return ProbeResult{}, ErrUnknownChannel
		}
		if p.Reclaim != nil && p.Reclaim(func(*Lease) bool { return true }) {
			continue
		}
		left := time.Until(deadline)
		if left <= 0 {
			if lastErr != nil && !errors.Is(lastErr, errInUse) {
				return ProbeResult{}, fmt.Errorf("%w (last error: %v)", ErrNoTuner, lastErr)
			}
			return ProbeResult{}, ErrNoTuner
		}
		if left > 300*time.Millisecond {
			left = 300 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ProbeResult{}, ctx.Err()
		case <-wake:
		case <-time.After(left):
		}
	}
}

func (p *Pool) probeOn(ctx context.Context, dev *Device, idx int, vch string) (ProbeResult, error) {
	src, err := openHTTP(ctx, dev.StreamAddr(), fmt.Sprintf("/tuner%d/v%s", idx, vch), p.cfg.TuneTimeout)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			switch {
			case he.HDHRCode == 801, he.Code == 404:
				return ProbeResult{}, ErrUnknownChannel
			case he.HDHRCode == 804 || he.HDHRCode == 805:
				return ProbeResult{}, errInUse
			case he.HDHRCode == 806:
				return ProbeResult{}, errTuneFailed
			case he.HDHRCode == 807:
				return ProbeResult{}, fmt.Errorf("%w: %v", ErrNoSignal, he)
			}
		}
		return ProbeResult{}, err
	}
	// Keep reading so the device does not stall while we ask it questions.
	go func() {
		buf := make([]byte, 64<<10)
		for {
			if _, err := src.Read(buf, time.Time{}); err != nil {
				return
			}
		}
	}()
	defer src.Close()

	name := func(v string) string { return fmt.Sprintf("/tuner%d/%s", idx, v) }
	var res ProbeResult
	var prev string
	stop := time.Now().Add(8 * time.Second)
	for time.Now().Before(stop) {
		st, err := dev.ctl.Get(name("status"))
		if err == nil {
			s := ParseStatus(st)
			if s.Locked() {
				if info, err := dev.ctl.Get(name("streaminfo")); err == nil && strings.Contains(info, ":") {
					res = ProbeResult{Frequency: FrequencyOf(s.Channel), Lock: s.Lock, StreamInfo: info}
					if info == prev && res.Frequency != 0 {
						return res, nil
					}
					prev = info
				}
			}
		}
		select {
		case <-ctx.Done():
			return ProbeResult{}, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	if res.Frequency != 0 {
		return res, nil
	}
	return ProbeResult{}, fmt.Errorf("%w: no stream information from %s", ErrNoSignal, dev.Addr())
}
