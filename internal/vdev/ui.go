package vdev

import (
	"embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/psip"
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
}

type uiTuner struct {
	Index     int        `json:"index"`
	Physical  string     `json:"physical"`
	State     string     `json:"state"` // idle, watching, other
	Frequency uint64     `json:"frequency,omitempty"`
	Signal    *uiSignal  `json:"signal,omitempty"`
	RxBPS     int        `json:"rxBps"` // pulled from the device
	TxBPS     int        `json:"txBps"` // delivered to clients
	Viewers   []uiViewer `json:"viewers"`
}

type uiStream struct {
	Tuner      string `json:"tuner"`
	Frequency  uint64 `json:"frequency"`
	Viewers    int    `json:"viewers"`
	RxBPS      int    `json:"rxBps"`
	Background bool   `json:"background,omitempty"`
	Lingering  bool   `json:"lingering,omitempty"`
}

type uiActivity struct {
	Time   int64 `json:"time"` // unix milliseconds
	Device struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Model  string `json:"model"`
		Uptime int64  `json:"uptime"` // seconds
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
			now = &uiProgramme{Title: e.Title, Start: e.Start.Unix(), End: e.End().Unix()}
		case e.Start.After(at) && (nx == nil || e.Start.Before(nx.Start)):
			nx = &e
		}
	}
	if nx != nil {
		next = &uiProgramme{Title: nx.Title, Start: nx.Start.Unix(), End: nx.End().Unix()}
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
		a.Streams = append(a.Streams, uiStream{Tuner: s.TunerID, Frequency: s.Spec.Frequency, Viewers: s.Subs, RxBPS: s.RxBPS, Background: s.Background, Lingering: s.Lingering})
		a.Summary.RxBPS += s.RxBPS
	}
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
	for _, e := range d.lineup.Entries(false) {
		c := uiChannel{Number: e.GuideNumber, Name: e.GuideName, Frequency: e.Frequency, Program: e.Program, HD: e.HD,
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
	writeJSON(w, map[string]any{"channels": out})
}
