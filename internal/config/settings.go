package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hdhrmux/internal/hdhrproto"
)

// Settings that can be changed from the web interface are stored in
// settings.json in the state directory, in the same shape as the config file,
// and laid over it: config file < settings.json < environment variables. The
// config file itself is never written (it may be read-only, or managed by
// something else).

const settingsFile = "settings.json"

// Field describes one setting for the web interface.
type Field struct {
	Key         string   `json:"key"` // dotted path in the JSON, e.g. "guide.refresh"
	Group       string   `json:"group"`
	Label       string   `json:"label"`
	Help        string   `json:"help"`
	Type        string   `json:"type"` // bool, int, text, duration, list, choice
	Choices     []string `json:"choices,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Env         string   `json:"env,omitempty"` // an environment variable that overrides it
}

var schema = []Field{
	{Key: "friendly_name", Group: "General", Label: "Name", Type: "text", Help: "How the multiplexer shows up in clients and on the web page."},
	{Key: "device_id", Group: "General", Label: "Device ID", Type: "text", Placeholder: "auto", Help: "Eight hex digits with a valid check digit, or auto for a stable generated one. Changing it makes clients see a different device."},
	{Key: "virtual_tuners", Group: "General", Label: "Virtual tuners", Type: "int", Placeholder: "0", Help: "0 gives one per physical tuner, which is what you want."},
	{Key: "discoverable", Group: "General", Label: "Discoverable", Type: "bool", Env: "HDHOMERUN_DISCOVERABLE", Help: "Answer wildcard discovery and announce over SSDP, so clients find the multiplexer by themselves. When off, add it to a client by address."},

	{Key: "bind", Group: "Network", Label: "Bind address", Type: "text", Placeholder: "all interfaces", Help: "Listen on this address only (for example 127.0.0.1 when a DVR runs on this machine). Applies to the addresses below that have no host. Loopback also turns SSDP off."},
	{Key: "http_listen", Group: "Network", Label: "Web / API address", Type: "text", Placeholder: ":80", Help: "host:port the web interface and the HDHomeRun HTTP API listen on. Use 127.0.0.1:80 to serve this machine only."},
	{Key: "stream_listen", Group: "Network", Label: "Streaming address", Type: "text", Placeholder: ":5004", Help: "host:port streams are served on (- turns the extra port off)."},
	{Key: "control_listen", Group: "Network", Label: "Control address", Type: "text", Placeholder: ":65001", Help: "host:port for the HDHomeRun control and discovery protocol (TCP and UDP)."},
	{Key: "advertise_host", Group: "Network", Label: "Advertised host", Type: "text", Placeholder: "automatic", Help: "Address clients are told to use. Only needed behind NAT or a proxy."},
	{Key: "ssdp", Group: "Network", Label: "SSDP / DLNA", Type: "bool", Help: "Announce as a DLNA media server and answer UPnP searches (TVs list the multiplexer as a source)."},
	{Key: "ssdp_ipv4", Group: "Network", Label: "SSDP over IPv4", Type: "bool", Help: "Announcements and answers on 239.255.255.250."},
	{Key: "ssdp_ipv6", Group: "Network", Label: "SSDP over IPv6", Type: "bool", Help: "Announcements and answers on ff02::c, as real devices do. Off by default."},
	{Key: "ssdp_atsc3", Group: "Network", Label: "Announce the ATSC 3.0 device", Type: "bool", Help: "Also announce the ATSC 3.0 primary device real 4K devices announce. Off by default."},

	{Key: "upstream.discover", Group: "Devices", Label: "Find devices by broadcast", Type: "bool", Env: "HDHOMERUN_DISABLE_DISCOVERY", Help: "Look for HDHomeRun devices on the network. Turn off to use only the addresses listed below."},
	{Key: "upstream.devices", Group: "Devices", Label: "Device addresses", Type: "list", Env: "HDHOMERUN_DEVICES", Help: "Addresses of devices to use in addition (or instead). One per line, host or host:port."},
	{Key: "upstream.allow", Group: "Devices", Label: "Only these device IDs", Type: "list", Help: "Limit to these devices. One 8-digit ID per line; empty means every device found."},
	{Key: "upstream.tuners", Group: "Devices", Label: "Only these tuners", Type: "list", Env: "HDHOMERUN_TUNERS", Help: "DEVICEID, DEVICEID/N or DEVICEID/N-M, one per line. Only these tuners are used and numbered; empty means all."},
	{Key: "upstream.exclude", Group: "Devices", Label: "Never use these tuners", Type: "list", Help: "DEVICEID/N, one per line, for tuners another application owns."},
	{Key: "upstream.mask", Group: "Devices", Label: "Pull only what is watched", Type: "bool", Help: "Ask the devices to send only the programmes being watched plus the guide, instead of whole multiplexes (about 19 Mbit/s each)."},
	{Key: "upstream.mode", Group: "Devices", Label: "Transport", Type: "choice", Choices: []string{"http", "rtp", "udp"}, Help: "http works through Docker and NAT. rtp and udp need the devices to reach this machine."},
	{Key: "upstream.stream_port", Group: "Devices", Label: "Device streaming port", Type: "int", Placeholder: "5004", Help: "Port the devices stream on."},

	{Key: "linger", Group: "Streaming", Label: "Keep an unused tuner tuned", Type: "duration", Placeholder: "5s", Help: "So flipping back to a channel is instant. A tuner is given up at once when anybody needs it. -1s turns it off."},
	{Key: "client_buffer_mb", Group: "Streaming", Label: "Buffer per client (MB)", Type: "int", Placeholder: "16", Help: "How much a slow client may fall behind before data is dropped for it."},
	{Key: "idle_release", Group: "Streaming", Label: "Release forgotten tuners after", Type: "duration", Help: "Free a tuner left tuned without a viewer after this long. Empty means never."},
	{Key: "auto_join_direct", Group: "Streaming", Label: "/auto/ joins direct clients", Type: "bool", Help: "An /auto/ request (Channels DVR, the playlist) for a channel that tvheadend or a request for a tuner by number already receives uses that tuner instead of taking another. Direct clients never join anybody."},
	{Key: "auto_join_other_locations", Group: "Streaming", Label: "/auto/ rides other locations", Type: "bool", Help: "The same station can be received on a second RF channel the lineup does not list. An /auto/ request rides an open session that carries it there instead of taking a tuner."},
	{Key: "evict_scans", Group: "Streaming", Label: "Guide scans give way to viewers", Type: "bool", Help: "When every tuner is busy, an /auto/ request takes one from a client that is only collecting tables (tvheadend's idle guide scan). Clients watching a programme are never touched."},
	{Key: "stream_ttl", Group: "Network", Label: "Stream TTL (hops)", Type: "int", Placeholder: "3", Help: "IP time-to-live of the video sent to clients, 3 like the devices, so it cannot be taken beyond a couple of routers. At most 8; larger values are cut to 8. Use a VPN for remote viewing."},

	{Key: "lineup.channel_map", Group: "Lineup", Label: "Channel map", Type: "choice", Choices: []string{"us-bcast", "us-cable", "us-hrc", "us-irc", "kr-bcast", "kr-cable", "eu-bcast", "eu-cable", "au-bcast", "au-cable", "tw-bcast", "tw-cable"}, Help: "Used for scans and for tuning by channel number."},
	{Key: "lineup.refresh", Group: "Lineup", Label: "Re-read device lineups every", Type: "duration", Placeholder: "10m", Help: "The channel list comes from the devices; -1s never refreshes."},
	{Key: "lineup.scan_mode", Group: "Lineup", Label: "Rescan with", Type: "choice", Choices: []string{"device", "own"}, Help: "device hands a rescan (Channels DVR asks daily) to one idle device's own scan; own runs the multiplexer's scan on every free tuner."},
	{Key: "lineup.scan_on_start", Group: "Lineup", Label: "Scan at start-up", Type: "bool", Help: "Run a channel scan when the multiplexer starts (off by default: the devices' own lineups are used)."},

	{Key: "guide.refresh", Group: "Guide", Label: "Collect the guide every", Type: "duration", Placeholder: "3h", Help: "How often idle multiplexes are listened to for programme data. -1s never."},
	{Key: "guide.hold", Group: "Guide", Label: "Time on each multiplex", Type: "duration", Placeholder: "45s", Help: "How long each multiplex is listened to during a guide pass."},
	{Key: "guide.parallel", Group: "Guide", Label: "Multiplexes at once", Type: "int", Placeholder: "3", Help: "How many multiplexes a guide pass listens to at the same time."},
	{Key: "guide.start_delay", Group: "Guide", Label: "Wait after start-up", Type: "duration", Placeholder: "90s", Help: "Delay before the first guide pass."},
	{Key: "guide.unlocked", Group: "Guide", Label: "Leave tuners unlocked", Type: "bool", Help: "Let other applications take a tuner a guide pass is using."},

	{Key: "log_requests", Group: "Diagnostics", Label: "Log every request", Type: "bool", Help: "Write every client request to the log. Noisy; for finding out what a client does."},
	{Key: "pprof_listen", Group: "Diagnostics", Label: "Profiling address", Type: "text", Placeholder: "off", Help: "For example 127.0.0.1:6060. Go profiling endpoint; leave empty unless you are debugging."},
}

// Schema lists the settings the web interface offers.
func Schema() []Field { return append([]Field(nil), schema...) }

var (
	reDeviceID = regexp.MustCompile(`^[0-9A-Fa-f]{8}$`)
	reTuner    = regexp.MustCompile(`^[0-9A-Fa-f]{8}(/[0-9]+(-[0-9]+)?)?$`)
	reExclude  = regexp.MustCompile(`^[0-9A-Fa-f]{8}/[0-9]+$`)
)

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func getPath(m map[string]any, key string) (any, bool) {
	var cur any = m
	for _, p := range strings.Split(key, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func setPath(m map[string]any, key string, v any) {
	parts := strings.Split(key, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

func deletePath(m map[string]any, key string) {
	parts := strings.Split(key, ".")
	var chain []map[string]any
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			return
		}
		chain = append(chain, m)
		m = next
	}
	delete(m, parts[len(parts)-1])
	// drop objects left empty
	for i := len(chain) - 1; i >= 0 && len(m) == 0; i-- {
		delete(chain[i], parts[i])
		m = chain[i]
	}
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// Manager reads and writes the web-editable settings of a running instance.
type Manager struct {
	path string // the config file ("" if none)
	cfg  *Config
}

// NewManager returns a manager for cfg, which was loaded from path.
func NewManager(path string, cfg *Config) *Manager { return &Manager{path: path, cfg: cfg} }

// ConfigPath is the config file the multiplexer was started with.
func (m *Manager) ConfigPath() string { return m.path }

// FilePath is where the web-edited settings are stored.
func (m *Manager) FilePath() string { return filepath.Join(m.cfg.StateDir, settingsFile) }

// Values returns the settings in effect, by key.
func (m *Manager) Values() map[string]any {
	b, _ := json.Marshal(m.cfg)
	var all map[string]any
	json.Unmarshal(b, &all)
	out := map[string]any{}
	for _, f := range schema {
		if v, ok := getPath(all, f.Key); ok {
			if f.Type == "duration" && v == "0s" {
				v = "" // not set: the built-in default applies (shown as the placeholder)
			}
			out[f.Key] = v
		}
	}
	return out
}

// Saved returns the keys that currently have a value saved from the web
// interface.
func (m *Manager) Saved() []string {
	ov, _ := readOverlay(m.cfg.StateDir)
	keys := []string{}
	for _, f := range schema {
		if _, ok := getPath(ov, f.Key); ok {
			keys = append(keys, f.Key)
		}
	}
	return keys
}

// OverriddenByEnv lists the settings an environment variable is overriding.
func (m *Manager) OverriddenByEnv() map[string]string {
	out := map[string]string{}
	for _, f := range schema {
		if f.Env != "" && os.Getenv(f.Env) != "" {
			out[f.Key] = f.Env
		}
	}
	return out
}

func readOverlay(stateDir string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, settingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(stateDir, settingsFile), err)
	}
	return m, nil
}

// normalize checks one value from the web interface against its field and
// returns it in the form stored in JSON.
func normalize(f Field, v any) (any, error) {
	switch f.Type {
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("must be on or off")
		}
		return b, nil
	case "int":
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case string:
			p, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil {
				return nil, errors.New("must be a number")
			}
			n = p
		default:
			return nil, errors.New("must be a number")
		}
		if n != float64(int(n)) || n < 0 || n > 1_000_000 {
			return nil, errors.New("must be a whole number, 0 or more")
		}
		return int(n), nil
	case "duration":
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be a duration like 30s, 5m or 3h")
		}
		s = strings.TrimSpace(s)
		if _, err := time.ParseDuration(s); err != nil {
			return nil, errors.New("must be a duration like 30s, 5m or 3h (-1s switches it off)")
		}
		return s, nil
	case "choice":
		s, _ := v.(string)
		for _, c := range f.Choices {
			if s == c {
				return s, nil
			}
		}
		return nil, fmt.Errorf("must be one of %s", strings.Join(f.Choices, ", "))
	case "list":
		var out []string
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
		case string:
			for _, s := range strings.FieldsFunc(x, func(r rune) bool { return r == '\n' || r == ',' }) {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		default:
			return nil, errors.New("must be a list")
		}
		for _, s := range out {
			ok := true
			switch f.Key {
			case "upstream.allow":
				ok = reDeviceID.MatchString(s)
			case "upstream.tuners":
				ok = reTuner.MatchString(s)
			case "upstream.exclude":
				ok = reExclude.MatchString(s)
			}
			if !ok {
				return nil, fmt.Errorf("%q is not valid here", s)
			}
		}
		if out == nil {
			out = []string{}
		}
		return out, nil
	default: // text
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be text")
		}
		s = strings.TrimSpace(s)
		switch f.Key {
		case "device_id":
			if s != "" && !strings.EqualFold(s, "auto") {
				id, err := hdhrproto.ParseDeviceID(strings.ToUpper(s))
				if err != nil || !hdhrproto.ValidDeviceID(id) {
					return nil, errors.New("must be auto or 8 hex digits with a valid check digit")
				}
			}
		case "bind":
			if s != "" && net.ParseIP(s) == nil {
				return nil, errors.New("must be an IP address such as 127.0.0.1")
			}
		case "http_listen", "control_listen", "stream_listen", "pprof_listen":
			if s != "" && s != "-" {
				if _, _, err := net.SplitHostPort(s); err != nil {
					return nil, errors.New("must look like host:port, for example :80 or 127.0.0.1:80")
				}
			}
		}
		return s, nil
	}
}

// Save stores changes (key to value; a nil value removes the saved setting, so
// the config file's value or the default applies again). Nothing is written
// unless the result loads as a valid configuration. Most settings take effect
// when the multiplexer restarts.
func (m *Manager) Save(changes map[string]any) error {
	fields := map[string]Field{}
	for _, f := range schema {
		fields[f.Key] = f
	}
	ov, err := readOverlay(m.cfg.StateDir)
	if err != nil {
		return err
	}
	var problems []string
	for key, v := range changes {
		f, ok := fields[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: unknown setting", key))
			continue
		}
		if v == nil {
			deletePath(ov, key)
			continue
		}
		nv, err := normalize(f, v)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f.Label, err))
			continue
		}
		setPath(ov, key, nv)
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	b, _ := json.MarshalIndent(ov, "", "  ")
	if _, err := loadConfig(m.path, b, false); err != nil {
		return fmt.Errorf("the result is not a valid configuration: %w", err)
	}
	if err := os.MkdirAll(m.cfg.StateDir, 0o755); err != nil {
		return err
	}
	dst := m.FilePath()
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot write %s: %w", dst, err)
	}
	return nil
}
