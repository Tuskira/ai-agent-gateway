#!/usr/bin/env bash
#
# 02-claude-code: static validation only -- no network, no running gateway,
# no Claude Code install required. This is what CI runs for this example
# (unlike 01/04/05, there is no live run.sh here: Claude Code itself isn't
# scriptable end to end in CI). It checks that:
#
#   - .mcp.json is valid JSON and shaped the way Claude Code's HTTP
#     transport expects (type/url/headers)
#   - its header names match what the gateway actually authenticates and
#     scopes on (Authorization, X-Agent-Profile-Name)
#   - claude-mcp-add.sh is syntactically valid bash and sets the same
#     two headers
#
# Exits non-zero on the first failure.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

echo "check: required tools (jq)"
command -v jq >/dev/null 2>&1 || fail "'jq' is required but was not found in PATH."

MCP_JSON="$SCRIPT_DIR/.mcp.json"

echo "check: $MCP_JSON exists and is valid JSON"
[[ -f "$MCP_JSON" ]] || fail "$MCP_JSON not found"
jq empty "$MCP_JSON" 2>/dev/null || fail "$MCP_JSON is not valid JSON"

server_count="$(jq '.mcpServers | length' "$MCP_JSON")"
echo "check: $MCP_JSON has at least one entry under mcpServers"
[[ "$server_count" -ge 1 ]] || fail "$MCP_JSON: .mcpServers is empty"

server_name="$(jq -r '.mcpServers | keys[0]' "$MCP_JSON")"
echo "  -> validating server '$server_name'"

server_type="$(jq -r --arg n "$server_name" '.mcpServers[$n].type // empty' "$MCP_JSON")"
server_url="$(jq -r --arg n "$server_name" '.mcpServers[$n].url // empty' "$MCP_JSON")"

echo "check: type is \"http\""
[[ "$server_type" == "http" ]] || fail "$MCP_JSON: .mcpServers.$server_name.type is '$server_type', expected 'http'"

echo "check: url points at the gateway's MCP plane (:8080/mcp)"
[[ "$server_url" == "http://localhost:8080/mcp" ]] || fail "$MCP_JSON: .mcpServers.$server_name.url is '$server_url', expected http://localhost:8080/mcp"

echo "check: headers carry Authorization and X-Agent-Profile-Name"
has_auth="$(jq -r --arg n "$server_name" '.mcpServers[$n].headers | has("Authorization")' "$MCP_JSON")"
has_profile="$(jq -r --arg n "$server_name" '.mcpServers[$n].headers | has("X-Agent-Profile-Name")' "$MCP_JSON")"
# internal/auth/apikey.ParseBearer accepts "Authorization: Bearer gk_..."
# (or X-Gateway-Key); internal/dataplane/profile.Header is X-Agent-Profile-Name.
[[ "$has_auth" == "true" ]] || fail "$MCP_JSON: headers.Authorization is missing"
[[ "$has_profile" == "true" ]] || fail "$MCP_JSON: headers.X-Agent-Profile-Name is missing"

auth_val="$(jq -r --arg n "$server_name" '.mcpServers[$n].headers.Authorization' "$MCP_JSON")"
echo "check: Authorization header value starts with \"Bearer \""
[[ "$auth_val" == Bearer\ * ]] || fail "$MCP_JSON: headers.Authorization does not start with 'Bearer '"

ADD_SH="$SCRIPT_DIR/claude-mcp-add.sh"
echo "check: $ADD_SH exists and passes bash -n"
[[ -f "$ADD_SH" ]] || fail "$ADD_SH not found"
bash -n "$ADD_SH" || fail "$ADD_SH failed a bash syntax check"

echo "check: $ADD_SH sets the same two headers"
grep -q -- '--header "Authorization: Bearer' "$ADD_SH" || fail "$ADD_SH does not set an Authorization: Bearer header"
grep -q -- '--header "X-Agent-Profile-Name:' "$ADD_SH" || fail "$ADD_SH does not set an X-Agent-Profile-Name header"

echo
echo "================================================================"
echo "02-claude-code check.sh passed (static checks only, no network)."
echo "================================================================"
