package chanmap

import "testing"

func TestFrequencies(t *testing.T) {
	for _, c := range []struct {
		m    string
		n    int
		want uint64
	}{
		{"us-bcast", 2, 57000000}, {"us-bcast", 7, 177000000}, {"us-bcast", 14, 473000000}, {"us-bcast", 36, 605000000},
		{"us-cable", 2, 57000000}, {"us-cable", 14, 123000000}, {"us-cable", 23, 219000000}, {"us-cable", 100, 651000000},
	} {
		if got, ok := Frequency(c.m, c.n); !ok || got != c.want {
			t.Errorf("%s ch%d = %d (%v), want %d", c.m, c.n, got, ok, c.want)
		}
	}
}

func TestParse(t *testing.T) {
	s, ok, err := Parse("auto:177000000", "us-bcast")
	if err != nil || !ok || s.Frequency != 177000000 || s.Modulation != "auto" {
		t.Fatalf("%+v %v %v", s, ok, err)
	}
	if s, _, _ := Parse("8vsb:14", "us-bcast"); s.Frequency != 473000000 {
		t.Fatalf("channel number: %+v", s)
	}
	if _, ok, err := Parse("none", ""); ok || err != nil {
		t.Fatal("none")
	}
	if _, _, err := Parse("bogus", ""); err == nil {
		t.Fatal("expected error")
	}
}
