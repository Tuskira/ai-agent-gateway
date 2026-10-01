package llmplane

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// anthropicErrorType maps an HTTP status to the Anthropic error `type` string, so
// gateway-originated errors surface in the same envelope Anthropic clients already
// parse (shapes per docs.claude.com/en/api/errors).
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	default:
		return "api_error" // 500/502/504 and anything else gateway-originated
	}
}

// writeAnthropicError writes a gateway error as the Anthropic error envelope:
//
//	{"type":"error","error":{"type":"<mapped>","message":"..."},"request_id":"..."}
func writeAnthropicError(w http.ResponseWriter, status int, requestID, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(anthropicErrorBody(status, requestID, message), '\n'))
}

// anthropicErrorBody is the Anthropic error envelope; request_id only when set.
func anthropicErrorBody(status int, requestID, message string) []byte {
	return errorEnvelope(anthropicErrorType(status), requestID, message)
}

func errorEnvelope(typ, requestID, message string) []byte {
	body := map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": message},
	}
	if requestID != "" {
		body["request_id"] = requestID
	}
	b, _ := json.Marshal(body)
	return b
}

// writeDialectError writes a gateway-originated error in the envelope the
// CLIENT's dialect parses, so an SDK surfaces the message instead of a
// decode failure:
//
//	anthropic  {"type":"error","error":{"type":"...","message":"..."},"request_id":"..."}
//	openai     {"error":{"message":"...","type":"...","param":null,"code":null}}
//	gemini     {"error":{"code":400,"message":"...","status":"INVALID_ARGUMENT"}}
//	bedrock    {"message":"..."} + X-Amzn-ErrorType
func writeDialectError(w http.ResponseWriter, dialect string, status int, requestID, message string) {
	var body any
	switch dialect {
	case "openai":
		body = map[string]any{"error": map[string]any{
			"message": message, "type": anthropicErrorType(status), "param": nil, "code": nil,
		}}
	case "gemini":
		st := "INTERNAL"
		switch status {
		case http.StatusBadRequest:
			st = "INVALID_ARGUMENT"
		case http.StatusForbidden:
			st = "PERMISSION_DENIED"
		case http.StatusNotFound:
			st = "NOT_FOUND"
		case http.StatusTooManyRequests:
			st = "RESOURCE_EXHAUSTED"
		}
		body = map[string]any{"error": map[string]any{"code": status, "message": message, "status": st}}
	case "bedrock":
		et := "InternalServerException"
		if status/100 == 4 {
			et = "ValidationException"
		}
		w.Header().Set("X-Amzn-ErrorType", et)
		body = map[string]string{"message": message}
	default:
		writeAnthropicError(w, status, requestID, message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteRateLimited answers a caller locked out by the auth-failure limiter
// with a 429 + Retry-After in the error envelope of the dialect the request
// path addresses (see pickProvider's prefixes), so an SDK surfaces the
// message instead of failing to decode it. It is mounted AHEAD of the plane
// (the caller authenticates first), so it sniffs the dialect from the path.
func WriteRateLimited(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	dialect := "anthropic"
	if seg, _ := firstSegment(r.URL.Path); seg == "openai" || seg == "gemini" || seg == "bedrock" {
		dialect = seg
	} else if strings.HasPrefix(r.URL.Path, "/model/") {
		dialect = "bedrock"
	}
	writeDialectError(w, dialect, http.StatusTooManyRequests, "", "too many failed authentication attempts; retry later")
}

// clientError marks a BuildUpstream failure caused by the request itself
// (malformed region, missing credential header, ...): the router answers 400
// with its message instead of a generic 502.
type clientError struct{ msg string }

func (e clientError) Error() string { return e.msg }

// errorText renders an upstream error for capture with any URL query removed:
// a transport failure is a *url.Error embedding the full request URL, and query
// strings can carry credentials (Gemini's ?key=).
func errorText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			u.RawQuery, u.User = "", nil
			return ue.Op + " " + u.String() + ": " + ue.Err.Error()
		}
		return ue.Op + ": " + ue.Err.Error()
	}
	return err.Error()
}
