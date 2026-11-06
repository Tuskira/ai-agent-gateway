package oidc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Login-state cookie names. On an https callback the cookie carries the
// __Host- prefix (Secure, Path=/, no Domain: a sibling host or a plain-http
// response cannot plant or overwrite it); a plain-http loopback callback
// cannot use the prefix and gets the bare name.
const (
	StateCookieSecure = "__Host-gw_oidc"
	StateCookiePlain  = "gw_oidc"
)

// loginState is everything the callback needs, sealed into the cookie.
type loginState struct {
	Provider    string `json:"p"`
	State       string `json:"s"`
	Nonce       string `json:"n"`
	Verifier    string `json:"v"`
	ReturnTo    string `json:"r"`
	RedirectURL string `json:"u"`
	Expires     int64  `json:"e"` // unix seconds
}

func cookieNameFor(secure bool) string {
	if secure {
		return StateCookieSecure
	}
	return StateCookiePlain
}

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Service) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.sealKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal encrypts st with AES-256-GCM; the cookie name is the additional
// data, so a value minted for one name does not open under the other.
func (s *Service) seal(st *loginState, cookieName string) (string, error) {
	plain, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	gcm, err := s.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, plain, []byte(cookieName))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

func (s *Service) open(value, cookieName string) (*loginState, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	gcm, err := s.aead()
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("short value")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(cookieName))
	if err != nil {
		return nil, err
	}
	var st loginState
	if err := json.Unmarshal(plain, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Begin starts a login with p for the request r. returnTo must already be
// validated (CleanReturnTo). It returns the IdP authorize URL to redirect
// to and the login-state cookie to set on that redirect: HttpOnly,
// SameSite=Lax (the callback arrives as a cross-site top-level
// navigation, which Lax allows and Strict would drop), Secure and
// __Host- prefixed when the callback is https, living StateLifetime.
func (s *Service) Begin(p *Provider, r *http.Request, returnTo string) (authURL string, cookie *http.Cookie, err error) {
	d, _, err := p.discover(s.now())
	if err != nil {
		return "", nil, &Error{Code: CodeProvider, Err: fmt.Errorf("discovery: %w", err)}
	}
	redirect := p.redirectFor(r)
	st := &loginState{Provider: p.Name, ReturnTo: returnTo, RedirectURL: redirect.String(),
		Expires: s.now().Add(StateLifetime).Unix()}
	if st.State, err = randomString(); err != nil {
		return "", nil, err
	}
	if st.Nonce, err = randomString(); err != nil {
		return "", nil, err
	}
	st.Verifier = oauth2.GenerateVerifier()

	secure := redirect.Scheme == "https"
	name := cookieNameFor(secure)
	value, err := s.seal(st, name)
	if err != nil {
		return "", nil, err
	}

	opts := []oauth2.AuthCodeOption{gooidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)}
	for k, v := range p.cfg.AuthParams {
		opts = append(opts, oauth2.SetAuthURLParam(k, v))
	}
	authURL = p.oauth2Config(d, st.RedirectURL).AuthCodeURL(st.State, opts...)
	cookie = &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(StateLifetime.Seconds()),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	}
	return authURL, cookie, nil
}

// ClearStateCookies returns the cookies that delete both login-state
// cookie names; the callback sets them on every answer.
func ClearStateCookies() []*http.Cookie {
	return []*http.Cookie{
		{Name: StateCookieSecure, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode},
		{Name: StateCookiePlain, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode},
	}
}

// readState finds and opens the login-state cookie of a callback and
// checks it belongs to p, is unexpired, and matches the state parameter.
func (s *Service) readState(p *Provider, r *http.Request) (*loginState, error) {
	var (
		st   *loginState
		name string
	)
	for _, n := range []string{StateCookieSecure, StateCookiePlain} {
		c, err := r.Cookie(n)
		if err != nil || c.Value == "" {
			continue
		}
		if got, err := s.open(c.Value, n); err == nil {
			st, name = got, n
			break
		}
	}
	if st == nil {
		return nil, errors.New("no valid login-state cookie")
	}
	// An https login's state only counts from the __Host- cookie: a
	// plain cookie could have been planted over http.
	if strings.HasPrefix(st.RedirectURL, "https://") && name != StateCookieSecure {
		return nil, errors.New("https login state in a non-__Host- cookie")
	}
	if st.Provider != p.Name {
		return nil, fmt.Errorf("login state is for provider %q", st.Provider)
	}
	if s.now().Unix() >= st.Expires {
		return nil, errors.New("login state expired")
	}
	got := r.URL.Query().Get("state")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(st.State)) != 1 {
		return nil, errors.New("state parameter does not match")
	}
	return st, nil
}

// Result is a completed login: the verified identity plus where the user
// wanted to go.
type Result struct {
	Identity *Identity
	ReturnTo string
}

// Finish completes a login on the callback request r. Every failure is an
// *Error whose Code is one of the Code* constants. On success the
// identity's issuer, subject and email are verified and the email passed
// the provider's domain, verification and upstream-IdP rules; mapping it
// to a user is up to the caller.
func (s *Service) Finish(p *Provider, r *http.Request) (*Result, error) {
	st, err := s.readState(p, r)
	if err != nil {
		return nil, &Error{Code: CodeState, Err: err}
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		// The IdP's error code is logged by the caller through Err; it is
		// truncated so a long value cannot flood the log.
		if len(e) > 64 {
			e = e[:64]
		}
		return nil, fail(CodeProvider, "the issuer returned error %q", e)
	}
	code := q.Get("code")
	if code == "" {
		return nil, fail(CodeToken, "no authorization code")
	}
	d, verifier, err := p.discover(s.now())
	if err != nil {
		return nil, &Error{Code: CodeProvider, Err: fmt.Errorf("discovery: %w", err)}
	}

	ctx := gooidc.ClientContext(r.Context(), p.client)
	tok, err := p.oauth2Config(d, st.RedirectURL).Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return nil, fail(CodeToken, "code exchange: %v", exchangeError(err))
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return nil, fail(CodeToken, "token response has no id_token")
	}
	idTok, err := verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, fail(CodeToken, "verify id_token: %v", err)
	}
	ident, err := s.checkToken(p, idTok, st.Nonce)
	if err != nil {
		return nil, err
	}
	return &Result{Identity: ident, ReturnTo: st.ReturnTo}, nil
}

// exchangeError keeps the OAuth error code of a failed exchange and drops
// the response body, which an IdP may fill with anything.
func exchangeError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		status := 0
		if re.Response != nil {
			status = re.Response.StatusCode
		}
		return fmt.Errorf("token endpoint answered %d %q", status, re.ErrorCode)
	}
	return err
}
