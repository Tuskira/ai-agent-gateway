// Package reqctx carries the mutable, request-scoped record that every
// stage of the MCP pipeline contributes to and the access-log middleware
// reads back at the end.
//
// It exists to replace the process-global "correlation id -> connector
// id" map the gateway's predecessor used. That map was a correctness
// hazard (an entry leaked whenever a handler returned early, and two
// in-flight requests sharing a correlation id overwrote each other) and
// it forced every stage that wanted to record something to know the
// logging layer's key. A pointer in the request context has neither
// problem: it dies with the request, it is unambiguously per-request, and
// the stages only need this leaf package, so transport can depend on the
// orchestrator without the orchestrator depending back.
package reqctx

import (
	"context"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/trace"
)

type ctxKey struct{}

// Info is one request's log record under construction. The fields set at
// creation are read-only; everything a later stage discovers is written
// through a setter, because the initialize fan-out touches several
// connectors concurrently.
type Info struct {
	// Set once, at creation, before the value is shared.
	RequestID     string
	CorrelationID string
	Trace         trace.Context
	ClientIP      string
	UserAgent     string
	StartedAt     time.Time

	mu          sync.Mutex
	principal   *auth.Principal
	method      string
	jsonRPCID   string
	connectorID string
	toolName    string
	skillName   string
	profile     string
	sessionID   string
	errorCode   int
	bytesIn     int64
	bytesOut    int64
}

// New returns an Info stamped with the immutable fields.
func New(requestID, correlationID string, tc trace.Context, clientIP, userAgent string, startedAt time.Time) *Info {
	return &Info{
		RequestID:     requestID,
		CorrelationID: correlationID,
		Trace:         tc,
		ClientIP:      clientIP,
		UserAgent:     userAgent,
		StartedAt:     startedAt,
	}
}

// With returns a context carrying info.
func With(ctx context.Context, info *Info) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

// From returns the Info carried by ctx, if any. Every accessor on *Info
// tolerates a nil receiver, so a caller outside the request path (a
// background refresher, a test) can use the result without a nil check.
func From(ctx context.Context) *Info {
	info, _ := ctx.Value(ctxKey{}).(*Info)
	return info
}

// SetPrincipal records the authenticated caller.
//
// The principal is recorded here as well as on the context because the
// access-log middleware sits OUTSIDE authentication (so a 401 is still
// logged), and a context value added by an inner middleware is invisible
// to an outer one: authentication derives a new context and hands it
// downstream, it cannot write into its caller's.
func (i *Info) SetPrincipal(p *auth.Principal) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.principal = p
	i.mu.Unlock()
}

// Principal returns the authenticated caller, or nil for a request that
// never got past authentication.
func (i *Info) Principal() *auth.Principal {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.principal
}

// SetMethod records the JSON-RPC method being served.
func (i *Info) SetMethod(method string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.method = method
	i.mu.Unlock()
}

// SetJSONRPCID records the request's JSON-RPC id, rendered as a string.
func (i *Info) SetJSONRPCID(id string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.jsonRPCID = id
	i.mu.Unlock()
}

// SetConnector records which connector served the request. A tools/call
// sets it even when the call fails, so a failure is still attributed.
func (i *Info) SetConnector(connectorID string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.connectorID = connectorID
	i.mu.Unlock()
}

// SetTool records the fully-qualified tool name a tools/call targeted.
func (i *Info) SetTool(toolName string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.toolName = toolName
	i.mu.Unlock()
}

// SetSkill records the name of the skill or command a request loaded --
// a gateway__skill tools/call, or a native command's prompts/get (see
// internal/dataplane/orchestrator's skills.go). Empty for every other
// request, including a normal connector-routed tools/call.
func (i *Info) SetSkill(name string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.skillName = name
	i.mu.Unlock()
}

// SetProfile records the agent profile enforced for the request: the one
// its API key is bound to, else the one named by the caller's header.
func (i *Info) SetProfile(name string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.profile = name
	i.mu.Unlock()
}

// SetSession records the gateway session id in play.
func (i *Info) SetSession(sessionID string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.sessionID = sessionID
	i.mu.Unlock()
}

// SetErrorCode records the JSON-RPC error code the request ended with.
func (i *Info) SetErrorCode(code int) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.errorCode = code
	i.mu.Unlock()
}

// SetBytes records the inbound request and outbound response sizes.
func (i *Info) SetBytes(in, out int64) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.bytesIn, i.bytesOut = in, out
	i.mu.Unlock()
}

// Snapshot is a consistent read of everything the stages recorded.
type Snapshot struct {
	Method      string
	JSONRPCID   string
	ConnectorID string
	ToolName    string
	SkillName   string
	Profile     string
	SessionID   string
	ErrorCode   int
	BytesIn     int64
	BytesOut    int64
}

// Snapshot returns the recorded fields under one lock acquisition.
func (i *Info) Snapshot() Snapshot {
	if i == nil {
		return Snapshot{}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return Snapshot{
		Method:      i.method,
		JSONRPCID:   i.jsonRPCID,
		ConnectorID: i.connectorID,
		ToolName:    i.toolName,
		SkillName:   i.skillName,
		Profile:     i.profile,
		SessionID:   i.sessionID,
		ErrorCode:   i.errorCode,
		BytesIn:     i.bytesIn,
		BytesOut:    i.bytesOut,
	}
}
