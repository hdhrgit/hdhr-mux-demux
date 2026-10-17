// Command tscheck reads an MPEG transport stream on stdin and reports packet
// counts, PIDs, continuity errors and the programs announced by the PAT/PMTs.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"

	"hdhrmux/internal/ts"
)

func main() {
	r := bufio.NewReaderSize(os.Stdin, 1<<20)
	pkt := make([]byte, ts.PacketSize)
	pids := map[uint16]int{}
	last := map[uint16]int{}
	ccErr, syncErr, total := 0, 0, 0
	readers := map[uint16]*ts.SectionReader{0: {}}
	pmtPIDs := map[uint16]uint16{}
	pmts := map[uint16]*ts.PMT{}
	var pat *ts.PAT
	for {
		if _, err := io.ReadFull(r, pkt); err != nil {
			break
		}
		if pkt[0] != ts.SyncByte {
			syncErr++
			// resync byte by byte
			for pkt[0] != ts.SyncByte {
				b, err := r.ReadByte()
				if err != nil {
					break
				}
				copy(pkt, pkt[1:])
				pkt[ts.PacketSize-1] = b
			}
			continue
		}
		total++
		pid := ts.PID(pkt)
		pids[pid]++
		if pid != ts.PIDNull && ts.HasPayload(pkt) {
			if c, ok := last[pid]; ok && int(ts.CC(pkt)) != (c+1)&15 && int(ts.CC(pkt)) != c {
				ccErr++
			}
			last[pid] = int(ts.CC(pkt))
		}
		if rd := readers[pid]; rd != nil {
			for _, sec := range rd.Push(pkt) {
				if pid == 0 {
					if p, err := ts.ParsePAT(sec); err == nil {
						pat = p
						for prog, pp := range p.Programs {
							pmtPIDs[pp] = prog
							if readers[pp] == nil {
								readers[pp] = &ts.SectionReader{}
							}
						}
					}
				} else if m, err := ts.ParsePMT(sec); err == nil {
					pmts[m.Program] = m
				}
			}
		}
	}
	fmt.Printf("packets=%d sync_errors=%d continuity_errors=%d distinct_pids=%d\n", total, syncErr, ccErr, len(pids))
	if pat != nil {
		var progs []int
		for p := range pat.Programs {
			progs = append(progs, int(p))
		}
		sort.Ints(progs)
		fmt.Printf("PAT tsid=0x%04x programs=%v\n", pat.TSID, progs)
		for _, p := range progs {
			if m := pmts[uint16(p)]; m != nil {
				fmt.Printf("  program %d pmt_pid=0x%04x pcr=0x%04x streams=%v\n", p, pat.Programs[uint16(p)], m.PCRPID, m.Streams)
			}
		}
	}
	var keys []int
	for p := range pids {
		keys = append(keys, int(p))
	}
	sort.Ints(keys)
	for _, p := range keys {
		fmt.Printf("  pid 0x%04x: %d\n", p, pids[uint16(p)])
	}
}
