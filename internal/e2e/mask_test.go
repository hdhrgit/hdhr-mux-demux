package e2e

import (
	"testing"
	"time"

	"hdhrmux/internal/hub"
	"hdhrmux/internal/ts"
)

// The device is asked for only the PIDs somebody watches (plus tables and
// guide): extra viewers widen the filter at once, leaving viewers narrow it
// again after a grace period, and letting go restores the tuner.
func TestMaskNarrowsTheStream(t *testing.T) {
	// restored by a cleanup registered before the hub's, so it runs after the streams stop
	m, l, g := testMask, testLinger, hub.MaskGrace
	t.Cleanup(func() { testMask, testLinger, hub.MaskGrace = m, l, g })
	testMask, testLinger, hub.MaskGrace = true, 3500*time.Millisecond, 200*time.Millisecond
	e := setup(t, []int{2}, 2, nil)

	has := func(s ts.PIDSet, pids ...uint16) bool {
		for _, p := range pids {
			if !s.Has(p) {
				return false
			}
		}
		return true
	}
	hasNone := func(s ts.PIDSet, pids ...uint16) bool {
		for _, p := range pids {
			if s.Has(p) {
				return false
			}
		}
		return true
	}

	c1, _ := e.open("/auto/v7.1") // program 1: PMT 0x100, video 0x101, audio 0x102
	if c1 == nil {
		t.Fatal("stream refused")
	}
	defer c1.close()
	waitFor(t, "filter narrowed to program 1", 5*time.Second, func() bool {
		f, locked := physFilter(e)
		return locked && has(f, 0, 0x100, 0x101, 0x102, 0x110, 0x1ffb) && hasNone(f, 0x111, 0x112)
	})
	f, _ := physFilter(e)
	t.Logf("filter with one viewer: %s", pidsOf(f))
	if a := analyze(t, c1.read(t, 188*400)); !a.esProgs[1] || len(a.esProgs) != 1 || a.patProgs[1] != 0x100 {
		t.Fatalf("viewer 1 content: programs %v pat %v", a.esProgs, a.patProgs)
	}

	// A second program of the same multiplex widens the filter.
	c2, _ := e.open("/auto/v7.2")
	if c2 == nil {
		t.Fatal("second viewer refused")
	}
	defer c2.close()
	waitFor(t, "filter widened to program 2", 5*time.Second, func() bool {
		f, _ := physFilter(e)
		return has(f, 0x101, 0x102, 0x111, 0x112)
	})
	if a := analyze(t, c2.read(t, 188*400)); !a.esProgs[2] || len(a.esProgs) != 1 {
		t.Fatalf("viewer 2 content: programs %v", a.esProgs)
	}
	if got := e.activePhysical(); got != 1 {
		t.Fatalf("%d physical tuners busy, want 1", got)
	}

	// Program 1's viewer leaves: after the grace period its PIDs stop flowing
	// while program 2 carries on.
	c1.close()
	waitFor(t, "program 1 dropped from the filter", 5*time.Second, func() bool {
		f, _ := physFilter(e)
		return hasNone(f, 0x101, 0x102) && has(f, 0x111, 0x112, 0x100, 0x110, 0)
	})
	if a := analyze(t, c2.read(t, 188*200)); !a.esProgs[2] || len(a.esProgs) != 1 {
		t.Fatalf("viewer 2 disturbed: %v", a.esProgs)
	}

	// Nobody watching (the stream lingers): tables and guide only.
	c2.close()
	waitFor(t, "tables only", 5*time.Second, func() bool {
		f, _ := physFilter(e)
		return hasNone(f, 0x101, 0x102, 0x111, 0x112) && has(f, 0, 0x100, 0x110, 0x1ffb)
	})

	// Linger over: the tuner is given back unfiltered and unlocked.
	waitFor(t, "released", 5*time.Second, func() bool { return e.activePhysical() == 0 })
	waitFor(t, "unlocked and unfiltered", 3*time.Second, func() bool {
		for i := 0; i < 2; i++ {
			f, _ := e.fakes[0].TunerFilter(i)
			if e.fakes[0].TunerLocked(i) || f != ts.FullSet() {
				return false
			}
		}
		return true
	})
}

// A viewer that wants the whole multiplex (no program) keeps the filter open.
func TestMaskStaysOpenForWholeMultiplex(t *testing.T) {
	m := testMask
	t.Cleanup(func() { testMask = m })
	testMask = true
	e := setup(t, []int{2}, 2, nil)
	c, _ := e.open("/auto/ch177000000")
	if c == nil {
		t.Fatal("stream refused")
	}
	defer c.close()
	time.Sleep(2500 * time.Millisecond) // several masker ticks
	f, _ := physFilter(e)
	if f != ts.FullSet() {
		t.Fatalf("filter narrowed for a whole-multiplex viewer: %s", pidsOf(f))
	}
	if a := analyze(t, c.read(t, 188*400)); a.pids[0x101] == 0 || a.pids[0x111] == 0 {
		t.Fatalf("whole multiplex viewer saw %v, want both programs", a.pids)
	}
}

// Masking can be switched off: the device then keeps its own lock.
func TestMaskOff(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	c, _ := e.open("/auto/v7.1")
	if c == nil {
		t.Fatal("stream refused")
	}
	defer c.close()
	time.Sleep(2500 * time.Millisecond)
	f, keyed := physFilter(e)
	if f != ts.FullSet() || keyed {
		t.Fatalf("masking happened although off: filter %s keyed %v", pidsOf(f), keyed)
	}
}

// With masking on, the guide of the programs nobody watches is still
// collected, and after a failover the new session is narrowed again.
func TestMaskKeepsGuideAndSurvivesFailover(t *testing.T) {
	m, l, g, sl := testMask, testLinger, hub.MaskGrace, hub.StallLimit
	t.Cleanup(func() { testMask, testLinger, hub.MaskGrace, hub.StallLimit = m, l, g, sl })
	testMask, hub.MaskGrace, hub.StallLimit = true, 200*time.Millisecond, 1500*time.Millisecond
	e := setup(t, []int{2}, 1, nil)
	c, _ := e.open("/auto/v7.1")
	if c == nil {
		t.Fatal("stream refused")
	}
	defer c.close()
	c.read(t, 188*50)
	waitFor(t, "narrowed", 5*time.Second, func() bool {
		f, _ := physFilter(e)
		return !f.Has(0x112) && f.Has(0x102)
	})
	waitFor(t, "guide of the unwatched program", 10*time.Second, func() bool {
		return len(fetchEPG(t, e, "?channel=7.2").Programmes) >= 2
	})

	e.fakes[0].KillStreams()
	time.Sleep(200 * time.Millisecond)
loop:
	for {
		select {
		case <-c.data:
		default:
			break loop
		}
	}
	if a := analyze(t, c.read(t, 188*100)); !a.esProgs[1] {
		t.Fatalf("no data after failover: %v", a.pids)
	}
	waitFor(t, "narrowed again after failover", 5*time.Second, func() bool {
		f, keyed := physFilter(e)
		return keyed && !f.Has(0x112) && f.Has(0x102)
	})
	c.close()
	waitFor(t, "released", 5*time.Second, func() bool { return e.activePhysical() == 0 })
}
