package lineup

import "testing"

func TestMergeReportsWhatChanged(t *testing.T) {
	l, _ := Open("")
	var got []Change
	l.SetObserver(func(c Change) { got = append(got, c) })
	l.Merge([]DeviceEntry{
		{GuideNumber: "2.1", GuideName: "KTVU", Frequency: 545000000, Program: 1},
		{GuideNumber: "7.1", GuideName: "KGO"},
		{GuideNumber: "9.1", GuideName: "KQED"},
	})
	if len(got) != 0 {
		t.Fatalf("filling an empty lineup is not a change: %+v", got)
	}
	l.SetDisabled("7.1", true) // a disabled channel is watched too
	l.Merge([]DeviceEntry{
		{GuideNumber: "2.1", GuideName: "KTVU-HD", Frequency: 575000000, Program: 1},
		{GuideNumber: "7.1", GuideName: "KGO-HD"},
		{GuideNumber: "9.2", GuideName: "KQED+"},
	})
	byKind := map[string]Change{}
	for _, c := range got {
		byKind[c.Kind+"/"+c.Channel] = c
	}
	for _, want := range []string{"callsign/2.1", "moved/2.1", "callsign/7.1", "added/9.2", "removed/9.1"} {
		if _, ok := byKind[want]; !ok {
			t.Errorf("no %s in %+v", want, got)
		}
	}
	if c := byKind["moved/2.1"]; c.OldFrequency != 545000000 || c.Frequency != 575000000 {
		t.Errorf("moved: %+v", c)
	}
	if c := byKind["callsign/7.1"]; c.Old != "KGO" || c.New != "KGO-HD" {
		t.Errorf("a disabled channel's call sign: %+v", c)
	}
}
