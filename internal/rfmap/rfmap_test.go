package rfmap

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreKeepsWhatWasLearnedAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rfmap.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.NoteMux(575000000, "8vsb", 321, []Prog{{Number: 3, Channel: "2.1", Name: "KTVU-HD", PMTPID: 0x30, PIDs: []uint16{0x31, 0x34}}, {Number: 7, Channel: "4.1", Name: "KRON-TV"}}, now)
	s.NoteGuide(575000000, 3, "2.1", "KTVU-HD", 20, now.Add(52*time.Hour), now)
	s.NoteSignal(575000000, "10A22E9C", 80, 100, 100, now)
	s.NoteSignal(575000000, "10813750", 74, 98, 100, now)
	s.Flush()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := s2.Snapshot()
	if len(snap) != 1 || snap[0].Frequency != 575000000 || snap[0].TSID != 321 || snap[0].Modulation != "8vsb" {
		t.Fatalf("reloaded: %+v", snap)
	}
	p := snap[0].Programs[3]
	if p == nil || p.Channel != "2.1" || p.PMTPID != 0x30 || len(p.PIDs) != 2 || p.Guide == nil || p.Guide.Events != 20 {
		t.Fatalf("programme 3: %+v", p)
	}
	if len(snap[0].Signal) != 2 || snap[0].Signal["10A22E9C"].SS != 80 {
		t.Errorf("signal by device not kept: %+v", snap[0].Signal)
	}
}

func TestNoteMuxForgetsProgrammesThatLeftButKeepsTheirGuide(t *testing.T) {
	s, _ := Open("")
	now := time.Now()
	s.NoteMux(473000000, "8vsb", 9739, []Prog{{Number: 15, Channel: "24.7"}, {Number: 16, Channel: "24.1"}}, now)
	s.NoteGuide(473000000, 16, "24.1", "Diya TV", 18, now.Add(40*time.Hour), now)
	// the table is read again: 24.7 left, 24.1 stays (without channel info this time)
	s.NoteMux(473000000, "8vsb", 9739, []Prog{{Number: 16}}, now.Add(time.Minute))
	r := s.Snapshot()[0]
	if _, ok := r.Programs[15]; ok {
		t.Error("a programme that left the PAT was kept")
	}
	if p := r.Programs[16]; p == nil || p.Channel != "24.1" || p.Name != "Diya TV" || p.Guide == nil || p.Guide.Events != 18 {
		t.Errorf("a programme that stayed lost what was known about it: %+v", p)
	}
	// a snapshot is a copy
	r.Programs[16].Channel = "changed"
	if s.Snapshot()[0].Programs[16].Channel != "24.1" {
		t.Error("Snapshot shares memory with the store")
	}
}

func TestIgnoresNothingToNote(t *testing.T) {
	s, _ := Open("")
	s.NoteMux(0, "8vsb", 1, []Prog{{Number: 1}}, time.Now())
	s.NoteMux(177000000, "8vsb", 1, nil, time.Now())
	s.NoteSignal(177000000, "", 1, 1, 1, time.Now())
	if len(s.Snapshot()) != 0 {
		t.Errorf("recorded something from nothing: %+v", s.Snapshot())
	}
}
