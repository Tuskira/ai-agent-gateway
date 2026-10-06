package turn

import (
	"context"
	"slices"
	"strings"
)

// PreparedTurn is one stage of a call as it crosses from the customer's side (the
// detection agent) to the engine. Everything that touches raw content ran
// already on the customer's host: the new turn was extracted, secrets were
// found and redacted in every string, and only then was each clipped. Only this redacted state, the ids that
// point decisions at their events, and where each secret was found travel.
//
// PrepareRequest / PrepareResponse build it (the agent's half) and
// the engine decides it (the engine's half); deciding a prepared turn is
// exactly the same as doing both steps at once, so the split cannot
// change a decision.
type PreparedTurn struct {
	Stage Stage    `json:"stage"`
	State State    `json:"state"`
	Refs  TurnRefs `json:"refs"`
	// Secrets is where each kind of secret was first found, before redaction.
	Secrets []SecretHit `json:"secrets,omitempty"`
	// Unreadable marks a request body the extractor could not read. No text
	// of it travels (what a body the extractor cannot read holds, and how
	// a secret is written in it, is unknown): State is empty and Secrets
	// holds the kinds of secret its raw bytes and its strings show, each
	// at user_text.
	Unreadable bool `json:"unreadable,omitempty"`
	// NotJudged is set, with no state, when the agent could not hand the
	// call over (its judging cap was reached); it is the error's detail.
	NotJudged string `json:"not_judged,omitempty"`

	// normalized marks a turn Normalized returned, so normalizing it again
	// costs nothing. Never on the wire: a received turn is never trusted.
	normalized bool
}

// TurnRefs carries the State fields that are never sent to the judge model (json:"-")
// but that evidence needs, by index.
type TurnRefs struct {
	ToolResults       []ToolResultRef `json:"tool_results,omitempty"`
	PriorToolCalls    []string        `json:"prior_tool_calls,omitempty"`
	ResponseToolCalls []string        `json:"response_tool_calls,omitempty"`
}

// ToolResultRef is a tool result's call id and the input that produced it.
type ToolResultRef struct {
	CallID    string `json:"call_id,omitempty"`
	CallInput string `json:"call_input,omitempty"`
}

// SecretHit is where a kind of secret was found: field is user_text,
// tool_results, harness_text, response_text or response_tool_calls, index
// its event.
type SecretHit struct {
	Kind  string `json:"kind"`
	Field string `json:"field"`
	Index int    `json:"index"`
}

// PrepareRequest is the customer-side half of judging a request body.
func PrepareRequest(body []byte) PreparedTurn {
	return PrepareRequestContext(context.Background(), body)
}

// PrepareRequestContext is PrepareRequest bounded by ctx: once ctx is done
// it stops and returns a NotJudgedTurn (DeadlineReason), never a turn left
// half redacted. Extraction and every scan are bounded by the body's size
// and the caps, so only a hostile or huge body runs into a deadline.
func PrepareRequestContext(ctx context.Context, body []byte) PreparedTurn {
	s, ok := extractRequest(body)
	if !ok {
		t, err := unreadableTurn(ctx, body)
		if err != nil {
			return NotJudgedTurn(StageRequest, DeadlineReason)
		}
		return t
	}
	hits, err := s.scrub(ctx, StageRequest, bodyTexts(body), scanCacheOn.Load())
	if err != nil {
		return NotJudgedTurn(StageRequest, DeadlineReason)
	}
	return newTurn(StageRequest, &s, hits)
}

// DeadlineReason is the not-judged detail of a turn whose preparation ran
// past its context's deadline.
const DeadlineReason = "not judged: preparing the turn on the agent ran past its deadline"

// unreadableTurn is a request body the extractor cannot read: no text,
// only the secrets found in it, so the secret rule still decides. The raw
// bytes are scanned (bounded as a user_text is) and so is each string a
// lenient decoder reads out of them, where an escaped secret reads plain.
func unreadableTurn(ctx context.Context, body []byte) (PreparedTurn, error) {
	t := PreparedTurn{Stage: StageRequest, Unreadable: true}
	seen := map[string]bool{}
	for _, x := range append([]string{string(body)}, bodyTexts(body)...) {
		if err := ctx.Err(); err != nil {
			return PreparedTurn{}, err
		}
		for _, m := range scanWindows(validUTF8(x), capText) {
			if !seen[m.kind] && len(t.Secrets) < maxSecretHits {
				seen[m.kind] = true
				t.Secrets = append(t.Secrets, SecretHit{Kind: m.kind, Field: "user_text"})
			}
		}
	}
	return t, nil
}

