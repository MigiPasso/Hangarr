package web

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// localSuffixes are domains that only resolve on a local network: an attacker
// cannot point them at a victim's network from the Internet.
var localSuffixes = []string{".local", ".lan", ".home.arpa", ".internal", ".localdomain", ".localhost"}

// hostExempt are the paths answered whatever the Host: probes, and the
// webhook (authenticated by its secret).
var hostExempt = []string{"/healthz", "/readyz", "/webhooks/tautulli"}

// allowedHost reports whether a plain-HTTP request for host may be served.
// DNS rebinding (an attacker's domain that resolves to a LAN address, opened
// in a victim's browser) needs plain HTTP: over HTTPS the certificate does
// not match the attacker's domain. So over HTTP only local names are
// accepted: localhost, IP literals, single-label names (hangarr, nas), names
// under a local-only suffix, and ALLOWED_HOSTS.
func (s *Server) allowedHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return false
	}
	if slices.Contains(s.Config.AllowedHosts, "*") || slices.Contains(s.Config.AllowedHosts, host) {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range localSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// checkHost answers 421 Misdirected Request to a plain-HTTP request whose
// Host is not one of ours (see allowedHost).
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(hostExempt, r.URL.Path) || s.ips.SecureRequest(r) || s.allowedHost(r.Host) {
			next.ServeHTTP(w, r)
			return
		}
		s.Log.Warn("request for an unknown host refused: add it to ALLOWED_HOSTS if it is yours", "host", r.Host,
			"ip", s.clientMeta(r).IP)
		msg := s.loc(r).T("Unknown host %q: Hangarr refuses it over plain HTTP (DNS rebinding protection). If this address is yours, add it to ALLOWED_HOSTS, or use HTTPS through a proxy listed in TRUSTED_PROXIES.", r.Host)
		http.Error(w, msg, http.StatusMisdirectedRequest)
	})
}
