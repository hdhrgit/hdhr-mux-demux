package vdev

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"hdhrmux/internal/chanmap"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/upstream"
)

func (d *Device) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", d.handleIndex)
	mux.HandleFunc("GET /discover.json", d.handleDiscover)
	mux.HandleFunc("GET /lineup.json", d.handleLineupJSON)
	mux.HandleFunc("GET /lineup.xml", d.handleLineupXML)
	mux.HandleFunc("GET /lineup_status.json", d.handleLineupStatus)
	mux.HandleFunc("POST /lineup.post", d.handleLineupPost)
	mux.HandleFunc("GET /lineup.post", d.handleLineupPost)
	d.dmsRoutes(mux)
	d.playlistRoutes(mux)
	mux.HandleFunc("GET /epg", d.handleEPG)
	mux.HandleFunc("GET /epg.xml", d.handleEPG)
	mux.HandleFunc("GET /epg.json", d.handleEPGJSON)
	mux.HandleFunc("POST /epg/refresh", d.handleEPGRefresh)
	mux.HandleFunc("GET /status.json", d.handleStatus)
	mux.HandleFunc("GET /tuners.html", d.handleTunersHTML)
	mux.HandleFunc("GET /metrics", d.handleMetrics)
	d.uiRoutes(mux)
	mux.HandleFunc("GET /lineup.html", d.handleLineupHTML)
	mux.HandleFunc("GET /{tuner}/{channel}", d.handleStream)
	return mux
}

// streamBase returns the base URL of the streaming port, as real devices
// advertise in their lineups (http://host:5004).
func (d *Device) streamBase(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if d.cfg.AdvertiseHost != "" {
		host = d.cfg.AdvertiseHost
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(d.StreamPort()))
}

// requestBase returns http://host:port as the client addressed us.
func (d *Device) requestBase(r *http.Request) string {
	if d.cfg.AdvertiseHost != "" {
		return d.baseURL("")
	}
	host := r.Host
	if host == "" {
		host = net.JoinHostPort("127.0.0.1", strconv.Itoa(d.HTTPPort()))
	}
	return "http://" + host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "\t")
	enc.Encode(v)
}

func (d *Device) handleDiscover(w http.ResponseWriter, r *http.Request) {
	base := d.requestBase(r)
	writeJSON(w, map[string]any{
		"FriendlyName":    d.cfg.FriendlyName,
		"Manufacturer":    d.cfg.Manufacturer,
		"ModelNumber":     d.cfg.ModelNumber,
		"FirmwareName":    d.cfg.FirmwareName,
		"FirmwareVersion": d.cfg.FirmwareVersion,
		"DeviceID":        hdhrproto.FormatDeviceID(d.cfg.DeviceID),
		"DeviceAuth":      "hdhr-mux",
		"BaseURL":         base,
		"LineupURL":       base + "/lineup.json",
		"TunerCount":      len(d.tuners),
	})
}

type lineupJSONEntry struct {
	GuideNumber string
	GuideName   string
	Frequency   string `json:",omitempty"`
	Modulation  string `json:",omitempty"`
	VideoCodec  string `json:",omitempty"`
	AudioCodec  string `json:",omitempty"`
	HD          int    `json:",omitempty"`
	Favorite    int    `json:",omitempty"`
	DRM         int    `json:",omitempty"`
	URL         string
}

func (d *Device) handleLineupJSON(w http.ResponseWriter, r *http.Request) {
	sbase := d.streamBase(r)
	all := r.URL.Query().Get("show") == "found"
	out := []lineupJSONEntry{}
	for _, e := range d.lineup.Entries(all) {
		j := lineupJSONEntry{GuideNumber: e.GuideNumber, GuideName: e.GuideName, VideoCodec: e.VideoCodec, AudioCodec: e.AudioCodec, URL: sbase + "/auto/v" + e.GuideNumber}
		if e.HD {
			j.HD = 1
		}
		if e.Favorite {
			j.Favorite = 1
		}
		if e.DRM {
			j.DRM = 1
		}
		out = append(out, j)
	}
	writeJSON(w, out)
}

