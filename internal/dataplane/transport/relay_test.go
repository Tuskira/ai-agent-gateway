package transport_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// allCapabilities is an agent that declares every capability that
// invites a server-initiated request, roots with a sub-field.
const allCapabilities = `{"sampling":{},"elicitation":{},"roots":{"listChanged":true}}`

// allowAll is connector metadata whose policy permits every
// server-initiated request.
func allowAll() map[string]any {
	return map[string]any{"server_requests": map[string]any{"sampling": true, "elicitation": true, "roots": true}}
}

func askCall(id any, args string) mcp.Request {
	return mcp.Request{JSONRPC: mcp.Version, ID: id, Method: mcp.MethodToolsCall,
		Params: json.RawMessage(`{"name":"alpha__` + dptest.AskTool + `","arguments":` + args + `}`)}
}

type callOutcome struct {
	resp mcp.Response
	err  error
}

// callAsync runs a tools/call in the background: it blocks until the
// agent has answered what the tool asks.
func (f *fixture) callAsync(sessionID string, req mcp.Request) <-chan callOutcome {
	out := make(chan callOutcome, 1)
	go func() {
		_, resp, err := f.post(sessionID, req)
		out <- callOutcome{resp, err}
	}()
	return out
}

func waitCall(t *testing.T, ch <-chan callOutcome) mcp.Response {
	t.Helper()
	select {
	case out := <-ch:
		if out.err != nil {
			t.Fatalf("tools/call: %v", out.err)
		}
		return out.resp
	case <-time.After(10 * time.Second):
		t.Fatal("the tools/call never returned")
		return mcp.Response{}
	}
}

// respond POSTs raw as the agent's answer on sessionID and returns the
// HTTP status and body.
func (f *fixture) respond(t *testing.T, sessionID, raw string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/mcp", strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if sessionID != "" {
		req.Header.Set(mcp.HeaderSessionID, sessionID)
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// answer POSTs a result for the relayed request id and requires the 202.
func (f *fixture) answer(t *testing.T, sessionID, id, result string) {
	t.Helper()
	status, body := f.respond(t, sessionID, `{"jsonrpc":"2.0","id":"`+id+`","result":`+result+`}`)
	if status != http.StatusAccepted || body != "" {
		t.Fatalf("answer POST = %d %q, want 202 and no body", status, body)
	}
}

// nextRequest reads the agent's stream until a relayed request arrives.
func nextRequest(t *testing.T, frames <-chan string) mcp.Request {
	t.Helper()
	msg, ok := nextNotification(t, frames, 5*time.Second)
	if !ok {
		t.Fatal("no request reached the agent's stream")
	}
	id, _ := msg.ID.(string)
	if !strings.HasPrefix(id, "gw-") || len(id) != len("gw-")+16 {
		t.Fatalf("relayed message %+v: want a request with a gw-<16 hex> id", msg)
	}
	return msg
}

// noMessage asserts nothing but tools/list_changed reaches the stream
// for a while.
func noMessage(t *testing.T, frames <-chan string) {
	t.Helper()
	if msg, ok := nextNotification(t, frames, 300*time.Millisecond); ok {
		t.Fatalf("the agent's stream got %+v, want nothing", msg)
	}
}

// askAnswers decodes AskTool's result: the responses it received.
func askAnswers(t *testing.T, resp mcp.Response) []mcp.Response {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("tools/call failed: %+v", resp.Error)
	}
	var result mcp.ToolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil || len(result.Content) != 1 {
		t.Fatalf("tools/call result = %s", resp.Result)
	}
	var answers []mcp.Response
	if err := json.Unmarshal([]byte(result.Content[0].Text), &answers); err != nil {
		t.Fatalf("ask result %q: %v", result.Content[0].Text, err)
	}
	return answers
}

// sameJSON reports whether got and want are the same JSON value. The
// tool path goes through dptest decoding its arguments, which loses key
// order; the stream path is checked byte for byte instead.
func sameJSON(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		return false
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(a, b)
}

var relayCases = []struct {
	method, params, result string
}{
	{"sampling/createMessage",
		`{"messages":[{"role":"user","content":{"type":"text","text":"Summarise this"}}],"maxTokens":64}`,
		`{"role":"assistant","content":{"type":"text","text":"A summary."},"model":"agent-model","stopReason":"endTurn"}`},
	{"elicitation/create",
		`{"message":"Which project?","requestedSchema":{"type":"object","properties":{"project":{"type":"string"}}}}`,
		`{"action":"accept","content":{"project":"apollo"}}`},
	{"roots/list", `{}`, `{"roots":[{"uri":"file:///work/apollo","name":"apollo"}]}`},
}

