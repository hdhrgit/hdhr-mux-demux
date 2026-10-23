// Command uidemo runs the multiplexer's web interface against simulated
// HomeRun devices and a few simulated viewers, so the interface can be
// developed and looked at without hardware or real viewers.
//
//	uidemo -http 127.0.0.1:18080
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"hdhrmux/internal/config"
	"hdhrmux/internal/fakehdhr"
	"hdhrmux/internal/guide"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/logbuf"
	"hdhrmux/internal/upstream"
	"hdhrmux/internal/vdev"
)

func prog(n uint16, vch, name, now, next string, elapsedMin, lengthMin int) fakehdhr.Program {
	return fakehdhr.Program{Number: n, VChannel: vch, Name: name, Now: now, Next: next,
		Elapsed: time.Duration(elapsedMin) * time.Minute, Length: time.Duration(lengthMin) * time.Minute}
}

var channels = []fakehdhr.Channel{
	{Frequency: 177000000, TSID: 1, Programs: []fakehdhr.Program{
		prog(1, "7.1", "KGO-HD", "ABC7 Morning News", "Good Morning America", 22, 60),
		prog(2, "7.2", "LOCLish", "Bay Area Backyard", "Cooking With Friends", 12, 30),
		prog(3, "7.3", "Charge!", "To Be Announced", "MacGyver", 40, 60)}},
	{Frequency: 473000000, TSID: 2, Programs: []fakehdhr.Program{
		prog(1, "14.1", "KDTV-HD", "Caso Cerrado", "Noticias 14", 35, 60),
		prog(3, "14.3", "Bounce", "Family Time", "Lincoln Heights", 5, 30)}},
	{Frequency: 545000000, TSID: 3, Programs: []fakehdhr.Program{
		prog(1, "2.1", "KTVU-HD", "Big Noon Kickoff", "College Football", 48, 120),
		prog(2, "2.2", "FOXWX", "Fox Weather Now", "Fox Weather Now", 7, 30),
		prog(4, "4.1", "KRON-TV", "The Golden Bachelor", "Jeopardy!", 18, 60)}},
	{Frequency: 551000000, TSID: 4, Programs: []fakehdhr.Program{
		prog(1, "5.1", "KPIX-HD", "CBS Mornings", "Let's Make a Deal", 51, 90),
		prog(2, "5.2", "Heroes", "Wonder Woman", "The Flash", 9, 60)}},
	{Frequency: 569000000, TSID: 5, Programs: []fakehdhr.Program{
		prog(1, "9.1", "KQED-HD", "Nature: Cheetahs", "NOVA", 14, 60),
		prog(2, "9.2", "KQED+", "Austin City Limits", "Soundstage", 44, 60)}},
	{Frequency: 581000000, TSID: 6, Programs: []fakehdhr.Program{
		prog(1, "11.1", "KNTV-HD", "Today", "Today with Hoda", 63, 120)}},
	// the same stations on a second RF channel, as 2.x and 4.1 are in the real market
	{Frequency: 575000000, TSID: 3, Programs: []fakehdhr.Program{
		prog(1, "2.1", "KTVU-HD", "Big Noon Kickoff", "College Football", 48, 120),
		prog(2, "2.2", "FOXWX", "Fox Weather Now", "Fox Weather Now", 7, 30),
		prog(4, "4.1", "KRON-TV", "The Golden Bachelor", "Jeopardy!", 18, 60),
		prog(5, "2.3", "MOVIES!", "Twins of Evil", "The Bounty Hunter", 30, 90)}},
}

// viewers: who watches what, from which address, at the start.
var scenes = []struct{ ip, ch string }{
	{"127.0.0.2", "7.1"}, {"127.0.0.3", "7.1"}, {"127.0.0.4", "2.1"},
	{"127.0.0.5", "5.1"}, {"127.0.0.6", "9.1"}, {"127.0.0.7", "14.1"},
}

