package dptest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// Backend is an httptest MCP server: it answers initialize, tools/list
// and tools/call -- and, when configured with them, the prompts and
// resources families, resources/subscribe included -- issues an
// mcp-session-id on initialize and requires it on every request
// afterwards, records the headers it was sent, serves a
// server-to-client stream on GET that tests push messages down (see
// stream.go), and sends server-to-client requests, on that stream or
// mid-tools/call, recording the client's answers (see requests.go).
//
// It is the stand-in for a real backend in both the plane's unit tests
// and the end-to-end test, so "what the gateway actually puts on the
// wire" is asserted against one implementation rather than two.
type Backend struct {
	*httptest.Server

	mu sync.Mutex
	// SessionID is the handle issued at initialize.
	sessionID string
	// LastHeaders is the header set of the most recent request.
	lastHeaders http.Header
	// calls counts requests by JSON-RPC method.
	calls map[string]int
	// toolArgs records the arguments of the last tools/call, by tool.
	toolArgs map[string]map[string]any

	tools    []mcp.Tool
	handlers map[string]ToolFunc

	prompts   []mcp.Prompt
	resources []mcp.Resource
	templates []mcp.ResourceTemplate
	pageSize  int
	// promptArgs records the arguments of the last prompts/get, by
	// prompt; readURIs every URI resources/read was asked for.
	promptArgs map[string]map[string]string
	readURIs   []string

	// RequireSession rejects a non-initialize request that arrives
	// without the session header.
	requireSession bool

	// running maps the JSON-RPC id of each SlowTool call in progress to
	// the channel a notifications/cancelled for it closes.
	running map[string]chan struct{}
	// cancelled records every notifications/cancelled received.
	cancelled []Cancellation
	// slowIDs records the JSON-RPC id of every SlowTool call.
	slowIDs []string

	// The server-to-client stream (GET): see stream.go.
	noStream      bool
	plainCaps     bool
	streams       map[*sseStream]struct{}
	streamGETs    int
	streamHeaders http.Header
	lastEventIDs  []string
	history       []streamEvent
	nextEvent     int
	nextRequestID int
	replies       []mcp.Response
	subscribed    map[string]bool

	// Server-to-client requests: see requests.go.
	initCaps []json.RawMessage
	asked    []mcp.Request
	waiting  map[string]chan mcp.Response
}

// SlowTool is the name of a tool every Backend serves (unless Handlers
// overrides it) whether or not it is advertised: it answers over SSE,
// streaming notifications/progress before its result, and honours
// notifications/cancelled. SlowToolDef advertises it.
//
// Arguments, all optional:
//
//	steps        number of progress notifications (default 3)
//	interval_ms  pause after each one (default 5)
//	hold         after the steps, wait for a cancellation (or the
//	             request to end) instead of answering
//	foreign      first emit one progress notification under a token
//	             that is not the caller's
//
// Progress is only emitted when the call carried
// params._meta.progressToken, and echoes that token unchanged.
const SlowTool = "slow"

// SlowToolDef is SlowTool's tools/list entry.
func SlowToolDef() mcp.Tool {
	return mcp.Tool{
		Name:        SlowTool,
		Description: "Streams progress, then answers; honours cancellation",
		InputSchema: mcp.InputSchema{Type: "object", Properties: map[string]any{
			"steps":       map[string]any{"type": "number"},
			"interval_ms": map[string]any{"type": "number"},
			"hold":        map[string]any{"type": "boolean"},
			"foreign":     map[string]any{"type": "boolean"},
		}},
	}
}

// Cancellation is one notifications/cancelled a Backend received.
type Cancellation struct {
	// RequestID is the cancelled request's id, rendered by mcp.FormatID.
	RequestID string
	Reason    string
}

// ToolFunc produces a tool's result from its arguments.
type ToolFunc func(args map[string]any) (mcp.ToolsCallResult, error)

