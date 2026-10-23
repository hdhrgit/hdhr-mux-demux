package e2e

import (
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/vdev"
)

// A control client that picks PIDs (tvheadend) never says which channel it is
// watching; the activity page works it out from the programme its PIDs belong to.
func TestActivityNamesTheChannelAPIDClientWatches(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	set := func(name, val string) {
		t.Helper()
		if _, err := ctl.Set(name, val, 0); err != nil {
			t.Fatalf("set %s %s: %v", name, val, err)
		}
	}
	set("/tuner0/channel", "auto:177000000")
	// the PAT, the PMTs it wants to look at, and the video and audio of the second programme (7.2)
	// and the video of the first (which is not complete: its audio is missing)
	set("/tuner0/filter", "0x0000 0x0100 0x0110 0x0101 0x0111-0x0112")
	set("/tuner0/target", "udp://"+rx.LocalAddr().String())

	var a struct {
		Tuners []struct {
			State   string
			Viewers []struct{ Channel, Name string }
		}
	}
	waitFor(t, "the programme behind the filter", 6*time.Second, func() bool {
		a.Tuners = nil
		getJSON(t, e.api+"/ui/api/activity", &a)
		return len(a.Tuners) > 0 && len(a.Tuners[0].Viewers) == 1 && a.Tuners[0].Viewers[0].Channel == "7.2"
	})
	if v := a.Tuners[0].Viewers[0]; v.Name != "BETA" {
		t.Errorf("viewer %+v", v)
	}
	// every channel that is completely included is listed, one row each
	set("/tuner0/filter", "0x0000 0x0101-0x0102 0x0111-0x0112")
	waitFor(t, "both programmes named", 6*time.Second, func() bool {
		a.Tuners = nil
		getJSON(t, e.api+"/ui/api/activity", &a)
		return len(a.Tuners[0].Viewers) == 2 && a.Tuners[0].Viewers[0].Channel == "7.1" && a.Tuners[0].Viewers[1].Channel == "7.2"
	})
	// discovery-only filters (PMTs but no elementary streams) complete no programme:
	// the client is collecting tables, which is a scan, not a channel
	set("/tuner0/filter", "0x0000 0x0100 0x0110")
	waitFor(t, "a scan, not a channel", 6*time.Second, func() bool {
		a.Tuners = nil
		getJSON(t, e.api+"/ui/api/activity", &a)
		return len(a.Tuners[0].Viewers) == 1 && a.Tuners[0].Viewers[0].Channel == "EPG scan" && a.Tuners[0].Viewers[0].Name == "177 MHz"
	})
}

// A device that receives a station on another frequency than the lineup has it
// (another antenna, a cable feed): the Activity page still names what a
// PID-selecting client watches, from the multiplex's own channel table, and calls
// it a scan only when the client's PIDs complete no programme.
func TestActivityNamesProgrammesTheLineupDoesNotPlaceHere(t *testing.T) {
	e := setup(t, []int{2}, 2, nil)
	moved := []lineup.Entry{
		{GuideNumber: "7.1", GuideName: "ALPHA", Frequency: 999000000, Program: 1},
		{GuideNumber: "7.2", GuideName: "BETA", Frequency: 999000000, Program: 2},
	}
	e.lu.Replace(moved) // the channels are not where tuner 0 is
	ctl := hdhrproto.NewControl(e.ctl)
	defer ctl.Close()
	rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	set := func(name, val string) {
		t.Helper()
		if _, err := ctl.Set(name, val, 0); err != nil {
			t.Fatalf("set %s %s: %v", name, val, err)
		}
	}
	set("/tuner0/channel", "auto:177000000")
	set("/tuner0/filter", "0x0000 0x1ffb 0x0100-0x0102 0x0110-0x0112")
	set("/tuner0/target", "udp://"+rx.LocalAddr().String())
	var a struct {
		Tuners []struct {
			Viewers []struct {
				Channel string
				Scan    bool
			}
		}
	}
	waitFor(t, "the programmes named from the stream", 8*time.Second, func() bool {
		a.Tuners = nil
		getJSON(t, e.api+"/ui/api/activity", &a)
		if len(a.Tuners) == 0 || len(a.Tuners[0].Viewers) != 2 {
			return false
		}
		v0, v1 := a.Tuners[0].Viewers[0], a.Tuners[0].Viewers[1]
		return !v0.Scan && !v1.Scan && v0.Channel == "7.1" && v1.Channel == "7.2"
	})
}

