package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"hdhrmux/internal/fakehdhr"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/vdev"
)

// Watching a channel collects the guide of its whole multiplex, and /epg
// serves it as XMLTV (and JSON).
func TestGuideFromRunningStream(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	if x := fetchEPG(t, e, ""); len(x.Programmes) != 0 {
		t.Fatalf("a guide before anything was heard: %+v", x.Programmes)
	}
	c, _ := e.open("/auto/v7.1")
	defer c.close()
	c.read(t, 188*50)
	waitFor(t, "guide collected from the stream", 10*time.Second, func() bool {
		return len(fetchEPG(t, e, "?channel=7.2").Programmes) >= 2
	})

	x := fetchEPG(t, e, "")
	titles := map[string]string{}
	descs := map[string]string{}
	for _, p := range x.Programmes {
		titles[p.Channel+"/"+p.Title] = p.Start
		descs[p.Channel+"/"+p.Title] = p.Desc
		if p.Start >= p.Stop || !strings.HasSuffix(p.Start, " +0000") {
			t.Errorf("bad times %q-%q", p.Start, p.Stop)
		}
	}
	for _, want := range []string{"7.1/Now Show ALPHA", "7.1/Next Show ALPHA", "7.2/Now Show BETA"} {
		if _, ok := titles[want]; !ok {
			t.Errorf("missing %q in %v", want, titles)
		}
	}
	if descs["7.2/Now Show BETA"] != "About Now Show BETA" {
		t.Errorf("description %q", descs["7.2/Now Show BETA"])
	}
	if len(x.Channels) < 2 || x.Channels[0].ID != "7.1" || len(x.Channels[0].Names) != 3 || x.Channels[0].Names[0] != "7.1 ALPHA" {
		t.Errorf("channels %+v", x.Channels)
	}
	for _, p := range fetchEPG(t, e, "?channel=7.2").Programmes {
		if p.Channel != "7.2" {
			t.Errorf("channel filter leaked %q", p.Channel)
		}
	}
	for _, p := range x.Programmes {
		if p.Channel == "14.1" {
			t.Error("a multiplex nobody listened to has no guide yet")
		}
	}
	var js []map[string]any
	getJSON(t, e.api+"/epg.json?channel=7.1", &js)
	if len(js) != 1 || js[0]["GuideNumber"] != "7.1" || len(js[0]["Events"].([]any)) < 2 {
		t.Fatalf("epg.json: %v", js)
	}
}

// A pass listens to the multiplexes nobody is watching, using spare tuners,
// and a viewer who needs the tuner gets it at once.
func TestGuidePassAndYielding(t *testing.T) {
	e := setup(t, []int{1}, 2, func(c *vdev.Config) { c.GuideHold = 4 * time.Second })
	e.lu.Replace(append(append([]lineup.Entry{}, testLineup...), atsc3Lineup...))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if n, total, err := e.dev.GuidePass(context.Background()); err != nil || n != 2 || total != 2 {
			t.Errorf("pass: %d/%d %v (the ATSC 3.0 channel has no PSIP and is not visited)", n, total, err)
		}
	}()
	time.Sleep(700 * time.Millisecond)
	// the single tuner is held by the pass; a viewer must not have to wait for it
	began := time.Now()
	c, resp := e.open("/auto/v14.1")
	if c == nil {
		t.Fatalf("viewer refused during a guide pass: %d %v", resp.StatusCode, resp.Header)
	}
	c.read(t, 188*50)
	if d := time.Since(began); d > 1500*time.Millisecond {
		t.Fatalf("viewer waited %v for the guide pass", d)
	}
	c.close()
	<-done

	got := fetchEPG(t, e, "")
	seen := map[string]bool{}
	for _, p := range got.Programmes {
		seen[p.Channel] = true
	}
	for _, ch := range []string{"7.1", "7.2", "14.1", "14.3"} {
		if !seen[ch] {
			t.Errorf("no guide for %s after the pass (have %v)", ch, seen)
		}
	}
	if seen["104.1"] {
		t.Error("ATSC 3.0 services carry no PSIP guide")
	}
	if resp, err := http.Post(e.api+"/epg/refresh", "", nil); err != nil || resp.StatusCode != 202 {
		t.Fatalf("POST /epg/refresh: %v %v", resp, err)
	}
}

// An unlocked guide pass holds no lock: another application can take the
// tuner at any moment, the pass lets go without touching what the new user set
// up, and the new user's stream carries on.
func TestUnlockedGuidePassYieldsToOutsiders(t *testing.T) {
	e := setup(t, []int{1}, 1, func(c *vdev.Config) { c.GuideHold = 8 * time.Second; c.GuideUnlocked = true })
	e.lu.Replace(append([]lineup.Entry(nil), testLineup[2:]...)) // 14.1, 14.3 on 473 MHz only
	raw := hdhrproto.NewControl(e.fakes[0].Addr())
	defer raw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.dev.GuidePass(ctx)
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, "pass listening without a lock", 5*time.Second, func() bool {
		tg, _ := raw.Get("/tuner0/target")
		lk, _ := raw.Get("/tuner0/lockkey")
		return strings.HasPrefix(tg, "http://") && lk == "none"
	})
	if lk, _ := raw.Get("/tuner0/lockkey"); lk != "none" {
		t.Fatalf("the guide pass holds a lock (%q)", lk)
	}

	// an outsider takes the tuner
	began := time.Now()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/tuner0/ch177000000", e.fakes[0].StreamPort()))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("an outsider could not take an unlocked tuner: %v %v", resp, err)
	}
	defer resp.Body.Close()
	// (the pass goes on to listen to other RF channels, so it is the stream that is watched)
	waitFor(t, "the pass to give up the tuner", 5*time.Second, func() bool {
		for _, s := range e.hub.Streams() {
			if s.Background && s.Spec.Frequency == 473000000 {
				return false
			}
		}
		return true
	})
	if time.Since(began) > 4*time.Second {
		t.Fatalf("the pass took %v to notice", time.Since(began))
	}
	// the outsider's stream goes on, and nothing of theirs was cleared
	buf := make([]byte, 188*100)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("the outsider's stream was disturbed: %v", err)
	}
	if ch, _ := raw.Get("/tuner0/channel"); !strings.Contains(ch, "177000000") {
		t.Fatalf("the pass cleared the new user's channel: %q", ch)
	}
}

