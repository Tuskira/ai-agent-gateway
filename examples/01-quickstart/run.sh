#!/usr/bin/env bash
#
# 01-quickstart: bring up the gateway's compose stack, bootstrap an admin
# API key, register the @modelcontextprotocol/server-everything reference
# MCP server as a connector, and drive it end to end -- discover, then a
# real tools/list and tools/call, prompts/list and prompts/get, and
# resources/list and resources/read over the MCP plane.
#
# Safe to re-run: every step checks current state first and reuses it
# instead of failing or duplicating work.
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
  # this script hits that case, and the earlier key's plaintext is gone,
  # so mint another one.
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
existing="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/connectors?limit=500" | jq -r '.items[] | select(.slug=="everything") | .id' | head -n1)"

if [[ -n "$existing" ]]; then
  CONNECTOR_ID="$existing"
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

# ---------------------------------------------------------------------------
# 7. MCP handshake: initialize -> notifications/initialized -> tools/list -> tools/call
#    -> prompts/list -> prompts/get -> resources/list -> resources/read
# ---------------------------------------------------------------------------

HEADERS_TMP="$(mktemp)"
trap 'rm -f "$HEADERS_TMP"' EXIT

SESSION_ID=""
MCP_RESP=""

# mcp_call POSTs one JSON-RPC request to $MCP_URL, authenticated, carrying
# the session id from a prior initialize (once one exists). The response
# body is left in MCP_RESP (not echoed: a $(...) capture would run this in
# a subshell and lose the SESSION_ID it records). It fails loudly
# on a non-200 HTTP status or a JSON-RPC-level "error" in the response --
# per internal/dataplane/transport/http.go, every non-auth failure the
# gateway itself produces is still HTTP 200 with a JSON-RPC error object,
# so the HTTP status alone can't tell success from failure.
mcp_call() {
  local label="$1" body="$2"
  local extra_headers=()
  if [[ -n "$SESSION_ID" ]]; then
    extra_headers+=(-H "Mcp-Session-Id: $SESSION_ID")
  fi

  local full http_code resp
  full="$(curl -sS -D "$HEADERS_TMP" \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
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
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
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

echo "check: MCP initialize"
mcp_call "initialize" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"01-quickstart","version":"0.1.0"}}}'
echo "  -> protocolVersion=$(jq -r '.result.protocolVersion' <<<"$MCP_RESP"), session=$SESSION_ID"
if [[ -z "$SESSION_ID" ]]; then
  echo "FAIL: initialize did not return an Mcp-Session-Id header" >&2
  exit 1
fi

# Not required by this gateway for tools/list or tools/call to work (both
# lazily handshake with a connector on demand -- see orchestrator.go's
# ensureBackend), but it's part of the MCP handshake every well-behaved
# client sends, so the example sends it too.
echo "check: MCP notifications/initialized"
mcp_notify "notifications/initialized" '{"jsonrpc":"2.0","method":"notifications/initialized"}'

echo "check: MCP tools/list"
mcp_call "tools/list" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
list_resp="$MCP_RESP"
tool_count="$(jq '[.result.tools[] | select(.name | startswith("everything__"))] | length' <<<"$list_resp")"
echo "  -> $tool_count tool(s) advertised under the 'everything' connector"

# NOTE on the "10" floor: @modelcontextprotocol/server-everything's exact
# tool count isn't pinned anywhere in this repo -- test/e2e/mcp_test.go and
# internal/dataplane/dptest only ever stand up a synthetic 2-tool double,
# never the real npm package -- and `npx -y` always pulls the latest
# published version, so the upstream count can drift across releases.
# Assert a floor, not an exact number.
if [[ "$tool_count" -lt 10 ]]; then
  echo "FAIL: expected at least 10 tools from the 'everything' connector, got $tool_count" >&2
  exit 1
fi
if ! jq -e '[.result.tools[].name] | index("everything__echo")' >/dev/null 2>&1 <<<"$list_resp"; then
  echo "FAIL: tools/list did not include everything__echo" >&2
  exit 1
fi

echo "check: MCP tools/call everything__echo"
mcp_call "tools/call" '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"everything__echo","arguments":{"message":"hello from the gateway"}}}'
echo_text="$(jq -r '.result.content[0].text // empty' <<<"$MCP_RESP")"
echo "  -> $echo_text"
if [[ "$echo_text" != *"hello from the gateway"* ]]; then
  echo "FAIL: tools/call response did not echo back the sent message. Got: $echo_text" >&2
  exit 1
fi

# Prompts and resources pass through the same way. Prompt names are
# namespaced like tools ("everything__simple-prompt"); resource URIs are
# wrapped as "gw://<connector slug>/<the server's own uri>", so
# "demo://resource/static/document/architecture.md" is advertised as
# "gw://everything/demo://resource/static/document/architecture.md".
echo "check: MCP prompts/list"
mcp_call "prompts/list" '{"jsonrpc":"2.0","id":4,"method":"prompts/list"}'
prompt_count="$(jq '[.result.prompts[] | select(.name | startswith("everything__"))] | length' <<<"$MCP_RESP")"
echo "  -> $prompt_count prompt(s) advertised under the 'everything' connector"
if [[ "$prompt_count" -lt 1 ]]; then
  echo "FAIL: expected at least 1 everything__ prompt, got $prompt_count" >&2
  exit 1
fi

echo "check: MCP prompts/get everything__simple-prompt"
mcp_call "prompts/get" '{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":{"name":"everything__simple-prompt"}}'
message_count="$(jq '.result.messages | length' <<<"$MCP_RESP")"
echo "  -> $message_count message(s): $(jq -c '.result.messages[0].content' <<<"$MCP_RESP")"
if [[ "$message_count" -lt 1 ]]; then
  echo "FAIL: prompts/get returned no messages" >&2
  exit 1
fi

echo "check: MCP resources/list"
mcp_call "resources/list" '{"jsonrpc":"2.0","id":6,"method":"resources/list"}'
resource_uri="$(jq -r '[.result.resources[].uri | select(startswith("gw://everything/"))][0] // empty' <<<"$MCP_RESP")"
resource_count="$(jq '[.result.resources[] | select(.uri | startswith("gw://everything/"))] | length' <<<"$MCP_RESP")"
echo "  -> $resource_count resource(s) under gw://everything/, first: $resource_uri"
if [[ -z "$resource_uri" ]]; then
  echo "FAIL: expected at least 1 gw://everything/ resource" >&2
  exit 1
fi

echo "check: MCP resources/read $resource_uri"
mcp_call "resources/read" "$(jq -nc --arg uri "$resource_uri" '{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":$uri}}')"
contents_count="$(jq '.result.contents | length' <<<"$MCP_RESP")"
echo "  -> $contents_count content item(s)"
if [[ "$contents_count" -lt 1 ]]; then
  echo "FAIL: resources/read returned no contents" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

echo
echo "================================================================"
echo "01-quickstart passed."
echo "Console:    $CONSOLE_URL"
echo "Admin key:  $GATEWAY_ADMIN_KEY"
echo "================================================================"
