package upstream_test

import (
	"os"
	"path/filepath"
	"testing"

	"hdhrmux/internal/fakehdhr"
	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/upstream"
)

func fake(t *testing.T, id uint32, tuners int) *fakehdhr.Device {
	t.Helper()
	d, err := fakehdhr.Start(fakehdhr.Config{DeviceID: hdhrproto.MakeDeviceID(id), Tuners: tuners})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

// Tuner numbers are global, device order is by ID the first time and is then
// remembered, so a missing or late device never renumbers the others.
func TestStableGlobalNumbering(t *testing.T) {
	a, b := fake(t, 0x10100000, 4), fake(t, 0x10200000, 2)
	order := filepath.Join(t.TempDir(), "devices.json")

	// "b" is listed first on purpose; numbering must still follow device IDs.
	p, err := upstream.NewPool(upstream.Config{Devices: []string{b.Addr(), a.Addr()}, OrderFile: order})
	if err != nil {
		t.Fatal(err)
	}
	p.Refresh()
	if got := p.TotalTuners(); got != 6 {
		t.Fatalf("total %d", got)
	}
	ref, _ := p.Resolve(5)
	if ref.DeviceID != b.ID() || ref.Index != 1 {
		t.Fatalf("tuner 5 = %08X/%d", ref.DeviceID, ref.Index)
	}
	ref, _ = p.Resolve(0)
	if ref.DeviceID != a.ID() || ref.Index != 0 {
		t.Fatalf("tuner 0 = %08X/%d", ref.DeviceID, ref.Index)
	}
	if _, err := os.Stat(order); err != nil {
		t.Fatal("order not persisted:", err)
	}

	// restart with device "a" offline: its tuners keep their numbers
	p2, err := upstream.NewPool(upstream.Config{Devices: []string{b.Addr()}, OrderFile: order})
	if err != nil {
		t.Fatal(err)
	}
	p2.Refresh()
	if got := p2.TotalTuners(); got != 6 {
		t.Fatalf("total with a device offline: %d", got)
	}
	ref, _ = p2.Resolve(4)
	if ref.DeviceID != b.ID() || ref.Index != 0 || ref.Device == nil {
		t.Fatalf("tuner 4 = %+v", ref)
	}
	ref, _ = p2.Resolve(2)
	if ref.DeviceID != a.ID() || ref.Device != nil {
		t.Fatalf("offline tuner 2 = %+v", ref)
	}

	// a brand new device goes after the existing ones
	c := fake(t, 0x10050000, 1) // sorts before both, but is new
	p3, _ := upstream.NewPool(upstream.Config{Devices: []string{a.Addr(), b.Addr(), c.Addr()}, OrderFile: order})
	p3.Refresh()
	if ref, _ := p3.Resolve(6); ref.DeviceID != c.ID() {
		t.Fatalf("new device should take the next number, tuner 6 = %08X", ref.DeviceID)
	}
	if ref, _ := p3.Resolve(0); ref.DeviceID != a.ID() {
		t.Fatalf("existing numbering changed")
	}
}

// Another multiplexer on the network must not be mistaken for a physical
// device (it would be used as a source of itself).
func TestIgnoresOtherMultiplexers(t *testing.T) {
	real := fake(t, 0x10100000, 2)
	mux, err := fakehdhr.Start(fakehdhr.Config{DeviceID: hdhrproto.MakeDeviceID(0x10300000), Tuners: 8, Model: "hdhomerun_mux"})
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	p, _ := upstream.NewPool(upstream.Config{Devices: []string{real.Addr(), mux.Addr()}})
	p.Refresh()
	if got := p.TotalTuners(); got != 2 {
		t.Fatalf("%d tuners, want only the 2 of the real device", got)
	}
}
