// Package hdhrproto implements the SiliconDust HDHomeRun discovery and
// control wire protocol (UDP/TCP port 65001): framing, TLV tags, CRC and
// device-ID helpers.
package hdhrproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const ControlPort = 65001

// Frame types.
const (
	TypeDiscoverReq uint16 = 0x0002
	TypeDiscoverRpy uint16 = 0x0003
	TypeGetSetReq   uint16 = 0x0004
	TypeGetSetRpy   uint16 = 0x0005
	TypeUpgradeReq  uint16 = 0x0006
	TypeUpgradeRpy  uint16 = 0x0007
)

// TLV tags.
const (
	TagDeviceType   byte = 0x01
	TagDeviceID     byte = 0x02
	TagGetSetName   byte = 0x03
	TagGetSetValue  byte = 0x04
	TagErrorMessage byte = 0x05
	TagTunerCount   byte = 0x10
	TagGetSetLock   byte = 0x15
	TagLineupURL    byte = 0x27
	TagStorageURL   byte = 0x28
	TagBaseURL      byte = 0x2A
	TagDeviceAuth   byte = 0x2B
	TagStorageID    byte = 0x2C
)

// Device types.
const (
	DeviceTypeWildcard uint32 = 0xFFFFFFFF
	DeviceTypeTuner    uint32 = 0x00000001
	DeviceTypeStorage  uint32 = 0x00000005
	DeviceIDWildcard   uint32 = 0xFFFFFFFF
)

// MaxFrame bounds the payload we are willing to accept.
const MaxFrame = 8192

type Tag struct {
	ID    byte
	Value []byte
}

type Packet struct {
	Type uint16
	Tags []Tag
}

var (
	ErrShort = errors.New("hdhrproto: short packet")
	ErrCRC   = errors.New("hdhrproto: bad crc")
)

// Get returns the first tag value with the given id.
func (p *Packet) Get(id byte) ([]byte, bool) {
	for _, t := range p.Tags {
		if t.ID == id {
			return t.Value, true
		}
	}
	return nil, false
}

// GetString returns a tag as a string with any trailing NUL removed.
func (p *Packet) GetString(id byte) (string, bool) {
	v, ok := p.Get(id)
	if !ok {
		return "", false
	}
	if n := len(v); n > 0 && v[n-1] == 0 {
		v = v[:n-1]
	}
	return string(v), true
}

func (p *Packet) GetU32(id byte) (uint32, bool) {
	v, ok := p.Get(id)
	if !ok || len(v) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(v), true
}

func (p *Packet) Add(id byte, v []byte) { p.Tags = append(p.Tags, Tag{id, v}) }

// AddString adds a NUL-terminated string tag.
func (p *Packet) AddString(id byte, s string) {
	b := make([]byte, len(s)+1)
	copy(b, s)
	p.Add(id, b)
}

func (p *Packet) AddU32(id byte, v uint32) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	p.Add(id, b)
}

// Encode serialises the packet: type, length (big endian), tags, CRC32 (LE).
func (p *Packet) Encode() []byte {
	var body []byte
	for _, t := range p.Tags {
		body = append(body, t.ID)
		n := len(t.Value)
		if n <= 127 {
			body = append(body, byte(n))
		} else {
			body = append(body, byte(n&0x7F)|0x80, byte(n>>7))
		}
		body = append(body, t.Value...)
	}
	out := make([]byte, 4, 4+len(body)+4)
	binary.BigEndian.PutUint16(out[0:], p.Type)
	binary.BigEndian.PutUint16(out[2:], uint16(len(body)))
	out = append(out, body...)
	var crc [4]byte
	binary.LittleEndian.PutUint32(crc[:], crc32.ChecksumIEEE(out))
	return append(out, crc[:]...)
}

// FrameLen reports the total number of bytes of the frame whose 4-byte
// header is hdr.
func FrameLen(hdr []byte) int { return 4 + int(binary.BigEndian.Uint16(hdr[2:4])) + 4 }

// Decode parses and CRC-checks one complete frame.
func Decode(b []byte) (*Packet, error) {
	if len(b) < 8 {
		return nil, ErrShort
	}
	if FrameLen(b) != len(b) {
		if FrameLen(b) > len(b) {
			return nil, ErrShort
		}
		b = b[:FrameLen(b)]
	}
	got := binary.LittleEndian.Uint32(b[len(b)-4:])
	if crc32.ChecksumIEEE(b[:len(b)-4]) != got {
		return nil, ErrCRC
	}
	p := &Packet{Type: binary.BigEndian.Uint16(b[0:2])}
	body := b[4 : len(b)-4]
	for len(body) > 0 {
		if len(body) < 2 {
			return nil, ErrShort
		}
		id := body[0]
		n := int(body[1])
		body = body[2:]
		if n&0x80 != 0 {
			if len(body) < 1 {
				return nil, ErrShort
			}
			n = n&0x7F | int(body[0])<<7
			body = body[1:]
		}
		if len(body) < n {
			return nil, ErrShort
		}
		p.Tags = append(p.Tags, Tag{id, append([]byte(nil), body[:n]...)})
		body = body[n:]
	}
	return p, nil
}

var idLookup = [16]byte{0xA, 0x5, 0xF, 0x6, 0x7, 0xC, 0x1, 0xB, 0x9, 0x2, 0x8, 0xD, 0x4, 0x3, 0xE, 0x0}

func idChecksum(id uint32) byte {
	var c byte
	for shift := 28; shift >= 0; shift -= 8 {
		c ^= idLookup[(id>>uint(shift))&0xF]
		c ^= byte(id>>uint(shift-4)) & 0xF
	}
	return c
}

// ValidDeviceID reports whether id carries a correct check nibble.
func ValidDeviceID(id uint32) bool { return idChecksum(id) == 0 }

// MakeDeviceID replaces the low nibble of seed with the correct check nibble.
func MakeDeviceID(seed uint32) uint32 {
	seed &^= 0xF
	return seed | uint32(idChecksum(seed))
}

// ParseDeviceID parses 8 hex digits.
func ParseDeviceID(s string) (uint32, error) {
	var id uint32
	if len(s) != 8 {
		return 0, fmt.Errorf("device id %q: want 8 hex digits", s)
	}
	if _, err := fmt.Sscanf(s, "%08X", &id); err != nil {
		return 0, fmt.Errorf("device id %q: %v", s, err)
	}
	return id, nil
}

func FormatDeviceID(id uint32) string { return fmt.Sprintf("%08X", id) }
