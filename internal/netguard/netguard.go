// Package netguard is the gateway's outbound-request (SSRF) guard. Every
// HTTP client that dials an operator- or tenant-supplied URL uses
// DialContext (or Transport) so the address ACTUALLY dialed -- after DNS
// resolution, redirects and any encoding trick in the URL -- is checked
// against a blocklist of internal ranges. The check runs in the dialer's
// Control hook, i.e. on the resolved IP right before connect(2), which is
// what makes DNS rebinding and "a hostname that resolves to 127.0.0.1"
// ineffective.
//
// The process-wide Policy is installed once at startup from the
// egress.* config keys (SetPolicy). The default policy blocks every
// internal range; an operator opens specific CIDRs or hostnames.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrBlocked is wrapped by every refusal so callers can map it to a
// stable error code (egress_blocked).
var ErrBlocked = errors.New("egress_blocked")

// MaxRedirects caps redirect chains for clients that follow them.
const MaxRedirects = 5

// blocked lists the ranges refused unless the operator allowlists them.
var blocked = mustPrefixes(
	"0.0.0.0/8",      // "this" network, unspecified
	"10.0.0.0/8",     // RFC 1918
	"100.64.0.0/10",  // CGNAT
	"127.0.0.0/8",    // loopback
	"169.254.0.0/16", // link-local, cloud metadata
	"172.16.0.0/12",  // RFC 1918
	"192.168.0.0/16", // RFC 1918
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved, includes 255.255.255.255 broadcast
	"::/128",         // unspecified
	"::1/128",        // loopback
	"fe80::/10",      // link-local
	"fc00::/7",       // unique local (includes fd00:ec2::254 metadata)
	"ff00::/8",       // multicast
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Policy is an immutable egress policy.
type Policy struct {
	cidrs []netip.Prefix
	hosts map[string]bool
}

// NewPolicy builds a Policy from allowed CIDRs and exact hostnames.
func NewPolicy(allowedCIDRs, allowedHosts []string) (*Policy, error) {
	p := &Policy{hosts: map[string]bool{}}
	for _, c := range allowedCIDRs {
		c = strings.TrimSpace(c)
		pfx, err := netip.ParsePrefix(c)
		if err != nil {
			// A bare IP is accepted as a /32 or /128.
			ip, ipErr := netip.ParseAddr(c)
			if ipErr != nil {
				return nil, fmt.Errorf("egress.allowed_cidrs: %q is not a CIDR or IP", c)
			}
			pfx = netip.PrefixFrom(ip, ip.BitLen())
		}
		p.cidrs = append(p.cidrs, pfx.Masked())
	}
	for _, h := range allowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			return nil, errors.New("egress.allowed_hosts: empty hostname")
		}
		p.hosts[h] = true
	}
	return p, nil
}

var current atomic.Pointer[Policy]

// SetPolicy installs the process-wide policy (nil restores the default,
// which blocks everything internal).
func SetPolicy(p *Policy) { current.Store(p) }

// Current returns the installed policy, or the block-all default.
func Current() *Policy {
	if p := current.Load(); p != nil {
		return p
	}
	return &Policy{hosts: map[string]bool{}}
}

func (p *Policy) cidrAllowed(ip netip.Addr) bool {
	for _, c := range p.cidrs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// HostAllowed reports whether host is an operator-allowlisted hostname.
func (p *Policy) HostAllowed(host string) bool {
	return p.hosts[strings.ToLower(strings.TrimSuffix(host, "."))]
}

// CheckIP returns an ErrBlocked error when ip is in a blocked range and
// not allowlisted. IPv4-mapped (::ffff:a.b.c.d), IPv4-compatible
// (::a.b.c.d) and NAT64 (64:ff9b::a.b.c.d) forms are judged by the IPv4
// address they embed.
func (p *Policy) CheckIP(ip netip.Addr) error {
	ip = ip.Unmap().WithZone("")
	if p.cidrAllowed(ip) {
		return nil
	}
	if emb, ok := embeddedV4(ip); ok {
		if p.cidrAllowed(emb) {
			return nil
		}
		ip = emb
	}
	for _, b := range blocked {
		if b.Contains(ip) {
			return fmt.Errorf("%w: destination %s is in the internal range %s (allow it with egress.allowed_cidrs or egress.allowed_hosts)", ErrBlocked, ip, b)
		}
	}
	return nil
}

func embeddedV4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	allZero := func(s []byte) bool {
		for _, x := range s {
			if x != 0 {
				return false
			}
		}
		return true
	}
	// ::a.b.c.d (deprecated IPv4-compatible); skip :: and ::1 and ::0.0.0.x.
	if allZero(b[:12]) && !allZero(b[12:15]) {
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	// 64:ff9b::/96 NAT64.
	if b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && allZero(b[4:12]) {
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	return netip.Addr{}, false
}

// ParseHostIP interprets host as an IP literal in any form the C
// resolver accepts: IPv6, dotted quad, and the inet_aton shorthand
// (decimal 2130706433, hex 0x7f000001, octal 0177.1, 127.1). It returns
// ok=false for a real hostname.
func ParseHostIP(host string) (netip.Addr, bool) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip, true
	}
	return parseLegacyIPv4(host)
}