func main() {
	httpAddr := flag.String("http", "127.0.0.1:18080", "web interface address")
	streamAddr := flag.String("stream", "127.0.0.1:15004", "streaming address")
	viewers := flag.Int("viewers", 5, "simulated viewers at the start (0-6); they come and go over time")
	devices := flag.Int("devices", 3, "simulated devices (4 tuners each)")
	uiDir := flag.String("ui-dir", "", "serve the interface's files from this directory (internal/vdev/ui) so edits show on reload")
	flag.Parse()
	vdev.UIDir = *uiDir
	logs := logbuf.New(4000)
	log.SetOutput(io.MultiWriter(os.Stderr, logs))
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	var addrs []string
	var fakes []*fakehdhr.Device
	for i := 0; i < *devices; i++ {
		f, err := fakehdhr.Start(fakehdhr.Config{DeviceID: hdhrproto.MakeDeviceID(0x10200000 + uint32(i)<<8), Tuners: 4, Channels: channels, Bitrate: 6_000_000})
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
	h := hub.New(pool, hub.Config{Linger: 20 * time.Second, Mask: true})
	defer h.Close()

	lu, _ := lineup.Open("")
	var entries []lineup.Entry
	seen := map[string]bool{} // a channel on two RF channels is listed once, at the first
	for _, c := range channels {
		for _, p := range c.Programs {
			if seen[p.VChannel] {
				continue
			}
			seen[p.VChannel] = true
			entries = append(entries, lineup.Entry{GuideNumber: p.VChannel, GuideName: p.Name, Frequency: c.Frequency, Program: p.Number,
				HD: p.Name != "" && len(p.Name) > 3 && p.Name[len(p.Name)-3:] == "-HD", VideoCodec: "MPEG2", AudioCodec: "AC3"})
		}
	}
	lu.Replace(entries)
	// a throw-away config so the settings page works
	tmp, _ := os.MkdirTemp("", "uidemo")
	defer os.RemoveAll(tmp)
	cfgPath := filepath.Join(tmp, "config.json")
	os.WriteFile(cfgPath, []byte(`{"state_dir":"`+tmp+`","friendly_name":"HDHomeRun Mux (demo)"}`), 0o644)
	cfg, _ := config.Load(cfgPath)
	dev, err := vdev.New(vdev.Config{DeviceID: hdhrproto.MakeDeviceID(0x10900000), FriendlyName: "HDHomeRun Mux (demo)", Tuners: pool.TotalTuners(),
		HTTPAddr: *httpAddr, StreamAddr: *streamAddr, ControlAddr: "127.0.0.1:0",
		GuideHold: 4 * time.Second, Logs: logs, StateDir: tmp, Settings: config.NewManager(cfgPath, cfg), Restart: func() { log.Printf("restart requested (the demo just ignores it)") }}, h, lu)
	if err != nil {
		log.Fatal(err)
	}
	gs, _ := guide.Open("")
	dev.SetGuide(gs)
	go func() { time.Sleep(3 * time.Second); dev.GuidePass(context.Background()) }() // so the channels page has something to show
	if err := dev.Start(); err != nil {
		log.Fatal(err)
	}
	defer dev.Close()
	log.Printf("demo up: http://%s/ui/  (%d simulated tuners)", *httpAddr, pool.TotalTuners())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go simulate(ctx, "http://"+*streamAddr, *viewers)
	<-ctx.Done()
}

// simulate keeps a handful of clients watching, changing now and then.
func simulate(ctx context.Context, base string, n int) {
	type viewer struct{ cancel context.CancelFunc }
	live := map[int]viewer{}
	start := func(i int) {
		s := scenes[i%len(scenes)]
		cctx, cancel := context.WithCancel(ctx)
		live[i] = viewer{cancel}
		go func() {
			d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(s.ip)}}
			cl := &http.Client{Transport: &http.Transport{DialContext: d.DialContext}}
			req, _ := http.NewRequestWithContext(cctx, "GET", base+"/auto/v"+s.ch, nil)
			resp, err := cl.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
		}()
	}
	for i := 0; i < n && i < len(scenes); i++ {
		start(i)
		time.Sleep(700 * time.Millisecond)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(15+rand.Intn(20)) * time.Second):
		}
		i := rand.Intn(len(scenes))
		if v, ok := live[i]; ok {
			v.cancel()
			delete(live, i)
		} else {
			start(i)
		}
	}
}
