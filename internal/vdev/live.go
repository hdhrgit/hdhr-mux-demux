package vdev

import (
	"sync"
	"sync/atomic"
	"time"

	"hdhrmux/internal/config"
)

// Live holds the settings that can change while the multiplexer runs (the
// settings page applies them without a restart). Everything else in Config is
// read once, at start.
type Live struct {
	AutoJoinDirect         bool
	AutoJoinOtherLocations bool
	EvictScans             bool
	StreamTTL              int
	ScanMode               string
	ScanTuners             int
	LineupRefresh          time.Duration
	GuideRefresh           time.Duration
	GuideHold              time.Duration
	GuideParallel          int
	GuideUnlocked          bool
	IdleRelease            time.Duration
	LogRequests            bool
}

func liveOf(c *Config) Live {
	return Live{
		AutoJoinDirect: c.AutoJoinDirect, AutoJoinOtherLocations: c.AutoJoinOtherLocations, EvictScans: c.EvictScans,
		StreamTTL: c.StreamTTL, ScanMode: c.ScanMode, ScanTuners: c.ScanTuners,
		LineupRefresh: c.LineupRefresh, GuideRefresh: c.GuideRefresh, GuideHold: c.GuideHold,
		GuideParallel: c.GuideParallel, GuideUnlocked: c.GuideUnlocked, IdleRelease: c.IdleRelease,
		LogRequests: c.LogRequests,
	}
}

// liveFromConfig is the same for a freshly loaded configuration file.
func liveFromConfig(c *config.Config) Live {
	return Live{
		AutoJoinDirect: *c.AutoJoinDirect, AutoJoinOtherLocations: *c.AutoJoinOtherLocations, EvictScans: *c.EvictScans,
		StreamTTL: *c.StreamTTL, ScanMode: c.Lineup.ScanMode, ScanTuners: c.Lineup.ScanTuners,
		LineupRefresh: time.Duration(c.Lineup.Refresh), GuideRefresh: time.Duration(c.Guide.Refresh),
		GuideHold: time.Duration(c.Guide.Hold), GuideParallel: c.Guide.Parallel, GuideUnlocked: c.Guide.Unlocked,
		IdleRelease: time.Duration(c.IdleRelease), LogRequests: c.LogRequests,
	}
}

// liveState is the current Live settings and a channel closed (and replaced)
// whenever they change, which wakes the loops that wait on timers.
type liveState struct {
	cur atomic.Pointer[Live]
	mu  sync.Mutex
	ch  chan struct{}
}

func (s *liveState) init(l Live) { s.cur.Store(&l); s.ch = make(chan struct{}) }

// lv returns the settings in force now.
func (d *Device) lv() *Live { return d.live.cur.Load() }

// changed returns a channel that is closed when the live settings next change.
func (d *Device) changed() <-chan struct{} {
	d.live.mu.Lock()
	defer d.live.mu.Unlock()
	return d.live.ch
}

// Reconfigure applies new live settings.
func (d *Device) Reconfigure(l Live) {
	d.live.cur.Store(&l)
	d.live.mu.Lock()
	close(d.live.ch)
	d.live.ch = make(chan struct{})
	d.live.mu.Unlock()
}

// ApplyConfig applies the live part of a freshly loaded configuration, here and
// in the hub.
func (d *Device) ApplyConfig(c *config.Config) {
	d.hub.SetLinger(time.Duration(c.Linger))
	d.hub.SetClientBuffer(c.ClientBufferMB << 20)
	d.hub.SetSignalSettle(time.Duration(c.SignalSettle))
	d.lineup.SetHideATSC3(!*c.Lineup.ATSC3)
	d.Reconfigure(liveFromConfig(c))
}
