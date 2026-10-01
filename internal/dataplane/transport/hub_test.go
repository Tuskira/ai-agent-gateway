package transport

import (
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// TestHubNotifyProfileListChangedSendsBothNotifications is the
// whitebox unit test for the profile-scoped live-update path (Phase 5 of
// "skills & commands on agent profiles"): a single
// NotifyProfileListChanged call must deliver both
// notifications/tools/list_changed and notifications/prompts/list_changed
// to a subscriber, since a skill/command attach or detach can affect
// either capability (or both) and the Hub does not track which
// capabilities any one stream's client declared.
func TestHubNotifyProfileListChangedSendsBothNotifications(t *testing.T) {
	h := NewHub()
	msgs, unsubscribe := h.SubscribeProfile("tenant-a", "tenant-a-reader")
	defer unsubscribe()

	h.NotifyProfileListChanged("tenant-a", "tenant-a-reader")

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case msg := <-msgs:
			if msg.JSONRPC != mcp.Version {
				t.Errorf("notification %d JSONRPC = %q, want %q", i, msg.JSONRPC, mcp.Version)
			}
			got[msg.Method] = true
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for notification %d of 2", i+1)
		}
	}
	if !got[mcp.NotificationToolsListChanged] {
		t.Error("did not receive notifications/tools/list_changed")
	}
	if !got[mcp.NotificationPromptsListChanged] {
		t.Error("did not receive notifications/prompts/list_changed")
	}
}

// TestHubNotifyProfileListChangedIsScopedByTenantAndSlug guards the
// isolation NotifyProfileListChanged's doc comment promises: a different
// profile in the same tenant, and the same profile slug in a different
// tenant, must never see another profile's notification.
func TestHubNotifyProfileListChangedIsScopedByTenantAndSlug(t *testing.T) {
	h := NewHub()
	sameTenantOtherSlug, unsub1 := h.SubscribeProfile("tenant-a", "tenant-a-other")
	defer unsub1()
	otherTenantSameSlug, unsub2 := h.SubscribeProfile("tenant-b", "tenant-a-reader")
	defer unsub2()
	target, unsub3 := h.SubscribeProfile("tenant-a", "tenant-a-reader")
	defer unsub3()

	h.NotifyProfileListChanged("tenant-a", "tenant-a-reader")

	// The intended subscriber gets it...
	select {
	case <-target:
	case <-time.After(time.Second):
		t.Fatal("the target profile's subscriber never received a notification")
	}

	// ...but neither of the other two do.
	select {
	case msg := <-sameTenantOtherSlug:
		t.Fatalf("unrelated profile in the same tenant received %+v, want nothing", msg)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case msg := <-otherTenantSameSlug:
		t.Fatalf("same slug in a different tenant received %+v, want nothing", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestHubNotifyProfileListChangedNoSubscriberIsNoop guards against a
// panic or a block when a profile write invalidates a profile nobody
// currently has a stream open for -- the common case.
func TestHubNotifyProfileListChangedNoSubscriberIsNoop(t *testing.T) {
	h := NewHub()
	h.NotifyProfileListChanged("tenant-a", "tenant-a-reader")
}

// TestHubSubscribeProfileUnsubscribeStopsDelivery guards the returned
// unsubscribe function: once called, the channel must receive nothing
// more, and the Hub must not leak the (now dangling) channel in its map
// (exercised indirectly: a second Notify after unsubscribe must still be
// a no-op, not a panic on a stale entry).
func TestHubSubscribeProfileUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()
	msgs, unsubscribe := h.SubscribeProfile("tenant-a", "tenant-a-reader")
	unsubscribe()

	h.NotifyProfileListChanged("tenant-a", "tenant-a-reader")

	select {
	case msg := <-msgs:
		t.Fatalf("unsubscribed channel received %+v, want nothing", msg)
	case <-time.After(50 * time.Millisecond):
	}

	if len(h.profileSubs) != 0 {
		t.Errorf("profileSubs = %v, want the empty map entry to be pruned on last unsubscribe", h.profileSubs)
	}
}
