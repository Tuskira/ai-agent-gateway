package headers

import (
	"context"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// Supported token_field field names.
const (
	tokenFieldEmail       = "email"
	tokenFieldSubject     = "subject"
	tokenFieldTenantID    = "tenant_id"
	tokenFieldBearerToken = "bearer_token"
)

// tokenFieldResolver is the "token_field" built-in HeaderResolver: it
// returns one field of the caller's authenticated Principal (see
// pkg/auth), e.g. to forward the caller's own email or bearer token
// through to a backend connector. Config:
// {"type": "token_field", "field": "email|subject|tenant_id|bearer_token"}.
type tokenFieldResolver struct{}

func (tokenFieldResolver) Type() string { return "token_field" }

func (tokenFieldResolver) Validate(cfg map[string]any) error {
	field, _ := cfg["field"].(string)
	switch field {
	case tokenFieldEmail, tokenFieldSubject, tokenFieldTenantID, tokenFieldBearerToken:
		return nil
	default:
		return fmt.Errorf("token_field: unsupported \"field\" %q (want one of %s, %s, %s, %s)",
			field, tokenFieldEmail, tokenFieldSubject, tokenFieldTenantID, tokenFieldBearerToken)
	}
}

func (tokenFieldResolver) Resolve(ctx context.Context, cfg map[string]any) (string, error) {
	principal, ok := auth.PrincipalFrom(ctx)
	if !ok || principal == nil {
		return "", fmt.Errorf("token_field: no authenticated principal in context")
	}

	field, _ := cfg["field"].(string)
	switch field {
	case tokenFieldEmail:
		if principal.Email == "" {
			return "", fmt.Errorf("token_field: principal has no email")
		}
		return principal.Email, nil
	case tokenFieldSubject:
		if principal.Subject == "" {
			return "", fmt.Errorf("token_field: principal has no subject")
		}
		return principal.Subject, nil
	case tokenFieldTenantID:
		if principal.TenantID == "" {
			return "", fmt.Errorf("token_field: principal has no tenant_id")
		}
		return principal.TenantID, nil
	case tokenFieldBearerToken:
		// bearer_token forwards the caller's own raw credential.
		if principal.RawCredential == "" {
			return "", fmt.Errorf("token_field: principal has no raw credential")
		}
		return principal.RawCredential, nil
	default:
		// Unreachable in practice: Registry.Resolve always calls
		// Validate first, which rejects every other field value.
		return "", fmt.Errorf("token_field: unsupported field %q", field)
	}
}
