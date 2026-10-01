package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func mustPolicy(t *testing.T, cidrs, hosts []string) *Policy {
	t.Helper()
	p, err := NewPolicy(cidrs, hosts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckIP_BlockedRanges(t *testing.T) {
	p := mustPolicy(t, nil, nil)
	blocked := []string{
		"127.0.0.1", "127.255.255.254", "0.0.0.0", "0.1.2.3",
		"10.0.0.1", "10.255.255.1",
		"172.16.0.1", "172.31.255.255",
		"192.168.1.1",
		"169.254.169.254", "169.254.0.1",
		"100.64.0.1", "100.127.255.255",
		"224.0.0.1", "239.255.255.255",
		"255.255.255.255", "240.0.0.1",
		"::", "::1", "fe80::1", "fc00::1", "fd00:ec2::254", "ff02::1",
		// IPv4-mapped / compatible / NAT64 forms of blocked addresses.
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"::127.0.0.1", "::10.1.2.3", "64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe",
	}
	for _, s := range blocked {
		err := p.CheckIP(netip.MustParseAddr(s))
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("CheckIP(%s) = %v, want ErrBlocked", s, err)
		}
	}
	allowed := []string{
		"8.8.8.8", "1.1.1.1", "172.15.255.255", "172.32.0.1", "100.63.255.255", "100.128.0.1",
		"169.253.0.1", "192.169.0.1", "11.0.0.1",
		"2606:4700:4700::1111", "::ffff:8.8.8.8", "64:ff9b::808:808",
	}
	for _, s := range allowed {
		if err := p.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("CheckIP(%s) = %v, want nil", s, err)
		}
	}
}

func TestCheckHost_LiteralForms(t *testing.T) {
	p := mustPolicy(t, nil, nil)
	for _, h := range []string{
		"127.0.0.1", "2130706433", "0x7f000001", "0x7f.1", "0177.0.0.1", "017700000001", "127.1", "127.0.1",
		"169.254.169.254", "2852039166", "0xa9fea9fe", "10.255.255.1", "167837697",
		"[::1]", "::1", "::ffff:127.0.0.1", "[::ffff:7f00:1]", "0", "0.0.0.0", "fe80::1%eth0",
	} {
		if err := p.CheckHost(h); !errors.Is(err, ErrBlocked) {
			t.Errorf("CheckHost(%q) = %v, want ErrBlocked", h, err)
		}
	}
	for _, h := range []string{"example.com", "mcp.deepwiki.com", "8.8.8.8", "localhost", "999.1.1.1", "1.2.3.4.5", "256.0.0.1"} {
		if err := p.CheckHost(h); err != nil {
			t.Errorf("CheckHost(%q) = %v, want nil (hostnames are checked at dial time)", h, err)
		}
	}
}

func TestAllowlist(t *testing.T) {
	p := mustPolicy(t, []string{"127.0.0.0/8", "10.1.0.0/16", "::1/128", "192.168.5.5"}, []string{"Host.Docker.Internal"})
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "::1", "::ffff:127.0.0.1", "192.168.5.5"} {
		if err := p.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("allowlisted %s: %v", s, err)
		}
	}
	for _, s := range []string{"10.2.0.1", "192.168.5.6", "169.254.169.254"} {
		if err := p.CheckIP(netip.MustParseAddr(s)); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s = %v, want blocked", s, err)
		}
	}
	if !p.HostAllowed("host.docker.internal") || !p.HostAllowed("HOST.docker.internal.") || p.HostAllowed("host.docker.internal.evil.com") {
		t.Error("HostAllowed must match the exact name only (case-insensitive)")
	}
	if err := p.CheckHost("host.docker.internal"); err != nil {
		t.Error(err)
	}
	if _, err := NewPolicy([]string{"nope"}, nil); err == nil {
		t.Error("bad CIDR accepted")
	}
}

func TestDialContext_BlocksLoopbackAndAllowlists(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if c, err := mustPolicy(t, nil, nil).DialContext()(ctx, "tcp", addr); err == nil {
		c.Close()
		t.Fatal("dial to loopback succeeded under the default policy")
	} else if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}

	// A hostname that resolves to 127.0.0.1 is blocked on the resolved IP.
	_, port, _ := net.SplitHostPort(addr)
	if c, err := mustPolicy(t, nil, nil).DialContext()(ctx, "tcp", net.JoinHostPort("localhost", port)); err == nil {
		c.Close()
		t.Fatal("dial via localhost succeeded under the default policy")
	} else if !errors.Is(err, ErrBlocked) {
		t.Fatalf("localhost err = %v, want ErrBlocked", err)
	}

	// Allowlisted CIDR.
	c, err := mustPolicy(t, []string{"127.0.0.0/8", "::1/128"}, nil).DialContext()(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("allowlisted CIDR dial: %v", err)
	}
	c.Close()

	// Allowlisted hostname (resolves to loopback, allowed for that name only).
	c, err = mustPolicy(t, nil, []string{"localhost"}).DialContext()(ctx, "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("allowlisted host dial: %v", err)
	}
	c.Close()
	if c, err := mustPolicy(t, nil, []string{"localhost"}).DialContext()(ctx, "tcp", addr); err == nil {
		c.Close()
		t.Fatal("allowing the name localhost must not allow the literal 127.0.0.1")
	}
}

