package vdev

import (
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/psip"
	"hdhrmux/internal/ts"
	"hdhrmux/internal/upstream"
)

// The web interface: a single-page app served from /ui/ (static files embedded
// in the binary) that polls /ui/api/activity and /ui/api/lineup.

//go:embed ui
var uiFiles embed.FS

var processStarted = time.Now()

// UIDir, when set, serves the interface's files from that directory instead of
// the copies embedded in the binary, so they can be edited and reloaded.
var UIDir string

func uiFile(name string) ([]byte, error) {
	if UIDir != "" {
		return os.ReadFile(filepath.Join(UIDir, name))
	}
	return uiFiles.ReadFile("ui/" + name)
}

func (d *Device) uiRoutes(mux *http.ServeMux) {
	static := func(name, ctype string) {
		mux.HandleFunc("GET /ui/"+name, func(w http.ResponseWriter, r *http.Request) {
			b, err := uiFile(name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(b)
		})
	}
	static("app.css", "text/css; charset=utf-8")
	static("app.js", "text/javascript; charset=utf-8")
	static("favicon.svg", "image/svg+xml")
	mux.HandleFunc("GET /ui/{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := uiFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write([]byte(strings.ReplaceAll(string(b), "{{NAME}}", esc(d.cfg.FriendlyName))))
	})
	mux.HandleFunc("GET /ui/api/activity", d.handleUIActivity)
	mux.HandleFunc("GET /ui/api/lineup", d.handleUILineup)
	mux.HandleFunc("PUT /ui/api/lineup/{number}", d.handleUILineupPut)
	mux.HandleFunc("GET /ui/api/channels", d.handleUIChannels)
	mux.HandleFunc("PUT /ui/api/channels/{number}", d.handleUIChannelPut)
	mux.HandleFunc("GET /ui/api/merges", d.handleUIMergesGet)
	mux.HandleFunc("PUT /ui/api/merges", d.handleUIMergesPut)
	d.settingsRoutes(mux)
}

// ---- activity -------------------------------------------------------------

type uiSignal struct {
	Strength int `json:"strength"`
	Quality  int `json:"quality"`
	Symbol   int `json:"symbol"`
}

type uiProgramme struct {
	Title string `json:"title"`
	Start int64  `json:"start"` // unix seconds
	End   int64  `json:"end"`
	// Placeholder marks the "To Be Announced" entries some stations broadcast
	// for hours ahead: real data, but not a programme title.
	Placeholder bool `json:"placeholder,omitempty"`
}

var rePlaceholder = regexp.MustCompile(`(?i)^\s*(to be announced|tba|tbd|(program(me)?|programming) information (is )?(currently )?(not available|unavailable)|no (program(me)? )?information)\b`)

func newProgramme(e psip.Event) *uiProgramme {
	return &uiProgramme{Title: e.Title, Start: e.Start.Unix(), End: e.End().Unix(), Placeholder: rePlaceholder.MatchString(e.Title)}
}

type uiViewer struct {
	Client  string       `json:"client"`
	Channel string       `json:"channel"` // as requested: a guide number or a raw RF path
	Name    string       `json:"name,omitempty"`
	Now     *uiProgramme `json:"now,omitempty"`
	Next    *uiProgramme `json:"next,omitempty"`
	Since   int64        `json:"since"`
	BPS     int          `json:"bps"`
	Queue   int          `json:"queue"` // % of the client buffer in use
	Drops   uint64       `json:"drops"`
	Control bool         `json:"control,omitempty"` // a control-protocol client rather than HTTP
	// Inferred: a guess from the PIDs the client asked for; Alt are all the
	// channels it may be watching when they cannot be told apart.
	Inferred bool `json:"inferred,omitempty"`
	// Scan: collecting tables (an EPG or channel scan), not watching a channel.
	Scan bool     `json:"scan,omitempty"`
	Alt  []string `json:"alt,omitempty"`
}

