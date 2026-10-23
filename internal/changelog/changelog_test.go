package changelog

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryIsKeptInItsOwnFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "changes.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	l.Add(Event{Kind: "callsign", Channel: "7.1", Old: "KGO", New: "KGO-HD", Text: "7.1: call sign KGO → KGO-HD"})
	l.Add(Event{Kind: "moved", Channel: "2.1", Old: "545", New: "575", Text: "2.1 moved from 545 to 575 MHz"})
	again, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	got := again.List(0)
	if len(got) != 2 || got[0].Channel != "7.1" || got[1].Kind != "moved" || got[0].Time.IsZero() {
		t.Fatalf("history after a restart: %+v", got)
	}
	if one := again.List(1); len(one) != 1 || one[0].Kind != "moved" {
		t.Errorf("the newest one: %+v", one)
	}
}

func TestTheSameChangeFromTwoSourcesIsRecordedOnce(t *testing.T) {
	l, _ := Open("")
	e := Event{Kind: "callsign", Channel: "7.1", Old: "KGO", New: "KGO-HD", Text: "x"}
	l.Add(e)
	e.Frequency = 177000000 // the multiplex's own table says it too
	l.Add(e)
	if n := len(l.List(0)); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
	old := e
	old.Time = time.Now().Add(-48 * time.Hour)
	l2, _ := Open("")
	l2.Add(old)
	l2.Add(Event{Kind: "callsign", Channel: "7.1", Old: "KGO", New: "KGO-HD", Text: "x"})
	if n := len(l2.List(0)); n != 2 {
		t.Errorf("a change repeated after days is a new event: %d", n)
	}
}
