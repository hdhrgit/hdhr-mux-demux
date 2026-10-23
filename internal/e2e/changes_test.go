package e2e

import (
	"testing"

	"hdhrmux/internal/lineup"
)

// The history of changes (changes.log): a renamed or moved channel, disabled ones included,
// is recorded and listed on the page's API, newest first.
func TestChangesAreRecordedAndListed(t *testing.T) {
	e := setup(t, []int{1}, 1, nil)
	e.lu.Replace(nil)
	e.lu.Merge([]lineup.DeviceEntry{
		{GuideNumber: "7.1", GuideName: "ALPHA", Frequency: 177000000, Program: 1},
		{GuideNumber: "7.2", GuideName: "BETA", Frequency: 177000000, Program: 2},
	})
	e.lu.SetDisabled("7.2", true)
	e.lu.Merge([]lineup.DeviceEntry{
		{GuideNumber: "7.1", GuideName: "ALPHA-HD", Frequency: 191000000, Program: 1},
		{GuideNumber: "7.2", GuideName: "BETA-HD", Frequency: 177000000, Program: 2},
	})
	var v struct {
		File   string
		Events []struct {
			Kind, Channel, Old, New, Text string
		}
	}
	getJSON(t, e.api+"/ui/api/changes", &v)
	seen := map[string]bool{}
	for _, ev := range v.Events {
		seen[ev.Kind+"/"+ev.Channel] = true
	}
	for _, want := range []string{"callsign/7.1", "moved/7.1", "callsign/7.2"} {
		if !seen[want] {
			t.Errorf("%s missing from %+v", want, v.Events)
		}
	}
	if len(v.Events) == 0 || v.Events[0].Text == "" {
		t.Fatalf("events %+v", v.Events)
	}
}