// BackendOptions configures a Backend.
type BackendOptions struct {
	// Tools are advertised by tools/list, with their schemas.
	Tools []mcp.Tool
	// Handlers maps a tool name to its implementation. A tool with no
	// handler echoes its arguments.
	Handlers map[string]ToolFunc
	// RequireSession makes the backend reject an un-sessioned request,
	// which is what a session-tracking MCP server does.
	RequireSession bool

	// Prompts are advertised by prompts/list. A non-empty list makes
	// the backend declare the prompts capability; prompts/get renders
	// one user message naming the prompt and its arguments.
	Prompts []mcp.Prompt
	// Resources are advertised by resources/list and ResourceTemplates
	// by resources/templates/list. Either makes the backend declare the
	// resources capability; resources/read answers any listed URI, or
	// any URI starting with a template's text before its first "{",
	// with one text content echoing the URI.
	Resources         []mcp.Resource
	ResourceTemplates []mcp.ResourceTemplate
	// PageSize, when positive, splits every prompts/resources list into
	// pages of that many items chained by nextCursor.
	PageSize int
	// NoStream makes the backend answer GET with 405, as a server with
	// no server-to-client stream does. By default GET opens one.
	NoStream bool
	// PlainCatalogCapabilities declares the prompts and resources
	// capabilities bare -- no listChanged, no subscribe -- instead of
	// with every flag set.
	PlainCatalogCapabilities bool
}

// NewBackend starts a fake MCP backend. The caller closes it.
func NewBackend(opts BackendOptions) *Backend {
	b := &Backend{
		sessionID:      "backend-session-" + fmt.Sprint(len(opts.Tools)) + "-fixed",
		calls:          make(map[string]int),
		toolArgs:       make(map[string]map[string]any),
		tools:          opts.Tools,
		handlers:       opts.Handlers,
		requireSession: opts.RequireSession,
		prompts:        opts.Prompts,
		resources:      opts.Resources,
		templates:      opts.ResourceTemplates,
		pageSize:       opts.PageSize,
		promptArgs:     make(map[string]map[string]string),
		running:        make(map[string]chan struct{}),
		noStream:       opts.NoStream,
		plainCaps:      opts.PlainCatalogCapabilities,
		streams:        make(map[*sseStream]struct{}),
		subscribed:     make(map[string]bool),
		waiting:        make(map[string]chan mcp.Response),
	}
	b.Server = httptest.NewServer(http.HandlerFunc(b.serve))
	return b
}

// SessionID returns the handle the backend issues at initialize.
func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// LastHeaders returns a copy of the most recent request's headers.
func (b *Backend) LastHeaders() http.Header {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastHeaders.Clone()
}

// Calls returns how many requests carried the given JSON-RPC method.
func (b *Backend) Calls(method string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[method]
}

// ToolArgs returns the arguments of the last call to a tool.
func (b *Backend) ToolArgs(tool string) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.toolArgs[tool]
}

// PromptArgs returns the arguments of the last prompts/get of a prompt,
// or nil if it was never fetched.
func (b *Backend) PromptArgs(prompt string) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.promptArgs[prompt]
}

// ReadURIs returns every URI resources/read was called with, in order.
func (b *Backend) ReadURIs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.readURIs...)
}

// Cancellations returns every notifications/cancelled received so far.
func (b *Backend) Cancellations() []Cancellation {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Cancellation(nil), b.cancelled...)
}

// SlowCallIDs returns the JSON-RPC id (rendered by mcp.FormatID) of
// every SlowTool call received, in order.
func (b *Backend) SlowCallIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.slowIDs...)
}

// Running reports how many SlowTool calls are in progress.
func (b *Backend) Running() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.running)
}

