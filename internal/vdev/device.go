// Package vdev implements the virtual HDHomeRun device the multiplexer
// presents to clients: discovery, the get/set control protocol, the HTTP API
// and streaming endpoints, SSDP, and lineup scanning.
package vdev

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"hdhrmux/internal/chanmap"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/lineup"
)

type Config struct {
	DeviceID        uint32
	FriendlyName    string
	Manufacturer    string
	ModelNumber     string
	FirmwareName    string
	FirmwareVersion string

	Tuners int // number of virtual tuners

	// ChannelMap is the initial channel map of every tuner.
	ChannelMap string
	// Sources maps a lineup source name to the channel map scanned for it.
	Sources       map[string]string
	DefaultSource string
	ScanFrom      int // first/last channel number to scan (0 = whole map)
	// LineupRefresh is how often the devices' own lineups are re-read (0 =
	// only when asked, e.g. by a rescan).
	LineupRefresh time.Duration
	// ScanTuners limits how many tuners of each device a scan uses (0 = every
	// free one, like a real device's own scan; viewers are never evicted).
	ScanTuners int
	ScanTo     int

	HTTPAddr string // web/API port, e.g. ":80"
	// StreamAddr is the streaming port real devices use (default ":5004");
	// "-" disables it. It serves the same handler as HTTPAddr.
	StreamAddr  string
	ControlAddr string // TCP+UDP, e.g. ":65001"
	// AdvertiseHost overrides the host placed in BaseURL for discovery.
	AdvertiseHost string
	SSDP          bool
	// Hidden stops the multiplexer advertising itself: wildcard UDP discovery
	// requests are not answered and SSDP is not started. Requests naming its
	// device ID, and direct connections, still work.
	Hidden bool
	// LogRequests logs every discovery, control and HTTP request received,
	// for finding out what a client asks for.
	LogRequests bool

	// IdleRelease frees a tuned tuner nobody is using after this long.
	IdleRelease time.Duration
	// ScanLockWait and ScanInfoWait bound the time spent on each RF channel
	// while scanning (defaults suit real hardware).
	ScanLockWait time.Duration
	ScanInfoWait time.Duration
	// LockTimeout expires an idle lockkey.
	LockTimeout time.Duration
}

func (c *Config) defaults() {
	if c.FriendlyName == "" {
		c.FriendlyName = "HDHomeRun Mux"
	}
	if c.Manufacturer == "" {
		c.Manufacturer = "hdhr-mux"
	}
	if c.ModelNumber == "" {
		c.ModelNumber = "HDHR-MUX"
	}
	if c.FirmwareName == "" {
		c.FirmwareName = "hdhomerun_mux"
	}
	if c.FirmwareVersion == "" {
		c.FirmwareVersion = "20240101"
	}
	if c.ChannelMap == "" {
		c.ChannelMap = "us-bcast"
	}
	if c.Sources == nil {
		c.Sources = map[string]string{"Antenna": "us-bcast", "Cable": "us-cable"}
	}
	if c.DefaultSource == "" {
		c.DefaultSource = "Antenna"
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":80"
	}
	if c.StreamAddr == "" {
		c.StreamAddr = ":5004"
	}
	if c.ControlAddr == "" {
		c.ControlAddr = fmt.Sprintf(":%d", hdhrproto.ControlPort)
	}
	if c.IdleRelease == 0 {
		c.IdleRelease = 60 * time.Second
	}
	if c.LockTimeout == 0 {
		c.LockTimeout = 30 * time.Second
	}
}

type Device struct {
	cfg    Config
	hub    *hub.Hub
	lineup *lineup.Lineup

	tuners []*VTuner

	httpLn   net.Listener
	streamLn net.Listener
	tcpLn    net.Listener
	udpConn  net.PacketConn
	httpSrv  *http.Server
	ssdp     *ssdpServer

	scanMu    sync.Mutex
	scanning  bool
	scanProg  hub.Progress
	scanCtx   context.CancelFunc
	scanEnd   string // outcome of the last scan: complete or aborted (empty if none)
	scanFound int
	source    string

	quit chan struct{}
	wg   sync.WaitGroup
}

func New(cfg Config, h *hub.Hub, lu *lineup.Lineup) (*Device, error) {
	cfg.defaults()
	if cfg.Tuners <= 0 {
		return nil, errors.New("vdev: need at least one virtual tuner")
	}
	if cfg.DeviceID == 0 || !hdhrproto.ValidDeviceID(cfg.DeviceID) {
		return nil, fmt.Errorf("vdev: invalid device id %08X", cfg.DeviceID)
	}
	if _, err := chanmap.Channels(cfg.ChannelMap); err != nil {
		return nil, err
	}
	d := &Device{cfg: cfg, hub: h, lineup: lu, quit: make(chan struct{}), source: cfg.DefaultSource}
	h.SetLearner(d.learn)
	for i := 0; i < cfg.Tuners; i++ {
		d.tuners = append(d.tuners, newVTuner(d, i))
	}
	return d, nil
}

