package headers

import (
	"context"
	"testing"

	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// lockProbe is an external provider that answers whether the ctx it was
// handed allows resolving a platform-locked credential, and names the
// credential its config holds.
type lockProbe struct{}

func (lockProbe) Type() string                  { return "external" }
func (lockProbe) ProviderID() string            { return "probe" }
func (lockProbe) ProviderName() string          { return "Probe" }
func (lockProbe) ConfigSchema() map[string]any  { return nil }
func (lockProbe) Validate(map[string]any) error { return nil }
func (lockProbe) ReferencedCredentials(cfg map[string]any) []string {
	if n, _ := cfg["credential"].(string); n != "" {
		return []string{n}
	}
	return nil
}
func (lockProbe) Resolve(ctx context.Context, _ map[string]any) (string, error) {
	if store.LockedAccess(ctx) {
		return "allowed", nil
	}
	return "denied", nil
}

// Only a header carrying the platform admin's stamp resolves with locked
// access, and an allowance already in the caller's ctx never leaks into a
// header without it.
func TestRegistry_LockedAccessComesOnlyFromTheHeaderStamp(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterExternal(lockProbe{}); err != nil {
		t.Fatal(err)
	}
	plain := map[string]any{"type": "external", "provider": "probe", "config": map[string]any{"credential": "c"}}
	stamped := map[string]any{"type": "external", "provider": "probe", "config": map[string]any{"credential": "c"}, pkgheaders.AllowLockedKey: true}
	notBool := map[string]any{"type": "external", "provider": "probe", "config": map[string]any{}, pkgheaders.AllowLockedKey: "true"}

	cases := []struct {
		name string
		ctx  context.Context
		cfg  map[string]any
		want string
	}{
		{"plain header", context.Background(), plain, "denied"},
		{"plain header, allowance in caller ctx", store.WithLockedAccess(context.Background(), true), plain, "denied"},
		{"stamped header", context.Background(), stamped, "allowed"},
		{"stamp that is not a bool", context.Background(), notBool, "denied"},
	}
	for _, tc := range cases {
		got, err := reg.Resolve(tc.ctx, "X-Probe", tc.cfg, nil)
		if err != nil || got != tc.want {
			t.Errorf("%s: Resolve = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}

	if got := reg.ReferencedCredentials(plain); len(got) != 1 || got[0] != "c" {
		t.Errorf("ReferencedCredentials(external probe) = %v, want [c]", got)
	}
	if got := reg.ReferencedCredentials(map[string]any{"type": "static", "value": "x"}); got != nil {
		t.Errorf("ReferencedCredentials(static) = %v, want none", got)
	}
	if got := reg.ReferencedCredentials(map[string]any{"type": "external", "provider": "missing"}); got != nil {
		t.Errorf("ReferencedCredentials(unknown provider) = %v, want none", got)
	}
}