func (b *Backend) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		b.serveStream(w, r)
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req mcp.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// A message with no method is the client's response to a request
	// this backend sent on its stream.
	if req.Method == "" {
		var resp mcp.Response
		if err := json.Unmarshal(raw, &resp); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		b.recordReply(resp)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	b.mu.Lock()
	b.lastHeaders = r.Header.Clone()
	b.calls[req.Method]++
	sessionID := b.sessionID
	b.mu.Unlock()

	if b.requireSession && req.Method != mcp.MethodInitialize {
		if got := r.Header.Get(mcp.HeaderSessionID); got != sessionID {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
	}

	switch req.Method {
	case mcp.MethodInitialize:
		var init struct {
			Capabilities json.RawMessage `json:"capabilities"`
		}
		_ = json.Unmarshal(req.Params, &init)
		b.mu.Lock()
		b.initCaps = append(b.initCaps, init.Capabilities)
		b.mu.Unlock()

		caps := mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{ListChanged: true}}
		if len(b.prompts) > 0 {
			caps.Prompts = &mcp.PromptsCapability{ListChanged: !b.plainCaps}
		}
		if len(b.resources) > 0 || len(b.templates) > 0 {
			caps.Resources = &mcp.ResourcesCapability{Subscribe: !b.plainCaps, ListChanged: !b.plainCaps}
		}
		w.Header().Set(mcp.HeaderSessionID, sessionID)
		b.reply(w, req.ID, mcp.InitializeResult{
			ProtocolVersion: r.Header.Get(mcp.HeaderProtocolVersion),
			Capabilities:    caps,
			ServerInfo:      mcp.Implementation{Name: "dptest-backend", Version: "1.0.0"},
		})

	case mcp.MethodInitialized:
		w.WriteHeader(http.StatusAccepted)

	case mcp.MethodToolsList:
		b.reply(w, req.ID, mcp.ToolsListResult{Tools: b.tools})

	case mcp.MethodToolsCall:
		var params mcp.ToolsCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			b.replyError(w, req.ID, mcp.NewInvalidParamsError(err.Error()))
			return
		}
		b.mu.Lock()
		b.toolArgs[params.Name] = params.Arguments
		handler := b.handlers[params.Name]
		b.mu.Unlock()

		if handler == nil && params.Name == SlowTool {
			b.serveSlow(w, r, req.ID, params)
			return
		}
		if handler == nil && params.Name == AskTool {
			b.serveAsk(w, r, req.ID, params)
			return
		}
		if handler == nil {
			args, _ := json.Marshal(params.Arguments)
			b.reply(w, req.ID, mcp.ToolsCallResult{
				Content: []mcp.Content{mcp.TextContent(params.Name + ":" + string(args))},
			})
			return
		}
		out, err := handler(params.Arguments)
		if err != nil {
			b.replyError(w, req.ID, mcp.NewInternalError(err.Error()))
			return
		}
		b.reply(w, req.ID, out)

	case mcp.MethodPing:
		b.reply(w, req.ID, mcp.PingResult{})

	case mcp.MethodPromptsList, mcp.MethodResourcesList, mcp.MethodResourcesTemplatesList:
		b.serveList(w, req)

	case mcp.MethodPromptsGet:
		var params mcp.PromptsGetParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			b.replyError(w, req.ID, mcp.NewInvalidParamsError(err.Error()))
			return
		}
		if !b.hasPrompt(params.Name) {
			b.replyError(w, req.ID, mcp.NewInvalidParamsError("prompt not found: "+params.Name))
			return
		}
		b.mu.Lock()
		b.promptArgs[params.Name] = params.Arguments
		if b.promptArgs[params.Name] == nil {
			b.promptArgs[params.Name] = map[string]string{}
		}
		b.mu.Unlock()

		args, _ := json.Marshal(params.Arguments)
		content, _ := json.Marshal(mcp.TextContent(params.Name + ":" + string(args)))
		b.reply(w, req.ID, mcp.PromptsGetResult{
			Description: "rendered " + params.Name,
			Messages:    []mcp.PromptMessage{{Role: "user", Content: content}},
		})

	case mcp.MethodResourcesRead:
		var params mcp.ResourcesReadParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			b.replyError(w, req.ID, mcp.NewInvalidParamsError(err.Error()))
			return
		}
		b.mu.Lock()
		b.readURIs = append(b.readURIs, params.URI)
		b.mu.Unlock()
		if !b.hasResource(params.URI) {
			// -32002 is the spec's "resource not found".
			b.replyError(w, req.ID, mcp.NewError(-32002, "resource not found", map[string]any{"uri": params.URI}))
			return
		}
		text := "contents of " + params.URI
		b.reply(w, req.ID, mcp.ResourcesReadResult{
			Contents: []mcp.ResourceContents{{URI: params.URI, MimeType: "text/plain", Text: &text}},
		})

	case mcp.MethodResourcesSubscribe, mcp.MethodResourcesUnsubscribe:
		if len(b.resources) == 0 && len(b.templates) == 0 {
			b.replyError(w, req.ID, mcp.NewMethodNotFoundError(req.Method))
			return
		}
		var params mcp.ResourcesSubscribeParams
		if err := json.Unmarshal(req.Params, &params); err != nil || params.URI == "" {
			b.replyError(w, req.ID, mcp.NewInvalidParamsError("uri is required"))
			return
		}
		b.mu.Lock()
		if req.Method == mcp.MethodResourcesSubscribe {
			b.subscribed[params.URI] = true
		} else {
			delete(b.subscribed, params.URI)
		}
		b.mu.Unlock()
		b.reply(w, req.ID, struct{}{})

	case mcp.NotificationCancelled:
		var params mcp.CancelledParams
		_ = json.Unmarshal(req.Params, &params)
		var id any
		_ = json.Unmarshal(params.RequestID, &id)
		key := mcp.FormatID(id)

		b.mu.Lock()
		if stop, running := b.running[key]; running {
			close(stop)
			delete(b.running, key)
		}
		b.cancelled = append(b.cancelled, Cancellation{RequestID: key, Reason: params.Reason})
		b.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)

	default:
		b.replyError(w, req.ID, mcp.NewMethodNotFoundError(req.Method))
	}
}

