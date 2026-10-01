#!/usr/bin/env bash
#
# 05-profiles: one MCP server, two agent profiles, one agent key. Creates
# an "Echo Only" profile (2 tools, on the "everything" connector) and a
# "Full Access" profile (every tool on every connector in the tenant --
# see step 7's comment for why it isn't scoped to just "everything"),
# mints a single agent-role API key, and drives tools/list and tools/call
# under each profile to show:
#
#   - tools/list is filtered per profile
#   - tools/call on a tool the profile doesn't grant fails with JSON-RPC
#     error -32003
#   - a profile name that doesn't exist grants NOTHING (not "everything")
#   - with no X-Agent-Profile-Name header, every tenant tool is served --
#     unless mcp.require_profile is turned on, which then denies the
#     no-header request outright
#
# Self-contained: does not assume 01-quickstart has been run first, but
# reuses whatever it already left running (the "everything" MCP server,
# the compose stack, the "everything" connector).
#
# Safe to re-run: every step checks current state first and reuses it
# instead of failing or duplicating work. The one exception is the agent
# API key, which is minted fresh on every run (see step 3 below) -- its
# plaintext can only be read once, so there is nothing to "reuse".
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.yml"

EVERYTHING_PORT=23014
EVERYTHING_PID_FILE="$SCRIPT_DIR/.everything.pid"
EVERYTHING_LOG_FILE="$SCRIPT_DIR/.everything.log"

CONTROL_BASE="http://localhost:8081/api/v1"
MCP_URL="http://localhost:8080/mcp"
CONSOLE_URL="http://localhost:8081"

GATEWAY_COMPOSE=(docker compose -f "$COMPOSE_FILE")
OBSERVABILITY_COMPOSE_FILE="$ROOT_DIR/examples/08-observability/docker-compose.observability.yml"

# compose_up_preserving_analytics / compose_up_gateway_preserving_analytics
# are "${GATEWAY_COMPOSE[@]}" up -d [gateway]'s safe replacement, used
# everywhere this script brings the gateway container up or recreates it
# (the initial compose-up below, and both directions of the strict-mode
# toggle in step 10 -- see STRICT_MODE_ON / restore_strict_mode further
# down). A plain "docker compose -f $COMPOSE_FILE up -d" against a gateway
# container that's already running under examples/08-observability's
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
# Any GATEWAY_MCP_REQUIRE_PROFILE set on the caller's own command (the
# strict-mode toggle uses this) is still in the environment for either
# branch below, since a "VAR=val cmd" prefix exports VAR for that whole
# command -- these functions included.
compose_up_preserving_analytics() {
  if [[ -n "$(docker compose -f "$COMPOSE_FILE" --profile analytics ps -q --status running clickhouse 2>/dev/null)" ]]; then
    echo "  -> clickhouse is running (examples/08-observability's analytics profile); bringing the stack up through that overlay too"
    GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose -f "$COMPOSE_FILE" -f "$OBSERVABILITY_COMPOSE_FILE" --profile analytics up -d
  else
    "${GATEWAY_COMPOSE[@]}" up -d
  fi
}

