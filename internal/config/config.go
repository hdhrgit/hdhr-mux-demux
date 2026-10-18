// Package config loads the multiplexer's JSON configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"hdhrmux/internal/hdhrproto"
)

// Duration is a time.Duration that unmarshals from strings like "30s".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

type Config struct {
	// DeviceID of the virtual device: 8 hex digits with a valid check
	// digit, or "auto" to derive a stable one from StateDir.
	DeviceID     string `json:"device_id"`
	FriendlyName string `json:"friendly_name"`
	ModelNumber  string `json:"model_number"`
	Manufacturer string `json:"manufacturer"`

	// VirtualTuners is how many virtual tuners to expose. 0 = one per
	// physical tuner, numbered 0..N-1 across all devices in a stable order.
	VirtualTuners int `json:"virtual_tuners"`

	HTTPListen    string `json:"http_listen"`    // web/API, default ":80"
	StreamListen  string `json:"stream_listen"`  // streams, default ":5004" ("-" disables)
	ControlListen string `json:"control_listen"` // default ":65001"
	AdvertiseHost string `json:"advertise_host"`
	SSDP          *bool  `json:"ssdp"`
	// SSDPIPv4 and SSDPIPv6 (default true) choose the IP families SSDP uses for
	// announcements and search answers; ssdp: false switches both off.
	SSDPIPv4 *bool `json:"ssdp_ipv4"`
	SSDPIPv6 *bool `json:"ssdp_ipv6"`
	// SSDPATSC3 (default true) also announces the ATSC 3.0 primary device,
	// as real HDHomeRun 4K devices do.
	SSDPATSC3 *bool `json:"ssdp_atsc3"`
	// Discoverable (default true) lets clients find the multiplexer: it answers
	// wildcard UDP discovery and announces itself over SSDP. When false it is
	// only found by those who know its address or device ID.
	Discoverable *bool  `json:"discoverable"`
	LogRequests  bool   `json:"log_requests"` // log every client request (diagnostics)
	PprofListen  string `json:"pprof_listen"` // e.g. "127.0.0.1:6060": Go profiling endpoint (off by default)

	StateDir string `json:"state_dir"` // lineup.json and device id live here

	Upstream Upstream `json:"upstream"`
	Lineup   Lineup   `json:"lineup"`

	Guide Guide `json:"guide"`

	Linger Duration `json:"linger"` // keep an unused physical tuner tuned this long (default 5s, -1s disables)
	// ClientBufferMB is the per-client queue for slow consumers (default 16).
	ClientBufferMB int      `json:"client_buffer_mb"`
	IdleRelease    Duration `json:"idle_release"` // free tuners left tuned without a viewer
}

// Guide controls the programme guide collected from the broadcasts.
type Guide struct {
	Refresh    Duration `json:"refresh"`     // how often idle multiplexes are listened to (default 3h, -1s = never)
	Hold       Duration `json:"hold"`        // time spent on each multiplex (default 45s)
	Parallel   int      `json:"parallel"`    // multiplexes listened to at once (default 3)
	StartDelay Duration `json:"start_delay"` // wait after start-up (default 90s)
	// Unlocked drops the tuner's lock while listening so others can take it over.
	Unlocked bool `json:"unlocked"`
}

type Upstream struct {
	Discover       *bool    `json:"discover"` // broadcast discovery (default true)
	DiscoverTarget []string `json:"discover_targets"`
	Devices        []string `json:"devices"` // static addresses, "host" or "host:port"
	Allow          []string `json:"allow"`   // device IDs to use (empty = all found)
	// Tuners lists the tuners to use: "DEVICEID", "DEVICEID/N" or "DEVICEID/N-M"
	// (empty = all). Only these are used and numbered.
	Tuners     []string `json:"tuners"`
	Exclude    []string `json:"exclude"`     // "DEVICEID/tuner" never to use
	Mode       string   `json:"mode"`        // "http" (default), "rtp" or "udp"
	StreamPort int      `json:"stream_port"` // device streaming port (default 5004)
	LocalIP    string   `json:"local_ip"`    // address devices should stream to
	WaitFree   Duration `json:"wait_free"`
	// Mask makes the devices send only the PIDs somebody watches, plus the
	// tables and guide, rather than the whole multiplex (default true).
	Mask *bool `json:"mask"`
}

