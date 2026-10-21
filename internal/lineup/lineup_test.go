package lineup

import "testing"

func TestHideATSC3KeepsThemFindable(t *testing.T) {
	l, _ := Open("")
	l.Replace([]Entry{
		{GuideNumber: "7.1", GuideName: "A", Frequency: 177000000, Program: 1},
		{GuideNumber: "104.1", GuideName: "N", Frequency: 605000000, Program: 1376, Modulation: "atsc3"},
	})
	if n := len(l.Entries(false)); n != 2 {
		t.Fatalf("%d entries", n)
	}
	l.SetHideATSC3(true)
	if es := l.Entries(false); len(es) != 1 || es[0].GuideNumber != "7.1" {
		t.Fatalf("listed with ATSC 3.0 hidden: %+v", es)
	}
	if n := len(l.Entries(true)); n != 2 {
		t.Errorf("Entries(true) has %d, want all", n)
	}
	if _, ok := l.Find("104.1"); !ok {
		t.Error("a hidden channel can no longer be found")
	}
	l.SetHideATSC3(false)
	if n := len(l.Entries(false)); n != 2 {
		t.Errorf("not shown again: %d", n)
	}
}

func TestDevicePlacementIsAuthoritative(t *testing.T) {
	l, _ := Open("")
	_, _, err := l.Merge([]DeviceEntry{
		{GuideNumber: "2.1", GuideName: "KTVU-HD", Frequency: 575000000, Modulation: "8vsb", Program: 3, TSID: 321},
		{GuideNumber: "9.1", GuideName: "KQED-HD"}, // an older device: no placement
	})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := l.Find("2.1")
	if e.Frequency != 575000000 || e.Program != 3 || e.Modulation != "8vsb" || e.TSID != 321 {
		t.Fatalf("placement from the device not applied: %+v", e)
	}
	// a stream seen on another RF channel does not move a placed channel...
	if n := l.LearnRF([]Entry{{GuideNumber: "2.1", GuideName: "KTVU-HD", Frequency: 545000000, Program: 3, Modulation: "8vsb"}}, false); n != 0 {
		t.Errorf("a placed channel was changed by a stream: %d", n)
	}
	if e, _ := l.Find("2.1"); e.Frequency != 575000000 {
		t.Errorf("2.1 moved to %d Hz", e.Frequency)
	}
	// ...but fills in what is missing at the same place
	l.LearnRF([]Entry{{GuideNumber: "2.1", Frequency: 575000000, Program: 3, TSID: 321, DRM: true}}, false)
	// an unplaced channel is placed by a stream
	if n := l.LearnRF([]Entry{{GuideNumber: "9.1", Frequency: 569000000, Program: 1, Modulation: "8vsb"}}, false); n != 1 {
		t.Errorf("an unplaced channel was not placed: %d", n)
	}
	if e, _ := l.Find("9.1"); e.Frequency != 569000000 {
		t.Errorf("9.1 at %d Hz", e.Frequency)
	}
	// an explicit scan may move things
	l.LearnRF([]Entry{{GuideNumber: "2.1", Frequency: 545000000, Program: 3, Modulation: "8vsb"}}, true)
	if e, _ := l.Find("2.1"); e.Frequency != 545000000 {
		t.Errorf("a scan could not move 2.1: %d", e.Frequency)
	}
}
