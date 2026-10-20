// Command hdhr-mux presents a virtual HDHomeRun that multiplexes several
// physical HDHomeRun devices and shares each physical tuner between all
// clients watching channels on the same RF channel.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"hdhrmux/internal/config"
	"hdhrmux/internal/guide"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/upstream"
	"hdhrmux/internal/vdev"
)

// version is set at build time: -ldflags "-X main.version=0.99.0".
var version = "dev"

// exitRestart is the status run returns after "Restart" on the settings page. main starts the
// program again in place; if that is impossible it is also the exit status: not 0, so
// that supervisors that only restart on failure (Docker on-failure, systemd
// Restart=on-failure) bring the multiplexer back too.
const exitRestart = 3

func main() {
	code := run()
	if code == exitRestart {
		// "Restart" on the settings page: start again in place, so that it works
		// without a supervisor (a container run without --restart). Everything has
		// been stopped and flushed by now. If that fails, exiting with a status that
		// is not 0 lets a supervisor do it.
		log.Printf("restarting in place")
		if err := execSelf(); err != nil {
			log.Printf("cannot restart in place (%v): exiting with status %d for a supervisor to restart", err, exitRestart)
		}
	}
	os.Exit(code)
}

func run() int {
	cfgPath := flag.String("config", "", "path to a JSON config file (defaults to /etc/hdhr-mux/config.json if that exists; defaults are used without one)")
	scan := flag.Bool("scan", false, "scan for channels at startup even if a lineup exists")
	ignoreSettings := flag.Bool("ignore-settings", false, "ignore settings.json (saved from the web interface) in the state directory: the way out if a saved setting stops start-up")
	showVersion := flag.Bool("version", false, "print the version and exit")
	httpAddr := flag.String("http", "", "web interface address, for example :8080 or 127.0.0.1:8080 (overrides the http_listen setting; without either it is :80, or :8080 when :80 is not allowed or in use)")
	flag.Parse()
	*cfgPath = pickConfig(*cfgPath, func(p string) bool { _, err := os.Stat(p); return err == nil })
	if *showVersion {
		fmt.Println("hdhr-mux", version)
		return 0
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.Printf("hdhr-mux %s", version)

	load := config.Load
	if *ignoreSettings {
		load = config.LoadWithoutSettings
		log.Printf("ignoring settings.json")
	}
	// the same way at start and whenever the settings page applies changes
	loadCfg := func() (*config.Config, error) {
		c, err := load(*cfgPath)
		if err == nil {
			applyOverrides(c, *httpAddr)
		}
		return c, err
	}
	cfg, err := loadCfg()
	if err != nil {
		log.Fatal(err)
	}
	id, err := deviceID(cfg)
	if err != nil {
		log.Fatal(err)
	}

	if cfg.PprofListen != "" {
		go func() { log.Printf("pprof: %v", http.ListenAndServe(cfg.PprofListen, nil)) }()
	}

	pool, err := upstream.NewPool(upstream.Config{
		Discover:        *cfg.Upstream.Discover,
		DiscoverTargets: cfg.Upstream.DiscoverTarget,
		Devices:         cfg.Upstream.Devices,
		AllowIDs:        cfg.Upstream.Allow,
		Tuners:          cfg.Upstream.Tuners,
		ExcludeTuners:   cfg.Upstream.Exclude,
		IgnoreIDs:       []uint32{id},
		Mode:            cfg.Upstream.Mode,
		StreamPort:      cfg.Upstream.StreamPort,
		OrderFile:       filepath.Join(cfg.StateDir, "devices.json"),
		LocalIP:         cfg.Upstream.LocalIP,
		WaitFree:        time.Duration(cfg.Upstream.WaitFree),
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var restart atomic.Bool

	tuners := cfg.VirtualTuners
	for i := 0; i < 5; i++ {
		pool.Refresh()
		if pool.TotalTuners() > 0 || tuners > 0 {
			break
		}
		log.Printf("no physical HDHomeRun found yet, retrying...")
	}
	if tuners == 0 {
		tuners = pool.TotalTuners()
	}
	setup := ""
	if tuners == 0 {
		// Nothing to serve, but the web page is how somebody fixes that: run without
		// advertising, and start over when a device shows up.
		tuners = 1
		setup = "No HDHomeRun devices were found on this network. Check that they are on the same network, or add their addresses under Devices on the Settings page and restart. The multiplexer keeps looking and restarts itself when it finds one."
		log.Print(setup)
	}
	go pool.Run(ctx)
	if setup != "" {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				if pool.TotalTuners() > 0 {
					log.Printf("a device was found: restarting")
					restart.Store(true)
					stop()
					return
				}
			}
		}()
	}

	lu, err := lineup.Open(filepath.Join(cfg.StateDir, "lineup.json"))
	if err != nil {
		log.Fatal(err)
	}
	h := hub.New(pool, hub.Config{Linger: time.Duration(cfg.Linger), ClientBuffer: cfg.ClientBufferMB << 20, Mask: *cfg.Upstream.Mask})
	defer h.Close()
	h.SetSignalSettle(time.Duration(cfg.SignalSettle))

	guideRefresh, refresh := time.Duration(cfg.Guide.Refresh), time.Duration(cfg.Lineup.Refresh)
	settings := config.NewManager(*cfgPath, cfg)
	settings.SetLoader(loadCfg)
	if *httpAddr != "" {
		settings.SetOverride("http_listen", "the -http option")
	}
	logConfigSource(*cfgPath, cfg, settings)
	dev, err := vdev.New(vdev.Config{
		DeviceID:               id,
		FriendlyName:           cfg.FriendlyName,
		ModelNumber:            cfg.ModelNumber,
		Manufacturer:           cfg.Manufacturer,
		Tuners:                 tuners,
		ChannelMap:             cfg.Lineup.ChannelMap,
		Sources:                cfg.Lineup.Sources,
		DefaultSource:          cfg.Lineup.DefaultSource,
		ScanFrom:               cfg.Lineup.ScanFrom,
		ScanTo:                 cfg.Lineup.ScanTo,
		ScanTuners:             cfg.Lineup.ScanTuners,
		ScanMode:               cfg.Lineup.ScanMode,
		AutoJoinDirect:         *cfg.AutoJoinDirect,
		AutoJoinOtherLocations: *cfg.AutoJoinOtherLocations,
		EvictScans:             *cfg.EvictScans,
		HideATSC3:              !*cfg.Lineup.ATSC3,
		Setup:                  setup,
		StreamTTL:              *cfg.StreamTTL,
		LineupRefresh:          refresh,
		GuideRefresh:           guideRefresh,
		GuideHold:              time.Duration(cfg.Guide.Hold),
		GuideParallel:          cfg.Guide.Parallel,
		GuideUnlocked:          cfg.Guide.Unlocked,
		GuideStartDelay:        time.Duration(cfg.Guide.StartDelay),
		HTTPAddr:               cfg.HTTPListen,
		HTTPFallback:           httpFallback(cfg),
		ControlAddr:            cfg.ControlListen,
		StreamAddr:             cfg.StreamListen,
		AdvertiseHost:          cfg.AdvertiseHost,
		SSDP:                   *cfg.SSDP && !cfg.Loopback(), // multicast does not reach anybody over loopback
		NoSSDP4:                !*cfg.SSDPIPv4,
		NoSSDP6:                !*cfg.SSDPIPv6,
		NoATSC3SSDP:            !*cfg.SSDPATSC3,
		Hidden:                 !*cfg.Discoverable,
		LogRequests:            cfg.LogRequests,
		IdleRelease:            time.Duration(cfg.IdleRelease),
		StateDir:               cfg.StateDir,
		Settings:               settings,
		SettingsLocked:         cfg.SettingsLocked,
		Restart:                func() { restart.Store(true); stop() },
		Version:                version,
	}, h, lu)
	if err != nil {
		log.Fatal(err)
	}
	gs, err := guide.Open(filepath.Join(cfg.StateDir, "epg.json"))
	if err != nil {
		log.Fatal(err)
	}
	defer gs.Flush()
	dev.SetGuide(gs)
	if err := dev.Start(); err != nil {
		log.Fatal(err)
	}
	defer dev.Close()
	log.Printf("virtual HDHomeRun %s up: %d tuner(s), %d channel(s), http :%d, control :%d",
		hdhrproto.FormatDeviceID(id), tuners, len(lu.Entries(true)), dev.HTTPPort(), dev.ControlPort())

	if *scan || cfg.Lineup.ScanOnStart {
		if err := dev.StartScan(""); err != nil {
			log.Printf("scan: %v", err)
		}
	}
	<-ctx.Done()
	log.Printf("shutting down")
	if restart.Load() {
		return exitRestart
	}
	return 0
}

// deviceID returns the configured device ID, or a persisted random one.
func deviceID(cfg *config.Config) (uint32, error) {
	if s := strings.ToUpper(cfg.DeviceID); s != "" && s != "AUTO" {
		id, err := hdhrproto.ParseDeviceID(s)
		if err != nil {
			return 0, err
		}
		if !hdhrproto.ValidDeviceID(id) {
			return 0, fmt.Errorf("device_id %s has an invalid check digit; use %08X", s, hdhrproto.MakeDeviceID(id))
		}
		return id, nil
	}
	path := filepath.Join(cfg.StateDir, "device_id")
	if b, err := os.ReadFile(path); err == nil {
		if id, err := hdhrproto.ParseDeviceID(strings.TrimSpace(string(b))); err == nil && hdhrproto.ValidDeviceID(id) {
			return id, nil
		}
	}
	var r [4]byte
	rand.Read(r[:])
	id := hdhrproto.MakeDeviceID(binary.BigEndian.Uint32(r[:])&0x0FFFFFF0 | 0x10000000)
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return 0, err
	}
	if err := os.WriteFile(path, []byte(hdhrproto.FormatDeviceID(id)+"\n"), 0o644); err != nil {
		return 0, err
	}
	return id, nil
}

