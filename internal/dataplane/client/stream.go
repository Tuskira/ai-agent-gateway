package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// HeaderLastEventID is the SSE resumption header: a client reconnecting
// to a stream names the last event id it saw, and a server that keeps
// history replays what came after it.
const HeaderLastEventID = "Last-Event-ID"

// ErrStreamUnsupported means a connector answered the GET that opens a
// server-to-client stream with 405 or 404: it offers no such stream (or
// not for this session). It is a fact about the connector, not a
// failure, so a caller records it rather than retrying.
var ErrStreamUnsupported = errors.New("client: connector offers no server-to-client stream")

// StreamMessage is one JSON-RPC message read off a server-to-client
// stream.
type StreamMessage struct {
	// EventID is the stream's last event id as of this message ("" when
	// the server sends none), to resume from with Last-Event-ID.
	EventID string
	// Raw is the message exactly as the server sent it.
	Raw json.RawMessage
}

// Stream is an open server-to-client SSE stream to one connector: the
// long-lived GET a Streamable HTTP server uses to send notifications and
// requests that are not replies to anything the client sent.
//
// It carries no deadline once open. An idle stream is healthy -- a
// server with nothing to say says nothing -- so the per-connector
// timeout bounds only the wait for the response headers.
type Stream struct {
	body   io.ReadCloser
	events *sseReader
	cancel context.CancelFunc
	once   sync.Once
}

// Next blocks until the next message and returns it. It returns io.EOF
// when the server ends the stream, and the read error when the
// connection fails or the stream is closed.
func (s *Stream) Next() (StreamMessage, error) {
	for {
		ev, err := s.events.next()
		if err != nil {
			return StreamMessage{}, err
		}
		if !ev.isMessage() {
			continue
		}
		return StreamMessage{EventID: ev.id, Raw: json.RawMessage(ev.data)}, nil
	}
}

// Close ends the stream, unblocking a Next in progress. It is safe to
// call more than once.
func (s *Stream) Close() error {
	var err error
	s.once.Do(func() {
		s.cancel()
		err = s.body.Close()
	})
	return err
}

// StreamStatusError is a GET that the connector answered with a status
// other than 200, 404 or 405.
type StreamStatusError struct {
	Status int
	Body   string
}

func (e *StreamStatusError) Error() string {
	return fmt.Sprintf("client: stream request returned status %d: %s", e.Status, e.Body)
}

// OpenStream opens call.Connector's server-to-client stream: a GET on
// its endpoint accepting text/event-stream, carrying the backend session
// id and protocol version in call and every header a call carries (see
// newRequest). lastEventID, when set, asks the server to resume after
// that event. call.Request is ignored.
//
// A 405 or 404 is ErrStreamUnsupported. A 401 evicts the connector's
// cached credentials and is replayed once, as for a call. There is no
// retry here otherwise: reconnecting is the caller's policy.
func (c *Client) OpenStream(ctx context.Context, call Call, lastEventID string) (*Stream, error) {
	if call.Connector == nil {
		return nil, fmt.Errorf("client: stream has no connector")
	}

	stream, status, err := c.openStream(ctx, call, lastEventID)
	if status == http.StatusUnauthorized && c.evictCredentials(ctx, call.Connector) {
		stream, _, err = c.openStream(ctx, call, lastEventID)
	}
	return stream, err
}

