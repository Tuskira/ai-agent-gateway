package headers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
)

// ---------------------------------------------------------------------------
// static
// ---------------------------------------------------------------------------

func TestRegistry_Static(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "static", "value": "hello"}
	v, err := reg.Resolve(context.Background(), "X-Foo", cfg, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "hello" {
		t.Errorf("Resolve = %q, want hello", v)
	}
}

func TestRegistry_Static_MissingValue(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "static"}
	if _, err := reg.Resolve(context.Background(), "X-Foo", cfg, nil); err == nil {
		t.Error("Resolve with no \"value\" succeeded, want error")
	}
}

func TestRegistry_Prefix(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "static", "value": "abc123", "prefix": "Bearer "}
	v, err := reg.Resolve(context.Background(), "Authorization", cfg, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "Bearer abc123" {
		t.Errorf("Resolve = %q, want \"Bearer abc123\"", v)
	}
}

func TestRegistry_RejectsCRLF(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "static", "value": "evil\r\nX-Injected: yes"}
	if _, err := reg.Resolve(context.Background(), "X-Foo", cfg, nil); err == nil {
		t.Error("Resolve of a value containing CRLF succeeded, want error")
	}
}

func TestRegistry_RejectsBareLF(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "static", "value": "evil\nX-Injected: yes"}
	if _, err := reg.Resolve(context.Background(), "X-Foo", cfg, nil); err == nil {
		t.Error("Resolve of a value containing a bare LF succeeded, want error")
	}
}

func TestRegistry_UnknownType(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "no-such-type"}
	if _, err := reg.Resolve(context.Background(), "X-Foo", cfg, nil); err == nil {
		t.Error("Resolve with an unknown type succeeded, want error")
	}
}

func TestRegistry_MissingType(t *testing.T) {
	reg := NewRegistry()
	if _, err := reg.Resolve(context.Background(), "X-Foo", map[string]any{}, nil); err == nil {
		t.Error("Resolve with no \"type\" succeeded, want error")
	}
}

// ---------------------------------------------------------------------------
// token_field
// ---------------------------------------------------------------------------

func TestRegistry_TokenField(t *testing.T) {
	reg := NewRegistry()
	reg.SetAllowBearerForwarding(true)
	principal := &auth.Principal{
		Subject:       "user-1",
		TenantID:      "tenant-a",
		Email:         "avinash@tuskira.ai",
		RawCredential: "gk_abc123",
	}
	ctx := auth.WithPrincipal(context.Background(), principal)

	cases := []struct {
		field string
		want  string
	}{
		{"email", "avinash@tuskira.ai"},
		{"subject", "user-1"},
		{"tenant_id", "tenant-a"},
		{"bearer_token", "gk_abc123"},
	}
	for _, tc := range cases {
		cfg := map[string]any{"type": "token_field", "field": tc.field}
		v, err := reg.Resolve(ctx, "X-Foo", cfg, nil)
		if err != nil {
			t.Errorf("Resolve(field=%s): %v", tc.field, err)
			continue
		}
		if v != tc.want {
			t.Errorf("Resolve(field=%s) = %q, want %q", tc.field, v, tc.want)
		}
	}
}

func TestRegistry_TokenField_UnsupportedField(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "token_field", "field": "password"}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "u"})
	if _, err := reg.Resolve(ctx, "X-Foo", cfg, nil); err == nil {
		t.Error("Resolve with unsupported field succeeded, want error")
	}
}

func TestRegistry_TokenField_NoPrincipal(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "token_field", "field": "email"}
	if _, err := reg.Resolve(context.Background(), "X-Foo", cfg, nil); err == nil {
		t.Error("Resolve with no principal in context succeeded, want error")
	}
}

// ---------------------------------------------------------------------------
// incoming_field
// ---------------------------------------------------------------------------