type uiTuner struct {
	Index     int       `json:"index"`
	Physical  string    `json:"physical"`
	PhysURL   string    `json:"physicalUrl,omitempty"` // the device's own status page for this tuner
	State     string    `json:"state"`                 // idle, watching, other
	Frequency uint64    `json:"frequency,omitempty"`
	Signal    *uiSignal `json:"signal,omitempty"`
	RxBPS     int       `json:"rxBps"` // pulled from the device
	TxBPS     int       `json:"txBps"` // delivered to clients
	// For state "other": who holds the physical tuner, as the device reports it.
	Holder      string        `json:"holder,omitempty"`      // address holding the lock
	HolderLocal bool          `json:"holderLocal,omitempty"` // ... and it is this machine
	Transport   string        `json:"transport,omitempty"`   // udp, rtp or http: how it receives
	Watching    []uiOtherChan `json:"watching,omitempty"`    // what the other application is tuned to, read from the device
	Viewers     []uiViewer    `json:"viewers"`
}

// uiOtherChan is a channel another application has selected on a tuner.
type uiOtherChan struct {
	Channel  string       `json:"channel"`
	Name     string       `json:"name,omitempty"`
	Now      *uiProgramme `json:"now,omitempty"`
	Next     *uiProgramme `json:"next,omitempty"`
	Inferred bool         `json:"inferred,omitempty"` // worked out from the PIDs it asked for
}

type uiStream struct {
	Tuner      string `json:"tuner"`
	PhysURL    string `json:"physicalUrl,omitempty"`
	Frequency  uint64 `json:"frequency"`
	Viewers    int    `json:"viewers"`
	RxBPS      int    `json:"rxBps"`
	Background bool   `json:"background,omitempty"`
	Lingering  bool   `json:"lingering,omitempty"`
}

type uiActivity struct {
	Time   int64 `json:"time"` // unix milliseconds
	Device struct {
		ID      string `json:"id"`
		Version string `json:"version,omitempty"`
		Name    string `json:"name"`
		Model   string `json:"model"`
		Uptime  int64  `json:"uptime"` // seconds
	} `json:"device"`
	Summary struct {
		Viewers      int `json:"viewers"`
		TunersInUse  int `json:"tunersInUse"` // virtual tuners with viewers
		Tuners       int `json:"tuners"`
		Physical     int `json:"physical"`
		PhysicalUp   int `json:"physicalOnline"`
		PhysicalBusy int `json:"physicalBusy"`
		RxBPS        int `json:"rxBps"`
		TxBPS        int `json:"txBps"`
		OtherTuners  int `json:"otherTuners"` // used by other applications
	} `json:"summary"`
	Tuners   []uiTuner         `json:"tuners"`
	Streams  []uiStream        `json:"streams"`
	Counters map[string]uint64 `json:"counters"`
	System   uiSystem          `json:"system"`
	Setup    string            `json:"setup,omitempty"` // why there is nothing to serve yet
	Notice   string            `json:"notice,omitempty"`
}

// entryFor finds the lineup entry behind what a client asked for: a guide
// number, or a raw /chFREQ[-PROGRAM] path.
func (d *Device) entryFor(label string) (lineup.Entry, bool) {
	if e, ok := d.lineup.Find(label); ok {
		return e, true
	}
	if m := reChNum.FindStringSubmatch(label); m != nil {
		freq, _ := strconv.ParseUint(m[1], 10, 64)
		prog, _ := strconv.Atoi(m[2])
		for _, e := range d.lineup.Entries(true) {
			if e.Frequency == freq && int(e.Program) == prog {
				return e, true
			}
		}
	}
	return lineup.Entry{}, false
}

// holderOf says who holds physical tuner idx: the address with the lock, whether
// that is this machine (another program here, such as tvheadend, looks exactly
// like us from the device's side), and how it receives the stream. Answers are
// cached for a few seconds: the activity page asks every two.
func (d *Device) holderOf(idx int, freq uint64) (holder string, local bool, transport string, watching []uiOtherChan) {
	d.holderMu.Lock()
	c, ok := d.holderCache[idx]
	d.holderMu.Unlock()
	if ok && time.Since(c.at) < 4*time.Second {
		return c.holder, c.local, c.transport, c.watching
	}
	pool := d.hub.Pool()
	if ref, found := pool.Resolve(idx); found && ref.Device != nil {
		if v, err := pool.RawGet(ref.DeviceID, ref.Index, "lockkey"); err == nil && net.ParseIP(v) != nil {
			holder = v
			local = isLocalAddress(net.ParseIP(v))
		}
		if v, err := pool.RawGet(ref.DeviceID, ref.Index, "target"); err == nil {
			if scheme, _, ok := strings.Cut(v, "://"); ok {
				transport = scheme
			}
		}
		watching = d.otherWatching(pool, ref.DeviceID, ref.Index, freq)
	}
	d.holderMu.Lock()
	if d.holderCache == nil {
		d.holderCache = map[int]holderInfo{}
	}
	d.holderCache[idx] = holderInfo{holder, local, transport, watching, time.Now()}
	d.holderMu.Unlock()
	return holder, local, transport, watching
}

