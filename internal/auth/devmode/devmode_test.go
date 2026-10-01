package devmode

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

func TestDevPrincipalIsTenantAdminUnlessPlatform(t *testing.T) {
	authz := auth.NewRoleAuthorizer()
	req := httptest.NewRequest("GET", "/x", nil)

	p, err := New("tenant-1", false, nil).Authenticate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !authz.Allow(context.Background(), p, "connector.create") {
		t.Error("dev principal should be a tenant admin")
	}
	if authz.Allow(context.Background(), p, auth.PermPlatformAdmin) {
		t.Error("dev principal must not hold platform.admin by default")
	}

	p, _ = New("tenant-1", true, nil).Authenticate(context.Background(), req)
	if !authz.Allow(context.Background(), p, auth.PermPlatformAdmin) {
		t.Error("auth.dev_mode.platform should grant platform.admin")
	}

	// A request carrying any credential is left to the real authenticators.
	req.Header.Set("Authorization", "Bearer nope")
	if _, err := New("tenant-1", false, nil).Authenticate(context.Background(), req); err != auth.ErrNoCredential {
		t.Errorf("err = %v, want ErrNoCredential", err)
	}
}
