// Package clientip resolves the caller's IP address for rate limiting and
// access logging. It is the ONE implementation every plane (API, MCP, LLM)
// and every capture path uses, so a request is attributed to the same
// address everywhere.
//
// X-Forwarded-For is only believed when the TCP peer is a configured
// trusted proxy, and then it is read from the RIGHT: every proxy appends the
// address it received the request from, so the right-most entries are the
// ones written by infrastructure we trust and the left-most is whatever the
// client chose to claim. Reading the left-most entry (the old behavior) let
// any client pick its own rate-limit identity, or frame someone else's.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolver turns a request into a client IP. The zero value and a nil
// *Resolver trust no proxy: the TCP peer address is always used.
type Resolver struct {
	trusted []netip.Prefix
}

// New builds a Resolver trusting the given proxies. Each entry is a single
// IP ("10.0.0.5", "::1") or a CIDR ("10.0.0.0/8"). Blank entries are
// skipped; a malformed entry is an error so a typo fails startup instead of
// silently trusting nothing (or, worse, the wrong range).
func New(entries []string) (*Resolver, error) {
	r := &Resolver{}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			p, err := netip.ParsePrefix(e)
			if err != nil {
				return nil, fmt.Errorf("clientip: invalid trusted proxy CIDR %q: %w", e, err)
			}
			r.trusted = append(r.trusted, normalizePrefix(p))
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("clientip: invalid trusted proxy %q (want an IP or a CIDR): %w", e, err)
		}
		a = normalize(a)
		r.trusted = append(r.trusted, netip.PrefixFrom(a, a.BitLen()))
	}
	return r, nil
}

// MustNew is New for static, known-good input (tests, defaults).
func MustNew(entries ...string) *Resolver {
	r, err := New(entries)
	if err != nil {
		panic(err)
	}
	return r
}

// Trusts reports whether any proxy is configured.
func (r *Resolver) Trusts() bool { return r != nil && len(r.trusted) > 0 }

// IP returns the client address for req as a canonical string.
//
//   - The peer (RemoteAddr) is the answer unless it is a trusted proxy.
//   - If it is, X-Forwarded-For (all header lines, in order) is walked from
//     the right, skipping entries inside a trusted range; the first
//     untrusted entry is the client.
//   - If every entry is trusted, the left-most entry is returned.
//   - A malformed entry stops the walk: nothing to its left can be believed,
//     so the closest address already seen (the hop to its right) is returned.
func (r *Resolver) IP(req *http.Request) string {
	peerStr := hostOnly(req.RemoteAddr)
	peer, err := netip.ParseAddr(peerStr)
	if err != nil {
		return peerStr
	}
	peer = normalize(peer)
	if !r.isTrusted(peer) {
		return peer.String()
	}

	hops := forwardedHops(req)
	cur := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			return cur.String()
		}
		if !r.isTrusted(a) {
			return a.String()
		}
		cur = a
	}
	// No XFF at all, or every hop is trusted infrastructure: the left-most
	// address we saw (the peer itself when the header is absent).
	return cur.String()
}

func (r *Resolver) isTrusted(a netip.Addr) bool {
	if r == nil {
		return false
	}
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// forwardedHops returns every X-Forwarded-For entry, header lines joined in
// the order received.
func forwardedHops(req *http.Request) []string {
	var hops []string
	for _, line := range req.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(line, ",")...)
	}
	return hops
}

// parseHop parses one XFF entry: a bare IP, a bracketed IPv6, or ip:port.
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return normalize(a), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return normalize(ap.Addr()), true
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		if a, err := netip.ParseAddr(s[1 : len(s)-1]); err == nil {
			return normalize(a), true
		}
	}
	return netip.Addr{}, false
}

// normalize canonicalizes an address so one client has one limiter key:
// IPv4-mapped IPv6 collapses to IPv4 and a zone is dropped.
func normalize(a netip.Addr) netip.Addr {
	return a.Unmap().WithZone("")
}

func normalizePrefix(p netip.Prefix) netip.Prefix {
	a := p.Addr()
	if a.Is4In6() {
		// ::ffff:10.0.0.0/104 style: re-express as the IPv4 prefix.
		if bits := p.Bits() - 96; bits >= 0 {
			return netip.PrefixFrom(a.Unmap(), bits).Masked()
		}
	}
	return p.Masked()
}

func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
