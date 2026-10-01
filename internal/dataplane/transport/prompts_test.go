package transport_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// addConnector registers a second backend under slug in the fixture's
// tenant. The fixture's "Reader" profile grants nothing on it.
func (f *fixture) addConnector(t *testing.T, slug string, opts dptest.BackendOptions) (*dptest.Backend, *store.Connector) {
	t.Helper()
	backend := dptest.NewBackend(opts)
	t.Cleanup(backend.Close)

	conn := &store.Connector{
		TenantID: tenantID, Name: slug, Slug: slug,
		Endpoint: backend.URL, TimeoutMS: 3000, Status: "unknown", Metadata: map[string]any{},
	}
	if err := f.store.Connectors().Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	return backend, conn
}

func catalogOptions() dptest.BackendOptions {
	return dptest.BackendOptions{
		Tools:             []mcp.Tool{{Name: "echo", InputSchema: dptest.ObjectSchema(nil, "message")}},
		Prompts:           dptest.SamplePrompts(),
		Resources:         dptest.SampleResources(),
		ResourceTemplates: dptest.SampleResourceTemplates(),
		RequireSession:    true,
	}
}

func rpc(method string, id int, params string) mcp.Request {
	req := mcp.Request{JSONRPC: mcp.Version, ID: id, Method: method}
	if params != "" {
		req.Params = json.RawMessage(params)
	}
	return req
}

func decodeResult[T any](t *testing.T, resp mcp.Response) T {
	t.Helper()
	var out T
	if resp.Error != nil {
		t.Fatalf("request failed: %+v", resp.Error)
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func promptNames(t *testing.T, resp mcp.Response) []string {
	t.Helper()
	names := []string{}
	for _, p := range decodeResult[mcp.PromptsListResult](t, resp).Prompts {
		names = append(names, p.Name)
	}
	return names
}

func resourceURIs(t *testing.T, resp mcp.Response) []string {
	t.Helper()
	uris := []string{}
	for _, r := range decodeResult[mcp.ResourcesListResult](t, resp).Resources {
		uris = append(uris, r.URI)
	}
	return uris
}

func wantCode(t *testing.T, resp mcp.Response, code int) {
	t.Helper()
	if resp.Error == nil || resp.Error.Code != code {
		t.Fatalf("error = %+v, want code %d", resp.Error, code)
	}
}

// ---------------------------------------------------------------------------
// capabilities
// ---------------------------------------------------------------------------

// servedBy names, per server capability family, a method the gateway
// must serve if it advertises that family. A family with no entry here
// cannot be advertised at all.
var servedBy = map[string]string{
	"tools":     mcp.MethodToolsList,
	"prompts":   mcp.MethodPromptsList,
	"resources": mcp.MethodResourcesList,
}

func TestEveryAdvertisedCapabilityHasAServedMethod(t *testing.T) {
	// Advertising a family the gateway then answers with -32601 is what
	// made Claude Code's prompts/list and resources/list fail on
	// connect: the client believes the capability object.
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, rpc(mcp.MethodInitialize, 1,
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`), nil)
	result := decodeResult[struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}](t, decoded)

	for _, family := range []string{"tools", "prompts", "resources"} {
		if _, ok := result.Capabilities[family]; !ok {
			t.Errorf("capability %q not advertised although the backend has it", family)
		}
	}
	for family := range result.Capabilities {
		method, ok := servedBy[family]
		if !ok {
			t.Errorf("capability %q is advertised but no served method is known for it", family)
			continue
		}
		_, resp := f.call(t, rpc(method, 2, ""), nil)
		if resp.Error != nil && resp.Error.Code == mcp.ErrorCodeMethodNotFound {
			t.Errorf("capability %q is advertised but %s answers -32601", family, method)
		}
	}

	// The backend reports resources.subscribe and listChanged for both
	// families, so the gateway -- which relays them -- advertises them.
	if got := string(result.Capabilities["prompts"]); got != `{"listChanged":true}` {
		t.Errorf("capability prompts = %s, want {\"listChanged\":true}", got)
	}
	if got := string(result.Capabilities["resources"]); got != `{"subscribe":true,"listChanged":true}` {
		t.Errorf("capability resources = %s, want subscribe and listChanged", got)
	}

	// resources.subscribe promises resources/subscribe: it must be served
	// (a session is required, so ask on one).
	var resources mcp.ResourcesCapability
	if err := json.Unmarshal(result.Capabilities["resources"], &resources); err != nil {
		t.Fatal(err)
	}
	if resources.Subscribe {
		sessionID := f.initialize(t)
		_, resp := f.call(t, rpc(mcp.MethodResourcesSubscribe, 3, `{"uri":"gw://alpha/test://static/resource/1"}`),
			map[string]string{mcp.HeaderSessionID: sessionID})
		if resp.Error != nil && resp.Error.Code == mcp.ErrorCodeMethodNotFound {
			t.Errorf("resources.subscribe is advertised but resources/subscribe answers -32601: %+v", resp.Error)
		}
	}
}

func TestCapabilityFlagsAreAdvertisedOnlyWhenABackendReportsThem(t *testing.T) {
	// A backend with prompts and resources but no subscribe and no
	// list_changed: the gateway has nothing to relay, so promises none.
	f := newFixture(t, fixtureOptions{PlainCatalogCapabilities: true})

	_, decoded := f.call(t, rpc(mcp.MethodInitialize, 1,
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`), nil)
	result := decodeResult[struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}](t, decoded)

	for _, family := range []string{"prompts", "resources"} {
		if got := string(result.Capabilities[family]); got != "{}" {
			t.Errorf("capability %s = %s, want {}", family, got)
		}
	}
}

