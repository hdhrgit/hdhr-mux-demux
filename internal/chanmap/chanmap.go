// Package chanmap translates channel numbers to centre frequencies for the
// channel maps HDHomeRun devices know about (currently the North American
// ones), and parses HDHomeRun channel strings such as "auto:177000000".
package chanmap

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type Channel struct {
	Number    int
	Frequency uint64 // Hz, centre frequency
}

// Maps lists the supported channel map names.
func Maps() []string {
	m := []string{"us-bcast", "us-cable", "us-hrc"}
	sort.Strings(m)
	return m
}

const mhz = 1000000

// Channels returns the channels of a map in ascending number order.
func Channels(name string) ([]Channel, error) {
	var out []Channel
	switch name {
	case "us-bcast":
		for n := 2; n <= 69; n++ {
			out = append(out, Channel{n, bcast(n)})
		}
	case "us-cable":
		for n := 2; n <= 135; n++ {
			if f := cable(n); f != 0 {
				out = append(out, Channel{n, f})
			}
		}
	case "us-hrc":
		for n := 2; n <= 135; n++ {
			if f := cable(n); f != 0 {
				// HRC carriers are offset 1.25 MHz below standard.
				out = append(out, Channel{n, f - 1250000})
			}
		}
	default:
		return nil, fmt.Errorf("unsupported channel map %q (supported: %s)", name, strings.Join(Maps(), ", "))
	}
	return out, nil
}

// Frequency returns the centre frequency of channel number n in map name.
func Frequency(name string, n int) (uint64, bool) {
	cs, err := Channels(name)
	if err != nil {
		return 0, false
	}
	for _, c := range cs {
		if c.Number == n {
			return c.Frequency, true
		}
	}
	return 0, false
}

func bcast(n int) uint64 {
	switch {
	case n <= 4:
		return uint64(57+6*(n-2)) * mhz
	case n <= 6:
		return uint64(79+6*(n-5)) * mhz
	case n <= 13:
		return uint64(177+6*(n-7)) * mhz
	default:
		return uint64(473+6*(n-14)) * mhz
	}
}

func cable(n int) uint64 {
	switch {
	case n < 2:
		return 0
	case n <= 4:
		return uint64(57+6*(n-2)) * mhz
	case n <= 6:
		return uint64(79+6*(n-5)) * mhz
	case n <= 13:
		return uint64(177+6*(n-7)) * mhz
	case n <= 22:
		return uint64(123+6*(n-14)) * mhz
	case n <= 94:
		return uint64(219+6*(n-23)) * mhz
	case n <= 99:
		return uint64(93+6*(n-95)) * mhz
	case n <= 135:
		return uint64(651+6*(n-100)) * mhz
	}
	return 0
}

// Spec is a parsed HDHomeRun channel string.
type Spec struct {
	Modulation string // "auto", "8vsb", "qam256", ...
	Frequency  uint64 // Hz
}

func (s Spec) String() string { return fmt.Sprintf("%s:%d", s.Modulation, s.Frequency) }

// Key identifies the physical tuning independent of modulation spelling.
func (s Spec) Key() string { return strconv.FormatUint(s.Frequency, 10) }

// Parse understands "none", "<mod>:<hz>", "<mod>:<channel>" (resolved via
// channelmap) and a bare "<hz>". Returns ok=false for "none".
func Parse(v, channelmap string) (spec Spec, ok bool, err error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "none" {
		return Spec{}, false, nil
	}
	mod, num := "auto", v
	if i := strings.IndexByte(v, ':'); i >= 0 {
		mod, num = v[:i], v[i+1:]
	}
	n, perr := strconv.ParseUint(num, 10, 64)
	if perr != nil {
		return Spec{}, false, fmt.Errorf("invalid channel %q", v)
	}
	if n < 1000 { // channel number rather than frequency
		f, found := Frequency(channelmap, int(n))
		if !found {
			return Spec{}, false, fmt.Errorf("channel %d not in channelmap %q", n, channelmap)
		}
		n = f
	}
	return Spec{mod, n}, true, nil
}
