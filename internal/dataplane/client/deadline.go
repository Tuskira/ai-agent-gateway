package client

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errCallDeadline is the cancellation cause of a call whose
// per-connector timeout ran out. Do reports it as ErrTimeout.
var errCallDeadline = errors.New("client: per-connector timeout elapsed")

// callDeadline is a call's per-connector timeout, as a timer that can be
// paused. It stands in for context.WithTimeout so that the time a call
// spends waiting on the AGENT -- answering a request the connector sent
// mid-call (sampling, elicitation, roots) -- is not charged against the
// connector's budget: a human filling in an elicitation form is not the
// backend being slow. Pauses nest: the clock runs again only when the
// last concurrent request has been answered.
type callDeadline struct {
	cancel context.CancelCauseFunc

	mu        sync.Mutex
	timer     *time.Timer
	remaining time.Duration
	started   time.Time
	paused    int
	stopped   bool
}

// withCallDeadline returns ctx bounded by timeout, the deadline that
// bounds it, and the function that releases both.
func withCallDeadline(ctx context.Context, timeout time.Duration) (context.Context, *callDeadline, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	d := &callDeadline{cancel: cancel, remaining: timeout, started: time.Now()}
	d.timer = time.AfterFunc(timeout, func() { cancel(errCallDeadline) })
	ctx = context.WithValue(ctx, callDeadlineKey{}, d)
	return ctx, d, func() {
		d.mu.Lock()
		d.stopped = true
		d.timer.Stop()
		d.mu.Unlock()
		cancel(nil)
	}
}

type callDeadlineKey struct{}

// deadlineFrom returns the deadline Do put on ctx, or nil.
func deadlineFrom(ctx context.Context) *callDeadline {
	d, _ := ctx.Value(callDeadlineKey{}).(*callDeadline)
	return d
}

// pause stops the clock. A deadline that has already fired stays fired.
func (d *callDeadline) pause() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused++
	if d.paused > 1 || d.stopped {
		return
	}
	if d.timer.Stop() {
		d.remaining -= time.Since(d.started)
	} else {
		d.stopped = true // it already fired: nothing to resume
	}
}

// resume restarts the clock with what was left of it when the first of
// the outstanding pauses began.
func (d *callDeadline) resume() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.paused == 0 {
		return
	}
	d.paused--
	if d.paused > 0 || d.stopped {
		return
	}
	if d.remaining <= 0 {
		d.cancel(errCallDeadline)
		return
	}
	d.started = time.Now()
	d.timer.Reset(d.remaining)
}

// expired reports whether ctx was ended by its call deadline.
func expired(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errCallDeadline)
}