func parseLegacyIPv4(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		if p == "" {
			return netip.Addr{}, false
		}
		base := 10
		switch {
		case strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X"):
			base, p = 16, p[2:]
			if p == "" {
				p = "0"
			}
		case len(p) > 1 && p[0] == '0':
			base, p = 8, p[1:]
		}
		n, err := strconv.ParseUint(p, base, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		nums[i] = n
	}
	var v uint64
	last := len(nums) - 1
	for i := 0; i < last; i++ {
		if nums[i] > 255 {
			return netip.Addr{}, false
		}
		v |= nums[i] << (8 * uint(3-i))
	}
	if nums[last] >= 1<<(8*uint(4-last)) {
		return netip.Addr{}, false
	}
	v |= nums[last]
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

// CheckHost is the create-time fast-feedback check: when host is an IP
// literal (in any encoding) in a blocked range it returns ErrBlocked.
// Hostnames pass; they are checked when dialed.
func (p *Policy) CheckHost(host string) error {
	if p.HostAllowed(host) {
		return nil
	}
	if ip, ok := ParseHostIP(host); ok {
		return p.CheckIP(ip)
	}
	return nil
}

// CheckHost checks host against the installed policy.
func CheckHost(host string) error { return Current().CheckHost(host) }

// hostResolveTimeout bounds how long CheckHostResolve waits for DNS
// before giving up and letting the create go through (the dial-time
// guard remains the authority either way).
const hostResolveTimeout = 2 * time.Second

// hostResolver is the subset of *net.Resolver CheckHostResolve needs,
// factored out so tests can substitute a stub without touching the
// network or a real nameserver. Production always uses net.DefaultResolver
// -- the same resolver net.Dialer.DialContext uses internally when given
// a hostname -- so a name CheckHostResolve judges safe resolves the same
// way at dial time.
type hostResolverFunc interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

var resolver hostResolverFunc = net.DefaultResolver

// SetResolverForTest swaps the resolver CheckHostResolve uses and returns a
// func that restores it. Tests only (see netguardtest.HangDNS).
func SetResolverForTest(r interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}) (restore func()) {
	prev := resolver
	resolver = r
	return func() { resolver = prev }
}

// CheckHostResolve is CheckHost plus a DNS lookup: where CheckHost only
// catches an IP literal, CheckHostResolve also catches a hostname that
// resolves to a blocked range (e.g. an MCP connector endpoint of
// "http://localhost:1234", which CheckHost lets through because
// "localhost" isn't a literal). It is still just fast feedback, not the
// security boundary -- that remains the dial-time Control hook on the
// resolved IP, which a DNS answer can't be rebound past:
//
//   - "localhost" and any "*.localhost" name are judged as loopback
//     without a DNS round trip (RFC 6761 reserves the TLD for it, and
//     some resolvers don't forward it upstream at all).
//   - Otherwise the host is resolved with a hostResolveTimeout bound. A
//     lookup failure or timeout is NOT a rejection -- the name may not
//     have propagated yet, or this environment may be offline -- so the
//     create is allowed and the dial-time guard is left to enforce it.
//   - A host that resolves to at least one address outside the blocked
//     ranges is allowed (the same reasoning CheckIP uses: Go's dialer
//     may pick any resolved address, so rejecting requires every one of
//     them to be blocked).
func (p *Policy) CheckHostResolve(ctx context.Context, host string) error {
	if p.HostAllowed(host) {
		return nil
	}
	if ip, ok := ParseHostIP(host); ok {
		return p.CheckIP(ip)
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return p.CheckIP(netip.MustParseAddr("127.0.0.1"))
	}

	rctx, cancel := context.WithTimeout(ctx, hostResolveTimeout)
	defer cancel()
	addrs, err := resolver.LookupHost(rctx, host)
	if err != nil || len(addrs) == 0 {
		return nil
	}
	var blockedErr error
	for _, a := range addrs {
		ip, perr := netip.ParseAddr(a)
		if perr != nil {
			continue
		}
		if cerr := p.CheckIP(ip); cerr != nil {
			blockedErr = cerr
			continue
		}
		// At least one resolved address is not blocked.
		return nil
	}
	if blockedErr != nil {
		return fmt.Errorf("%s: %w", host, blockedErr)
	}
	return nil
}

// CheckHostResolve checks host (resolving it if it isn't an IP literal)
// against the installed policy. See Policy.CheckHostResolve.
func CheckHostResolve(ctx context.Context, host string) error {
	return Current().CheckHostResolve(ctx, host)
}

// control is the net.Dialer Control hook: it sees the resolved IP.
func (p *Policy) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: cannot parse dial address %q", ErrBlocked, address)
	}
	return p.CheckIP(ap.Addr())
}

// DialContext returns a dial function enforcing p. An allowlisted
// hostname is dialed unchecked for that dial only; everything else is
// checked on the resolved IP.
func (p *Policy) DialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	guarded := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: p.control}
	open := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err == nil && p.HostAllowed(host) {
			return open.DialContext(ctx, network, addr)
		}
		return guarded.DialContext(ctx, network, addr)
	}
}

// DialContext dials under the process-wide policy, read at dial time so
// SetPolicy after client construction still applies.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return Current().DialContext()(ctx, network, addr)
}

// Transport returns a clone of http.DefaultTransport that dials through
// the guard.
func Transport() *http.Transport {
	var t *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		t = dt.Clone()
	} else {
		t = &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	}
	t.DialContext = DialContext
	return t
}

// LimitRedirects is a CheckRedirect that caps the chain at MaxRedirects.
// Each hop is dialed through the guard, so a hop to an internal address
// fails at connect time.
func LimitRedirects(_ *http.Request, via []*http.Request) error {
	if len(via) >= MaxRedirects {
		return fmt.Errorf("stopped after %d redirects", MaxRedirects)
	}
	return nil
}
