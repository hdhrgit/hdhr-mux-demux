package psip

import (
	"strings"
	"testing"
	"time"

	"hdhrmux/internal/ts"
)

func feed(c *Collector, pid uint16, secs ...[]byte) {
	var cc byte
	for _, s := range secs {
		for _, p := range ts.Packetize(pid, &cc, s) {
			c.Push(pid, p)
		}
	}
}

func TestGuideRoundTrip(t *testing.T) {
	c := NewCollector()
	start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	feed(c, BasePID,
		BuildMGT([]uint16{0x1D00, 0x1D01}, []uint16{0x1E00}),
		BuildVCT(0x017D, []Channel{{Name: "KGO-HD", Major: 7, Minor: 1, TSID: 0x017D, Program: 3, SourceID: 5}, {Name: "LOCL", Major: 7, Minor: 2, SourceID: 6, Program: 4}}),
		BuildSTT(start, 18))
	if !c.Wants(0x1D00) || !c.Wants(0x1D01) || !c.Wants(0x1E00) || c.Wants(0x1D02) {
		t.Fatal("PIDs from the MGT are not picked up")
	}
	long := strings.Repeat("A long description. ", 12) // needs two packets
	feed(c, 0x1D00,
		BuildEIT(5, []Event{{ID: 1, Start: start, Duration: 30 * time.Minute, Title: "News at Noon", Language: "eng"}, {ID: 2, Start: start.Add(30 * time.Minute), Duration: 90 * time.Minute, Title: "Café Show"}}, 18),
		BuildEIT(6, []Event{{ID: 9, Start: start, Duration: time.Hour, Title: "Other"}}, 18))
	feed(c, 0x1E00, BuildETT(5, 1, "eng", long), BuildETT(5, 2, "eng", "Short."))
	if !c.Changed() || c.Changed() {
		t.Fatal("Changed should report once")
	}
	g := c.Snapshot()
	ch, ok := g.Channels["7.1"]
	if !ok || ch.Name != "KGO-HD" || ch.Program != 3 || ch.TSID != 0x017D || ch.SourceID != 5 {
		t.Fatalf("channel: %+v", ch)
	}
	evs := g.Events["7.1"]
	if len(evs) != 2 || evs[0].Title != "News at Noon" || !evs[0].Start.Equal(start) || evs[0].Duration != 30*time.Minute {
		t.Fatalf("events: %+v", evs)
	}
	if evs[0].Description != strings.TrimSpace(long) {
		t.Fatalf("description %q", evs[0].Description)
	}
	if evs[1].Title != "Café Show" || evs[1].Description != "Short." {
		t.Fatalf("second event: %+v", evs[1])
	}
	if len(g.Events["7.2"]) != 1 || g.Events["7.2"][0].Title != "Other" {
		t.Fatalf("second channel: %+v", g.Events["7.2"])
	}
}

func TestLeapSecondsFromSTT(t *testing.T) {
	c := NewCollector()
	start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	feed(c, BasePID, BuildMGT([]uint16{0x1D00}, nil), BuildVCT(1, []Channel{{Name: "X", Major: 1, Minor: 1, SourceID: 1}}), BuildSTT(start, 19))
	feed(c, 0x1D00, BuildEIT(1, []Event{{ID: 1, Start: start, Duration: time.Hour, Title: "T"}}, 19))
	if got := c.Snapshot().Events["1.1"][0].Start; !got.Equal(start) {
		t.Fatalf("start %v, want %v: the GPS offset from the STT must be used", got, start)
	}
}

func TestUnsupportedCompressionIsSkipped(t *testing.T) {
	// compression_type 1 (Huffman) is not decoded; it must not produce garbage or panic
	mss := []byte{1, 'e', 'n', 'g', 1, 1, 0, 3, 0xAA, 0xBB, 0xCC}
	if text, _ := decodeMSS(mss); text != "" {
		t.Fatalf("got %q", text)
	}
	if text, _ := decodeMSS([]byte{1, 'e', 'n', 'g', 1, 0, 0, 200, 'x'}); text != "" {
		t.Fatalf("truncated string gave %q", text)
	}
}
