package router_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const tenant = "tenant-a"

func newRouter(t *testing.T, now func() time.Time, coolDown time.Duration, connectors ...*store.Connector) (*router.Router, *dptest.Store) {
	t.Helper()
	st := dptest.New()
	for _, c := range connectors {
		if err := st.Connectors().Create(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	return router.New(st.Connectors(), router.Options{CoolDown: coolDown, Now: now}), st
}

func healthy(name string) *store.Connector {
	return &store.Connector{TenantID: tenant, Name: name, Slug: name, Endpoint: "http://" + name, Status: router.StatusHealthy}
}

func TestParseToolName(t *testing.T) {
	cases := []struct {
		in            string
		conn, tool    string
		wantParseFail bool
	}{
		{in: "alpha__search", conn: "alpha", tool: "search"},
		// The split is on the FIRST "__" so a backend tool name may
		// itself contain the separator.
		{in: "alpha__get__all__groups", conn: "alpha", tool: "get__all__groups"},
		{in: "search", wantParseFail: true},
		{in: "__search", wantParseFail: true},
		{in: "alpha__", wantParseFail: true},
		{in: "", wantParseFail: true},
	}

	for _, c := range cases {
		conn, tool, err := router.ParseToolName(c.in)
		if c.wantParseFail {
			if !errors.Is(err, router.ErrBadToolName) {
				t.Errorf("ParseToolName(%q) = %v, want ErrBadToolName", c.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseToolName(%q) = %v", c.in, err)
			continue
		}
		if conn != c.conn || tool != c.tool {
			t.Errorf("ParseToolName(%q) = %q/%q, want %q/%q", c.in, conn, tool, c.conn, c.tool)
		}
	}
}

func TestParsePromptName(t *testing.T) {
	conn, prompt, err := router.ParsePromptName("alpha__get__all")
	if err != nil || conn != "alpha" || prompt != "get__all" {
		t.Errorf("ParsePromptName = %q/%q/%v, want alpha/get__all", conn, prompt, err)
	}
	for _, bad := range []string{"", "alpha", "__x", "alpha__"} {
		if _, _, err := router.ParsePromptName(bad); !errors.Is(err, router.ErrBadPromptName) {
			t.Errorf("ParsePromptName(%q) = %v, want ErrBadPromptName", bad, err)
		}
	}
}

func TestParseResourceURIReturnsTheBackendURIVerbatim(t *testing.T) {
	cases := []struct{ in, conn, uri string }{
		{"gw://alpha/test://static/resource/1", "alpha", "test://static/resource/1"},
		{"gw://alpha/file:///etc/x?y=1#z", "alpha", "file:///etc/x?y=1#z"},
		{"gw://alpha/gw://beta/nested", "alpha", "gw://beta/nested"},
		{"gw://alpha/plain-name", "alpha", "plain-name"},
	}
	for _, c := range cases {
		conn, uri, err := router.ParseResourceURI(c.in)
		if err != nil || conn != c.conn || uri != c.uri {
			t.Errorf("ParseResourceURI(%q) = %q/%q/%v, want %q/%q", c.in, conn, uri, err, c.conn, c.uri)
		}
	}
	for _, bad := range []string{"", "test://static/resource/1", "gw://", "gw://alpha", "gw://alpha/", "gw:///x"} {
		if _, _, err := router.ParseResourceURI(bad); !errors.Is(err, router.ErrBadResourceURI) {
			t.Errorf("ParseResourceURI(%q) = %v, want ErrBadResourceURI", bad, err)
		}
	}
}

func TestResolveFindsAConnectorInTheTenant(t *testing.T) {
	conn := healthy("alpha")
	r, _ := newRouter(t, time.Now, 0, conn)

	route, err := r.Resolve(context.Background(), tenant, "alpha__search")
	if err != nil {
		t.Fatal(err)
	}
	if route.Connector.ID != conn.ID || route.ToolName != "search" || route.Probe {
		t.Fatalf("unexpected route: %+v", route)
	}
}

func TestResolveDoesNotCrossTenants(t *testing.T) {
	r, _ := newRouter(t, time.Now, 0, healthy("alpha"))

	_, err := r.Resolve(context.Background(), "tenant-b", "alpha__search")
	if !errors.Is(err, router.ErrConnectorNotFound) {
		t.Fatalf("Resolve for another tenant = %v, want ErrConnectorNotFound", err)
	}
}

func TestResolveFallsBackToTheConnectorName(t *testing.T) {
	// The tool prefix is the connector's NAME; a deployment may have
	// slugged it differently.
	conn := &store.Connector{TenantID: tenant, Name: "Alpha Prod", Slug: "alpha-prod", Endpoint: "http://a", Status: router.StatusHealthy}
	r, _ := newRouter(t, time.Now, 0, conn)

	route, err := r.Resolve(context.Background(), tenant, "Alpha Prod__search")
	if err != nil {
		t.Fatal(err)
	}
	if route.Connector.ID != conn.ID {
		t.Fatalf("resolved to %q", route.Connector.ID)
	}
}

func TestUnhealthyConnectorIsRefusedThenProbedAfterTheCoolDown(t *testing.T) {
	// This is the recovery behaviour the gateway's predecessor lacked:
	// once a connector went unhealthy it stayed unreachable until
	// something re-ran initialize, in practice until a restart.
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	conn := healthy("alpha")
	conn.Status = router.StatusUnhealthy
	r, _ := newRouter(t, clock, 30*time.Second, conn)
	ctx := context.Background()

	// The first call after the connector was marked unhealthy is the
	// probe: the cool-down has not started ticking against it yet.
	route, err := r.Resolve(ctx, tenant, "alpha__search")
	if err != nil {
		t.Fatalf("the first attempt should be a probe: %v", err)
	}
	if !route.Probe {
		t.Error("the route was not flagged as a probe")
	}

	// A second attempt inside the window is refused.
	if _, err := r.Resolve(ctx, tenant, "alpha__search"); !errors.Is(err, router.ErrConnectorUnhealthy) {
		t.Fatalf("Resolve inside the cool-down = %v, want ErrConnectorUnhealthy", err)
	}

	now = now.Add(31 * time.Second)
	route, err = r.Resolve(ctx, tenant, "alpha__search")
	if err != nil {
		t.Fatalf("Resolve after the cool-down = %v, want a probe", err)
	}
	if !route.Probe {
		t.Error("the post-cool-down route was not flagged as a probe")
	}
}

func TestMarkHealthyClearsTheProbeWindow(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	conn := healthy("alpha")
	conn.Status = router.StatusUnhealthy
	r, st := newRouter(t, func() time.Time { return now }, 30*time.Second, conn)
	ctx := context.Background()

	route, err := r.Resolve(ctx, tenant, "alpha__search")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkHealthy(ctx, route.Connector); err != nil {
		t.Fatal(err)
	}

	persisted, err := st.Connectors().Get(ctx, tenant, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != router.StatusHealthy {
		t.Fatalf("status = %q, want healthy", persisted.Status)
	}

	// Recovery must be immediate, not after another cool-down.
	route, err = r.Resolve(ctx, tenant, "alpha__search")
	if err != nil {
		t.Fatal(err)
	}
	if route.Probe {
		t.Error("a recovered connector is still being treated as a probe")
	}
}

func TestMarkUnhealthyStartsTheCoolDown(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	conn := healthy("alpha")
	r, st := newRouter(t, func() time.Time { return now }, 30*time.Second, conn)
	ctx := context.Background()

	if err := r.MarkUnhealthy(ctx, conn); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.Connectors().Get(ctx, tenant, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != router.StatusUnhealthy {
		t.Fatalf("status = %q, want unhealthy", persisted.Status)
	}

	if _, err := r.Resolve(ctx, tenant, "alpha__search"); !errors.Is(err, router.ErrConnectorUnhealthy) {
		t.Fatalf("Resolve right after MarkUnhealthy = %v, want ErrConnectorUnhealthy", err)
	}
}

func TestCallableSkipsCoolingConnectorsAndKeepsUnknownOnes(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	good := healthy("alpha")
	unknown := healthy("beta")
	unknown.Status = router.StatusUnknown
	bad := healthy("gamma")

	r, _ := newRouter(t, func() time.Time { return now }, 30*time.Second, good, unknown, bad)
	ctx := context.Background()

	if err := r.MarkUnhealthy(ctx, bad); err != nil {
		t.Fatal(err)
	}

	callable, err := r.Callable(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range callable {
		names[c.Name] = true
	}
	if !names["alpha"] {
		t.Error("a healthy connector was skipped")
	}
	// Never initialized is not the same as failed.
	if !names["beta"] {
		t.Error("an unknown-status connector was skipped")
	}
	if names["gamma"] {
		t.Error("a cooling-down connector was included")
	}
}

func TestStampOverridesWinsOverCallerArguments(t *testing.T) {
	// The overrides carry tenant constants. If a caller's value won, a
	// model that guessed one could read across a tenant boundary.
	conn := healthy("alpha")
	conn.Metadata = map[string]any{
		"tool_arg_overrides": map[string]any{"project_id": "tenant-a-project", "region": "eu"},
	}

	args := router.StampOverrides(conn, "search", map[string]any{"query": "x", "project_id": "somebody-elses"})

	if args["project_id"] != "tenant-a-project" {
		t.Errorf("project_id = %v, want the connector's override", args["project_id"])
	}
	if args["region"] != "eu" {
		t.Errorf("region = %v, want it stamped in", args["region"])
	}
	if args["query"] != "x" {
		t.Errorf("query = %v, want the caller's value preserved", args["query"])
	}
}

func TestStampOverridesDoesNotMutateTheCallersMap(t *testing.T) {
	conn := healthy("alpha")
	conn.Metadata = map[string]any{"tool_arg_overrides": map[string]any{"region": "eu"}}

	original := map[string]any{"query": "x"}
	router.StampOverrides(conn, "search", original)

	if _, leaked := original["region"]; leaked {
		t.Fatal("StampOverrides wrote into the caller's arguments")
	}
}

func TestStampOverridesIsANoOpWithoutOverrides(t *testing.T) {
	args := map[string]any{"query": "x"}
	if got := router.StampOverrides(healthy("alpha"), "search", args); len(got) != 1 || got["query"] != "x" {
		t.Fatalf("got %v", got)
	}
}

func TestStampOverrides_NestedPerTool(t *testing.T) {
	conn := healthy("alpha")
	conn.Metadata = map[string]any{"tool_arg_overrides": map[string]any{
		"region": "eu",                                  // flat: every tool
		"search": map[string]any{"customer_id": "acme"}, // only search
	}}
	got := router.StampOverrides(conn, "search", map[string]any{"q": "x", "customer_id": "guess"})
	if got["region"] != "eu" || got["customer_id"] != "acme" || got["q"] != "x" {
		t.Fatalf("search args = %v", got)
	}
	other := router.StampOverrides(conn, "lookup", map[string]any{"q": "y"})
	if other["region"] != "eu" || other["customer_id"] != nil {
		t.Fatalf("lookup args = %v (per-tool block must not leak)", other)
	}
}

func TestDisabledConnectorIsNeitherResolvedNorCallable(t *testing.T) {
	off := healthy("off")
	off.Metadata = map[string]any{"enabled": false}
	on := healthy("on")
	on.Metadata = map[string]any{"enabled": true}
	r, _ := newRouter(t, time.Now, 0, off, on, healthy("legacy"))

	if _, err := r.Resolve(context.Background(), tenant, "off__search"); !errors.Is(err, router.ErrConnectorNotFound) {
		t.Fatalf("Resolve(disabled) = %v, want ErrConnectorNotFound", err)
	}
	if _, err := r.Resolve(context.Background(), tenant, "on__search"); err != nil {
		t.Fatalf("Resolve(enabled) = %v", err)
	}
	if _, err := r.Resolve(context.Background(), tenant, "legacy__search"); err != nil {
		t.Fatalf("a connector without the flag must stay enabled: %v", err)
	}
	got, err := r.Callable(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Callable = %d connectors, want 2 (disabled one skipped)", len(got))
	}
	for _, c := range got {
		if c.Name == "off" {
			t.Fatal("Callable returned the disabled connector")
		}
	}
}
