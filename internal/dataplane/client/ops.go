package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// gatewayImplementation is how the gateway identifies itself to backends.
var gatewayImplementation = mcp.Implementation{Name: "tusk-ai-secured-gateway", Version: "0.1.0"}

// initializeParams is mcp.InitializeParams with the capabilities kept
// raw, so what an agent declared reaches the backend exactly as it was
// written.
type initializeParams struct {
	ProtocolVersion string                     `json:"protocolVersion"`
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	ClientInfo      mcp.Implementation         `json:"clientInfo"`
}

// Initialize performs the MCP handshake with a backend and returns its
// declared capabilities alongside the session handle to use from now on.
//
// The client capabilities declared are call.ClientCapabilities and
// nothing else: the gateway invites a server-initiated request only when
// it can relay it to an agent that declared it (see the orchestrator's
// relay), so by default it declares none.
func (c *Client) Initialize(ctx context.Context, call Call) (*mcp.InitializeResult, *Result, error) {
	params := initializeParams{
		// Offer the same version the transport is negotiating; on a
		// downgrade the whole exchange is replayed with the older one,
		// so the body and the header never disagree.
		ProtocolVersion: mcp.ProtocolVersion,
		ClientInfo:      gatewayImplementation,
		Capabilities:    call.ClientCapabilities,
	}
	if params.Capabilities == nil {
		params.Capabilities = map[string]json.RawMessage{}
	}
	if call.ProtocolVersion != "" {
		params.ProtocolVersion = call.ProtocolVersion
	}

	raw, err := json.Marshal(params)
	if err != nil {
		return nil, nil, fmt.Errorf("client: marshal initialize params: %w", err)
	}

	call.Request = &mcp.Request{JSONRPC: mcp.Version, ID: "initialize", Method: mcp.MethodInitialize, Params: raw}

	// On a downgrade the negotiated version changes mid-flight, so the
	// body's protocolVersion has to follow it. Do that by letting the
	// transport negotiate and, if it landed on a different version than
	// the body claimed, replaying once with a matching body.
	result, err := c.Do(ctx, call)
	if err != nil {
		return nil, result, err
	}
	if result.ProtocolVersion != params.ProtocolVersion {
		params.ProtocolVersion = result.ProtocolVersion
		if raw, err = json.Marshal(params); err != nil {
			return nil, result, fmt.Errorf("client: marshal initialize params: %w", err)
		}
		call.Request.Params = raw
		call.ProtocolVersion = result.ProtocolVersion
		call.SessionID = result.SessionID
		if result, err = c.Do(ctx, call); err != nil {
			return nil, result, err
		}
	}

	if result.Response == nil {
		return nil, result, fmt.Errorf("client: initialize returned no response")
	}
	if result.Response.Error != nil {
		return nil, result, fmt.Errorf("client: initialize failed: %s", result.Response.Error.Message)
	}

	var out mcp.InitializeResult
	if err := json.Unmarshal(result.Response.Result, &out); err != nil {
		return nil, result, fmt.Errorf("client: decode initialize result: %w", err)
	}
	return &out, result, nil
}

// SendInitialized sends the post-handshake notification. The MCP spec
// requires it before any other request, and a backend that tracks
// sessions requires the session header on it.
func (c *Client) SendInitialized(ctx context.Context, call Call) error {
	call.Request = &mcp.Request{
		JSONRPC: mcp.Version,
		Method:  mcp.MethodInitialized,
		Params:  json.RawMessage(`{}`),
	}
	_, err := c.Do(ctx, call)
	return err
}

