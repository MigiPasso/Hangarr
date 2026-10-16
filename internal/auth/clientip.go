package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// IPResolver extracts the real client IP, trusting X-Forwarded-For only when
// the TCP peer is a configured trusted proxy.
type IPResolver struct {
	Trusted []netip.Prefix
}

func (r IPResolver) trusted(a netip.Addr) bool {
	for _, p := range r.Trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func parseAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}

// ClientIP returns the client address. X-Forwarded-For is walked from right
// to left, skipping trusted proxies; the first untrusted hop is the client.
// If every hop is trusted, the leftmost valid one is returned.
func (r IPResolver) ClientIP(req *http.Request) netip.Addr {
	peer, ok := parseAddr(req.RemoteAddr)
	if !ok {
		return netip.Addr{}
	}
	if !r.trusted(peer) {
		return peer
	}
	var hops []string
	for _, v := range req.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseAddr(hops[i])
		if !ok {
			// Garbage in the chain: stop at the last trusted hop.
			return client
		}
		client = a
		if !r.trusted(a) {
			return a
		}
	}
	return client
}

// ClientKey is the key of a client for rate limits and lockouts: the IPv4
// address, or the /64 network of an IPv6 address (one host usually owns a
// whole /64, so its addresses must count as one client).
func ClientKey(ip string) string {
	a, ok := parseAddr(ip)
	if !ok {
		return strings.TrimSpace(ip)
	}
	if a.Is6() {
		p, err := a.Prefix(64)
		if err == nil {
			return p.String()
		}
	}
	return a.String()
}
