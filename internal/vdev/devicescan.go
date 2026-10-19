package vdev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/hub"
	"hdhrmux/internal/upstream"
)

// A rescan (what Channels DVR asks for once a day) is handed to one device's own
// firmware scan, and the multiplexer reads the result back from the device's
// lineup. The firmware scan takes every tuner of the device it runs on and cuts
// whatever they are streaming, so it is only given to a device nobody is using:
// no viewers of ours, nobody else's streams. A device with ATSC 3.0 tuners is
// preferred, because it finds both kinds of channel. Its tuners are marked as
// scanning for the duration, so no stream is started on them.

var errNoQuietDevice = errors.New("no device is free to scan with: every one has viewers or another user")

// quietDevices lists the devices that can scan now, best first: ATSC 3.0
// capable ones, then by ID. Tuners held only by a lingering stream count as free.
func (d *Device) quietDevices() []*upstream.Device {
	pool := d.hub.Pool()
	lingering := d.hub.IdleTuners()
	var out []*upstream.Device
	for _, dev := range pool.Devices() {
		quiet := true
		for idx := 0; idx < dev.TunerCount; idx++ {
			g := pool.GlobalOf(dev.ID, idx)
			if g < 0 {
				continue // not one of ours to use
			}
			if pool.HeldByScan(g) || (pool.HeldByUs(dev.ID, idx) && !lingering[g]) {
				quiet = false
				break
			}
			if !pool.HeldByUs(dev.ID, idx) {
				if raw, err := pool.RawStatus(dev.ID, idx); err != nil || !strings.HasPrefix(raw, "ch=none") {
					quiet = false // somebody else is using it
					break
				}
			}
		}
		if quiet {
			out = append(out, dev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a3, b3 := strings.Contains(out[i].Model, "atsc3"), strings.Contains(out[j].Model, "atsc3")
		if a3 != b3 {
			return a3
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func webURL(dev *upstream.Device, path string) string {
	return "http://" + net.JoinHostPort(dev.IP.String(), strconv.Itoa(dev.WebPort)) + path
}

// startDeviceScan hands the scan to a device. It returns once the device has
// accepted it; progress is followed in the background.
func (d *Device) startDeviceScan(source string) error {
	if source == "" {
		source = d.source
	}
	d.scanMu.Lock()
	if d.scanning {
		d.scanMu.Unlock()
		return errors.New("scan already in progress")
	}
	d.scanning = true // claimed; the rest may fail and undo it
	d.scanMu.Unlock()
	fail := func(err error) error {
		d.scanMu.Lock()
		d.scanning = false
		d.scanMu.Unlock()
		return err
	}

	candidates := d.quietDevices()
	if len(candidates) == 0 {
		return fail(errNoQuietDevice)
	}
	var dev *upstream.Device
	var release func()
	for _, c := range candidates {
		d.hub.ReleaseLingeringOn(c.ID) // nobody is watching: let go of tuners held for a quick return
		if r, err := d.hub.Pool().HoldDevice(c.ID); err == nil {
			dev, release = c, r
			break
		}
	}
	if dev == nil {
		return fail(errNoQuietDevice)
	}

	hc := &http.Client{Timeout: 15 * time.Second}
	post := func(q string) error {
		resp, err := hc.Post(webURL(dev, "/lineup.post?"+q), "", nil)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("%s answered %s", dev.Addr(), resp.Status)
		}
		return nil
	}
	if err := post("scan=start&source=" + url.QueryEscape(source)); err != nil {
		release()
		return fail(fmt.Errorf("starting the scan on %s: %w", hdhrproto.FormatDeviceID(dev.ID), err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.scanMu.Lock()
	d.scanCtx, d.source = cancel, source
	d.scanProg = hub.Progress{Total: 100}
	d.scanMu.Unlock()
	log.Printf("scan: handed to device %s (%s)", hdhrproto.FormatDeviceID(dev.ID), dev.Model)

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer release()
		end, found := d.followDeviceScan(ctx, hc, dev, post)
		if end == "complete" {
			if err := d.RefreshLineup(context.Background()); err != nil {
				log.Printf("scan: refreshing lineup: %v", err)
			}
		}
		log.Printf("scan: device %s %s (%d found)", hdhrproto.FormatDeviceID(dev.ID), end, found)
		d.scanMu.Lock()
		d.scanning, d.scanCtx = false, nil
		d.scanEnd, d.scanFound = end, found
		d.scanMu.Unlock()
	}()
	return nil
}

// followDeviceScan polls the device until its scan ends, mirroring progress.
func (d *Device) followDeviceScan(ctx context.Context, hc *http.Client, dev *upstream.Device, post func(string) error) (end string, found int) {
	deadline := time.After(20 * time.Minute)
	tick := time.NewTicker(d.cfg.ScanPoll())
	defer tick.Stop()
	started := time.Now()
	for {
		select {
		case <-ctx.Done():
			post("scan=abort")
			return "aborted", found
		case <-d.quit:
			post("scan=abort")
			return "aborted", found
		case <-deadline:
			post("scan=abort")
			log.Printf("scan: the device took too long; aborted")
			return "aborted", found
		case <-tick.C:
		}
		resp, err := hc.Get(webURL(dev, "/lineup_status.json"))
		if err != nil {
			if time.Since(started) > time.Minute {
				log.Printf("scan: lost the device: %v", err)
				return "aborted", found
			}
			continue
		}
		var st struct {
			ScanInProgress int
			Progress       int
			Found          int
		}
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if err != nil {
			continue
		}
		found = st.Found
		if st.ScanInProgress == 0 {
			return "complete", found
		}
		d.scanMu.Lock()
		d.scanProg = hub.Progress{Total: 100, Done: st.Progress, Found: st.Found}
		d.scanMu.Unlock()
	}
}
