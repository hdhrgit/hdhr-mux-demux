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
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/vdev"
)

// The Channels page shows every place a channel is received, with the signal there and
// how far ahead its guide goes, learned from the guide pass that reads each multiplex.
func TestChannelsPageShowsWhereChannelsAre(t *testing.T) {
	testChannels = append(append([]fakehdhr.Channel{}, channels...),
		fakehdhr.Channel{Frequency: 191000000, TSID: 9, Programs: []fakehdhr.Program{{Number: 5, VChannel: "7.1", Name: "ALPHA-2"}, {Number: 6, VChannel: "9.9", Name: "ZETA"}}})
	defer func() { testChannels = nil }()
	e := setup(t, []int{3}, 3, func(c *vdev.Config) { c.GuideHold = 2 * time.Second })
	e.lu.Replace([]lineup.Entry{
		{GuideNumber: "7.1", GuideName: "ALPHA", Frequency: 177000000, Program: 1},
		{GuideNumber: "14.1", GuideName: "GAMMA", Frequency: 473000000, Program: 1},
		{GuideNumber: "9.9", GuideName: "ZETA", Frequency: 191000000, Program: 6},
	})
	if _, _, err := e.dev.GuidePass(context.Background()); err != nil {
		t.Fatal(err)
	}
	type loc struct {
		Frequency   uint64
		Program     uint16
		Name        string
		Chosen      bool
		Guide       bool
		GuideHours  float64
		GuideEvents int
		Signal      *struct{ SS, SNQ, SEQ int }
	}
	var v struct {
		Duplicates, Known int
		Channels          []struct {
			Number    string
			Locations []loc
		}
	}
	getJSON(t, e.api+"/ui/api/channels", &v)
	if v.Known != 3 || v.Duplicates != 1 {
		t.Errorf("known %d RF channels, %d duplicates; want 3 and 1", v.Known, v.Duplicates)
	}
	for _, c := range v.Channels {
		switch c.Number {
		case "7.1":
			if len(c.Locations) != 2 || c.Locations[0].Frequency != 177000000 || c.Locations[1].Frequency != 191000000 {
				t.Fatalf("7.1 places: %+v", c.Locations)
			}
			if !c.Locations[0].Chosen || c.Locations[1].Chosen {
				t.Errorf("only the lineup's place is chosen: %+v", c.Locations)
			}
			for _, l := range c.Locations {
				if !l.Guide || l.GuideEvents == 0 || l.GuideHours <= 0 || l.Name == "" || l.Signal == nil || l.Signal.SS == 0 {
					t.Errorf("7.1 at %d Hz is missing what the pass read: %+v", l.Frequency, l)
				}
			}
			if c.Locations[1].Name != "ALPHA-2" || c.Locations[1].Program != 5 {
				t.Errorf("the other place has the name and program the multiplex gives it: %+v", c.Locations[1])
			}
		case "14.1":
			if len(c.Locations) != 1 {
				t.Errorf("14.1 should be in one place: %+v", c.Locations)
			}
		}
	}
}