func TestPromptsAndResourcesAreNotAdvertisedWithoutABackendThatHasThem(t *testing.T) {
	f := newFixture(t, fixtureOptions{NoCatalog: true})

	_, decoded := f.call(t, rpc(mcp.MethodInitialize, 1,
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`), nil)
	result := decodeResult[mcp.InitializeResult](t, decoded)

	if result.Capabilities.Prompts != nil || result.Capabilities.Resources != nil {
		t.Fatalf("capabilities = %+v, want tools only", result.Capabilities)
	}
}

// ---------------------------------------------------------------------------
// */list
// ---------------------------------------------------------------------------

func TestPromptsListMergesEveryConnectorAndNamespacesNames(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.addConnector(t, "beta", catalogOptions())

	// An inbound cursor is ignored: the answer is always one merged page.
	_, decoded := f.call(t, rpc(mcp.MethodPromptsList, 1, `{"cursor":"whatever"}`), nil)

	got := promptNames(t, decoded)
	want := []string{"alpha__greet", "alpha__simple", "beta__greet", "beta__simple"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prompts = %v, want %v", got, want)
	}
	if strings.Contains(string(decoded.Result), "nextCursor") {
		t.Errorf("merged list carries a nextCursor: %s", decoded.Result)
	}
	// alpha pages one prompt at a time: both pages were followed.
	if n := f.backend.Calls(mcp.MethodPromptsList); n != 2 {
		t.Errorf("alpha saw %d prompts/list calls, want 2 (one per page)", n)
	}

	// Arguments survive the round trip.
	for _, p := range decodeResult[mcp.PromptsListResult](t, decoded).Prompts {
		if p.Name == "alpha__greet" && (len(p.Arguments) != 1 || !p.Arguments[0].Required || p.Title != "Greeting") {
			t.Errorf("alpha__greet = %+v, want its title and required argument", p)
		}
	}
}

func TestResourcesListWrapsEveryURIInTheGatewayNamespace(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.addConnector(t, "beta", catalogOptions())

	_, decoded := f.call(t, rpc(mcp.MethodResourcesList, 1, ""), nil)

	got := resourceURIs(t, decoded)
	want := []string{
		"gw://alpha/file:///var/data/report.md",
		"gw://alpha/test://static/resource/1",
		"gw://beta/file:///var/data/report.md",
		"gw://beta/test://static/resource/1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resources = %v, want %v", got, want)
	}

	// Everything but the URI passes through untouched.
	for _, r := range decodeResult[mcp.ResourcesListResult](t, decoded).Resources {
		switch r.URI {
		case "gw://alpha/test://static/resource/1":
			if r.Size == nil || *r.Size != 42 || r.MimeType != "text/plain" {
				t.Errorf("%s = %+v, want size and mimeType kept", r.URI, r)
			}
		case "gw://alpha/file:///var/data/report.md":
			if string(r.Annotations) != `{"audience":["user"],"priority":0.5}` {
				t.Errorf("%s annotations = %s, want them verbatim", r.URI, r.Annotations)
			}
		}
	}
}

func TestResourceTemplatesListWrapsTheTemplate(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, rpc(mcp.MethodResourcesTemplatesList, 1, ""), nil)
	templates := decodeResult[mcp.ResourceTemplatesListResult](t, decoded).ResourceTemplates

	if len(templates) != 1 || templates[0].URITemplate != "gw://alpha/test://dynamic/resource/{id}" {
		t.Fatalf("templates = %+v, want the one template, namespaced", templates)
	}
}

func TestAConnectorWithoutTheCapabilityIsNotAsked(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	toolsOnly, _ := f.addConnector(t, "beta", dptest.BackendOptions{
		Tools: []mcp.Tool{{Name: "echo", InputSchema: dptest.ObjectSchema(nil)}}, RequireSession: true,
	})

	_, decoded := f.call(t, rpc(mcp.MethodPromptsList, 1, ""), nil)
	if got := promptNames(t, decoded); len(got) != 2 {
		t.Fatalf("prompts = %v, want alpha's two", got)
	}
	if n := toolsOnly.Calls(mcp.MethodPromptsList); n != 0 {
		t.Errorf("a backend that declared no prompts capability got %d prompts/list calls", n)
	}
}

func TestAnEmptyCatalogIsAnEmptyArray(t *testing.T) {
	f := newFixture(t, fixtureOptions{NoCatalog: true})

	for method, key := range map[string]string{
		mcp.MethodPromptsList:            `"prompts":[]`,
		mcp.MethodResourcesList:          `"resources":[]`,
		mcp.MethodResourcesTemplatesList: `"resourceTemplates":[]`,
	} {
		_, decoded := f.call(t, rpc(method, 1, ""), nil)
		if decoded.Error != nil || !strings.Contains(string(decoded.Result), key) {
			t.Errorf("%s = %s / %+v, want %s", method, decoded.Result, decoded.Error, key)
		}
	}
}

// ---------------------------------------------------------------------------
// prompts/get, resources/read
// ---------------------------------------------------------------------------

func TestPromptsGetRoutesTheUnprefixedNameAndArguments(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	resp, decoded := f.call(t, rpc(mcp.MethodPromptsGet, 1,
		`{"name":"alpha__greet","arguments":{"who":"ann"}}`), nil)

	result := decodeResult[mcp.PromptsGetResult](t, decoded)
	if len(result.Messages) != 1 || result.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", result.Messages)
	}
	// Content is passed through verbatim.
	if got := string(result.Messages[0].Content); got != `{"type":"text","text":"greet:{\"who\":\"ann\"}"}` {
		t.Errorf("content = %s", got)
	}
	if got := f.backend.PromptArgs("greet"); got["who"] != "ann" {
		t.Errorf("backend saw arguments %v for the unprefixed prompt, want who=ann", got)
	}
	if got := resp.Header.Get("X-Connector-ID"); got != f.conn.ID {
		t.Errorf("X-Connector-ID = %q, want %q", got, f.conn.ID)
	}
}

func TestResourcesReadStripsTheNamespace(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	for _, tc := range []struct{ qualified, backend string }{
		{"gw://alpha/test://static/resource/1", "test://static/resource/1"},
		{"gw://alpha/file:///var/data/report.md", "file:///var/data/report.md"},
		// An expansion of the advertised template routes the same way.
		{"gw://alpha/test://dynamic/resource/7", "test://dynamic/resource/7"},
	} {
		params, _ := json.Marshal(mcp.ResourcesReadParams{URI: tc.qualified})
		_, decoded := f.call(t, rpc(mcp.MethodResourcesRead, 1, string(params)), nil)

		result := decodeResult[mcp.ResourcesReadResult](t, decoded)
		if len(result.Contents) != 1 || result.Contents[0].Text == nil ||
			*result.Contents[0].Text != "contents of "+tc.backend {
			t.Errorf("read %s: contents = %+v", tc.qualified, result.Contents)
		}
		uris := f.backend.ReadURIs()
		if last := uris[len(uris)-1]; last != tc.backend {
			t.Errorf("read %s: backend saw %q, want %q", tc.qualified, last, tc.backend)
		}
	}
}

func TestABackendRefusalIsForwardedAndLeavesTheConnectorHealthy(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, rpc(mcp.MethodPromptsGet, 1, `{"name":"alpha__nope"}`), nil)
	wantCode(t, decoded, mcp.ErrorCodeInvalidParams)

	_, decoded = f.call(t, rpc(mcp.MethodResourcesRead, 2, `{"uri":"gw://alpha/test://missing"}`), nil)
	wantCode(t, decoded, -32002) // the spec's "resource not found", from the backend

	conn, err := f.store.Connectors().Get(context.Background(), tenantID, f.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Status != "healthy" {
		t.Errorf("connector status = %q, want healthy: the backend answered", conn.Status)
	}
}

func TestAnUnknownConnectorIs32004(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, rpc(mcp.MethodPromptsGet, 1, `{"name":"gamma__greet"}`), nil)
	wantCode(t, decoded, mcp.ErrorCodeConnectorNotFound)

	_, decoded = f.call(t, rpc(mcp.MethodResourcesRead, 2, `{"uri":"gw://gamma/test://static/resource/1"}`), nil)
	wantCode(t, decoded, mcp.ErrorCodeConnectorNotFound)
}

func TestAMalformedNameOrURIIsInvalidParams(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	for _, tc := range []struct{ method, params string }{
		// A prompts/get name with no "__" is no longer malformed on its
		// own -- Phase 2 repurposes that shape as a native command name
		// (see TestAPromptNameWithNoConnectorPrefixIsANativeCommand) --
		// so only an outright missing "name" stays -32602 here.
		{mcp.MethodPromptsGet, `{}`},
		// The backend's own URI, without the gateway namespace.
		{mcp.MethodResourcesRead, `{"uri":"test://static/resource/1"}`},
		{mcp.MethodResourcesRead, `{"uri":"gw://alpha/"}`},
		{mcp.MethodResourcesRead, `{}`},
	} {
		_, decoded := f.call(t, rpc(tc.method, 1, tc.params), nil)
		if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeInvalidParams {
			t.Errorf("%s %s: error = %+v, want -32602", tc.method, tc.params, decoded.Error)
		}
	}
	if n := len(f.backend.ReadURIs()); n != 0 {
		t.Errorf("a malformed request reached the backend %d time(s)", n)
	}
}

// TestAPromptNameWithNoConnectorPrefixIsANativeCommand documents the
// Phase 2 behavior change TestAMalformedNameOrURIIsInvalidParams's
// trimmed case used to cover: "greet" is syntactically a native command
// name (no "__"), so it is denied as "not attached to this profile"
// (-32003), never as malformed (-32602) -- see docs/profiles.md.
func TestAPromptNameWithNoConnectorPrefixIsANativeCommand(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, rpc(mcp.MethodPromptsGet, 1, `{"name":"greet"}`), nil)
	wantCode(t, decoded, mcp.ErrorCodeToolNotAllowed)
	if n := len(f.backend.ReadURIs()); n != 0 {
		t.Errorf("a native command name reached the backend %d time(s)", n)
	}
}

// ---------------------------------------------------------------------------
// profile scoping
// ---------------------------------------------------------------------------

func TestTheProfileScopesPromptsAndResourcesByConnector(t *testing.T) {
	// Reader grants one tool on alpha and none on beta: alpha's prompts
	// and resources are in scope, all of them, and beta's are not.
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})
	beta, _ := f.addConnector(t, "beta", catalogOptions())
	reader := map[string]string{profile.Header: "Reader"}

	_, decoded := f.call(t, rpc(mcp.MethodPromptsList, 1, ""), reader)
	if got := promptNames(t, decoded); !reflect.DeepEqual(got, []string{"alpha__greet", "alpha__simple"}) {
		t.Errorf("prompts = %v, want alpha's only", got)
	}
	_, decoded = f.call(t, rpc(mcp.MethodResourcesList, 2, ""), reader)
	if got := resourceURIs(t, decoded); len(got) != 2 || !strings.HasPrefix(got[0], "gw://alpha/") {
		t.Errorf("resources = %v, want alpha's only", got)
	}
	if n := beta.Calls(mcp.MethodPromptsList) + beta.Calls(mcp.MethodResourcesList); n != 0 {
		t.Errorf("a connector outside the profile was asked to list %d time(s)", n)
	}

	_, decoded = f.call(t, rpc(mcp.MethodPromptsGet, 3, `{"name":"alpha__simple"}`), reader)
	if decoded.Error != nil {
		t.Fatalf("prompts/get on a granted connector failed: %+v", decoded.Error)
	}

	_, decoded = f.call(t, rpc(mcp.MethodPromptsGet, 4, `{"name":"beta__greet","arguments":{"who":"x"}}`), reader)
	wantCode(t, decoded, mcp.ErrorCodeToolNotAllowed)
	if !strings.Contains(decoded.Error.Message, `connector "beta"`) {
		t.Errorf("denial message %q does not name the connector", decoded.Error.Message)
	}
	_, decoded = f.call(t, rpc(mcp.MethodResourcesRead, 5, `{"uri":"gw://beta/test://static/resource/1"}`), reader)
	wantCode(t, decoded, mcp.ErrorCodeToolNotAllowed)

	if beta.PromptArgs("greet") != nil || len(beta.ReadURIs()) != 0 {
		t.Fatal("a denied prompts/get or resources/read still reached the backend")
	}
}

func TestAnUnknownProfileSeesNoPromptsOrResources(t *testing.T) {
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})
	typo := map[string]string{profile.Header: "Typo"}

	_, decoded := f.call(t, rpc(mcp.MethodPromptsList, 1, ""), typo)
	if got := promptNames(t, decoded); len(got) != 0 {
		t.Errorf("prompts = %v, want []", got)
	}
	_, decoded = f.call(t, rpc(mcp.MethodResourcesList, 2, ""), typo)
	if got := resourceURIs(t, decoded); len(got) != 0 {
		t.Errorf("resources = %v, want []", got)
	}
	_, decoded = f.call(t, rpc(mcp.MethodPromptsGet, 3, `{"name":"alpha__simple"}`), typo)
	wantCode(t, decoded, mcp.ErrorCodeToolNotAllowed)
	_, decoded = f.call(t, rpc(mcp.MethodResourcesRead, 4, `{"uri":"gw://alpha/test://static/resource/1"}`), typo)
	wantCode(t, decoded, mcp.ErrorCodeToolNotAllowed)
}

func TestRequireProfileAppliesToPromptsAndResources(t *testing.T) {
	f := newFixture(t, fixtureOptions{RequireProfile: true, ProfileTools: []string{"echo"}})

	for _, method := range []string{mcp.MethodPromptsList, mcp.MethodResourcesList, mcp.MethodResourcesTemplatesList} {
		_, decoded := f.call(t, rpc(method, 1, ""), nil)
		if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeToolNotAllowed {
			t.Errorf("%s without a profile: error = %+v, want -32003", method, decoded.Error)
		}
	}
}

// ---------------------------------------------------------------------------
// access log
// ---------------------------------------------------------------------------

func TestPromptsGetAndResourcesReadAreAttributedInTheAccessLog(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	f.call(t, rpc(mcp.MethodPromptsGet, 1, `{"name":"alpha__simple"}`), nil)
	f.call(t, rpc(mcp.MethodResourcesRead, 2, `{"uri":"gw://alpha/test://static/resource/1"}`), nil)

	records := f.sink.all()
	if len(records) != 2 {
		t.Fatalf("got %d access-log records, want 2", len(records))
	}
	for i, want := range []struct{ method, tool string }{
		{mcp.MethodPromptsGet, "alpha__simple"},
		{mcp.MethodResourcesRead, "gw://alpha/test://static/resource/1"},
	} {
		r := records[i]
		if r.Method != want.method || r.ToolName != want.tool || r.ConnectorID != f.conn.ID {
			t.Errorf("record %d = method %q tool %q connector %q, want %q %q %q",
				i, r.Method, r.ToolName, r.ConnectorID, want.method, want.tool, f.conn.ID)
		}
	}
}
