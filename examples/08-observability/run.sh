#!/usr/bin/env bash
#
# 08-observability: turn on the ClickHouse analytics tee and the OTel span
# exporter, send MCP traffic under one shared W3C trace id, then read the
# same numbers back three ways -- the analytics REST API, ClickHouse SQL,
# and the OTel collector's own debug log -- and print the Jaeger URL for
# that trace.
#
# Self-contained: does not assume 01-quickstart has been run first, but
# reuses whatever it already left running (the "everything" MCP server,
# the compose stack, the "everything" connector) instead of duplicating
# it.
#
# Safe to re-run: every step checks current state first and reuses it
# instead of failing or duplicating work.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.yml"
OBSERVABILITY_COMPOSE_FILE="$SCRIPT_DIR/docker-compose.observability.yml"

EVERYTHING_PORT=23014
EVERYTHING_PID_FILE="$SCRIPT_DIR/.everything.pid"
EVERYTHING_LOG_FILE="$SCRIPT_DIR/.everything.log"

CONTROL_BASE="http://localhost:8081/api/v1"
MCP_URL="http://localhost:8080/mcp"
CONSOLE_URL="http://localhost:8081"

# Matches deploy/docker-compose.yml's own CLICKHOUSE_PASSWORD default --
# needed below to exec into the clickhouse container as the same user the
# gateway itself connects as.
CLICKHOUSE_PASSWORD="${CLICKHOUSE_PASSWORD:-local}"

# This example is the analytics profile's reason to exist -- every
# command against this stack needs both compose files and the profile, so
# there is no "without it" variant to keep separate (unlike
# 01-quickstart/05-profiles, which never touch either).
ANALYTICS_COMPOSE=(docker compose -f "$COMPOSE_FILE" -f "$OBSERVABILITY_COMPOSE_FILE" --profile analytics)

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "FAIL: '$1' is required but was not found in PATH. $2" >&2
    exit 1
  fi
}

echo "check: required tools (docker, curl, jq, npx, od)"
require_cmd docker "Install Docker Desktop or Docker Engine."
require_cmd curl "Install curl."
require_cmd jq "Install jq (e.g. 'brew install jq' or 'apt-get install jq')."
require_cmd npx "Install Node.js -- npx ships with npm."
require_cmd od "od ships with every macOS/Linux base install; used to mint the trace/span ids below without any extra dependency."
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
# 2. compose up: base stack + the analytics profile + the OTel/Jaeger overlay
# ---------------------------------------------------------------------------
#
# GATEWAY_SINKS_CLICKHOUSE_ENABLED is read straight off the shell by
# deploy/docker-compose.yml (GATEWAY_SINKS_CLICKHOUSE_ENABLED:
# "${GATEWAY_SINKS_CLICKHOUSE_ENABLED:-false}") -- exactly the invocation
# docs/observability.md documents. The four GATEWAY_SINKS_OTEL_* vars
# have no such default in the base file to override, so
# docker-compose.observability.yml sets them directly instead (see that
# file's header comment).

echo "check: starting the compose stack with the analytics profile (ClickHouse + OTel collector + Jaeger)"
GATEWAY_SINKS_CLICKHOUSE_ENABLED=true "${ANALYTICS_COMPOSE[@]}" up -d

# ---------------------------------------------------------------------------
# 3. health on all three gateway planes + the otel-collector
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
wait_for_health "otel-collector" "http://localhost:13133/"

echo "check: control-plane health reports both the clickhouse and otel sinks enabled"
health_body="$(curl -sSf "http://localhost:8081/api/v1/health")"
if ! jq -e '.sinks.clickhouse.enabled == true and .sinks.otel.enabled == true' >/dev/null 2>&1 <<<"$health_body"; then
  echo "FAIL: expected sinks.clickhouse.enabled and sinks.otel.enabled both true, got: $(jq -c '.sinks' <<<"$health_body")" >&2
  exit 1
fi
echo "  -> $(jq -c '.sinks' <<<"$health_body")"

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
  # this script (or of 01-quickstart/05-profiles against the same stack)
  # hits that case, and the earlier key's plaintext is gone, so mint
  # another one.
  if out="$("${ANALYTICS_COMPOSE[@]}" exec -T gateway /gateway bootstrap-key 2>/dev/null)" && [[ -n "$out" ]]; then
    echo "  -> bootstrapped a new admin key"
  else
    echo "  -> tenant 'default' already has an admin key; minting another with --force"
    out="$("${ANALYTICS_COMPOSE[@]}" exec -T gateway /gateway bootstrap-key --force 2>/dev/null)" || true
    if [[ -z "$out" ]]; then
      echo "FAIL: could not bootstrap an admin API key. Check '${ANALYTICS_COMPOSE[*]} logs gateway'." >&2
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

# ---------------------------------------------------------------------------
# 7. MCP handshake, then generate traffic under one shared W3C trace id
# ---------------------------------------------------------------------------

HEADERS_TMP="$(mktemp)"
trap 'rm -f "$HEADERS_TMP"' EXIT

