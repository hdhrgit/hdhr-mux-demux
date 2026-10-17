// Command hdhrctl is a minimal hdhomerun_config work-alike for poking at a
// physical device or the multiplexer:
//
//	hdhrctl discover
//	hdhrctl HOST[:PORT] get /tuner0/status
//	hdhrctl HOST[:PORT] set /tuner0/channel auto:177000000
//	hdhrctl HOST[:PORT] save N SECONDS [rtp|udp]   # TS to stdout
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"hdhrmux/internal/hdhrproto"
)

func main() {
	a := os.Args[1:]
	if len(a) == 1 && a[0] == "discover" {
		infos, _ := hdhrproto.Discover([]string{"255.255.255.255:65001"}, 2*time.Second)
		for _, i := range infos {
			fmt.Printf("hdhomerun device %08X found at %v (%d tuners) %s\n", i.DeviceID, i.IP, i.TunerCount, i.BaseURL)
		}
		return
	}
	if len(a) < 3 {
		fmt.Fprintln(os.Stderr, "usage: hdhrctl discover | HOST[:PORT] get VAR | set VAR VALUE | save TUNER SECONDS [rtp|udp]")
		os.Exit(2)
	}
	host := a[0]
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "65001")
	}
	c := hdhrproto.NewControl(host)
	defer c.Close()
	switch a[1] {
	case "get":
		v, err := c.Get(a[2])
		out(v, err)
	case "set":
		if len(a) < 4 {
			os.Exit(2)
		}
		var key uint32
		if k := os.Getenv("HDHR_LOCKKEY"); k != "" {
			n, _ := strconv.ParseUint(k, 10, 32)
			key = uint32(n)
		}
		_, err := c.Set(a[2], a[3], key)
		out("", err)
	case "save":
		if len(a) < 4 {
			os.Exit(2)
		}
		save(c, host, a[2], a[3], a[4:])
	}
}

func out(v string, err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if v != "" {
		fmt.Println(v)
	}
}

func save(c *hdhrproto.Control, host, tuner, secs string, rest []string) {
	mode := "rtp"
	if len(rest) > 0 {
		mode = rest[0]
	}
	n, _ := strconv.Atoi(secs)
	hostAddr, _ := net.ResolveUDPAddr("udp4", host)
	probe, _ := net.DialUDP("udp4", nil, hostAddr)
	local := probe.LocalAddr().(*net.UDPAddr).IP
	probe.Close()
	rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: local})
	if err != nil {
		out("", err)
	}
	defer rx.Close()
	rx.SetReadBuffer(4 << 20)
	t := "/tuner" + tuner
	_, err = c.Set(t+"/target", mode+"://"+rx.LocalAddr().String(), 0)
	out("", err)
	defer c.Set(t+"/target", "none", 0)
	end := time.Now().Add(time.Duration(n) * time.Second)
	buf := make([]byte, 4096)
	for time.Now().Before(end) {
		rx.SetReadDeadline(end)
		k, err := rx.Read(buf)
		if err != nil {
			break
		}
		b := buf[:k]
		if mode == "rtp" {
			if k < 12 {
				continue
			}
			b = b[12+int(b[0]&0xF)*4:]
		}
		os.Stdout.Write(b)
	}
}
