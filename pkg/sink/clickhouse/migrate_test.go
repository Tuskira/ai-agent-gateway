package clickhouse

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

var fastRetry = retryPolicy{attempts: 5, baseDelay: time.Millisecond, maxDelay: 4 * time.Millisecond}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRetryRetriesCannotAssignAlterThenSucceeds(t *testing.T) {
	calls := 0
	err := fastRetry.do(context.Background(), discardLogger(), "ALTER TABLE x", func(context.Context) error {
		calls++
		if calls < 3 {
			return &clickhouse.Exception{Code: 517, Name: "DB::Exception", Message: "Looks like this replica doesn't catchup with latest ALTER query updates. Please retry this query"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("do() error = %v, want nil after retries", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (two 517s, then success)", calls)
	}
}

func TestRetryRecognisesWrappedException(t *testing.T) {
	calls := 0
	err := fastRetry.do(context.Background(), discardLogger(), "ALTER TABLE x", func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.Join(errors.New("exec"), &clickhouse.Exception{Code: 517})
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("do() = %v after %d calls, want nil after 2", err, calls)
	}
}

func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	calls := 0
	err := fastRetry.do(context.Background(), discardLogger(), "ALTER TABLE x", func(context.Context) error {
		calls++
		return &clickhouse.Exception{Code: 517}
	})
	var ex *clickhouse.Exception
	if !errors.As(err, &ex) || ex.Code != 517 {
		t.Fatalf("do() error = %v, want the 517 exception", err)
	}
	if calls != fastRetry.attempts {
		t.Fatalf("calls = %d, want %d", calls, fastRetry.attempts)
	}
}

func TestRetryFailsImmediatelyOnNonRetryableCode(t *testing.T) {
	for _, err := range []error{
		&clickhouse.Exception{Code: 62, Message: "Syntax error"}, // SYNTAX_ERROR
		&clickhouse.Exception{Code: 497, Message: "Not enough privileges"},
		errors.New("dial tcp: connection refused"), // not a server exception
	} {
		calls := 0
		got := fastRetry.do(context.Background(), discardLogger(), "ALTER TABLE x", func(context.Context) error {
			calls++
			return err
		})
		if !errors.Is(got, err) || calls != 1 {
			t.Errorf("do(%v) = %v after %d calls, want the same error after 1", err, got, calls)
		}
	}
}

func TestRetryStopsWhenContextIsCancelled(t *testing.T) {
	slow := retryPolicy{attempts: 10, baseDelay: time.Hour, maxDelay: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- slow.do(ctx, discardLogger(), "ALTER TABLE x", func(context.Context) error {
			calls++
			return &clickhouse.Exception{Code: 517}
		})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("do() error = %v, want it to wrap context.Canceled", err)
		}
		var ex *clickhouse.Exception
		if !errors.As(err, &ex) || ex.Code != 517 {
			t.Fatalf("do() error = %v, want it to keep the 517 exception", err)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("do() did not return after ctx was cancelled")
	}
}

func TestBackoffStaysWithinBounds(t *testing.T) {
	for attempt := 1; attempt <= 70; attempt++ {
		d := schemaRetry.backoff(attempt)
		if d <= 0 || d > schemaRetry.maxDelay {
			t.Fatalf("backoff(%d) = %v, want in (0, %v]", attempt, d, schemaRetry.maxDelay)
		}
	}
}

func TestSchemaFingerprint(t *testing.T) {
	stmts := schemaStatements()
	fp := schemaFingerprint(stmts)
	if len(fp) != 64 || fp != schemaFingerprint(schemaStatements()) {
		t.Fatalf("fingerprint %q not a stable sha256 hex", fp)
	}
	if schemaFingerprint(append(stmts[:len(stmts):len(stmts)], "ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS x String")) == fp {
		t.Error("adding a statement did not change the fingerprint")
	}
	if schemaFingerprint([]string{"ab", "c"}) == schemaFingerprint([]string{"a", "bc"}) {
		t.Error("statement boundaries are not part of the fingerprint")
	}
	if stmts[0] != migrationLedger {
		t.Error("the ledger table must be created first")
	}
}
