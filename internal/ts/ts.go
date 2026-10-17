// Package ts contains the small amount of MPEG transport stream handling the
// multiplexer needs: packet header access, PSI section reassembly, PAT/PMT
// parsing and generation, and PID sets.
package ts

import "fmt"

const (
	PacketSize = 188
	SyncByte   = 0x47
	PIDPAT     = 0x0000
	PIDNull    = 0x1FFF
	PIDMax     = 0x1FFF
)

// Packet is one 188-byte transport stream packet.
type Packet = []byte

func PID(p Packet) uint16 { return uint16(p[1]&0x1F)<<8 | uint16(p[2]) }
func PUSI(p Packet) bool  { return p[1]&0x40 != 0 }
func CC(p Packet) byte    { return p[3] & 0x0F }
func HasPayload(p Packet) bool {
	return p[3]&0x10 != 0
}

// Payload returns the payload bytes of p (nil if none).
func Payload(p Packet) []byte {
	if !HasPayload(p) {
		return nil
	}
	off := 4
	if p[3]&0x20 != 0 {
		off += 1 + int(p[4])
	}
	if off >= PacketSize {
		return nil
	}
	return p[off:]
}

// PIDSet is a bitset over all 8192 PIDs.
type PIDSet [(PIDMax + 1) / 64]uint64

func (s *PIDSet) Add(pid uint16)      { s[pid>>6] |= 1 << (pid & 63) }
func (s *PIDSet) Has(pid uint16) bool { return s[pid>>6]&(1<<(pid&63)) != 0 }
func (s *PIDSet) AddRange(lo, hi uint16) {
	for p := uint32(lo); p <= uint32(hi) && p <= PIDMax; p++ {
		s.Add(uint16(p))
	}
}

// FullSet returns a set containing every PID.
func FullSet() PIDSet {
	var s PIDSet
	s.AddRange(0, PIDMax)
	return s
}

// String renders the set the way HDHomeRun's /tunerN/filter does:
// "0x0000-0x0012 0x0100".
func (s *PIDSet) String() string {
	out := ""
	for p := 0; p <= PIDMax; {
		if !s.Has(uint16(p)) {
			p++
			continue
		}
		q := p
		for q+1 <= PIDMax && s.Has(uint16(q+1)) {
			q++
		}
		if out != "" {
			out += " "
		}
		if q == p {
			out += fmt.Sprintf("0x%04x", p)
		} else {
			out += fmt.Sprintf("0x%04x-0x%04x", p, q)
		}
		p = q + 1
	}
	if out == "" {
		return "none"
	}
	return out
}

// ParsePIDSet parses a filter value ("0x0000-0x1FFF 0x0100", "none").
func ParsePIDSet(v string) (PIDSet, error) {
	var s PIDSet
	i := 0
	for i < len(v) {
		for i < len(v) && (v[i] == ' ' || v[i] == '\t') {
			i++
		}
		j := i
		for j < len(v) && v[j] != ' ' && v[j] != '\t' {
			j++
		}
		tok := v[i:j]
		i = j
		if tok == "" || tok == "none" {
			continue
		}
		var lo, hi uint32
		if n, _ := fmt.Sscanf(tok, "0x%x-0x%x", &lo, &hi); n == 2 {
		} else if n, _ := fmt.Sscanf(tok, "0x%x", &lo); n == 1 {
			hi = lo
		} else {
			return s, fmt.Errorf("bad pid range %q", tok)
		}
		if lo > hi || hi > PIDMax {
			return s, fmt.Errorf("bad pid range %q", tok)
		}
		s.AddRange(uint16(lo), uint16(hi))
	}
	return s, nil
}

