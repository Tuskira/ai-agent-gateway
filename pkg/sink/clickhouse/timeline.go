package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
)

// mcpTimelineRow is one mcp_access_logs row read for a session timeline,
// carrying its ClientSessionID alongside the TimelineEvent shape (that
// field is not part of the wire shape -- see analytics.TimelineEvent --
// it only decides which LLM rows also belong to this session).
type mcpTimelineRow struct {
	analytics.TimelineEvent
	clientSessionID string
}

// SessionTimeline implements analytics.Reader. See that interface
// method's doc comment, and docs/observability.md#session-timeline-ownership,
// for the ownership rule this enforces: a caller-supplied session tag
// (ClientSessionID) is never sufficient on its own to pull an LLM call into
// a timeline -- the call's key_id must also match the session's owner.
func (s *Sink) SessionTimeline(ctx context.Context, tenantID, sessionID string, opts analytics.TimelineOptions) (*analytics.SessionTimeline, error) {
	order, _ := analytics.ParseTimelineOrder(string(opts.Order))
	mcpRows, err := s.mcpSessionEvents(ctx, tenantID, sessionID, false)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: session timeline: mcp events: %w", err)
	}
	// Not an MCP session id: it may be a client's own session tag (the
	// X-Session-Id / X-Claude-Code-Session-Id it sends on both planes), in
	// which case its MCP calls carry it as client_session_id. The owner is
	// then the earliest event's key across both planes (as for an LLM-only
	// session), so another key claiming the same tag is still excluded.
	viaTag := false
	if len(mcpRows) == 0 {
		if mcpRows, err = s.mcpSessionEvents(ctx, tenantID, sessionID, true); err != nil {
			return nil, fmt.Errorf("clickhouse: session timeline: tagged mcp events: %w", err)
		}
		viaTag = len(mcpRows) > 0
	}

	// tags is every session_id an LLM call may carry and still belong to
	// this timeline: the session id itself, plus every ClientSessionID
	// (caller-claimed X-Session-Id/X-Claude-Code-Session-Id) actually seen
	// on one of this session's own MCP events. Membership in this set is
	// necessary but not sufficient -- the key_id check below is what
	// actually enforces ownership.
	tags := map[string]struct{}{sessionID: {}}
	ownerKeyID := ""
	if len(mcpRows) > 0 {
		ownerKeyID = mcpRows[0].KeyID // earliest MCP event -- see SessionTimeline's doc comment
	}
	for _, m := range mcpRows {
		if m.clientSessionID != "" {
			tags[m.clientSessionID] = struct{}{}
		}
	}
	tagList := make([]string, 0, len(tags))
	for t := range tags {
		tagList = append(tagList, t)
	}

	llmRows, err := s.llmSessionEvents(ctx, tenantID, tagList)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: session timeline: llm events: %w", err)
	}
	// The reverse of linkedMCPEvents: an MCP session whose client tagged no
	// model calls is joined by the conversation that asked for its tools.
	if len(mcpRows) > 0 && !viaTag && len(llmRows) == 0 {
		if llmRows, err = s.linkedLLMEvents(ctx, tenantID, sessionID, mcpRows); err != nil {
			return nil, fmt.Errorf("clickhouse: session timeline: linked llm events: %w", err)
		}
	}

	// LLM-only session: nothing on the MCP plane ever used this session
	// id, so there is no MCP session to own it -- the owner is whichever
	// key made the earliest LLM call tagged with it directly (not via a
	// ClientSessionID -- tagList's only member is sessionID itself here).
	if len(mcpRows) == 0 && len(llmRows) > 0 {
		analytics.SortTimelineEvents(llmRows, analytics.TimelineOrderAsc)
		ownerKeyID = llmRows[0].KeyID
		// An agent that tags only its model calls (Claude Code's
		// X-Claude-Code-Session-Id never reaches the MCP plane): link the MCP
		// sessions in which the owner's key called a tool the model asked for.
		if mcpRows, err = s.linkedMCPEvents(ctx, tenantID, sessionID, ownerKeyID,
			llmRows[0].Timestamp, llmRows[len(llmRows)-1].Timestamp); err != nil {
			return nil, fmt.Errorf("clickhouse: session timeline: linked mcp events: %w", err)
		}
	}
	if viaTag && len(llmRows) > 0 {
		analytics.SortTimelineEvents(llmRows, analytics.TimelineOrderAsc)
		if llmRows[0].Timestamp.Before(mcpRows[0].Timestamp) {
			ownerKeyID = llmRows[0].KeyID
		}
	}

	events := make([]analytics.TimelineEvent, 0, len(mcpRows)+len(llmRows))
	excluded := 0
	for _, m := range mcpRows {
		if m.KeyID != ownerKeyID {
			excluded++
			continue
		}
		events = append(events, m.TimelineEvent)
	}
	for _, l := range llmRows {
		if l.KeyID != ownerKeyID {
			excluded++
			continue
		}
		events = append(events, l)
	}
	// The per-plane queries and the owner derivation above always run
	// oldest-first ((timestamp, request_id) ASC): the owner and the link
	// windows are defined by the earliest events, so they, and with them
	// every stat, must not depend on the requested order. Only the final
	// merge applies the requested direction, on the same composite key.
	analytics.SortTimelineEvents(events, order)

	return &analytics.SessionTimeline{
		SessionID:             sessionID,
		OwnerKeyID:            ownerKeyID,
		Order:                 order,
		Events:                events,
		TotalEvents:           len(events),
		ExcludedForeignEvents: excluded,
	}, nil
}

