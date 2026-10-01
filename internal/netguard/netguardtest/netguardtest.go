// Package netguardtest lets tests that stand up httptest servers on
// loopback reach them through the SSRF guard. Production code must never
// import it.
package netguardtest

import (
	"context"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
)

// AllowLoopback installs a process-wide policy that exempts 127.0.0.0/8
// and ::1 (what httptest listens on). Call it from a test package's init.
func AllowLoopback() {
	p, err := netguard.NewPolicy([]string{"127.0.0.0/8", "::1/128"}, []string{"localhost"})
	if err != nil {
		panic(err)
	}
	netguard.SetPolicy(p)
}

// HangDNS makes every hostname lookup block until its context ends, as an
// unreachable nameserver does. Call the returned func to restore.
func HangDNS() (restore func()) { return netguard.SetResolverForTest(hangingResolver{}) }

type hangingResolver struct{}

func (hangingResolver) LookupHost(ctx context.Context, _ string) ([]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