func TestARequestInsideAToolCallReachesTheAgentAndItsAnswerTheConnector(t *testing.T) {
	for _, tc := range relayCases {
		t.Run(tc.method, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll()})
			sessionID := f.initializeWith(t, allCapabilities)
			frames := f.openStream(t, sessionID)

			done := f.callAsync(sessionID, askCall(5, `{"method":"`+tc.method+`","params":`+tc.params+`}`))

			req := nextRequest(t, frames)
			if req.Method != tc.method || !sameJSON(t, req.Params, json.RawMessage(tc.params)) {
				t.Fatalf("relayed %s %s, want %s %s", req.Method, req.Params, tc.method, tc.params)
			}
			f.answer(t, sessionID, req.ID.(string), tc.result)

			answers := askAnswers(t, waitCall(t, done))
			asked := f.backend.AskRequests()
			if len(answers) != 1 || len(asked) != 1 {
				t.Fatalf("answers %+v for %d requests, want 1", answers, len(asked))
			}
			// The connector is answered under its own id, with the
			// agent's result.
			if mcp.FormatID(answers[0].ID) != mcp.FormatID(asked[0].ID) || answers[0].Error != nil ||
				!sameJSON(t, answers[0].Result, json.RawMessage(tc.result)) {
				t.Fatalf("connector got %+v, want the agent's result under id %v", answers[0], asked[0].ID)
			}
		})
	}
}

func TestARequestOnTheConnectorStreamReachesTheAgentAndItsAnswerTheConnector(t *testing.T) {
	for _, tc := range relayCases {
		t.Run(tc.method, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll()})
			sessionID := f.initializeWith(t, allCapabilities)
			frames := f.openStream(t, sessionID)
			// The handshake opened the connector's stream, since the
			// gateway declared it capabilities.
			eventually(t, "the connector stream", func() bool { return f.backend.OpenStreams() == 1 })

			id := f.backend.Request(tc.method, json.RawMessage(tc.params))

			// The params reach the agent exactly as the connector wrote
			// them, key order and all.
			req := nextRequest(t, frames)
			if req.Method != tc.method || !bytes.Equal(req.Params, []byte(tc.params)) {
				t.Fatalf("relayed %s %s, want %s %s verbatim", req.Method, req.Params, tc.method, tc.params)
			}
			f.answer(t, sessionID, req.ID.(string), tc.result)

			reply, ok := f.backend.WaitReply(id, 5*time.Second)
			if !ok {
				t.Fatal("the connector was never answered")
			}
			if reply.Error != nil || !sameJSON(t, reply.Result, json.RawMessage(tc.result)) {
				t.Fatalf("connector got %+v, want the agent's result", reply)
			}
		})
	}
}

func TestAnAgentErrorIsRelayedAsAnError(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll()})
	sessionID := f.initializeWith(t, allCapabilities)
	frames := f.openStream(t, sessionID)

	done := f.callAsync(sessionID, askCall(5, `{"method":"sampling/createMessage"}`))
	req := nextRequest(t, frames)
	status, _ := f.respond(t, sessionID,
		`{"jsonrpc":"2.0","id":"`+req.ID.(string)+`","error":{"code":-1,"message":"User rejected sampling request","data":{"why":"no"}}}`)
	if status != http.StatusAccepted {
		t.Fatalf("error answer POST = %d, want 202", status)
	}

	answers := askAnswers(t, waitCall(t, done))
	if len(answers) != 1 || answers[0].Error == nil || answers[0].Error.Code != -1 ||
		answers[0].Error.Message != "User rejected sampling request" || !sameJSON(t, answers[0].Error.Data, json.RawMessage(`{"why":"no"}`)) {
		t.Fatalf("connector got %+v, want the agent's error verbatim", answers)
	}
}