func (d *Device) handleLineupXML(w http.ResponseWriter, r *http.Request) {
	type program struct {
		GuideNumber string
		GuideName   string
		URL         string
	}
	type lineup struct {
		XMLName xml.Name `xml:"Lineup"`
		Program []program
	}
	base := d.streamBase(r)
	var l lineup
	for _, e := range d.lineup.Entries(false) {
		l.Program = append(l.Program, program{e.GuideNumber, e.GuideName, base + "/auto/v" + e.GuideNumber})
	}
	w.Header().Set("Content-Type", "text/xml")
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "\t")
	enc.Encode(l)
}

func (d *Device) handleLineupStatus(w http.ResponseWriter, r *http.Request) {
	scanning, prog, src := d.scanState()
	if scanning {
		pct := 0
		if prog.Total > 0 {
			pct = prog.Done * 100 / prog.Total
		}
		writeJSON(w, map[string]any{"ScanInProgress": 1, "Progress": pct, "Found": prog.Found})
		return
	}
	var sources []string
	for s := range d.cfg.Sources {
		sources = append(sources, s)
	}
	sortStrings(sources)
	writeJSON(w, map[string]any{"ScanInProgress": 0, "ScanPossible": 1, "Source": src, "SourceList": sources})
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (d *Device) handleLineupPost(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch q.Get("scan") {
	case "start":
		if err := d.StartScan(q.Get("source")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	case "abort":
		d.AbortScan()
	}
	for _, f := range q["favorite"] {
		for _, item := range strings.Split(f, ",") {
			if len(item) > 1 && (item[0] == '+' || item[0] == '-' || item[0] == ' ') {
				d.lineup.SetFavorite(item[1:], item[0] != '-')
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (d *Device) uuid() string {
	id := hdhrproto.FormatDeviceID(d.cfg.DeviceID)
	return fmt.Sprintf("%s-0000-0000-0000-%s0000", strings.ToLower(id), strings.ToLower(id))
}

type tunerStatusJSON struct {
	Resource              string
	VctNumber             string `json:",omitempty"`
	VctName               string `json:",omitempty"`
	Frequency             uint64 `json:",omitempty"`
	SignalStrengthPercent int
	SignalQualityPercent  int
	SymbolQualityPercent  int
	TargetIP              string `json:",omitempty"`
	NetworkRate           int
}

func (d *Device) handleStatus(w http.ResponseWriter, r *http.Request) {
	out := []tunerStatusJSON{}
	for _, v := range d.tuners {
		v.mu.Lock()
		sub, spec, vch, tgt := v.sub, v.spec, v.vchannel, v.targetStr
		v.mu.Unlock()
		s := tunerStatusJSON{Resource: fmt.Sprintf("tuner%d", v.idx)}
		st := ParseVStatus(v.statusString())
		if sub == nil && st.Channel != "" && st.Channel != "none" {
			// idle here, but somebody else is using the physical tuner
			if _, f, ok := strings.Cut(st.Channel, ":"); ok {
				s.Frequency, _ = strconv.ParseUint(f, 10, 64)
			}
			s.SignalStrengthPercent, s.SignalQualityPercent, s.SymbolQualityPercent = st.SS, st.SNQ, st.SEQ
			s.NetworkRate = st.BPS
		}
		if sub != nil {
			s.Frequency = spec.Frequency
			s.SignalStrengthPercent, s.SignalQualityPercent, s.SymbolQualityPercent = st.SS, st.SNQ, st.SEQ
			s.NetworkRate = st.BPS
			if vch != "" {
				s.VctNumber = vch
				if e, ok := d.lineup.Find(vch); ok {
					s.VctName = e.GuideName
				}
			} else if num, name, ok := v.inferredChannel(sub, spec); ok {
				s.VctNumber, s.VctName = num, name // a client that picks PIDs, not a channel
			}
			if i := strings.Index(tgt, "://"); i >= 0 {
				host, _, _ := net.SplitHostPort(tgt[i+3:])
				s.TargetIP = host
			}
		}
		out = append(out, s)
	}
	writeJSON(w, out)
}

// ParseVStatus parses a status string.
func ParseVStatus(s string) upstream.Status { return upstream.ParseStatus(s) }

func (d *Device) page(w http.ResponseWriter, title, sub string, body func()) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!DOCTYPE HTML PUBLIC \"-//W3C//DTD HTML 4.01//EN\" \"http://www.w3.org/TR/html4/strict.dtd\">\n<html><head><title>%s</title>"+
		"<style>body{font-family:sans-serif;margin:1em 2em}table{border-collapse:collapse}td,th{border:1px solid #999;padding:3px 8px;text-align:left}</style></head><body>",
		html.EscapeString(title))
	fmt.Fprintf(w, "<h1><a href=\"/\" style=\"color:inherit;text-decoration:none\">%s</a></h1><p>%s</p>", html.EscapeString(d.cfg.FriendlyName), sub)
	body()
	fmt.Fprint(w, "</body></html>")
}

func (d *Device) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Browsers get the web interface; everything else (and ?classic) the plain
	// menu real devices serve.
	if strings.Contains(r.Header.Get("Accept"), "text/html") && r.URL.Query().Get("classic") == "" {
		http.Redirect(w, r, "/ui/", http.StatusFound)
		return
	}
	d.page(w, "HDHomeRun Main Menu", fmt.Sprintf("Model: %s<br>Device ID: %s<br>Firmware: %s",
		html.EscapeString(d.cfg.ModelNumber), hdhrproto.FormatDeviceID(d.cfg.DeviceID), html.EscapeString(d.cfg.FirmwareVersion)), func() {
		fmt.Fprint(w, "<ul><li><a href=\"/lineup.html\">Channel Lineup</a><li><a href=\"/tuners.html\">Tuner Status</a>"+
			"<li><a href=\"/discover.json\">discover.json</a><li><a href=\"/lineup.json\">lineup.json</a><li><a href=\"/status.json\">status.json</a></ul>")
	})
}

func (d *Device) handleLineupHTML(w http.ResponseWriter, r *http.Request) {
	sbase := d.streamBase(r)
	d.page(w, "HDHomeRun Channel Lineup", "Channel Lineup", func() {
		fmt.Fprint(w, "<table><tr><th>Channel<th>Name<th>RF<th>Program<th>Codec<th>Stream")
		for _, e := range d.lineup.Entries(true) {
			url := sbase + "/auto/v" + e.GuideNumber
			fmt.Fprintf(w, "<tr><td>%s<td>%s<td>%.0f MHz %s<td>%d<td>%s/%s<td><a href=\"%s\">%s</a>", html.EscapeString(e.GuideNumber),
				html.EscapeString(e.GuideName), float64(e.Frequency)/1e6, html.EscapeString(e.Modulation), e.Program,
				html.EscapeString(e.VideoCodec), html.EscapeString(e.AudioCodec), html.EscapeString(url), html.EscapeString(url))
		}
		fmt.Fprint(w, "</table>")
	})
}

func (d *Device) handleTunersHTML(w http.ResponseWriter, r *http.Request) {
	pool := d.hub.Pool()
	type row struct {
		v       *VTuner
		phys    string
		viewers []ViewerRow
	}
	var rows []row
	clients, busy := 0, 0
	for _, v := range d.tuners {
		phys := ""
		if ref, ok := pool.Resolve(v.idx); ok {
			phys = fmt.Sprintf("%s/%d", hdhrproto.FormatDeviceID(ref.DeviceID), ref.Index)
		}
		if sub := v.currentSub(); sub != nil {
			if id := sub.Info().TunerID; id != phys {
				phys += " → " + id
			}
		}
		vs := v.viewerRows()
		clients += len(vs)
		if len(vs) > 0 {
			busy++
		}
		rows = append(rows, row{v, phys, vs})
	}
	d.page(w, "HDHomeRun Tuner Status", fmt.Sprintf("Tuner Status: %d client(s) on %d of %d tuner(s)", clients, busy, len(d.tuners)), func() {
		fmt.Fprint(w, "<table><tr><th>Tuner<th>Physical<th>Status<th>Clients<th>Channel, client, time, rate, buffer")
		for _, rw := range rows {
			fmt.Fprintf(w, "<tr><td>tuner%d<td>%s<td>%s<td>%d<td>", rw.v.idx, html.EscapeString(rw.phys),
				html.EscapeString(rw.v.statusString()), len(rw.viewers))
			for _, vw := range rw.viewers {
				name, label := "", vw.Label
				if e, ok := d.lineup.Find(vw.Label); ok {
					name = " " + e.GuideName
				}
				if len(vw.Alt) > 1 { // a guess that cannot tell these apart
					label = strings.Join(vw.Alt, " / ")
				}
				kind := ""
				if vw.Control {
					kind = " (control"
					if vw.Inferred {
						kind += ", channel guessed from its PIDs"
					}
					kind += ")"
				}
				fmt.Fprintf(w, "%s%s &mdash; %s%s &mdash; %s &mdash; %.1f Mbps &mdash; buffer %d%%, dropped %d<br>",
					html.EscapeString(label), html.EscapeString(name), html.EscapeString(vw.Client), kind,
					time.Since(vw.Since).Round(time.Second), float64(vw.BPS)/1e6, vw.Queue, vw.Drops)
			}
		}
		fmt.Fprint(w, "</table>")
	})
}

// ---- streaming ---------------------------------------------------------

var (
	reChNum = regexp.MustCompile(`^ch(\d+)(?:-(\d+))?$`)
	reChMod = regexp.MustCompile(`^ch([a-z0-9]+):(\d+)(?:-(\d+))?$`)
)

type streamReq struct {
	spec     chanmap.Spec
	program  uint16
	vchannel string
	service  bool // ATSC 3.0: the device delivers one service per tuner
}

// resolve returns the lineup entry for a guide number, learning where the
// channel is on the air first if that is not known yet.
func (d *Device) resolve(ctx context.Context, guide string) (lineup.Entry, error) {
	e, ok := d.lineup.Find(guide)
	if !ok {
		return e, fmt.Errorf("%w %q", errUnknownChannel, guide)
	}
	if e.Frequency != 0 {
		return e, nil
	}
	if err := d.hub.LearnVChannel(ctx, guide); err != nil {
		if errors.Is(err, upstream.ErrUnknownChannel) {
			return e, fmt.Errorf("%w %q", errUnknownChannel, guide)
		}
		return e, err
	}
	if e, ok = d.lineup.Find(guide); !ok || e.Frequency == 0 {
		return e, fmt.Errorf("%w: %s did not show up on the multiplex it was found on", upstream.ErrNoSignal, guide)
	}
	return e, nil
}

func (d *Device) parseChannelPath(ctx context.Context, s string) (streamReq, error) {
	var rq streamReq
	switch {
	case strings.HasPrefix(s, "v"):
		e, err := d.resolve(ctx, s[1:])
		if err != nil {
			return rq, err
		}
		rq.spec = chanmap.Spec{Modulation: "auto", Frequency: e.Frequency}
		rq.program, rq.vchannel, rq.service = e.Program, e.GuideNumber, e.Modulation == "atsc3"
		return rq, nil
	case reChNum.MatchString(s):
		m := reChNum.FindStringSubmatch(s)
		return d.numericChannel("auto", m[1], m[2])
	case reChMod.MatchString(s):
		m := reChMod.FindStringSubmatch(s)
		return d.numericChannel(m[1], m[2], m[3])
	}
	return rq, fmt.Errorf("%w %q", errBadChannel, s)
}

func (d *Device) numericChannel(mod, num, prog string) (streamReq, error) {
	spec, _, err := chanmap.Parse(mod+":"+num, d.cfg.ChannelMap)
	if err != nil {
		return streamReq{}, err
	}
	rq := streamReq{spec: spec}
	if prog != "" {
		p, _ := strconv.Atoi(prog)
		rq.program = uint16(p)
	}
	return rq, nil
}

// httpErr answers the way a real HDHomeRun does: empty body, an
// X-HDHomeRun-Error header such as "804 Tuner In Use".
func httpErr(w http.ResponseWriter, code int, hdhr string) {
	h := w.Header()
	h.Set("Server", "HDHomeRun/1.0")
	h.Set("Connection", "close")
	h.Set("Cache-Control", "no-cache")
	if hdhr != "" {
		h["X-HDHomeRun-Error"] = []string{hdhr} // exact case, as real devices send it
	}
	w.WriteHeader(code)
	countStreamResponse(code, hdhr)
}

// openStream finds the virtual tuner for a stream request and starts it.
//
// Virtual tuner N is physical tuner N. A request for a specific tuner uses
// that tuner; if it is busy it is still served when the tuner already carries
// the same stream (another viewer joins), otherwise it gets 804. /auto/ goes
// further, because the point of multiplexing is to keep tuners free: it first
// reuses any tuner already carrying the stream, and only then opens a new one.
func (d *Device) openStream(ctx context.Context, which string, rq streamReq) (*VTuner, *hub.Sub, error) {
	start := func(v *VTuner) (*VTuner, *hub.Sub, error) {
		sub, err := v.startHTTP(ctx, rq.spec, rq.program, rq.vchannel, rq.service, which == "auto")
		return v, sub, err
	}
	if which == "auto" {
		return d.openAuto(ctx, rq, start)
	}
	if !strings.HasPrefix(which, "tuner") {
		return nil, nil, errNotFound
	}
	idx, err := strconv.Atoi(which[5:])
	if err != nil || idx < 0 || idx >= len(d.tuners) {
		return nil, nil, errNotFound
	}
	v := d.tuners[idx]
	if v.tryReserve() {
		return start(v)
	}
	// A tuner asked for by number is that client's, as on a real device: if it is
	// busy, even with the same channel, the answer is 804. (Sharing is for /auto/.)
	return nil, nil, ErrBusy
}

func (d *Device) openAuto(ctx context.Context, rq streamReq, start func(*VTuner) (*VTuner, *hub.Sub, error)) (*VTuner, *hub.Sub, error) {
	// 1. Reuse: a tuner already carrying this stream (busy ones first, then
	// ones only lingering on it) costs no additional tuner at all.
	carriers := d.hub.Carrying(rq.spec, d.tuners[0].opts(rq.program, rq.service))
	for _, idle := range []bool{false, true} {
		for _, c := range carriers {
			if c.Idle != idle || c.Tuner >= len(d.tuners) {
				continue
			}
			v := d.tuners[c.Tuner]
			if idle {
				if v.tryReserve() {
					if v2, sub, err := start(v); err == nil {
						return v2, sub, nil
					}
				}
			} else if sub, err := v.joinHTTP(ctx, rq.spec, rq.program, rq.service, false); err == nil {
				return v, sub, nil
			}
		}
	}

	// 2. Nothing to reuse: open a new tuner, choosing the one that does the
	// least harm to what may be asked later (ATSC 1.0 stays off the ATSC 3.0
	// tuners, tuners used by other applications or still lingering come last).
	pool := d.hub.Pool()
	idle := d.hub.IdleTuners()
	type cand struct {
		v     *VTuner
		score int
	}
	var cands []cand
	for _, v := range d.tuners {
		if !v.idleNow() {
			continue
		}
		score := 1000 // unpinned tuners draw from the pool: last resort
		if v.pinned() && pool.HeldByScan(v.idx) {
			continue // a channel scan has it
		}
		if v.pinned() {
			class, ok := pool.Suitability(v.idx, rq.service || d.hub.IsService(rq.spec.Frequency))
			if !ok {
				continue
			}
			score = class
			if idle[v.idx] {
				score += 10 // would cut short a lingering stream
			}
			if _, foreign := v.foreignStatus(); foreign {
				score += 100 // used by another application
			}
		}
		cands = append(cands, cand{v, score})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score < cands[j].score })
	var lastErr error = errBusyAll
	for _, c := range cands {
		if !c.v.tryReserve() {
			continue
		}
		v, sub, err := start(c.v)
		if err == nil {
			return v, sub, nil
		}
		switch {
		case errors.Is(err, upstream.ErrTunerBusy), errors.Is(err, upstream.ErrTunerUnsuitable):
			lastErr = err // try the next tuner
		case errors.Is(err, upstream.ErrNoTuner):
			return nil, nil, errBusyAll // the pool is exhausted; more tries will not help
		default:
			return nil, nil, err
		}
	}
	return nil, nil, lastErr
}

var (
	errBusyAll        = errors.New("all tuners in use")
	errNotFound       = errors.New("not found")
	errUnknownChannel = errors.New("unknown virtual channel")
	errBadChannel     = errors.New("unrecognised channel")
)

func (d *Device) handleStream(w http.ResponseWriter, r *http.Request) {
	which, ch := r.PathValue("tuner"), r.PathValue("channel")
	if which != "auto" && !strings.HasPrefix(which, "tuner") {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	rq, err := d.parseChannelPath(ctx, ch)
	var v *VTuner
	var sub *hub.Sub
	if err == nil {
		v, sub, err = d.openStream(ctx, which, rq)
	}
	cancel()
	if err != nil {
		switch {
		case errors.Is(err, errUnknownChannel):
			httpErr(w, http.StatusNotFound, "801 Unknown Channel")
		case errors.Is(err, errBadChannel):
			httpErr(w, http.StatusNotFound, "")
		case errors.Is(err, errNotFound):
			httpErr(w, http.StatusNotFound, "")
		case errors.Is(err, ErrBusy):
			httpErr(w, http.StatusServiceUnavailable, "804 Tuner In Use")
		case errors.Is(err, errBusyAll):
			httpErr(w, http.StatusServiceUnavailable, "805 All Tuners In Use")
		case errors.Is(err, upstream.ErrTunerBusy):
			httpErr(w, http.StatusServiceUnavailable, "804 Tuner In Use")
		case errors.Is(err, upstream.ErrTunerUnsuitable):
			httpErr(w, http.StatusServiceUnavailable, "806 Tune Failed")
		case errors.Is(err, hub.ErrNeedsProgram):
			httpErr(w, http.StatusNotFound, "")
		case errors.Is(err, hub.ErrNoSignal):
			httpErr(w, http.StatusServiceUnavailable, "807 No Video Data")
		case errors.Is(err, upstream.ErrNoTuner):
			httpErr(w, http.StatusServiceUnavailable, "805 All Tuners In Use")
		default:
			log.Printf("http stream %s: %v", ch, err)
			httpErr(w, http.StatusServiceUnavailable, "806 Tune Failed")
		}
		return
	}
	client, _, _ := net.SplitHostPort(r.RemoteAddr)
	label := rq.vchannel
	if label == "" {
		label = ch
	}
	v.noteViewer(sub, viewerMeta{client: client, label: label, since: time.Now()})
	defer v.endHTTP(sub)

	var limit <-chan time.Time
	if secs, err := strconv.Atoi(r.URL.Query().Get("duration")); err == nil && secs > 0 {
		t := time.NewTimer(time.Duration(secs) * time.Second)
		defer t.Stop()
		limit = t.C
	}
	h := w.Header()
	h.Set("Server", "HDHomeRun/1.0")
	h.Set("Content-Type", "video/mpeg")
	h.Set("Cache-Control", "no-cache")
	h.Set("Access-Control-Allow-Origin", "*")
	h["transferMode.dlna.org"] = []string{"Streaming"}
	// Tells the client which tuner it got (real devices do the same); Channels
	// uses it to poll that tuner's /tunerN/debug for its signal display.
	h["X-HDHomeRun-Resource"] = []string{fmt.Sprintf("tuner%d", v.idx)}
	w.WriteHeader(http.StatusOK)
	countStreamResponse(http.StatusOK, "")
	rc := http.NewResponseController(w)
	rc.Flush()
	pump(r.Context(), w, rc, sub, limit)
}

// pump copies a subscription to an HTTP response. Writes are coalesced into
// chunks of up to 64 KiB (or 15 ms of data) because every flush is a chunked
// write on the socket: flushing every 7-packet batch cost about a third of a
// CPU core per viewer.
func pump(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, sub *hub.Sub, limit <-chan time.Time) {
	const maxChunk = 64 << 10
	buf := make([]byte, 0, maxChunk+7*188)
	tick := time.NewTicker(15 * time.Millisecond)
	defer tick.Stop()
	flush := func() bool {
		if len(buf) == 0 {
			return true
		}
		rc.SetWriteDeadline(time.Now().Add(20 * time.Second))
		_, err := w.Write(buf)
		buf = buf[:0]
		return err == nil && rc.Flush() == nil
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-limit:
			flush()
			return
		case b := <-sub.C:
			buf = append(buf, b...)
			sub.Recycle(b)
			if len(buf) >= maxChunk && !flush() {
				return
			}
		case <-tick.C:
			if !flush() {
				return
			}
		}
	}
}