// CRC32MPEG computes the MPEG-2 section CRC (poly 0x04C11DB7, init all-ones).
func CRC32MPEG(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, x := range b {
		crc ^= uint32(x) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// SectionReader reassembles PSI sections delivered on a single PID.
type SectionReader struct {
	buf    []byte
	active bool
}

// Push feeds one TS packet for the PID and returns any completed sections
// (each includes the table header and trailing CRC; CRC is verified).
func (r *SectionReader) Push(p Packet) [][]byte {
	pl := Payload(p)
	if len(pl) == 0 {
		return nil
	}
	var out [][]byte
	if PUSI(p) {
		ptr := int(pl[0])
		pl = pl[1:]
		if ptr > len(pl) {
			r.active = false
			return nil
		}
		if r.active {
			r.buf = append(r.buf, pl[:ptr]...)
			out = r.drain(out)
		}
		pl = pl[ptr:]
		r.buf = r.buf[:0]
		r.active = true
	} else if !r.active {
		return nil
	}
	r.buf = append(r.buf, pl...)
	return r.drain(out)
}

func (r *SectionReader) drain(out [][]byte) [][]byte {
	for len(r.buf) >= 3 {
		if r.buf[0] == 0xFF { // stuffing: rest of packet is padding
			r.buf = r.buf[:0]
			return out
		}
		n := 3 + (int(r.buf[1]&0x0F)<<8 | int(r.buf[2]))
		if n > 4096 {
			r.buf = r.buf[:0]
			r.active = false
			return out
		}
		if len(r.buf) < n {
			return out
		}
		sec := append([]byte(nil), r.buf[:n]...)
		r.buf = r.buf[n:]
		if CRC32MPEG(sec) == 0 {
			out = append(out, sec)
		}
	}
	return out
}

// PAT is a parsed program association table.
type PAT struct {
	TSID     uint16
	Version  byte
	Programs map[uint16]uint16 // program number -> PMT PID (program 0 = NIT, omitted)
}

func ParsePAT(sec []byte) (*PAT, error) {
	if len(sec) < 12 || sec[0] != 0x00 {
		return nil, fmt.Errorf("not a PAT")
	}
	p := &PAT{
		TSID:     uint16(sec[3])<<8 | uint16(sec[4]),
		Version:  sec[5] >> 1 & 0x1F,
		Programs: map[uint16]uint16{},
	}
	body := sec[8 : len(sec)-4]
	for len(body) >= 4 {
		num := uint16(body[0])<<8 | uint16(body[1])
		pid := uint16(body[2]&0x1F)<<8 | uint16(body[3])
		if num != 0 {
			p.Programs[num] = pid
		}
		body = body[4:]
	}
	return p, nil
}

type Stream struct {
	Type uint8
	PID  uint16
}

// PMT is a parsed program map table.
type PMT struct {
	Program uint16
	Version byte
	PCRPID  uint16
	Streams []Stream
	// Raw is the section as received (used to detect changes).
	Raw []byte
}

func ParsePMT(sec []byte) (*PMT, error) {
	if len(sec) < 16 || sec[0] != 0x02 {
		return nil, fmt.Errorf("not a PMT")
	}
	m := &PMT{
		Program: uint16(sec[3])<<8 | uint16(sec[4]),
		Version: sec[5] >> 1 & 0x1F,
		PCRPID:  uint16(sec[8]&0x1F)<<8 | uint16(sec[9]),
		Raw:     sec,
	}
	infoLen := int(sec[10]&0x0F)<<8 | int(sec[11])
	body := sec[12:]
	if len(body) < infoLen+4 {
		return nil, fmt.Errorf("short PMT")
	}
	body = body[infoLen : len(body)-4]
	for len(body) >= 5 {
		es := int(body[3]&0x0F)<<8 | int(body[4])
		m.Streams = append(m.Streams, Stream{body[0], uint16(body[1]&0x1F)<<8 | uint16(body[2])})
		if len(body) < 5+es {
			break
		}
		body = body[5+es:]
	}
	return m, nil
}

// PIDs returns the PCR PID and all elementary stream PIDs.
func (m *PMT) PIDs() []uint16 {
	out := make([]uint16, 0, len(m.Streams)+1)
	out = append(out, m.PCRPID)
	for _, s := range m.Streams {
		out = append(out, s.PID)
	}
	return out
}

// BuildPAT returns a PAT section carrying a single program (or no program
// when pmtPID is 0).
func BuildPAT(tsid uint16, version byte, program, pmtPID uint16) []byte {
	sec := []byte{0x00, 0xB0, 0, byte(tsid >> 8), byte(tsid), 0xC1 | version<<1, 0, 0}
	if pmtPID != 0 {
		sec = append(sec, byte(program>>8), byte(program), 0xE0|byte(pmtPID>>8), byte(pmtPID))
	}
	sec[2] = byte(len(sec) - 3 + 4)
	crc := CRC32MPEG(sec)
	return append(sec, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

// BuildPMT assembles a PMT section (used by the test device).
func BuildPMT(program uint16, version byte, pcrPID uint16, streams []Stream) []byte {
	sec := []byte{0x02, 0xB0, 0, byte(program >> 8), byte(program), 0xC1 | version<<1, 0, 0,
		0xE0 | byte(pcrPID>>8), byte(pcrPID), 0xF0, 0x00}
	for _, s := range streams {
		sec = append(sec, s.Type, 0xE0|byte(s.PID>>8), byte(s.PID), 0xF0, 0x00)
	}
	n := len(sec) - 3 + 4
	sec[1] = 0xB0 | byte(n>>8)
	sec[2] = byte(n)
	crc := CRC32MPEG(sec)
	return append(sec, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

// PacketizeSection wraps a short (single packet) section into a TS packet.
func PacketizeSection(pid uint16, cc byte, sec []byte) []byte {
	p := make([]byte, PacketSize)
	for i := range p {
		p[i] = 0xFF
	}
	p[0] = SyncByte
	p[1] = 0x40 | byte(pid>>8)
	p[2] = byte(pid)
	p[3] = 0x10 | cc&0x0F
	p[4] = 0 // pointer field
	copy(p[5:], sec)
	return p
}

// Packetize wraps a section of any length into transport stream packets on
// pid, advancing the continuity counter *cc as it goes.
func Packetize(pid uint16, cc *byte, sec []byte) [][]byte {
	data := append([]byte{0}, sec...) // pointer_field = 0
	var out [][]byte
	for first := true; len(data) > 0; first = false {
		p := make([]byte, PacketSize)
		for i := range p {
			p[i] = 0xFF
		}
		p[0] = SyncByte
		p[1] = byte(pid >> 8)
		if first {
			p[1] |= 0x40
		}
		p[2] = byte(pid)
		p[3] = 0x10 | *cc&0x0F
		*cc = (*cc + 1) & 0x0F
		n := copy(p[4:], data)
		data = data[n:]
		out = append(out, p)
	}
	return out
}
