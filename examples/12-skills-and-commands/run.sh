#!/usr/bin/env bash
#
# 12-skills-and-commands: a skill and a command, registered in the skills
# & commands registry, attached to one agent profile, and driven three
# ways -- the gateway-native MCP path, the raw registry API, and the MCP
# Skills Extension (SEP-2640) -- against a real gateway. No mocks:
#
#   - POST /api/v1/skills registers "code-review" (kind "skill", two
#     files: SKILL.md and references/checklist.md) and "fix-lint" (kind
#     "command", one required argument "path", a {{path}} template)
#   - PUT /api/v1/profiles/{id} sets free-text instructions; PUT
#     .../skills attaches both to the "skills-demo" profile
#   - MCP initialize's instructions carry the profile's text plus a
#     capped skill index, and advertise the prompts capability (the
#     profile has a command attached)
#   - tools/list carries the native gateway__skill tool; tools/call on it
#     loads code-review's SKILL.md content
#   - prompts/list carries fix-lint (plain name, no "__") with its "path"
#     argument; prompts/get renders {{path}} into the template
#   - skills/list and resources/read (the MCP Skills Extension) describe
#     code-review with a verifiable sha256 digest, checked against the
#     file's actual content
#   - a negative: with no X-Agent-Profile-Name header, gateway__skill is
#     not listed at all -- "no profile header -> no skills" is
#     unconditional, unlike tools' mcp.require_profile-gated fallback
#     (see docs/profiles.md)
#
# Self-contained: does not assume any other example has been run first,
# and registers its own skill, command and profile rather than reusing
# anything.
#
# Safe to re-run: the skill, command and profile are looked up by name
# first and reused if already there (the same pattern 05-profiles uses
# for its two profiles) -- this also keeps "skills-demo" sitting on the
# stack afterwards for the README's Claude Code walkthrough to point at.
# The one exception is the agent API key, minted fresh on every run,
# exactly like 05-profiles' step 3: its plaintext is only ever returned
# once, so there is nothing to "reuse".
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.yml"

CONTROL_BASE="http://localhost:8081/api/v1"
MCP_URL="http://localhost:8080/mcp"
CONSOLE_URL="http://localhost:8081"

PROFILE_NAME="skills-demo"

GATEWAY_COMPOSE=(docker compose -f "$COMPOSE_FILE")
OBSERVABILITY_COMPOSE_FILE="$ROOT_DIR/examples/08-observability/docker-compose.observability.yml"

# compose_up_preserving_analytics is "${GATEWAY_COMPOSE[@]}" up -d's safe
# replacement: a plain "docker compose -f $COMPOSE_FILE up -d" against a
# gateway container that's already running under examples/08-observability's
# analytics profile (ClickHouse sink + OTel exporter, via
# docker-compose.observability.yml + --profile analytics +
# GATEWAY_SINKS_CLICKHOUSE_ENABLED=true) gets recreated by Compose (it
# detects the merged config differs) using THIS file's env only -- silently
# dropping those sinks back to the ClickHouse-off, OTel-off baseline. A real
# e2e run caught exactly this. A running "clickhouse" container (only ever
# started via --profile analytics; nothing else in examples-smoke starts it)
# is a reliable signal that the analytics overlay is what to preserve, not
# the plain base file -- same signal, same fix, as
# examples/09-roles-keys-and-rate-limits/run.sh's restore_default_compose.
compose_up_preserving_analytics() {
  if [[ -n "$(docker compose -f "$COMPOSE_FILE" --profile analytics ps -q --status running clickhouse 2>/dev/null)" ]]; then
    echo "  -> clickhouse is running (examples/08-observability's analytics profile); bringing the stack up through that overlay too"
    GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose -f "$COMPOSE_FILE" -f "$OBSERVABILITY_COMPOSE_FILE" --profile analytics up -d
  else
    "${GATEWAY_COMPOSE[@]}" up -d
  fi
}

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "FAIL: '$1' is required but was not found in PATH. $2" >&2
    exit 1
  fi
}

echo "check: required tools (docker, curl, jq, shasum)"
require_cmd docker "Install Docker Desktop or Docker Engine."
require_cmd curl "Install curl."
require_cmd jq "Install jq (e.g. 'brew install jq' or 'apt-get install jq')."
require_cmd shasum "Install shasum (ships with macOS and most Linux perl installs)."
if ! docker compose version >/dev/null 2>&1; then
  echo "FAIL: 'docker compose' (the v2 plugin) is required." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 1. compose up
