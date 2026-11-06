package oidc

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
)

// Identity is a verified ID token's subject, as the handler needs it.
type Identity struct {
	Provider string
	Issuer   string
	Subject  string
	// Email is lowercased and passed the provider's domain rule.
	Email         string
	EmailVerified bool
	// Name is the token's "name" claim (display name for a new user).
	Name string
}

// maxIssuedAtSkew is how far in the future an ID token's iat may be.
const maxIssuedAtSkew = 5 * time.Minute

// idClaims are the claims read beyond what go-oidc verifies. The email
// itself is read from the provider's email_claim (see emailClaim).
type idClaims struct {
	EmailVerified json.RawMessage `json:"email_verified"`
	Name          string          `json:"name"`
	TokenUse      string          `json:"token_use"`
	AZP           string          `json:"azp"`
	Identities    json.RawMessage `json:"identities"`
}

// checkToken applies the checks go-oidc leaves to the caller (nonce, iat,
// azp, Cognito's token_use) and then the provider's identity rules. go-oidc
// has already verified the signature against the issuer's JWKS (with the
// asymmetric algorithms in signingAlgs only), iss, aud and exp.
func (s *Service) checkToken(p *Provider, t *gooidc.IDToken, wantNonce string) (*Identity, error) {
	if t.Nonce == "" || subtle.ConstantTimeCompare([]byte(t.Nonce), []byte(wantNonce)) != 1 {
		return nil, fail(CodeToken, "nonce does not match")
	}
	if t.IssuedAt.IsZero() || t.IssuedAt.After(s.now().Add(maxIssuedAtSkew)) {
		return nil, fail(CodeToken, "iat missing or in the future (%v)", t.IssuedAt)
	}
	if t.Subject == "" {
		return nil, fail(CodeToken, "no sub")
	}
	var c idClaims
	if err := t.Claims(&c); err != nil {
		return nil, fail(CodeToken, "decode claims: %v", err)
	}
	if c.TokenUse != "" && c.TokenUse != "id" {
		return nil, fail(CodeToken, "token_use is %q, not id", c.TokenUse)
	}
	if len(t.Audience) > 1 && c.AZP != p.cfg.ClientID {
		return nil, fail(CodeToken, "several audiences and azp is not the client id")
	}

	if want := p.cfg.RequiredUpstreamIdP; want != "" {
		if err := checkUpstream(c.Identities, want); err != nil {
			return nil, &Error{Code: CodeIdP, Err: err}
		}
	}
	raw, err := emailClaim(t, p.cfg.EmailClaimName())
	if err != nil {
		return nil, &Error{Code: CodeDomain, Err: err}
	}
	email, err := NormalizeEmail(raw, p.domains)
	if err != nil {
		return nil, &Error{Code: CodeDomain, Err: err}
	}
	verified := emailVerified(c.EmailVerified)
	if !verified && p.cfg.VerifiedEmailRequired() {
		return nil, fail(CodeUnverified, "email_verified is not true")
	}
	return &Identity{
		Provider: p.Name, Issuer: p.Issuer, Subject: t.Subject,
		Email: email, EmailVerified: verified, Name: c.Name,
	}, nil
}

// emailClaim reads the string claim name from the token; absent is "",
// any non-string value is an error.
func emailClaim(t *gooidc.IDToken, name string) (string, error) {
	var all map[string]json.RawMessage
	if err := t.Claims(&all); err != nil {
		return "", fmt.Errorf("decode claims: %w", err)
	}
	raw, ok := all[name]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("claim %q is not a string", name)
	}
	return s, nil
}

// emailVerified reads email_verified: JSON true, or the string "true"
// (some issuers, Cognito among them, send it as a string). Anything else,
// including absence, is false.
func emailVerified(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.EqualFold(s, "true")
	}
	return false
}

// NormalizeEmail lowercases an email claim and checks it: printable ASCII
// with no spaces, exactly one "@" with a non-empty local part, at most 254
// bytes, and a domain that is exactly one of allowed (already lowercase).
// Subdomains, a trailing dot and IDN look-alikes therefore never match.
func NormalizeEmail(raw string, allowed map[string]bool) (string, error) {
	if raw == "" {
		return "", errors.New("no email claim")
	}
	if len(raw) > 254 {
		return "", errors.New("email longer than 254 bytes")
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= 0x20 || c >= 0x7f {
			return "", errors.New("email contains a space, control or non-ASCII character")
		}
	}
	email := strings.ToLower(raw)
	if strings.Count(email, "@") != 1 {
		return "", errors.New("email must contain exactly one @")
	}
	local, domain, _ := strings.Cut(email, "@")
	if local == "" || domain == "" {
		return "", errors.New("email has an empty local part or domain")
	}
	if !allowed[domain] {
		return "", fmt.Errorf("email domain %q is not allowed", domain)
	}
	return email, nil
}

// identityEntry is one element of Cognito's "identities" claim.
type identityEntry struct {
	ProviderName string `json:"providerName"`
}

// checkUpstream enforces required_upstream_idp on Cognito's "identities"
// claim, which is a JSON array of the user's federated identities -- or,
// from some token paths, that array JSON-encoded inside a string. The
// claim must be present, non-empty, and every entry must name want: a
// user linked to several upstream IdPs is refused, since the token does
// not say which one this login used.
func checkUpstream(raw json.RawMessage, want string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("no identities claim")
	}
	var entries []identityEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		var inner string
		if err2 := json.Unmarshal(raw, &inner); err2 != nil {
			return errors.New("identities claim is neither an array nor a string")
		}
		if err := json.Unmarshal([]byte(inner), &entries); err != nil {
			return errors.New("identities claim string does not hold a JSON array")
		}
	}
	if len(entries) == 0 {
		return errors.New("identities claim is empty")
	}
	for _, e := range entries {
		if e.ProviderName != want {
			return fmt.Errorf("identities names upstream provider %q", e.ProviderName)
		}
	}
	return nil
}