func (c *Client) openStream(ctx context.Context, call Call, lastEventID string) (*Stream, int, error) {
	version := call.ProtocolVersion
	if version == "" {
		version = mcp.ProtocolVersion
	}

	// The stream outlives this function, so its context is one Close
	// can cancel. The connector's timeout bounds only the wait for the
	// headers: once they arrive the timer is stopped, and a stream that
	// then sits idle for an hour is as healthy as one that talks.
	streamCtx, cancel := context.WithCancel(ctx)
	timeout := c.defaultTimeout
	if call.Connector.TimeoutMS > 0 {
		timeout = time.Duration(call.Connector.TimeoutMS) * time.Millisecond
	}
	timer := time.AfterFunc(timeout, cancel)

	httpReq, err := c.newRequest(streamCtx, http.MethodGet, call, version, "text/event-stream", nil)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, 0, err
	}
	if lastEventID != "" {
		httpReq.Header.Set(HeaderLastEventID, lastEventID)
	}

	httpClient, err := c.httpClient(call.Connector)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, 0, err
	}

	c.logger.Debug("opening backend MCP stream",
		"connector_id", call.Connector.ID, "endpoint", call.Connector.Endpoint,
		"protocol_version", version, "resuming", lastEventID != "",
		"headers", maskHeaders(httpReq.Header))

	httpResp, err := httpClient.Do(httpReq)
	if !timer.Stop() {
		// The timer fired: whatever came back, the headers were late.
		if err == nil {
			httpResp.Body.Close()
		}
		cancel()
		return nil, 0, fmt.Errorf("%w: no stream headers from %s within %s", ErrTimeout, call.Connector.Endpoint, timeout)
	}
	if err != nil {
		cancel()
		return nil, 0, fmt.Errorf("client: open stream to %s: %w", call.Connector.Endpoint, err)
	}

	switch {
	case httpResp.StatusCode == http.StatusMethodNotAllowed || httpResp.StatusCode == http.StatusNotFound:
		httpResp.Body.Close()
		cancel()
		return nil, httpResp.StatusCode, fmt.Errorf("%w (status %d)", ErrStreamUnsupported, httpResp.StatusCode)
	case httpResp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1024))
		httpResp.Body.Close()
		cancel()
		return nil, httpResp.StatusCode, &StreamStatusError{Status: httpResp.StatusCode, Body: snippet(body)}
	case !isEventStream(httpResp.Header.Get("Content-Type")):
		httpResp.Body.Close()
		cancel()
		return nil, httpResp.StatusCode, fmt.Errorf("client: stream request returned Content-Type %q, want text/event-stream",
			httpResp.Header.Get("Content-Type"))
	}

	return &Stream{body: httpResp.Body, events: newSSEReader(httpResp.Body), cancel: cancel}, http.StatusOK, nil
}

// Reply POSTs resp -- the gateway's answer to a request the connector
// sent over its stream -- back to the connector's endpoint, on the
// backend session in call. The connector acknowledges it with 200, 202
// or 204. call.Request is ignored; the per-connector timeout applies.
func (c *Client) Reply(ctx context.Context, call Call, resp *mcp.Response) error {
	if call.Connector == nil {
		return fmt.Errorf("client: reply has no connector")
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("client: marshal reply: %w", err)
	}

	timeout := c.defaultTimeout
	if call.Connector.TimeoutMS > 0 {
		timeout = time.Duration(call.Connector.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	version := call.ProtocolVersion
	if version == "" {
		version = mcp.ProtocolVersion
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, call, version, "application/json, text/event-stream", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient, err := c.httpClient(call.Connector)
	if err != nil {
		return err
	}
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return classifyErr(fmt.Errorf("client: send reply to %s: %w", call.Connector.Endpoint, err))
	}
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1024))

	switch httpResp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent:
		return nil
	default:
		return fmt.Errorf("client: reply returned status %d: %s", httpResp.StatusCode, snippet(raw))
	}
}

// SubscribeResource asks a backend to send notifications/resources/updated
// for uri, the backend's own URI with the gateway's "gw://<connector>/"
// prefix already removed. The updates arrive on the connector's stream
// (OpenStream), not on this call. A JSON-RPC error from the backend
// comes back as an *RPCError.
func (c *Client) SubscribeResource(ctx context.Context, call Call, uri string) (*Result, error) {
	return c.resourceSubscription(ctx, call, mcp.MethodResourcesSubscribe, uri)
}

// UnsubscribeResource undoes SubscribeResource.
func (c *Client) UnsubscribeResource(ctx context.Context, call Call, uri string) (*Result, error) {
	return c.resourceSubscription(ctx, call, mcp.MethodResourcesUnsubscribe, uri)
}

func (c *Client) resourceSubscription(ctx context.Context, call Call, method, uri string) (*Result, error) {
	raw, err := json.Marshal(mcp.ResourcesSubscribeParams{URI: uri})
	if err != nil {
		return nil, fmt.Errorf("client: marshal %s params: %w", method, err)
	}
	call.Request = &mcp.Request{JSONRPC: mcp.Version, ID: method, Method: method, Params: raw}

	var out json.RawMessage
	return c.doDecode(ctx, call, &out)
}
