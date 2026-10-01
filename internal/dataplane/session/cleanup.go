package session

import (
	"context"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/supervisor"
	pkgsession "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
)

// CleanupRoutine returns a supervisor.Routine that sweeps expired
// sessions every CleanupInterval, or nil when the store needs no sweep.
//
// Expiry is already enforced on read (Manager.Resolve rejects and deletes
// a lapsed session, and the store refuses to return one), so this routine
// is about reclaiming storage from sessions nobody comes back for --
// which, for a client that crashed mid-conversation, is all of them.
//
// Only a store that implements pkg/session.Sweeper (the memory driver)
// needs it. A store whose entries expire on their own (Redis, whose keys
// carry a TTL) does not implement Sweeper and gets no routine.
func (m *Manager) CleanupRoutine() supervisor.Routine {
	sw, ok := m.store.(pkgsession.Sweeper)
	if !ok {
		return nil
	}
	return &cleanupRoutine{mgr: m, sweeper: sw, done: make(chan struct{})}
}

type cleanupRoutine struct {
	mgr     *Manager
	sweeper pkgsession.Sweeper
	done    chan struct{}
	// stopped guards against a second Stop closing done twice.
	stopped bool
}

func (c *cleanupRoutine) Name() string { return "mcp-session-cleanup" }

func (c *cleanupRoutine) Init(context.Context) error { return nil }

func (c *cleanupRoutine) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.mgr.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return nil
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n, err := c.sweeper.Sweep(ctx, c.mgr.now())
			if err != nil {
				c.mgr.logger.Warn("session cleanup failed", "error", err)
				continue
			}
			if n > 0 {
				c.mgr.logger.Debug("expired sessions swept", "count", n)
			}
		}
	}
}

func (c *cleanupRoutine) Stop(context.Context) error {
	if !c.stopped {
		c.stopped = true
		close(c.done)
	}
	return nil
}