// serveList answers one of the three prompts/resources list methods,
// one page at a time when PageSize is set. The cursor is the index of
// the page's first item.
func (b *Backend) serveList(w http.ResponseWriter, req mcp.Request) {
	var params mcp.PaginatedParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			b.replyError(w, req.ID, mcp.NewInvalidParamsError(err.Error()))
			return
		}
	}

	var total int
	switch req.Method {
	case mcp.MethodPromptsList:
		total = len(b.prompts)
	case mcp.MethodResourcesList:
		total = len(b.resources)
	default:
		total = len(b.templates)
	}
	// A backend that does not declare the capability does not serve
	// the family at all, as a real one would not.
	if (req.Method == mcp.MethodPromptsList && len(b.prompts) == 0) ||
		(req.Method != mcp.MethodPromptsList && len(b.resources) == 0 && len(b.templates) == 0) {
		b.replyError(w, req.ID, mcp.NewMethodNotFoundError(req.Method))
		return
	}

	start := 0
	if params.Cursor != "" {
		if _, err := fmt.Sscan(params.Cursor, &start); err != nil || start < 0 || start > total {
			b.replyError(w, req.ID, mcp.NewInvalidParamsError("bad cursor"))
			return
		}
	}
	end := total
	if b.pageSize > 0 && start+b.pageSize < total {
		end = start + b.pageSize
	}
	next := ""
	if end < total {
		next = fmt.Sprint(end)
	}

	switch req.Method {
	case mcp.MethodPromptsList:
		b.reply(w, req.ID, mcp.PromptsListResult{Prompts: b.prompts[start:end], NextCursor: next})
	case mcp.MethodResourcesList:
		b.reply(w, req.ID, mcp.ResourcesListResult{Resources: b.resources[start:end], NextCursor: next})
	default:
		b.reply(w, req.ID, mcp.ResourceTemplatesListResult{ResourceTemplates: b.templates[start:end], NextCursor: next})
	}
}

func (b *Backend) hasPrompt(name string) bool {
	for _, p := range b.prompts {
		if p.Name == name {
			return true
		}
	}
	return false
}

func (b *Backend) hasResource(uri string) bool {
	for _, r := range b.resources {
		if r.URI == uri {
			return true
		}
	}
	for _, t := range b.templates {
		prefix, _, _ := strings.Cut(t.URITemplate, "{")
		if strings.HasPrefix(uri, prefix) && len(uri) > len(prefix) {
			return true
		}
	}
	return false
}

