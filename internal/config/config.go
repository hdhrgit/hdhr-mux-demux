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
	LogRequests   bool   `json:"log_requests"` // log every client request (diagnostics)
	PprofListen   string `json:"pprof_listen"` // e.g. "127.0.0.1:6060": Go profiling endpoint (off by default)

	StateDir string `json:"state_dir"` // lineup.json and device id live here

	Upstream Upstream `json:"upstream"`
	Lineup   Lineup   `json:"lineup"`

	Linger Duration `json:"linger"` // keep an unused physical tuner tuned this long (default 5s, -1s disables)
	// ClientBufferMB is the per-client queue for slow consumers (default 16).
	ClientBufferMB int      `json:"client_buffer_mb"`
	IdleRelease    Duration `json:"idle_release"` // free tuners left tuned without a viewer
}

type Upstream struct {
	Discover       *bool    `json:"discover"` // broadcast discovery (default true)
	DiscoverTarget []string `json:"discover_targets"`
	Devices        []string `json:"devices"`     // static addresses, "host" or "host:port"
	Allow          []string `json:"allow"`       // device IDs to use (empty = all found)
	Exclude        []string `json:"exclude"`     // "DEVICEID/tuner" never to use
	Mode           string   `json:"mode"`        // "http" (default), "rtp" or "udp"
	StreamPort     int      `json:"stream_port"` // device streaming port (default 5004)
	LocalIP        string   `json:"local_ip"`    // address devices should stream to
	WaitFree       Duration `json:"wait_free"`
}

type Lineup struct {
	ChannelMap    string            `json:"channel_map"`
	Sources       map[string]string `json:"sources"` // source name -> channel map
	DefaultSource string            `json:"default_source"`
	ScanFrom      int               `json:"scan_from"`
	ScanTo        int               `json:"scan_to"`
	ScanOnStart   bool              `json:"scan_on_start"` // when the lineup is empty
	// ScanTuners limits tuners used per device by a scan (0 = every free one).
	ScanTuners int `json:"scan_tuners_per_device"`
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
	if c.SSDP == nil {
		t := true
		c.SSDP = &t
	}
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
