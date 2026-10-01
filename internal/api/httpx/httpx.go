// Package httpx provides the small set of HTTP helpers every control-plane
// handler (internal/api/handlers) shares: JSON encode/decode, the gateway's
// one error envelope shape, and list-endpoint pagination.
package httpx

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error type strings used in the response envelope
// {"error":{"type":"...","message":"..."}}. These are the only values the
// control-plane API emits; see WriteError's doc comment for the status
// code each pairs with.
const (
	TypeValidation     = "validation_error"
	TypeNotFound       = "not_found"
	TypeConflict       = "conflict"
	TypePermission     = "permission_error"
	TypeInternal       = "internal"
	TypeNotImplemented = "not_implemented"
	TypeUnavailable    = "unavailable"
	TypeTooLarge       = "too_large"
	TypeRateLimited    = "rate_limited"
)

// maxBodyBytes bounds every request body Decode reads: 1 MiB.
const maxBodyBytes = 1 << 20

// DefaultLimit and MaxLimit bound list-endpoint pagination (see
// ParsePagination).
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// WriteJSON writes v as a JSON body with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the gateway's one error envelope shape:
// {"error":{"type":errType,"message":message}}.
func WriteError(w http.ResponseWriter, status int, errType, message string) {
	WriteJSON(w, status, errorBody{Error: errorDetail{Type: errType, Message: message}})
}

// ValidationError writes a 400 validation_error. A message carrying the
// SSRF guard's "egress_blocked:" marker (see internal/netguard) is reported
// with that error type instead, so a client can tell a blocked destination
// from a malformed field.
func ValidationError(w http.ResponseWriter, message string) {
	if strings.Contains(message, "egress_blocked:") {
		WriteError(w, http.StatusBadRequest, "egress_blocked", message)
		return
	}
	WriteError(w, http.StatusBadRequest, TypeValidation, message)
}

// NotFound writes a 404 not_found.
func NotFound(w http.ResponseWriter, message string) {
	WriteError(w, http.StatusNotFound, TypeNotFound, message)
}

// Conflict writes a 409 conflict.
func Conflict(w http.ResponseWriter, message string) {
	WriteError(w, http.StatusConflict, TypeConflict, message)
}

// Internal writes a 500 internal. message is what the caller sees -- it
// should never include a raw driver error; log the real error server-side
// separately.
func Internal(w http.ResponseWriter, message string) {
	WriteError(w, http.StatusInternalServerError, TypeInternal, message)
}

// NotImplemented writes a 501 not_implemented.
func NotImplemented(w http.ResponseWriter, message string) {
	WriteError(w, http.StatusNotImplemented, TypeNotImplemented, message)
}

// Unavailable writes a 503 unavailable. Used when a route is mounted (so
// the OpenAPI surface stays stable) but the collaborator it needs is not
// wired up on this instance -- e.g. the MCP data plane is disabled.
func Unavailable(w http.ResponseWriter, message string) {
	WriteError(w, http.StatusServiceUnavailable, TypeUnavailable, message)
}

// TooLarge writes a 413 too_large. Used by a handler with its own,
// larger-than-1-MiB body limit (see Decode's doc comment), once that
// limit is exceeded.
func TooLarge(w http.ResponseWriter, message string) {
	WriteError(w, http.StatusRequestEntityTooLarge, TypeTooLarge, message)
}

// RateLimited writes a 429 rate_limited with a Retry-After header
// (seconds, rounded up, minimum 1). Mirrors
// internal/auth.RateLimitMiddleware's response shape, for a handler-level
// limiter (e.g. a per-API-key cap) rather than the control plane's
// per-IP one.
func RateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	WriteError(w, http.StatusTooManyRequests, TypeRateLimited, "too many requests")
}

// Decode reads r's body into dst as JSON, rejecting bodies over 1 MiB,
// unknown fields, and trailing garbage after the single JSON value. On any
// decode failure it writes a 400 validation_error itself and returns
// false; callers should return immediately when it does.
func Decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			ValidationError(w, "request body exceeds the 1 MiB limit")
			return false
		}
		ValidationError(w, "invalid request body: "+err.Error())
		return false
	}
	if dec.More() {
		ValidationError(w, "request body must contain a single JSON object")
		return false
	}
	return true
}

// Pagination is a parsed ?limit=&offset= pair, already clamped to
// [1, MaxLimit] / [0, +inf).
type Pagination struct {
	Limit  int
	Offset int
}

// ParsePagination reads limit/offset query params, applying
// DefaultLimit/MaxLimit bounds. Invalid or out-of-range values fall back
// to the default/zero rather than erroring -- pagination is a convenience,
// not a contract worth rejecting requests over.
func ParsePagination(r *http.Request) Pagination {
	p := Pagination{Limit: DefaultLimit, Offset: 0}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			p.Limit = n
		}
	}
	if p.Limit > MaxLimit {
		p.Limit = MaxLimit
	}

	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			p.Offset = n
		}
	}

	return p
}

// Page is the {"items":[...],"total":n} shape returned by list endpoints
// once pagination has been applied.
type Page struct {
	Items any `json:"items"`
	Total int `json:"total"`
}