# ---------------------------------------------------------------------------

echo "check: starting the compose stack (deploy/docker-compose.yml)"
compose_up_preserving_analytics

# ---------------------------------------------------------------------------
# 2. health on all three planes
# ---------------------------------------------------------------------------

wait_for_health() {
  local name="$1" url="$2"
  echo "check: $name health ($url)"
  local body=""
  for _ in $(seq 1 60); do
    if body="$(curl -sSf "$url" 2>/dev/null)"; then
      echo "  -> $body"
      return 0
    fi
    sleep 1
  done
  echo "FAIL: $name never became healthy at $url" >&2
  exit 1
}

wait_for_health "MCP plane" "http://localhost:8080/health"
wait_for_health "control plane" "http://localhost:8081/api/v1/health"
wait_for_health "LLM plane" "http://localhost:8082/health"

# ---------------------------------------------------------------------------
# 3. bootstrap an admin API key
# ---------------------------------------------------------------------------

if [[ -n "${GATEWAY_ADMIN_KEY:-}" ]]; then
  echo "check: reusing GATEWAY_ADMIN_KEY from the environment"
else
  echo "check: bootstrapping an admin API key (tenant 'default')"
  # bootstrap-key prints only the plaintext key on stdout; it refuses to
  # mint a second admin key for a tenant that already has one unless
  # --force is passed (cmd/gateway/main.go, runBootstrapKey). A re-run of
  # this script (or of another example against the same stack) hits that
  # case, and the earlier key's plaintext is gone, so mint another one.
  if out="$("${GATEWAY_COMPOSE[@]}" exec -T gateway /gateway bootstrap-key 2>/dev/null)" && [[ -n "$out" ]]; then
    echo "  -> bootstrapped a new admin key"
  else
    echo "  -> tenant 'default' already has an admin key; minting another with --force"
    out="$("${GATEWAY_COMPOSE[@]}" exec -T gateway /gateway bootstrap-key --force 2>/dev/null)" || true
    if [[ -z "$out" ]]; then
      echo "FAIL: could not bootstrap an admin API key. Check '${GATEWAY_COMPOSE[*]} logs gateway'." >&2
      exit 1
    fi
  fi
  GATEWAY_ADMIN_KEY="$out"
fi
export GATEWAY_ADMIN_KEY

# ---------------------------------------------------------------------------
# 4. register (or reuse) the "code-review" skill and "fix-lint" command
# ---------------------------------------------------------------------------
#
# ensure_skill looks up a skill/command by (name, kind) among the
# tenant's OWN rows (never a platform default -- this example always
# registers its own) and creates it from $3 if missing, leaving the
# result in $SKILL_ID. Unlike a profile's PUT .../tools (a full replace,
# so it's safe to call every run), there is no "PUT the whole skill"
# route -- a new version is a separate, additive call -- so a re-run
# simply reuses whatever this example created the first time rather than
# re-uploading it.
ensure_skill() {
  local name="$1" kind="$2" body="$3"
  SKILL_ID="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
    "$CONTROL_BASE/skills?kind=$kind&limit=500" \
    | jq -r --arg n "$name" '.items[] | select(.name==$n and .scope=="tenant") | .id' | head -n1)"

  if [[ -n "$SKILL_ID" ]]; then
    echo "  -> reusing $kind '$name' ($SKILL_ID)"
  else
    local resp
    resp="$(curl -sSf -X POST "$CONTROL_BASE/skills" \
      -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
      -d "$body")"
    SKILL_ID="$(jq -r '.id' <<<"$resp")"
    if [[ -z "$SKILL_ID" || "$SKILL_ID" == "null" ]]; then
      echo "FAIL: creating $kind '$name' did not return an id: $resp" >&2
      exit 1
    fi
    echo "  -> created $kind '$name' ($SKILL_ID)"
  fi
}

# NOTE: none of the three heredocs below may contain an apostrophe/single
# quote. macOS's /bin/bash (3.2.57) mis-parses a `'` inside a <<'EOF'
# heredoc body when that heredoc sits inside a $(...) command
# substitution ("unexpected EOF while looking for matching `''" or a
# bogus "command substitution: line N: syntax error"), even though the
# quoted delimiter should make the body fully literal. bash -n does not
# catch it either -- it only shows up at runtime. Verified against
# /bin/bash 3.2.57 on Darwin; write around contractions/possessives here
# instead of reaching for a workaround.
CODE_REVIEW_SKILL_MD="$(cat <<'EOF'
---
name: code-review
description: How to review a pull request for code style and safety in this repository.
---

