package postgres

import (
	"context"
	"database/sql/driver"
	"fmt"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// keyRevokedChannel is the LISTEN/NOTIFY channel carrying the key hash of
// every revoked API key. apiKeyStore.Revoke publishes to it in the same
// statement (hence the same transaction) as the UPDATE, so a notification
// is delivered if and only if the revoke committed.
const keyRevokedChannel = "gateway_key_revoked"

var _ store.RevocationNotifier = (*Store)(nil)

// ListenKeyRevocations implements store.RevocationNotifier. It pins one
// connection out of the pool for the duration of the subscription (LISTEN is
// per-session state, so it cannot share the pool's rotating connections).
// Any error -- including the server closing the connection -- returns, and
// the caller reconnects with backoff.
func (s *Store) ListenKeyRevocations(ctx context.Context, onReady func(), onRevoke func(keyHash string)) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("postgres: acquire listen connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	err = conn.Raw(func(dc any) error {
		sc, ok := dc.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("postgres: unexpected driver connection %T", dc)
		}
		pc := sc.Conn()
		if _, err := pc.Exec(ctx, "LISTEN "+keyRevokedChannel); err != nil {
			return fmt.Errorf("postgres: LISTEN: %w", err)
		}
		onReady()
		for {
			n, err := pc.WaitForNotification(ctx)
			if err != nil {
				return err
			}
			if n.Channel == keyRevokedChannel && n.Payload != "" {
				onRevoke(n.Payload)
			}
		}
	})
	// Whatever ended the subscription, the session still has LISTEN
	// registered (or is broken): make database/sql discard this connection
	// rather than hand it back to the pool.
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	return err
}
