package headers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// sourceEchoProvider is a source-aware provider: it answers with what the
// registry handed it, so the tests can see the source and request info.
type sourceEchoProvider struct{ id string }

func (p sourceEchoProvider) Type() string                        { return "external" }
func (p sourceEchoProvider) ProviderID() string                  { return p.id }
func (p sourceEchoProvider) ProviderName() string                { return p.id }
func (p sourceEchoProvider) ConfigSchema() map[string]any        { return map[string]any{} }
func (p sourceEchoProvider) SourceSchema() map[string]any        { return map[string]any{} }
func (p sourceEchoProvider) ValidateSource(map[string]any) error { return nil }
func (p sourceEchoProvider) Validate(map[string]any) error       { return nil }
func (p sourceEchoProvider) Resolve(ctx context.Context, cfg map[string]any) (string, error) {
	info, _ := pkgheaders.ResolveInfoFrom(ctx)
	if info.Source == nil {
		return "", errors.New("no source")
	}
	v, _ := info.Source.Config["value"].(string)
	return info.ConnectorSlug + "/" + info.HeaderName + "/" + info.Source.Name + "=" + v, nil
}

type sourceFixture struct {
	reg     *Registry
	st      *dptest.Store
	tenantA string
	tenantB string
}

func newSourceFixture(t *testing.T) *sourceFixture {
	t.Helper()
	ctx := context.Background()
	st := dptest.New()
	reg := NewRegistry()
	for _, id := range []string{"svc", "other_svc"} {
		if err := reg.RegisterExternal(sourceEchoProvider{id: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.RegisterExternal(&fakeProvider{id: "plain", name: "Plain"}); err != nil {
		t.Fatal(err)
	}
	reg.SetSourceLookup(StoreSourceLookup(st.CredentialSources()))

	mk := func(tenant, name, provider, value string, enabled bool) {
		if err := st.CredentialSources().Create(ctx, &store.CredentialSource{
			TenantID: tenant, Name: name, Provider: provider, Enabled: enabled,
			Config: map[string]any{"value": value},
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("tenant-a", "vault", "svc", "a-value", true)
	mk("tenant-a", "off", "svc", "off-value", false)
	mk("tenant-a", "foreign", "other_svc", "x", true)
	mk("tenant-b", "b-only", "svc", "b-value", true)
	return &sourceFixture{reg: reg, st: st, tenantA: "tenant-a", tenantB: "tenant-b"}
}

func principalCtx(tenant string) context.Context {
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "k", TenantID: tenant})
	return pkgheaders.WithResolveInfo(ctx, pkgheaders.ResolveInfo{ConnectorSlug: "conn"})
}

func sourceHeader(provider, source string) map[string]any {
	return map[string]any{"type": "external", "provider": provider, "source": source, "config": map[string]any{}}
}

func TestRegistry_Source_PassedToProvider(t *testing.T) {
	f := newSourceFixture(t)
	v, err := f.reg.Resolve(principalCtx(f.tenantA), "X-Key", sourceHeader("svc", "vault"), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "conn/X-Key/vault=a-value" {
		t.Errorf("Resolve = %q", v)
	}
}

func TestRegistry_Source_Refusals(t *testing.T) {
	f := newSourceFixture(t)
	for _, tc := range []struct {
		name, tenant, provider, source, want string
	}{
		{"another tenant's source", f.tenantA, "svc", "b-only", "not found"},
		{"own source from another tenant", f.tenantB, "svc", "vault", "not found"},
		{"disabled", f.tenantA, "svc", "off", "disabled"},
		{"provider mismatch", f.tenantA, "svc", "foreign", "belongs to provider"},
		{"unknown", f.tenantA, "svc", "nope", "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.reg.Resolve(principalCtx(tc.tenant), "X-Key", sourceHeader(tc.provider, tc.source), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.source) {
				t.Errorf("err = %v, want one naming %q and containing %q", err, tc.source, tc.want)
			}
		})
	}

	t.Run("no principal", func(t *testing.T) {
		if _, err := f.reg.Resolve(context.Background(), "X-Key", sourceHeader("svc", "vault"), nil); err == nil {
			t.Error("Resolve without a principal succeeded")
		}
	})
	t.Run("no lookup installed", func(t *testing.T) {
		f.reg.SetSourceLookup(nil)
		defer f.reg.SetSourceLookup(StoreSourceLookup(f.st.CredentialSources()))
		if _, err := f.reg.Resolve(principalCtx(f.tenantA), "X-Key", sourceHeader("svc", "vault"), nil); err == nil {
			t.Error("Resolve without a source lookup succeeded")
		}
	})
}

func TestRegistry_Source_ShapeChecks(t *testing.T) {
	f := newSourceFixture(t)
	for _, tc := range []struct {
		name    string
		cfg     map[string]any
		wantErr string
	}{
		{"source provider without source", map[string]any{"type": "external", "provider": "svc", "config": map[string]any{}}, "needs a credential source"},
		{"plain provider with source", map[string]any{"type": "external", "provider": "plain", "source": "vault", "config": map[string]any{}}, "does not use credential sources"},
		{"non-string source", map[string]any{"type": "external", "provider": "svc", "source": 7, "config": map[string]any{}}, "must be a string"},
		{"ok", sourceHeader("svc", "vault"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := f.reg.CheckConnectorHeaders(map[string]any{"headers": map[string]any{"X-Key": tc.cfg}})
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("CheckConnectorHeaders = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("CheckConnectorHeaders = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestRegistry_CheckConnectorSources(t *testing.T) {
	f := newSourceFixture(t)
	ctx := context.Background()
	meta := func(provider, source string) map[string]any {
		return map[string]any{"headers": map[string]any{"X-Key": sourceHeader(provider, source)}}
	}
	if err := f.reg.CheckConnectorSources(ctx, f.tenantA, meta("svc", "vault")); err != nil {
		t.Errorf("own enabled source: %v", err)
	}
	if err := f.reg.CheckConnectorSources(ctx, f.tenantA, meta("svc", "off")); err != nil {
		t.Errorf("own disabled source should be accepted at save time: %v", err)
	}
	if err := f.reg.CheckConnectorSources(ctx, f.tenantA, meta("svc", "b-only")); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("another tenant's source: err = %v, want ErrSourceNotFound", err)
	}
	if err := f.reg.CheckConnectorSources(ctx, f.tenantA, meta("svc", "foreign")); err == nil {
		t.Error("provider mismatch accepted")
	}
	if err := f.reg.CheckConnectorSources(ctx, f.tenantA, map[string]any{}); err != nil {
		t.Errorf("no headers: %v", err)
	}
}