type holderInfo struct {
	holder    string
	local     bool
	transport string
	watching  []uiOtherChan
	at        time.Time
}

// isLocalAddress reports whether ip is one of this machine's own addresses.
func isLocalAddress(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// physURL is the address of the status page a physical tuner's own device
// serves ("DEVICEID/N"), where the multiplexer knows the device.
func (d *Device) physURL(tuner string) string {
	id, idx, ok := strings.Cut(tuner, "/")
	if !ok {
		return ""
	}
	for _, dev := range d.hub.Pool().Devices() {
		if hdhrproto.FormatDeviceID(dev.ID) == id {
			host := dev.IP.String()
			if dev.WebPort != 0 && dev.WebPort != 80 {
				host = net.JoinHostPort(host, strconv.Itoa(dev.WebPort))
			}
			return "http://" + host + "/tuners.html?page=tuner" + idx
		}
	}
	return ""
}

// nowNext finds what is on now and what follows, from the guide.
func (d *Device) nowNext(guideNumber string, at time.Time) (now, next *uiProgramme) {
	d.guide.mu.Lock()
	store := d.guide.store
	d.guide.mu.Unlock()
	if store == nil || guideNumber == "" {
		return nil, nil
	}
	var nx *psip.Event
	for _, e := range store.Events(guideNumber) {
		e := e
		switch {
		case !at.Before(e.Start) && at.Before(e.End()):
			now = newProgramme(e)
		case e.Start.After(at) && (nx == nil || e.Start.Before(nx.Start)):
			nx = &e
		}
	}
	if nx != nil {
		next = newProgramme(*nx)
	}
	return now, next
}

func (d *Device) activity() uiActivity {
	var a uiActivity
	now := time.Now()
	a.Time = now.UnixMilli()
	a.Device.ID = hdhrproto.FormatDeviceID(d.cfg.DeviceID)
	a.Device.Name = d.cfg.FriendlyName
	a.Device.Model = d.cfg.ModelNumber
	a.Device.Version = d.cfg.Version
	a.Device.Uptime = int64(now.Sub(processStarted).Seconds())

	pool := d.hub.Pool()
	for _, v := range d.tuners {
		phys := ""
		if ref, ok := pool.Resolve(v.idx); ok {
			phys = fmt.Sprintf("%s/%d", hdhrproto.FormatDeviceID(ref.DeviceID), ref.Index)
		}
		t := uiTuner{Index: v.idx, Physical: phys, State: "idle", Viewers: []uiViewer{}}
		sub := v.currentSub()
		st := upstream.ParseStatus(v.statusString())
		if sub != nil {
			info := sub.Info()
			t.Physical = info.TunerID
			t.Frequency = info.Spec.Frequency
			t.RxBPS = info.RxBPS
			t.Signal = &uiSignal{info.Status.SS, info.Status.SNQ, info.Status.SEQ}
		} else if st.Channel != "" && st.Channel != "none" {
			// idle for us, but another application is using the physical tuner
			if _, f, ok := strings.Cut(st.Channel, ":"); ok {
				t.Frequency, _ = strconv.ParseUint(f, 10, 64)
			}
			t.State = "other"
			t.Holder, t.HolderLocal, t.Transport, t.Watching = d.holderOf(v.idx, t.Frequency)
			t.RxBPS = st.BPS
			t.Signal = &uiSignal{st.SS, st.SNQ, st.SEQ}
			a.Summary.OtherTuners++
		}
		for _, r := range v.viewerRows() {
			uv := uiViewer{Client: r.Client, Channel: r.Label, Since: r.Since.Unix(), BPS: r.BPS, Queue: r.Queue, Drops: r.Drops, Control: r.Control}
			gn := r.Label
			if e, ok := d.entryFor(r.Label); ok {
				uv.Name, gn = e.GuideName, e.GuideNumber
				uv.Channel = e.GuideNumber
			}
			if r.Inferred {
				uv.Inferred = true
				if len(r.Alt) > 1 {
					// no telling which of them: say all, and the names once each
					uv.Alt = r.Alt
					uv.Channel = strings.Join(r.Alt, " / ")
					var names []string
					seen := map[string]bool{}
					for _, n := range r.Alt {
						if e, ok := d.entryFor(n); ok && !seen[e.GuideName] {
							seen[e.GuideName] = true
							names = append(names, e.GuideName)
						}
					}
					uv.Name = strings.Join(names, " / ")
				}
			}
			if r.Scan {
				uv.Scan, uv.Channel, uv.Name = true, "EPG scan", ""
				if r.Freq != 0 {
					uv.Name = fmt.Sprintf("%d MHz", r.Freq/1000000)
				}
			}
			uv.Now, uv.Next = d.nowNext(gn, now)
			t.TxBPS += r.BPS
			t.Viewers = append(t.Viewers, uv)
		}
		if len(t.Viewers) > 0 {
			t.State = "watching"
			a.Summary.TunersInUse++
			a.Summary.Viewers += len(t.Viewers)
		}
		a.Summary.TxBPS += t.TxBPS
		t.PhysURL = d.physURL(t.Physical)
		a.Tuners = append(a.Tuners, t)
	}
	a.Summary.Tuners = len(d.tuners)
	a.Summary.Physical = pool.TotalTuners()
	a.Summary.PhysicalUp = pool.TunerCount()
	for g := 0; g < a.Summary.Physical; g++ {
		if ref, ok := pool.Resolve(g); ok && pool.HeldByUs(ref.DeviceID, ref.Index) {
			a.Summary.PhysicalBusy++
		}
	}

	a.Streams = []uiStream{}
	streams := d.hub.Streams()
	sort.Slice(streams, func(i, j int) bool { return streams[i].TunerID < streams[j].TunerID })
	for _, s := range streams {
		a.Streams = append(a.Streams, uiStream{Tuner: s.TunerID, PhysURL: d.physURL(s.TunerID), Frequency: s.Spec.Frequency, Viewers: s.Subs, RxBPS: s.RxBPS, Background: s.Background, Lingering: s.Lingering})
		a.Summary.RxBPS += s.RxBPS
	}
	a.System = d.system()
	a.Setup = d.cfg.Setup
	a.Notice = d.cfg.Notice
	hs := d.hub.Stats()
	a.Counters = map[string]uint64{
		"streamsOpened": hs.StreamsOpened, "failovers": hs.Failovers, "filterUpdates": hs.MaskUpdates,
		"reclaims": hs.Reclaims, "droppedBatches": hs.DroppedBatches,
	}
	return a
}

func (d *Device) handleUIActivity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, d.activity())
}

