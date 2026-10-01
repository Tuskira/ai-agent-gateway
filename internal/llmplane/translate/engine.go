// Package translate is the LLM plane's translation engine: it serves a call
// whose client speaks one wire format (an llm.Dialect) by a vendor that
// speaks another (an llm.Provider).
//
// The algorithm, for one call:
//
//  1. Dialect.ParseRequest turns the client's body into an llm.Request.
//  2. Check refuses (400 "unsupported_by_route: <field>", in the client's
//     error envelope) any field the Provider's Capabilities cannot carry,
//     BEFORE anything is sent upstream. Only the fields on the drop list
//     (check.go) are left out instead.
//  3. Provider.BuildRequest builds the vendor request with a fresh header
//     set: nothing of the client's request reaches the vendor except what
//     the neutral form carries and the Target's key.
//  4. The vendor's answer is translated back. Only an allowlist of vendor
//     headers survives, a vendor redirect becomes 502, and a vendor 401/403
//     on the operator's key is reported without the vendor's message.
//     Non-stream: ParseResponse then RenderResponse, the body capped at
//     llm.MaxBodyBytes. Stream: StreamDecoder events are re-encoded one by
//     one by the Dialect's StreamEncoder; nothing is buffered here, and the
//     decoders hold at most llm.MaxFrameBytes per pending block. A ping is
//     sent every PingEvery while the vendor is silent, and a stream that
//     fails or is cut before its end finishes with an error event.
//
// The LLM plane's router calls Prepare and Call.Response around its own
// relay (which captures and prices the call); Run is the same algorithm as
// one self-contained handler.
package translate

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// PingEvery keeps bytes flowing while a vendor is silent (a reasoning model
// thinking): Claude Code aborts a stream idle for 5 minutes. A var so tests
// can shorten it.
var PingEvery = 15 * time.Second

// Options are per-call settings the engine cannot infer.
type Options struct {
	// RequestID is the gateway request id, echoed in error envelopes.
	RequestID string
	// ManagedKey means the Target's key is the operator's, not the caller's:
	// a vendor 401/403 is then reported without the vendor's message (often
	// a partly masked key, not the caller's business).
	ManagedKey bool
}

// StatusError is a failure answered before anything is sent upstream.
type StatusError struct {
	Status  int
	Message string // for the client
	Err     error  // the cause, for capture
}

func (e *StatusError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *StatusError) Unwrap() error { return e.Err }

// Call is one prepared translated call.
type Call struct {
	// Request is the client's request in neutral form.
	Request *llm.Request
	// Upstream is the vendor request to send. Its context carries the Call
	// (see CallOf).
	Upstream *http.Request

	d    llm.Dialect
	p    llm.Provider
	t    llm.Target
	opts Options

	mu    sync.Mutex
	usage llm.Usage
}

type callKey struct{}

// CallOf returns the Call whose Upstream request is req (or a request
// derived from it), or nil.
func CallOf(req *http.Request) *Call {
	if req == nil {
		return nil
	}
	c, _ := req.Context().Value(callKey{}).(*Call)
	return c
}

// Usage is the usage the vendor reported so far (final once the response
// body has been read to its end).
func (c *Call) Usage() llm.Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}

func (c *Call) setUsage(u llm.Usage, start bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if start { // message_start: input side only; message_delta overrides
		c.usage.InputTokens, c.usage.CacheReadTokens, c.usage.CacheWriteTokens = u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens
		return
	}
	c.usage = u
}

// Prepare runs steps 1-3: parse r with d, check it against p, and build the
// upstream request for t. A failure is a *StatusError (400 for the client's
// request, 502 when the upstream request cannot be built).
func Prepare(ctx context.Context, r *http.Request, d llm.Dialect, p llm.Provider, t llm.Target, opts Options) (*Call, error) {
	req, err := d.ParseRequest(r)
	if err != nil {
		return nil, &StatusError{Status: http.StatusBadRequest, Message: "cannot translate request: " + err.Error(), Err: err}
	}
	if err := Check(req, p.Capabilities()); err != nil {
		return nil, &StatusError{Status: http.StatusBadRequest, Message: err.Error(), Err: err}
	}
	up, err := p.BuildRequest(ctx, req, t)
	if err != nil {
		var unsup *llm.ErrUnsupported
		var bad *llm.RequestError
		switch {
		case errors.As(err, &unsup):
			return nil, &StatusError{Status: http.StatusBadRequest, Message: unsup.Error(), Err: err}
		case errors.As(err, &bad):
			return nil, &StatusError{Status: http.StatusBadRequest, Message: "cannot translate request for model " + req.Model + ": " + bad.Error(), Err: err}
		}
		return nil, &StatusError{Status: http.StatusBadGateway, Message: "upstream request build failed", Err: err}
	}
	c := &Call{Request: req, d: d, p: p, t: t, opts: opts}
	c.Upstream = up.WithContext(context.WithValue(up.Context(), callKey{}, c))
	return c, nil
}