// mcpSessionEvents returns every mcp_access_logs row for tenantID whose
// session_id is sessionID (or, with byTag, whose client_session_id is),
// oldest first.
func (s *Sink) mcpSessionEvents(ctx context.Context, tenantID, sessionID string, byTag bool) ([]mcpTimelineRow, error) {
	column := "session_id"
	if byTag {
		column = "client_session_id"
	}
	query := `SELECT timestamp, request_id, method, tool_name, status_code, error_code, duration_ms, key_id, client_session_id
		FROM mcp_access_logs WHERE tenant_id = ? AND ` + column + ` = ? ORDER BY timestamp ASC, request_id ASC`
	rows, err := s.c.Query(ctx, query, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []mcpTimelineRow
	for rows.Next() {
		var (
			ts                     time.Time
			requestID, method      string
			toolName               string
			statusCode             uint16
			errorCode              string
			durationMS             uint32
			keyID, clientSessionID string
		)
		if err := rows.Scan(&ts, &requestID, &method, &toolName, &statusCode, &errorCode, &durationMS, &keyID, &clientSessionID); err != nil {
			return nil, err
		}
		name := ""
		if method == "tools/call" {
			name = toolName
		}
		out = append(out, mcpTimelineRow{
			TimelineEvent: analytics.TimelineEvent{
				Timestamp:  ts,
				Plane:      "mcp",
				Kind:       method,
				Name:       name,
				Status:     mcpTimelineStatus(int(statusCode), errorCode),
				DurationMS: int64(durationMS),
				KeyID:      keyID,
				ID:         requestID,
			},
			clientSessionID: clientSessionID,
		})
	}
	return out, rows.Err()
}

// What links an LLM reply to the MCP calls it asked for. A reply names each
// MCP tool mcp__<server>__<tool> and gives each request an id; Claude Code
// copies that id into the MCP call's _meta, so its calls link exactly. A call
// without one links by tool name within the conversation's window.
const (
	mcpToolNames = `extractAll(response_body, '"name"\\s*:\\s*"(mcp__[^"]+)"')`
	toolUseIDs   = `extractAll(response_body, '"id"\\s*:\\s*"(toolu_[A-Za-z0-9_]+)"')`
	mcpToolUseID = `JSONExtractString(request_body, 'params', '_meta', 'claudecode/toolUseId')`
)

// linkedMCPEvents returns the events of every MCP session in which ownerKeyID
// called, between a minute before from and five minutes after to, a tool one
// of sessionID's LLM replies asked for: by tool-use id when the call carries
// one, else by tool name (the gateway logs mcp__<server>__<tool> as <tool>).
// Needs stored bodies (llm_proxy.capture.store_bodies). Without ids, two
// conversations on one key calling the same tool at once cannot be told apart.
func (s *Sink) linkedMCPEvents(ctx context.Context, tenantID, sessionID, ownerKeyID string, from, to time.Time) ([]mcpTimelineRow, error) {
	var tools, uses []string
	if err := s.c.QueryRow(ctx, `SELECT arrayDistinct(arrayFlatten(groupArray(`+mcpToolNames+`))),
			arrayDistinct(arrayFlatten(groupArray(`+toolUseIDs+`)))
		FROM llm_calls WHERE tenant_id = ? AND session_id = ? AND key_id = ?`,
		tenantID, sessionID, ownerKeyID).Scan(&tools, &uses); err != nil || len(tools) == 0 {
		return nil, err
	}
	var ids []string
	if err := s.c.QueryRow(ctx, `SELECT groupUniqArray(session_id) FROM mcp_access_logs
		WHERE tenant_id = ? AND key_id = ? AND method = 'tools/call' AND session_id != ''
		  AND timestamp BETWEEN ? AND ?
		  AND if(`+mcpToolUseID+` != '', has(?, `+mcpToolUseID+`),
		         arrayExists(t -> endsWith(t, concat('__', tool_name)), ?))`,
		tenantID, ownerKeyID, from.Add(-time.Minute), to.Add(5*time.Minute), uses, tools).Scan(&ids); err != nil {
		return nil, err
	}
	var out []mcpTimelineRow
	for _, id := range ids {
		ev, err := s.mcpSessionEvents(ctx, tenantID, id, false)
		if err != nil {
			return nil, err
		}
		out = append(out, ev...)
	}
	return out, nil
}

// linkedLLMEvents returns the LLM events of every conversation of the MCP
// session's owner whose replies asked for a call the session made, in the
// session's window (the rule linkedMCPEvents applies the other way round).
func (s *Sink) linkedLLMEvents(ctx context.Context, tenantID, sessionID string, mcpRows []mcpTimelineRow) ([]analytics.TimelineEvent, error) {
	var tools, uses []string
	if err := s.c.QueryRow(ctx, `SELECT groupUniqArrayIf(tool_name, tool_name != ''),
			groupUniqArrayIf(`+mcpToolUseID+`, `+mcpToolUseID+` != '')
		FROM mcp_access_logs WHERE tenant_id = ? AND session_id = ? AND method = 'tools/call'`,
		tenantID, sessionID).Scan(&tools, &uses); err != nil || len(tools) == 0 {
		return nil, err
	}
	match, arg := `arrayExists(n -> arrayExists(t -> endsWith(n, concat('__', t)), ?), `+mcpToolNames+`)`, any(tools)
	if len(uses) > 0 {
		match, arg = `hasAny(?, `+toolUseIDs+`)`, uses
	}
	var ids []string
	if err := s.c.QueryRow(ctx, `SELECT groupUniqArray(session_id) FROM llm_calls
		WHERE tenant_id = ? AND key_id = ? AND session_id != '' AND timestamp BETWEEN ? AND ? AND `+match,
		tenantID, mcpRows[0].KeyID, mcpRows[0].Timestamp.Add(-5*time.Minute), mcpRows[len(mcpRows)-1].Timestamp.Add(time.Minute), arg).Scan(&ids); err != nil {
		return nil, err
	}
	return s.llmSessionEvents(ctx, tenantID, ids)
}

// llmSessionEvents returns every llm_calls row for tenantID whose
// session_id is one of sessionTags, oldest first. sessionTags is never
// empty in practice (it always at least holds the session id itself).
func (s *Sink) llmSessionEvents(ctx context.Context, tenantID string, sessionTags []string) ([]analytics.TimelineEvent, error) {
	if len(sessionTags) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sessionTags)), ",")
	query := `SELECT timestamp, request_id, model, status_code, error, duration_ms, key_id
		FROM llm_calls WHERE tenant_id = ? AND session_id IN (` + placeholders + `) ORDER BY timestamp ASC, request_id ASC`
	args := make([]any, 0, len(sessionTags)+1)
	args = append(args, tenantID)
	for _, t := range sessionTags {
		args = append(args, t)
	}
	rows, err := s.c.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []analytics.TimelineEvent
	for rows.Next() {
		var (
			ts               time.Time
			requestID, model string
			statusCode       uint16
			errText          string
			durationMS       uint32
			keyID            string
		)
		if err := rows.Scan(&ts, &requestID, &model, &statusCode, &errText, &durationMS, &keyID); err != nil {
			return nil, err
		}
		out = append(out, analytics.TimelineEvent{
			Timestamp:  ts,
			Plane:      "llm",
			Kind:       "llm_call",
			Name:       model,
			Status:     llmTimelineStatus(int(statusCode), errText),
			DurationMS: int64(durationMS),
			KeyID:      keyID,
			ID:         requestID,
		})
	}
	return out, rows.Err()
}

// mcpTimelineStatus mirrors the Overview outcome-bucket convention
// (pkg/analytics.Outcome*): 204 is a notification (no verdict of its own),
// a non-empty error code or a 4xx/5xx status is an error, else success.
func mcpTimelineStatus(statusCode int, errorCode string) string {
	switch {
	case statusCode == 204:
		return analytics.TimelineStatusNotification
	case errorCode != "" || statusCode >= 400:
		return analytics.TimelineStatusError
	default:
		return analytics.TimelineStatusSuccess
	}
}

// llmTimelineStatus is mcpTimelineStatus's LLM-plane counterpart: no
// notification bucket (every LLM call carries a verdict).
func llmTimelineStatus(statusCode int, errText string) string {
	if errText != "" || statusCode >= 400 {
		return analytics.TimelineStatusError
	}
	return analytics.TimelineStatusSuccess
}
