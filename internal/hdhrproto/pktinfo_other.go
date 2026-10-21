//go:build !linux

package hdhrproto

import "net"

// Other systems do not say which address a datagram was sent to here: every UDP
// request is treated as possibly a broadcast.
func enablePktinfo(*net.UDPConn) bool { return false }

func destinationOf([]byte) net.IP { return nil }
