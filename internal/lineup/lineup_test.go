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
