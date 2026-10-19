// Command loadtest looks for leaks and stalls in the multiplexer under churn.
//
//	loadtest serve              run a multiplexer against simulated devices
//	loadtest swarm -url URL     start many viewers that keep switching channels
//
// serve needs no hardware: it runs several simulated HomeRun devices (ATSC 1.0
// and 3.0 multiplexes) and the multiplexer in one process, with the web port,
// streaming port and metrics on the given addresses. swarm opens N viewers,
// each watching a random channel for a random time before picking another, a
// few of them too slow to keep up, and every minute reads /metrics to follow the
// multiplexer's memory, goroutines and file descriptors. At the end it reports
// whether any of them kept growing.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hdhrmux/internal/fakehdhr"
	"hdhrmux/internal/guide"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/upstream"
	"hdhrmux/internal/vdev"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: loadtest serve|swarm [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "swarm":
		swarm(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: loadtest serve|swarm [flags]")
		os.Exit(2)
	}
}

// ---- serve ----------------------------------------------------------------

func prog(n uint16, vch, name string) fakehdhr.Program {
	return fakehdhr.Program{Number: n, VChannel: vch, Name: name}
}

func channels() []fakehdhr.Channel {
	var out []fakehdhr.Channel
	freq := uint64(177000000)
	for i := 0; i < 8; i++ { // eight ATSC 1.0 multiplexes of three programmes
		var ps []fakehdhr.Program
		for p := 1; p <= 3; p++ {
			ps = append(ps, prog(uint16(p), fmt.Sprintf("%d.%d", 7+i*2, p), fmt.Sprintf("CH%d-%d", 7+i*2, p)))
		}
		out = append(out, fakehdhr.Channel{Frequency: freq, TSID: uint16(i + 1), Programs: ps})
		freq += 6000000
	}
	// two ATSC 3.0 multiplexes: one service per tuner, only tuners 0 and 1 lock
	for i := 0; i < 2; i++ {
		var ps []fakehdhr.Program
		for p := 0; p < 3; p++ {
			ps = append(ps, prog(uint16(1376+p), fmt.Sprintf("%d.%d", 104+i*10, p+1), fmt.Sprintf("NEXT%d-%d", i, p)))
		}
		out = append(out, fakehdhr.Channel{Frequency: 605000000 + uint64(i)*6000000, TSID: uint16(100 + i), ATSC3: true, Programs: ps})
	}
	return out
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	httpAddr := fs.String("http", "127.0.0.1:18080", "web, API and metrics address")
	streamAddr := fs.String("stream", "127.0.0.1:15004", "streaming address")
	devices := fs.Int("devices", 3, "simulated devices (4 tuners each)")
	bitrate := fs.Int("bitrate", 6_000_000, "bits per second of each active tuner")
	linger := fs.Duration("linger", 5*time.Second, "how long an idle tuner stays tuned")
	pprof := fs.String("pprof", "127.0.0.1:6061", "Go profiling address (\"\" = off)")
	fs.Parse(args)
	if *pprof != "" {
		go func() { log.Printf("pprof: %v", http.ListenAndServe(*pprof, nil)) }()
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	chs := channels()
	var addrs []string
	var fakes []*fakehdhr.Device
	for i := 0; i < *devices; i++ {
		f, err := fakehdhr.Start(fakehdhr.Config{DeviceID: hdhrproto.MakeDeviceID(0x10200000 + uint32(i)<<8), Tuners: 4, Channels: chs, Bitrate: *bitrate})
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		fakes = append(fakes, f)
		addrs = append(addrs, f.Addr())
	}
	pool, err := upstream.NewPool(upstream.Config{Devices: addrs, WaitFree: 2 * time.Second})
	if err != nil {
		log.Fatal(err)
	}
	pool.Refresh()
	for _, f := range fakes {
		pool.SetStreamPort(f.ID(), f.StreamPort())
		pool.SetWebPort(f.ID(), f.WebPort())
	}
	h := hub.New(pool, hub.Config{Linger: *linger, Mask: true})
	defer h.Close()
	lu, _ := lineup.Open("")
	var entries []lineup.Entry
	for _, c := range chs {
		for _, p := range c.Programs {
			e := lineup.Entry{GuideNumber: p.VChannel, GuideName: p.Name, Frequency: c.Frequency, Program: p.Number, VideoCodec: "MPEG2", AudioCodec: "AC3", HD: true}
			if c.ATSC3 {
				e.Modulation = "atsc3"
			}
			entries = append(entries, e)
		}
	}
	lu.Replace(entries)
	dev, err := vdev.New(vdev.Config{DeviceID: hdhrproto.MakeDeviceID(0x10900000), FriendlyName: "loadtest", Tuners: pool.TotalTuners(),
		HTTPAddr: *httpAddr, StreamAddr: *streamAddr, ControlAddr: "127.0.0.1:0"}, h, lu)
	if err != nil {
		log.Fatal(err)
	}
	gs, _ := guide.Open("")
	dev.SetGuide(gs)
	if err := dev.Start(); err != nil {
		log.Fatal(err)
	}
	defer dev.Close()
	log.Printf("loadtest server up: web http://%s  streams http://%s  (%d simulated tuners, %d channels)", *httpAddr, *streamAddr, pool.TotalTuners(), len(entries))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}

// ---- swarm ----------------------------------------------------------------

type stats struct {
	started, ended atomic.Uint64
	refused        atomic.Uint64 // 804/805 and friends
	errors         atomic.Uint64 // connection or read errors
	bytes          atomic.Uint64
	stalls         atomic.Uint64 // a viewer that got nothing for 10 s
	byStatus       sync.Map      // string -> *atomic.Uint64
	watching       atomic.Int64
}

func (s *stats) count(code string) {
	v, _ := s.byStatus.LoadOrStore(code, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

type sample struct {
	t                       time.Duration
	rss, heap               float64 // MB
	goroutines, fds         float64
	viewers                 float64
	opened, dropped, failov float64
}

func scrape(base string) (map[string]float64, error) {
	c := &http.Client{Timeout: 10 * time.Second}
	r, err := c.Get(base + "/metrics")
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	out := map[string]float64{}
	sc := bufio.NewScanner(r.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err == nil {
			out[line[:i]] = v
		}
	}
	return out, sc.Err()
}

// slope fits y = a + b*x by least squares and returns b.
func slope(xs, ys []float64) float64 {
	n := float64(len(xs))
	if n < 3 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / d
}

func swarm(args []string) {
	fs := flag.NewFlagSet("swarm", flag.ExitOnError)
	web := fs.String("url", "http://127.0.0.1:18080", "web/metrics base URL of the multiplexer")
	stream := fs.String("stream", "http://127.0.0.1:15004", "streaming base URL")
	n := fs.Int("viewers", 100, "simultaneous viewers")
	dur := fs.Duration("duration", time.Hour, "how long to run")
	slow := fs.Float64("slow", 0.05, "fraction of viewers too slow to keep up")
	minWatch := fs.Duration("min", 8*time.Second, "shortest time on a channel")
	maxWatch := fs.Duration("max", 150*time.Second, "longest time on a channel")
	every := fs.Duration("sample", time.Minute, "how often to sample the multiplexer's metrics")
	csv := fs.String("csv", "", "write the samples to this CSV file")
	fs.Parse(args)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	// the channel list comes from the multiplexer itself
	r, err := http.Get(*web + "/lineup.json")
	if err != nil {
		log.Fatal(err)
	}
	var lu []struct{ GuideNumber string }
	json.NewDecoder(r.Body).Decode(&lu)
	r.Body.Close()
	if len(lu) == 0 {
		log.Fatal("empty lineup")
	}
	log.Printf("%d channels, %d viewers for %v", len(lu), *n, *dur)

	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; cancel() }()

	var st stats
	var wg sync.WaitGroup
	for i := 0; i < *n; i++ {
		wg.Add(1)
		slowViewer := float64(i) < *slow*float64(*n)
		go func(i int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(i) + time.Now().UnixNano()))
			time.Sleep(time.Duration(rng.Intn(10000)) * time.Millisecond) // do not all start at once
			ip := net.ParseIP(fmt.Sprintf("127.0.%d.%d", 1+i/250, 2+i%250))
			cl := &http.Client{Transport: &http.Transport{
				DialContext:       (&net.Dialer{LocalAddr: &net.TCPAddr{IP: ip}, Timeout: 10 * time.Second}).DialContext,
				DisableKeepAlives: true,
			}}
			for ctx.Err() == nil {
				ch := lu[rng.Intn(len(lu))].GuideNumber
				watch := *minWatch + time.Duration(rng.Int63n(int64(*maxWatch-*minWatch)))
				view(ctx, cl, *stream+"/auto/v"+ch, watch, slowViewer, &st)
			}
		}(i)
	}

	var samples []sample
	t0 := time.Now()
	tick := time.NewTicker(*every)
	defer tick.Stop()
	var f *os.File
	if *csv != "" {
		f, _ = os.Create(*csv)
		defer f.Close()
		fmt.Fprintln(f, "minute,rss_mb,heap_mb,goroutines,fds,viewers,tunings,dropped_batches,failovers,client_mb,refused,errors,stalls")
	}
	report := func() {
		m, err := scrape(*web)
		if err != nil {
			log.Printf("metrics: %v", err)
			return
		}
		s := sample{t: time.Since(t0), rss: m["process_resident_memory_bytes"] / 1e6, heap: m["go_memstats_heap_alloc_bytes"] / 1e6,
			goroutines: m["go_goroutines"], fds: m["process_open_fds"], viewers: m["hdhrmux_viewers"],
			opened: m["hdhrmux_streams_opened_total"], dropped: m["hdhrmux_dropped_batches_total"], failov: m["hdhrmux_stream_failovers_total"]}
		samples = append(samples, s)
		log.Printf("t=%3.0fm rss=%6.1fMB heap=%6.1fMB goroutines=%4.0f fds=%4.0f viewers=%3.0f tunings=%5.0f dropped=%5.0f failovers=%2.0f | clients: %d watching, %d sessions, %.0f MB, %d refused, %d errors, %d stalls",
			s.t.Minutes(), s.rss, s.heap, s.goroutines, s.fds, s.viewers, s.opened, s.dropped, s.failov, st.watching.Load(), st.ended.Load(), float64(st.bytes.Load())/1e6, st.refused.Load(), st.errors.Load(), st.stalls.Load())
		if f != nil {
			fmt.Fprintf(f, "%.1f,%.1f,%.1f,%.0f,%.0f,%.0f,%.0f,%.0f,%.0f,%.0f,%d,%d,%d\n", s.t.Minutes(), s.rss, s.heap, s.goroutines, s.fds, s.viewers, s.opened, s.dropped, s.failov,
				float64(st.bytes.Load())/1e6, st.refused.Load(), st.errors.Load(), st.stalls.Load())
		}
	}
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
			report()
		}
	}
	report()
	wg.Wait()
	finish(samples, &st)
}

