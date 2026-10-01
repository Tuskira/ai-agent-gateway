#!/usr/bin/env bash
#
# 12-skills-and-commands/check.sh: the MCP-protocol assertions only, no
# setup -- run.sh's twin for CI or a quick re-check against a stack
# run.sh has already provisioned (health up, GATEWAY_ADMIN_KEY minted,
# the "code-review" skill, "fix-lint" command and "skills-demo" profile
# registered and attached). It mints its own fresh agent key (an API
# key's plaintext is only ever returned once, so there's nothing to
# reuse) and then repeats exactly the MCP round trip run.sh's step 7
# performs: initialize, tools/list, tools/call gateway__skill,
# prompts/list, prompts/get, skills/list, resources/read, and the
# no-header negative.
#
# Run `./run.sh` at least once first -- this script does not start the
# compose stack, bootstrap an admin key, or register anything; it FAILs
# fast with a clear message if the profile isn't there yet.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CONTROL_BASE="http://localhost:8081/api/v1"
MCP_URL="http://localhost:8080/mcp"

PROFILE_NAME="skills-demo"
PROFILE_INSTRUCTIONS="This profile is for code review and lint-fixing tasks. Load the code-review skill before approving a diff."

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "FAIL: '$1' is required but was not found in PATH. $2" >&2
    exit 1
  fi
}

echo "check: required tools (curl, jq, shasum)"
require_cmd curl "Install curl."
require_cmd jq "Install jq (e.g. 'brew install jq' or 'apt-get install jq')."
require_cmd shasum "Install shasum (ships with macOS and most Linux perl installs)."

echo "check: control plane health ($CONTROL_BASE/health)"
if ! curl -sSf "$CONTROL_BASE/health" >/dev/null 2>&1; then
  echo "FAIL: control plane is not reachable at $CONTROL_BASE. Run './run.sh' first." >&2
  exit 1
fi

if [[ -z "${GATEWAY_ADMIN_KEY:-}" ]]; then
  echo "FAIL: GATEWAY_ADMIN_KEY is not set. Run './run.sh' first (it prints how to export it), or:" >&2
  echo "  export GATEWAY_ADMIN_KEY=\$(docker compose -f $SCRIPT_DIR/../../deploy/docker-compose.yml exec -T gateway /gateway bootstrap-key --force 2>/dev/null)" >&2
  exit 1
fi

echo "check: looking up the '$PROFILE_NAME' profile"
PROFILE_ID="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/profiles?limit=500" | jq -r --arg n "$PROFILE_NAME" '.items[] | select(.name==$n) | .id' | head -n1)"
if [[ -z "$PROFILE_ID" ]]; then
  echo "FAIL: profile '$PROFILE_NAME' does not exist. Run './run.sh' first." >&2
  exit 1
fi

echo "check: profile '$PROFILE_NAME' has code-review and fix-lint attached"
attached_names="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/profiles/$PROFILE_ID/skills" | jq -r '[.items[].name] | sort | join(",")')"
if [[ "$attached_names" != "code-review,fix-lint" ]]; then
  echo "FAIL: expected 'code-review,fix-lint' attached to '$PROFILE_NAME', got '$attached_names'. Run './run.sh' first." >&2
  exit 1
fi

echo "check: minting a fresh agent-role API key ('12-skills-and-commands-check')"
agent_key_resp="$(curl -sSf -X POST "$CONTROL_BASE/api-keys" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "12-skills-and-commands-check", "role": "agent"}')"
AGENT_KEY="$(jq -r '.key' <<<"$agent_key_resp")"
if [[ -z "$AGENT_KEY" || "$AGENT_KEY" == "null" ]]; then
  echo "FAIL: api-key creation did not return a plaintext key" >&2
  exit 1
fi
echo "  -> minted $(jq -r '.prefix' <<<"$agent_key_resp")..."

HEADERS_TMP="$(mktemp)"
SESSION_ID=""
MCP_RESP=""

cleanup() { rm -f "$HEADERS_TMP"; }
trap cleanup EXIT

