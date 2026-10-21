package guide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hdhrmux/internal/psip"
)

func ev(id uint16, start time.Time, mins int, title string) psip.Event {
	return psip.Event{ID: id, Start: start, Duration: time.Duration(mins) * time.Minute, Title: title}
}

func TestMergeReplacesCoveredWindowAndKeepsTheRest(t *testing.T) {
	s, _ := Open("")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	t0 := now
	s.Merge(psip.Guide{Events: map[string][]psip.Event{"7.1": {
		ev(1, t0, 30, "A"), ev(2, t0.Add(30*time.Minute), 30, "B"), ev(3, t0.Add(time.Hour), 60, "C"),
	}}}, now)
	// a later collection covers only 12:30-13:00 and changes B; also adds a far-future event
	s.Merge(psip.Guide{Events: map[string][]psip.Event{"7.1": {
		ev(2, t0.Add(30*time.Minute), 30, "B2"), ev(9, t0.Add(10*time.Hour), 60, "Later"),
	}}}, now)
	var titles []string
	for _, e := range s.Events("7.1") {
		titles = append(titles, e.Title)
	}
	// A (before the window) and C (after it, before the new far event's window) survive; B is replaced
	want := []string{"A", "B2", "C", "Later"}
	if len(titles) != len(want) {
		t.Fatalf("%v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Fatalf("%v, want %v", titles, want)
		}
	}
	// old events are dropped
	s.Merge(psip.Guide{Events: map[string][]psip.Event{"7.1": {ev(20, now.Add(5*time.Hour), 30, "Fresh")}}}, now.Add(6*time.Hour))
	for _, e := range s.Events("7.1") {
		if e.Title == "A" || e.Title == "B2" {
			t.Fatalf("expired event kept: %+v", e)
		}
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epg.json")
	s, _ := Open(path)
	now := time.Now()
	s.Merge(psip.Guide{Events: map[string][]psip.Event{"2.1": {ev(1, now, 60, "Persisted")}}}, now)
	s.Flush()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if evs := s2.Events("2.1"); len(evs) != 1 || evs[0].Title != "Persisted" {
		t.Fatalf("%+v", evs)
	}
	if ch, n := s2.Summary(); ch != 1 || n != 1 {
		t.Fatalf("summary %d %d", ch, n)
	}
}

func evh(title string, start time.Time, hours int) psip.Event {
	return psip.Event{Title: title, Start: start, Duration: time.Duration(hours) * time.Hour}
}

// A channel carried on two RF channels keeps a guide for each; the one used is chosen,
// or else the one that goes furthest ahead.
func TestGuidePerRFChannel(t *testing.T) {
	s, _ := Open("")
	now := time.Now().Truncate(time.Hour)
	s.Merge(psip.Guide{Frequency: 533000000, Events: map[string][]psip.Event{"24.1": {evh("short", now, 2)}}}, now)
	s.Merge(psip.Guide{Frequency: 473000000, Events: map[string][]psip.Event{"24.1": {evh("long", now, 40)}}}, now)
	if got := s.Events("24.1"); len(got) != 1 || got[0].Title != "long" {
		t.Fatalf("automatic should take the guide that goes furthest ahead: %+v", got)
	}
	s.SetChooser(func(ch string) uint64 { return 533000000 })
	if got := s.Events("24.1"); len(got) != 1 || got[0].Title != "short" {
		t.Fatalf("the chosen RF channel's guide: %+v", got)
	}
	// a choice with no guide falls back to automatic instead of leaving the channel empty
	s.SetChooser(func(ch string) uint64 { return 999000000 })
	if got := s.Events("24.1"); len(got) != 1 || got[0].Title != "long" {
		t.Fatalf("a choice with no guide should fall back: %+v", got)
	}
	// merging one RF channel again does not touch the other's events
	s.Merge(psip.Guide{Frequency: 533000000, Events: map[string][]psip.Event{"24.1": {evh("short2", now.Add(2*time.Hour), 2)}}}, now)
	s.SetChooser(func(ch string) uint64 { return 533000000 })
	if got := s.Events("24.1"); len(got) != 2 {
		t.Errorf("533's own events: %+v", got)
	}
	if ch, n := s.Summary(); ch != 1 || n != 2 {
		t.Errorf("summary %d channels %d events, want the chosen guide's 1 and 2", ch, n)
	}
}

// Files written before guides were kept per RF channel still load, and their events
// give way once the RF channel is known.
func TestGuideFileFromBeforeRFChannelsWasKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epg.json")
	now := time.Now().Truncate(time.Hour)
	old := map[string][]psip.Event{"7.1": {evh("old", now, 3)}}
	b, _ := json.Marshal(old)
	os.WriteFile(path, b, 0o644)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Events("7.1"); len(got) != 1 || got[0].Title != "old" {
		t.Fatalf("old file: %+v", got)
	}
	s.Merge(psip.Guide{Frequency: 177000000, Events: map[string][]psip.Event{"7.1": {evh("new", now, 3)}}}, now)
	s.Flush()
	s2, _ := Open(path)
	if got := s2.Events("7.1"); len(got) != 1 || got[0].Title != "new" {
		t.Fatalf("after merging with a known RF channel and a reload: %+v", got)
	}
}
