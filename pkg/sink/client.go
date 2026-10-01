package sink

import "strings"

// ClientFamily maps a raw User-Agent header to one of the gateway's named
// client families. It is the single place the substring rule lives, so the
// MCP plane's requestsByClient grouping (pkg/sink/clickhouse/reader.go) and
// the LLM plane's LLMCall.ClientName (internal/llmplane/router.go) classify
// the same caller the same way.
//
// Matching is case-insensitive substring matching, first match wins, in
// this order:
//
//   - "claude-cli" / "claude-code" -> "claude-code". The real Claude Code
//     CLI User-Agent is "claude-cli/<version> (external, cli)", not
//     "claude-code/..."; both substrings are matched so a future rename
//     either way still classifies correctly.
//   - "claude-desktop" / "claude desktop" / "claude/" -> "claude-desktop".
//     Claude Desktop's exact gateway-mode User-Agent isn't published, so
//     this is deliberately conservative: any remaining "Claude/<version>"
//     or "Claude Desktop"-shaped UA that didn't already match claude-code.
//   - "cursor" -> "cursor"
//   - "vscode" / "visual studio code" -> "vscode". VS Code's native MCP
//     client sends the literal "Visual Studio Code" (no "vscode" substring),
//     so both spellings are matched.
//   - "codex" -> "codex"
//   - anything else, non-empty -> "other"
//   - "" (no User-Agent header sent) -> ""
//
// The empty case is deliberately its own bucket, not "other": a call with
// no User-Agent at all ("no client info") is a different fact than a call
// from a real, unrecognized client.
func ClientFamily(userAgent string) string {
	if userAgent == "" {
		return ""
	}
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "claude-cli"), strings.Contains(ua, "claude-code"):
		return "claude-code"
	case strings.Contains(ua, "claude-desktop"), strings.Contains(ua, "claude desktop"), strings.Contains(ua, "claude/"):
		return "claude-desktop"
	case strings.Contains(ua, "cursor"):
		return "cursor"
	case strings.Contains(ua, "vscode"), strings.Contains(ua, "visual studio code"):
		return "vscode"
	case strings.Contains(ua, "codex"):
		return "codex"
	default:
		return "other"
	}
}

// MaxUserAgentLen bounds sink.LLMCall.UserAgent: only the classification
// prefix matters for analytics, so a caller cannot bloat a capture row by
// sending an oversized header.
const MaxUserAgentLen = 256

// CapUserAgent truncates ua to MaxUserAgentLen bytes; ua is returned
// unchanged when it is already within the bound.
func CapUserAgent(ua string) string {
	if len(ua) <= MaxUserAgentLen {
		return ua
	}
	return ua[:MaxUserAgentLen]
}
