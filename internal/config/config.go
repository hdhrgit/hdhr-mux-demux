// Package config loads the multiplexer's JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hdhrmux/internal/hdhrproto"
)

// Loopback reports whether the multiplexer is bound to the loopback address only.
func (c *Config) Loopback() bool {
	ip := net.ParseIP(c.Bind)
	return ip != nil && ip.IsLoopback()
}

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

	// Bind restricts every listener whose address has no host (the default
	// ":80", ":5004", ":65001") to this address. "127.0.0.1" serves only this
	// machine, for a DVR running next to the multiplexer: SSDP is then off, and
	// the DVR finds the multiplexer by address, not by broadcast.
	Bind string `json:"bind"`

	HTTPListen    string `json:"http_listen"`    // web/API, default ":80"
	StreamListen  string `json:"stream_listen"`  // streams, default ":5004" ("-" disables)
	ControlListen string `json:"control_listen"` // default ":65001"
	AdvertiseHost string `json:"advertise_host"`
	SSDP          *bool  `json:"ssdp"`
	// SSDPIPv4 (default true) and SSDPIPv6 (default false) choose the IP families
	// SSDP uses for announcements and search answers; ssdp: false switches both off.
	SSDPIPv4 *bool `json:"ssdp_ipv4"`
	SSDPIPv6 *bool `json:"ssdp_ipv6"`
	// SSDPATSC3 (default false) also announces the ATSC 3.0 primary device,
	// as real HDHomeRun 4K devices do.
	SSDPATSC3 *bool `json:"ssdp_atsc3"`
	// Discoverable lets clients find the multiplexer: it answers wildcard UDP
	// discovery and announces itself over SSDP. It is off until somebody turns it on
	// (settings page, config file, HDHOMERUN_DISCOVERABLE or -discoverable): a new
	// installation does not announce itself on the network by surprise. When off it
	// is only found by those who know its address or device ID.
	Discoverable *bool `json:"discoverable"`
	// DiscoverableDefault says nobody chose: it is off because that is the default.
	DiscoverableDefault bool `json:"-"`
	// SettingsLocked refuses every change from the web interface's settings page.
	SettingsLocked bool `json:"settings_locked"`
	LogRequests    bool `json:"log_requests"` // log every client request (diagnostics)
	// LogRequestsOn limits what log_requests logs to requests received on these: web (the web /
	// API port), udp and tcp (the control port), stream (the streaming port). Empty: all.
	LogRequestsOn string `json:"log_requests_on"`
	PprofListen   string `json:"pprof_listen"` // e.g. "127.0.0.1:6060": Go profiling endpoint (off by default)

	StateDir string `json:"state_dir"` // lineup.json and device id live here
	// HTTPListenDefault says http_listen was not set anywhere, so it may fall back to
	// another port if port 80 cannot be used.
	HTTPListenDefault bool `json:"-"`
	// Path is the config file this was loaded from (not part of the file).
	Path string `json:"-"`

	Upstream Upstream `json:"upstream"`
	Lineup   Lineup   `json:"lineup"`

	Guide Guide `json:"guide"`

	Linger Duration `json:"linger"` // keep an unused physical tuner tuned this long (default 5s, 0 disables)
	// SignalSettle (default 2s, 0 = off): for this long after a stream starts, a symbol quality of
	// 0 (how a tuner starts before its figures climb) is not shown to clients. Channels DVR
	// counts each such second as a signal error.
	SignalSettle Duration `json:"signal_settle"`
	// AutoJoinDirect (default true): an /auto/ request for a channel a direct client
	// (tvheadend over the control protocol, or a request for a tuner by number) is
	// already receiving joins that tuner instead of taking another. Never the other
	// way round: a direct client does not join an /auto/ stream.
	AutoJoinDirect *bool `json:"auto_join_direct"`
	// AutoJoinOtherLocations (default true): an /auto/ request for a channel that a
	// session already open receives on another RF channel than the lineup lists (the
	// same station via another antenna or feed) rides that session.
	AutoJoinOtherLocations *bool `json:"auto_join_other_locations"`
	// EvictScans (default true): an /auto/ request that finds every tuner busy takes one
	// from a control client that only collects tables (tvheadend's idle guide scan).
	EvictScans *bool `json:"evict_scans"`
	// StreamTTL is the IP TTL of the video the multiplexer sends (HTTP streams, UDP/RTP
	// targets), 3 by default like the devices, so that it does not travel beyond a
	// couple of routers. Values above 8 are quietly reduced to 8; 0 means the default.
	StreamTTL *int `json:"stream_ttl"`
	// ClientBufferMB is the per-client queue for slow consumers (default 16).
	ClientBufferMB int      `json:"client_buffer_mb"`
	IdleRelease    Duration `json:"idle_release"` // free tuners left tuned without a viewer
}

