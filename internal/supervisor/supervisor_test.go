package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeRoutine's Run blocks on ctx.Done() until Stop is called (mirroring an
// http.Server, which does not exit its Serve loop on context cancellation
// alone), unless runErr or exitImmediately is set.
type fakeRoutine struct {
	mu   sync.Mutex
	name string

	initErr         error
	runErr          error
	stopErr         error
	exitImmediately bool

	initCalled bool
	runCalled  bool
	stopCalled bool
}

func (f *fakeRoutine) Name() string { return f.name }

func (f *fakeRoutine) Init(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.initCalled = true
	return f.initErr
}

func (f *fakeRoutine) Run(ctx context.Context) error {
	f.mu.Lock()
	f.runCalled = true
	f.mu.Unlock()

	if f.exitImmediately {
		return f.runErr
	}
	<-ctx.Done() // real routines exit on ctx cancellation
	return f.runErr
}

func (f *fakeRoutine) Stop(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalled = true
	return f.stopErr
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRun_InitFailureFastFails(t *testing.T) {
	first := &fakeRoutine{name: "first", initErr: errors.New("boom")}
	second := &fakeRoutine{name: "second"}

	err := Run(context.Background(), testLogger(), first, second)
	if err == nil {
		t.Fatal("expected error")
	}
	if second.initCalled {
		t.Error("second.Init should not have been called after first.Init failed")
	}
	if first.runCalled || second.runCalled {
		t.Error("Run should not be called when Init fails")
	}
}

func TestRun_ContextCancellationStopsAllInReverseOrder(t *testing.T) {
	var order []string
	var mu sync.Mutex
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	a := &fakeRoutine{name: "a"}
	b := &fakeRoutine{name: "b"}
	c := &fakeRoutine{name: "c"}

	// Wrap each in a recordingRoutine to capture Stop call order.
	routines := []Routine{
		&recordingRoutine{fakeRoutine: a, record: record},
		&recordingRoutine{fakeRoutine: b, record: record},
		&recordingRoutine{fakeRoutine: c, record: record},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := Run(ctx, testLogger(), routines...)
	if err != nil {
		t.Fatalf("expected clean shutdown, got error: %v", err)
	}

	want := []string{"c", "b", "a"}
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("stop order = %v, want %v", got, want)
	}
}

// recordingRoutine wraps a fakeRoutine to record Stop call order.
type recordingRoutine struct {
	*fakeRoutine
	record func(string)
}

func (r *recordingRoutine) Stop(ctx context.Context) error {
	r.record(r.name)
	return r.fakeRoutine.Stop(ctx)
}

func TestRun_OneRoutineErrorTriggersShutdownOfOthers(t *testing.T) {
	failing := &fakeRoutine{name: "failing", runErr: errors.New("crashed"), exitImmediately: true}
	blocking := &fakeRoutine{name: "blocking"}

	err := Run(context.Background(), testLogger(), failing, blocking)
	if err == nil {
		t.Fatal("expected error")
	}

	if !blocking.stopCalled {
		t.Error("blocking routine should have been stopped after failing routine errored")
	}
	if !failing.stopCalled {
		t.Error("failing routine should also have Stop called")
	}
}

func TestRun_StopErrorsAreAggregatedWhenNoRunError(t *testing.T) {
	a := &fakeRoutine{name: "a", exitImmediately: true, stopErr: errors.New("stop-a-failed")}
	b := &fakeRoutine{name: "b", exitImmediately: true}

	err := Run(context.Background(), testLogger(), a, b)
	if err == nil {
		t.Fatal("expected aggregated stop error")
	}
}
