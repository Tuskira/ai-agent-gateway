// Package supervisor runs a fixed set of long-lived Routines with a common
// Init/Run/Stop lifecycle, and coordinates their shutdown.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
)

// Routine is a managed unit of work. It mirrors lib-core's runner interface
// so implementations can move between the two without adaptation.
type Routine interface {
	Name() string
	Init(ctx context.Context) error
	Run(ctx context.Context) error
	Stop(ctx context.Context) error
}

// stopTimeout bounds how long Stop is given to run across all routines.
const stopTimeout = 10 * time.Second

// Run initializes every routine in order, failing fast on the first Init
// error. It then runs all routines concurrently. Many routines (e.g. an
// http.Server's Serve loop) block until Stop is called rather than reacting
// to context cancellation, so Run treats "one routine has terminated" or "a
// shutdown signal arrived" purely as a trigger to proactively call Stop on
// every routine (in reverse order, with a bounded timeout) rather than
// assuming cancellation alone unblocks them. It returns the first error
// encountered, if any.
func Run(ctx context.Context, logger *slog.Logger, routines ...Routine) error {
	for _, r := range routines {
		if err := r.Init(ctx); err != nil {
			return fmt.Errorf("init %s: %w", r.Name(), err)
		}
	}

	sigCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	g, gCtx := errgroup.WithContext(sigCtx)
	for _, r := range routines {
		g.Go(func() error {
			if err := r.Run(gCtx); err != nil {
				return fmt.Errorf("run %s: %w", r.Name(), err)
			}
			return nil
		})
	}

	doneCh := make(chan error, 1)
	go func() { doneCh <- g.Wait() }()

	var runErr error
	var doneReceived bool
	select {
	case runErr = <-doneCh:
		doneReceived = true
	case <-gCtx.Done():
		if sigCtx.Err() != nil {
			logger.Info("shutdown signal received")
		} else {
			logger.Warn("a routine exited, shutting down the rest")
		}
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), stopTimeout)
	defer cancelStop()

	var stopErrs []error
	for i := len(routines) - 1; i >= 0; i-- {
		r := routines[i]
		if err := r.Stop(stopCtx); err != nil {
			logger.Error("stop failed", "routine", r.Name(), "error", err)
			stopErrs = append(stopErrs, fmt.Errorf("stop %s: %w", r.Name(), err))
		}
	}

	if !doneReceived {
		runErr = <-doneCh
	}
	if runErr != nil {
		logger.Error("routine run failed", "error", runErr)
	}

	if runErr != nil {
		return runErr
	}
	if len(stopErrs) > 0 {
		return errors.Join(stopErrs...)
	}
	return nil
}
