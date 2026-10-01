package transport_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// readSSEFrames starts a goroutine reading resp.Body into an
// accumulating string, sent on the returned channel on every read --
// tests select on it in a loop, checking the accumulated text so far, so
// a notification split across two TCP reads (or two notifications
// coalesced into one) are both handled the same way.
func readSSEFrames(t *testing.T, resp *http.Response) <-chan string {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		defer close(out)
		var acc strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				select {
				case out <- acc.String():
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}

// waitForSubstrings blocks until acc's accumulated text contains every
// one of want, or fails the test after timeout.
func waitForSubstrings(t *testing.T, acc <-chan string, timeout time.Duration, want ...string) {
	t.Helper()
	deadline := time.After(timeout)
	var last string
	for {
		select {
		case s, ok := <-acc:
			if !ok {
				t.Fatalf("stream closed before seeing %v; last accumulated text: %q", want, last)
			}
			last = s
			missing := false
			for _, w := range want {
				if !strings.Contains(s, w) {
					missing = true
					break
				}
			}
			if !missing {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %v; last accumulated text: %q", want, last)
		}
	}
}

// openStream opens GET /mcp/stream, optionally bound to a profile,
// asserts the 200/text-event-stream handshake, and registers cleanup.
func openStream(t *testing.T, f *fixture, profileName string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, f.server.URL+"/mcp/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if profileName != "" {
		req.Header.Set(profile.Header, profileName)
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	return resp
}

// retryInvalidateProfile calls ProfileOps.InvalidateProfile on a short
// tick until stop is closed, mirroring
// TestSSEStreamSendsKeepAlivesAndNotifications' own retry of
// NotifyToolsListChanged: the client may observe the stream's 200 status
// line a moment before the handler goroutine has reached its Subscribe
// call, so the write needs to be repeated rather than sent once.
func retryInvalidateProfile(f *fixture, tenantID, profileID string, stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = f.plane.ProfileOps.InvalidateProfile(context.Background(), tenantID, profileID)
			}
		}
	}()
}

// TestSSEStreamProfileListChangedOnSkillsInvalidate is an end-to-end
// check, over real HTTP against a real dataplane.Plane (no mocks), of
// Phase 5's live-update path: a GET /mcp/stream opened with
// X-Agent-Profile-Name receives BOTH notifications/tools/list_changed and
// notifications/prompts/list_changed the moment that exact profile's
// cached resolution is invalidated (the same ops.ProfileOps.
// InvalidateProfile call PUT /profiles/{id}/skills, PUT
// /profiles/{id}/tools, PUT /profiles/{id} and DELETE /profiles/{id}
// already make -- see internal/api/handlers.Profiles) -- without waiting
// out the enforcer's 30s cache TTL.
func TestSSEStreamProfileListChangedOnSkillsInvalidate(t *testing.T) {
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})
	f.attachSkill(t, "runbook", "skill", "How to triage", nil)

	prof, err := f.store.AgentProfiles().GetBySlug(context.Background(), tenantID, profile.Slug(tenantID, "Reader"))
	if err != nil {
		t.Fatal(err)
	}

	resp := openStream(t, f, "Reader")
	frames := readSSEFrames(t, resp)

	stop := make(chan struct{})
	defer close(stop)
	retryInvalidateProfile(f, tenantID, prof.ID, stop)

	waitForSubstrings(t, frames, 5*time.Second,
		mcp.NotificationToolsListChanged, mcp.NotificationPromptsListChanged)
}

// TestSSEStreamProfileListChangedIsScopedToProfile guards the scoping
// promise: invalidating the "Reader" profile must not wake a stream
// bound to a different profile in the same tenant.
func TestSSEStreamProfileListChangedIsScopedToProfile(t *testing.T) {
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})
	f.attachSkill(t, "runbook", "skill", "How to triage", nil)

	// A second profile, granted nothing, purely as the "other stream"
	// this notification must not reach.
	other := &store.AgentProfile{TenantID: tenantID, Name: "Other", Slug: profile.Slug(tenantID, "Other")}
	if err := f.store.AgentProfiles().Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}

	prof, err := f.store.AgentProfiles().GetBySlug(context.Background(), tenantID, profile.Slug(tenantID, "Reader"))
	if err != nil {
		t.Fatal(err)
	}

	readerResp := openStream(t, f, "Reader")
	readerFrames := readSSEFrames(t, readerResp)
	otherResp := openStream(t, f, "Other")
	otherFrames := readSSEFrames(t, otherResp)

	stop := make(chan struct{})
	defer close(stop)
	retryInvalidateProfile(f, tenantID, prof.ID, stop)

	waitForSubstrings(t, readerFrames, 5*time.Second, mcp.NotificationPromptsListChanged)

	// The "Other" stream must not have seen it either, in the same
	// window we just waited out on the "Reader" stream.
	select {
	case s := <-otherFrames:
		if strings.Contains(s, mcp.NotificationPromptsListChanged) || strings.Contains(s, mcp.NotificationToolsListChanged) {
			t.Fatalf("the Other profile's stream received a list_changed notification meant for Reader: %q", s)
		}
	default:
	}
}
