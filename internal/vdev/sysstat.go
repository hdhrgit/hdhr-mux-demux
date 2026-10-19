package vdev

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// uiSystem is how the multiplexer itself is doing, for the activity page: on a
// small machine (a Raspberry Pi) CPU, memory and heat decide whether video stutters.
type uiSystem struct {
	CPUPercent    float64 `json:"cpuPercent"`              // this process, of one core, over the last minute
	Cores         int     `json:"cores"`                   //
	RSSBytes      uint64  `json:"rssBytes"`                // resident memory
	MemLimitBytes uint64  `json:"memLimitBytes,omitempty"` // container or service limit, if any
	Goroutines    int     `json:"goroutines"`
	OpenFDs       int     `json:"openFds"`
	Load1         float64 `json:"load1,omitempty"` // host load average
	Load5         float64 `json:"load5,omitempty"`
	TempC         float64 `json:"tempC,omitempty"`     // CPU temperature, where the host reports it
	Throttled     bool    `json:"throttled,omitempty"` // a Raspberry Pi is under-voltage or throttling now
}

type sysSample struct {
	at  time.Time
	cpu float64 // process CPU seconds
}

// sysSampler remembers the last minute of CPU use, one sample every 5 s.
type sysSampler struct {
	mu      sync.Mutex
	samples []sysSample
}

const (
	sysEvery = 5 * time.Second
	sysKeep  = 13 // a minute and a bit
)

func processCPUSeconds() float64 {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

func (s *sysSampler) add(smp sysSample) {
	s.mu.Lock()
	s.samples = append(s.samples, smp)
	if len(s.samples) > sysKeep {
		s.samples = s.samples[len(s.samples)-sysKeep:]
	}
	s.mu.Unlock()
}

// sysLoop takes a sample every few seconds while the device runs.
func (d *Device) sysLoop() {
	defer d.wg.Done()
	d.sys.add(sysSample{time.Now(), processCPUSeconds()})
	t := time.NewTicker(sysEvery)
	defer t.Stop()
	for {
		select {
		case <-d.quit:
			return
		case <-t.C:
			d.sys.add(sysSample{time.Now(), processCPUSeconds()})
		}
	}
}

// system describes the process and the machine now. It only reports: what to make of
// the figures is for the person running it (the same data is in /metrics).
func (d *Device) system() uiSystem {
	s := uiSystem{Cores: runtime.NumCPU(), Goroutines: runtime.NumGoroutine(), RSSBytes: uint64(residentBytes())}
	if n, err := openFDs(); err == nil {
		s.OpenFDs = n
	}
	d.sys.mu.Lock()
	smp := append([]sysSample(nil), d.sys.samples...)
	d.sys.mu.Unlock()
	s.MemLimitBytes = memoryLimit()
	s.Load1, s.Load5 = loadAverages()
	s.TempC, s.Throttled = piHealth()
	s.cpuOver(smp)
	return s
}

// cpuOver works out the process's CPU use over the samples.
func (s *uiSystem) cpuOver(smp []sysSample) {
	if len(smp) < 2 {
		return
	}
	first, last := smp[0], smp[len(smp)-1]
	if dt := last.at.Sub(first.at).Seconds(); dt > 0 {
		s.CPUPercent = (last.cpu - first.cpu) / dt * 100
	}
}

func loadAverages() (l1, l5 float64) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0
	}
	f := strings.Fields(string(b))
	if len(f) >= 2 {
		l1, _ = strconv.ParseFloat(f[0], 64)
		l5, _ = strconv.ParseFloat(f[1], 64)
	}
	return l1, l5
}

// memoryLimit is the container's or service's memory limit (cgroup), 0 if none.
func memoryLimit() uint64 {
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if v == "max" {
			return 0
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n >= 1<<60 { // "no limit" is a huge number in cgroup v1
			return 0
		}
		return n
	}
	return 0
}

// piHealth reads the CPU temperature and, on a Raspberry Pi, whether the firmware
// says it is under-voltage or throttled right now.
func piHealth() (tempC float64, throttled bool) {
	if b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if n, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			tempC = n / 1000
		}
	}
	if b, err := os.ReadFile("/sys/devices/platform/soc/soc:firmware/get_throttled"); err == nil {
		if n, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x")), 16, 32); err == nil {
			throttled = n&0x5 != 0 // bit 0: under-voltage now, bit 2: throttled now
		}
	}
	return tempC, throttled
}