// keepHeaders are the only vendor response headers relayed: the rest
// describe the vendor account (organization, project, rate limits of an
// operator key, cookies), and a Location would let the client re-send its
// request and credentials to the vendor.
var keepHeaders = []string{"Retry-After", "Request-Id", "X-Request-Id", "X-Should-Retry"}

// Response runs step 4: it rewrites resp in place into the client's format.
// A 2xx event stream is translated incrementally through a pipe (closing
// resp.Body stops the translating goroutine and closes the vendor body);
// any other body is small and translated whole. A failure the client can
// still be told about becomes an error in its envelope, and reading the
// body then fails, so a relay that captures the call records the failure.
func (c *Call) Response(resp *http.Response) {
	src, ct := resp.Body, resp.Header.Get("Content-Type")
	keep := http.Header{}
	for _, k := range keepHeaders {
		if v := resp.Header.Values(k); len(v) > 0 {
			keep[k] = v
		}
	}
	resp.Header = keep
	resp.ContentLength = -1
	ok := resp.StatusCode/100 == 2

	if ok && strings.HasPrefix(ct, "text/event-stream") {
		pr, pw := io.Pipe()
		go func() {
			defer src.Close()
			// The client already has its error event; failing the pipe makes
			// the relay (and capture) record the failure too.
			pw.CloseWithError(c.stream(pw, src))
		}()
		resp.Body = pipeBody{pr, src}
		resp.Header.Set("Content-Type", "text/event-stream")
		return
	}

	b, err := io.ReadAll(io.LimitReader(src, llm.MaxBodyBytes+1))
	if err == nil && len(b) > llm.MaxBodyBytes {
		err = llm.ErrFrameTooLarge
	}
	src.Close()
	resp.Header.Set("Content-Type", "application/json")
	var out []byte
	switch {
	case err != nil:
		resp.StatusCode = http.StatusBadGateway
		out = c.errorBody(http.StatusBadGateway, "upstream read failed")
	case !ok && resp.StatusCode < 400: // 1xx/3xx: nothing to relay
		resp.StatusCode = http.StatusBadGateway
		out = c.errorBody(http.StatusBadGateway, "upstream redirected")
	case !ok && c.opts.ManagedKey && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden):
		out = c.errorBody(resp.StatusCode, "upstream credential rejected for model "+c.Request.Model)
	case !ok:
		e := c.p.ParseError(resp.StatusCode, b)
		e.Status, e.RequestID = resp.StatusCode, c.opts.RequestID
		out = c.d.RenderError(e)
	default:
		var r *llm.Response
		r, err = c.p.ParseResponse(&http.Response{StatusCode: resp.StatusCode, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(bytes.NewReader(b))})
		if err == nil {
			r.Model = c.Request.Model
			c.setUsage(r.Usage, false)
			out, err = c.d.RenderResponse(r)
		}
		if err != nil {
			resp.StatusCode = http.StatusBadGateway
			out = c.errorBody(http.StatusBadGateway, "cannot translate upstream response")
		}
	}
	var body io.Reader = bytes.NewReader(out)
	if err != nil {
		body = io.MultiReader(body, errReader{err})
	}
	resp.Body = io.NopCloser(body)
}

func (c *Call) errorBody(status int, msg string) []byte {
	return c.d.RenderError(&llm.Error{Status: status, Type: llm.ErrorTypeForStatus(status), Message: msg, RequestID: c.opts.RequestID})
}

