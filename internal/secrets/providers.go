package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
)

// ---------------------------------------------------------------------------
// secret_store provider
// ---------------------------------------------------------------------------

// SecretStoreProvider is the "secret_store" headers.ExternalProvider: it
// resolves a header value from a credential already held in this
// gateway's own encrypted store (Service). Config:
// {"credential": "<name>", "field": "<field>"}.
//
// The tenant is never taken from config -- it is always the caller's own
// tenant, read from auth.PrincipalFrom(ctx) -- so a shared connector
// definition can never be used to read a different tenant's credential.
type SecretStoreProvider struct {
	svc *Service
}

var (
	_ headers.ExternalProvider = (*SecretStoreProvider)(nil)
	_ headers.Invalidator      = (*SecretStoreProvider)(nil)
)

// NewSecretStoreProvider builds the "secret_store" provider over svc.
func NewSecretStoreProvider(svc *Service) *SecretStoreProvider {
	return &SecretStoreProvider{svc: svc}
}

func (p *SecretStoreProvider) ProviderID() string   { return "secret_store" }
func (p *SecretStoreProvider) ProviderName() string { return "Secret Store" }
func (p *SecretStoreProvider) Type() string         { return "external" }

func (p *SecretStoreProvider) ConfigSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"credential": map[string]any{
				"type":        "string",
				"description": "Name of the credential in this gateway's secret store.",
			},
			"field": map[string]any{
				"type":        "string",
				"description": "Field within the credential's decrypted payload to return.",
			},
		},
		"required": []string{"credential", "field"},
	}
}

func (p *SecretStoreProvider) Validate(cfg map[string]any) error {
	_, _, err := secretStoreArgs(cfg)
	return err
}

func (p *SecretStoreProvider) Resolve(ctx context.Context, cfg map[string]any) (string, error) {
	credential, field, err := secretStoreArgs(cfg)
	if err != nil {
		return "", err
	}

	principal, ok := auth.PrincipalFrom(ctx)
	if !ok || principal == nil {
		return "", errors.New("secret_store: no authenticated principal in context")
	}

	return p.svc.Fetcher()(ctx, principal.TenantID, credential, field)
}

// Invalidate evicts every cached field for the calling principal's tenant.
// It is a coarse, best-effort flush: the Invalidator contract (see
// headers.Invalidator) is not told which credential backs connectorSlug,
// only that something did fail for principalID against it, so the safest
// correct action is to drop the whole tenant's cache rather than risk
// leaving a stale value behind. principalID/connectorSlug are logged for
// correlation; the tenant itself is read from ctx (best effort -- if the
// caller didn't thread the original request's Principal through, this is
// a no-op).
func (p *SecretStoreProvider) Invalidate(ctx context.Context, principalID, connectorSlug string) {
	principal, ok := auth.PrincipalFrom(ctx)
	if !ok || principal == nil {
		slog.Default().Warn("secret_store: invalidate skipped, no principal in context",
			"principal_id", principalID, "connector", connectorSlug)
		return
	}
	p.svc.InvalidateTenant(principal.TenantID)
}

func secretStoreArgs(cfg map[string]any) (credential, field string, err error) {
	credential, _ = cfg["credential"].(string)
	field, _ = cfg["field"].(string)
	if credential == "" {
		return "", "", errors.New("secret_store: \"credential\" is required")
	}
	if field == "" {
		return "", "", errors.New("secret_store: \"field\" is required")
	}
	return credential, field, nil
}

// ---------------------------------------------------------------------------
// env provider
// ---------------------------------------------------------------------------

// EnvProvider is the "env" headers.ExternalProvider: it resolves a header
// value from the gateway process's own environment. Config:
// {"var": "<name>"}.
//
// Disabled by default (secret_store.allow_env_provider). In a shared
// deployment, enabling this would let any tenant admin who can configure a
// connector's headers read ANY environment variable visible to the
// process -- including GATEWAY_MASTER_KEY, the database password, or a
// cloud instance-metadata token -- simply by pointing a header at it. Only
// enable it on a single-tenant deployment, or one where every tenant is
// already trusted with the host environment.
type EnvProvider struct {
	enabled bool
}

var _ headers.ExternalProvider = (*EnvProvider)(nil)

// NewEnvProvider builds the "env" provider; it refuses to Validate/Resolve
// unless enabled is true (secret_store.allow_env_provider).
func NewEnvProvider(enabled bool) *EnvProvider {
	return &EnvProvider{enabled: enabled}
}

func (p *EnvProvider) ProviderID() string   { return "env" }
func (p *EnvProvider) ProviderName() string { return "Environment Variable" }
func (p *EnvProvider) Type() string         { return "external" }

func (p *EnvProvider) ConfigSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"var": map[string]any{
				"type":        "string",
				"description": "Name of the environment variable to read.",
			},
		},
		"required": []string{"var"},
	}
}

func (p *EnvProvider) Validate(cfg map[string]any) error {
	if !p.enabled {
		return errEnvProviderDisabled
	}
	_, err := envArg(cfg)
	return err
}

