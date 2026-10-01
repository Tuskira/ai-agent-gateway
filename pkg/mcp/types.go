package mcp

import "encoding/json"

// Protocol versions. ProtocolVersion is what the gateway advertises to
// its own clients and tries first against a backend; ProtocolVersionLegacy
// is the fallback for backends that reject the newer revision (see
// internal/dataplane/client's negotiation).
const (
	ProtocolVersion       = "2025-06-18"
	ProtocolVersionLegacy = "2024-11-05"

	// HeaderProtocolVersion and HeaderSessionID are the two MCP-defined
	// HTTP headers the gateway both reads and writes. Their canonical
	// wire spelling is lowercase; net/http canonicalizes on Set and on
	// Get, so either spelling works with the stdlib helpers.
	HeaderProtocolVersion = "Mcp-Protocol-Version"
	HeaderSessionID       = "Mcp-Session-Id"
)

// Implementation identifies a client or server by name and version.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCapabilities describes what a client supports.
type ClientCapabilities struct {
	Experimental map[string]any   `json:"experimental,omitempty"`
	Sampling     map[string]any   `json:"sampling,omitempty"`
	Roots        *RootsCapability `json:"roots,omitempty"`
}

// ServerCapabilities describes what a server supports.
type ServerCapabilities struct {
	Experimental map[string]any       `json:"experimental,omitempty"`
	Logging      map[string]any       `json:"logging,omitempty"`
	Prompts      *PromptsCapability   `json:"prompts,omitempty"`
	Resources    *ResourcesCapability `json:"resources,omitempty"`
	Tools        *ToolsCapability     `json:"tools,omitempty"`
	// Extensions carries namespaced MCP extension capabilities, keyed by
	// their reverse-DNS-style identifier (e.g. "io.modelcontextprotocol/skills"
	// for the Skills Extension, SEP-2640). The gateway sets it only when
	// the caller's resolved agent profile has at least one attached
	// skill; see internal/dataplane/orchestrator/skills.go.
	Extensions map[string]any `json:"extensions,omitempty"`
}

// RootsCapability indicates support for listing roots.
type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// PromptsCapability indicates support for prompts. The gateway sets
// ListChanged when a reachable backend does: it relays that backend's
// notifications/prompts/list_changed.
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability indicates support for resources. The gateway sets
// each field when a reachable backend does: Subscribe because it serves
// resources/subscribe and relays the updates, ListChanged because it
// relays notifications/resources/list_changed.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// ToolsCapability indicates support for tools. The gateway always
// advertises ListChanged: it is fundamentally a tool router, and a client
// that sees an empty capability object skips tools/list entirely.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// InitializeParams is the payload of an initialize request.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the payload of an initialize response.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// Tool is one tool advertised by a server.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"inputSchema"`
}

// InputSchema is a tool's JSON Schema for its arguments.
//
// Extra holds any schema keyword the gateway does not model explicitly
// (additionalProperties, $defs, title, ...) so a backend's schema survives
// the gateway's decode/re-encode round-trip intact; see the custom
// MarshalJSON/UnmarshalJSON in schema.go.
type InputSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
	Required   []string       `json:"required,omitempty"`
	Extra      map[string]any `json:"-"`
}