SESSION_ID=""
MCP_RESP=""

# mcp_call POSTs one JSON-RPC request to $MCP_URL, authenticated as the
# admin key, carrying the session id from a prior initialize (once one
# exists) and, when $3 is given, a "traceparent" header -- this example's
# one addition to the helper 01-quickstart/05-profiles both define (05
# adds X-Agent-Profile-Name the same way, as its $3). The response body is
# left in MCP_RESP (not echoed: a $(...) capture would run this in a
# subshell and lose the SESSION_ID it records). It fails loudly on a
# non-200 HTTP status or a JSON-RPC-level "error" in the response --
# per internal/dataplane/transport/http.go, every non-auth failure the
# gateway itself produces is still HTTP 200 with a JSON-RPC error object,
# so the HTTP status alone can't tell success from failure.
mcp_call() {
  local label="$1" body="$2" traceparent="${3:-}"
  local extra_headers=()
  if [[ -n "$SESSION_ID" ]]; then
    extra_headers+=(-H "Mcp-Session-Id: $SESSION_ID")
  fi
  if [[ -n "$traceparent" ]]; then
    extra_headers+=(-H "traceparent: $traceparent")
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
mcp_call "initialize" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"08-observability","version":"0.1.0"}}}'
echo "  -> protocolVersion=$(jq -r '.result.protocolVersion' <<<"$MCP_RESP"), session=$SESSION_ID"
if [[ -z "$SESSION_ID" ]]; then
  echo "FAIL: initialize did not return an Mcp-Session-Id header" >&2
  exit 1
fi

echo "check: MCP notifications/initialized"
mcp_notify "notifications/initialized" '{"jsonrpc":"2.0","method":"notifications/initialized"}'

# One shared trace id for the whole run, minted the same way
# pkg/trace.GenerateTraceID does conceptually (32 random hex chars) but
# with no Go/python dependency: 16 random bytes from /dev/urandom via od,
# which already prints lowercase hex -- pkg/trace.ParseTraceparent
# rejects uppercase outright (it's echoed into logs and forwarded to
# backends, so it validates strictly). A malformed traceparent isn't an
# error at the gateway: trace.FromHeader silently falls back to a FRESH
# trace id instead (best-effort correlation), which would make every
# assertion below fail against the wrong id -- so this has to be exactly
# right.
TRACE_ID="$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')"
echo "check: minted one shared trace id for this run: $TRACE_ID"

# next_span_id mints a fresh 16-hex-char span id per call (the "parent
# span id" half of each traceparent header below). All-zero is invalid
# per the W3C spec (pkg/trace.ValidateSpanID rejects it, same fallback
# risk as an all-zero/malformed trace id above) -- vanishingly unlikely
# from 8 random bytes, but retried rather than risked.
next_span_id() {
  local id
  id="$(od -An -tx1 -N8 /dev/urandom | tr -d ' \n')"
  while [[ "$id" == "0000000000000000" ]]; do
    id="$(od -An -tx1 -N8 /dev/urandom | tr -d ' \n')"
  done
  echo "$id"
}

echo "check: sending 3 tools/list calls under trace id $TRACE_ID"
for i in 1 2 3; do
  mcp_call "tools/list ($i/3)" "{\"jsonrpc\":\"2.0\",\"id\":$((10 + i)),\"method\":\"tools/list\"}" \
    "00-$TRACE_ID-$(next_span_id)-01"
done
list_tool_count="$(jq '.result.tools | length' <<<"$MCP_RESP")"
echo "  -> $list_tool_count tool(s) advertised"

echo "check: sending 20 tools/call everything__echo calls under trace id $TRACE_ID"
for i in $(seq 1 20); do
  mcp_call "tools/call everything__echo ($i/20)" \
    "{\"jsonrpc\":\"2.0\",\"id\":$((20 + i)),\"method\":\"tools/call\",\"params\":{\"name\":\"everything__echo\",\"arguments\":{\"message\":\"08-observability-$i\"}}}" \
    "00-$TRACE_ID-$(next_span_id)-01"
  echo_text="$(jq -r '.result.content[0].text // empty' <<<"$MCP_RESP")"
  if [[ "$echo_text" != *"08-observability-$i"* ]]; then
    echo "FAIL: tools/call #$i did not echo back its message. Got: $echo_text" >&2
    exit 1
  fi
done
echo "  -> 20/20 calls succeeded and echoed back correctly"

# ---------------------------------------------------------------------------
# 8. wait for the ClickHouse tee to flush, then assert via the analytics API
# ---------------------------------------------------------------------------

# pkg/sink/clickhouse/config.go: defaultBatchSize=100, defaultFlushInterval
# =5s -- this run's ~25 records never reaches the batch-size trigger, so
# everything waits on the 5s timer. 7s gives a 2s margin.
echo "check: waiting 7s for the ClickHouse sink to flush its batch"
sleep 7

