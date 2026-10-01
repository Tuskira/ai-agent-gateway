package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// ---------------------------------------------------------------------------
// secret_store provider
// ---------------------------------------------------------------------------

func TestSecretStoreProvider_ResolvesFromCallersTenant(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "tenant-a", "okta-prod", "secret_store", map[string]string{"client_secret": "s3cr3t"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	p := NewSecretStoreProvider(svc)
	cfg := map[string]any{"credential": "okta-prod", "field": "client_secret"}
	if err := p.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	reqCtx := auth.WithPrincipal(ctx, &auth.Principal{Subject: "u1", TenantID: "tenant-a"})
	v, err := p.Resolve(reqCtx, cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "s3cr3t" {
		t.Errorf("Resolve = %q, want s3cr3t", v)
	}
}

func TestSecretStoreProvider_CrossTenantDenied(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "tenant-a", "okta-prod", "secret_store", map[string]string{"client_secret": "s3cr3t"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	p := NewSecretStoreProvider(svc)
	cfg := map[string]any{"credential": "okta-prod", "field": "client_secret"}

	// Principal belongs to tenant-b; the config only names the credential,
	// never a tenant -- Resolve must use the caller's own tenant, so this
	// must NOT be able to read tenant-a's credential.
	reqCtx := auth.WithPrincipal(ctx, &auth.Principal{Subject: "u2", TenantID: "tenant-b"})
	if _, err := p.Resolve(reqCtx, cfg); err == nil {
		t.Error("Resolve leaked another tenant's credential, want error")
	}
}

func TestSecretStoreProvider_NoPrincipal(t *testing.T) {
	svc, _ := newTestService(t)
	p := NewSecretStoreProvider(svc)
	cfg := map[string]any{"credential": "okta-prod", "field": "client_secret"}
	if _, err := p.Resolve(context.Background(), cfg); err == nil {
		t.Error("Resolve with no principal in context succeeded, want error")
	}
}

func TestSecretStoreProvider_Validate_MissingFields(t *testing.T) {
	svc, _ := newTestService(t)
	p := NewSecretStoreProvider(svc)

	cases := []map[string]any{
		{},
		{"credential": "okta-prod"},
		{"field": "client_secret"},
	}
	for _, cfg := range cases {
		if err := p.Validate(cfg); err == nil {
			t.Errorf("Validate(%v) succeeded, want error", cfg)
		}
	}
}

func TestSecretStoreProvider_ConfigSchema(t *testing.T) {
	svc, _ := newTestService(t)
	p := NewSecretStoreProvider(svc)
	schema := p.ConfigSchema()
	if _, ok := schema["properties"]; !ok {
		t.Error("ConfigSchema missing \"properties\"")
	}
	if _, ok := schema["required"]; !ok {
		t.Error("ConfigSchema missing \"required\"")
	}
}

func TestSecretStoreProvider_Invalidate(t *testing.T) {
	svc, cs := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "tenant-a", "okta-prod", "secret_store", map[string]string{"client_secret": "s3cr3t"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	p := NewSecretStoreProvider(svc)
	reqCtx := auth.WithPrincipal(ctx, &auth.Principal{Subject: "u1", TenantID: "tenant-a"})

	if _, err := p.Resolve(reqCtx, map[string]any{"credential": "okta-prod", "field": "client_secret"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Mutate the underlying row directly, bypassing Service (so no
	// automatic invalidation), then confirm the provider's Invalidate
	// evicts the stale cached value.
	row, err := cs.Get(ctx, "tenant-a", "okta-prod")
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	ciphertext, nonce, err := svc.ring.Encrypt(svc.ring.ActiveKeyID, []byte(`{"client_secret":"changed"}`))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := cs.Rotate(ctx, "tenant-a", "okta-prod", ciphertext, nonce, row.KeyID, row.FieldNames); err != nil {
		t.Fatalf("raw Rotate: %v", err)
	}

	p.Invalidate(reqCtx, "u1", "okta-connector")

	v, err := p.Resolve(reqCtx, map[string]any{"credential": "okta-prod", "field": "client_secret"})
	if err != nil {
		t.Fatalf("Resolve after Invalidate: %v", err)
	}
	if v != "changed" {
		t.Errorf("Resolve after Invalidate = %q, want fresh value \"changed\"", v)
	}
}

// ---------------------------------------------------------------------------
// env provider
// ---------------------------------------------------------------------------

func TestEnvProvider_DisabledByDefault(t *testing.T) {
	p := NewEnvProvider(false)
	cfg := map[string]any{"var": "SOME_VAR"}
	if err := p.Validate(cfg); err == nil {
		t.Error("Validate on disabled env provider succeeded, want error")
	}
	if _, err := p.Resolve(context.Background(), cfg); err == nil {
		t.Error("Resolve on disabled env provider succeeded, want error")
	}
}

func TestEnvProvider_EnabledResolves(t *testing.T) {
	t.Setenv("GATEWAY_TEST_ENV_PROVIDER_VAR", "hello")
	p := NewEnvProvider(true)
	cfg := map[string]any{"var": "GATEWAY_TEST_ENV_PROVIDER_VAR"}
	if err := p.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	v, err := p.Resolve(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "hello" {
		t.Errorf("Resolve = %q, want hello", v)
	}
}

func TestEnvProvider_MissingVar(t *testing.T) {
	p := NewEnvProvider(true)
	cfg := map[string]any{"var": "GATEWAY_TEST_ENV_PROVIDER_DOES_NOT_EXIST"}
	if _, err := p.Resolve(context.Background(), cfg); err == nil {
		t.Error("Resolve of an unset env var succeeded, want error")
	}
}

func TestEnvProvider_ValidateMissingArg(t *testing.T) {
	p := NewEnvProvider(true)
	if err := p.Validate(map[string]any{}); err == nil {
		t.Error("Validate with no \"var\" succeeded, want error")
	}
}

// ---------------------------------------------------------------------------
// file provider
// ---------------------------------------------------------------------------

func TestFileProvider_DisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("s3cr3t"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	p := NewFileProvider(false, dir)
	cfg := map[string]any{"path": path}
	if err := p.Validate(cfg); err == nil {
		t.Error("Validate on disabled file provider succeeded, want error")
	}
	if _, err := p.Resolve(context.Background(), cfg); err == nil {
		t.Error("Resolve on disabled file provider succeeded, want error")
	}
}

func TestFileProvider_PlainTextWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	p := NewFileProvider(true, dir)
	cfg := map[string]any{"path": path}
	if err := p.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	v, err := p.Resolve(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "s3cr3t" {
		t.Errorf("Resolve = %q, want trimmed \"s3cr3t\"", v)
	}
}

func TestFileProvider_JSONField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(path, []byte(`{"client_secret":"s3cr3t","client_id":"abc"}`), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	p := NewFileProvider(true, dir)
	cfg := map[string]any{"path": path, "field": "client_secret"}
	v, err := p.Resolve(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "s3cr3t" {
		t.Errorf("Resolve = %q, want s3cr3t", v)
	}
}

func TestFileProvider_JSONFieldMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(path, []byte(`{"client_id":"abc"}`), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	p := NewFileProvider(true, dir)
	cfg := map[string]any{"path": path, "field": "client_secret"}
	if _, err := p.Resolve(context.Background(), cfg); err == nil {
		t.Error("Resolve of a missing JSON field succeeded, want error")
	}
}

func TestFileProvider_PathEscapeDotDotRejected(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("s3cr3t"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	p := NewFileProvider(true, root)
	cfg := map[string]any{"path": filepath.Join(root, "..", "outside.txt")}

	err := p.Validate(cfg)
	if err == nil {
		t.Error("Validate of a \"..\" path escape succeeded, want error")
	} else if strings.Contains(err.Error(), dir) {
		// The error reaches API callers when a connector is saved; it must
		// not echo the server's root or the (read-masked) stored path.
		t.Errorf("Validate error leaks a server path: %v", err)
	}
	if _, err := p.Resolve(context.Background(), cfg); err == nil {
		t.Error("Resolve of a \"..\" path escape succeeded, want error")
	}
}

func TestFileProvider_SymlinkEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("s3cr3t"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	p := NewFileProvider(true, root)
	cfg := map[string]any{"path": link}

	// Validate is a lexical check only (no symlink following, so the
	// file need not exist for a save-time check) and will pass here since
	// "escape.txt" is lexically under root; Resolve must still refuse it
	// once it follows the symlink.
	if _, err := p.Resolve(context.Background(), cfg); err == nil {
		t.Error("Resolve of a symlink escaping the root succeeded, want error")
	}
}

func TestFileProvider_SymlinkWithinRootAllowed(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	real := filepath.Join(root, "real.txt")
	if err := os.WriteFile(real, []byte("s3cr3t"), 0o600); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	p := NewFileProvider(true, root)
	v, err := p.Resolve(context.Background(), map[string]any{"path": link})
	if err != nil {
		t.Fatalf("Resolve of an in-root symlink failed: %v", err)
	}
	if v != "s3cr3t" {
		t.Errorf("Resolve = %q, want s3cr3t", v)
	}
}

func TestFileProvider_RootNotConfigured(t *testing.T) {
	p := NewFileProvider(true, "")
	cfg := map[string]any{"path": "/tmp/whatever"}
	if err := p.Validate(cfg); err == nil {
		t.Error("Validate with no root configured succeeded, want error")
	}
}

func TestFileProvider_ConfigSchema(t *testing.T) {
	p := NewFileProvider(true, "/root")
	schema := p.ConfigSchema()
	if _, ok := schema["properties"]; !ok {
		t.Error("ConfigSchema missing \"properties\"")
	}
}

func TestFileProvider_ErrorsAreDistinguishable(t *testing.T) {
	// Sanity check that our sentinel-ish errors aren't accidentally
	// identical across the two gates (helps catch a copy/paste bug).
	if errors.Is(errEnvProviderDisabled, errFileProviderDisabled) {
		t.Error("env and file disabled errors must be distinct")
	}
}
