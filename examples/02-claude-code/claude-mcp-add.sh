#!/usr/bin/env bash
#
# 02-claude-code, Option B: register the gateway as an MCP server via the
# Claude Code CLI instead of hand-editing .mcp.json (Option A). Equivalent
# result, different scope by default -- see the README.
#
# `claude mcp add` with no --scope defaults to *local* scope: private to
# you, stored in ~/.claude.json, NOT the project's checked-in .mcp.json.
# Pass --scope project (or edit .mcp.json directly, per Option A) to share
# the entry with the rest of the repo instead.
set -euo pipefail

: "${GATEWAY_KEY:?set GATEWAY_KEY to your gk_... agent-role API key first -- see the README Prerequisites}"
PROFILE="${PROFILE:-Full Access}"

command -v claude >/dev/null 2>&1 || {
  echo "FAIL: 'claude' (Claude Code) is required but was not found in PATH." >&2
  exit 1
}

claude mcp add --transport http tusk-ai-gateway http://localhost:8080/mcp \
  --header "Authorization: Bearer ${GATEWAY_KEY}" \
  --header "X-Agent-Profile-Name: ${PROFILE}"