# compose_up_gateway_preserving_analytics is the same guard, scoped to just
# the "gateway" service -- what the strict-mode toggle (step 10) and its
# restore actually recreate.
compose_up_gateway_preserving_analytics() {
  if [[ -n "$(docker compose -f "$COMPOSE_FILE" --profile analytics ps -q --status running clickhouse 2>/dev/null)" ]]; then
    echo "  -> clickhouse is running (examples/08-observability's analytics profile); bringing gateway up through that overlay too"
    GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose -f "$COMPOSE_FILE" -f "$OBSERVABILITY_COMPOSE_FILE" --profile analytics up -d gateway
  else
    "${GATEWAY_COMPOSE[@]}" up -d gateway
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

echo "check: required tools (docker, curl, jq, npx)"
require_cmd docker "Install Docker Desktop or Docker Engine."
require_cmd curl "Install curl."
require_cmd jq "Install jq (e.g. 'brew install jq' or 'apt-get install jq')."
require_cmd npx "Install Node.js -- npx ships with npm."
if ! docker compose version >/dev/null 2>&1; then
  echo "FAIL: 'docker compose' (the v2 plugin) is required." >&2
  exit 1
fi

# port_open reports (via exit status) whether something is already
# listening on 127.0.0.1:$1, using bash's built-in /dev/tcp rather than an
# extra dependency like nc or lsof.
port_open() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# ---------------------------------------------------------------------------
# 1. start the local "everything" MCP server
# ---------------------------------------------------------------------------

if port_open "$EVERYTHING_PORT"; then
  echo "check: MCP 'everything' server already listening on :$EVERYTHING_PORT -- reusing it"
else
  echo "check: starting MCP 'everything' server on :$EVERYTHING_PORT (npx @modelcontextprotocol/server-everything)"
  PORT="$EVERYTHING_PORT" npx -y @modelcontextprotocol/server-everything streamableHttp \
    >"$EVERYTHING_LOG_FILE" 2>&1 &
  echo $! >"$EVERYTHING_PID_FILE"

  ready=false
  for _ in $(seq 1 60); do
    if port_open "$EVERYTHING_PORT"; then
      ready=true
      break
    fi
    sleep 1
  done
  if [[ "$ready" != true ]]; then
    echo "FAIL: MCP 'everything' server did not start listening on :$EVERYTHING_PORT within 60s" >&2
    echo "---- last 50 lines of $EVERYTHING_LOG_FILE ----" >&2
    tail -n 50 "$EVERYTHING_LOG_FILE" >&2 || true
    exit 1
  fi
  echo "  -> listening on :$EVERYTHING_PORT (pid $(cat "$EVERYTHING_PID_FILE"))"
fi

# ---------------------------------------------------------------------------
# 2. compose up
# ---------------------------------------------------------------------------

echo "check: starting the compose stack (deploy/docker-compose.yml)"
compose_up_preserving_analytics

# ---------------------------------------------------------------------------
# 3. health on all three planes
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

# wait_for_health_soft is wait_for_health's best-effort twin, used only
# inside the strict-mode cleanup path (restore_strict_mode below): it
# warns instead of exiting, so a cleanup hiccup never masks whatever
# error actually triggered the cleanup.
wait_for_health_soft() {
  local name="$1" url="$2"
  for _ in $(seq 1 30); do
    if curl -sSf "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "WARN: $name did not become healthy at $url during cleanup" >&2
  return 0
}

wait_for_health "MCP plane" "http://localhost:8080/health"
wait_for_health "control plane" "http://localhost:8081/api/v1/health"
wait_for_health "LLM plane" "http://localhost:8082/health"

# ---------------------------------------------------------------------------
# 4. bootstrap an admin API key
# ---------------------------------------------------------------------------

if [[ -n "${GATEWAY_ADMIN_KEY:-}" ]]; then
  echo "check: reusing GATEWAY_ADMIN_KEY from the environment"
else
  echo "check: bootstrapping an admin API key (tenant 'default')"
  # bootstrap-key prints only the plaintext key on stdout; it refuses to
  # mint a second admin key for a tenant that already has one unless
  # --force is passed (cmd/gateway/main.go, runBootstrapKey). A re-run of
  # this script (or of 01-quickstart against the same stack) hits that
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
# 5. register the "everything" connector (idempotent: reuse by slug)
# ---------------------------------------------------------------------------

echo "check: looking for an existing 'everything' connector"
CONNECTOR_ID="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/connectors?limit=500" | jq -r '.items[] | select(.slug=="everything") | .id' | head -n1)"

if [[ -n "$CONNECTOR_ID" ]]; then
  echo "  -> reusing connector $CONNECTOR_ID"
else
  echo "check: registering the 'everything' connector"
  CONNECTOR_ID="$(curl -sSf -X POST "$CONTROL_BASE/connectors" \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
    -H "Content-Type: application/json" \
    -d '{
          "name": "everything",
          "slug": "everything",
          "endpoint": "http://host.docker.internal:23014/mcp"
        }' | jq -r '.id')"

  if [[ -z "$CONNECTOR_ID" || "$CONNECTOR_ID" == "null" ]]; then
    echo "FAIL: connector creation did not return an id" >&2
    exit 1
  fi
  echo "  -> created connector $CONNECTOR_ID"
fi