// ToolsListParams is the payload of a tools/list request.
type ToolsListParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ToolsListResult is the payload of a tools/list response. Tools is
// always emitted, as [] rather than null when empty, because clients
// treat a missing list differently from an empty one.
type ToolsListResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// ToolsCallParams is the payload of a tools/call request.
//
// Meta is the request's "_meta" member, kept raw so the gateway forwards
// it to the backend byte for byte: it carries the caller's progressToken,
// which a backend must echo unchanged on every notifications/progress.
type ToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments map[string]any  `json:"arguments"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

// MarshalJSON always writes "arguments" as an object -- {} when there are
// none -- never null and never omitted. A tool that takes no arguments is
// still called with {}, and strict MCP servers answer -32602 to a
// tools/call whose params lack the member.
func (p ToolsCallParams) MarshalJSON() ([]byte, error) {
	type wire ToolsCallParams
	w := wire(p)
	if w.Arguments == nil {
		w.Arguments = map[string]any{}
	}
	return json.Marshal(w)
}

// RequestMeta is the part of a request's "_meta" member the gateway
// interprets. ProgressToken is a string or a number, kept raw so it is
// compared and echoed exactly as the caller sent it.
type RequestMeta struct {
	ProgressToken json.RawMessage `json:"progressToken,omitempty"`
}

// CancelledParams is the payload of notifications/cancelled. RequestID
// is the id of the request to abandon, a string or a number, kept raw
// because JSON-RPC treats 1 and "1" as different ids.
type CancelledParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}

// ProgressParams is the payload of notifications/progress.
// ProgressToken is the token from the originating request's
// params._meta.progressToken; Progress increases on every notification
// and Total, when known, is where it ends.
type ProgressParams struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      float64         `json:"progress"`
	Total         *float64        `json:"total,omitempty"`
	Message       string          `json:"message,omitempty"`
}

// ToolsCallResult is the payload of a tools/call response.
type ToolsCallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Content is one item of a tool result: text, an image, or an embedded
// blob.
type Content struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	Data     string         `json:"data,omitempty"`
	MimeType string         `json:"mimeType,omitempty"`
	Blob     string         `json:"blob,omitempty"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

// TextContent returns a Content carrying text.
func TextContent(text string) Content { return Content{Type: "text", Text: text} }

// PingResult is the (empty) payload of a ping response.
type PingResult struct{}

// PaginatedParams is the payload of a paginated */list request.
type PaginatedParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ---------------------------------------------------------------------------
// prompts (MCP 2025-06-18)
// ---------------------------------------------------------------------------

// Prompt is one prompt template advertised by a server.
type Prompt struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
	Meta        json.RawMessage  `json:"_meta,omitempty"`
}

