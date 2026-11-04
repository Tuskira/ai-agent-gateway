package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/turn"
	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire"
)

// The inline tool-call check (POST /v1/tool-calls/check): before the
// gateway forwards a granted MCP tools/call to its connector, it hands the
// call to the agent, which redacts its arguments and asks the engine for a
// verdict. Only a tenant whose engine policy sets inline_tool_calls is
// checked. Like the inline request stage, every failure fails open: the
// agent answers 200 with no block, and the gateway forwards the call.

// maxToolCheckBytes bounds a ToolCallCheck body: the arguments are cut at
// wire.MaxToolCallArguments by the gateway, the rest is a few ids.
const maxToolCheckBytes = 1 << 20

// toolCheckStats are the check's counts on /healthz: verdicts the engine
// returned (sent) and how many blocked, checks that failed open (engine
// error), checks answered unchecked because no inline slot was free (busy),
// and arguments dropped because their redaction did not finish.
type toolCheckStats struct {
	sent, blocked, failedOpen, busy, argumentsDropped atomic.Int64
}

func (a *Agent) toolCheck(w http.ResponseWriter, r *http.Request) {
	var c wire.ToolCallCheck
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxToolCheckBytes)).Decode(&c); err != nil {
		http.Error(w, "agent: bad tool-call check: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case c.V > wire.TurnVersion:
		http.Error(w, fmt.Sprintf("%s %d: this agent speaks %d", wire.UnsupportedVersion, c.V, wire.TurnVersion), http.StatusBadRequest)
		return
	case c.TenantID == "":
		http.Error(w, "agent: bad tool-call check: tenant_id is required", http.StatusBadRequest)
		return
	case c.Server == "":
		http.Error(w, "agent: bad tool-call check: server is required", http.StatusBadRequest)
		return
	case c.Tool == "":
		http.Error(w, "agent: bad tool-call check: tool is required", http.StatusBadRequest)
		return
	}
	if !a.policy(r.Context(), c.TenantID).InlineToolCalls {
		answerJSON(w, wire.ToolCallVerdict{})
		return
	}
	// The same slots as the inline request stage: each check holds a
	// handler goroutine and an engine connection until it is answered.
	if !a.acquireInline() {
		a.toolChecks.busy.Add(1)
		answerJSON(w, wire.ToolCallVerdict{})
		return
	}
	defer func() { <-a.inline }()
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.ToolCheckTimeout)
	defer cancel()
	answerJSON(w, a.checkToolCall(ctx, c))
}

// checkToolCall redacts c and asks the engine; any engine failure is no
// block (fail open).
func (a *Agent) checkToolCall(ctx context.Context, c wire.ToolCallCheck) wire.ToolCallVerdict {
	if c.Arguments != "" {
		out, err := turn.RedactRaw(ctx, []byte(c.Arguments))
		if err != nil {
			// A scan that did not finish may have missed a secret: the
			// call is checked without its arguments.
			a.toolChecks.argumentsDropped.Add(1)
			c.Arguments, c.ArgumentsTruncated = "", false
		} else {
			c.Arguments = string(out)
		}
	}
	c.V = wire.TurnVersion
	c.ClientIP = a.clientIP(c.ClientIP)
	var v wire.ToolCallVerdict
	if err := a.call(ctx, http.MethodPost, wire.PathToolCallCheck, c, &v); err != nil {
		a.toolChecks.failedOpen.Add(1)
		a.warn("agent: tool-call check failed; failing open", err, "request_id", c.ID, "tenant", c.TenantID)
		return wire.ToolCallVerdict{}
	}
	a.toolChecks.sent.Add(1)
	if v.Block != nil {
		a.toolChecks.blocked.Add(1)
	}
	return v
}
