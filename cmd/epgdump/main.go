// Command epgdump reads an MPEG transport stream of a whole ATSC 1.0
// multiplex on stdin and prints the programme guide found in its PSIP tables.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"hdhrmux/internal/psip"
	"hdhrmux/internal/ts"
)

func main() {
	r := bufio.NewReaderSize(os.Stdin, 1<<20)
	c := psip.NewCollector()
	pkt := make([]byte, ts.PacketSize)
	for {
		if _, err := io.ReadFull(r, pkt); err != nil {
			break
		}
		if pkt[0] != ts.SyncByte {
			continue
		}
		c.Push(ts.PID(pkt), pkt)
	}
	g := c.Snapshot()
	var names []string
	for n := range g.Channels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ch := g.Channels[n]
		fmt.Printf("== %s %s (source %d, program %d): %d event(s)\n", n, ch.Name, ch.SourceID, ch.Program, len(g.Events[n]))
		for _, e := range g.Events[n] {
			fmt.Printf("  %s +%v  %s\n", e.Start.Local().Format("Mon 15:04"), e.Duration.Round(time.Minute), e.Title)
			if e.Description != "" {
				fmt.Printf("      %.100s\n", e.Description)
			}
		}
	}
}