// What a person chooses on the Channels page decides where /auto/ tunes a channel that
// is received on two RF channels, and which RF channel's guide is served for it.
func TestChannelChoicesDecideWhereAutoTunesAndWhichGuide(t *testing.T) {
	testChannels = append(append([]fakehdhr.Channel{}, channels...),
		fakehdhr.Channel{Frequency: 191000000, TSID: 9, Programs: []fakehdhr.Program{{Number: 5, VChannel: "7.1", Name: "ALPHA-2"}, {Number: 6, VChannel: "9.9", Name: "ZETA"}}})
	defer func() { testChannels = nil }()
	e := setup(t, []int{4}, 4, func(c *vdev.Config) { c.GuideHold = 2 * time.Second })
	e.lu.Replace([]lineup.Entry{
		{GuideNumber: "7.1", GuideName: "ALPHA", Frequency: 177000000, Program: 1},
		{GuideNumber: "9.9", GuideName: "ZETA", Frequency: 191000000, Program: 6},
	})
	if _, _, err := e.dev.GuidePass(context.Background()); err != nil {
		t.Fatal(err)
	}
	put := func(number, body string) (int, string) {
		req, _ := http.NewRequest("PUT", e.api+"/ui/api/channels/"+number, strings.NewReader(body))
		req.Header.Set("X-Requested-With", "hdhr-mux-ui")
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	programWatched := func() uint16 { // which programme /auto/v7.1 delivers: 1 (at 177 MHz) or 5 (at 191 MHz)
		c, rc := e.open("/auto/v7.1")
		if c == nil {
			t.Fatalf("/auto/v7.1 refused: %d %v", rc.StatusCode, rc.Header)
		}
		defer c.close()
		an := analyze(t, c.read(t, 188*60))
		for p := range an.esProgs {
			return p
		}
		t.Fatal("no programme")
		return 0
	}
	if p := programWatched(); p != 1 {
		t.Fatalf("by default the lineup's place (177 MHz, programme 1): got %d", p)
	}
	// first choice: the other place
	if code, body := put("7.1", `{"video":191000000}`); code != 200 {
		t.Fatalf("PUT: %d %s", code, body)
	}
	waitFor(t, "the first choice is used", 3*time.Second, func() bool { return programWatched() == 5 })
	// switched off: the other place is used even when it is not the first choice
	put("7.1", `{"off":[191000000]}`)
	waitFor(t, "a switched-off place is not used", 3*time.Second, func() bool { return programWatched() == 1 })

	// the guide: automatic, then a chosen place
	titles := func() string {
		var out []map[string]any
		getJSON(t, e.api+"/epg.json", &out)
		for _, c := range out {
			if c["GuideNumber"] == "7.1" {
				if evs, _ := c["Events"].([]any); len(evs) > 0 {
					return fmt.Sprint(evs[0].(map[string]any)["Title"])
				}
			}
		}
		return ""
	}
	put("7.1", `{}`)
	auto := titles()
	put("7.1", `{"guide":191000000}`)
	chosen191 := titles()
	put("7.1", `{"guide":177000000}`)
	chosen177 := titles()
	if chosen191 == "" || chosen177 == "" || chosen191 == chosen177 || (auto != chosen191 && auto != chosen177) {
		t.Errorf("guide titles: automatic %q, 191 MHz %q, 177 MHz %q", auto, chosen191, chosen177)
	}

	// the last RF channel cannot be switched off here: whether a channel exists is up to the Lineup
	if code, _ := put("7.1", `{"off":[177000000,191000000]}`); code != 400 {
		t.Errorf("switching off every place was accepted: %d", code)
	}
	if _, rc := e.open("/auto/v7.1"); rc.StatusCode != 200 {
		t.Errorf("the channel should still tune: %d", rc.StatusCode)
	}
	// and what is not allowed
	if code, _ := put("7.1", `{"off":[999000000]}`); code != 400 {
		t.Errorf("an RF channel the channel is not received on was accepted: %d", code)
	}
	if code, _ := put("7.1", `{"off":[177000000],"video":177000000}`); code != 400 {
		t.Errorf("a first choice that is switched off was accepted: %d", code)
	}
	if code, _ := put("1.1", `{}`); code != 404 {
		t.Errorf("an unknown channel: %d", code)
	}
	req, _ := http.NewRequest("PUT", e.api+"/ui/api/channels/7.1", strings.NewReader(`{}`))
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 403 {
		t.Errorf("a PUT without the admin header: %v %v", err, r)
	}
}

// Channels merged into a group are one for the tuners: a request for any of them rides a
// stream that carries any of them, and with nothing open it tunes where the requested one is.
func TestMergedChannelsShareTheTuner(t *testing.T) {
	testChannels = append(append([]fakehdhr.Channel{}, channels...),
		fakehdhr.Channel{Frequency: 191000000, TSID: 9, Programs: []fakehdhr.Program{{Number: 6, VChannel: "9.9", Name: "ZETA"}}})
	defer func() { testChannels = nil }()
	e := setup(t, []int{4}, 4, nil)
	e.lu.Replace([]lineup.Entry{
		{GuideNumber: "7.1", GuideName: "ALPHA", Frequency: 177000000, Program: 1},
		{GuideNumber: "9.9", GuideName: "ZETA", Frequency: 191000000, Program: 6},
	})
	tuned := func() int {
		n := 0
		for _, c := range e.fakes[0].TunedChannels() {
			if c != "none" {
				n++
			}
		}
		return n
	}
	// not merged: two channels, two tuners
	a, ra := e.open("/auto/v9.9")
	if ra.StatusCode != 200 {
		t.Fatalf("9.9: %d", ra.StatusCode)
	}
	b, rb := e.open("/auto/v7.1")
	if rb.StatusCode != 200 {
		t.Fatalf("7.1: %d", rb.StatusCode)
	}
	if n := tuned(); n != 2 {
		t.Fatalf("unmerged channels used %d tuners, want 2", n)
	}
	a.cancel()
	b.cancel()
	waitFor(t, "the tuners to be released", 5*time.Second, func() bool { return tuned() == 0 })

	put := func(body string) int {
		req, _ := http.NewRequest("PUT", e.api+"/ui/api/merges", strings.NewReader(body))
		req.Header.Set("X-Requested-With", "hdhr-mux-ui")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := put(`{"groups":[["7.1","9.9"]]}`); c != 200 {
		t.Fatalf("PUT merges: %d", c)
	}
	if put(`{"groups":[["7.1","1.1"]]}`) != 400 || put(`{"groups":[["7.1","9.9"],["9.9","7.1"]]}`) != 400 {
		t.Error("an unknown channel, or a channel in two groups, should be refused")
	}
	var got struct {
		Groups [][]struct{ Number string }
	}
	getJSON(t, e.api+"/ui/api/merges", &got)
	if len(got.Groups) != 1 || len(got.Groups[0]) != 2 {
		t.Fatalf("groups %+v", got.Groups)
	}

	// merged: whichever is asked for rides the stream that is open
	a, ra = e.open("/auto/v9.9")
	if ra.StatusCode != 200 {
		t.Fatalf("9.9: %d", ra.StatusCode)
	}
	b, rb = e.open("/auto/v7.1")
	if rb.StatusCode != 200 {
		t.Fatalf("7.1: %d", rb.StatusCode)
	}
	if n := tuned(); n != 1 {
		t.Errorf("merged channels used %d tuners, want 1", n)
	}
	a.cancel()
	b.cancel()
}

// By default only the lineup's RF channel is used for a channel received on two: the other
// shows as not ticked on the Advanced tab until a person allows it.
func TestOtherRFChannelsAreOffUntilAllowed(t *testing.T) {
	testChannels = append(append([]fakehdhr.Channel{}, channels...),
		fakehdhr.Channel{Frequency: 191000000, TSID: 9, Programs: []fakehdhr.Program{{Number: 5, VChannel: "7.1", Name: "ALPHA-2"}, {Number: 6, VChannel: "9.9", Name: "ZETA"}}})
	defer func() { testChannels = nil }()
	e := setup(t, []int{3}, 3, func(c *vdev.Config) { c.GuideHold = 2 * time.Second })
	e.lu.Replace([]lineup.Entry{
		{GuideNumber: "7.1", GuideName: "ALPHA", Frequency: 177000000, Program: 1},
		{GuideNumber: "9.9", GuideName: "ZETA", Frequency: 191000000, Program: 6},
	})
	if _, _, err := e.dev.GuidePass(context.Background()); err != nil {
		t.Fatal(err)
	}
	offs := func() map[uint64]bool {
		var v struct {
			Channels []struct {
				Number    string
				Locations []struct {
					Frequency uint64
					Off       bool
				}
			}
		}
		getJSON(t, e.api+"/ui/api/channels", &v)
		out := map[uint64]bool{}
		for _, c := range v.Channels {
			if c.Number == "7.1" {
				for _, l := range c.Locations {
					out[l.Frequency] = l.Off
				}
			}
		}
		return out
	}
	if o := offs(); len(o) != 2 || o[177000000] || !o[191000000] {
		t.Fatalf("by default only the lineup's place is ticked: %v", o)
	}
	req, _ := http.NewRequest("PUT", e.api+"/ui/api/channels/7.1", strings.NewReader(`{"off":[]}`))
	req.Header.Set("X-Requested-With", "hdhr-mux-ui")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("PUT: %v %v", r, err)
	}
	if o := offs(); o[177000000] || o[191000000] {
		t.Errorf("after ticking the other place both are on: %v", o)
	}
}
