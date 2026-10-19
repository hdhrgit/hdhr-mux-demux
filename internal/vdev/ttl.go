package vdev

import (
	"context"
	"net"
	"syscall"
)

// connKey carries the connection of a request in its context, so that a handler
// that starts a stream can set socket options on it.
type connKey struct{}

func withConn(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

// setTTL sets the IP time-to-live (hop limit) of what the socket sends. Real
// devices send their streams with a TTL of 3, so that a stream cannot be taken
// further than a couple of routers from the device; the multiplexer's own
// streams do the same. ttl <= 0 leaves the system default. It reports whether
// the option was accepted.
func setTTL(c net.Conn, ttl int) bool {
	if ttl <= 0 || c == nil {
		return false
	}
	sc, ok := c.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	done := false
	raw.Control(func(fd uintptr) {
		// An IPv6 socket carrying an IPv4 connection answers to IP_TTL; a pure IPv6
		// one needs the hop limit. Try both, one of them applies.
		e4 := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
		e6 := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS, ttl)
		done = e4 == nil || e6 == nil
	})
	return done
}

// getTTL reads the socket's TTL back (for tests).
func getTTL(c net.Conn, v6 bool) int {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return -1
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return -1
	}
	n := -1
	raw.Control(func(fd uintptr) {
		var err error
		if v6 {
			n, err = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS)
		} else {
			n, err = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL)
		}
		if err != nil {
			n = -1
		}
	})
	return n
}
