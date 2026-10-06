package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Dialect is the client side of a translation: the wire format a client
// speaks (the Anthropic Messages API, ...). It must be safe for concurrent
// use; per-stream state lives in the StreamEncoder it returns.
type Dialect interface {
	// Name is the registry name ("anthropic").
	Name() string
	// ParseRequest reads the client's request (its body) into the neutral
	// form. A malformed request is a *RequestError.
	ParseRequest(r *http.Request) (*Request, error)
	// RenderResponse renders a complete answer in the client's format.
	RenderResponse(resp *Response) ([]byte, error)
	// RenderError renders an error in the client's error envelope.
	RenderError(e *Error) []byte
	// NewStreamEncoder returns an encoder that writes Events to w in the
	// client's stream format, one w.Write per event (so the relay can flush
	// each one).
	NewStreamEncoder(w io.Writer) StreamEncoder
}

// StreamEncoder writes neutral stream events in a client's format.
type StreamEncoder interface {
	// Write renders one event. After an error it must not be called again.
	Write(ev Event) error
	// Close finishes the stream (a format with a terminator writes it here).
	Close() error
}

// Provider is the vendor side of a translation: how to call one kind of
// vendor API with a neutral Request. It must be safe for concurrent use; the
// per-call destination is the Target.
type Provider interface {
	// Name is the registry name ("openai_compat").
	Name() string
	// Capabilities lists what the Provider can carry; the engine refuses a
	// request that needs more (see Capabilities).
	Capabilities() Capabilities
	// BuildRequest builds the vendor request for req, bound to ctx. It sets
	// a FRESH header set: nothing from the client's request may reach the
	// vendor except what the neutral form carries and t.Auth. A target-
	// dependent limit is reported as *ErrUnsupported, a request the vendor
	// cannot take as *RequestError.
	BuildRequest(ctx context.Context, req *Request, t Target) (*http.Request, error)
	// ParseResponse reads a successful (2xx) non-stream vendor response.
	// The engine has already capped the body at MaxBodyBytes.
	ParseResponse(resp *http.Response) (*Response, error)
	// ParseError maps a vendor error response (status >= 400, body capped at
	// MaxBodyBytes) onto a neutral Error.
	ParseError(status int, body []byte) *Error
	// NewStreamDecoder returns a decoder over a successful streamed vendor
	// response body.
	NewStreamDecoder(body io.Reader) StreamDecoder
}

// StreamDecoder turns a vendor stream into neutral events, in the order
// documented on EventType.
//
// Next returns the next event. It returns io.EOF after message_stop (or
// after an error event: a vendor error inside the stream is an Event of type
// EventError followed by io.EOF), io.ErrUnexpectedEOF when the vendor stream
// ended before the model finished, and any other error when reading failed.
// Next is called from one goroutine; it may block on the body, which the
// engine closes to abandon it.
type StreamDecoder interface {
	Next() (Event, error)
}

// Reader reads one wire format into the neutral types: what a client sent
// and what it received, as opposed to a Dialect (which serves the client)
// and a Provider (which calls the vendor). It is how the gateway looks at a
// call in any format the same way, whoever served it. A Dialect may also be
// a Reader; a format no client is served in yet ("openai_chat") can be a
// Reader alone. It must be safe for concurrent use.
//
// A Reader must be strict where two parsers could disagree, so a body can
// never be judged on one field and executed on another: it rejects a body
// whose objects repeat a key (or hold two keys that differ only by case,
// which Go's struct decoding would merge), reads field names exactly, and
// refuses what it cannot place (an unknown role, a malformed part) instead
// of skipping it. What it can place but has no neutral slot for goes to
// Extra or Raw, as for a Dialect.
type Reader interface {
	// Name is the registry name, the wire format's ("anthropic").
	Name() string
	// DecodeRequest reads a client request body. A malformed or ambiguous
	// body is a *RequestError. The body is not capped here: the caller
	// bounds it.
	DecodeRequest(body []byte) (*Request, error)
	// DecodeResponse reads a complete (non-stream) successful response body
	// as the client received it; a body over MaxBodyBytes is
	// ErrFrameTooLarge.
	DecodeResponse(body []byte) (*Response, error)
	// NewResponseDecoder reads a streamed response as the client received
	// it, under the StreamDecoder contract: the events seen so far, then
	// io.EOF after message_stop, or io.ErrUnexpectedEOF when the stream
	// was cut before the model finished. An ambiguous frame ends the
	// stream with an error.
	NewResponseDecoder(r io.Reader) StreamDecoder
}