type Lineup struct {
	ChannelMap    string            `json:"channel_map"`
	Sources       map[string]string `json:"sources"` // source name -> channel map
	DefaultSource string            `json:"default_source"`
	ScanFrom      int               `json:"scan_from"`
	ScanTo        int               `json:"scan_to"`
	ScanOnStart   bool              `json:"scan_on_start"` // scan at start-up (off by default: the devices' lineups are used)
	Refresh       Duration          `json:"refresh"`       // re-read the devices' lineups this often (default 10m, -1s = never)
	// ScanTuners limits tuners used per device by a scan (0 = every free one).
	ScanTuners int `json:"scan_tuners_per_device"`
}

// applyEnv lets the environment override the file, using the same variable
// names as the hdhomerunsignal tool where they mean the same thing:
//
//	HDHOMERUN_DEVICES            comma-separated IPs/hostnames of devices to use,
//	                             in addition to auto-discovery
//	HDHOMERUN_DISABLE_DISCOVERY  "true": no auto-discovery, only HDHOMERUN_DEVICES
//	HDHOMERUN_TUNERS             comma-separated tuners to use (DEVICEID, DEVICEID/N,
//	                             DEVICEID/N-M); replaces upstream.tuners
//	HDHOMERUN_DISCOVERABLE       "false": do not let clients discover the multiplexer
func applyEnv(c *Config) {
	split := func(v string) []string {
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if v := os.Getenv("HDHOMERUN_DEVICES"); v != "" {
		c.Upstream.Devices = append(c.Upstream.Devices, split(v)...)
	}
	if v := strings.ToLower(os.Getenv("HDHOMERUN_DISABLE_DISCOVERY")); v == "true" || v == "1" {
		f := false
		c.Upstream.Discover = &f
	}
	if v := os.Getenv("HDHOMERUN_TUNERS"); v != "" {
		c.Upstream.Tuners = split(v)
	}
	if v := strings.ToLower(os.Getenv("HDHOMERUN_DISCOVERABLE")); v == "false" || v == "0" {
		f := false
		c.Discoverable = &f
	} else if v == "true" || v == "1" {
		t := true
		c.Discoverable = &t
	}
}

func Load(path string) (*Config, error) {
	c := &Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if c.StateDir == "" {
		c.StateDir = "."
	}
	if c.HTTPListen == "" {
		c.HTTPListen = ":80"
	}
	if c.ControlListen == "" {
		c.ControlListen = fmt.Sprintf(":%d", hdhrproto.ControlPort)
	}
	if c.Upstream.Discover == nil {
		t := true
		c.Upstream.Discover = &t
	}
	if c.Upstream.Mask == nil {
		t := true
		c.Upstream.Mask = &t
	}
	if c.SSDPIPv4 == nil {
		t := true
		c.SSDPIPv4 = &t
	}
	if c.SSDPIPv6 == nil {
		t := true
		c.SSDPIPv6 = &t
	}
	if c.SSDPATSC3 == nil {
		t := true
		c.SSDPATSC3 = &t
	}
	if c.SSDP == nil {
		t := true
		c.SSDP = &t
	}
	if c.Discoverable == nil {
		t := true
		c.Discoverable = &t
	}
	applyEnv(c)
	if c.StreamListen == "" {
		c.StreamListen = ":5004"
	}
	if c.Linger == 0 {
		c.Linger = Duration(5 * time.Second)
	}
	if c.ClientBufferMB == 0 {
		c.ClientBufferMB = 16
	}
	if c.Upstream.WaitFree == 0 {
		c.Upstream.WaitFree = Duration(3 * time.Second)
	}
	return c, nil
}