// Start opens all listeners and begins serving.
func (d *Device) Start() error {
	var err error
	d.httpLn, err = net.Listen("tcp", d.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}
	if d.cfg.StreamAddr != "-" {
		d.streamLn, err = net.Listen("tcp", d.cfg.StreamAddr)
		if err != nil {
			d.httpLn.Close()
			return fmt.Errorf("stream listen: %w", err)
		}
	}
	if err := d.listenControl(); err != nil {
		d.httpLn.Close()
		if d.streamLn != nil {
			d.streamLn.Close()
		}
		return err
	}
	h := d.routes()
	if d.cfg.LogRequests {
		inner := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("req: http %s %s %s (ua=%q)", r.RemoteAddr, r.Method, r.URL.RequestURI(), r.UserAgent())
			inner.ServeHTTP(w, r)
		})
	}
	d.httpSrv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	d.wg.Add(3)
	go func() { defer d.wg.Done(); d.httpSrv.Serve(d.httpLn) }()
	if d.streamLn != nil {
		d.wg.Add(1)
		go func() { defer d.wg.Done(); d.httpSrv.Serve(d.streamLn) }()
	}
	go func() { defer d.wg.Done(); hdhrproto.ServeTCP(d.tcpLn, d) }()
	go func() { defer d.wg.Done(); hdhrproto.ServeUDP(d.udpConn, d) }()
	d.wg.Add(1)
	go d.reaper()
	if d.cfg.LineupRefresh > 0 {
		d.wg.Add(1)
		go d.lineupLoop()
	}
	if d.cfg.SSDP && !d.cfg.Hidden {
		d.ssdp = newSSDP(d)
		if err := d.ssdp.start(); err != nil {
			log.Printf("ssdp disabled: %v", err)
			d.ssdp = nil
		}
	}
	return nil
}

func (d *Device) listenControl() error {
	host, port, err := net.SplitHostPort(d.cfg.ControlAddr)
	if err != nil {
		return err
	}
	for try := 0; try < 20; try++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
		if err != nil {
			return fmt.Errorf("control tcp listen: %w", err)
		}
		p := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		pc, err := net.ListenPacket("udp4", net.JoinHostPort(host, p))
		if err != nil {
			ln.Close()
			if port == "0" {
				continue
			}
			return fmt.Errorf("control udp listen: %w", err)
		}
		d.tcpLn, d.udpConn = ln, pc
		return nil
	}
	return errors.New("no free control port")
}

func (d *Device) HTTPPort() int { return d.httpLn.Addr().(*net.TCPAddr).Port }

// StreamPort is the port streams are served on (the API port if the
// dedicated streaming port is disabled).
func (d *Device) StreamPort() int {
	if d.streamLn != nil {
		return d.streamLn.Addr().(*net.TCPAddr).Port
	}
	return d.HTTPPort()
}
func (d *Device) ControlPort() int { return d.tcpLn.Addr().(*net.TCPAddr).Port }
func (d *Device) ID() uint32       { return d.cfg.DeviceID }

func (d *Device) Close() {
	close(d.quit)
	if d.ssdp != nil {
		d.ssdp.stop()
	}
	d.scanMu.Lock()
	if d.scanCtx != nil {
		d.scanCtx()
	}
	d.scanMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.httpSrv.Shutdown(ctx)
	d.httpSrv.Close()
	d.tcpLn.Close()
	d.udpConn.Close()
	for _, v := range d.tuners {
		v.op.Lock()
		v.releaseAll(true)
		v.op.Unlock()
	}
	d.wg.Wait()
}

func (d *Device) reaper() {
	defer d.wg.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-d.quit:
			return
		case <-t.C:
			for _, v := range d.tuners {
				v.reap(d.cfg.IdleRelease)
			}
		}
	}
}

// advertiseURL builds the base URL clients should use to reach the HTTP API,
// given the local address that faces them.
func (d *Device) baseURL(host string) string {
	if d.cfg.AdvertiseHost != "" {
		host = d.cfg.AdvertiseHost
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(d.HTTPPort()))
}