// TokenEstimator is implemented by a Provider that can estimate a request's
// input tokens (for a client's token-count endpoint) better than
// EstimateTokens.
type TokenEstimator interface {
	EstimateTokens(req *Request) int64
}

// EstimateTokens is the default input-token estimate: one token per four
// characters (runes) of the text the model reads, rounded up. It counts the
// system prompt, every text, thinking, tool_use input and tool_result text,
// and each tool's name, description and input schema. Images and documents
// other than plain text are not counted. It is an estimate, not a
// tokenizer: vendors' tokenizers differ by tens of percent.
func EstimateTokens(req *Request) int64 {
	var chars int
	var blocks func([]Block)
	blocks = func(bs []Block) {
		for _, b := range bs {
			chars += utf8.RuneCountInString(b.Text) + utf8.RuneCountInString(b.Thinking) + utf8.RuneCount(b.Input)
			if b.Source != nil && b.Source.Type == "text" {
				chars += utf8.RuneCountInString(b.Source.Data)
			}
			blocks(b.Content)
		}
	}
	blocks(req.System)
	for _, m := range req.Messages {
		blocks(m.Content)
	}
	for _, t := range req.Tools {
		chars += utf8.RuneCountInString(t.Name) + utf8.RuneCountInString(t.Description) + utf8.RuneCount(t.InputSchema)
	}
	return int64((chars + 3) / 4)
}

var (
	registryMu sync.RWMutex
	dialects   = map[string]Dialect{}
	providers  = map[string]Provider{}
	readers    = map[string]Reader{}
)

// RegisterDialect makes d available under d.Name(). It panics on a nil
// Dialect or a duplicate name -- programmer errors caught at init time.
func RegisterDialect(d Dialect) {
	if d == nil {
		panic("llm: RegisterDialect called with a nil Dialect")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := dialects[d.Name()]; dup {
		panic("llm: RegisterDialect called twice for " + d.Name())
	}
	dialects[d.Name()] = d
}

// RegisterProvider makes p available under p.Name(). It panics on a nil
// Provider or a duplicate name.
func RegisterProvider(p Provider) {
	if p == nil {
		panic("llm: RegisterProvider called with a nil Provider")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := providers[p.Name()]; dup {
		panic("llm: RegisterProvider called twice for " + p.Name())
	}
	providers[p.Name()] = p
}

// RegisterReader makes r available under r.Name(). It panics on a nil
// Reader or a duplicate name.
func RegisterReader(r Reader) {
	if r == nil {
		panic("llm: RegisterReader called with a nil Reader")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := readers[r.Name()]; dup {
		panic("llm: RegisterReader called twice for " + r.Name())
	}
	readers[r.Name()] = r
}

// DialectByName returns the Dialect registered under name.
func DialectByName(name string) (Dialect, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if d, ok := dialects[name]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("llm: unknown dialect %q (registered: %s; missing blank import?)", name, names(dialects))
}

// ProviderByName returns the Provider registered under name.
func ProviderByName(name string) (Provider, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if p, ok := providers[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("llm: unknown provider %q (registered: %s; missing blank import?)", name, names(providers))
}

// ReaderByName returns the Reader registered under name.
func ReaderByName(name string) (Reader, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if r, ok := readers[name]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("llm: unknown reader %q (registered: %s; missing blank import?)", name, names(readers))
}

func names[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}