// Guide controls the programme guide collected from the broadcasts.
type Guide struct {
	Refresh    Duration `json:"refresh"`     // how often idle multiplexes are listened to (default 1h, 0 = never)
	Hold       Duration `json:"hold"`        // time spent on each multiplex (default 45s)
	Parallel   int      `json:"parallel"`    // multiplexes listened to at once (default 3)
	StartDelay Duration `json:"start_delay"` // wait after start-up (default 90s, 0 = at once)
	// Unlocked drops the tuner's lock while listening so others can take it over (default true).
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
	Refresh       Duration          `json:"refresh"`       // re-read the devices lineups this often (default 10m, 0 = never)
	// ScanMode: "device" (default) hands a rescan to one idle device's own firmware
	// scan, preferring one with ATSC 3.0 tuners; "own" runs the multiplexer's own scan
	// on every free tuner of every device.
	ScanMode string `json:"scan_mode"`
	// ScanTuners limits tuners used per device by a scan (0 = every free one).
	ScanTuners int `json:"scan_tuners_per_device"`
	// ATSC3 (default true) lists ATSC 3.0 channels in the lineup, playlist and guide; false
	// leaves them out of the listings (they still tune when asked for by number).
	ATSC3 *bool `json:"atsc3"`
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
	split := SplitList
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
		c.DiscoverableDefault = false
	} else if v == "true" || v == "1" {
		t := true
		c.Discoverable = &t
		c.DiscoverableDefault = false
	}
}

// Load reads the config file (if any), lays the settings saved from the web
// interface over it, then the environment, and fills in defaults.
func Load(path string) (*Config, error) { return loadConfig(path, nil, true) }

// LoadWithoutSettings is Load without settings.json: the way out when a saved
// setting stops the multiplexer from starting.
func LoadWithoutSettings(path string) (*Config, error) { return loadConfig(path, []byte("{}"), true) }

// loadConfig builds the configuration. overlay, if not nil, replaces settings.json.
func loadConfig(path string, overlay []byte, env bool) (*Config, error) {
	c := &Config{}
	base := map[string]any{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &base); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if overlay == nil {
		stateDir, _ := base["state_dir"].(string)
		if stateDir == "" {
			stateDir = "."
		}
		var err error
		if overlay, err = os.ReadFile(filepath.Join(stateDir, settingsFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if len(bytes.TrimSpace(overlay)) > 0 {
		ov := map[string]any{}
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return nil, fmt.Errorf("settings.json: %w", err)
		}
		deepMerge(base, ov)
	}
	if len(base) > 0 {
		b, _ := json.Marshal(base)
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	c.Path = path
	c.HTTPListenDefault = !has(base, "http_listen")
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
	if c.SSDPIPv6 == nil { // off unless asked for: the conservative choice
		f := false
		c.SSDPIPv6 = &f
	}
	if c.SSDPATSC3 == nil {
		f := false
		c.SSDPATSC3 = &f
	}
	if c.SSDP == nil {
		t := true
		c.SSDP = &t
	}
	if c.Discoverable == nil {
		f := false
		c.Discoverable, c.DiscoverableDefault = &f, true
	}
	if c.AutoJoinDirect == nil {
		t := true
		c.AutoJoinDirect = &t
	}
	if c.AutoJoinOtherLocations == nil {
		t := true
		c.AutoJoinOtherLocations = &t
	}
	if c.EvictScans == nil {
		t := true
		c.EvictScans = &t
	}
	if c.Lineup.ATSC3 == nil {
		t := true
		c.Lineup.ATSC3 = &t
	}
	// The streams must not travel the internet at large (the devices push a lot of
	// bandwidth, which would make a fine weapon): 3 unless set, never more than 8.
	n := 3
	if c.StreamTTL != nil && *c.StreamTTL > 0 {
		n = min(*c.StreamTTL, 8)
	}
	c.StreamTTL = &n
	if env {
		applyEnv(c)
	}
	if c.StreamListen == "" {
		c.StreamListen = ":5004"
	}
	switch c.Lineup.ScanMode {
	case "":
		c.Lineup.ScanMode = "device"
	case "device", "own":
	default:
		return nil, fmt.Errorf("lineup.scan_mode %q: want device or own", c.Lineup.ScanMode)
	}
	if c.Bind != "" {
		if net.ParseIP(c.Bind) == nil {
			return nil, fmt.Errorf("bind %q is not an IP address", c.Bind)
		}
		for _, a := range []*string{&c.HTTPListen, &c.StreamListen, &c.ControlListen} {
			if strings.HasPrefix(*a, ":") {
				*a = net.JoinHostPort(c.Bind, (*a)[1:])
			}
		}
	}
	// A duration that is absent gets its default; 0 switches the thing off (a negative
	// value, the old way to say so, still does).
	setDur := func(d *Duration, key string, def time.Duration) {
		if !has(base, key) {
			*d = Duration(def)
		} else if *d < 0 {
			*d = 0
		}
	}
	setDur(&c.Linger, "linger", 5*time.Second)
	setDur(&c.SignalSettle, "signal_settle", 2*time.Second)
	setDur(&c.Guide.Refresh, "guide.refresh", time.Hour)
	setDur(&c.Guide.StartDelay, "guide.start_delay", 90*time.Second)
	setDur(&c.Lineup.Refresh, "lineup.refresh", 10*time.Minute)
	if !has(base, "guide.unlocked") { // other applications may take a tuner a guide pass is using
		c.Guide.Unlocked = true
	}
	if c.ClientBufferMB == 0 {
		c.ClientBufferMB = 16
	}
	if c.Upstream.WaitFree == 0 {
		c.Upstream.WaitFree = Duration(3 * time.Second)
	}
	return c, nil
}

// has reports whether the dotted key is present in the (merged) configuration.
func has(m map[string]any, key string) bool {
	var cur any = m
	for _, p := range strings.Split(key, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = mm[p]; !ok {
			return false
		}
	}
	return true
}
