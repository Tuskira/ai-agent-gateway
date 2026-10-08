package dataplane_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	// Registers the "redis" session driver, for the gated test below.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/redis"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// agentAuth accepts every request as the same agent principal.
type agentAuth struct{}

func (agentAuth) Name() string { return "agent" }
func (agentAuth) Authenticate(context.Context, *http.Request) (*pkgauth.Principal, error) {
	return &pkgauth.Principal{Subject: "key-1", TenantID: opsTenantID, Roles: []string{"agent"}, AuthMethod: "apikey"}, nil
}

func newCountPlane(t *testing.T, mutate func(*config.Config)) *dataplane.Plane {
	t.Helper()
	st := dptest.New()
	if err := st.Tenants().Create(context.Background(), &store.Tenant{ID: opsTenantID, Slug: "ops", Name: "Ops"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Service.Version = "test"
	if mutate != nil {
		mutate(cfg)
	}
	p, err := dataplane.New(dataplane.Deps{
		Config:        cfg,
		Store:         st,
		Headers:       headers.NewRegistry(),
		Authenticator: agentAuth{},
		Authorizer:    pkgauth.NewRoleAuthorizer(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// initialize mints one session through the plane's real HTTP handler.
func initialize(t *testing.T, p *dataplane.Plane) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	p.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get(mcp.HeaderSessionID) == "" {
		t.Fatalf("initialize = %d, session %q, body %s", rec.Code, rec.Header().Get(mcp.HeaderSessionID), rec.Body.String())
	}
}

// With the memory driver both numbers follow initialize calls.
func TestPlaneSessionCountsMemory(t *testing.T) {
	p := newCountPlane(t, nil)
	if p.SessionCount() != 0 || p.SessionsCreated() != 0 {
		t.Fatalf("fresh plane: active %d created %d", p.SessionCount(), p.SessionsCreated())
	}
	initialize(t, p)
	initialize(t, p)
	if p.SessionCount() != 2 || p.SessionsCreated() != 2 {
		t.Errorf("after two initializes: active %d created %d, want 2 / 2", p.SessionCount(), p.SessionsCreated())
	}
}

// Redis cannot count its keys cheaply: SessionCount says so with -1 (the
// collector then omits the gauge), while SessionsCreated still counts.
func TestPlaneSessionCountsRedis(t *testing.T) {
	addr := os.Getenv("GATEWAY_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("GATEWAY_TEST_REDIS_ADDR not set; skipping the redis session count test")
	}
	p := newCountPlane(t, func(c *config.Config) {
		c.Sessions.Store = "redis"
		c.Redis.Enabled = true
		c.Redis.Addr = addr
	})
	initialize(t, p)
	if got := p.SessionCount(); got != -1 {
		t.Errorf("SessionCount() with redis = %d, want -1", got)
	}
	if got := p.SessionsCreated(); got != 1 {
		t.Errorf("SessionsCreated() = %d, want 1", got)
	}
}