# ---------------------------------------------------------------------------
# 6. connector health check + discover
# ---------------------------------------------------------------------------

echo "check: connector health probe"
health_resp="$(curl -sSf -X GET "$CONTROL_BASE/connectors/$CONNECTOR_ID/health" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY")"
echo "  -> $health_resp"
health_status="$(jq -r '.status' <<<"$health_resp")"
if [[ "$health_status" != "healthy" ]]; then
  echo "FAIL: connector health is '$health_status', expected 'healthy'" >&2
  exit 1
fi

echo "check: connector tool discovery"
discover_resp="$(curl -sSf -X POST "$CONTROL_BASE/connectors/$CONNECTOR_ID/discover" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY")"
discovered_count="$(jq -r '.total' <<<"$discover_resp")"
echo "  -> discovered $discovered_count tool(s), written to the tool cache"

# NOTE on the "10" floor: like 01-quickstart, the exact tool count of
# @modelcontextprotocol/server-everything isn't pinned anywhere in this
# repo, and `npx -y` always pulls the latest published version, so assert
# a floor, not an exact number.
if [[ "$discovered_count" -lt 10 ]]; then
  echo "FAIL: expected at least 10 tools from the 'everything' connector, got $discovered_count" >&2
  exit 1
fi
if ! jq -e '[.items[].tool_name] | index("echo")' >/dev/null 2>&1 <<<"$discover_resp"; then
  echo "FAIL: discovery did not include the 'echo' tool" >&2
  exit 1
fi
if ! jq -e '[.items[].tool_name] | index("get-sum")' >/dev/null 2>&1 <<<"$discover_resp"; then
  echo "FAIL: discovery did not include the 'get-sum' tool" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 7. create (or reuse) the two profiles and grant their tools
# ---------------------------------------------------------------------------
#
# A profile is a named allow-list of (connector, tool) pairs -- see
# docs/profiles.md. POST /profiles is not idempotent by itself (a second
# POST with the same name is a 409: the slug is derived from the name and
# is unique among live profiles), so look up by name first, exactly like
# step 5 looks up the connector by slug first.
#
# PUT /profiles/{id}/tools *is* idempotent: it fully replaces the allow-list
# (internal/api/handlers/profiles.go, Profiles.SetTools), so it's called
# unconditionally below rather than only on first creation.

# ensure_profile looks up a profile by exact name and creates it if
# missing, leaving the result in $PROFILE_ID.
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

echo "check: ensuring the 'Echo Only' profile exists"
ensure_profile "Echo Only"
ECHO_PROFILE_ID="$PROFILE_ID"

echo "check: granting 'Echo Only' exactly 2 tools (echo, get-sum)"
echo_tools_json="$(jq -n --arg cid "$CONNECTOR_ID" \
  '{tools: [{connector_id: $cid, tool_name: "echo"}, {connector_id: $cid, tool_name: "get-sum"}]}')"
curl -sSf -X PUT "$CONTROL_BASE/profiles/$ECHO_PROFILE_ID/tools" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d "$echo_tools_json" >/dev/null
echo "  -> granted 2 tool(s)"

echo "check: ensuring the 'Full Access' profile exists"
ensure_profile "Full Access"
FULL_PROFILE_ID="$PROFILE_ID"

# "Full Access" is granted every tool on EVERY connector currently in the
# tenant, not just "everything" -- on a fresh stack "everything" is the
# only one, but this example is run repeatedly against the same
# persistent stack (examples-smoke, or by hand), and other examples
# register their own connectors that outlive their own run by design
# (06-credentials-and-headers' "header-echo", in particular -- see that
# example's README Cleanup section: "down alone leaves ... the connector
# in the volume for the next run to reuse"). Step 9 below compares the
# no-header tools/list count against "Full Access" specifically because
# mcp.require_profile is false by default: with no profile header, a
# caller sees every tool the TENANT owns, across every connector, so
# "Full Access" has to mean that literally, or the comparison is really
# just re-testing "everything" is the only connector -- true on a fresh
# stack, false the moment any other example has left one behind.
echo "check: discovering every connector in the tenant, for 'Full Access'"
all_connector_ids="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/connectors?limit=500" | jq -r '.items[].id')"
full_tools_json='{"tools":[]}'
connector_total=0
while IFS= read -r cid; do
  [[ -z "$cid" ]] && continue
  connector_total=$((connector_total + 1))
  cid_discover_resp="$(curl -sSf -X POST "$CONTROL_BASE/connectors/$cid/discover" \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY")"
  full_tools_json="$(jq --argjson prev "$full_tools_json" --arg cid "$cid" \
    '{tools: ($prev.tools + [.items[] | {connector_id: $cid, tool_name: .tool_name}])}' \
    <<<"$cid_discover_resp")"