// ---- lineup ---------------------------------------------------------------

type uiChannel struct {
	Number    string       `json:"number"`
	Name      string       `json:"name"`
	Frequency uint64       `json:"frequency"`
	Program   uint16       `json:"program"`
	HD        bool         `json:"hd,omitempty"`
	Favorite  bool         `json:"favorite,omitempty"`
	Disabled  bool         `json:"disabled,omitempty"`
	Codec     string       `json:"codec,omitempty"`
	Modul     string       `json:"modulation,omitempty"`
	Now       *uiProgramme `json:"now,omitempty"`
	Next      *uiProgramme `json:"next,omitempty"`
	Watching  int          `json:"watching"`
	URL       string       `json:"url"`
}

func (d *Device) handleUILineup(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	sbase := d.streamBase(r)
	watching := map[string]int{}
	for _, v := range d.tuners {
		for _, row := range v.viewerRows() {
			if e, ok := d.entryFor(row.Label); ok {
				watching[e.GuideNumber]++
			}
		}
	}
	out := []uiChannel{}
	for _, e := range d.lineup.Entries(true) {
		c := uiChannel{Disabled: e.Disabled, Number: e.GuideNumber, Name: e.GuideName, Frequency: e.Frequency, Program: e.Program, HD: e.HD,
			Favorite: e.Favorite, Modul: e.Modulation, Watching: watching[e.GuideNumber], URL: sbase + "/auto/v" + e.GuideNumber}
		if e.VideoCodec != "" {
			c.Codec = e.VideoCodec
			if e.AudioCodec != "" {
				c.Codec += " / " + e.AudioCodec
			}
		}
		c.Now, c.Next = d.nowNext(e.GuideNumber, now)
		out = append(out, c)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"channels": out, "editable": d.adminCheck(nil, r, false) && !d.cfg.SettingsLocked})
}