// view watches one channel for the given time. A slow viewer reads about a
// fifth of what a channel carries.
func view(ctx context.Context, cl *http.Client, url string, watch time.Duration, slow bool, st *stats) {
	vctx, cancel := context.WithTimeout(ctx, watch)
	defer cancel()
	req, _ := http.NewRequestWithContext(vctx, "GET", url, nil)
	resp, err := cl.Do(req)
	if err != nil {
		if ctx.Err() == nil && vctx.Err() == nil {
			st.errors.Add(1)
			time.Sleep(time.Second)
		}
		return
	}
	defer resp.Body.Close()
	st.count(strconv.Itoa(resp.StatusCode))
	if resp.StatusCode != 200 {
		st.refused.Add(1)
		time.Sleep(2 * time.Second)
		return
	}
	st.started.Add(1)
	st.watching.Add(1)
	defer func() { st.watching.Add(-1); st.ended.Add(1) }()
	buf := make([]byte, 64<<10)
	last := time.Now()
	got := false
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			st.bytes.Add(uint64(n))
			last, got = time.Now(), true
		}
		if err != nil {
			if vctx.Err() == nil && ctx.Err() == nil && err != io.EOF {
				st.errors.Add(1)
			}
			return
		}
		if !got && time.Since(last) > 10*time.Second {
			st.stalls.Add(1)
			return
		}
		if slow {
			time.Sleep(250 * time.Millisecond)
		}
	}
}