done <<<"$all_connector_ids"
tenant_total_tool_count="$(jq '.tools | length' <<<"$full_tools_json")"

curl -sSf -X PUT "$CONTROL_BASE/profiles/$FULL_PROFILE_ID/tools" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d "$full_tools_json" >/dev/null
echo "  -> granted $tenant_total_tool_count tool(s) across $connector_total connector(s)"

# ---------------------------------------------------------------------------
# 8. mint an agent-role API key
# ---------------------------------------------------------------------------
#
# Unlike the profiles and connector above, this step is NOT idempotent by
# design: an API key's plaintext (the "key" field) is only ever returned
# once, on creation (internal/api/handlers/apikeys.go, APIKeys.Create), so
# there is nothing to "look up and reuse" -- a re-run mints a fresh
# "05-profiles-agent" key and leaves the previous one active but orphaned.
# See the README's Cleanup section.

echo "check: minting a fresh agent-role API key ('05-profiles-agent')"
agent_key_resp="$(curl -sSf -X POST "$CONTROL_BASE/api-keys" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "05-profiles-agent", "role": "agent"}')"
AGENT_KEY="$(jq -r '.key' <<<"$agent_key_resp")"
if [[ -z "$AGENT_KEY" || "$AGENT_KEY" == "null" ]]; then
  echo "FAIL: api-key creation did not return a plaintext key" >&2
  exit 1
fi
echo "  -> minted $(jq -r '.prefix' <<<"$agent_key_resp")... (role: $(jq -r '.role' <<<"$agent_key_resp"))"

# ---------------------------------------------------------------------------
# 9. MCP handshake + profile-scoped tools/list and tools/call
# ---------------------------------------------------------------------------

HEADERS_TMP="$(mktemp)"
SESSION_ID=""
MCP_RESP=""
MCP_ERROR_CODE=""

# STRICT_MODE_ON gates restore_strict_mode (step 10): it is flipped on
# right before the gateway is recreated with require_profile=true, so the
# trap below only tries to restore it if that actually happened.
STRICT_MODE_ON=false

# restore_strict_mode turns GATEWAY_MCP_REQUIRE_PROFILE back off and waits
# for the gateway to come back healthy. It is both called directly (the
# happy path, at the end of step 10) and registered as part of the EXIT
# trap just below, so a failure partway through strict-mode testing still
# leaves the local stack in its default, non-strict state.
restore_strict_mode() {
  if [[ "$STRICT_MODE_ON" != true ]]; then
    return 0
  fi
  echo "cleanup: restoring GATEWAY_MCP_REQUIRE_PROFILE=false"
  GATEWAY_MCP_REQUIRE_PROFILE=false compose_up_gateway_preserving_analytics \
    || echo "WARN: failed to restore the gateway to non-strict mode -- run 'GATEWAY_MCP_REQUIRE_PROFILE=false docker compose -f $COMPOSE_FILE up -d gateway' (add '-f $OBSERVABILITY_COMPOSE_FILE --profile analytics' and GATEWAY_SINKS_CLICKHOUSE_ENABLED=true too if examples/08-observability is running) by hand" >&2
  wait_for_health_soft "MCP plane" "http://localhost:8080/health"
  STRICT_MODE_ON=false
}

cleanup() {
  rm -f "$HEADERS_TMP"
  restore_strict_mode
}
trap cleanup EXIT

# mcp_call POSTs one JSON-RPC request to $MCP_URL, authenticated as the
# agent key minted in step 8 and, when $3 is given, scoped by an
# X-Agent-Profile-Name header. The response body is left in MCP_RESP (not
# echoed: a $(...) capture would run this in a subshell and lose the
# SESSION_ID it records). It fails loudly on a non-200 HTTP status or a
# JSON-RPC-level "error" in the response -- per
# internal/dataplane/transport/http.go, every non-auth failure the gateway
# itself produces is still HTTP 200 with a JSON-RPC error object, so the
# HTTP status alone can't tell success from failure. See
# 01-quickstart/run.sh for the same helper against the admin key.
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