# Code review

Read references/checklist.md before approving a diff. In short: no
secrets in the diff, no swallowed errors, every new code path has a real
(non-mocked) test, and docs/CHANGELOG.md is updated alongside the code.

See references/checklist.md for the full checklist.
EOF
)"

CODE_REVIEW_CHECKLIST="$(cat <<'EOF'
# Full review checklist

- [ ] Secrets: no API keys, private keys, or tokens anywhere in the diff.
- [ ] Errors: every error is logged or returned, never silently dropped.
- [ ] Tests: new code paths are covered by a real, non-mocked test.
- [ ] Docs: CHANGELOG.md and the relevant docs/*.md are updated.
- [ ] Migrations: a new column or table has a matching .down.sql.
EOF
)"

echo "check: ensuring the 'code-review' skill exists"
code_review_body="$(jq -n --arg md "$CODE_REVIEW_SKILL_MD" --arg checklist "$CODE_REVIEW_CHECKLIST" '{
  name: "code-review",
  kind: "skill",
  files: [
    {path: "SKILL.md", content: $md},
    {path: "references/checklist.md", content: $checklist}
  ]
}')"
ensure_skill "code-review" "skill" "$code_review_body"
CODE_REVIEW_ID="$SKILL_ID"

FIX_LINT_SKILL_MD="$(cat <<'EOF'
---
name: fix-lint
description: Fix every lint error in one file, given its path.
---

Fix every lint error in `{{path}}`. Run the linter on `{{path}}` first,
then edit the file until it passes, and show the diff you made.
EOF
)"

echo "check: ensuring the 'fix-lint' command exists"
fix_lint_body="$(jq -n --arg md "$FIX_LINT_SKILL_MD" '{
  name: "fix-lint",
  kind: "command",
  arguments: [{name: "path", description: "The file to fix.", required: true}],
  files: [{path: "SKILL.md", content: $md}]
}')"
ensure_skill "fix-lint" "command" "$fix_lint_body"
FIX_LINT_ID="$SKILL_ID"

# ---------------------------------------------------------------------------
# 5. create (or reuse) the "skills-demo" profile, set instructions, attach
# ---------------------------------------------------------------------------
#
# ensure_profile is 05-profiles' own helper: look up by exact name first
# (POST /profiles is not itself idempotent -- only the derived slug, not
# the name, is unique), create if missing.
ensure_profile() {
  local name="$1"
  PROFILE_ID="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
    "$CONTROL_BASE/profiles?limit=500" | jq -r --arg n "$name" '.items[] | select(.name==$n) | .id' | head -n1)"

  if [[ -n "$PROFILE_ID" ]]; then
    echo "  -> reusing profile '$name' ($PROFILE_ID)"
  else
    PROFILE_ID="$(curl -sSf -X POST "$CONTROL_BASE/profiles" \
      -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
      -H "Content-Type: application/json" \
      -d "$(jq -n --arg n "$name" '{name: $n}')" | jq -r '.id')"
    if [[ -z "$PROFILE_ID" || "$PROFILE_ID" == "null" ]]; then
      echo "FAIL: profile creation for '$name' did not return an id" >&2
      exit 1
    fi
    echo "  -> created profile '$name' ($PROFILE_ID)"
  fi
}

echo "check: ensuring the '$PROFILE_NAME' profile exists"
ensure_profile "$PROFILE_NAME"

PROFILE_INSTRUCTIONS="This profile is for code review and lint-fixing tasks. Load the code-review skill before approving a diff."

echo "check: setting the profile's instructions (PUT /profiles/\$id)"
# PUT /profiles/{id} is a full replace of name/description/instructions
# (docs/profiles.md), so it's called unconditionally, whether or not the
# profile already existed -- the same reasoning 05-profiles' step 2
# applies to PUT .../tools.
curl -sSf -X PUT "$CONTROL_BASE/profiles/$PROFILE_ID" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d "$(jq -n --arg n "$PROFILE_NAME" --arg i "$PROFILE_INSTRUCTIONS" '{name: $n, instructions: $i}')" >/dev/null
echo "  -> instructions set"

echo "check: attaching the skill and command to '$PROFILE_NAME' (PUT /profiles/\$id/skills)"
# PUT .../skills fully replaces the attachment set (replace semantics
# like /tools), so -- like the instructions PUT above -- it's safe and
# correct to call every run.
curl -sSf -X PUT "$CONTROL_BASE/profiles/$PROFILE_ID/skills" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d "$(jq -n --arg s "$CODE_REVIEW_ID" --arg c "$FIX_LINT_ID" '{items: [{skill_id: $s}, {skill_id: $c}]}')" >/dev/null
echo "  -> attached: code-review (skill), fix-lint (command)"

# ---------------------------------------------------------------------------
# 6. mint an agent-role API key
# ---------------------------------------------------------------------------
#
# Not idempotent by design, like 05-profiles' step 3: an API key's
# plaintext is only ever returned once, so a re-run mints a fresh
# "12-skills-and-commands-agent" key rather than trying to reuse one.

echo "check: minting a fresh agent-role API key ('12-skills-and-commands-agent')"
agent_key_resp="$(curl -sSf -X POST "$CONTROL_BASE/api-keys" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "12-skills-and-commands-agent", "role": "agent"}')"
AGENT_KEY="$(jq -r '.key' <<<"$agent_key_resp")"
if [[ -z "$AGENT_KEY" || "$AGENT_KEY" == "null" ]]; then
  echo "FAIL: api-key creation did not return a plaintext key" >&2
  exit 1
fi
echo "  -> minted $(jq -r '.prefix' <<<"$agent_key_resp")... (role: $(jq -r '.role' <<<"$agent_key_resp"))"

# ---------------------------------------------------------------------------
# 7. MCP handshake, native tools/prompts, and the Skills Extension
# ---------------------------------------------------------------------------

HEADERS_TMP="$(mktemp)"
SESSION_ID=""
MCP_RESP=""

cleanup() { rm -f "$HEADERS_TMP"; }
trap cleanup EXIT

# mcp_call POSTs one JSON-RPC request to $MCP_URL, authenticated as the
# agent key from step 6 and, when $3 is given, scoped by an
# X-Agent-Profile-Name header. The response body is left in MCP_RESP (not
# echoed: a $(...) capture would run this in a subshell and lose the
# SESSION_ID it records). It fails loudly on a non-200 HTTP status or a
# JSON-RPC-level "error" -- per internal/dataplane/transport/http.go,
# every non-auth failure the gateway itself produces is still HTTP 200
# with a JSON-RPC error object. See 05-profiles/run.sh for the same
# helper.
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

# mcp_notify is for a JSON-RPC notification (no "id"): the gateway
# answers with an empty HTTP 204.
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
mcp_call "initialize" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"12-skills-and-commands","version":"0.1.0"}}}' "$PROFILE_NAME"
echo "  -> protocolVersion=$(jq -r '.result.protocolVersion' <<<"$MCP_RESP"), session=$SESSION_ID"
if [[ -z "$SESSION_ID" ]]; then
  echo "FAIL: initialize did not return an Mcp-Session-Id header" >&2
  exit 1
fi

instructions="$(jq -r '.result.instructions' <<<"$MCP_RESP")"
echo "  -> instructions:"
while IFS= read -r line; do
  echo "     $line"
done <<<"$instructions"
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
tool_names="$(jq -r '[.result.tools[].name] | join(",")' <<<"$MCP_RESP")"
echo "  -> tools: $tool_names"
if [[ "$tool_names" != *"gateway__skill"* ]]; then
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
echo "  -> loaded $(echo -n "$skill_text" | wc -c | tr -d ' ') bytes of SKILL.md content"

echo "check: MCP prompts/list carries the native 'fix-lint' command"
mcp_call "prompts/list" '{"jsonrpc":"2.0","id":4,"method":"prompts/list"}' "$PROFILE_NAME"
fix_lint_prompt="$(jq -c '.result.prompts[] | select(.name=="fix-lint")' <<<"$MCP_RESP")"
if [[ -z "$fix_lint_prompt" ]]; then
  echo "FAIL: prompts/list did not include 'fix-lint'. Got: $(jq -c '.result.prompts' <<<"$MCP_RESP")" >&2
  exit 1
fi
if [[ "$(jq -r '.arguments[0].name' <<<"$fix_lint_prompt")" != "path" || "$(jq -r '.arguments[0].required' <<<"$fix_lint_prompt")" != "true" ]]; then
  echo "FAIL: fix-lint's prompt entry did not declare a required 'path' argument: $fix_lint_prompt" >&2
  exit 1
fi
echo "  -> fix-lint: $fix_lint_prompt"

echo "check: MCP prompts/get fix-lint (argument path)"
mcp_call "prompts/get fix-lint" '{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":{"name":"fix-lint","arguments":{"path":"internal/foo.go"}}}' "$PROFILE_NAME"
rendered="$(jq -r '.result.messages[0].content.text' <<<"$MCP_RESP")"
echo "  -> rendered: $rendered"
if [[ "$rendered" != *"internal/foo.go"* ]]; then
  echo "FAIL: prompts/get did not substitute {{path}} with the sent argument. Got: $rendered" >&2
  exit 1
fi

echo "check: MCP skills/list (the Skills Extension, SEP-2640)"
mcp_call "skills/list" '{"jsonrpc":"2.0","id":6,"method":"skills/list"}' "$PROFILE_NAME"
code_review_entry="$(jq -c '.result.skills[] | select(.uri=="skill://code-review/SKILL.md")' <<<"$MCP_RESP")"
if [[ -z "$code_review_entry" ]]; then
  echo "FAIL: skills/list did not include skill://code-review/SKILL.md. Got: $(jq -c '.result.skills' <<<"$MCP_RESP")" >&2
  exit 1
fi
SKILL_MD_DIGEST="$(jq -r '.resources[] | select(.uri=="skill://code-review/SKILL.md") | .digest' <<<"$code_review_entry")"
if [[ "$SKILL_MD_DIGEST" != sha256:* ]]; then
  echo "FAIL: skills/list did not carry a sha256 digest for SKILL.md: $code_review_entry" >&2
  exit 1
fi
echo "  -> code-review: $(jq -r '.frontmatter.description' <<<"$code_review_entry") ($SKILL_MD_DIGEST)"

echo "check: MCP resources/read skill://code-review/SKILL.md, digest verified"
mcp_call "resources/read skill://code-review/SKILL.md" '{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"skill://code-review/SKILL.md"}}' "$PROFILE_NAME"
read_text="$(jq -r '.result.contents[0].text' <<<"$MCP_RESP")"
read_mime="$(jq -r '.result.contents[0].mimeType' <<<"$MCP_RESP")"
if [[ "$read_mime" != "text/markdown" ]]; then
  echo "FAIL: resources/read mimeType = $read_mime, want text/markdown" >&2
  exit 1
fi
computed_digest="sha256:$(printf '%s' "$read_text" | shasum -a 256 | awk '{print $1}')"
if [[ "$computed_digest" != "$SKILL_MD_DIGEST" ]]; then
  echo "FAIL: resources/read content hashes to $computed_digest, but skills/list's manifest said $SKILL_MD_DIGEST" >&2
  exit 1
fi
echo "  -> content verified against its own manifest digest"

echo "check: MCP tools/list with NO X-Agent-Profile-Name header (expect no gateway__skill)"
# "No profile header -> no skills" is unconditional (docs/profiles.md):
# unlike a tool allow-list, there is no mcp.require_profile fallback to
# "every tool in the tenant" for the native skill tool -- it never
# appears without a resolved profile that has ≥1 skill attached.
mcp_call "tools/list (no profile header)" '{"jsonrpc":"2.0","id":8,"method":"tools/list"}'
no_header_tool_names="$(jq -r '[.result.tools[].name] | join(",")' <<<"$MCP_RESP")"
if [[ "$no_header_tool_names" == *"gateway__skill"* ]]; then
  echo "FAIL: gateway__skill was listed with no X-Agent-Profile-Name header" >&2
  exit 1
fi
echo "  -> gateway__skill absent, as expected ($no_header_tool_names)"

echo
echo "================================================================"
echo "12-skills-and-commands passed."
echo "Console:    $CONSOLE_URL"
echo "Profile:    $PROFILE_NAME"
echo "Agent key:  $AGENT_KEY"
echo
echo "A client should send, on every MCP request:"
echo "  Authorization: Bearer $AGENT_KEY"
echo "  X-Agent-Profile-Name: $PROFILE_NAME"
echo "================================================================"