// A new installation knows no channel's frequency; the first guide pass finds out by
// tuning channels by name (one probe teaches the whole multiplex) instead of
// finding nothing to listen to.
func TestGuidePassLearnsWhereChannelsAre(t *testing.T) {
	e := setup(t, []int{2}, 2, func(c *vdev.Config) { c.GuideHold = 2 * time.Second })
	e.lu.Replace([]lineup.Entry{
		{GuideNumber: "7.1", GuideName: "ALPHA"}, {GuideNumber: "7.2", GuideName: "BETA"},
		{GuideNumber: "14.1", GuideName: "GAMMA"}, {GuideNumber: "14.3", GuideName: "DELTA"},
	})
	done, total, err := e.dev.GuidePass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for g, want := range map[string]uint64{"7.1": 177000000, "7.2": 177000000, "14.1": 473000000, "14.3": 473000000} {
		if en, _ := e.lu.Find(g); en.Frequency != want {
			t.Errorf("%s at %d Hz after the pass, want %d", g, en.Frequency, want)
		}
	}
	if total != 2 || done != 2 {
		t.Errorf("the pass listened to %d of %d multiplexes, want 2 of 2", done, total)
	}
}

// The devices' lineups place every station once. A station carried on another RF
// channel is found because a guide pass also listens to the RF channels the lineup
// does not mention.
func TestGuidePassFindsStationsOnOtherRFChannels(t *testing.T) {
	testChannels = append(append([]fakehdhr.Channel{}, channels...),
		fakehdhr.Channel{Frequency: 183000000, TSID: 9, Programs: []fakehdhr.Program{{Number: 5, VChannel: "7.1", Name: "ALPHA-3"}}})
	defer func() { testChannels = nil }()
	e := setup(t, []int{3}, 3, func(c *vdev.Config) { c.GuideHold = 2 * time.Second })
	e.lu.Replace([]lineup.Entry{{GuideNumber: "7.1", GuideName: "ALPHA", Frequency: 177000000, Program: 1}})
	if _, _, err := e.dev.GuidePass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var v struct {
		Channels []struct {
			Number    string
			Locations []struct {
				Frequency uint64
				Name      string
			}
		}
	}
	getJSON(t, e.api+"/ui/api/channels", &v)
	for _, c := range v.Channels {
		if c.Number != "7.1" {
			continue
		}
		if len(c.Locations) != 2 || c.Locations[0].Frequency != 177000000 || c.Locations[1].Frequency != 183000000 || c.Locations[1].Name != "ALPHA-3" {
			t.Fatalf("7.1 places: %+v", c.Locations)
		}
		return
	}
	t.Fatal("7.1 is not on the Channels page")
}

// Somebody clearing the tuner under an unlocked guide pass is a takeover like any other:
// the pass lets go and does not go looking for another tuner (a "failover").
func TestUnlockedGuidePassLetsGoWhenTheTunerIsCleared(t *testing.T) {
	e := setup(t, []int{2}, 2, func(c *vdev.Config) { c.GuideHold = 8 * time.Second; c.GuideUnlocked = true })
	e.lu.Replace(append([]lineup.Entry(nil), testLineup[2:]...)) // 14.1, 14.3 on 473 MHz only
	raw := hdhrproto.NewControl(e.fakes[0].Addr())
	defer raw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.dev.GuidePass(ctx)
	}()
	defer func() { cancel(); <-done }()

	tuner := -1
	waitFor(t, "pass listening without a lock", 5*time.Second, func() bool {
		for i := 0; i < 2; i++ {
			tg, _ := raw.Get(fmt.Sprintf("/tuner%d/target", i))
			lk, _ := raw.Get(fmt.Sprintf("/tuner%d/lockkey", i))
			if strings.HasPrefix(tg, "http://") && lk == "none" {
				tuner = i
				return true
			}
		}
		return false
	})
	if _, err := raw.Set(fmt.Sprintf("/tuner%d/channel", tuner), "none", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pass to let go", 5*time.Second, func() bool {
		for _, s := range e.hub.Streams() {
			if s.Background && s.Spec.Frequency == 473000000 {
				return false
			}
		}
		return true
	})
	var m struct{ Failovers float64 }
	body, _ := io.ReadAll(func() io.Reader { r, _ := http.Get(e.api + "/metrics"); return r.Body }())
	for _, ln := range strings.Split(string(body), "\n") {
		if v, ok := strings.CutPrefix(ln, "hdhrmux_stream_failovers_total "); ok {
			fmt.Sscan(v, &m.Failovers)
		}
	}
	if m.Failovers != 0 {
		t.Errorf("the pass looked for another tuner (%v failovers)", m.Failovers)
	}
}
