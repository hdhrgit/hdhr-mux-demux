package vdev

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"hdhrmux/internal/hdhrproto"
)

// Metrics in the Prometheus text exposition format on GET /metrics, for
// Prometheus, Netdata, Telegraf, VictoriaMetrics and the like.

var processStart = time.Now()

var (
	respMu     sync.Mutex
	respCounts = map[string]uint64{}
)

// countStreamResponse tallies the answers given to stream requests: 200, or
// the HDHomeRun error ("804", "805", ...), or the bare HTTP status.
func countStreamResponse(status int, hdhr string) {
	code := strconv.Itoa(status)
	if f := strings.Fields(hdhr); len(f) > 0 {
		code = f[0]
	}
	respMu.Lock()
	respCounts[code]++
	respMu.Unlock()
}

type promWriter struct {
	w    *bufio.Writer
	seen map[string]bool
}

// sample writes one sample; kv are label name/value pairs. The HELP and TYPE
// lines go out before the first sample of each metric.
func (p *promWriter) sample(name, typ, help string, v float64, kv ...string) {
	if !p.seen[name] {
		p.seen[name] = true
		fmt.Fprintf(p.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	p.w.WriteString(name)
	if len(kv) > 0 {
		p.w.WriteByte('{')
		for i := 0; i+1 < len(kv); i += 2 {
			if i > 0 {
				p.w.WriteByte(',')
			}
			fmt.Fprintf(p.w, "%s=%q", kv[i], kv[i+1])
		}
		p.w.WriteByte('}')
	}
	p.w.WriteByte(' ')
	p.w.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	p.w.WriteByte('\n')
}

func (p *promWriter) gauge(name, help string, v float64, kv ...string) {
	p.sample(name, "gauge", help, v, kv...)
}

func (p *promWriter) counter(name, help string, v float64, kv ...string) {
	p.sample(name, "counter", help, v, kv...)
}

func (d *Device) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	p := &promWriter{w: bufio.NewWriter(w), seen: map[string]bool{}}
	defer p.w.Flush()
	d.writeMetrics(p)
}

func (d *Device) writeMetrics(p *promWriter) {
	pool := d.hub.Pool()
	id := hdhrproto.FormatDeviceID(d.cfg.DeviceID)

	p.gauge("hdhrmux_info", "The multiplexer; always 1.", 1, "device_id", id, "friendly_name", d.cfg.FriendlyName, "version", d.cfg.Version)

	// physical tuners
	total := pool.TotalTuners()
	p.gauge("hdhrmux_physical_tuners", "Physical tuners in the numbering.", float64(total))
	p.gauge("hdhrmux_physical_tuners_online", "Physical tuners whose device is reachable.", float64(pool.TunerCount()))
	p.gauge("hdhrmux_virtual_tuners", "Tuners the virtual device presents.", float64(len(d.tuners)))
	busy := 0
	for g := 0; g < total; g++ {
		ref, ok := pool.Resolve(g)
		if !ok {
			continue
		}
		name := fmt.Sprintf("%s/%d", hdhrproto.FormatDeviceID(ref.DeviceID), ref.Index)
		held := pool.HeldByUs(ref.DeviceID, ref.Index)
		if held {
			busy++
		}
		p.gauge("hdhrmux_tuner_online", "1 if the physical tuner's device is reachable.", b2f(ref.Device != nil), "tuner", name, "global", strconv.Itoa(g))
		p.gauge("hdhrmux_tuner_in_use", "1 if the multiplexer holds the physical tuner.", b2f(held), "tuner", name, "global", strconv.Itoa(g))
	}
	p.gauge("hdhrmux_physical_tuners_in_use", "Physical tuners the multiplexer holds.", float64(busy))

	// streams: one per tuned RF channel (or ATSC 3.0 service)
	streams := d.hub.Streams()
	sort.Slice(streams, func(i, j int) bool { return streams[i].TunerID < streams[j].TunerID })
	for _, s := range streams {
		lb := []string{"tuner", s.TunerID, "channel", strconv.FormatUint(s.Spec.Frequency, 10)}
		p.gauge("hdhrmux_stream_viewers", "Subscriptions attached to the stream.", float64(s.Subs), lb...)
		p.counter("hdhrmux_stream_received_bytes_total", "Bytes pulled from the physical tuner (resets when the stream restarts).", float64(s.RxBytes), lb...)
		p.gauge("hdhrmux_stream_received_bits_per_second", "Bitrate pulled from the physical tuner.", float64(s.RxBPS), lb...)
		p.counter("hdhrmux_stream_lost_packets_total", "Packets lost in transit from the device (RTP mode only).", float64(s.Lost), lb...)
		p.gauge("hdhrmux_stream_signal_strength_percent", "Signal strength reported by the tuner.", float64(s.Status.SS), lb...)
		p.gauge("hdhrmux_stream_signal_quality_percent", "Signal-to-noise quality reported by the tuner.", float64(s.Status.SNQ), lb...)
		p.gauge("hdhrmux_stream_symbol_quality_percent", "Symbol error quality reported by the tuner.", float64(s.Status.SEQ), lb...)
	}

	// viewers
	viewers, queueMax := 0, 0
	var outBPS int
	for _, v := range d.tuners {
		rows := v.viewerRows()
		viewers += len(rows)
		for _, r := range rows {
			outBPS += r.BPS
			if r.Queue > queueMax {
				queueMax = r.Queue
			}
		}
		p.gauge("hdhrmux_virtual_tuner_viewers", "Clients on the virtual tuner.", float64(len(rows)), "tuner", strconv.Itoa(v.idx))
	}
	p.gauge("hdhrmux_viewers", "Clients watching.", float64(viewers))
	p.gauge("hdhrmux_viewers_sent_bits_per_second", "Total bitrate delivered to clients.", float64(outBPS))
	p.gauge("hdhrmux_viewer_buffer_max_percent", "Fullest client buffer.", float64(queueMax))

	st := d.hub.Stats()
	p.counter("hdhrmux_streams_opened_total", "Physical tunings started.", float64(st.StreamsOpened))
	p.counter("hdhrmux_stream_failovers_total", "Streams re-tuned or moved after a failure.", float64(st.Failovers))
	p.counter("hdhrmux_filter_updates_total", "PID filter changes sent to devices.", float64(st.MaskUpdates))
	p.counter("hdhrmux_idle_streams_reclaimed_total", "Lingering streams given up to make room for another.", float64(st.Reclaims))
	p.counter("hdhrmux_dropped_batches_total", "Batches of packets dropped because a client was too slow.", float64(st.DroppedBatches))

	respMu.Lock()
	codes := make([]string, 0, len(respCounts))
	for c := range respCounts {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		p.counter("hdhrmux_stream_requests_total", "Stream requests by answer (200, or the HDHomeRun/HTTP error).", float64(respCounts[c]), "code", c)
	}
	respMu.Unlock()

	// lineup and guide
	p.gauge("hdhrmux_lineup_channels", "Channels in the lineup.", float64(len(d.lineup.Entries(false))))
	d.guide.mu.Lock()
	store, last, running := d.guide.store, d.guide.last, d.guide.running
	d.guide.mu.Unlock()
	if store != nil {
		ch, ev := store.Summary()
		p.gauge("hdhrmux_guide_channels", "Channels with guide data.", float64(ch))
		p.gauge("hdhrmux_guide_events", "Programme events held.", float64(ev))
		p.gauge("hdhrmux_guide_pass_running", "1 while a guide pass is running.", b2f(running))
		if !last.IsZero() {
			p.gauge("hdhrmux_guide_last_pass_timestamp_seconds", "When the last guide pass ended.", float64(last.Unix()))
		}
	}
	p.gauge("hdhrmux_scan_running", "1 while a channel scan is running.", b2f(d.scanRunning()))

	// the process
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	cpu := float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
	p.counter("process_cpu_seconds_total", "User and system CPU time.", cpu)
	p.gauge("process_start_time_seconds", "Start time since the epoch.", float64(processStart.Unix()))
	p.gauge("process_resident_memory_bytes", "Resident memory.", residentBytes())
	if n, err := openFDs(); err == nil {
		p.gauge("process_open_fds", "Open file descriptors.", float64(n))
	}
	p.gauge("go_goroutines", "Goroutines.", float64(runtime.NumGoroutine()))
	p.gauge("go_memstats_heap_alloc_bytes", "Heap bytes in use.", float64(ms.HeapAlloc))
	p.counter("go_gc_cycles_total", "Completed GC cycles.", float64(ms.NumGC))
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func residentBytes() float64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.ParseFloat(f[1], 64)
	return n * float64(os.Getpagesize())
}

func openFDs() (int, error) {
	es, err := os.ReadDir("/proc/self/fd")
	return len(es), err
}

func (d *Device) scanRunning() bool {
	d.scanMu.Lock()
	defer d.scanMu.Unlock()
	return d.scanning
}

func resetStreamCounts() {
	respMu.Lock()
	respCounts = map[string]uint64{}
	respMu.Unlock()
}