func TestRegistry_IncomingField(t *testing.T) {
	reg := NewRegistry()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Correlation-Id", "req-123")

	cfg := map[string]any{"type": "incoming_field", "header": "X-Correlation-Id"}
	v, err := reg.Resolve(context.Background(), "X-Forwarded-Correlation-Id", cfg, req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "req-123" {
		t.Errorf("Resolve = %q, want req-123", v)
	}
}

func TestRegistry_IncomingField_MultiValueJoined(t *testing.T) {
	reg := NewRegistry()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Add("X-Trace", "a")
	req.Header.Add("X-Trace", "b")

	cfg := map[string]any{"type": "incoming_field", "header": "X-Trace"}
	v, err := reg.Resolve(context.Background(), "X-Out", cfg, req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "a, b" {
		t.Errorf("Resolve = %q, want \"a, b\"", v)
	}
}

func TestRegistry_IncomingField_MissingHeaderOnRequest(t *testing.T) {
	reg := NewRegistry()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	cfg := map[string]any{"type": "incoming_field", "header": "X-Not-Present"}
	if _, err := reg.Resolve(context.Background(), "X-Out", cfg, req); err == nil {
		t.Error("Resolve of an absent inbound header succeeded, want error")
	}
}

func TestRegistry_IncomingField_NoRequest(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "incoming_field", "header": "X-Trace"}
	if _, err := reg.Resolve(context.Background(), "X-Out", cfg, nil); err == nil {
		t.Error("Resolve with no inbound request succeeded, want error")
	}
}

func TestRegistry_IncomingField_Denylist(t *testing.T) {
	reg := NewRegistry()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("Tenantid", "tenant-a")
	req.Header.Set("X-Gateway-Key", "gk_x")
	req.Header.Set("Mcp-Session-Id", "sess-1")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Transfer-Encoding", "chunked")

	denied := []string{
		"Authorization", "authorization",
		"X-Tenant-Id", "x-tenant-id",
		"TenantId", "tenantid",
		"X-Gateway-Key",
		"mcp-session-id", "Mcp-Session-Id", "MCP-Foo",
		"Connection", "Transfer-Encoding",
	}
	for _, name := range denied {
		cfg := map[string]any{"type": "incoming_field", "header": name}
		if _, err := reg.Resolve(context.Background(), "X-Out", cfg, req); err == nil {
			t.Errorf("Resolve(header=%q) succeeded, want denylist error", name)
		}
	}
}

func TestRegistry_IncomingField_NonDenylistedAllowed(t *testing.T) {
	reg := NewRegistry()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Custom-Header", "ok")

	cfg := map[string]any{"type": "incoming_field", "header": "X-Custom-Header"}
	v, err := reg.Resolve(context.Background(), "X-Out", cfg, req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "ok" {
		t.Errorf("Resolve = %q, want ok", v)
	}
}

// ---------------------------------------------------------------------------
// external
// ---------------------------------------------------------------------------

type fakeProvider struct {
	id, name    string
	value       string
	err         error
	validateErr error
	lastCfg     map[string]any
}

func (p *fakeProvider) Type() string         { return "external" }
func (p *fakeProvider) ProviderID() string   { return p.id }
func (p *fakeProvider) ProviderName() string { return p.name }
func (p *fakeProvider) ConfigSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}}
}
func (p *fakeProvider) Validate(cfg map[string]any) error {
	p.lastCfg = cfg
	return p.validateErr
}
func (p *fakeProvider) Resolve(_ context.Context, cfg map[string]any) (string, error) {
	p.lastCfg = cfg
	if p.err != nil {
		return "", p.err
	}
	return p.value, nil
}

var _ pkgheaders.ExternalProvider = (*fakeProvider)(nil)

