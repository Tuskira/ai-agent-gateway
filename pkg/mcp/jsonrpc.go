package mcp

import (
	"encoding/json"
	"strconv"
)

// Version is the only JSON-RPC version MCP speaks.
const Version = "2.0"

// Request is a JSON-RPC 2.0 request or notification. A notification is a
// Request with no ID (see IsNotification); the gateway answers those with
// HTTP 204 and no body.
//
// Params is kept as raw JSON so a handler can decode it into the shape
// its method expects, and so the gateway can forward a payload it does
// not itself interpret without a lossy round-trip.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response. Exactly one of Result and Error is
// populated on a well-formed response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object. It implements the error interface
// so a handler can return one directly.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error returns the error's message, satisfying the error interface.
func (e *Error) Error() string { return e.Message }

// Standard JSON-RPC 2.0 error codes.
const (
	ErrorCodeParseError     = -32700
	ErrorCodeInvalidRequest = -32600
	ErrorCodeMethodNotFound = -32601
	ErrorCodeInvalidParams  = -32602
	ErrorCodeInternalError  = -32603

	// ErrorCodeRequestCancelled answers a request the caller itself
	// cancelled with notifications/cancelled. MCP 2025-06-18
	// (basic/utilities/cancellation) says the receiver SHOULD NOT
	// respond to a cancelled request, but over Streamable HTTP the
	// request's POST is still open and has to end with something. MCP
	// defines no code for it, so the gateway borrows LSP's
	// RequestCancelled (-32800), the established JSON-RPC convention,
	// and a client that already stopped waiting simply discards it.
	ErrorCodeRequestCancelled = -32800
)

// Gateway-specific error codes, drawn from JSON-RPC's -32000..-32099
// implementation-defined range.
//
// The three that clients are expected to branch on are fixed by the
// gateway's architecture spec and must not be renumbered: -32000 tells a
// client to re-run initialize, -32001 tells it its credential is bad, and
// -32003 tells it the tool exists but its agent profile does not grant it.
const (
	// ErrorCodeSessionNotFound is returned when a request carries an
	// Mcp-Session-Id the gateway does not know, or one that has expired.
	// Per the MCP spec the client's correct response is to re-initialize.
	ErrorCodeSessionNotFound = -32000

	// ErrorCodeUnauthorized accompanies an HTTP 401: the request carried
	// no usable gateway credential. It is the one error the gateway
	// returns with a non-200 HTTP status.
	ErrorCodeUnauthorized = -32001
	// ErrorCodeForbidden: the caller authenticated but its role does not
	// grant the mcp.access permission (HTTP 403).
	ErrorCodeForbidden = -32006
	// ErrorCodeRateLimited accompanies an HTTP 429: the caller is locked
	// out after too many failed authentication attempts (see the
	// Retry-After header).
	ErrorCodeRateLimited = -32029

	// ErrorCodeToolExecution reports a backend-side failure while running
	// a tool (transport error, backend JSON-RPC error, malformed result).
	ErrorCodeToolExecution = -32002

	// ErrorCodeToolNotAllowed reports that the caller's agent profile does
	// not grant the requested tool -- including the case where the named
	// profile does not exist at all, which grants nothing.
	ErrorCodeToolNotAllowed = -32003

	// ErrorCodeConnectorNotFound reports that the "<connector>__<tool>"
	// prefix names no connector in the caller's tenant.
	ErrorCodeConnectorNotFound = -32004

	// ErrorCodeConnectorUnhealthy reports that the target connector is
	// marked unhealthy and is still inside its recovery cool-down (see
	// internal/dataplane/router's half-open probe).
	ErrorCodeConnectorUnhealthy = -32005
)

// NewError builds an Error, JSON-encoding data into the optional "data"
// member. A data value that cannot be marshaled is dropped rather than
// failing the error itself -- an error response must always be sendable.
func NewError(code int, message string, data any) *Error {
	e := &Error{Code: code, Message: message}
	if data != nil {
		if raw, err := json.Marshal(data); err == nil {
			e.Data = raw
		}
	}
	return e
}

// Convenience constructors for the standard codes.

// NewParseError builds a -32700 error.
func NewParseError(message string) *Error { return NewError(ErrorCodeParseError, message, nil) }

// NewInvalidRequestError builds a -32600 error.
func NewInvalidRequestError(message string) *Error {
	return NewError(ErrorCodeInvalidRequest, message, nil)
}

// NewMethodNotFoundError builds a -32601 error naming the method.
func NewMethodNotFoundError(method string) *Error {
	return NewError(ErrorCodeMethodNotFound, "method not found: "+method, nil)
}

// NewInvalidParamsError builds a -32602 error.
func NewInvalidParamsError(message string) *Error {
	return NewError(ErrorCodeInvalidParams, message, nil)
}

// NewInternalError builds a -32603 error.
func NewInternalError(message string) *Error { return NewError(ErrorCodeInternalError, message, nil) }

// IsNotification reports whether r is a notification: a JSON-RPC request
// with no id, to which no response may be sent.
func (r *Request) IsNotification() bool { return r.ID == nil }

// Validate checks the envelope invariants every JSON-RPC request must
// satisfy, returning the JSON-RPC error to reply with when one is broken.
func (r *Request) Validate() *Error {
	if r.JSONRPC != Version {
		return NewInvalidRequestError(`invalid "jsonrpc" version, must be "2.0"`)
	}
	if r.Method == "" {
		return NewInvalidRequestError(`"method" is required`)
	}
	return nil
}

// NewSuccessResponse builds a success response carrying result. A result
// that cannot be marshaled degrades to an internal error response rather
// than to a response with neither result nor error, which no client can
// interpret.
func NewSuccessResponse(id any, result any) *Response {
	resp := &Response{JSONRPC: Version, ID: id}
	if result == nil {
		resp.Result = json.RawMessage(`null`)
		return resp
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return NewErrorResponse(id, NewInternalError("failed to encode result: "+err.Error()))
	}
	resp.Result = raw
	return resp
}

// NewErrorResponse builds an error response.
func NewErrorResponse(id any, err *Error) *Response {
	return &Response{JSONRPC: Version, ID: id, Error: err}
}

// FormatID renders a JSON-RPC id for logging. JSON-RPC allows a string, a
// number or null, and encoding/json decodes numbers into float64, so a
// whole number is rendered without a spurious ".0" and everything else
// falls back to its JSON encoding.
func FormatID(id any) string {
	switch v := id.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}
