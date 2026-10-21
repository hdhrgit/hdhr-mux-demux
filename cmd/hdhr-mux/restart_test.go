//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"hdhrmux/internal/fakehdhr"
	"hdhrmux/internal/hdhrproto"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func fds(pid int) int {
	ents, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	return len(ents)
}

// "Restart" on the settings page starts the program again in the same process
// (so it works without a supervisor), and leaks no file descriptors doing so.
func TestRestartInPlace(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the program")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "hdhr-mux")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Skipf("cannot build: %v\n%s", err, out)
	}
	fake, err := fakehdhr.Start(fakehdhr.Config{DeviceID: hdhrproto.MakeDeviceID(0x10200000), Tuners: 2, Channels: []fakehdhr.Channel{{Frequency: 177000000, TSID: 1, Programs: []fakehdhr.Program{{Number: 1, VChannel: "7.1", Name: "ALPHA"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	httpPort := freePort(t)
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"state_dir":%q,"http_listen":"127.0.0.1:%d","control_listen":"127.0.0.1:0","stream_listen":"-","ssdp":false,
  "upstream":{"discover":false,"devices":[%q]},"guide":{"refresh":"0"},"lineup":{"refresh":"0"}}`,
		filepath.Join(dir, "state"), httpPort, fake.Addr())), 0o644)

	cmd := exec.Command(bin, "-config", cfg)
	var out syncBuf
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	up := func(what string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if r, err := http.Get(base + "/discover.json"); err == nil {
				r.Body.Close()
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("not up %s:\n%s", what, out.String())
	}
	up("at start")
	time.Sleep(500 * time.Millisecond)
	pid, before := cmd.Process.Pid, fds(cmd.Process.Pid)
	if before == 0 {
		t.Skip("cannot read /proc")
	}

	for i := 1; i <= 3; i++ {
		req, _ := http.NewRequest("POST", base+"/ui/api/restart", nil)
		req.Header.Set("X-Requested-With", "hdhr-mux-ui")
		if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
			t.Fatalf("restart %d: %v %v", i, err, r)
		}
		deadline := time.Now().Add(20 * time.Second)
		for strings.Count(out.String(), "restarting in place") < i && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(time.Second)
		up(fmt.Sprintf("after restart %d", i))
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("process %d is gone after restart %d: %v\n%s", pid, i, err, out.String())
		}
		time.Sleep(500 * time.Millisecond)
		if n := fds(pid); n != before {
			t.Fatalf("restart %d: %d open descriptors, %d before", i, n, before)
		}
	}
	if n := strings.Count(out.String(), "cannot restart in place"); n != 0 {
		t.Errorf("the exec failed %d time(s):\n%s", n, out.String())
	}
	if n := strings.Count(out.String(), "hdhr-mux "); n < 4 {
		t.Errorf("expected the program to start 4 times (once + 3 restarts), saw %d\n%s", n, out.String())
	}
}

// syncBuf collects the program's output; it is read while still being written.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Run by hand: no service, no config file, the web port from -http, state in the
// working directory. The log says where the configuration came from and the
// settings page shows the port as set by the command line.
func TestRunByHandWithHTTPOption(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the program")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "hdhr-mux")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Skipf("cannot build: %v\n%s", err, out)
	}
	fake, err := fakehdhr.Start(fakehdhr.Config{DeviceID: hdhrproto.MakeDeviceID(0x10200000), Tuners: 2, Channels: []fakehdhr.Channel{{Frequency: 177000000, TSID: 1, Programs: []fakehdhr.Program{{Number: 1, VChannel: "7.1", Name: "ALPHA"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	port := freePort(t)
	cfg := filepath.Join(dir, "c.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"control_listen":"127.0.0.1:0","stream_listen":"-","ssdp":false,"guide":{"refresh":"0"},"lineup":{"refresh":"0"}}`)), 0o644)

	cmd := exec.Command(bin, "-config", cfg, "-http", fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HDHOMERUN_DISABLE_DISCOVERY=true", "HDHOMERUN_DEVICES="+fake.Addr())
	var out syncBuf
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if r, err := http.Get(base + "/discover.json"); err == nil {
			r.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not serving on the -http port:\n%s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s := out.String(); !strings.Contains(s, "config: "+cfg) || !strings.Contains(s, "http_listen by the -http option") {
		t.Errorf("the log does not say where the configuration came from:\n%s", s)
	}
	req, _ := http.NewRequest("GET", base+"/ui/api/settings", nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var st struct{ Env map[string]string }
	json.NewDecoder(r.Body).Decode(&st)
	r.Body.Close()
	if st.Env["http_listen"] != "the -http option" {
		t.Errorf("settings page does not show the -http override: %v", st.Env)
	}
}

// With no device to be found the program still runs, so that the web page can be
// used to say where they are.
func TestStaysUpWithoutDevices(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the program")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "hdhr-mux")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Skipf("cannot build: %v\n%s", err, out)
	}
	port := freePort(t)
	cfg := filepath.Join(dir, "c.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"control_listen":"127.0.0.1:0","stream_listen":"-","ssdp":false,"upstream":{"discover":false,"devices":["127.0.0.1:1"]}}`)), 0o644)
	cmd := exec.Command(bin, "-config", cfg, "-http", fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Dir = dir
	var out syncBuf
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	deadline := time.Now().Add(25 * time.Second)
	for {
		r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/ui/api/activity", port))
		if err == nil {
			var a struct{ Setup string }
			json.NewDecoder(r.Body).Decode(&a)
			r.Body.Close()
			if strings.Contains(a.Setup, "No HDHomeRun devices were found") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no setup message on the activity page:\n%s", out.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}