// stream re-encodes the vendor stream src onto dst. It returns nil for a
// complete stream, and otherwise the failure (after the client got an error
// event, when it can still be written).
func (c *Call) stream(dst io.Writer, src io.Reader) error {
	enc := c.d.NewStreamEncoder(dst)
	dec := c.p.NewStreamDecoder(src)

	type item struct {
		ev  llm.Event
		err error
	}
	items := make(chan item)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		for {
			ev, err := dec.Next()
			select {
			case items <- item{ev, err}:
			case <-quit:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	tick := time.NewTicker(PingEvery)
	defer tick.Stop()
	started := false
	// begin writes message_start: the decoder's, or -- when the vendor is
	// silent past a ping interval or fails before its first event -- one made
	// here, followed by a ping as a decoder's would be.
	begin := func(msg *llm.Response) error {
		made := msg == nil
		if made {
			msg = &llm.Response{ID: "msg_" + randText(), Role: "assistant", Content: []llm.Block{}}
		}
		m := *msg
		m.Model = c.Request.Model
		c.setUsage(m.Usage, true)
		started = true
		err := enc.Write(llm.Event{Type: llm.EventMessageStart, Message: &m})
		if err == nil && made {
			err = enc.Write(llm.Event{Type: llm.EventPing})
		}
		return err
	}
	fail := func(typ, msg string) error {
		if !started {
			if err := begin(nil); err != nil {
				return err
			}
		}
		return enc.Write(llm.Event{Type: llm.EventError, Error: &llm.Error{Type: typ, Message: msg}})
	}

	for {
		select {
		case <-tick.C:
			var err error
			if !started {
				err = begin(nil)
			} else {
				err = enc.Write(llm.Event{Type: llm.EventPing})
			}
			if err != nil {
				return err
			}
		case it := <-items:
			switch {
			case it.err == nil:
			case errors.Is(it.err, io.ErrUnexpectedEOF), errors.Is(it.err, io.EOF):
				// io.EOF here means a decoder ended without message_stop.
				// Ending it as a complete message would hand the client a
				// truncated answer or tool input as final.
				return firstErr(fail(llm.ErrorTypeAPI, "upstream stream ended early"), io.ErrUnexpectedEOF)
			default:
				// The cause stays in the returned error (capture), not the client's.
				_ = fail(llm.ErrorTypeAPI, "upstream stream interrupted")
				return it.err
			}
			ev := it.ev
			switch ev.Type {
			case llm.EventMessageStart:
				if started {
					continue // one was made while the vendor was silent
				}
				if err := begin(ev.Message); err != nil {
					return err
				}
				continue
			case llm.EventError:
				e := llm.Error{Type: llm.ErrorTypeAPI, Message: "upstream error"}
				if ev.Error != nil {
					e = *ev.Error
				}
				if err := fail(e.Type, e.Message); err != nil {
					return err
				}
				return errors.New("upstream stream error: " + e.Message)
			case llm.EventMessageDelta:
				if ev.Usage != nil {
					u := *ev.Usage
					prev := c.Usage()
					u.InputTokens, u.CacheReadTokens = max(u.InputTokens, prev.InputTokens), max(u.CacheReadTokens, prev.CacheReadTokens)
					u.CacheWriteTokens = max(u.CacheWriteTokens, prev.CacheWriteTokens)
					c.setUsage(u, false)
				}
			}
			if !started {
				if err := begin(nil); err != nil {
					return err
				}
			}
			if err := enc.Write(ev); err != nil {
				return err
			}
			if ev.Type == llm.EventMessageStop {
				return enc.Close()
			}
		}
	}
}

// Result is what Run did.
type Result struct {
	Status int
	Usage  llm.Usage
	// Err is the failure, if any: before the upstream call (a *StatusError),
	// of the upstream call, or of the relay.
	Err error
}

// Client sends Run's upstream requests: TLS verified, no timeout (streams),
// and redirects never followed -- following one would re-send the body and
// the vendor key to whatever host the Location names.
var Client = &http.Client{Transport: netguard.Transport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Run serves one call end to end: steps 1-4 with Client, relaying the
// translated answer to w (flushed per write).
func Run(ctx context.Context, w http.ResponseWriter, r *http.Request, d llm.Dialect, p llm.Provider, t llm.Target, opts Options) Result {
	call, err := Prepare(ctx, r, d, p, t, opts)
	if err != nil {
		var se *StatusError
		errors.As(err, &se)
		writeError(w, d, se.Status, se.Message, opts.RequestID)
		return Result{Status: se.Status, Err: err}
	}
	resp, err := Client.Do(call.Upstream)
	if err != nil {
		writeError(w, d, http.StatusBadGateway, "upstream error", opts.RequestID)
		return Result{Status: http.StatusBadGateway, Err: err}
	}
	call.Response(resp)
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(flushWriter{w}, resp.Body)
	return Result{Status: resp.StatusCode, Usage: call.Usage(), Err: err}
}

// EstimateTokens parses r with d and returns p's input-token estimate
// (llm.TokenEstimator), or llm.EstimateTokens when p has none. It serves a
// client's token-count endpoint for a translated model: the vendor's own
// tokenizer is not reachable, so the answer is marked estimated.
func EstimateTokens(r *http.Request, d llm.Dialect, p llm.Provider) (int64, error) {
	req, err := d.ParseRequest(r)
	if err != nil {
		return 0, err
	}
	if te, ok := p.(llm.TokenEstimator); ok {
		return te.EstimateTokens(req), nil
	}
	return llm.EstimateTokens(req), nil
}

// EstimateBody is the count_tokens answer for an estimate.
func EstimateBody(n int64) []byte {
	return []byte(`{"input_tokens":` + itoa(n) + `,"estimated":true}`)
}

func writeError(w http.ResponseWriter, d llm.Dialect, status int, msg, reqID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(d.RenderError(&llm.Error{Status: status, Type: llm.ErrorTypeForStatus(status), Message: msg, RequestID: reqID}))
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// pipeBody closes both ends so the translating goroutine never outlives the
// relay: closing the reader fails its pending Write, closing the upstream
// body fails its pending Read.
type pipeBody struct {
	*io.PipeReader
	up io.Closer
}

func (b pipeBody) Close() error {
	_ = b.PipeReader.Close()
	return b.up.Close()
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func randText() string { return rand.Text() }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
