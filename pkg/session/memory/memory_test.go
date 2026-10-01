package memory_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/memory"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/sessiontest"
)

func TestConformance(t *testing.T) {
	sessiontest.Run(t, func(*testing.T) session.Store { return memory.New() })
}

func TestRegisteredAsDriver(t *testing.T) {
	st, n, err := session.Open(context.Background(), memory.Driver, session.Config{})
	if err != nil {
		t.Fatalf("Open(%q) = %v", memory.Driver, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, ok := st.(*memory.Store); !ok {
		t.Fatalf("Open returned %T, want *memory.Store", st)
	}
	if n != nil {
		t.Fatalf("Open returned a Notifier (%T) for the single-replica driver, want nil", n)
	}
}

func TestOpenUnknownDriverListsRegistered(t *testing.T) {
	_, _, err := session.Open(context.Background(), "etcd", session.Config{})
	if err == nil {
		t.Fatal("Open(unknown) = nil, want an error")
	}
	if got := err.Error(); !strings.Contains(got, `"etcd"`) || !strings.Contains(got, memory.Driver) {
		t.Fatalf("error %q should name the unknown driver and list the registered ones", got)
	}
}

// Get filters with the store's own clock; a fake clock proves the
// filter is the store's and not an artefact of the record's timestamps.
func TestGetHidesRecordsExpiredByTheStoreClock(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	st := memory.NewWithClock(func() time.Time { return now })
	ctx := context.Background()

	r := &session.Record{ID: "s1", TenantID: "t", ExpiresAt: now.Add(time.Minute)}
	if err := st.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "s1"); err != nil {
		t.Fatalf("Get before expiry = %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := st.Get(ctx, "s1"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get at expiry = %v, want ErrNotFound", err)
	}
	if st.Len() != 0 {
		t.Fatal("an expired record read through Get was not reclaimed")
	}
}