# mcp_call_expect_error is mcp_call's mirror image for this example's
# negative-path assertions: a JSON-RPC "error" in the response is the
# EXPECTED outcome here (left in MCP_ERROR_CODE, as a bare number), and it
# fails the script if the call unexpectedly succeeds instead.
mcp_call_expect_error() {
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
  if ! jq -e '.error' >/dev/null 2>&1 <<<"$resp"; then
    echo "FAIL: $label was expected to return a JSON-RPC error but succeeded: $resp" >&2
    exit 1
  fi

  MCP_ERROR_CODE="$(jq -r '.error.code' <<<"$resp")"
  MCP_RESP="$resp"
}

# mcp_notify is for a JSON-RPC notification (no "id"): the gateway answers
# with an empty HTTP 204, never a JSON-RPC body (see http.go's rpc handler:
# "A notification gets no body at all, per JSON-RPC.").
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

echo "check: MCP initialize (agent key)"
mcp_call "initialize" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"05-profiles","version":"0.1.0"}}}'
echo "  -> protocolVersion=$(jq -r '.result.protocolVersion' <<<"$MCP_RESP"), session=$SESSION_ID"
if [[ -z "$SESSION_ID" ]]; then
  echo "FAIL: initialize did not return an Mcp-Session-Id header" >&2
  exit 1
fi

echo "check: MCP notifications/initialized"
mcp_notify "notifications/initialized" '{"jsonrpc":"2.0","method":"notifications/initialized"}'

# The profile header is not remembered on the session (docs/profiles.md):
# it is read fresh off every request, so one session can be reused across
# every tools/list and tools/call call below, varying only the header.

echo "check: MCP tools/list with X-Agent-Profile-Name: Echo Only"
mcp_call "tools/list (Echo Only)" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' "Echo Only"
echo_only_count="$(jq '.result.tools | length' <<<"$MCP_RESP")"
echo_only_names="$(jq -r '[.result.tools[].name] | sort | join(",")' <<<"$MCP_RESP")"
echo "  -> $echo_only_count tool(s): $echo_only_names"
if [[ "$echo_only_count" != "2" ]]; then
  echo "FAIL: expected exactly 2 tools for 'Echo Only', got $echo_only_count" >&2
  exit 1
fi
if [[ "$echo_only_names" != "everything__echo,everything__get-sum" ]]; then
  echo "FAIL: 'Echo Only' granted the wrong tools: $echo_only_names" >&2
  exit 1
fi

echo "check: MCP tools/list with X-Agent-Profile-Name: Full Access"
mcp_call "tools/list (Full Access)" '{"jsonrpc":"2.0","id":3,"method":"tools/list"}' "Full Access"
full_access_count="$(jq '.result.tools | length' <<<"$MCP_RESP")"
echo "  -> $full_access_count tool(s)"
if [[ "$full_access_count" != "$tenant_total_tool_count" ]]; then
  echo "FAIL: expected 'Full Access' to grant all $tenant_total_tool_count tenant tools, got $full_access_count" >&2
  exit 1
fi

echo "check: MCP tools/list with no X-Agent-Profile-Name header"
# "Full Access" is built (step 7 above) from every connector currently in
# the tenant, not just "everything", precisely so this comparison holds
# regardless of what other examples have left running against this same
# persistent stack.
mcp_call "tools/list (no profile header)" '{"jsonrpc":"2.0","id":4,"method":"tools/list"}'
no_header_count="$(jq '.result.tools | length' <<<"$MCP_RESP")"
echo "  -> $no_header_count tool(s)"
if [[ "$no_header_count" != "$full_access_count" ]]; then
  echo "FAIL: expected no-header tools/list ($no_header_count) to match 'Full Access' ($full_access_count) -- mcp.require_profile is false by default, so every tenant tool should be visible" >&2
  exit 1
fi