// A stored credential can legitimately be multi-line (a PEM key): it is sent
// with CR/LF escaped to `\n` instead of being dropped. Static values keep
// being rejected (TestRegistry_RejectsCRLF).
func TestRegistry_External_EscapesMultiLineValue(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterExternal(&fakeProvider{id: "fake", value: "-----BEGIN-----\r\nAAAA\n-----END-----"}); err != nil {
		t.Fatalf("RegisterExternal: %v", err)
	}
	cfg := map[string]any{"type": "external", "provider": "fake", "config": map[string]any{}}
	v, err := reg.Resolve(context.Background(), "X-Pem", cfg, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := `-----BEGIN-----\nAAAA\n-----END-----`; v != want {
		t.Errorf("Resolve = %q, want %q", v, want)
	}
}

func TestRegistry_External_Dispatch(t *testing.T) {
	reg := NewRegistry()
	fp := &fakeProvider{id: "fake", name: "Fake", value: "resolved-value"}
	if err := reg.RegisterExternal(fp); err != nil {
		t.Fatalf("RegisterExternal: %v", err)
	}

	cfg := map[string]any{
		"type":     "external",
		"provider": "fake",
		"config":   map[string]any{"foo": "bar"},
	}
	v, err := reg.Resolve(context.Background(), "X-Out", cfg, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "resolved-value" {
		t.Errorf("Resolve = %q, want resolved-value", v)
	}
	// The provider must receive only the inner "config" sub-object, not
	// the wrapper with "type"/"provider".
	if fp.lastCfg["foo"] != "bar" {
		t.Errorf("provider received cfg = %v, want {foo: bar}", fp.lastCfg)
	}
	if _, ok := fp.lastCfg["type"]; ok {
		t.Error("provider should not receive the wrapper's \"type\" key")
	}
}

func TestRegistry_External_UnknownProvider(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "external", "provider": "no-such-provider"}
	if _, err := reg.Resolve(context.Background(), "X-Out", cfg, nil); err == nil {
		t.Error("Resolve with an unregistered provider succeeded, want error")
	}
}

func TestRegistry_External_MissingProviderID(t *testing.T) {
	reg := NewRegistry()
	cfg := map[string]any{"type": "external"}
	if _, err := reg.Resolve(context.Background(), "X-Out", cfg, nil); err == nil {
		t.Error("Resolve with no \"provider\" succeeded, want error")
	}
}

func TestRegistry_External_ValidateError(t *testing.T) {
	reg := NewRegistry()
	fp := &fakeProvider{id: "fake", name: "Fake", validateErr: errors.New("bad config")}
	if err := reg.RegisterExternal(fp); err != nil {
		t.Fatalf("RegisterExternal: %v", err)
	}
	cfg := map[string]any{"type": "external", "provider": "fake"}
	if _, err := reg.Resolve(context.Background(), "X-Out", cfg, nil); err == nil {
		t.Error("Resolve with a failing provider Validate succeeded, want error")
	}
}

func TestRegistry_External_ResolveError(t *testing.T) {
	reg := NewRegistry()
	fp := &fakeProvider{id: "fake", name: "Fake", err: errors.New("upstream failure")}
	if err := reg.RegisterExternal(fp); err != nil {
		t.Fatalf("RegisterExternal: %v", err)
	}
	cfg := map[string]any{"type": "external", "provider": "fake"}
	if _, err := reg.Resolve(context.Background(), "X-Out", cfg, nil); err == nil {
		t.Error("Resolve with a failing provider Resolve succeeded, want error")
	}
}

// ---------------------------------------------------------------------------
// Registry plumbing: Register/RegisterExternal/Providers
// ---------------------------------------------------------------------------

func TestRegistry_Register_Duplicate(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(staticResolver{}); err == nil {
		t.Error("Register of a duplicate type succeeded, want error")
	}
}

func TestRegistry_RegisterExternal_Duplicate(t *testing.T) {
	reg := NewRegistry()
	fp := &fakeProvider{id: "fake", name: "Fake"}
	if err := reg.RegisterExternal(fp); err != nil {
		t.Fatalf("first RegisterExternal: %v", err)
	}
	if err := reg.RegisterExternal(fp); err == nil {
		t.Error("second RegisterExternal for the same id succeeded, want error")
	}
}

func TestRegistry_Providers_SortedByID(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterExternal(&fakeProvider{id: "zeta", name: "Zeta"}); err != nil {
		t.Fatalf("RegisterExternal: %v", err)
	}
	if err := reg.RegisterExternal(&fakeProvider{id: "alpha", name: "Alpha"}); err != nil {
		t.Fatalf("RegisterExternal: %v", err)
	}

	providers := reg.Providers()
	if len(providers) != 2 {
		t.Fatalf("Providers() = %d entries, want 2", len(providers))
	}
	if providers[0].ProviderID() != "alpha" || providers[1].ProviderID() != "zeta" {
		t.Errorf("Providers() not sorted by id: %q, %q", providers[0].ProviderID(), providers[1].ProviderID())
	}
}

func TestRegistry_Providers_EmptyInitially(t *testing.T) {
	reg := NewRegistry()
	if got := reg.Providers(); len(got) != 0 {
		t.Errorf("Providers() on a fresh Registry = %d entries, want 0", len(got))
	}
}

func TestRegistry_BearerForwardingOffByDefault(t *testing.T) {
	reg := NewRegistry()
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "u", RawCredential: "gk_secret"})
	cfg := map[string]any{"type": "token_field", "field": "bearer_token"}

	v, err := reg.Resolve(ctx, "Authorization", cfg, nil)
	if !errors.Is(err, ErrBearerForwardingDisabled) || v != "" {
		t.Fatalf("Resolve = %q, %v; want ErrBearerForwardingDisabled and no value", v, err)
	}
	// Other token_field fields are unaffected.
	if _, err := reg.Resolve(ctx, "X-User", map[string]any{"type": "token_field", "field": "subject"}, nil); err != nil {
		t.Errorf("subject: %v", err)
	}

	meta := map[string]any{"headers": map[string]any{"Authorization": cfg}}
	if err := reg.CheckConnectorHeaders(meta); !errors.Is(err, ErrBearerForwardingDisabled) {
		t.Errorf("CheckConnectorHeaders = %v, want ErrBearerForwardingDisabled", err)
	}
	if err := reg.CheckConnectorHeaders(map[string]any{"headers": map[string]any{"X": map[string]any{"type": "static", "value": "v"}}}); err != nil {
		t.Errorf("static header rejected: %v", err)
	}

	reg.SetAllowBearerForwarding(true)
	if v, err := reg.Resolve(ctx, "Authorization", cfg, nil); err != nil || v != "gk_secret" {
		t.Errorf("flag on: Resolve = %q, %v", v, err)
	}
	if err := reg.CheckConnectorHeaders(meta); err != nil {
		t.Errorf("flag on: CheckConnectorHeaders = %v", err)
	}
}

