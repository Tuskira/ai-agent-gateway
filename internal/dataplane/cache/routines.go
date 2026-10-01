package cache

import (
	"context"
	"log/slog"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/supervisor"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// RefreshFunc re-reads one tenant's tools from its connectors and writes
// them back into the cache.
//
// It is supplied by the caller rather than implemented here so that this
// package stays a leaf: fanning out to backends needs the router and the
// client, and having the cache reach for those would make the dependency
// graph circular for no gain.
type RefreshFunc func(ctx context.Context, tenantID string) error

// RefreshRoutine returns a supervisor.Routine that re-reads every
// tenant's tools on a fixed interval.
//
// It exists so a stale-served tool list has a bounded age even when
// nobody calls tools/list often enough to trigger a refresh themselves,
// and so a tool added to a backend shows up without an operator having to
// poke the gateway.
func RefreshRoutine(tenants store.TenantStore, refresh RefreshFunc, interval time.Duration, logger *slog.Logger) supervisor.Routine {
	if logger == nil {
		logger = slog.Default()
	}
	return &tickerRoutine{
		name:     "mcp-tool-cache-refresh",
		interval: interval,
		logger:   logger,
		done:     make(chan struct{}),
		tick: func(ctx context.Context) {
			all, err := tenants.List(ctx)
			if err != nil {
				logger.Warn("tool cache refresh: list tenants failed", "error", err)
				return
			}
			for _, tn := range all {
				if err := refresh(ctx, tn.ID); err != nil {
					logger.Warn("tool cache refresh failed", "tenant_id", tn.ID, "error", err)
					continue
				}
				logger.Debug("tool cache refreshed", "tenant_id", tn.ID)
			}
		},
	}
}

// CleanupRoutine returns a supervisor.Routine that deletes expired rows
// on a fixed interval.
//
// Expiry alone does not free anything: the serving path keeps reading
// stale rows on purpose. This is what stops the table growing without
// bound as connectors come and go.
func CleanupRoutine(tools store.ToolCacheStore, interval time.Duration, logger *slog.Logger) supervisor.Routine {
	if logger == nil {
		logger = slog.Default()
	}
	return &tickerRoutine{
		name:     "mcp-tool-cache-cleanup",
		interval: interval,
		logger:   logger,
		done:     make(chan struct{}),
		tick: func(ctx context.Context) {
			n, err := tools.DeleteExpired(ctx)
			if err != nil {
				logger.Warn("tool cache cleanup failed", "error", err)
				return
			}
			if n > 0 {
				logger.Debug("expired tool cache rows deleted", "count", n)
			}
		},
	}
}

// tickerRoutine runs tick every interval until stopped.
type tickerRoutine struct {
	name     string
	interval time.Duration
	logger   *slog.Logger
	tick     func(ctx context.Context)

	done    chan struct{}
	stopped bool
}

func (t *tickerRoutine) Name() string { return t.name }

func (t *tickerRoutine) Init(context.Context) error { return nil }

func (t *tickerRoutine) Run(ctx context.Context) error {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	for {
		select {
		case <-t.done:
			return nil
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			t.tick(ctx)
		}
	}
}

func (t *tickerRoutine) Stop(context.Context) error {
	if !t.stopped {
		t.stopped = true
		close(t.done)
	}
	return nil
}