# mcp_call is run.sh's own helper -- see that script for the full doc
# comment on why it fails loudly on a non-200 or a JSON-RPC error.
mcp_call() {
  local label="$1" body="$2" profile_name="${3:-}"
  local extra_headers=()
  if [[ -n "$SESSION_ID" ]]; then
    extra_headers+=(-H "Mcp-Session-Id: $SESSION_ID")
  fi
  if [[ -n "$profile_name" ]]; then
    extra_headers+=(-H "X-Agent-Profile-Name: $profile_name")
  fi

  local full http_code resp
  full="$(curl -sS -D "$HEADERS_TMP" \
    -H "Authorization: Bearer $AGENT_KEY" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    ${extra_headers[@]+"${extra_headers[@]}"} \
    -d "$body" \
    -w '\n%{http_code}' \
    "$MCP_URL")"
  http_code="${full##*$'\n'}"
  resp="${full%$'\n'*}"

  if [[ "$http_code" != "200" ]]; then
    echo "FAIL: $label got HTTP $http_code: $resp" >&2
    exit 1
  fi
  if jq -e '.error' >/dev/null 2>&1 <<<"$resp"; then
    echo "FAIL: $label returned a JSON-RPC error: $(jq -c '.error' <<<"$resp")" >&2
    exit 1
  fi

  local new_session
  new_session="$(grep -i '^Mcp-Session-Id:' "$HEADERS_TMP" | tail -n1 | sed -E 's/^[^:]+:[[:space:]]*//' | tr -d '\r\n' || true)"
  if [[ -n "$new_session" ]]; then
    SESSION_ID="$new_session"
  fi

  MCP_RESP="$resp"
}

mcp_notify() {
  local label="$1" body="$2"
  local extra_headers=()
  if [[ -n "$SESSION_ID" ]]; then
    extra_headers+=(-H "Mcp-Session-Id: $SESSION_ID")
  fi

  local http_code
  http_code="$(curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $AGENT_KEY" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    ${extra_headers[@]+"${extra_headers[@]}"} \
    -d "$body" \
    "$MCP_URL")"
  if [[ "$http_code" != "204" ]]; then
    echo "FAIL: $label got HTTP $http_code, expected 204" >&2
    exit 1
  fi
}

echo "check: MCP initialize with X-Agent-Profile-Name: $PROFILE_NAME"
mcp_call "initialize" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"12-skills-and-commands-check","version":"0.1.0"}}}' "$PROFILE_NAME"
if [[ -z "$SESSION_ID" ]]; then
  echo "FAIL: initialize did not return an Mcp-Session-Id header" >&2
  exit 1
fi
instructions="$(jq -r '.result.instructions' <<<"$MCP_RESP")"
if [[ "$instructions" != *"$PROFILE_INSTRUCTIONS"* ]]; then
  echo "FAIL: initialize instructions did not carry the profile's own text" >&2
  exit 1
fi
if [[ "$instructions" != *"Skills available to this profile"* || "$instructions" != *"code-review: How to review"* ]]; then
  echo "FAIL: initialize instructions did not carry the skill index for 'code-review'" >&2
  exit 1
fi
if [[ "$(jq -r '.result.capabilities.prompts // empty' <<<"$MCP_RESP")" == "" ]]; then
  echo "FAIL: capabilities.prompts was not advertised, even though the profile has a command attached" >&2
  exit 1
fi

echo "check: MCP notifications/initialized"
mcp_notify "notifications/initialized" '{"jsonrpc":"2.0","method":"notifications/initialized"}'

echo "check: MCP tools/list carries the native gateway__skill tool"
mcp_call "tools/list" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' "$PROFILE_NAME"
if [[ "$(jq -r '[.result.tools[].name] | join(",")' <<<"$MCP_RESP")" != *"gateway__skill"* ]]; then
  echo "FAIL: tools/list did not include gateway__skill" >&2
  exit 1
fi

