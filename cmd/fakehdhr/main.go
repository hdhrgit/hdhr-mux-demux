// Command fakehdhr runs a simulated physical HDHomeRun on 127.0.0.1 so the
// multiplexer can be tried without hardware.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"hdhrmux/internal/fakehdhr"
)

func main() {
	tuners := flag.Int("tuners", 2, "number of tuners")
	flag.Parse()
	d, err := fakehdhr.Start(fakehdhr.Config{
		Tuners: *tuners,
		Channels: []fakehdhr.Channel{
			{Frequency: 177000000, TSID: 1, Programs: []fakehdhr.Program{{Number: 1, VChannel: "7.1", Name: "FAKE-A"}, {Number: 2, VChannel: "7.2", Name: "FAKE-B"}}},
			{Frequency: 473000000, TSID: 2, Programs: []fakehdhr.Program{{Number: 1, VChannel: "14.1", Name: "FAKE-C"}, {Number: 3, VChannel: "14.3", Name: "FAKE-D"}}},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer d.Close()
	fmt.Printf("fake HDHomeRun %08X listening on %s (tcp+udp)\n", d.ID(), d.Addr())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
}
