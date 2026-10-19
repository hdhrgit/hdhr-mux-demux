package vdev

import (
	"strings"
	"testing"
	"time"
)

func samples(cpuSecondsPerSecond float64, droppedFirst, droppedLast uint64) []sysSample {
	t0 := time.Now().Add(-60 * time.Second)
	return []sysSample{
		{t0, 100, droppedFirst},
		{t0.Add(60 * time.Second), 100 + 60*cpuSecondsPerSecond, droppedLast},
	}
}

func TestSystemWarnings(t *testing.T) {
	has := func(s uiSystem, sub string) bool {
		for _, w := range s.Warnings {
			if strings.Contains(w, sub) {
				return true
			}
		}
		return false
	}
	// quiet: half a core of four, nothing dropped, cool
	s := uiSystem{Cores: 4, Load1: 0.5, RSSBytes: 80 << 20, TempC: 55}
	s.assess(samples(0.5, 7, 7), 7)
	if len(s.Warnings) != 0 || s.CPUPercent < 49 || s.CPUPercent > 51 {
		t.Fatalf("quiet machine: cpu %.1f warnings %v", s.CPUPercent, s.Warnings)
	}
	// 3.4 of 4 cores: 85% of the machine
	s = uiSystem{Cores: 4}
	s.assess(samples(3.4, 0, 0), 0)
	if !has(s, "85% of a 4-core") {
		t.Errorf("busy CPU: %.0f%% %v", s.CPUPercent, s.Warnings)
	}
	// dropped batches in the last minute
	s = uiSystem{Cores: 4}
	s.assess(samples(0.1, 10, 25), 25)
	if !has(s, "15 batches were dropped") {
		t.Errorf("dropped: %v", s.Warnings)
	}
	// overloaded host, hot CPU, throttled Pi, memory near its limit
	s = uiSystem{Cores: 4, Load1: 9, TempC: 83, Throttled: true, RSSBytes: 90 << 20, MemLimitBytes: 100 << 20}
	s.assess(samples(0.1, 0, 0), 0)
	for _, want := range []string{"overloaded", "hot (83", "under-voltage", "close to its limit"} {
		if !has(s, want) {
			t.Errorf("missing warning %q in %v", want, s.Warnings)
		}
	}
	// the sampler keeps about a minute
	var sm sysSampler
	for i := 0; i < 40; i++ {
		sm.add(sysSample{at: time.Now()})
	}
	if len(sm.samples) != sysKeep {
		t.Errorf("kept %d samples, want %d", len(sm.samples), sysKeep)
	}
}
