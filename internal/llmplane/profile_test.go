package llmplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// profileStore is the one AgentProfileStore method ProfileNames uses.
type profileStore struct {
	store.AgentProfileStore // other methods panic: ProfileNames must not call them
	profiles                map[string]string
	err                     error
	gets                    atomic.Int32
}

func (p *profileStore) Get(_ context.Context, tenantID, id string) (*store.AgentProfile, error) {
	p.gets.Add(1)
	if p.err != nil {
		return nil, p.err
	}
	if name, ok := p.profiles[tenantID+"|"+id]; ok {
		return &store.AgentProfile{ID: id, TenantID: tenantID, Name: name}, nil
	}
	return nil, store.ErrNotFound
}

func TestProfileNames_CachesHitsAndMisses(t *testing.T) {
	ps := &profileStore{profiles: map[string]string{"t|p1": "Triage Agent"}}
	now := time.Unix(1000, 0)
	n := NewProfileNames(ps, time.Minute).(*profileNames)
	n.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if got := n.ProfileName(ctx, "t", "p1"); got != "Triage Agent" {
			t.Fatalf("ProfileName = %q, want Triage Agent", got)
		}
		if got := n.ProfileName(ctx, "t", "gone"); got != "" {
			t.Fatalf("missing profile = %q, want empty", got)
		}
	}
	if got := ps.gets.Load(); got != 2 {
		t.Errorf("store reads = %d, want 2 (one per profile, hit or miss, within the ttl)", got)
	}
	// Another tenant's profile id is a different entry.
	if got := n.ProfileName(ctx, "other", "p1"); got != "" {
		t.Errorf("cross-tenant lookup = %q, want empty", got)
	}

	now = now.Add(2 * time.Minute)
	n.ProfileName(ctx, "t", "p1")
	if got := ps.gets.Load(); got != 4 {
		t.Errorf("store reads after the ttl = %d, want 4", got)
	}
}

func TestProfileNames_ErrorsAreNotCached(t *testing.T) {
	ps := &profileStore{err: errors.New("db down")}
	n := NewProfileNames(ps, time.Minute)
	for i := 0; i < 3; i++ {
		if got := n.ProfileName(context.Background(), "t", "p1"); got != "" {
			t.Fatalf("ProfileName on error = %q, want empty", got)
		}
	}
	if got := ps.gets.Load(); got != 3 {
		t.Errorf("store reads = %d, want 3: a failed lookup is retried, not remembered", got)
	}
}

// profileCall sends one call through the LLM plane as principal p and
// returns the captured row's profile.
func profileCall(t *testing.T, cfg Config, p *pkgauth.Principal, header string) string {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()
	cfg.UpstreamBaseURL = up.URL
	cfg.Authorizer = pkgauth.NewRoleAuthorizer()
	rec := &lastRec{}
	h, err := Handler(cfg, rec)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(withPrincipal(h, p))
	defer gw.Close()
	headers := map[string]string{}
	if header != "" {
		headers[ProfileHeader] = header
	}
	do(t, "POST", gw.URL+"/anthropic/v1/messages", headers, `{"model":"claude-sonnet-4-5"}`)
	return rec.wait(t).Profile
}

func TestRouter_RecordsProfile(t *testing.T) {
	ps := &profileStore{profiles: map[string]string{"t|p-triage": "Triage Agent"}}
	cfg := Config{Profiles: NewProfileNames(ps, time.Minute)}
	agent := func(profileID string) *pkgauth.Principal {
		return &pkgauth.Principal{TenantID: "t", Roles: []string{"agent"}, KeyID: "k1", ProfileID: profileID}
	}

	cases := []struct {
		name    string
		cfg     Config
		p       *pkgauth.Principal
		header  string
		want    string
		comment string
	}{
		{"unbound key, header", cfg, agent(""), "Research Agent", "Research Agent", "the caller's header names the agent"},
		{"unbound key, padded header", cfg, agent(""), "  Research Agent  ", "Research Agent", "trimmed"},
		{"unbound key, no header", cfg, agent(""), "", "", "nothing to name it by"},
		{"bound key beats a header", cfg, agent("p-triage"), "Spoofed", "Triage Agent", "the binding cannot be spoofed"},
		{"bound key, no header", cfg, agent("p-triage"), "", "Triage Agent", ""},
		{"bound key, profile deleted", cfg, agent("p-deleted"), "Spoofed", "", "a dangling binding is not the header's to name"},
		{"bound key, no namer", Config{}, agent("p-triage"), "Spoofed", "", "without a namer a bound key records no profile"},
		{"oversized header is capped", cfg, agent(""), strings.Repeat("a", 500), strings.Repeat("a", 128), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := profileCall(t, c.cfg, c.p, c.header); got != c.want {
				t.Errorf("profile = %q, want %q (%s)", got, c.want, c.comment)
			}
		})
	}
}

// A call refused before any upstream attempt is still attributed.
func TestRouter_RecordsProfileOnRefusedCall(t *testing.T) {
	rec := &lastRec{}
	gw := gateway(t, Config{BedrockEnabled: true, BedrockRegion: "us-east-1"}, rec)
	do(t, "POST", gw.URL+"/bedrock/model/m/invoke",
		map[string]string{"X-Bedrock-Access-Key-Id": "AKID", ProfileHeader: "Triage Agent"}, `{}`)
	if got := rec.wait(t).Profile; got != "Triage Agent" {
		t.Errorf("refused call profile = %q, want Triage Agent", got)
	}
}