echo "check: analytics/overview counts at least the 20 tools/call above"
before_body="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/analytics/overview?range=24h")"
overview_calls="$(jq -r '.kpis.mcpToolCalls.value' <<<"$before_body")"
echo "  -> kpis.mcpToolCalls.value (24h window) = $overview_calls"
if [[ "$overview_calls" -lt 20 ]]; then
  echo "FAIL: expected analytics/overview kpis.mcpToolCalls.value >= 20, got $overview_calls" >&2
  exit 1
fi
if ! jq -e '[.mcpTools[].name] | index("everything__echo")' >/dev/null 2>&1 <<<"$before_body"; then
  echo "FAIL: analytics/overview mcpTools did not include everything__echo: $(jq -c '.mcpTools' <<<"$before_body")" >&2
  exit 1
fi
echo "  -> mcpTools includes everything__echo: $(jq -c '.mcpTools' <<<"$before_body")"

echo "check: analytics/logs rows carry the shared trace id"
# AccessLogFilter (pkg/analytics/analytics.go) has no trace_id field to
# filter server-side by, so this fetches the tool's own rows (ordered
# newest-first, pkg/sink/clickhouse/reader.go's ListAccessLogs) and
# matches trace_id client-side with jq -- exactly 20, one per echo call
# above (the 3 tools/list calls share the same trace id but carry no
# tool_name, so they never match tool=everything__echo).
logs_resp="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/analytics/logs?tool=everything__echo&limit=50")"
matching_logs="$(jq --arg tid "$TRACE_ID" '[.items[] | select(.trace_id == $tid)] | length' <<<"$logs_resp")"
echo "  -> $matching_logs access-log row(s) match trace_id=$TRACE_ID"
if [[ "$matching_logs" -ne 20 ]]; then
  echo "FAIL: expected exactly 20 analytics/logs rows with trace_id=$TRACE_ID, got $matching_logs" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 9. assert the same thing straight from ClickHouse SQL
# ---------------------------------------------------------------------------

echo "check: same trace id, straight from ClickHouse (mcp_access_logs)"
ch_count_raw="$("${ANALYTICS_COMPOSE[@]}" exec -T clickhouse clickhouse-client \
  --user default --password "$CLICKHOUSE_PASSWORD" --database gateway \
  -q "SELECT count() FROM mcp_access_logs WHERE trace_id = '$TRACE_ID'")"
ch_count="$(tr -d '[:space:]' <<<"$ch_count_raw")"
echo "  -> mcp_access_logs rows with trace_id=$TRACE_ID: $ch_count (>= 20 tools/call + 3 tools/list)"
if [[ "$ch_count" -lt 20 ]]; then
  echo "FAIL: expected ClickHouse to have at least 20 mcp_access_logs rows with trace_id=$TRACE_ID, got $ch_count" >&2
  exit 1
fi

echo "check: running examples/08-observability/queries.sql against ClickHouse (top tools / slow calls / cost by model)"
# ClickHouse 24.8+ (the image deploy/docker-compose.yml pins) runs
# multiple ';'-separated statements from stdin by default -- no
# --multiquery flag needed (it's obsolete as of 24.8 and would be a no-op
# anyway).
if ! "${ANALYTICS_COMPOSE[@]}" exec -T clickhouse clickhouse-client \
  --user default --password "$CLICKHOUSE_PASSWORD" \
  <"$SCRIPT_DIR/queries.sql"; then
  echo "FAIL: queries.sql did not run cleanly against ClickHouse" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 10. assert the otel-collector's debug exporter saw the same trace id
# ---------------------------------------------------------------------------

echo "check: otel-collector debug exporter logged spans with trace id $TRACE_ID"
collector_container="$("${ANALYTICS_COMPOSE[@]}" ps -q otel-collector)"
if [[ -z "$collector_container" ]]; then
  echo "FAIL: otel-collector container not found -- is the compose stack up with the analytics profile?" >&2
  exit 1
fi
found=false
for _ in $(seq 1 30); do
  # grep -c, not grep -q: -q exits at the first match and the SIGPIPE it
  # sends docker logs would make this pipeline fail under pipefail.
  if [[ "$(docker logs "$collector_container" 2>&1 | grep -ic "$TRACE_ID")" -gt 0 ]]; then
    found=true
    break
  fi
  sleep 1
done
if [[ "$found" != true ]]; then
  echo "FAIL: otel-collector never logged a span with trace id $TRACE_ID within 30s -- check:" >&2
  echo "  ${ANALYTICS_COMPOSE[*]} logs otel-collector" >&2
  exit 1
fi
match_lines="$(docker logs "$collector_container" 2>&1 | grep -ic "$TRACE_ID")"
echo "  -> $match_lines log line(s) mention trace id $TRACE_ID"

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

echo
echo "================================================================"
echo "08-observability passed."
echo "Console:        $CONSOLE_URL"
echo "Overview:       $CONSOLE_URL/"
echo "Access Logs:    $CONSOLE_URL/access-logs"
echo "LLM Logs:       $CONSOLE_URL/llm-logs"
echo "Jaeger trace:   http://localhost:16686/trace/$TRACE_ID"
echo "Admin key:      $GATEWAY_ADMIN_KEY"
echo "================================================================"
