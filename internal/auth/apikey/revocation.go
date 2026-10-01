package apikey

import (
	"context"
	"log/slog"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const (
	listenBackoffMin = 500 * time.Millisecond
	listenBackoffMax = 30 * time.Second
	// listenStableAfter is how long a subscription must have stayed up for
	// its next failure to restart the backoff from the minimum.
	listenStableAfter = 30 * time.Second
)

// ListenRevocations keeps a's cache in step with revocations made by ANY
// gateway process: it subscribes through n (the store's optional
// store.RevocationNotifier), evicts each announced key hash, and flushes
// the whole cache whenever the subscription (re)establishes, because events
// published while it was down are gone. A failed subscription is retried
// with exponential backoff. It blocks until ctx is done.
//
// If the subscription cannot be kept up, revocation still takes effect
// everywhere within Options.CacheTTL; the listener only makes it prompt.
func (a *Authenticator) ListenRevocations(ctx context.Context, n store.RevocationNotifier, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	backoff := listenBackoffMin
	for ctx.Err() == nil {
		started := time.Now()
		err := n.ListenKeyRevocations(ctx, func() {
			a.Flush()
			logger.Info("api key revocation listener ready")
		}, a.Invalidate)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= listenStableAfter {
			backoff = listenBackoffMin
		}
		logger.Warn("api key revocation listener lost; retrying (revocations fall back to the cache TTL meanwhile)",
			"error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, listenBackoffMax)
	}
}
