package ts

import "testing"

func TestPATRoundTrip(t *testing.T) {
	sec := BuildPAT(0x1234, 5, 7, 0x0120)
	if CRC32MPEG(sec) != 0 {
		t.Fatal("bad crc")
	}
	var r SectionReader
	got := r.Push(PacketizeSection(PIDPAT, 0, sec))
	if len(got) != 1 {
		t.Fatalf("got %d sections", len(got))
	}
	pat, err := ParsePAT(got[0])
	if err != nil || pat.TSID != 0x1234 || pat.Version != 5 || pat.Programs[7] != 0x0120 || len(pat.Programs) != 1 {
		t.Fatalf("%+v %v", pat, err)
	}
}

func TestPMTRoundTrip(t *testing.T) {
	sec := BuildPMT(3, 1, 0x101, []Stream{{Type: 2, PID: 0x101}, {Type: 0x81, PID: 0x102}})
	var r SectionReader
	got := r.Push(PacketizeSection(0x100, 0, sec))
	if len(got) != 1 {
		t.Fatalf("got %d sections", len(got))
	}
	pmt, err := ParsePMT(got[0])
	if err != nil || pmt.Program != 3 || pmt.PCRPID != 0x101 || len(pmt.Streams) != 2 || pmt.Streams[1].PID != 0x102 {
		t.Fatalf("%+v %v", pmt, err)
	}
}

func TestSectionSpanningPackets(t *testing.T) {
	// A long PMT split over two packets.
	var streams []Stream
	for i := 0; i < 60; i++ {
		streams = append(streams, Stream{Type: 0x81, PID: uint16(0x200 + i)})
	}
	sec := BuildPMT(1, 0, 0x200, streams)
	if len(sec) < 184 {
		t.Fatalf("section too short (%d) for test", len(sec))
	}
	first := make([]byte, 188)
	first[0], first[1], first[2], first[3], first[4] = 0x47, 0x41, 0x00, 0x10, 0
	n := copy(first[5:], sec)
	second := make([]byte, 188)
	for i := range second {
		second[i] = 0xFF
	}
	second[0], second[1], second[2], second[3] = 0x47, 0x01, 0x00, 0x11
	copy(second[4:], sec[n:])
	var r SectionReader
	if got := r.Push(first); len(got) != 0 {
		t.Fatal("section completed too early")
	}
	got := r.Push(second)
	if len(got) != 1 {
		t.Fatalf("got %d sections", len(got))
	}
	if pmt, err := ParsePMT(got[0]); err != nil || len(pmt.Streams) != 60 {
		t.Fatalf("%v %v", pmt, err)
	}
}

func TestPIDSet(t *testing.T) {
	s, err := ParsePIDSet("0x0000 0x0100-0x0102 0x1ffb")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.String(); got != "0x0000 0x0100-0x0102 0x1ffb" {
		t.Fatalf("%q", got)
	}
	full := FullSet()
	if full.String() != "0x0000-0x1fff" {
		t.Fatal(full.String())
	}
	if _, err := ParsePIDSet("0x0000-0x2000"); err == nil {
		t.Fatal("expected error")
	}
	var none PIDSet
	if none.String() != "none" {
		t.Fatal(none.String())
	}
}
