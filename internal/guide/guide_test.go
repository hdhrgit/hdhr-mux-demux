package guide

import (
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