// ListTools fetches a backend's tools, prefixed and scrubbed for
// exposure to gateway clients.
//
// Two transformations happen here, and only here, so that both the live
// path and the cache-population path get them:
//
//   - every tool name becomes "<connector name>__<tool name>", which is
//     what makes a flat tool namespace across many backends routable;
//   - every argument the gateway stamps itself (connector
//     metadata.tool_arg_overrides) is deleted from the advertised
//     schema, because advertising an argument the gateway is going to
//     overwrite only invites a model to guess at a tenant constant.
func (c *Client) ListTools(ctx context.Context, call Call) ([]mcp.Tool, *Result, error) {
	call.Request = &mcp.Request{JSONRPC: mcp.Version, ID: "tools/list", Method: mcp.MethodToolsList}

	result, err := c.Do(ctx, call)
	if err != nil {
		return nil, result, err
	}
	if result.Response == nil {
		return nil, result, fmt.Errorf("client: tools/list returned no response")
	}
	if result.Response.Error != nil {
		return nil, result, fmt.Errorf("client: tools/list failed: %s", result.Response.Error.Message)
	}

	var out mcp.ToolsListResult
	if err := json.Unmarshal(result.Response.Result, &out); err != nil {
		return nil, result, fmt.Errorf("client: decode tools/list result: %w", err)
	}

	tools := make([]mcp.Tool, 0, len(out.Tools))
	for _, tool := range out.Tools {
		overrides := ToolArgOverrides(call.Connector, tool.Name)
		tool.Name = QualifyTool(Qualifier(call.Connector), tool.Name)
		if len(overrides) > 0 {
			tool.InputSchema = scrubOverridden(tool.InputSchema, overrides)
		}
		tools = append(tools, tool)
	}

	return tools, result, nil
}

// CallTool runs one tool on a backend. name is the backend's own,
// unprefixed tool name. call.RequestID, when set, is the upstream
// JSON-RPC id, and call.Meta is forwarded as params._meta.
func (c *Client) CallTool(ctx context.Context, call Call, name string, args map[string]any) (*mcp.ToolsCallResult, *Result, error) {
	raw, err := json.Marshal(mcp.ToolsCallParams{Name: name, Arguments: args, Meta: call.Meta})
	if err != nil {
		return nil, nil, fmt.Errorf("client: marshal tools/call params: %w", err)
	}
	id := call.RequestID
	if id == nil {
		id = "tools/call:" + name
	}
	call.Request = &mcp.Request{
		JSONRPC: mcp.Version,
		ID:      id,
		Method:  mcp.MethodToolsCall,
		Params:  raw,
	}

	result, err := c.Do(ctx, call)
	if err != nil {
		return nil, result, err
	}
	if result.Response == nil {
		return nil, result, fmt.Errorf("client: tools/call returned no response")
	}
	if result.Response.Error != nil {
		return nil, result, fmt.Errorf("client: tools/call failed: %s", result.Response.Error.Message)
	}

	var out mcp.ToolsCallResult
	if err := json.Unmarshal(result.Response.Result, &out); err != nil {
		return nil, result, fmt.Errorf("client: decode tools/call result: %w", err)
	}
	if out.Content == nil {
		out.Content = []mcp.Content{}
	}
	return &out, result, nil
}

// SendCancelled tells a backend to abandon the request it knows as
// requestID. It is a notification: the backend answers nothing, and a
// backend that does not implement cancellation just ignores it.
func (c *Client) SendCancelled(ctx context.Context, call Call, requestID any, reason string) error {
	rawID, err := json.Marshal(requestID)
	if err != nil {
		return fmt.Errorf("client: marshal request id: %w", err)
	}
	raw, err := json.Marshal(mcp.CancelledParams{RequestID: rawID, Reason: reason})
	if err != nil {
		return fmt.Errorf("client: marshal notifications/cancelled params: %w", err)
	}
	call.Request = &mcp.Request{JSONRPC: mcp.Version, Method: mcp.NotificationCancelled, Params: raw}
	_, err = c.Do(ctx, call)
	return err
}

// QualifyTool renders the gateway-visible name of a backend tool.
func QualifyTool(connectorName, toolName string) string {
	return connectorName + "__" + toolName
}