// PromptArgument is one argument a prompt template accepts.
type PromptArgument struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptsListResult is the payload of a prompts/list response. Prompts
// is always emitted, as [] rather than null when empty.
type PromptsListResult struct {
	Prompts    []Prompt `json:"prompts"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// PromptsGetParams is the payload of a prompts/get request.
type PromptsGetParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

// PromptsGetResult is the payload of a prompts/get response.
type PromptsGetResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// PromptMessage is one message of a rendered prompt. Content is kept raw:
// it is text, image, audio, a resource link or an embedded resource, and
// the gateway passes it through without looking inside.
type PromptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ---------------------------------------------------------------------------
// resources (MCP 2025-06-18)
// ---------------------------------------------------------------------------

// Resource is one resource advertised by a server.
type Resource struct {
	URI         string          `json:"uri"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Size        *int64          `json:"size,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// ResourceTemplate is one parameterized resource (RFC 6570 URI template)
// advertised by a server.
type ResourceTemplate struct {
	URITemplate string          `json:"uriTemplate"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// ResourcesListResult is the payload of a resources/list response.
// Resources is always emitted, as [] rather than null when empty.
type ResourcesListResult struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// ResourceTemplatesListResult is the payload of a
// resources/templates/list response. ResourceTemplates is always
// emitted, as [] rather than null when empty.
type ResourceTemplatesListResult struct {
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
	NextCursor        string             `json:"nextCursor,omitempty"`
}

// ResourcesReadParams is the payload of a resources/read request.
type ResourcesReadParams struct {
	URI string `json:"uri"`
}

// ResourcesSubscribeParams is the payload of resources/subscribe and
// resources/unsubscribe.
type ResourcesSubscribeParams struct {
	URI string `json:"uri"`
}

// ResourceUpdatedParams is the payload of
// notifications/resources/updated.
type ResourceUpdatedParams struct {
	URI string `json:"uri"`
}

// ResourcesReadResult is the payload of a resources/read response.
//
// ResultType, TTLMs and CacheScope are the MCP caching utility's fields
// (protocol revision 2026-07-28+; resources/read is one of its listed
// "Cacheable Results"). The gateway sets all three only when it served
// the read itself, for a "skill://" URI under the MCP Skills Extension
// (SEP-2640) -- see internal/dataplane/orchestrator/skills.go. A
// connector-routed ("gw://") read is passed through verbatim and a
// backend on an older protocol revision omits them, so they stay zero
// and, via omitempty, absent from the wire response exactly as before
// this field was added.
type ResourcesReadResult struct {
	Contents   []ResourceContents `json:"contents"`
	Meta       json.RawMessage    `json:"_meta,omitempty"`
	ResultType string             `json:"resultType,omitempty"`
	TTLMs      int                `json:"ttlMs,omitempty"`
	CacheScope string             `json:"cacheScope,omitempty"`
}

// ---------------------------------------------------------------------------
// MCP Skills Extension (SEP-2640)
// https://modelcontextprotocol.io/extensions/skills/overview
// ---------------------------------------------------------------------------

const (
	// ExtensionSkills is the capability-negotiation key for the MCP
	// Skills Extension: capabilities.extensions["io.modelcontextprotocol/skills"].
	ExtensionSkills = "io.modelcontextprotocol/skills"

	// ResultTypeComplete is the only "resultType" the gateway ever
	// produces. The extension's alternative, "input_required", belongs
	// to the base protocol's multi-round-trip request pattern, which the
	// gateway does not implement for skills/list, skills/get or
	// resources/read.
	ResultTypeComplete = "complete"

	// CacheScopePublic and CacheScopePrivate are the two values the MCP
	// caching utility (protocol revision 2026-07-28+) defines for a
	// cacheable result's "cacheScope" field: "public" for a response with
	// no caller-specific data, "private" for one scoped to the caller's
	// own authorization context. The gateway's skills responses are
	// always "private": they depend on the caller's agent profile.
	CacheScopePublic  = "public"
	CacheScopePrivate = "private"
)

// SkillsListParams is the payload of a skills/list request.
type SkillsListParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// SkillManifestResource is one file's entry in a SkillEntry's file
// manifest: its resource URI, content digest and byte size.
type SkillManifestResource struct {
	URI string `json:"uri"`
	// Digest is "sha256:<lowercase hex>", computed from the file's raw
	// bytes as stored (pkg/store.SkillFile.SHA256).
	Digest string `json:"digest"`
	Size   int    `json:"size"`
}

// SkillEntry describes one skill, as returned by both skills/list and
// skills/get: its SKILL.md resource URI, complete frontmatter, and full
// file manifest (SKILL.md included) computed from the bytes actually
// served.
type SkillEntry struct {
	URI         string                  `json:"uri"`
	Frontmatter map[string]any          `json:"frontmatter"`
	Resources   []SkillManifestResource `json:"resources"`
}

// SkillsListResult is the payload of a skills/list response. Skills is
// always emitted, as [] rather than null when empty.
type SkillsListResult struct {
	ResultType string       `json:"resultType"`
	Skills     []SkillEntry `json:"skills"`
	NextCursor string       `json:"nextCursor,omitempty"`
	TTLMs      int          `json:"ttlMs"`
	CacheScope string       `json:"cacheScope"`
}

// SkillsGetParams is the payload of a skills/get request.
type SkillsGetParams struct {
	URI string `json:"uri"`
}

// SkillsGetResult is the payload of a skills/get response.
type SkillsGetResult struct {
	ResultType string     `json:"resultType"`
	Skill      SkillEntry `json:"skill"`
	TTLMs      int        `json:"ttlMs"`
	CacheScope string     `json:"cacheScope"`
}

// ResourceContents is one item of a resources/read result: text or a
// base64 blob. Both are pointers so an empty text survives the round
// trip as "" rather than vanishing under omitempty.
type ResourceContents struct {
	URI      string          `json:"uri"`
	MimeType string          `json:"mimeType,omitempty"`
	Text     *string         `json:"text,omitempty"`
	Blob     *string         `json:"blob,omitempty"`
	Meta     json.RawMessage `json:"_meta,omitempty"`
}