func TestAPolicyThatForbidsItNeverReachesTheAgent(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: map[string]any{
		"server_requests": map[string]any{"sampling": true},
	}})
	sessionID := f.initializeWith(t, allCapabilities)
	frames := f.openStream(t, sessionID)

	answers := askAnswers(t, waitCall(t, f.callAsync(sessionID, askCall(5, `{"method":"elicitation/create"}`))))
	if len(answers) != 1 || answers[0].Error == nil || answers[0].Error.Code != mcp.ErrorCodeMethodNotFound ||
		!strings.Contains(answers[0].Error.Message, "not permitted for this connector") {
		t.Fatalf("connector got %+v, want -32601 not permitted", answers)
	}
	// A method no capability invites is refused the same way.
	answers = askAnswers(t, waitCall(t, f.callAsync(sessionID, askCall(6, `{"method":"tasks/get"}`))))
	if len(answers) != 1 || answers[0].Error == nil || answers[0].Error.Code != mcp.ErrorCodeMethodNotFound {
		t.Fatalf("connector got %+v, want -32601", answers)
	}
	noMessage(t, frames)
}

func TestACapabilityTheAgentDidNotDeclareNeverReachesIt(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll()})
	sessionID := f.initializeWith(t, `{"sampling":{}}`)
	frames := f.openStream(t, sessionID)

	answers := askAnswers(t, waitCall(t, f.callAsync(sessionID, askCall(5, `{"method":"elicitation/create"}`))))
	if len(answers) != 1 || answers[0].Error == nil || answers[0].Error.Code != mcp.ErrorCodeMethodNotFound ||
		answers[0].Error.Message != "client did not declare elicitation" {
		t.Fatalf("connector got %+v, want -32601 client did not declare elicitation", answers)
	}
	noMessage(t, frames)

	// A request with no session at all has no agent to ask.
	_, resp, err := f.post("", askCall(6, `{"method":"sampling/createMessage"}`))
	if err != nil {
		t.Fatal(err)
	}
	if answers := askAnswers(t, resp); len(answers) != 1 || answers[0].Error == nil ||
		answers[0].Error.Code != mcp.ErrorCodeMethodNotFound {
		t.Fatalf("stateless call: connector got %+v, want -32601", answers)
	}
}

