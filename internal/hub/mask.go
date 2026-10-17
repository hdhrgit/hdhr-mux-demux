package hub

import (
	"errors"
	"log"
	"time"

	"hdhrmux/internal/ts"
	"hdhrmux/internal/upstream"
)

// The device streams a whole multiplex (about 19 Mbit/s) however few of its
// programs are watched. Once the stream's tables are known, the masker tells
// the device, through the tuner's PID filter, to send only what somebody needs:
// the PAT and PMTs, the PSIP guide tables, and the audio and video of programs
// with viewers. A lingering stream with nobody on it drops to the tables alone.

// MaskGrace is how long a PID nobody wants any more keeps flowing, so that
// flipping back and forth does not make the filter flap.
var MaskGrace = 5 * time.Second

// maskMaxRanges bounds the filter we send; a more fragmented set is not worth
// the risk of upsetting the device and falls back to the whole multiplex.
const maskMaxRanges = 48

// kickMask asks the masker to look at the demand again.
func (st *stream) kickMask() {
	select {
	case st.maskKick <- struct{}{}:
	default:
	}
}

// masked reports whether this stream's PIDs may be filtered at the source.
func (st *stream) masked() bool {
	return st.h.cfg.Mask && !st.service && !st.unlocked && !st.h.isService(st.spec.Frequency)
}

// demand is the set of PIDs the stream must receive. Everything until the
// tables are known, and whenever somebody wants the whole multiplex.
func (st *stream) demand() ts.PIDSet {
	st.fmu.Lock()
	defer st.fmu.Unlock()
	full := ts.FullSet()
	if len(st.programs) == 0 {
		return full
	}
	var set ts.PIDSet
	set.Add(ts.PIDPAT)
	for _, pmtPID := range st.programs {
		set.Add(pmtPID)
	}
	set = set.Or(st.guidePIDs)
	for _, s := range *st.subs.Load() {
		switch s.mode {
		case modeAll:
			if s.active.Load() { // an unattached one is about to choose
				return full
			}
		case modePIDs:
			set = set.Or(s.pids)
		case modeProgram:
			if pmt := st.pmts[s.program]; pmt != nil {
				for _, pid := range pmt.PIDs() {
					set.Add(pid)
				}
			}
		}
	}
	if set.Ranges() > maskMaxRanges {
		return full
	}
	return set
}

// masker keeps the device's filter in step with demand. Additions are applied
// at once; removals only after MaskGrace.
func (st *stream) masker(grace time.Duration) {
	defer st.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lease *upstream.Lease
	applied := ts.FullSet() // what the device sends now
	var shrinkSince time.Time
	for {
		select {
		case <-st.stop:
			return
		case <-st.maskKick:
		case <-t.C:
		}
		if !st.masked() {
			continue
		}
		if l := st.currentLease(); l != lease {
			lease, applied, shrinkSince = l, ts.FullSet(), time.Time{} // a new session starts unfiltered
		}
		if lease == nil {
			continue
		}
		want := st.demand()
		if want == applied {
			shrinkSince = time.Time{}
			continue
		}
		next := want
		if !want.Minus(applied).Empty() {
			next = applied.Or(want) // new demand first; leftovers go later
		} else {
			if shrinkSince.IsZero() {
				shrinkSince = time.Now()
			}
			if time.Since(shrinkSince) < grace {
				continue
			}
		}
		if err := lease.SetFilter(next.String()); err != nil {
			if errors.Is(err, upstream.ErrMaskUnavailable) {
				log.Printf("hub: %s on %s: not masking (%v)", st.key, lease.ID(), err)
				return
			}
			log.Printf("hub: %s on %s: filter: %v", st.key, lease.ID(), err)
			continue
		}
		applied, shrinkSince = next, time.Time{}
	}
}
