//go:build linux

package hdhrproto

import (
	"net"
	"syscall"
)

// enablePktinfo asks the kernel to say which local address each datagram was sent
// to, so that a request addressed to this machine can be told from a broadcast.
func enablePktinfo(c *net.UDPConn) bool {
	rc, err := c.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	rc.Control(func(fd uintptr) {
		ok = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_PKTINFO, 1) == nil
	})
	return ok
}

// destinationOf reads the address a datagram was sent to from its control data
// (struct in_pktinfo: interface index, the address it was routed to, the header
// destination).
func destinationOf(oob []byte) net.IP {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	for _, m := range msgs {
		if m.Header.Level == syscall.IPPROTO_IP && m.Header.Type == syscall.IP_PKTINFO && len(m.Data) >= 12 {
			return net.IPv4(m.Data[8], m.Data[9], m.Data[10], m.Data[11])
		}
	}
	return nil
}