func finish(samples []sample, st *stats) {
	fmt.Println()
	fmt.Printf("sessions: %d started, %d ended; %d refused, %d errors, %d stalled; %.1f GB received\n",
		st.started.Load(), st.ended.Load(), st.refused.Load(), st.errors.Load(), st.stalls.Load(), float64(st.bytes.Load())/1e9)
	st.byStatus.Range(func(k, v any) bool {
		fmt.Printf("  HTTP %s: %d\n", k, v.(*atomic.Uint64).Load())
		return true
	})
	if len(samples) < 6 {
		fmt.Println("too few samples to judge growth")
		return
	}
	// judge growth on the second half, after start-up effects have settled
	half := samples[len(samples)/2:]
	hours := func(s sample) float64 { return s.t.Hours() }
	var xs []float64
	for _, s := range half {
		xs = append(xs, hours(s))
	}
	col := func(f func(sample) float64) []float64 {
		var ys []float64
		for _, s := range half {
			ys = append(ys, f(s))
		}
		return ys
	}
	type metric struct {
		name string
		unit string
		f    func(sample) float64
		tol  float64 // growth per hour that still counts as flat
	}
	first, last := samples[0], samples[len(samples)-1]
	fmt.Printf("\n%-12s %10s %10s %10s %14s  verdict\n", "", "first", "last", "max", "trend/hour")
	leak := false
	for _, m := range []metric{
		{"RSS", "MB", func(s sample) float64 { return s.rss }, 10},
		{"heap", "MB", func(s sample) float64 { return s.heap }, 5},
		{"goroutines", "", func(s sample) float64 { return s.goroutines }, 20},
		{"open fds", "", func(s sample) float64 { return s.fds }, 10},
	} {
		mx := 0.0
		for _, s := range samples {
			mx = math.Max(mx, m.f(s))
		}
		b := slope(xs, col(m.f))
		verdict := "flat"
		if b > m.tol {
			verdict = "GROWING"
			leak = true
		}
		fmt.Printf("%-12s %10.1f %10.1f %10.1f %+10.1f %s  %s\n", m.name, m.f(first), m.f(last), mx, b, m.unit, verdict)
	}
	if leak {
		fmt.Println("\nsomething kept growing: look at the CSV and take a heap profile (pprof_listen) while it is high")
	} else {
		fmt.Println("\nno growth beyond the noise in the second half of the run")
	}
}
