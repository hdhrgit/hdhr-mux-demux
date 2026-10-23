package rfmap

import (
	"testing"
	"time"
)

func prog(n uint16, ch, name string) Prog {
	return Prog{Number: n, Channel: ch, Name: name, PIDs: []uint16{n * 16}}
}

func TestChangesOfAnRFChannelAreReported(t *testing.T) {
	s, _ := Open("")
	var got []Change
	s.SetObserver(func(c Change) { got = append(got, c) })
	now := time.Now()
	s.NoteMux(545000000, "8vsb", 321, []Prog{prog(1, "2.1", "KTVU"), prog(4, "4.1", "KRON")}, now)
	if len(got) != 0 {
		t.Fatalf("the first reading of an RF channel is not a change: %+v", got)
	}
	s.NoteMux(545000000, "8vsb", 321, []Prog{prog(1, "2.1", "KTVU"), prog(4, "4.1", "KRON")}, now)
	if len(got) != 0 {
		t.Fatalf("the same reading is not a change: %+v", got)
	}
	// a new call sign, a new channel number, a programme gone and one added, a new transport stream id
	s.NoteMux(545000000, "8vsb", 322, []Prog{prog(1, "2.1", "KTVU-HD"), prog(4, "4.2", "KRON"), prog(8, "2.3", "MOVIES!")}, now)
	kinds := map[string]Change{}
	for _, c := range got {
		kinds[c.Kind+"/"+c.Channel] = c
	}
	for _, want := range []string{"callsign/2.1", "number/", "program-added/2.3", "tsid/"} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("no %s change in %+v", want, got)
		}
	}
	if c := kinds["callsign/2.1"]; c.Old != "KTVU" || c.New != "KTVU-HD" || c.Frequency != 545000000 {
		t.Errorf("call sign change %+v", c)
	}
	got = nil
	s.NoteMux(545000000, "8vsb", 322, []Prog{prog(1, "2.1", "KTVU-HD"), prog(8, "2.3", "MOVIES!")}, now)
	if len(got) != 1 || got[0].Kind != "program-removed" || got[0].Program != 4 {
		t.Errorf("programme removed: %+v", got)
	}
	// a reading that does not name the programmes (the table was not read yet) is not a rename
	got = nil
	s.NoteMux(545000000, "8vsb", 322, []Prog{{Number: 1, PIDs: []uint16{16}}, {Number: 8, PIDs: []uint16{128}}}, now)
	if len(got) != 0 {
		t.Errorf("an unnamed reading counted as a change: %+v", got)
	}
}