// defaultConfig is read when -config is not given and the file exists: the
// packages install no config file, so it is only there if somebody made one.
const defaultConfig = "/etc/hdhr-mux/config.json"

func pickConfig(flagValue string, exists func(string) bool) string {
	if flagValue == "" && exists(defaultConfig) {
		return defaultConfig
	}
	return flagValue
}

// applyOverrides puts the command-line options over what the files and the
// settings page say.
func applyOverrides(c *config.Config, httpAddr string) {
	if httpAddr != "" {
		c.HTTPListen, c.HTTPListenDefault = httpAddr, false
	}
}

// httpFallback is the port the web interface moves to if its default (80) cannot be
// used. Nothing is tried when the port was chosen on purpose.
func httpFallback(c *config.Config) string {
	if !c.HTTPListenDefault {
		return ""
	}
	if c.Bind != "" {
		return net.JoinHostPort(c.Bind, "8080")
	}
	return ":8080"
}

// logConfigSource says where the running configuration came from.
func logConfigSource(path string, cfg *config.Config, m *config.Manager) {
	src := "no config file, built-in defaults"
	if path != "" {
		src = path
	}
	var by []string
	for k, v := range m.OverriddenByEnv() {
		by = append(by, k+" by "+v)
	}
	sort.Strings(by)
	extra := ""
	if len(by) > 0 {
		extra = "; overridden: " + strings.Join(by, ", ")
	}
	log.Printf("config: %s; %d setting(s) saved from the web page (%s); state in %s%s",
		src, len(m.Saved()), m.FilePath(), cfg.StateDir, extra)
}