// handleUILineupPut sets what a channel is in this lineup: "normal", "favorite" or
// "disabled". The devices are not told: it is this multiplexer's own copy.
func (d *Device) handleUILineupPut(w http.ResponseWriter, r *http.Request) {
	if !d.adminCheck(w, r, true) {
		return
	}
	number := r.PathValue("number")
	if _, ok := d.lineup.Lookup(number); !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no such channel"})
		return
	}
	var p struct{ State string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&p); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	switch p.State {
	case "normal":
		d.lineup.SetFavorite(number, false)
		d.lineup.SetDisabled(number, false)
	case "favorite":
		d.lineup.SetFavorite(number, true)
	case "disabled":
		d.lineup.SetDisabled(number, true)
	default:
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "state must be normal, favorite or disabled"})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// otherWatching reads from the device what another application has selected on a
// tuner: the program it asked for (the device's streaminfo names it with the channel
// number the station gives it), or, for an application that selects PIDs instead
// (tvheadend), the programmes of the multiplex its filter takes completely.
func (d *Device) otherWatching(pool *upstream.Pool, dev uint32, idx int, freq uint64) []uiOtherChan {
	now := time.Now()
	make1 := func(num, name string, inferred bool) uiOtherChan {
		o := uiOtherChan{Channel: num, Name: name, Inferred: inferred}
		if e, ok := d.lineup.Find(num); ok {
			o.Name = e.GuideName
		}
		o.Now, o.Next = d.nowNext(num, now)
		return o
	}
	prog := 0
	if v, err := pool.RawGet(dev, idx, "program"); err == nil {
		prog, _ = strconv.Atoi(strings.TrimSpace(v))
	}
	if prog > 0 {
		if info, err := pool.RawGet(dev, idx, "streaminfo"); err == nil {
			for _, e := range hub.ParseStreamInfo(info, freq, "", "") {
				if int(e.Program) == prog {
					return []uiOtherChan{make1(e.GuideNumber, e.GuideName, false)}
				}
			}
		}
		if num, name, ok := d.lineup.ProgramName(freq, uint16(prog)); ok {
			return []uiOtherChan{make1(num, name, false)}
		}
		return []uiOtherChan{{Channel: fmt.Sprintf("program %d", prog)}}
	}
	// no program: a client that takes PIDs. A programme counts when its audio and video are all let through.
	v, err := pool.RawGet(dev, idx, "filter")
	if err != nil {
		return nil
	}
	set, err := ts.ParsePIDSet(v)
	if err != nil || set.Empty() {
		return nil
	}
	// nothing is filtered out: it takes the whole multiplex, whatever it does with it
	if set == ts.FullSet() {
		o := uiOtherChan{Channel: "Full multiplex"}
		for _, r := range d.rf.Snapshot() {
			if r.Frequency == freq && len(r.Programs) > 0 {
				o.Name = fmt.Sprintf("all %d programmes", len(r.Programs))
			}
		}
		return []uiOtherChan{o}
	}
	var out []uiOtherChan
	for _, r := range d.rf.Snapshot() {
		if r.Frequency != freq {
			continue
		}
		nums := make([]uint16, 0, len(r.Programs))
		for n := range r.Programs {
			nums = append(nums, n)
		}
		sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
		// programmes with the same audio and video are one: there is no telling which of them it is
		type group struct{ nums, names []string }
		groups := map[string]*group{}
		var order []string
		for _, n := range nums {
			p := r.Programs[n]
			if len(p.PIDs) == 0 || p.Channel == "" {
				continue
			}
			all := true
			for _, pid := range p.PIDs {
				if !set.Has(pid) {
					all = false
					break
				}
			}
			if !all {
				continue
			}
			key := fmt.Sprint(p.PIDs)
			g := groups[key]
			if g == nil {
				g = &group{}
				groups[key] = g
				order = append(order, key)
			}
			name := p.Name
			if e, ok := d.lineup.Find(p.Channel); ok {
				name = e.GuideName
			}
			g.nums = append(g.nums, p.Channel)
			if name != "" && !slices.Contains(g.names, name) {
				g.names = append(g.names, name)
			}
		}
		for _, key := range order {
			g := groups[key]
			o := make1(g.nums[0], "", true)
			o.Channel, o.Name = strings.Join(g.nums, " / "), strings.Join(g.names, " / ")
			out = append(out, o)
		}
	}
	return out
}