echo "check: MCP tools/call everything__get-env with X-Agent-Profile-Name: Echo Only (expect denial)"
mcp_call_expect_error "tools/call get-env (Echo Only)" '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"everything__get-env","arguments":{}}}' "Echo Only"
echo "  -> denied with JSON-RPC error code $MCP_ERROR_CODE"
if [[ "$MCP_ERROR_CODE" != "-32003" ]]; then
  echo "FAIL: expected error code -32003 (tool not allowed by profile), got $MCP_ERROR_CODE" >&2
  exit 1
fi

echo "check: MCP tools/call everything__echo with X-Agent-Profile-Name: Echo Only (expect success)"
mcp_call "tools/call echo (Echo Only)" '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"everything__echo","arguments":{"message":"hello from Echo Only"}}}' "Echo Only"
echo_text="$(jq -r '.result.content[0].text // empty' <<<"$MCP_RESP")"
echo "  -> $echo_text"
if [[ "$echo_text" != *"hello from Echo Only"* ]]; then
  echo "FAIL: tools/call response did not echo back the sent message. Got: $echo_text" >&2
  exit 1
fi

echo "check: MCP tools/list with X-Agent-Profile-Name: does-not-exist (unknown profile grants nothing)"
mcp_call "tools/list (unknown profile)" '{"jsonrpc":"2.0","id":7,"method":"tools/list"}' "does-not-exist"
unknown_count="$(jq '.result.tools | length' <<<"$MCP_RESP")"
echo "  -> $unknown_count tool(s)"
if [[ "$unknown_count" != "0" ]]; then
  echo "FAIL: expected an unknown profile name to grant 0 tools, got $unknown_count" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 10. strict mode: mcp.require_profile / GATEWAY_MCP_REQUIRE_PROFILE=true
# ---------------------------------------------------------------------------
#
# Recreating the gateway container drops every in-memory MCP session
# (internal/dataplane/session) along with it, so both here and on the way
# back down, a fresh initialize is required before the next call.

echo
echo "check: strict mode -- GATEWAY_MCP_REQUIRE_PROFILE=true"
STRICT_MODE_ON=true
GATEWAY_MCP_REQUIRE_PROFILE=true compose_up_gateway_preserving_analytics
wait_for_health "MCP plane" "http://localhost:8080/health"

SESSION_ID=""
echo "check: MCP initialize (strict mode)"
mcp_call "initialize (strict mode)" '{"jsonrpc":"2.0","id":10,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"05-profiles","version":"0.1.0"}}}'
echo "check: MCP notifications/initialized (strict mode)"
mcp_notify "notifications/initialized (strict mode)" '{"jsonrpc":"2.0","method":"notifications/initialized"}'

echo "check: MCP tools/list with no header under strict mode (expect denial)"
mcp_call_expect_error "tools/list (strict mode, no header)" '{"jsonrpc":"2.0","id":11,"method":"tools/list"}'
echo "  -> denied with JSON-RPC error code $MCP_ERROR_CODE"
if [[ "$MCP_ERROR_CODE" != "-32003" ]]; then
  echo "FAIL: expected error code -32003 (an agent profile is required), got $MCP_ERROR_CODE" >&2
  exit 1
fi

echo "check: MCP tools/list with X-Agent-Profile-Name: Echo Only still works under strict mode"
mcp_call "tools/list (strict mode, Echo Only)" '{"jsonrpc":"2.0","id":12,"method":"tools/list"}' "Echo Only"
strict_echo_count="$(jq '.result.tools | length' <<<"$MCP_RESP")"
echo "  -> $strict_echo_count tool(s)"
if [[ "$strict_echo_count" != "2" ]]; then
  echo "FAIL: expected 'Echo Only' to still grant exactly 2 tools under strict mode, got $strict_echo_count" >&2
  exit 1
fi

echo "check: restoring GATEWAY_MCP_REQUIRE_PROFILE=false"
restore_strict_mode

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

echo
echo "================================================================"
echo "05-profiles passed."
echo "Console:    $CONSOLE_URL"
echo "Agent key:  $AGENT_KEY"
echo
echo "A client should send, on every MCP request:"
echo "  Authorization: Bearer $AGENT_KEY"
echo "  X-Agent-Profile-Name: Echo Only"
echo "================================================================"