func (p *EnvProvider) Resolve(_ context.Context, cfg map[string]any) (string, error) {
	if !p.enabled {
		return "", errEnvProviderDisabled
	}
	name, err := envArg(cfg)
	if err != nil {
		return "", err
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("env: variable %q is not set", name)
	}
	return v, nil
}

var errEnvProviderDisabled = errors.New("env: provider disabled (set secret_store.allow_env_provider: true to enable)")

func envArg(cfg map[string]any) (string, error) {
	v, _ := cfg["var"].(string)
	if v == "" {
		return "", errors.New("env: \"var\" is required")
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// file provider
// ---------------------------------------------------------------------------

// FileProvider is the "file" headers.ExternalProvider: it resolves a
// header value from a file on the gateway's local filesystem -- either the
// whole file (plain text) or one field of it (JSON). Config:
// {"path": "/secrets/okta", "field": "client_secret"}; field is optional
// for a plain-text file and required for a JSON one.
//
// Disabled by default (secret_store.allow_file_provider). When enabled,
// every configured path is required to resolve under
// secret_store.file_provider_root: ".." components are rejected lexically,
// and the fully-resolved path (after following symlinks, via
// filepath.EvalSymlinks) is re-checked against the root, so a symlink
// planted under root cannot be used to read a file outside it (e.g.
// /etc/shadow, or a mounted Kubernetes service-account token).
type FileProvider struct {
	enabled bool
	root    string
}

var _ headers.ExternalProvider = (*FileProvider)(nil)

// NewFileProvider builds the "file" provider; it refuses to Validate/
// Resolve unless enabled is true (secret_store.allow_file_provider), and
// every path must resolve under root (secret_store.file_provider_root).
func NewFileProvider(enabled bool, root string) *FileProvider {
	return &FileProvider{enabled: enabled, root: root}
}

func (p *FileProvider) ProviderID() string   { return "file" }
func (p *FileProvider) ProviderName() string { return "File" }
func (p *FileProvider) Type() string         { return "external" }

func (p *FileProvider) ConfigSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the secret file; must resolve under secret_store.file_provider_root.",
			},
			"field": map[string]any{
				"type":        "string",
				"description": "Optional: JSON field to extract. Omit to read the whole file as plain text.",
			},
		},
		"required": []string{"path"},
	}
}

func (p *FileProvider) Validate(cfg map[string]any) error {
	if !p.enabled {
		return errFileProviderDisabled
	}
	path, _, err := fileArgs(cfg)
	if err != nil {
		return err
	}
	if p.root == "" {
		return errFileProviderRootUnset
	}
	_, err = cleanUnderRoot(p.root, path)
	return err
}

func (p *FileProvider) Resolve(_ context.Context, cfg map[string]any) (string, error) {
	if !p.enabled {
		return "", errFileProviderDisabled
	}
	path, field, err := fileArgs(cfg)
	if err != nil {
		return "", err
	}

	resolved, err := p.resolvePath(path)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("file: read %q: %w", path, err)
	}

	if field == "" {
		return strings.TrimSpace(string(data)), nil
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("file: %q is not valid JSON (field %q was requested): %w", path, field, err)
	}
	v, ok := parsed[field]
	if !ok {
		return "", fmt.Errorf("file: field %q not present in %q", field, path)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("file: field %q in %q is not a string", field, path)
	}
	return s, nil
}

var (
	errFileProviderDisabled  = errors.New("file: provider disabled (set secret_store.allow_file_provider: true to enable)")
	errFileProviderRootUnset = errors.New("file: secret_store.file_provider_root is not configured")
)

func fileArgs(cfg map[string]any) (path, field string, err error) {
	path, _ = cfg["path"].(string)
	field, _ = cfg["field"].(string)
	if path == "" {
		return "", "", errors.New("file: \"path\" is required")
	}
	return path, field, nil
}

// resolvePath resolves path under p.root, following symlinks (which
// requires the file to exist), and re-verifies containment after
// resolution. It returns the final, real, absolute path.
func (p *FileProvider) resolvePath(path string) (string, error) {
	if p.root == "" {
		return "", errFileProviderRootUnset
	}
	abs, err := cleanUnderRoot(p.root, path)
	if err != nil {
		return "", err
	}

	rootAbs, err := filepath.Abs(p.root)
	if err != nil {
		return "", fmt.Errorf("file: resolve root %q: %w", p.root, err)
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("file: resolve root %q: %w", p.root, err)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("file: resolve %q: %w", path, err)
	}
	if !isUnderDir(rootReal, resolved) {
		return "", fmt.Errorf("file: path %q resolves (via symlink) outside secret_store.file_provider_root %q", path, p.root)
	}
	return resolved, nil
}

// cleanUnderRoot lexically (no filesystem access, no symlink following)
// resolves path against root and rejects it if it escapes root via "..".
// A relative path is joined onto root; an absolute one is used as-is (and
// must still fall under root).
func cleanUnderRoot(root, path string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("file: resolve root %q: %w", root, err)
	}

	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(rootAbs, abs)
	}
	abs = filepath.Clean(abs)

	if !isUnderDir(rootAbs, abs) {
		return "", errors.New("file: path escapes secret_store.file_provider_root")
	}
	return abs, nil
}

// isUnderDir reports whether target is dir itself or lexically inside it.
func isUnderDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}