// A header config that could never resolve is refused when the connector is
// saved, not silently dropped on every call.
func TestRegistry_CheckConnectorHeaders_ValidatesEachHeader(t *testing.T) {
	reg := NewRegistry()
	for name, cfg := range map[string]map[string]any{
		"denylisted": {"type": "incoming_field", "header": "Authorization"},
		"no value":   {"type": "static", "value": ""},
		"bad field":  {"type": "token_field", "field": "password"},
		"no type":    {"value": "v"},
		"unknown":    {"type": "no-such-type"},
		"CRLF value": {"type": "static", "value": "a\r\nX-Injected: 1"},
		"bad source": {"type": "incoming_field", "header": "X Trace"},
	} {
		if err := reg.CheckConnectorHeaders(map[string]any{"headers": map[string]any{"X-H": cfg}}); err == nil {
			t.Errorf("%s: CheckConnectorHeaders accepted %v", name, cfg)
		}
	}
	if err := reg.CheckConnectorHeaders(map[string]any{"headers": map[string]any{"Bad Name": map[string]any{"type": "static", "value": "v"}}}); err == nil {
		t.Error("CheckConnectorHeaders accepted an invalid header name")
	}
	ok := map[string]any{"headers": map[string]any{
		"X-Static": map[string]any{"type": "static", "value": "v"},
		"X-Sub":    map[string]any{"type": "token_field", "field": "subject"},
		"X-Trace":  map[string]any{"type": "incoming_field", "header": "X-Trace-Id"},
	}}
	if err := reg.CheckConnectorHeaders(ok); err != nil {
		t.Errorf("valid headers rejected: %v", err)
	}
	if err := (*Registry)(nil).CheckConnectorHeaders(ok); err != nil {
		t.Errorf("nil registry: %v", err)
	}
}

// Two bad headers always report the same one (names are checked in order),
// and a headers value that is not an object is refused, not ignored.
func TestRegistry_CheckConnectorHeaders_DeterministicAndShape(t *testing.T) {
	reg := NewRegistry()
	meta := map[string]any{"headers": map[string]any{
		"A-Fwd": map[string]any{"type": "token_field", "field": "bearer_token"},
		"B-Bad": map[string]any{"type": "nope"},
	}}
	for i := 0; i < 50; i++ {
		if err := reg.CheckConnectorHeaders(meta); !errors.Is(err, ErrBearerForwardingDisabled) {
			t.Fatalf("run %d: err = %v, want the first header's ErrBearerForwardingDisabled", i, err)
		}
	}
	if err := reg.CheckConnectorHeaders(map[string]any{"headers": "Authorization: Bearer x"}); err == nil {
		t.Error("a string headers value was accepted")
	}
}

// A credential pasted with a trailing newline is sent without it; only line
// breaks inside the value (a PEM key) are escaped.
func TestRegistry_External_TrimsTrailingNewline(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterExternal(&fakeProvider{id: "fake", value: "tok\n"}); err != nil {
		t.Fatal(err)
	}
	v, err := reg.Resolve(context.Background(), "X", map[string]any{"type": "external", "provider": "fake", "config": map[string]any{}, "prefix": "Bearer "}, nil)
	if err != nil || v != "Bearer tok" {
		t.Errorf("Resolve = %q, %v; want \"Bearer tok\"", v, err)
	}
}
