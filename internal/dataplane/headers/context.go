package headers

import (
	"context"
	"net/http"
)

// requestCtxKey is the context key Registry.Resolve stashes the inbound
// *http.Request under, for the incoming_field resolver to read back.
type requestCtxKey struct{}

func withIncomingRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, requestCtxKey{}, r)
}

// incomingRequestFrom returns the inbound *http.Request stashed by
// Registry.Resolve, if any.
func incomingRequestFrom(ctx context.Context) (*http.Request, bool) {
	r, ok := ctx.Value(requestCtxKey{}).(*http.Request)
	return r, ok
}
