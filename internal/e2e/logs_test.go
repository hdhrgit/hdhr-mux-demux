package e2e

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/logbuf"
	"hdhrmux/internal/vdev"
)

// "Log every request" can be limited to what arrives on some of the ports.
func TestRequestLogCanBeLimitedByPort(t *testing.T) {
	var buf lockedBuf
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	e := setup(t, []int{1}, 1, func(c *vdev.Config) { c.LogRequests, c.LogRequestsOn = true, "web, tcp" })
	getJSON(t, e.api+"/discover.json", &map[string]any{})     // web
	hdhrproto.Discover([]string{e.ctl}, 300*time.Millisecond) // udp
	ctl := hdhrproto.NewControl(e.ctl)                        // tcp
	ctl.Get("/sys/model")
	ctl.Close()
	if r, err := http.Get(e.http + "/auto/v7.1"); err == nil { // stream port
		r.Body.Close()
	}
	out := buf.String()
	for _, want := range []string{"req: http ", "/discover.json", "req: control "} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	if strings.Contains(out, "req: discover") {
		t.Error("a udp request was logged although only web and tcp were asked for")
	}
	if strings.Contains(out, "/auto/v7.1") {
		t.Error("a stream-port request was logged although only web and tcp were asked for")
	}
}

// The Logs page: the log as JSON, and followed live as server-sent events that a browser
// can resume where it left off.
func TestLogsFollowedLive(t *testing.T) {
	logs := logbuf.New(100)
	e := setup(t, []int{1}, 1, func(c *vdev.Config) { c.Logs = logs })
	fmt.Fprintln(logs, "hub: first")
	fmt.Fprintln(logs, "guide: second")

	var j struct {
		Lines []struct {
			ID   uint64
			Text string
		}
	}
	getJSON(t, e.api+"/ui/api/logs?n=10", &j)
	if len(j.Lines) != 2 || j.Lines[1].Text != "guide: second" || j.Lines[1].ID != 2 {
		t.Fatalf("lines %+v", j.Lines)
	}

	read := func(lastID string, want int) []string {
		req, _ := http.NewRequest("GET", e.api+"/ui/api/logs/stream", nil)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil || resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("stream: %v %v", resp, err)
		}
		defer resp.Body.Close()
		var got []string
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() && len(got) < want {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				got = append(got, d)
			}
		}
		return got
	}
	if got := read("", 2); len(got) != 2 || got[0] != "hub: first" {
		t.Fatalf("a fresh browser gets the newest lines: %v", got)
	}
	go func() { time.Sleep(300 * time.Millisecond); fmt.Fprintln(logs, "req: third") }()
	if got := read("2", 1); len(got) != 1 || got[0] != "req: third" {
		t.Fatalf("a browser that reconnects gets what it missed, as it happens: %v", got)
	}

	e2 := setup(t, []int{1}, 1, nil)
	if r, err := http.Get(e2.api + "/ui/api/logs"); err != nil || r.StatusCode != 404 {
		t.Errorf("without a log buffer there is no logs page: %v %v", r, err)
	}
}
