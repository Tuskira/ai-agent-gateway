package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// fakeAuthenticator is a scripted pkg/auth.Authenticator for testing Chain
// and the middleware without a real credential source.
type fakeAuthenticator struct {
	name      string
	principal *pkgauth.Principal
	err       error
	calls     int
}

func (f *fakeAuthenticator) Name() string { return f.name }

func (f *fakeAuthenticator) Authenticate(_ context.Context, _ *http.Request) (*pkgauth.Principal, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.principal, nil
}

func TestChain_FirstSucceeds(t *testing.T) {
	want := &pkgauth.Principal{Subject: "u1"}
	a1 := &fakeAuthenticator{name: "a1", principal: want}
	a2 := &fakeAuthenticator{name: "a2", principal: &pkgauth.Principal{Subject: "u2"}}

	got, err := Chain(a1, a2).Authenticate(context.Background(), &http.Request{})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got != want {
		t.Errorf("Authenticate() = %+v, want %+v", got, want)
	}
	if a2.calls != 0 {
		t.Errorf("a2 should not have been tried; calls = %d", a2.calls)
	}
}

func TestChain_NoCredentialFallsThrough(t *testing.T) {
	want := &pkgauth.Principal{Subject: "u2"}
	a1 := &fakeAuthenticator{name: "a1", err: pkgauth.ErrNoCredential}
	a2 := &fakeAuthenticator{name: "a2", principal: want}

	got, err := Chain(a1, a2).Authenticate(context.Background(), &http.Request{})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got != want {
		t.Errorf("Authenticate() = %+v, want %+v", got, want)
	}
	if a1.calls != 1 || a2.calls != 1 {
		t.Errorf("calls a1=%d a2=%d, want 1,1", a1.calls, a2.calls)
	}
}

func TestChain_NonNoCredentialErrorStopsChain(t *testing.T) {
	stopErr := fmt.Errorf("%w: bad key", pkgauth.ErrInvalid)
	a1 := &fakeAuthenticator{name: "a1", err: stopErr}
	a2 := &fakeAuthenticator{name: "a2", principal: &pkgauth.Principal{Subject: "u2"}}

	_, err := Chain(a1, a2).Authenticate(context.Background(), &http.Request{})
	if !errors.Is(err, pkgauth.ErrInvalid) {
		t.Fatalf("Authenticate() error = %v, want wrapping ErrInvalid", err)
	}
	if a2.calls != 0 {
		t.Errorf("a2 should not have been tried after a non-ErrNoCredential error; calls = %d", a2.calls)
	}
}

func TestChain_AllNoCredential(t *testing.T) {
	a1 := &fakeAuthenticator{name: "a1", err: pkgauth.ErrNoCredential}
	a2 := &fakeAuthenticator{name: "a2", err: pkgauth.ErrNoCredential}

	_, err := Chain(a1, a2).Authenticate(context.Background(), &http.Request{})
	if !errors.Is(err, pkgauth.ErrNoCredential) {
		t.Fatalf("Authenticate() error = %v, want ErrNoCredential", err)
	}
}

func TestChain_Empty(t *testing.T) {
	_, err := Chain().Authenticate(context.Background(), &http.Request{})
	if !errors.Is(err, pkgauth.ErrNoCredential) {
		t.Fatalf("Authenticate() error = %v, want ErrNoCredential", err)
	}
}

func TestChain_Name(t *testing.T) {
	a1 := &fakeAuthenticator{name: "apikey"}
	a2 := &fakeAuthenticator{name: "dev"}
	if got, want := Chain(a1, a2).Name(), "chain(apikey,dev)"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}
