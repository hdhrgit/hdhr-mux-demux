// Package psip extracts the electronic programme guide that ATSC 1.0
// broadcasts inside every multiplex (A/65 PSIP): the channel table (VCT),
// the master guide table (MGT) that says which PIDs carry the guide, the
// event information tables (EIT, titles and times), the extended text tables
// (ETT, descriptions) and the system time table (STT).
package psip

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"hdhrmux/internal/ts"
)

const (
	BasePID = 0x1FFB // PSIP base PID: MGT, VCT, STT

	tidMGT  = 0xC7
	tidTVCT = 0xC8
	tidCVCT = 0xC9
	tidEIT  = 0xCB
	tidETT  = 0xCC
	tidSTT  = 0xCD

	gpsEpoch    = 315964800 // 1980-01-06 as Unix time
	defaultLeap = 18        // GPS-UTC offset until a system time table says otherwise
)

// Event is one programme.
type Event struct {
	ID          uint16
	Start       time.Time
	Duration    time.Duration
	Title       string
	Description string
	Language    string
}

func (e Event) End() time.Time { return e.Start.Add(e.Duration) }

// Channel is a virtual channel from the VCT.
type Channel struct {
	SourceID  uint16
	Major     uint16
	Minor     uint16
	Name      string
	TSID      uint16
	Program   uint16
	GuideName string // "major.minor"
}

// Collector assembles the guide from the packets of one multiplex. It is fed
// every packet; it only looks at the PSIP PIDs. Not safe for concurrent use.
type Collector struct {
	readers  map[uint16]*ts.SectionReader
	want     [ts.PIDMax + 1]bool          // PIDs carrying guide data
	wanted   ts.PIDSet                    // the same, as a set
	wantNew  bool                         // wanted grew since WantedPIDs
	channels map[uint16]*Channel          // by source id
	events   map[uint16]map[uint16]*Event // source id -> event id
	leap     int
	changed  bool
}

func NewCollector() *Collector {
	c := &Collector{
		readers:  map[uint16]*ts.SectionReader{},
		channels: map[uint16]*Channel{}, events: map[uint16]map[uint16]*Event{}, leap: defaultLeap,
	}
	c.want[BasePID] = true
	c.wanted.Add(BasePID)
	c.wantNew = true
	return c
}

// Wants reports whether packets of this PID carry guide data.
func (c *Collector) Wants(pid uint16) bool {
	return c.want[pid&ts.PIDMax]
}

// WantedPIDs returns the PIDs that carry guide data and reports whether the
// set has grown since the last call.
func (c *Collector) WantedPIDs() (set ts.PIDSet, grown bool) {
	grown, c.wantNew = c.wantNew, false
	return c.wanted, grown
}

// Push consumes one packet.
func (c *Collector) Push(pid uint16, pkt ts.Packet) {
	if !c.Wants(pid) {
		return
	}
	r := c.readers[pid]
	if r == nil {
		r = &ts.SectionReader{}
		c.readers[pid] = r
	}
	for _, sec := range r.Push(pkt) {
		c.section(sec)
	}
}

// Changed reports (and clears) whether anything new has been learned.
func (c *Collector) Changed() bool {
	ch := c.changed
	c.changed = false
	return ch
}

func be16(b []byte) int { return int(b[0])<<8 | int(b[1]) }
func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func (c *Collector) section(s []byte) {
	if len(s) < 12 || s[5]&1 == 0 { // short, or not yet current
		return
	}
	body := s[8 : len(s)-4] // after the long-section header, before the CRC
	switch s[0] {
	case tidMGT:
		c.mgt(body)
	case tidTVCT, tidCVCT:
		c.vct(be16(s[3:5]), body)
	case tidEIT:
		c.eit(uint16(be16(s[3:5])), body)
	case tidETT:
		c.ett(body)
	case tidSTT:
		c.stt(body)
	}
}

func (c *Collector) mgt(b []byte) {
	if len(b) < 3 {
		return
	}
	n := be16(b[1:3])
	b = b[3:]
	for i := 0; i < n; i++ {
		if len(b) < 11 {
			return
		}
		typ := be16(b[0:2])
		pid := uint16(be16(b[2:4]) & 0x1FFF)
		dl := be16(b[9:11]) & 0x0FFF
		switch {
		case (typ >= 0x0100 && typ <= 0x017F) || (typ >= 0x0200 && typ <= 0x027F):
			if !c.want[pid] {
				c.want[pid] = true
				c.wanted.Add(pid)
				c.wantNew = true
			}
		}
		if len(b) < 11+dl {
			return
		}
		b = b[11+dl:]
	}
}