// ToolArgOverrides reads connector.metadata.tool_arg_overrides: the
// arguments the gateway stamps onto every call to this connector.
//
// Upstream servers often require tenant constants (a project id, a
// customer id, a region) as ordinary tool arguments. Stamping them
// server-side is what stops a model from guessing one, and what stops a
// guessed one from reaching another tenant's data.
//
// Two shapes are accepted and merged, per-tool entries winning:
//
//	{"customerId": "acme"}                       // flat: every tool of the connector
//	{"search": {"customerId": "acme"}}           // nested: only the tool named "search"
//
// toolName is the backend's (unqualified) tool name. A flat value is
// anything that is not a JSON object; an object value is treated as a
// per-tool block, so a flat argument whose value is itself an object
// must be expressed under a tool name.
func ToolArgOverrides(conn *store.Connector, toolName string) map[string]any {
	if conn == nil {
		return nil
	}
	ov, ok := conn.Metadata["tool_arg_overrides"].(map[string]any)
	if !ok || len(ov) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range ov {
		if _, isBlock := v.(map[string]any); isBlock {
			continue
		}
		out[k] = v
	}
	if block, ok := ov[toolName].(map[string]any); ok {
		for k, v := range block {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ServerRequestPolicy reports which server-initiated MCP requests this
// connector may send to agents: sampling/createMessage (run an LLM
// completion on the agent's own model and budget), elicitation/create
// (ask the human operating the agent for input), and roots/list (ask
// which directories the agent is working in). All three are trust
// decisions made by whoever configured the connector, so every field
// defaults to false.
type ServerRequestPolicy struct{ Sampling, Elicitation, Roots bool }

// Allows reports whether method ("sampling/createMessage",
// "elicitation/create", or "roots/list") is permitted by p. Any other
// method name -- including a typo or a future addition this build
// doesn't know about -- is denied rather than guessed at.
func (p ServerRequestPolicy) Allows(method string) bool {
	switch method {
	case "sampling/createMessage":
		return p.Sampling
	case "elicitation/create":
		return p.Elicitation
	case "roots/list":
		return p.Roots
	default:
		return false
	}
}

// ServerRequests reads connector.metadata.server_requests: the
// per-connector policy for server-initiated MCP requests (see
// ServerRequestPolicy). Absent metadata, an absent or non-object
// server_requests, and unrecognized or non-boolean fields inside it are
// all treated as false -- validating the *shape* (unknown keys,
// non-boolean values) is the API's job at write time
// (internal/api/handlers/connectors.go); this accessor stays
// permissive on read so a malformed or pre-existing row never panics or
// misbehaves the data plane, it just denies.
func ServerRequests(conn *store.Connector) ServerRequestPolicy {
	if conn == nil {
		return ServerRequestPolicy{}
	}
	sr, ok := conn.Metadata["server_requests"].(map[string]any)
	if !ok {
		return ServerRequestPolicy{}
	}
	sampling, _ := sr["sampling"].(bool)
	elicitation, _ := sr["elicitation"].(bool)
	roots, _ := sr["roots"].(bool)
	return ServerRequestPolicy{Sampling: sampling, Elicitation: elicitation, Roots: roots}
}

// scrubOverridden returns a copy of schema with every overridden
// argument removed from its properties and its required list.
func scrubOverridden(schema mcp.InputSchema, overrides map[string]any) mcp.InputSchema {
	out := schema.Clone()
	for name := range overrides {
		delete(out.Properties, name)
	}
	if len(out.Required) > 0 {
		required := make([]string, 0, len(out.Required))
		for _, name := range out.Required {
			if _, overridden := overrides[name]; !overridden {
				required = append(required, name)
			}
		}
		out.Required = required
	}
	return out
}

// Qualifier is the prefix tools of conn are advertised under: the slug
// (lowercase, [a-z0-9-]) so the qualified name stays a valid MCP tool
// name even when the display name has spaces or capitals. Older rows
// without a slug fall back to the name.
func Qualifier(conn *store.Connector) string {
	if conn.Slug != "" {
		return conn.Slug
	}
	return conn.Name
}