// PrepareResponse is the customer-side half of judging a response body
// against the request that produced it (for the user's goal and the tool
// results). It reports false when the reply has nothing to judge. A value
// found anywhere in the request (the user's turn, the history, the system
// prompt) is removed from the reply too: a reply echoes what it was given.
func PrepareResponse(reqBody, respBody []byte) (PreparedTurn, bool) {
	return PrepareResponseContext(context.Background(), reqBody, respBody)
}

// PrepareResponseContext is PrepareResponse bounded by ctx, as
// PrepareRequestContext is.
func PrepareResponseContext(ctx context.Context, reqBody, respBody []byte) (PreparedTurn, bool) {
	s := extractResponse(respBody)
	if s.ResponseText == "" && len(s.ResponseToolCalls) == 0 {
		return PreparedTurn{}, false
	}
	if req, ok := extractRequest(reqBody); ok {
		s.UserGoal, s.ToolResults = req.UserGoal, req.ToolResults
	}
	extra := append(bodyTexts(reqBody), bodyTexts(respBody)...)
	hits, err := s.scrub(ctx, StageResponse, extra, scanCacheOn.Load())
	if err != nil {
		return NotJudgedTurn(StageResponse, DeadlineReason), true
	}
	return newTurn(StageResponse, &s, hits), true
}

// NotJudgedTurn is a stage the agent could not hand over; the engine records
// the turn as an error with reason as the detail.
func NotJudgedTurn(st Stage, reason string) PreparedTurn {
	return PreparedTurn{Stage: st, NotJudged: reason}
}

func newTurn(st Stage, s *State, secrets []SecretHit) PreparedTurn {
	t := PreparedTurn{Stage: st, State: *s}
	for _, r := range s.ToolResults {
		t.Refs.ToolResults = append(t.Refs.ToolResults, ToolResultRef{CallID: r.CallID, CallInput: r.CallInput})
	}
	for _, c := range s.PriorToolCalls {
		t.Refs.PriorToolCalls = append(t.Refs.PriorToolCalls, c.ID)
	}
	for _, c := range s.ResponseToolCalls {
		t.Refs.ResponseToolCalls = append(t.Refs.ResponseToolCalls, c.ID)
	}
	t.Secrets = append(t.Secrets, secrets...)
	return t
}

// Restore returns a copy of the state with the refs put back, and the secret
// hits, as prepare left them.
func (t PreparedTurn) Restore() (*State, []SecretHit) {
	s := t.State
	s.ToolResults = append([]ToolResult(nil), s.ToolResults...)
	s.PriorToolCalls = append([]ToolCall(nil), s.PriorToolCalls...)
	s.ResponseToolCalls = append([]ToolCall(nil), s.ResponseToolCalls...)
	for i, r := range t.Refs.ToolResults {
		if i < len(s.ToolResults) {
			s.ToolResults[i].CallID, s.ToolResults[i].CallInput = r.CallID, r.CallInput
		}
	}
	for i, id := range t.Refs.PriorToolCalls {
		if i < len(s.PriorToolCalls) {
			s.PriorToolCalls[i].ID = id
		}
	}
	for i, id := range t.Refs.ResponseToolCalls {
		if i < len(s.ResponseToolCalls) {
			s.ResponseToolCalls[i].ID = id
		}
	}
	return &s, append([]SecretHit(nil), t.Secrets...)
}

// Bounds of what a turn carries besides its state.
const (
	capNotJudged  = 512 // the not-judged reason
	maxSecretHits = 256 // one hit per gitleaks rule; more than it has rules
	// forgedSlack is how much more than its cap Normalized lets a string
	// keep, at each side of the cut, before it scans: no honest agent sends
	// more than the cap (and Clip's marker), so the rest of a longer string
	// is cut first, keeping the scan's work at a few KB per string.
	forgedSlack = 4 << 10
	// capNotJudgedRaw bounds a received not-judged reason before it is
	// scanned and clipped to capNotJudged.
	capNotJudgedRaw = 4 << 10
)

