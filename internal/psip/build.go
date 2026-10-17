package psip

import (
	"time"
	"unicode/utf16"

	"hdhrmux/internal/ts"
)

// Builders for the PSIP tables. The multiplexer does not broadcast a guide;
// they exist so the simulated device and the tests can.

func longSection(tid byte, ext uint16, payload []byte) []byte {
	n := 5 + len(payload) + 4
	s := []byte{tid, 0xF0 | byte(n>>8), byte(n), byte(ext >> 8), byte(ext), 0xC1, 0, 0}
	s = append(s, payload...)
	crc := ts.CRC32MPEG(s)
	return append(s, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

// EncodeMSS encodes one uncompressed string as a multiple_string_structure.
func EncodeMSS(lang, text string) []byte {
	if len(lang) != 3 {
		lang = "eng"
	}
	b := make([]byte, 0, len(text)) // mode 0 is ISO 8859-1
	for _, r := range text {
		if r > 0xFF {
			r = '?'
		}
		b = append(b, byte(r))
	}
	if len(b) > 255 {
		b = b[:255]
	}
	return append([]byte{1, lang[0], lang[1], lang[2], 1, 0, 0, byte(len(b))}, b...)
}

// BuildMGT lists the PIDs of the EIT-0..n and ETT tables.
func BuildMGT(eitPIDs, ettPIDs []uint16) []byte {
	p := []byte{0, 0, byte(len(eitPIDs) + len(ettPIDs))}
	add := func(typ int, pid uint16) {
		p = append(p, byte(typ>>8), byte(typ), 0xE0|byte(pid>>8), byte(pid), 0xE0, 0, 0, 0, 0, 0xF0, 0)
	}
	for i, pid := range eitPIDs {
		add(0x0100+i, pid)
	}
	for i, pid := range ettPIDs {
		add(0x0200+i, pid)
	}
	p = append(p, 0xF0, 0)
	return longSection(tidMGT, 0, p)
}

// BuildVCT builds a terrestrial virtual channel table.
func BuildVCT(tsid uint16, chans []Channel) []byte {
	p := []byte{0, byte(len(chans))}
	for _, c := range chans {
		name := make([]byte, 14)
		for i, u := range utf16.Encode([]rune(c.Name)) {
			if i < 7 {
				name[2*i], name[2*i+1] = byte(u>>8), byte(u)
			}
		}
		p = append(p, name...)
		p = append(p, 0xF0|byte(c.Major>>6), byte(c.Major<<2)|byte(c.Minor>>8), byte(c.Minor),
			0x04, 0, 0, 0, 0, byte(c.TSID>>8), byte(c.TSID), byte(c.Program>>8), byte(c.Program),
			0, 0, byte(c.SourceID>>8), byte(c.SourceID), 0xFC, 0)
	}
	p = append(p, 0xFC, 0)
	return longSection(tidTVCT, tsid, p)
}

// BuildEIT builds an event information table for one source.
func BuildEIT(source uint16, evs []Event, leap int) []byte {
	p := []byte{0, byte(len(evs))}
	for _, e := range evs {
		gps := uint32(e.Start.Unix() - gpsEpoch + int64(leap))
		title := EncodeMSS(e.Language, e.Title)
		secs := int(e.Duration / time.Second)
		p = append(p, 0xC0|byte(e.ID>>8), byte(e.ID), byte(gps>>24), byte(gps>>16), byte(gps>>8), byte(gps),
			0xC0|byte(secs>>16)&0x0F, byte(secs>>8), byte(secs), byte(len(title)))
		p = append(p, title...)
		p = append(p, 0xF0, 0)
	}
	return longSection(tidEIT, source, p)
}

// BuildETT builds the extended text (description) of one event.
func BuildETT(source, event uint16, lang, text string) []byte {
	etm := uint32(source)<<16 | uint32(event)<<2 | 2
	p := []byte{0, byte(etm >> 24), byte(etm >> 16), byte(etm >> 8), byte(etm)}
	p = append(p, EncodeMSS(lang, text)...)
	return longSection(tidETT, 0, p)
}

// BuildSTT builds the system time table.
func BuildSTT(now time.Time, leap int) []byte {
	gps := uint32(now.Unix() - gpsEpoch + int64(leap))
	return longSection(tidSTT, 0, []byte{0, byte(gps >> 24), byte(gps >> 16), byte(gps >> 8), byte(gps), byte(leap), 0, 0})
}