func TestTheConnectorIsDeclaredExactlyTheIntersection(t *testing.T) {
	for _, tc := range []struct {
		name, policy, agent, want string
		stream                    bool
	}{
		{"agent declares nothing", `{"sampling":true,"elicitation":true,"roots":true}`, `{}`, `{}`, false},
		{"policy allows nothing", `{}`, allCapabilities, `{}`, false},
		{"both", `{"sampling":true,"elicitation":false,"roots":true}`, `{"sampling":{},"elicitation":{},"experimental":{"x":{}}}`,
			`{"sampling":{}}`, true},
		{"sub-fields pass through", `{"roots":true,"elicitation":true}`, `{"roots":{"listChanged":true},"elicitation":{"form":{}}}`,
			`{"roots":{"listChanged":true},"elicitation":{"form":{}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var policy map[string]any
			if err := json.Unmarshal([]byte(tc.policy), &policy); err != nil {
				t.Fatal(err)
			}
			f := newFixture(t, fixtureOptions{ConnectorMetadata: map[string]any{"server_requests": policy}})
			sessionID := f.initializeWith(t, tc.agent)

			caps := f.backend.InitializeCapabilities()
			if len(caps) != 1 || !sameJSON(t, caps[0], json.RawMessage(tc.want)) {
				t.Fatalf("backend was declared %s, want %s", caps, tc.want)
			}
			if tc.stream {
				eventually(t, "the connector stream", func() bool { return f.backend.OpenStreams() == 1 })
				// Ending the session releases it.
				status, _ := f.deleteSession(t, sessionID)
				if status != http.StatusNoContent {
					t.Fatalf("DELETE = %d", status)
				}
				eventually(t, "the connector stream to close", func() bool { return f.backend.OpenStreams() == 0 })
			} else if n := f.backend.StreamGETs(); n != 0 {
				t.Fatalf("the gateway opened %d connector streams with nothing to receive on them", n)
			}
		})
	}
}

func (f *fixture) deleteSession(t *testing.T, sessionID string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, f.server.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set(mcp.HeaderSessionID, sessionID)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestAnAgentWithNoOpenStreamIsReportedToTheConnector(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll()})
	sessionID := f.initializeWith(t, allCapabilities)

	answers := askAnswers(t, waitCall(t, f.callAsync(sessionID, askCall(5, `{"method":"elicitation/create"}`))))
	if len(answers) != 1 || answers[0].Error == nil || answers[0].Error.Code != mcp.ErrorCodeInternalError ||
		answers[0].Error.Message != "client has no open stream" {
		t.Fatalf("connector got %+v, want -32603 client has no open stream", answers)
	}
}

func TestAnUnansweredRequestTimesOutAndIsCancelledAtTheAgent(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll(), ServerRequestTimeout: 300 * time.Millisecond})
	sessionID := f.initializeWith(t, allCapabilities)
	frames := f.openStream(t, sessionID)

	done := f.callAsync(sessionID, askCall(5, `{"method":"elicitation/create"}`))
	req := nextRequest(t, frames)

	answers := askAnswers(t, waitCall(t, done))
	if len(answers) != 1 || answers[0].Error == nil || answers[0].Error.Code != mcp.ErrorCodeInternalError ||
		answers[0].Error.Message != "timed out waiting for the client" {
		t.Fatalf("connector got %+v, want -32603 timed out", answers)
	}
	note, ok := nextNotification(t, frames, 5*time.Second)
	if !ok || note.Method != mcp.NotificationCancelled || !strings.Contains(string(note.Params), req.ID.(string)) {
		t.Fatalf("agent got %+v, want notifications/cancelled for %s", note, req.ID)
	}
	// A late answer is accepted and dropped.
	f.answer(t, sessionID, req.ID.(string), `{"action":"cancel"}`)
}

func TestPendingRequestsAreCappedPerSession(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll(), MaxPendingServerRequests: 2})
	sessionID := f.initializeWith(t, allCapabilities)
	frames := f.openStream(t, sessionID)

	done := f.callAsync(sessionID, askCall(5, `{"method":"roots/list","count":3}`))
	first, second := nextRequest(t, frames), nextRequest(t, frames)
	noMessage(t, frames)
	f.answer(t, sessionID, first.ID.(string), `{"roots":[]}`)
	f.answer(t, sessionID, second.ID.(string), `{"roots":[]}`)

	answers := askAnswers(t, waitCall(t, done))
	refused := 0
	for _, a := range answers {
		if a.Error != nil {
			if a.Error.Code != mcp.ErrorCodeInternalError || !strings.Contains(a.Error.Message, "too many requests pending") {
				t.Fatalf("refusal = %+v, want -32603 too many pending", a.Error)
			}
			refused++
		}
	}
	if len(answers) != 3 || refused != 1 {
		t.Fatalf("answers %+v: want 3 with exactly 1 refused", answers)
	}
}

func TestAnAnswerFromAnotherSessionIsIgnored(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll()})
	owner, other := f.initializeWith(t, allCapabilities), f.initializeWith(t, allCapabilities)
	frames := f.openStream(t, owner)

	done := f.callAsync(owner, askCall(5, `{"method":"elicitation/create"}`))
	req := nextRequest(t, frames)

	// The other session answers the owner's id: accepted, and ignored.
	f.answer(t, other, req.ID.(string), `{"action":"accept","content":{"forged":true}}`)
	select {
	case out := <-done:
		t.Fatalf("the call completed on another session's answer: %+v", out.resp)
	case <-time.After(300 * time.Millisecond):
	}

	f.answer(t, owner, req.ID.(string), `{"action":"decline"}`)
	answers := askAnswers(t, waitCall(t, done))
	if len(answers) != 1 || !sameJSON(t, answers[0].Result, json.RawMessage(`{"action":"decline"}`)) {
		t.Fatalf("connector got %+v, want the owner's answer", answers)
	}
}

func TestCancellingTheToolCallCancelsItsRelayedRequest(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll()})
	sessionID := f.initializeWith(t, allCapabilities)
	frames := f.openStream(t, sessionID)

	done := f.callAsync(sessionID, askCall(5, `{"method":"elicitation/create"}`))
	req := nextRequest(t, frames)

	if status, _, err := f.post(sessionID, cancelRequest("5")); err != nil || status != http.StatusNoContent {
		t.Fatalf("notifications/cancelled = %d %v", status, err)
	}
	resp := waitCall(t, done)
	if resp.Error == nil || resp.Error.Code != mcp.ErrorCodeRequestCancelled {
		t.Fatalf("tools/call = %+v, want -32800", resp)
	}
	note, ok := nextNotification(t, frames, 5*time.Second)
	if !ok || note.Method != mcp.NotificationCancelled || !strings.Contains(string(note.Params), `"`+req.ID.(string)+`"`) {
		t.Fatalf("agent got %+v, want notifications/cancelled for %s", note, req.ID)
	}
	// The connector is never answered for a request whose call is gone.
	time.Sleep(200 * time.Millisecond)
	if replies := f.backend.Replies(); len(replies) != 0 {
		t.Fatalf("connector got %+v after the call was cancelled", replies)
	}
}

func TestEndingTheSessionFailsItsPendingRequests(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll()})
	sessionID := f.initializeWith(t, allCapabilities)
	frames := f.openStream(t, sessionID)

	done := f.callAsync(sessionID, askCall(5, `{"method":"roots/list"}`))
	nextRequest(t, frames)
	if status, _ := f.deleteSession(t, sessionID); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d", status)
	}
	answers := askAnswers(t, waitCall(t, done))
	if len(answers) != 1 || answers[0].Error == nil || answers[0].Error.Code != mcp.ErrorCodeInternalError ||
		answers[0].Error.Message != "client session has ended" {
		t.Fatalf("connector got %+v, want -32603 client session has ended", answers)
	}
	// Its connector stream went with it, and nothing leaks.
	eventually(t, "the connector stream to close", func() bool {
		streams, goroutines := f.plane.UpstreamStreams()
		return streams == 0 && goroutines == 0 && f.backend.OpenStreams() == 0
	})
}

func TestWaitingForTheAgentDoesNotSpendTheToolTimeout(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll(), ConnectorTimeoutMS: 1000})
	sessionID := f.initializeWith(t, allCapabilities)
	frames := f.openStream(t, sessionID)

	// 300ms of connector time, 1.5s of agent time, 300ms more of
	// connector time: within the connector's 1s budget.
	done := f.callAsync(sessionID, askCall(5, `{"method":"elicitation/create","delay_ms":300,"after_ms":300}`))
	req := nextRequest(t, frames)
	time.Sleep(1500 * time.Millisecond)
	f.answer(t, sessionID, req.ID.(string), `{"action":"accept","content":{}}`)
	if answers := askAnswers(t, waitCall(t, done)); len(answers) != 1 || answers[0].Error != nil {
		t.Fatalf("connector got %+v, want the answer and no timeout", answers)
	}

	// The budget is paused, not reset: 600ms before and 600ms after the
	// answer is 1.2s of connector time, over budget.
	done = f.callAsync(sessionID, askCall(6, `{"method":"elicitation/create","delay_ms":600,"after_ms":600}`))
	req = nextRequest(t, frames)
	f.answer(t, sessionID, req.ID.(string), `{"action":"accept","content":{}}`)
	resp := waitCall(t, done)
	var result mcp.ToolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil || !result.IsError ||
		len(result.Content) != 1 || !strings.HasPrefix(result.Content[0].Text, "TOOL_TIMEOUT") {
		t.Fatalf("tools/call = %s %+v, want TOOL_TIMEOUT", resp.Result, resp.Error)
	}
}

func TestAResponsePOSTIsValidated(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll()})
	sessionID := f.initializeWith(t, allCapabilities)

	// An id nobody issued: accepted and dropped.
	if status, body := f.respond(t, sessionID, `{"jsonrpc":"2.0","id":"gw-0000000000000000","result":{}}`); status != http.StatusAccepted || body != "" {
		t.Fatalf("unknown id = %d %q, want 202", status, body)
	}
	for name, tc := range map[string]struct {
		session, body string
		code          int
	}{
		"no session":       {"", `{"jsonrpc":"2.0","id":"gw-1","result":{}}`, mcp.ErrorCodeInvalidRequest},
		"unknown session":  {"nope", `{"jsonrpc":"2.0","id":"gw-1","result":{}}`, mcp.ErrorCodeSessionNotFound},
		"result and error": {sessionID, `{"jsonrpc":"2.0","id":"gw-1","result":{},"error":{"code":1,"message":"x"}}`, mcp.ErrorCodeInvalidRequest},
		"wrong jsonrpc":    {sessionID, `{"jsonrpc":"1.0","id":"gw-1","result":{}}`, mcp.ErrorCodeInvalidRequest},
		"error not object": {sessionID, `{"jsonrpc":"2.0","id":"gw-1","error":"no"}`, mcp.ErrorCodeInvalidRequest},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := f.respond(t, tc.session, tc.body)
			var resp mcp.Response
			if status != http.StatusOK || json.Unmarshal([]byte(body), &resp) != nil || resp.Error == nil || resp.Error.Code != tc.code {
				t.Fatalf("= %d %s, want JSON-RPC %d", status, body, tc.code)
			}
		})
	}
}

func TestEveryRelayedRequestIsAccessLogged(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: map[string]any{
		"server_requests": map[string]any{"elicitation": true},
	}})
	sessionID := f.initializeWith(t, allCapabilities)
	frames := f.openStream(t, sessionID)

	done := f.callAsync(sessionID, askCall(5, `{"method":"elicitation/create","params":{"message":"secret question"}}`))
	req := nextRequest(t, frames)
	f.answer(t, sessionID, req.ID.(string), `{"action":"accept","content":{"answer":"secret answer"}}`)
	waitCall(t, done)
	waitCall(t, f.callAsync(sessionID, askCall(6, `{"method":"sampling/createMessage"}`)))

	var relayed, refused, response bool
	for _, rec := range f.sink.all() {
		switch rec.Method {
		case "elicitation/create":
			relayed = true
			if rec.JSONRPCID != req.ID || rec.ConnectorID != f.conn.ID || rec.ToolName != "alpha" ||
				rec.SessionID != sessionID || rec.TenantID != tenantID || rec.ErrorCode != "" {
				t.Errorf("relayed record = %+v", rec)
			}
		case "sampling/createMessage":
			refused = true
			if rec.ErrorCode != "-32601" || rec.ConnectorID != f.conn.ID {
				t.Errorf("refused record = %+v", rec)
			}
		case "response":
			response = true
			if rec.StatusCode != http.StatusAccepted || rec.JSONRPCID != req.ID {
				t.Errorf("response POST record = %+v", rec)
			}
		}
		raw, _ := json.Marshal(rec)
		if strings.Contains(string(raw), "secret") {
			t.Errorf("an access-log record carries request content: %s", raw)
		}
	}
	if !relayed || !refused || !response {
		t.Fatalf("records: relayed=%v refused=%v response=%v, want all three", relayed, refused, response)
	}
}

func TestARequestRelaysAcrossReplicasAndItsAnswerComesBack(t *testing.T) {
	addr := os.Getenv("GATEWAY_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("GATEWAY_TEST_REDIS_ADDR not set; skipping the cross-replica relay test")
	}
	// Replica A initializes the session and runs the call (so it owns
	// the pending request); the agent's stream and its answer land on B.
	a := newFixture(t, fixtureOptions{ConnectorMetadata: allowAll(), RedisAddr: addr})
	b := *a
	b.plane, b.server = a.newReplica(t)

	sessionID := a.initializeWith(t, allCapabilities)
	frames := b.openStream(t, sessionID)

	// The bridge subscribes in the background: retry until B's stream
	// is reachable from A.
	var (
		done <-chan callOutcome
		req  mcp.Request
	)
	deadline := time.Now().Add(10 * time.Second)
	for id := 10; ; id++ {
		done = a.callAsync(sessionID, askCall(id, `{"method":"elicitation/create","params":{"message":"Which?"}}`))
		if msg, ok := nextNotification(t, frames, 3*time.Second); ok {
			req = msg
			break
		}
		if answers := askAnswers(t, waitCall(t, done)); len(answers) != 1 || answers[0].Error == nil ||
			answers[0].Error.Message != "client has no open stream" || time.Now().After(deadline) {
			t.Fatalf("the request never crossed to replica B: %+v", answers)
		}
	}
	if req.Method != "elicitation/create" {
		t.Fatalf("B's stream got %+v", req)
	}

	// Answered on B; settled on A.
	b.answer(t, sessionID, req.ID.(string), `{"action":"accept","content":{"project":"apollo"}}`)
	answers := askAnswers(t, waitCall(t, done))
	if len(answers) != 1 || !sameJSON(t, answers[0].Result, json.RawMessage(`{"action":"accept","content":{"project":"apollo"}}`)) {
		t.Fatalf("connector got %+v, want the answer relayed back from B", answers)
	}
}