func TestRedirectToBlockedIPIsRefused(t *testing.T) {
	target := 0
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { target++ }))
	defer internal.Close()

	// The "public" front is also on loopback, so allowlist ONLY its port's
	// name via a hostname allowance: dial the front through a name that is
	// allowlisted, then redirect to the literal internal address, which is not.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer front.Close()
	_, frontPort, _ := net.SplitHostPort(front.Listener.Addr().String())

	p := mustPolicy(t, nil, []string{"localhost"})
	client := &http.Client{
		Transport:     &http.Transport{DialContext: p.DialContext()},
		CheckRedirect: LimitRedirects,
		Timeout:       5 * time.Second,
	}
	_, err := client.Get("http://localhost:" + frontPort + "/")
	if err == nil || !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect to internal literal: err = %v, want ErrBlocked", err)
	}
	if target != 0 {
		t.Errorf("internal listener was hit %d times", target)
	}
}

func TestLimitRedirects(t *testing.T) {
	via := make([]*http.Request, MaxRedirects)
	if err := LimitRedirects(nil, via[:MaxRedirects-1]); err != nil {
		t.Errorf("hop %d: %v", MaxRedirects-1, err)
	}
	if err := LimitRedirects(nil, via); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("hop %d: err = %v, want stop", MaxRedirects, err)
	}
}

func TestDefaultPolicyBlocksAndSetPolicy(t *testing.T) {
	SetPolicy(nil)
	if err := CheckHost("127.0.0.1"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("default policy: %v", err)
	}
	SetPolicy(mustPolicy(t, []string{"127.0.0.0/8"}, nil))
	defer SetPolicy(nil)
	if err := CheckHost("127.0.0.1"); err != nil {
		t.Fatalf("after SetPolicy: %v", err)
	}
}

// stubResolver answers LookupHost from a fixed map, so tests don't
// depend on a real nameserver. A name absent from the map behaves like
// an NXDOMAIN / lookup failure.
type stubResolver map[string][]string

func (s stubResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	addrs, ok := s[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return addrs, nil
}

// withResolver swaps the package resolver for the duration of the test.
func withResolver(t *testing.T, r hostResolverFunc) {
	t.Helper()
	prev := resolver
	resolver = r
	t.Cleanup(func() { resolver = prev })
}

func TestCheckHostResolve(t *testing.T) {
	withResolver(t, stubResolver{
		"public.example.com":         {"93.184.216.34"},
		"internal.example.com":       {"10.0.0.5"},
		"dual.example.com":           {"10.0.0.5", "93.184.216.34"}, // one blocked, one not
		"allowlisted-ip.example.com": {"10.0.0.5"},
	})

	t.Run("IP literal behaves like CheckHost", func(t *testing.T) {
		p := mustPolicy(t, nil, nil)
		if err := p.CheckHostResolve(context.Background(), "127.0.0.1"); !errors.Is(err, ErrBlocked) {
			t.Errorf("127.0.0.1: %v, want ErrBlocked", err)
		}
	})

	t.Run("localhost is blocked without DNS under the default policy", func(t *testing.T) {
		p := mustPolicy(t, nil, nil)
		for _, host := range []string{"localhost", "LOCALHOST", "foo.localhost", "a.b.localhost"} {
			err := p.CheckHostResolve(context.Background(), host)
			if !errors.Is(err, ErrBlocked) {
				t.Errorf("%s: %v, want ErrBlocked", host, err)
			}
		}
	})

	t.Run("localhost is allowed once allowlisted, still without DNS", func(t *testing.T) {
		p := mustPolicy(t, []string{"127.0.0.0/8"}, nil)
		if err := p.CheckHostResolve(context.Background(), "localhost"); err != nil {
			t.Errorf("allowlisted localhost: %v", err)
		}
	})

	t.Run("a name in allowed_hosts is allowed without a lookup", func(t *testing.T) {
		p := mustPolicy(t, nil, []string{"internal.example.com"})
		// Not in the stub map at all -- if this reached the resolver it
		// would fail the lookup and still (correctly) return nil, so
		// this alone wouldn't prove the allowlist short-circuits. Use a
		// name that WOULD be rejected by DNS to prove it.
		if err := p.CheckHostResolve(context.Background(), "internal.example.com"); err != nil {
			t.Errorf("allowlisted host: %v", err)
		}
	})

	t.Run("a name resolving only to blocked addresses is rejected", func(t *testing.T) {
		p := mustPolicy(t, nil, nil)
		err := p.CheckHostResolve(context.Background(), "internal.example.com")
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("internal.example.com: %v, want ErrBlocked", err)
		}
		if !strings.Contains(err.Error(), "egress.allowed_cidrs") || !strings.Contains(err.Error(), "internal.example.com") {
			t.Errorf("error %q does not name the host and the setting to change", err.Error())
		}
	})

	t.Run("a name resolving to a public address is allowed", func(t *testing.T) {
		p := mustPolicy(t, nil, nil)
		if err := p.CheckHostResolve(context.Background(), "public.example.com"); err != nil {
			t.Errorf("public.example.com: %v", err)
		}
	})

	t.Run("a name resolving to a mix of addresses is allowed if any one is not blocked", func(t *testing.T) {
		p := mustPolicy(t, nil, nil)
		if err := p.CheckHostResolve(context.Background(), "dual.example.com"); err != nil {
			t.Errorf("dual.example.com: %v", err)
		}
	})

	t.Run("an unresolvable name is not rejected at create time", func(t *testing.T) {
		p := mustPolicy(t, nil, nil)
		if err := p.CheckHostResolve(context.Background(), "does-not-exist.example.com"); err != nil {
			t.Errorf("unresolvable name: %v, want nil (dial-time guard remains the authority)", err)
		}
	})

	t.Run("package-level CheckHostResolve reads the installed policy", func(t *testing.T) {
		SetPolicy(nil)
		defer SetPolicy(nil)
		if err := CheckHostResolve(context.Background(), "localhost"); !errors.Is(err, ErrBlocked) {
			t.Errorf("CheckHostResolve(localhost): %v, want ErrBlocked", err)
		}
	})
}