// localFor returns the local address used to reach remote.
func localFor(remote net.IP) string {
	if remote == nil {
		return "127.0.0.1"
	}
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: remote, Port: 9})
	if err != nil {
		return "127.0.0.1"
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

// ---- hdhrproto.Handler -------------------------------------------------

func (d *Device) Discover(remote net.IP, devType, devID uint32) *hdhrproto.Packet {
	if d.cfg.LogRequests {
		log.Printf("req: discover from %v type=%08X id=%08X", remote, devType, devID)
	}
	if devType != hdhrproto.DeviceTypeWildcard && devType != hdhrproto.DeviceTypeTuner {
		return nil
	}
	if devID != hdhrproto.DeviceIDWildcard && devID != d.cfg.DeviceID {
		return nil
	}
	if d.cfg.Hidden && devID == hdhrproto.DeviceIDWildcard {
		return nil // not discoverable: only answer those who ask for us by ID
	}
	base := d.baseURL(localFor(remote))
	r := &hdhrproto.Packet{Type: hdhrproto.TypeDiscoverRpy}
	r.AddU32(hdhrproto.TagDeviceType, hdhrproto.DeviceTypeTuner)
	r.AddU32(hdhrproto.TagDeviceID, d.cfg.DeviceID)
	r.Add(hdhrproto.TagTunerCount, []byte{byte(len(d.tuners))})
	r.AddString(hdhrproto.TagBaseURL, base)
	r.AddString(hdhrproto.TagLineupURL, base+"/lineup.json")
	return r
}

const unknownVar = "ERROR: unknown getset variable"

func (d *Device) GetSet(remote net.IP, name string, value *string, lockkey *uint32) (string, error) {
	if d.cfg.LogRequests {
		if value != nil {
			log.Printf("req: control %v set %s %q", remote, name, *value)
		} else {
			log.Printf("req: control %v get %s", remote, name)
		}
	}
	n := strings.Trim(name, "/")
	if n == "help" && value == nil {
		return d.help(), nil
	}
	parts := strings.Split(n, "/")
	switch {
	case len(parts) == 2 && parts[0] == "sys":
		return d.sysVar(parts[1], value)
	case len(parts) == 2 && parts[0] == "lineup" && parts[1] == "scan":
		if value == nil {
			return d.lineupScan(), nil
		}
		switch *value {
		case "start":
			if err := d.StartScan(""); err != nil {
				return "", errors.New("ERROR: " + err.Error())
			}
		case "abort":
			d.AbortScan()
		default:
			return "", errors.New("ERROR: unknown scan command")
		}
		return "", nil
	case len(parts) == 2 && parts[0] == "lineup" && parts[1] == "location":
		if value == nil {
			return "disabled", nil
		}
		return *value, nil
	case len(parts) == 2 && parts[0] == "ir" && parts[1] == "target":
		if value == nil {
			return "none", nil
		}
		return *value, nil
	case len(parts) == 2 && strings.HasPrefix(parts[0], "tuner"):
		idx, err := strconv.Atoi(parts[0][5:])
		if err != nil || idx < 0 || idx >= len(d.tuners) {
			return "", errors.New(unknownVar)
		}
		if value == nil {
			return d.tuners[idx].get(parts[1])
		}
		return d.tuners[idx].set(remote, parts[1], *value, lockkey)
	}
	return "", errors.New(unknownVar)
}

func (d *Device) sysVar(name string, value *string) (string, error) {
	if value != nil {
		if name == "restart" || name == "debug" {
			return *value, nil
		}
		return "", errors.New("ERROR: invalid set value")
	}
	switch name {
	case "model":
		return d.cfg.FirmwareName, nil
	case "hwmodel":
		return d.cfg.ModelNumber, nil
	case "version":
		return d.cfg.FirmwareVersion, nil
	case "copyright":
		return "hdhr-mux-demux", nil
	case "features":
		return "channelmap: " + strings.Join(chanmap.Maps(), " ") +
			"\nmodulation: 8vsb qam256 qam64\nauto-modulation: auto", nil
	case "debug":
		return "", nil
	}
	return "", errors.New(unknownVar)
}

// help mirrors a real device's reply. It must stay short: libhdhomerun
// clients read control replies into a buffer of about 3 KB and report a
// communication error for anything larger, so tuners are not listed one by one.
func (d *Device) help() string {
	return "Supported configuration options:\n" +
		"/lineup/scan\n/sys/copyright\n/sys/debug\n/sys/features\n/sys/hwmodel\n/sys/model\n/sys/restart <resource>\n/sys/version\n" +
		"/tuner<n>/channel <modulation>:<freq|ch>\n/tuner<n>/channelmap <channelmap>\n/tuner<n>/debug\n" +
		"/tuner<n>/filter \"0x<nnnn>-0x<nnnn> [...]\"\n/tuner<n>/lockkey\n/tuner<n>/program <program number>\n" +
		"/tuner<n>/status\n/tuner<n>/plpinfo\n/tuner<n>/streaminfo\n/tuner<n>/target <ip>:<port>\n/tuner<n>/vchannel <vchannel>\n"
}

// ---- lineup -----------------------------------------------------------

// learn records where channels were seen on the air.
func (d *Device) learn(es []lineup.Entry) {
	d.scanMu.Lock()
	src := d.source
	d.scanMu.Unlock()
	for i := range es {
		if es[i].Source == "" {
			es[i].Source = src
		}
	}
	d.lineup.LearnRF(es, false)
}

// RefreshLineup re-reads the lineups of the physical devices, which are the
// channel list (names, codecs, HD/DRM flags). Where each channel is on the air
// is kept from what was learned before.
func (d *Device) RefreshLineup(ctx context.Context) error {
	devs := d.hub.Pool().Devices()
	entries, answered := hub.FetchLineups(ctx, devs)
	if answered == 0 || len(entries) == 0 {
		return errors.New("no device returned a lineup")
	}
	added, removed, err := d.lineup.Merge(entries)
	if added > 0 || removed > 0 {
		log.Printf("lineup: %d channel(s) from %d device(s) (%d added, %d removed)", len(entries), answered, added, removed)
	}
	return err
}

func (d *Device) lineupLoop() {
	defer d.wg.Done()
	t := time.NewTicker(d.cfg.LineupRefresh)
	defer t.Stop()
	for first := true; ; first = false {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := d.RefreshLineup(ctx); err != nil && first {
			log.Printf("lineup: %v", err)
		}
		cancel()
		select {
		case <-d.quit:
			return
		case <-t.C:
		}
	}
}

// ---- scanning ----------------------------------------------------------

// StartScan begins a background channel scan for source (default source if
// empty). It fails if one is already running.
func (d *Device) StartScan(source string) error {
	if source == "" {
		source = d.source
	}
	cmap, ok := d.cfg.Sources[source]
	if !ok {
		return fmt.Errorf("unknown source %q", source)
	}
	chans, err := chanmap.Channels(cmap)
	if err != nil {
		return err
	}
	var sel []chanmap.Channel
	for _, c := range chans {
		if (d.cfg.ScanFrom == 0 || c.Number >= d.cfg.ScanFrom) && (d.cfg.ScanTo == 0 || c.Number <= d.cfg.ScanTo) {
			sel = append(sel, c)
		}
	}
	d.scanMu.Lock()
	if d.scanning {
		d.scanMu.Unlock()
		return errors.New("scan already in progress")
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.scanning, d.scanCtx, d.source = true, cancel, source
	d.scanProg = hub.Progress{Total: len(sel)}
	d.scanMu.Unlock()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		log.Printf("scan: starting %s scan of %d channels", source, len(sel))
		found, err := d.hub.Scan(ctx, hub.ScanConfig{Channels: sel, Source: source, LockWait: d.cfg.ScanLockWait, InfoWait: d.cfg.ScanInfoWait, PerDevice: d.cfg.ScanTuners}, func(p hub.Progress) {
			d.scanMu.Lock()
			d.scanProg = p
			d.scanMu.Unlock()
		})
		end := "aborted"
		if err != nil && ctx.Err() == nil {
			log.Printf("scan: failed: %v", err)
		} else if ctx.Err() != nil {
			log.Printf("scan: aborted")
		} else {
			// A scan teaches where channels are (and adds any the devices do
			// not list); the channel list itself stays the devices'.
			n := d.lineup.LearnRF(found, true)
			if err := d.RefreshLineup(context.Background()); err != nil {
				log.Printf("scan: refreshing lineup: %v", err)
			}
			log.Printf("scan: finished, %d programs found, %d updated", len(found), n)
			end = "complete"
		}
		d.scanMu.Lock()
		d.scanning, d.scanCtx = false, nil
		d.scanEnd, d.scanFound = end, len(found)
		d.scanMu.Unlock()
	}()
	return nil
}

func (d *Device) AbortScan() {
	d.scanMu.Lock()
	defer d.scanMu.Unlock()
	if d.scanCtx != nil {
		d.scanCtx()
	}
}

// lineupScan renders the /lineup/scan variable the way a real device does:
// "state=complete progress=100% found=161".
func (d *Device) lineupScan() string {
	d.scanMu.Lock()
	defer d.scanMu.Unlock()
	switch {
	case d.scanning:
		pct := 0
		if d.scanProg.Total > 0 {
			pct = d.scanProg.Done * 100 / d.scanProg.Total
		}
		return fmt.Sprintf("state=running progress=%d%% found=%d", pct, d.scanProg.Found)
	case d.scanEnd == "aborted":
		return "state=aborted progress=100% found=0"
	case d.scanEnd != "":
		return fmt.Sprintf("state=complete progress=100%% found=%d", d.scanFound)
	}
	if n := len(d.lineup.Entries(true)); n > 0 {
		return fmt.Sprintf("state=complete progress=100%% found=%d", n)
	}
	return "state=idle progress=0% found=0"
}

func (d *Device) scanState() (bool, hub.Progress, string) {
	d.scanMu.Lock()
	defer d.scanMu.Unlock()
	return d.scanning, d.scanProg, d.source
}
