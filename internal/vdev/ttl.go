package vdev

import (
	"context"
	"net"
	"net/netip"
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
// streams do the same. Unset (0) means 3, and it is never more than 8: the
// devices push a lot of bandwidth and must not be usable from far away. It
// reports whether the option was accepted.
func setTTL(c net.Conn, ttl int) bool {
	if c == nil {
		return false
	}
	ttl = clampTTL(ttl)
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

const (
	defaultTTL = 3
	maxTTL     = 8
)

// clampTTL turns the configured value into the one used: 0 (or less) is the
// default, and more than maxTTL is quietly cut to it.
func clampTTL(ttl int) int {
	switch {
	case ttl <= 0:
		return defaultTTL
	case ttl > maxTTL:
		return maxTTL
	}
	return ttl
}

// privateTarget reports whether a UDP/RTP target is an address on a private
// IPv4 network (RFC 1918), or this machine, or link-local. A control request is
// unauthenticated and its source address can be forged, so a target anywhere
// else would turn a device into a source of unsolicited video at somebody.
func privateTarget(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	return a.Is4() && (a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast())
}
