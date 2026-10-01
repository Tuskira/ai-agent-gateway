package client

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// maxRequestsPerCall bounds how many server-to-client requests one call's
// streamed reply may have awaiting an answer at once. A connector that
// sends more is refused on the excess rather than growing goroutines
// without limit.
const maxRequestsPerCall = 4

// requestDispatcher answers the requests a connector sends inside the
// streamed reply to one call -- a tool asking for sampling or elicitation
// mid-tools/call. Each is handed to Call.OnRequest on its own goroutine,
// so the reply keeps being read (the connector may stream progress, or
// its final response, meanwhile), and the answer is POSTed back to the
// connector with Reply under the connector's own id, on the backend
// session the call runs on.
type requestDispatcher struct {
	c       *Client
	call    Call
	handle  func(ctx context.Context, req mcp.Request) (json.RawMessage, *mcp.Error)
	ctx     context.Context
	stop    context.CancelFunc
	clock   *callDeadline
	slots   chan struct{}
	pending sync.WaitGroup
}

// newRequestDispatcher returns the dispatcher for one exchange. sessionID
// and version are the backend session and protocol version the reply
// arrived on, which the answers must carry too.
func (c *Client) newRequestDispatcher(ctx context.Context, call Call, sessionID, version string) *requestDispatcher {
	ctx, stop := context.WithCancel(ctx)
	call.SessionID, call.ProtocolVersion = sessionID, version
	return &requestDispatcher{
		c:      c,
		call:   call,
		handle: call.OnRequest,
		ctx:    ctx,
		stop:   stop,
		clock:  deadlineFrom(ctx),
		slots:  make(chan struct{}, maxRequestsPerCall),
	}
}

// dispatch starts answering one request. It never blocks.
func (d *requestDispatcher) dispatch(id json.RawMessage, req mcp.Request) {
	// Answer under the connector's own id, byte for byte: a number
	// decoded through float64 and re-encoded could come back different.
	id = append(json.RawMessage(nil), id...)

	select {
	case d.slots <- struct{}{}:
	default:
		d.c.logger.Warn("connector sent more concurrent requests in one reply than the gateway answers; refusing one",
			"connector_id", d.call.Connector.ID, "method", req.Method, "limit", maxRequestsPerCall)
		d.pending.Add(1)
		go func() {
			defer d.pending.Done()
			d.reply(mcp.NewErrorResponse(id, mcp.NewInternalError("too many concurrent requests")))
		}()
		return
	}

	d.pending.Add(1)
	go func() {
		defer d.pending.Done()
		defer func() { <-d.slots }()

		var (
			result json.RawMessage
			rpcErr *mcp.Error
		)
		if d.handle == nil {
			rpcErr = mcp.NewError(mcp.ErrorCodeMethodNotFound, "not supported by this gateway: "+req.Method, nil)
		} else {
			// The wait for the answer is the agent's time, not the
			// connector's: hold the call's timeout while it runs.
			d.clock.pause()
			result, rpcErr = d.handle(d.ctx, req)
			d.clock.resume()
		}
		if d.ctx.Err() != nil {
			return // the call is over; nobody is waiting for the answer
		}
		if rpcErr != nil {
			d.reply(mcp.NewErrorResponse(id, rpcErr))
			return
		}
		if len(result) == 0 {
			result = json.RawMessage(`{}`)
		}
		d.reply(&mcp.Response{JSONRPC: mcp.Version, ID: id, Result: result})
	}()
}

func (d *requestDispatcher) reply(resp *mcp.Response) {
	if err := d.c.Reply(d.ctx, d.call, resp); err != nil && d.ctx.Err() == nil {
		d.c.logger.Warn("failed to answer a connector's request", "connector_id", d.call.Connector.ID, "error", err)
	}
}

// close ends every answer still in progress -- the call they belonged to
// is over -- and waits for them.
func (d *requestDispatcher) close() {
	d.stop()
	d.pending.Wait()
}
