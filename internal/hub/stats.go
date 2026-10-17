package hub

import "sync/atomic"

// counters are monotonic totals for monitoring.
type counters struct {
	opened      atomic.Uint64 // streams (physical tunings) started
	failovers   atomic.Uint64 // streams moved or re-tuned after a failure
	maskUpdates atomic.Uint64 // PID filter changes sent to devices
	reclaims    atomic.Uint64 // idle streams given up to make room
	dropped     atomic.Uint64 // batches dropped for slow consumers, from closed subscriptions
}

// Stats is a snapshot of the hub for monitoring.
type Stats struct {
	StreamsOpened  uint64
	Failovers      uint64
	MaskUpdates    uint64
	Reclaims       uint64
	DroppedBatches uint64 // all subscriptions, past and present
	Streams        int
	Viewers        int
}

// Stats returns the hub's counters and current sizes.
func (h *Hub) Stats() Stats {
	st := Stats{
		StreamsOpened: h.stats.opened.Load(), Failovers: h.stats.failovers.Load(),
		MaskUpdates: h.stats.maskUpdates.Load(), Reclaims: h.stats.reclaims.Load(),
		DroppedBatches: h.stats.dropped.Load(),
	}
	h.mu.Lock()
	list := make([]*stream, 0, len(h.streams))
	for _, s := range h.streams {
		list = append(list, s)
	}
	h.mu.Unlock()
	st.Streams = len(list)
	for _, s := range list {
		subs := *s.subs.Load()
		st.Viewers += len(subs)
		for _, sub := range subs {
			st.DroppedBatches += sub.drops.Load()
		}
	}
	return st
}
