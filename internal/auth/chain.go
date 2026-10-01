// Package auth wires the gateway's concrete Authenticators (see the apikey
// and devmode subpackages) into the auth.Authenticator chain the gateway's
// planes actually use, and provides the HTTP middleware that enforces it.
package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// chain tries a fixed sequence of Authenticators in order.
type chain struct {
	auths []auth.Authenticator
}

var _ auth.Authenticator = (*chain)(nil)

// Chain returns an auth.Authenticator that tries each of auths in order,
// stopping at the first one that either succeeds or fails with something
// other than auth.ErrNoCredential. If every Authenticator in the chain
// returns ErrNoCredential, Chain does too, so the caller can distinguish
// "nobody recognized this request" from "somebody rejected it".
func Chain(auths ...auth.Authenticator) auth.Authenticator {
	return &chain{auths: auths}
}

// Name lists the chained Authenticators' names, e.g. "chain(apikey,dev)".
func (c *chain) Name() string {
	name := "chain("
	for i, a := range c.auths {
		if i > 0 {
			name += ","
		}
		name += a.Name()
	}
	return name + ")"
}

// Authenticate tries each Authenticator in c in order. See Chain's doc for
// the exact semantics.
func (c *chain) Authenticate(ctx context.Context, r *http.Request) (*auth.Principal, error) {
	for _, a := range c.auths {
		p, err := a.Authenticate(ctx, r)
		if err == nil {
			return p, nil
		}
		if errors.Is(err, auth.ErrNoCredential) {
			continue
		}
		return nil, err
	}
	return nil, auth.ErrNoCredential
}
