package hub

import (
	"testing"

	"hdhrmux/internal/chanmap"

	"hdhrmux/internal/ts"
)

// Programmes with the same audio and video under two programme numbers (9.1 and
// 54.2 on KQED) cannot be told apart from the PIDs a client picked: they are one
// group.
func TestWatchedProgramGroups(t *testing.T) {
	pmt := func(prog, pcr uint16, es ...uint16) *ts.PMT {
		var ss []ts.Stream
		for _, p := range es {
			ss = append(ss, ts.Stream{Type: 0x81, PID: p})
		}
		m, err := ts.ParsePMT(ts.BuildPMT(prog, 0, pcr, ss))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	st := newStream(nil, chanmap.Spec{Modulation: "auto", Frequency: 569000000}, "569000000", "569000000@0", false, 0, 0, -1)
	st.programs = map[uint16]uint16{1: 0x30, 12: 0xd0, 2: 0x40}
	st.pmts = map[uint16]*ts.PMT{
		1:  pmt(1, 0x31, 0x31, 0x34),  // 9.1
		12: pmt(12, 0x31, 0x31, 0x34), // 54.2: the same audio and video
		2:  pmt(2, 0x41, 0x41, 0x44),  // 9.2
	}
	sub := func(pids ...uint16) *Sub {
		var set ts.PIDSet
		for _, p := range pids {
			set.Add(p)
		}
		return &Sub{st: st, mode: modePIDs, pids: set}
	}
	groups := func(s *Sub) [][]uint16 { return s.WatchedProgramGroups() }
	eq := func(got [][]uint16, want ...[]uint16) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if len(got[i]) != len(want[i]) {
				return false
			}
			for j := range got[i] {
				if got[i][j] != want[i][j] {
					return false
				}
			}
		}
		return true
	}

	// the shared audio and video: both numbers, as one group
	if g := groups(sub(0, 0x30, 0xd0, 0x31, 0x34)); !eq(g, []uint16{1, 12}) {
		t.Errorf("shared PIDs: %v, want [[1 12]]", g)
	}
	// the other programme only partly in the filter: not listed
	if g := groups(sub(0x31, 0x34, 0x41)); !eq(g, []uint16{1, 12}) {
		t.Errorf("partial programme: %v, want [[1 12]]", g)
	}
	// complete: two groups
	if g := groups(sub(0x31, 0x34, 0x41, 0x44)); !eq(g, []uint16{1, 12}, []uint16{2}) {
		t.Errorf("two programmes: %v, want [[1 12] [2]]", g)
	}
	// PMTs alone (discovery) name nothing
	if g := groups(sub(0, 0x30, 0xd0, 0x40)); len(g) != 0 {
		t.Errorf("PMT PIDs only: %v", g)
	}
	if p := sub(0x31, 0x34, 0x41, 0x44).WatchedPrograms(); len(p) != 3 || p[0] != 1 || p[1] != 2 || p[2] != 12 {
		t.Errorf("flat list: %v", p)
	}
}
