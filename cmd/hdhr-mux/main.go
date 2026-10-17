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
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"hdhrmux/internal/config"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/lineup"
	"hdhrmux/internal/upstream"
	"hdhrmux/internal/vdev"
)

func main() {
	cfgPath := flag.String("config", "", "path to JSON config file (defaults are used if omitted)")
	scan := flag.Bool("scan", false, "scan for channels at startup even if a lineup exists")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	cfg, err := config.Load(*cfgPath)
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
	if tuners == 0 {
		log.Fatal("no physical HDHomeRun tuners found; check the network or set upstream.devices / virtual_tuners")
	}
	go pool.Run(ctx)

	lu, err := lineup.Open(filepath.Join(cfg.StateDir, "lineup.json"))
	if err != nil {
		log.Fatal(err)
	}
	linger := time.Duration(cfg.Linger)
	if linger < 0 {
		linger = 0
	}
	h := hub.New(pool, hub.Config{Linger: linger, ClientBuffer: cfg.ClientBufferMB << 20})
	defer h.Close()

	refresh := time.Duration(cfg.Lineup.Refresh)
	if refresh == 0 {
		refresh = 10 * time.Minute
	}
	if refresh < 0 {
		refresh = 0
	}
	dev, err := vdev.New(vdev.Config{
		DeviceID:      id,
		FriendlyName:  cfg.FriendlyName,
		ModelNumber:   cfg.ModelNumber,
		Manufacturer:  cfg.Manufacturer,
		Tuners:        tuners,
		ChannelMap:    cfg.Lineup.ChannelMap,
		Sources:       cfg.Lineup.Sources,
		DefaultSource: cfg.Lineup.DefaultSource,
		ScanFrom:      cfg.Lineup.ScanFrom,
		ScanTo:        cfg.Lineup.ScanTo,
		ScanTuners:    cfg.Lineup.ScanTuners,
		LineupRefresh: refresh,
		HTTPAddr:      cfg.HTTPListen,
		ControlAddr:   cfg.ControlListen,
		StreamAddr:    cfg.StreamListen,
		AdvertiseHost: cfg.AdvertiseHost,
		SSDP:          *cfg.SSDP,
		Hidden:        !*cfg.Discoverable,
		LogRequests:   cfg.LogRequests,
		IdleRelease:   time.Duration(cfg.IdleRelease),
	}, h, lu)
	if err != nil {
		log.Fatal(err)
	}
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
