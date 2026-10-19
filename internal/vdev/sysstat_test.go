package vdev

import (
	"testing"
	"time"
)

func TestSystemCPUOverTheSamples(t *testing.T) {
	t0 := time.Now().Add(-60 * time.Second)
	var s uiSystem
	s.cpuOver([]sysSample{{t0, 100}, {t0.Add(60 * time.Second), 130}}) // 30 CPU-seconds in 60 s
	if s.CPUPercent < 49 || s.CPUPercent > 51 {
		t.Errorf("cpu %.1f%%, want 50%% of one core", s.CPUPercent)
	}
	s = uiSystem{}
	s.cpuOver(nil)
	s.cpuOver([]sysSample{{t0, 1}})
	if s.CPUPercent != 0 {
		t.Errorf("cpu %.1f with fewer than two samples", s.CPUPercent)
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
