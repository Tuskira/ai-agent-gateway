package handlers

import (
	"context"
	"sync"
)

// AuditTrail collects extra key/value details a handler wants on the audit
// line the API router writes for platform routes (see internal/api/audit.go).
// The router puts one in the request context; handlers add to it with
// AuditNote. Without a trail in the context AuditNote does nothing, so the
// shared handler bodies stay usable on the tenant-scoped routes and in tests.
type AuditTrail struct {
	mu     sync.Mutex
	fields []any
}

type auditTrailKey struct{}

// WithAuditTrail returns ctx carrying a new trail, and the trail.
func WithAuditTrail(ctx context.Context) (context.Context, *AuditTrail) {
	t := &AuditTrail{}
	return context.WithValue(ctx, auditTrailKey{}, t), t
}

// AuditNote appends slog-style key/value pairs to the request's trail.
// Never pass secrets (plaintext keys, hashes).
func AuditNote(ctx context.Context, kv ...any) {
	if t, ok := ctx.Value(auditTrailKey{}).(*AuditTrail); ok && t != nil {
		t.mu.Lock()
		t.fields = append(t.fields, kv...)
		t.mu.Unlock()
	}
}

// Fields returns a copy of the collected pairs.
func (t *AuditTrail) Fields() []any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]any(nil), t.fields...)
}