// A tuner another application is using shows up as such on the activity page,
// with who holds it: from the device's side a program on this machine has the
// same address as the multiplexer, which the page says.
func TestActivityShowsOtherApplications(t *testing.T) {
	e := setup(t, []int{4}, 4, nil)
	foreign, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/tuner2/ch473000000", e.fakes[0].StreamPort()))
	if err != nil || foreign.StatusCode != 200 {
		t.Fatalf("foreign client: %v %v", foreign, err)
	}
	defer foreign.Body.Close()

	var a struct {
		Summary struct{ OtherTuners, TunersInUse int }
		Tuners  []struct {
			Index       int
			State       string
			Frequency   uint64
			Holder      string
			HolderLocal bool
			Transport   string
		}
	}
	waitFor(t, "the other application on the activity page", 5*time.Second, func() bool {
		a = struct {
			Summary struct{ OtherTuners, TunersInUse int }
			Tuners  []struct {
				Index       int
				State       string
				Frequency   uint64
				Holder      string
				HolderLocal bool
				Transport   string
			}
		}{}
		getJSON(t, e.api+"/ui/api/activity", &a)
		return len(a.Tuners) == 4 && a.Tuners[2].State == "other"
	})
	o := a.Tuners[2]
	if o.Frequency != 473000000 || o.Holder != "127.0.0.1" || !o.HolderLocal || o.Transport != "http" {
		t.Errorf("tuner 2: %+v", o)
	}
	if a.Summary.OtherTuners != 1 || a.Summary.TunersInUse != 0 {
		t.Errorf("summary %+v", a.Summary)
	}
	for _, i := range []int{0, 1, 3} {
		if a.Tuners[i].State != "idle" || a.Tuners[i].Holder != "" {
			t.Errorf("tuner %d: %+v", i, a.Tuners[i])
		}
	}
}

// The activity page says how the multiplexer itself is doing.
func TestActivityReportsTheSystem(t *testing.T) {
	e := setup(t, []int{1}, 1, nil)
	var a struct {
		System struct {
			Cores      int
			RSSBytes   uint64
			Goroutines int
			OpenFds    int
		}
	}
	getJSON(t, e.api+"/ui/api/activity", &a)
	if a.System.Cores < 1 || a.System.RSSBytes == 0 || a.System.Goroutines == 0 || a.System.OpenFds == 0 {
		t.Errorf("system section %+v", a.System)
	}
}

// What another application is watching is read from the device: the program it
// selected on the tuner, named with the channel number the station gives it.
func TestActivityShowsWhatOtherApplicationsWatch(t *testing.T) {
	e := setup(t, []int{4}, 4, nil)
	e.lu.Replace(testLineup)
	foreign, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/tuner2/ch473000000-3", e.fakes[0].StreamPort()))
	if err != nil || foreign.StatusCode != 200 {
		t.Fatalf("foreign client: %v %v", foreign, err)
	}
	defer foreign.Body.Close()
	type watching struct{ Channel, Name string }
	var a struct {
		Tuners []struct {
			State    string
			Watching []watching
		}
	}
	waitFor(t, "the channel the other application watches", 8*time.Second, func() bool {
		a.Tuners = nil
		getJSON(t, e.api+"/ui/api/activity", &a)
		return len(a.Tuners) == 4 && len(a.Tuners[2].Watching) == 1 && a.Tuners[2].Watching[0].Channel == "14.3"
	})
	if w := a.Tuners[2].Watching[0]; w.Channel != "14.3" || w.Name == "" {
		t.Errorf("watching %+v, want 14.3 and its name", w)
	}
}

// The Activity page is told how to name the physical tuners (Settings, Miscellaneous).
func TestActivityTellsHowToNameTuners(t *testing.T) {
	e := setup(t, []int{1}, 1, func(c *vdev.Config) { c.TunerNames = "ip" })
	var a struct{ TunerNames string }
	getJSON(t, e.api+"/ui/api/activity", &a)
	if a.TunerNames != "ip" {
		t.Errorf("tunerNames %q, want ip", a.TunerNames)
	}
	e2 := setup(t, []int{1}, 1, nil)
	getJSON(t, e2.api+"/ui/api/activity", &a)
	if a.TunerNames != "" && a.TunerNames != "id" {
		t.Errorf("default tunerNames %q", a.TunerNames)
	}
}

// An application that takes the whole multiplex (no PID filter) is shown as that, not as
// every channel on it.
func TestActivityShowsFullMultiplexForUnfilteredTuner(t *testing.T) {
	e := setup(t, []int{4}, 4, nil)
	e.lu.Replace(testLineup)
	foreign, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/tuner2/ch473000000", e.fakes[0].StreamPort()))
	if err != nil || foreign.StatusCode != 200 {
		t.Fatalf("foreign client: %v %v", foreign, err)
	}
	defer foreign.Body.Close()
	var a struct {
		Tuners []struct {
			State    string
			Watching []struct{ Channel, Name string }
		}
	}
	waitFor(t, "the whole multiplex on the activity page", 8*time.Second, func() bool {
		a.Tuners = nil
		getJSON(t, e.api+"/ui/api/activity", &a)
		return len(a.Tuners) == 4 && len(a.Tuners[2].Watching) == 1
	})
	if w := a.Tuners[2].Watching[0]; w.Channel != "Full multiplex" {
		t.Errorf("watching %+v, want Full multiplex", w)
	}
}
