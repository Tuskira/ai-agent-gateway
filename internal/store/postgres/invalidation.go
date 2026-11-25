package postgres

import (
	"context"
	"encoding/json"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// invalidateChannel is the LISTEN/NOTIFY channel carrying one JSON
// store.Invalidation for every committed write to a row a gateway process
// caches: agent profiles and their grants and skills, the skill registry,
// credentials, credential sources and API keys. The events are published by
// row triggers (migration 000015), so they commit, or roll back, with the
// write that caused them, whichever process or code path made it.
const invalidateChannel = "gateway_invalidate"

var _ store.InvalidationNotifier = (*Store)(nil)

// ListenInvalidations implements store.InvalidationNotifier. A payload that
// is not valid JSON is skipped.
func (s *Store) ListenInvalidations(ctx context.Context, onReady func(), onEvent func(store.Invalidation)) error {
	return s.listen(ctx, invalidateChannel, onReady, func(payload string) {
		var ev store.Invalidation
		if err := json.Unmarshal([]byte(payload), &ev); err != nil || ev.Kind == "" {
			return
		}
		onEvent(ev)
	})
}
