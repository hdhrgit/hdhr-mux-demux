package vdev

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"hdhrmux/internal/chanmap"
	"hdhrmux/internal/guide"
	"hdhrmux/internal/psip"
)

// The programme guide. ATSC 1.0 stations broadcast it in every multiplex
// (PSIP). Whatever a running stream carries is collected as it goes by, and a
// pass over the multiplexes nobody is watching refreshes the rest using spare
// tuners. It is served as XMLTV on /epg and as JSON on /epg.json.

type guideState struct {
	mu      sync.Mutex
	store   *guide.Store
	running bool
	last    time.Time
}

// SetGuide attaches the guide store and starts feeding it from the streams.
func (d *Device) SetGuide(s *guide.Store) {
	d.guide.mu.Lock()
	d.guide.store = s
	d.guide.mu.Unlock()
	d.hub.SetGuideSink(func(g psip.Guide) { s.Merge(g, time.Now()) })
}

func (d *Device) guideStore() *guide.Store {
	d.guide.mu.Lock()
	defer d.guide.mu.Unlock()
	return d.guide.store
}

// GuidePass tunes every ATSC 1.0 multiplex in the lineup that nobody is
// watching, a few at a time on free tuners, and collects its guide. It reports
// how many multiplexes it could listen to.
func (d *Device) GuidePass(ctx context.Context) (done, total int, err error) {
	d.guide.mu.Lock()
	if d.guide.running {
		d.guide.mu.Unlock()
		return 0, 0, errors.New("a guide pass is already running")
	}
	d.guide.running = true
	d.guide.mu.Unlock()
	defer func() {
		d.guide.mu.Lock()
		d.guide.running, d.guide.last = false, time.Now()
		d.guide.mu.Unlock()
	}()

	seen := map[uint64]bool{}
	var freqs []uint64
	for _, e := range d.lineup.Entries(false) {
		if e.Frequency != 0 && e.Modulation != "atsc3" && !d.hub.IsService(e.Frequency) && !seen[e.Frequency] {
			seen[e.Frequency] = true
			freqs = append(freqs, e.Frequency)
		}
	}
	sort.Slice(freqs, func(i, j int) bool { return freqs[i] < freqs[j] })
	hold := d.cfg.GuideHold
	if hold == 0 {
		hold = 45 * time.Second
	}
	par := d.cfg.GuideParallel
	if par <= 0 {
		par = 3
	}
	work := make(chan uint64)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < par; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range work {
				if err := d.hub.CollectGuide(ctx, chanmap.Spec{Modulation: "auto", Frequency: f}, hold, d.cfg.GuideUnlocked); err != nil {
					log.Printf("guide: %d Hz: %v", f, err)
					continue
				}
				mu.Lock()
				done++
				mu.Unlock()
			}
		}()
	}
loop:
	for _, f := range freqs {
		select {
		case work <- f:
		case <-ctx.Done():
			break loop
		}
	}
	close(work)
	wg.Wait()
	if s := d.guideStore(); s != nil {
		s.Flush()
		ch, ev := s.Summary()
		log.Printf("guide: pass finished, %d of %d multiplexes heard; %d channels, %d events", done, len(freqs), ch, ev)
	}
	return done, len(freqs), ctx.Err()
}

func (d *Device) guideLoop() {
	defer d.wg.Done()
	delay := d.cfg.GuideStartDelay
	if delay == 0 {
		delay = 90 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-d.quit:
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-d.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		d.GuidePass(ctx)
		cancel()
		timer.Reset(d.cfg.GuideRefresh)
	}
}

// ---- serving -------------------------------------------------------------

type epgChannel struct {
	GuideNumber string
	GuideName   string
	Events      []epgEvent
}

type epgEvent struct {
	ID          uint16
	Start       time.Time
	End         time.Time
	Title       string
	Description string `json:",omitempty"`
	Language    string `json:",omitempty"`
}

// epgData collects the channels (in lineup order) and the events a request
// asks for: ?channel=7.1 (repeatable), ?hours=N ahead (default 72), ?all=1 to
// include channels without a guide.
func (d *Device) epgData(r *http.Request) []epgChannel {
	s := d.guideStore()
	q := r.URL.Query()
	hours := 72
	if h, err := strconv.Atoi(q.Get("hours")); err == nil && h > 0 {
		hours = h
	}
	want := map[string]bool{}
	for _, c := range q["channel"] {
		want[c] = true
	}
	now := time.Now()
	from, to := now.Add(-time.Hour), now.Add(time.Duration(hours)*time.Hour)
	var out []epgChannel
	for _, e := range d.lineup.Entries(false) {
		if e.DRM || (len(want) > 0 && !want[e.GuideNumber]) {
			continue
		}
		c := epgChannel{GuideNumber: e.GuideNumber, GuideName: e.GuideName}
		if s != nil {
			all := s.Events(e.GuideNumber)
			langs := psip.EventLanguages(all) // the broadcast's own labels are unreliable
			for i, ev := range all {
				if ev.End().After(from) && ev.Start.Before(to) {
					c.Events = append(c.Events, epgEvent{ev.ID, ev.Start, ev.End(), ev.Title, ev.Description, langs[i]})
				}
			}
		}
		if len(c.Events) > 0 || q.Get("all") == "1" {
			out = append(out, c)
		}
	}
	return out
}

func xmltvTime(t time.Time) string { return t.UTC().Format("20060102150405") + " +0000" }

func xmltvLang(l string) string {
	switch l {
	case "eng":
		return "en"
	case "spa":
		return "es"
	case "fra", "fre":
		return "fr"
	}
	return l
}

func (d *Device) handleEPG(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	var b strings.Builder
	esc := func(s string) string {
		var sb strings.Builder
		xml.EscapeText(&sb, []byte(s))
		return sb.String()
	}
	b.WriteString(xml.Header)
	b.WriteString(`<tv generator-info-name="hdhr-mux">` + "\n")
	chans := d.epgData(r)
	for _, c := range chans {
		fmt.Fprintf(&b, "  <channel id=%q>\n    <display-name>%s %s</display-name>\n    <display-name>%s</display-name>\n    <display-name>%s</display-name>\n  </channel>\n",
			c.GuideNumber, esc(c.GuideNumber), esc(c.GuideName), esc(c.GuideName), esc(c.GuideNumber))
	}
	for _, c := range chans {
		for _, e := range c.Events {
			lang := ""
			if e.Language != "" {
				lang = fmt.Sprintf(` lang=%q`, xmltvLang(e.Language))
			}
			fmt.Fprintf(&b, "  <programme start=%q stop=%q channel=%q>\n    <title%s>%s</title>\n", xmltvTime(e.Start), xmltvTime(e.End), c.GuideNumber, lang, esc(e.Title))
			if e.Description != "" {
				fmt.Fprintf(&b, "    <desc%s>%s</desc>\n", lang, esc(e.Description))
			}
			b.WriteString("  </programme>\n")
		}
	}
	b.WriteString("</tv>\n")
	w.Write([]byte(b.String()))
}

func (d *Device) handleEPGJSON(w http.ResponseWriter, r *http.Request) {
	chans := d.epgData(r)
	if chans == nil {
		chans = []epgChannel{}
	}
	writeJSON(w, chans)
}

func (d *Device) handleEPGRefresh(w http.ResponseWriter, r *http.Request) {
	d.guide.mu.Lock()
	running := d.guide.running
	d.guide.mu.Unlock()
	if running {
		http.Error(w, "a guide pass is already running", http.StatusConflict)
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-d.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		d.GuidePass(ctx)
	}()
	w.WriteHeader(http.StatusAccepted)
}
