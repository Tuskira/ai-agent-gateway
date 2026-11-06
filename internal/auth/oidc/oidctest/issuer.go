// Package oidctest is a local OpenID Connect issuer for unit tests: a real
// HTTP server (httptest) with a freshly generated RSA key, serving
// discovery, JWKS and a token endpoint that enforces the authorization
// code, the redirect URI, the client secret and PKCE (S256). It stands in
// for no particular vendor; the end-to-end test (test/e2e) runs against a
// real Keycloak instead.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Issuer is the test issuer. Set Claims or Alg before an exchange to shape
// the ID token it returns.
type Issuer struct {
	URL          string
	ClientID     string
	ClientSecret string

	mu  sync.Mutex
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
	// published is the key the JWKS serves (normally key).
	published *rsa.PrivateKey
	codes     map[string]authRequest
	// Claims, when set, edits the ID token claims of the next exchanges
	// (defaults: iss, aud, sub, email, email_verified, nonce, iat, exp).
	Claims func(c map[string]any)
	// Alg is the ID token's signing algorithm: "" or "RS256" (default),
	// "HS256" (signed with the client secret), or "none" (unsigned).
	Alg string
	// JWKSHits counts JWKS fetches.
	JWKSHits int
}

type authRequest struct {
	redirectURI, challenge, nonce, subject, email string
}

// New starts an issuer; it is closed when the test ends.
func New(t testing.TB) *Issuer {
	t.Helper()
	i := &Issuer{ClientID: "gateway-test", ClientSecret: "test-client-secret", codes: map[string]authRequest{}}
	i.rotate()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", i.discovery)
	mux.HandleFunc("GET /jwks", i.jwks)
	mux.HandleFunc("POST /token", i.token)
	i.srv = httptest.NewServer(mux)
	i.URL = i.srv.URL
	t.Cleanup(i.srv.Close)
	return i
}

func (i *Issuer) rotate() {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	i.key, i.published, i.kid = k, k, fmt.Sprintf("k-%x", b)
}

// RotateKey switches to a new signing key (published at once).
func (i *Issuer) RotateKey() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rotate()
}

func (i *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                i.URL,
		"authorization_endpoint":                i.URL + "/authorize",
		"token_endpoint":                        i.URL + "/token",
		"jwks_uri":                              i.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256", "HS256", "none"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
	})
}

func (i *Issuer) jwks(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	i.JWKSHits++
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &i.published.PublicKey, KeyID: i.kid, Algorithm: "RS256", Use: "sig"}}}
	i.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}

// Authorize plays the user approving the login at authURL (what the
// gateway's start route redirected to) as subject/email, and returns the
// callback query the IdP would redirect back with. It checks the request
// is a code flow with S256 PKCE, a state and a nonce for this client.
func (i *Issuer) Authorize(t testing.TB, authURL, subject, email string) url.Values {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != i.URL+"/authorize" {
		t.Fatalf("authorize endpoint = %s, want %s/authorize", got, i.URL)
	}
	q := u.Query()
	for k, want := range map[string]string{"response_type": "code", "client_id": i.ClientID, "code_challenge_method": "S256"} {
		if q.Get(k) != want {
			t.Fatalf("authorize %s = %q, want %q", k, q.Get(k), want)
		}
	}
	for _, k := range []string{"state", "nonce", "code_challenge", "redirect_uri", "scope"} {
		if q.Get(k) == "" {
			t.Fatalf("authorize request has no %s", k)
		}
	}
	code := randomCode()
	i.mu.Lock()
	i.codes[code] = authRequest{redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), subject: subject, email: email}
	i.mu.Unlock()
	return url.Values{"code": {code}, "state": {q.Get("state")}}
}

func randomCode() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != i.ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(i.ClientSecret)) != 1 {
		tokenError(w, "invalid_client")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		tokenError(w, "unsupported_grant_type")
		return
	}
	i.mu.Lock()
	req, ok := i.codes[r.PostForm.Get("code")]
	delete(i.codes, r.PostForm.Get("code")) // single use
	i.mu.Unlock()
	if !ok || req.redirectURI != r.PostForm.Get("redirect_uri") {
		tokenError(w, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != req.challenge {
		tokenError(w, "invalid_grant")
		return
	}

	now := time.Now()
	claims := map[string]any{
		"iss": i.URL, "aud": i.ClientID, "sub": req.subject, "email": req.email, "email_verified": true,
		"nonce": req.nonce, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
	i.mu.Lock()
	edit, alg, key, kid := i.Claims, i.Alg, i.key, i.kid
	i.mu.Unlock()
	if edit != nil {
		edit(claims)
	}
	idToken, err := sign(claims, alg, key, kid, i.ClientSecret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at-" + randomCode(), "token_type": "Bearer", "expires_in": 300, "id_token": idToken,
	})
}

// sign serializes claims as a compact JWS.
func sign(claims map[string]any, alg string, key *rsa.PrivateKey, kid, secret string) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	switch strings.ToUpper(alg) {
	case "", "RS256":
		s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kid}},
			(&jose.SignerOptions{}).WithType("JWT"))
		if err != nil {
			return "", err
		}
		jws, err := s.Sign(payload)
		if err != nil {
			return "", err
		}
		return jws.CompactSerialize()
	case "HS256":
		s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(secret)}, (&jose.SignerOptions{}).WithType("JWT"))
		if err != nil {
			return "", err
		}
		jws, err := s.Sign(payload)
		if err != nil {
			return "", err
		}
		return jws.CompactSerialize()
	case "NONE":
		enc := base64.RawURLEncoding
		return enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + ".", nil
	}
	return "", fmt.Errorf("unsupported alg %q", alg)
}