func (c *Collector) vct(tsid int, b []byte) {
	if len(b) < 2 {
		return
	}
	n := int(b[1])
	b = b[2:]
	for i := 0; i < n; i++ {
		if len(b) < 32 {
			return
		}
		name := make([]uint16, 0, 7)
		for j := 0; j < 7; j++ {
			if u := uint16(be16(b[2*j:])); u != 0 {
				name = append(name, u)
			}
		}
		major := uint16(be16(b[14:16])>>2) & 0x3FF
		minor := uint16(be16(b[15:17])) & 0x3FF
		ch := &Channel{
			Name: strings.TrimSpace(string(utf16.Decode(name))), Major: major, Minor: minor,
			TSID: uint16(be16(b[22:24])), Program: uint16(be16(b[24:26])), SourceID: uint16(be16(b[28:30])),
		}
		ch.GuideName = fmt.Sprintf("%d.%d", major, minor)
		if old := c.channels[ch.SourceID]; old == nil || *old != *ch {
			c.channels[ch.SourceID] = ch
			c.changed = true
		}
		dl := be16(b[30:32]) & 0x03FF
		if len(b) < 32+dl {
			return
		}
		b = b[32+dl:]
	}
}

func (c *Collector) stt(b []byte) {
	if len(b) >= 6 {
		c.leap = int(b[5])
	}
}

func (c *Collector) gpsTime(gps uint32) time.Time {
	return time.Unix(int64(gps)+gpsEpoch-int64(c.leap), 0).UTC()
}

func (c *Collector) eit(source uint16, b []byte) {
	if len(b) < 2 {
		return
	}
	n := int(b[1])
	b = b[2:]
	for i := 0; i < n; i++ {
		if len(b) < 10 {
			return
		}
		id := uint16(be16(b[0:2]) & 0x3FFF)
		start := be32(b[2:6])
		length := (int(b[6])&0x0F)<<16 | be16(b[7:9])
		tl := int(b[9])
		if len(b) < 10+tl+2 {
			return
		}
		title, lang := decodeMSS(b[10 : 10+tl])
		dl := be16(b[10+tl:12+tl]) & 0x0FFF
		if len(b) < 12+tl+dl {
			return
		}
		b = b[12+tl+dl:]
		evs := c.events[source]
		if evs == nil {
			evs = map[uint16]*Event{}
			c.events[source] = evs
		}
		e := evs[id]
		if e == nil {
			e = &Event{ID: id}
			evs[id] = e
			c.changed = true
		}
		st, dur := c.gpsTime(start), time.Duration(length)*time.Second
		if !e.Start.Equal(st) || e.Duration != dur || e.Title != title {
			e.Start, e.Duration, e.Title, e.Language = st, dur, title, lang
			c.changed = true
		}
	}
}

func (c *Collector) ett(b []byte) {
	if len(b) < 5 {
		return
	}
	etm := be32(b[1:5])
	if etm&3 != 2 { // 2 = an event's text; 0 is the channel's
		return
	}
	source, id := uint16(etm>>16), uint16(etm>>2)&0x3FFF
	text, _ := decodeMSS(b[5:])
	if e := c.events[source][id]; e != nil && e.Description != text {
		e.Description = text
		c.changed = true
	}
}

// decodeMSS reads an ATSC multiple_string_structure and returns the first
// string that is stored uncompressed (Huffman-coded text is not supported).
func decodeMSS(b []byte) (text, lang string) {
	if len(b) < 1 {
		return "", ""
	}
	n := int(b[0])
	b = b[1:]
	for i := 0; i < n; i++ {
		if len(b) < 4 {
			return "", ""
		}
		lang = string(b[0:3])
		segs := int(b[3])
		b = b[4:]
		var sb strings.Builder
		for j := 0; j < segs; j++ {
			if len(b) < 3 {
				return sb.String(), lang
			}
			comp, mode, nb := b[0], b[1], int(b[2])
			if len(b) < 3+nb {
				return sb.String(), lang
			}
			data := b[3 : 3+nb]
			b = b[3+nb:]
			if comp != 0 {
				continue
			}
			sb.WriteString(decodeBytes(mode, data))
		}
		if sb.Len() > 0 {
			return strings.TrimSpace(sb.String()), lang
		}
	}
	return "", lang
}

func decodeBytes(mode byte, data []byte) string {
	if mode == 0x3F { // UTF-16BE
		u := make([]uint16, 0, len(data)/2)
		for i := 0; i+1 < len(data); i += 2 {
			u = append(u, uint16(data[i])<<8|uint16(data[i+1]))
		}
		return string(utf16.Decode(u))
	}
	if mode == 0xFF {
		return ""
	}
	r := make([]rune, len(data))
	for i, x := range data {
		r[i] = rune(mode)<<8 | rune(x)
	}
	return string(r)
}

// Guide is a snapshot: events per channel, keyed by "major.minor", plus the
// channel table.
type Guide struct {
	Channels map[string]Channel
	Events   map[string][]Event
	// Frequency is the RF channel the tables were read from (set by whoever read them).
	Frequency uint64
}

// Snapshot returns what has been collected, sorted by start time. Only
// events of channels that appeared in the VCT are reported.
func (c *Collector) Snapshot() Guide {
	g := Guide{Channels: map[string]Channel{}, Events: map[string][]Event{}}
	for src, ch := range c.channels {
		g.Channels[ch.GuideName] = *ch
		for _, e := range c.events[src] {
			g.Events[ch.GuideName] = append(g.Events[ch.GuideName], *e)
		}
		evs := g.Events[ch.GuideName]
		sort.Slice(evs, func(i, j int) bool { return evs[i].Start.Before(evs[j].Start) })
	}
	return g
}
