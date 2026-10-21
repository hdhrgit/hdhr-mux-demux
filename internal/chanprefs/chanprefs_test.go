package chanprefs

import (
	"path/filepath"
	"testing"
)

func TestChoicesPersistAndEmptyOnesVanish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channels.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("24.1", Pref{Off: []uint64{533000000, 473000000}, Video: 473000000, Guide: 473000000}); err != nil {
		t.Fatal(err)
	}
	if s.Rev() != 1 {
		t.Errorf("rev %d", s.Rev())
	}
	s2, _ := Open(path)
	p := s2.Get("24.1")
	if len(p.Off) != 2 || p.Off[0] != 473000000 || !p.IsOff(533000000) || p.IsOff(575000000) || p.Video != 473000000 || p.Guide != 473000000 {
		t.Fatalf("reloaded: %+v", p)
	}
	if s.Get("2.1").Video != 0 || len(s.All()) != 1 {
		t.Error("an untouched channel has choices")
	}
	if err := s.Set("24.1", Pref{}); err != nil {
		t.Fatal(err)
	}
	if len(s.All()) != 0 {
		t.Error("empty choices were kept")
	}
	s3, _ := Open(path)
	if len(s3.All()) != 0 {
		t.Error("empty choices came back from the file")
	}
}