// serveSlow runs SlowTool: a Streamable HTTP reply that streams
// progress notifications ahead of its result.
func (b *Backend) serveSlow(w http.ResponseWriter, r *http.Request, id any, params mcp.ToolsCallParams) {
	steps, interval := 3, 5*time.Millisecond
	if v, ok := params.Arguments["steps"].(float64); ok {
		steps = int(v)
	}
	if v, ok := params.Arguments["interval_ms"].(float64); ok {
		interval = time.Duration(v) * time.Millisecond
	}
	hold, _ := params.Arguments["hold"].(bool)
	foreign, _ := params.Arguments["foreign"].(bool)

	var meta mcp.RequestMeta
	if len(params.Meta) > 0 {
		_ = json.Unmarshal(params.Meta, &meta)
	}

	key := mcp.FormatID(id)
	stop := make(chan struct{})
	b.mu.Lock()
	b.running[key] = stop
	b.slowIDs = append(b.slowIDs, key)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.running[key] == stop {
			delete(b.running, key)
		}
		b.mu.Unlock()
	}()

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	send := func(msg any) {
		raw, _ := json.Marshal(msg)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	progress := func(token json.RawMessage, n int) {
		total := float64(steps)
		raw, _ := json.Marshal(mcp.ProgressParams{
			ProgressToken: token, Progress: float64(n), Total: &total,
			Message: fmt.Sprintf("step %d of %d", n, steps),
		})
		send(mcp.Request{JSONRPC: mcp.Version, Method: mcp.NotificationProgress, Params: raw})
	}

	if foreign {
		progress(json.RawMessage(`"not-the-callers-token"`), 0)
	}
	for i := 1; i <= steps; i++ {
		if len(meta.ProgressToken) > 0 {
			progress(meta.ProgressToken, i)
		}
		select {
		case <-stop:
			return // cancelled: the spec says send no response
		case <-r.Context().Done():
			return
		case <-time.After(interval):
		}
	}

	if hold {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
		return
	}

	send(mcp.NewSuccessResponse(id, mcp.ToolsCallResult{
		Content: []mcp.Content{mcp.TextContent(fmt.Sprintf("slow: done after %d steps", steps))},
	}))
}

func (b *Backend) reply(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mcp.NewSuccessResponse(id, result))
}

func (b *Backend) replyError(w http.ResponseWriter, id any, err *mcp.Error) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mcp.NewErrorResponse(id, err))
}

// ObjectSchema builds a simple object input schema from property names.
func ObjectSchema(required []string, properties ...string) mcp.InputSchema {
	props := make(map[string]any, len(properties))
	for _, p := range properties {
		props[p] = map[string]any{"type": "string"}
	}
	return mcp.InputSchema{Type: "object", Properties: props, Required: required}
}

// SamplePrompts is a two-prompt set -- one bare, one with a required
// argument -- for tests that exercise the prompts family.
func SamplePrompts() []mcp.Prompt {
	return []mcp.Prompt{
		{Name: "greet", Title: "Greeting", Description: "Greet someone",
			Arguments: []mcp.PromptArgument{{Name: "who", Description: "Who to greet", Required: true}}},
		{Name: "simple", Description: "A prompt with no arguments"},
	}
}

// SampleResources is a two-resource set whose URIs carry schemes of
// their own, as real backends' do, so namespacing is tested against the
// shape it has to survive.
func SampleResources() []mcp.Resource {
	size := int64(42)
	return []mcp.Resource{
		{URI: "test://static/resource/1", Name: "Resource 1", MimeType: "text/plain", Size: &size},
		{URI: "file:///var/data/report.md", Name: "report.md", MimeType: "text/markdown",
			Annotations: json.RawMessage(`{"audience":["user"],"priority":0.5}`)},
	}
}

// SampleResourceTemplates is a one-template set whose expansions
// resources/read answers.
func SampleResourceTemplates() []mcp.ResourceTemplate {
	return []mcp.ResourceTemplate{
		{URITemplate: "test://dynamic/resource/{id}", Name: "Dynamic resource", MimeType: "text/plain"},
	}
}