// Normalized is the turn as an honest agent would have sent it: every cap
// re-applied (each string clipped, each list cut to maxToolItems, refs
// rebuilt from the state, hits that point nowhere dropped, the stage's
// fields only), and the secret scan and redaction run again, the new hits
// merged. The engine judges and records only a normalized turn, so an old,
// buggy or hostile agent can neither send it more than the caps nor make it
// pass a raw secret on to the judge model or the results store.
//
// Its work is bounded whatever was received: every string is first cut
// (head and tail, as Clip cuts) to its cap plus forgedSlack at each side,
// the reason to capNotJudgedRaw, the lists to their item caps and the hits
// to maxSecretHits, and only then scanned; hits are checked by their shape
// and one scan of them all.
//
// It is idempotent and changes nothing in a turn PrepareRequest,
// PrepareResponse or NotJudgedTurn built (scrub's output is a fixed point,
// and no string of it is longer than its cap and Clip's marker).
func (t PreparedTurn) Normalized() PreparedTurn {
	if t.normalized {
		return t
	}
	if t.NotJudged != "" {
		r := validUTF8(Clip(t.NotJudged, capNotJudgedRaw))
		return PreparedTurn{Stage: t.Stage, NotJudged: clipRedacted(r, capNotJudged, byLength(scanSecrets(r))), normalized: true}
	}
	t.Secrets = t.Secrets[:min(len(t.Secrets), maxSecretHits)]
	t.Refs.ToolResults = t.Refs.ToolResults[:min(len(t.Refs.ToolResults), maxToolItems)]
	t.Refs.PriorToolCalls = t.Refs.PriorToolCalls[:min(len(t.Refs.PriorToolCalls), maxToolItems)]
	t.Refs.ResponseToolCalls = t.Refs.ResponseToolCalls[:min(len(t.Refs.ResponseToolCalls), maxToolItems)]
	t.State.ToolResults = t.State.ToolResults[:min(len(t.State.ToolResults), maxToolItems)]
	t.State.PriorToolCalls = t.State.PriorToolCalls[:min(len(t.State.PriorToolCalls), maxToolItems)]
	t.State.ResponseToolCalls = t.State.ResponseToolCalls[:min(len(t.State.ResponseToolCalls), maxToolItems)]
	s, hits := t.Restore()
	unreadable := t.Unreadable && t.Stage != StageResponse
	switch {
	case unreadable:
		*s = State{}
	case t.Stage == StageResponse: // the reply, judged against the request's goal and results
		s.UserText, s.HarnessText, s.PriorToolCalls = "", "", nil
	default:
		s.ResponseText, s.ResponseToolCalls = "", nil
	}
	for _, r := range s.texts(t.Stage) {
		*r.p = Clip(*r.p, r.n+2*min(r.n, forgedSlack))
	}
	found, _ := s.scrub(context.Background(), t.Stage, nil, false) // no deadline: never an error; never the cache
	out := newTurn(t.Stage, s, nil)
	out.Unreadable, out.normalized = unreadable, true
	seen := map[string]bool{}
	for _, h := range append(s.validHits(hits, unreadable), found...) {
		if !seen[h.Kind] && len(out.Secrets) < maxSecretHits {
			seen[h.Kind] = true
			out.Secrets = append(out.Secrets, h)
		}
	}
	return out
}

// validHits drops the hits that point at nothing in s (an empty field, an
// index out of range) or that are not shaped like a rule id and a field
// name (lower-case words joined by "-", "_" or "."). An unreadable turn
// has no text: its hits are at user_text. Then one scan over them all
// drops any that carries a secret.
func (s *State) validHits(hits []SecretHit, unreadable bool) []SecretHit {
	var out []SecretHit
	for _, h := range hits {
		ok := h.Index == 0
		switch {
		case unreadable:
			ok = ok && h.Field == "user_text"
		case h.Field == "user_text":
			ok = ok && s.UserText != ""
		case h.Field == "harness_text":
			ok = ok && s.HarnessText != ""
		case h.Field == "response_text":
			ok = ok && s.ResponseText != ""
		case h.Field == "tool_results":
			ok = h.Index >= 0 && h.Index < len(s.ToolResults)
		case h.Field == "response_tool_calls":
			ok = h.Index >= 0 && h.Index < len(s.ResponseToolCalls)
		}
		// A field this version does not know (a newer agent's) is kept at
		// index 0, so its secret still fires the rule.
		if ok && isName(h.Kind) && isName(h.Field) {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return nil
	}
	var b strings.Builder
	for _, h := range out {
		b.WriteString(h.Kind + " " + h.Field + "\n")
	}
	ms := scanSecrets(b.String())
	if len(ms) == 0 {
		return out
	}
	return slices.DeleteFunc(out, func(h SecretHit) bool {
		return slices.ContainsFunc(ms, func(m secretMatch) bool {
			return strings.Contains(h.Kind, m.secret) || strings.Contains(h.Field, m.secret)
		})
	})
}

// isName reports whether s is shaped like a rule id or a field name:
// lower-case letters and digits, words joined by "-", "_" or ".", at most
// capName bytes.
func isName(s string) bool {
	if s == "" || len(s) > capName {
		return false
	}
	for i := range len(s) {
		c := s[i]
		word := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		join := (c == '-' || c == '_' || c == '.') && i > 0 && i < len(s)-1
		if !word && !join {
			return false
		}
	}
	return true
}