echo "check: MCP tools/call gateway__skill (name: code-review)"
mcp_call "tools/call gateway__skill" '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gateway__skill","arguments":{"name":"code-review"}}}' "$PROFILE_NAME"
skill_text="$(jq -r '.result.content[0].text' <<<"$MCP_RESP")"
if [[ "$skill_text" != *"# Code review"* ]]; then
  echo "FAIL: gateway__skill did not return code-review's SKILL.md content. Got: $skill_text" >&2
  exit 1
fi

echo "check: MCP prompts/list carries the native 'fix-lint' command"
mcp_call "prompts/list" '{"jsonrpc":"2.0","id":4,"method":"prompts/list"}' "$PROFILE_NAME"
fix_lint_prompt="$(jq -c '.result.prompts[] | select(.name=="fix-lint")' <<<"$MCP_RESP")"
if [[ -z "$fix_lint_prompt" ]]; then
  echo "FAIL: prompts/list did not include 'fix-lint'" >&2
  exit 1
fi
if [[ "$(jq -r '.arguments[0].name' <<<"$fix_lint_prompt")" != "path" || "$(jq -r '.arguments[0].required' <<<"$fix_lint_prompt")" != "true" ]]; then
  echo "FAIL: fix-lint's prompt entry did not declare a required 'path' argument: $fix_lint_prompt" >&2
  exit 1
fi

echo "check: MCP prompts/get fix-lint (argument path)"
mcp_call "prompts/get fix-lint" '{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":{"name":"fix-lint","arguments":{"path":"internal/foo.go"}}}' "$PROFILE_NAME"
rendered="$(jq -r '.result.messages[0].content.text' <<<"$MCP_RESP")"
if [[ "$rendered" != *"internal/foo.go"* ]]; then
  echo "FAIL: prompts/get did not substitute {{path}} with the sent argument. Got: $rendered" >&2
  exit 1
fi

echo "check: MCP skills/list (the Skills Extension, SEP-2640)"
mcp_call "skills/list" '{"jsonrpc":"2.0","id":6,"method":"skills/list"}' "$PROFILE_NAME"
code_review_entry="$(jq -c '.result.skills[] | select(.uri=="skill://code-review/SKILL.md")' <<<"$MCP_RESP")"
if [[ -z "$code_review_entry" ]]; then
  echo "FAIL: skills/list did not include skill://code-review/SKILL.md" >&2
  exit 1
fi
SKILL_MD_DIGEST="$(jq -r '.resources[] | select(.uri=="skill://code-review/SKILL.md") | .digest' <<<"$code_review_entry")"
if [[ "$SKILL_MD_DIGEST" != sha256:* ]]; then
  echo "FAIL: skills/list did not carry a sha256 digest for SKILL.md" >&2
  exit 1
fi

echo "check: MCP resources/read skill://code-review/SKILL.md, digest verified"
mcp_call "resources/read skill://code-review/SKILL.md" '{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"skill://code-review/SKILL.md"}}' "$PROFILE_NAME"
read_text="$(jq -r '.result.contents[0].text' <<<"$MCP_RESP")"
if [[ "$(jq -r '.result.contents[0].mimeType' <<<"$MCP_RESP")" != "text/markdown" ]]; then
  echo "FAIL: resources/read mimeType was not text/markdown" >&2
  exit 1
fi
computed_digest="sha256:$(printf '%s' "$read_text" | shasum -a 256 | awk '{print $1}')"
if [[ "$computed_digest" != "$SKILL_MD_DIGEST" ]]; then
  echo "FAIL: resources/read content hashes to $computed_digest, but skills/list's manifest said $SKILL_MD_DIGEST" >&2
  exit 1
fi

echo "check: MCP tools/list with NO X-Agent-Profile-Name header (expect no gateway__skill)"
mcp_call "tools/list (no profile header)" '{"jsonrpc":"2.0","id":8,"method":"tools/list"}'
if [[ "$(jq -r '[.result.tools[].name] | join(",")' <<<"$MCP_RESP")" == *"gateway__skill"* ]]; then
  echo "FAIL: gateway__skill was listed with no X-Agent-Profile-Name header" >&2
  exit 1
fi

echo
echo "================================================================"
echo "12-skills-and-commands check.sh passed."
echo "================================================================"
