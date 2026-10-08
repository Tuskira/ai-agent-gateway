#!/usr/bin/env bash
#
# 14-prometheus: turn on the gateway's Prometheus endpoint, run Prometheus
# (and Grafana, behind the "grafana" compose profile) next to it, drive real
# MCP traffic, then read the same traffic back three ways -- the raw
# /metrics endpoint, the Prometheus HTTP API, and Grafana's API -- and
# assert the series the dashboard and the alert rules are built on.
#
# Self-contained: does not assume any other example has been run, but
# reuses whatever it left running (the "everything" MCP server, the
# "everything" connector).
#
# Safe to re-run: every step checks current state first and reuses it
# instead of failing or duplicating work. On success the compose stack is
# torn down (volumes are kept); set KEEP_STACK=1 to leave it running and
# poke at Prometheus (:9090) and Grafana (:3000). On failure it is always
# left running so you can look.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.yml"
PROMETHEUS_COMPOSE_FILE="$SCRIPT_DIR/docker-compose.prometheus.yml"
DASHBOARD_FILE="$SCRIPT_DIR/grafana/dashboards/gateway.json"

EVERYTHING_PORT=23014
EVERYTHING_PID_FILE="$SCRIPT_DIR/.everything.pid"
EVERYTHING_LOG_FILE="$SCRIPT_DIR/.everything.log"

CONTROL_BASE="http://localhost:8081/api/v1"
MCP_URL="http://localhost:8080/mcp"
METRICS_URL="http://localhost:9464/metrics"
PROMETHEUS_URL="http://localhost:9090"
GRAFANA_URL="http://localhost:3000"
# The admin login set by GF_SECURITY_ADMIN_* in docker-compose.prometheus.yml.
GRAFANA_USER="admin"
GRAFANA_PASSWORD="admin"

# Every command against this stack needs both compose files and the
# grafana profile (a compose command without the profile would not see, and
# so would not stop, the grafana container).
PROM_COMPOSE=(docker compose -f "$COMPOSE_FILE" -f "$PROMETHEUS_COMPOSE_FILE" --profile grafana)

fail() {
  echo "FAIL: $*" >&2
  echo "      the stack is still running; inspect it with: ${PROM_COMPOSE[*]} logs gateway prometheus grafana" >&2
  exit 1
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
[[ -f "$DASHBOARD_FILE" ]] || { echo "FAIL: $DASHBOARD_FILE is missing" >&2; exit 1; }

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
# 2. compose up: base stack + metrics on the gateway + Prometheus + Grafana
# ---------------------------------------------------------------------------

echo "check: starting the compose stack (gateway with metrics, Prometheus, Grafana)"
# --build: this example exercises the metrics code in the working tree, so
# never reuse a gateway image built before it existed (a no-op when cached).
"${PROM_COMPOSE[@]}" up -d --build

# ---------------------------------------------------------------------------
# 3. health: the three planes and the metrics listener
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
  fail "$name never became healthy at $url"
}

wait_for_health "MCP plane" "http://localhost:8080/health"
wait_for_health "control plane" "$CONTROL_BASE/health"
wait_for_health "LLM plane" "http://localhost:8082/health"
wait_for_health "metrics listener" "http://localhost:9464/health"

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
  if out="$("${PROM_COMPOSE[@]}" exec -T gateway /gateway bootstrap-key 2>/dev/null)" && [[ -n "$out" ]]; then
    echo "  -> bootstrapped a new admin key"
  else
    echo "  -> tenant 'default' already has an admin key; minting another with --force"
    out="$("${PROM_COMPOSE[@]}" exec -T gateway /gateway bootstrap-key --force 2>/dev/null)" || true
    if [[ -z "$out" ]]; then
      fail "could not bootstrap an admin API key"
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
    fail "connector creation did not return an id"
  fi
  echo "  -> created connector $CONNECTOR_ID"
fi

echo "check: connector tool discovery"
discover_resp="$(curl -sSf -X POST "$CONTROL_BASE/connectors/$CONNECTOR_ID/discover" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY")"
echo "  -> discovered $(jq -r '.total' <<<"$discover_resp") tool(s)"

# ---------------------------------------------------------------------------
# 6. MCP traffic: initialize -> tools/list -> tools/call (x3)
# ---------------------------------------------------------------------------

HEADERS_TMP="$(mktemp)"
trap 'rm -f "$HEADERS_TMP"' EXIT

SESSION_ID=""
MCP_RESP=""

# mcp_call POSTs one JSON-RPC request to $MCP_URL, authenticated, carrying
# the session id from a prior initialize (once one exists). The response
# body is left in MCP_RESP. Every non-auth failure the gateway itself
# produces is HTTP 200 with a JSON-RPC error object, so both are checked.
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
    fail "$label got HTTP $http_code: $resp"
  fi
  if jq -e '.error' >/dev/null 2>&1 <<<"$resp"; then
    fail "$label returned a JSON-RPC error: $(jq -c '.error' <<<"$resp")"
  fi

  local new_session
  new_session="$(grep -i '^Mcp-Session-Id:' "$HEADERS_TMP" | tail -n1 | sed -E 's/^[^:]+:[[:space:]]*//' | tr -d '\r\n' || true)"
  if [[ -n "$new_session" ]]; then
    SESSION_ID="$new_session"
  fi
  MCP_RESP="$resp"
}

echo "check: MCP initialize"
mcp_call "initialize" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"14-prometheus","version":"0.1.0"}}}'
[[ -n "$SESSION_ID" ]] || fail "initialize did not return an Mcp-Session-Id header"
echo "  -> session=$SESSION_ID"

echo "check: MCP tools/list"
mcp_call "tools/list" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
if ! jq -e '[.result.tools[].name] | index("everything__echo")' >/dev/null 2>&1 <<<"$MCP_RESP"; then
  fail "tools/list did not include everything__echo"
fi
echo "  -> $(jq '[.result.tools[] | select(.name | startswith("everything__"))] | length' <<<"$MCP_RESP") tool(s) under 'everything'"

CALLS=3
for i in $(seq 1 "$CALLS"); do
  echo "check: MCP tools/call everything__echo ($i/$CALLS)"
  mcp_call "tools/call" "$(jq -nc --argjson id "$((10 + i))" --arg msg "metrics $i" \
    '{"jsonrpc":"2.0","id":$id,"method":"tools/call","params":{"name":"everything__echo","arguments":{"message":$msg}}}')"
  echo_text="$(jq -r '.result.content[0].text // empty' <<<"$MCP_RESP")"
  [[ "$echo_text" == *"metrics $i"* ]] || fail "tools/call did not echo the message back. Got: $echo_text"
done

# One rejected credential, so gateway_auth_failures_total has something to
# show. The key has the gateway's "gk_" shape: a request with no credential
# (or one that is not shaped like a gateway key) is not an auth failure.
# A single failure stays well under the lockout threshold.
echo "check: one request with an unknown key (expect 401)"
code="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer gk_not_a_real_key_0000000000000000" \
  -H "Content-Type: application/json" -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' "$MCP_URL")"
[[ "$code" == "401" ]] || fail "unknown key got HTTP $code, expected 401"
echo "  -> 401"

# ---------------------------------------------------------------------------
# 7. the raw /metrics endpoint
# ---------------------------------------------------------------------------

echo "check: gateway /metrics ($METRICS_URL)"
metrics_body="$(curl -sSf "$METRICS_URL")" || fail "could not GET $METRICS_URL"

assert_series() {
  local desc="$1" pattern="$2" line
  line="$(grep -E "$pattern" <<<"$metrics_body" | head -n1 || true)"
  [[ -n "$line" ]] || fail "/metrics has no series matching: $desc ($pattern)"
  echo "  -> $line"
}

assert_series "build info" '^gateway_build_info\{'
assert_series "MCP-plane HTTP requests" '^gateway_http_requests_total\{[^}]*plane="mcp"'
assert_series "API-plane HTTP requests" '^gateway_http_requests_total\{[^}]*plane="api"'
assert_series "tool calls for everything__echo" '^gateway_mcp_tool_calls_total\{[^}]*connector="everything"[^}]*tool="everything__echo"'
assert_series "MCP requests by method" '^gateway_mcp_requests_total\{[^}]*method="tools/call"'
assert_series "request-duration histogram" '^gateway_http_request_duration_seconds_bucket\{'
assert_series "auth failures on the MCP plane" '^gateway_auth_failures_total\{[^}]*plane="mcp"[^}]*\} [1-9]'
assert_series "database connections" '^gateway_db_connections\{'
assert_series "Go runtime" '^go_goroutines '

# The tool-call counter must have counted at least our CALLS calls.
tool_calls="$(awk -v p='^gateway_mcp_tool_calls_total\\{.*tool="everything__echo"' '$0 ~ p {s += $NF} END {print s+0}' <<<"$metrics_body")"
if ! awk -v n="$tool_calls" -v want="$CALLS" 'BEGIN {exit !(n >= want)}'; then
  fail "gateway_mcp_tool_calls_total{tool=\"everything__echo\"} is $tool_calls, expected at least $CALLS"
fi
echo "  -> gateway_mcp_tool_calls_total for everything__echo = $tool_calls (>= $CALLS)"

# ---------------------------------------------------------------------------
# 8. Prometheus: target up, series scraped, alert rules loaded
# ---------------------------------------------------------------------------

# prom_query prints the .data.result array of an instant query.
prom_query() {
  curl -sSf -G "$PROMETHEUS_URL/api/v1/query" --data-urlencode "query=$1" | jq -c '.data.result'
}

# wait_for_prom_series polls (the scrape interval is 5s) until the query
# returns at least one series, up to 60s.
wait_for_prom_series() {
  local desc="$1" query="$2" result=""
  echo "check: Prometheus has $desc  [$query]"
  for _ in $(seq 1 60); do
    result="$(prom_query "$query" 2>/dev/null || true)"
    if [[ -n "$result" && "$result" != "[]" ]]; then
      echo "  -> $(jq -c '.[0]' <<<"$result")"
      return 0
    fi
    sleep 1
  done
  fail "Prometheus returned no series for $desc ($query) within 60s"
}

echo "check: Prometheus is ready"
for _ in $(seq 1 60); do
  curl -sSf "$PROMETHEUS_URL/-/ready" >/dev/null 2>&1 && break
  sleep 1
done
curl -sSf "$PROMETHEUS_URL/-/ready" >/dev/null 2>&1 || fail "Prometheus never became ready at $PROMETHEUS_URL"

echo "check: Prometheus target up{job=\"gateway\"} == 1"
up_ok=false
for _ in $(seq 1 60); do
  up_val="$(prom_query 'up{job="gateway"}' 2>/dev/null | jq -r '.[0].value[1] // empty' 2>/dev/null || true)"
  if [[ "$up_val" == "1" ]]; then
    up_ok=true
    break
  fi
  sleep 1
done
[[ "$up_ok" == true ]] || fail "up{job=\"gateway\"} never reached 1 within 60s (is gateway:9464 reachable from the prometheus container?)"
echo "  -> up{job=\"gateway\"} = 1"

wait_for_prom_series "gateway_http_requests_total" 'gateway_http_requests_total'
wait_for_prom_series "tool calls for everything__echo" 'gateway_mcp_tool_calls_total{tool="everything__echo"}'

echo "check: the alert rules are loaded"
rules_json="$(curl -sSf "$PROMETHEUS_URL/api/v1/rules")" || fail "could not read $PROMETHEUS_URL/api/v1/rules"
for alert in GatewayLLMServerErrors GatewayDetectionTurnsDropped GatewayDatabasePoolWaiting GatewayAuthFailureBurst; do
  if ! jq -e --arg a "$alert" '[.data.groups[].rules[] | select(.name == $a and .health == "ok")] | length > 0' >/dev/null <<<"$rules_json"; then
    fail "alert rule $alert is missing or unhealthy in Prometheus"
  fi
  echo "  -> $alert: ok"
done

# Every PromQL expression the dashboard uses must run against this
# Prometheus, and the panels whose series this traffic creates must not be
# empty. (LLM, cost and detection panels need LLM traffic or a detection
# agent, so they are only checked for a valid query.)
echo "check: every dashboard query runs against Prometheus"
queries_ok=0
while IFS=$'\t' read -r panel_id title expr; do
  # $__rate_interval is Grafana's; give Prometheus a fixed window.
  query="${expr//\$__rate_interval/1m}"
  # rate() needs two scrapes inside the window; allow time for them on the
  # panels that must have data.
  tries=1
  case "$panel_id" in 1|3|6|8|9) tries=60 ;; esac
  for attempt in $(seq 1 "$tries"); do
    resp="$(curl -sS -G "$PROMETHEUS_URL/api/v1/query" --data-urlencode "query=$query")" \
      || fail "dashboard panel '$title': request to Prometheus failed"
    [[ "$(jq -r '.status' <<<"$resp")" == "success" ]] \
      || fail "dashboard panel '$title': query failed: $(jq -r '.error // .' <<<"$resp")  [$query]"
    [[ "$tries" -eq 1 || "$(jq '.data.result | length' <<<"$resp")" -gt 0 ]] && break
    [[ "$attempt" -lt "$tries" ]] && sleep 1
  done
  if [[ "$tries" -gt 1 && "$(jq '.data.result | length' <<<"$resp")" -eq 0 ]]; then
    fail "dashboard panel '$title' returned no series within 60s  [$query]"
  fi
  queries_ok=$((queries_ok + 1))
done < <(jq -r '.panels[] | .id as $id | .title as $t | .targets[] | [$id, $t, .expr] | @tsv' "$DASHBOARD_FILE")
echo "  -> $queries_ok dashboard queries ran; panels 1, 3, 6, 8 and 9 have data"

# ---------------------------------------------------------------------------
# 9. Grafana: healthy, dashboard and datasource provisioned, queries work
# ---------------------------------------------------------------------------

echo "check: Grafana health ($GRAFANA_URL/api/health)"
grafana_ok=false
for _ in $(seq 1 90); do
  gh="$(curl -sSf "$GRAFANA_URL/api/health" 2>/dev/null || true)"
  if [[ "$(jq -r '.database // empty' <<<"${gh:-null}" 2>/dev/null || true)" == "ok" ]]; then
    grafana_ok=true
    break
  fi
  sleep 1
done
[[ "$grafana_ok" == true ]] || fail "Grafana never reported database ok at $GRAFANA_URL/api/health"
echo "  -> $gh"

echo "check: Grafana finds the dashboard as an anonymous viewer (/api/search?query=Gateway)"
dash_ok=false
for _ in $(seq 1 30); do
  search="$(curl -sSf -G "$GRAFANA_URL/api/search" --data-urlencode "query=Gateway" 2>/dev/null || true)"
  if jq -e '[.[]? | select(.type == "dash-db" and .title == "AI Gateway")] | length == 1' >/dev/null 2>&1 <<<"${search:-null}"; then
    dash_ok=true
    break
  fi
  sleep 1
done
[[ "$dash_ok" == true ]] || fail "Grafana search did not return the 'AI Gateway' dashboard. Got: ${search:-nothing}"
echo "  -> $(jq -c '[.[] | {title, uid}]' <<<"$search")"

echo "check: Grafana has the Prometheus datasource (/api/datasources, admin login)"
datasources="$(curl -sSf -K <(printf 'user = "%s:%s"\n' "$GRAFANA_USER" "$GRAFANA_PASSWORD") "$GRAFANA_URL/api/datasources")" || fail "could not read $GRAFANA_URL/api/datasources"
if ! jq -e '[.[] | select(.type == "prometheus" and .name == "Prometheus")] | length == 1' >/dev/null <<<"$datasources"; then
  fail "Grafana has no Prometheus datasource. Got: $(jq -c '[.[] | {name, type}]' <<<"$datasources")"
fi
echo "  -> $(jq -c '[.[] | {name, type, url}]' <<<"$datasources")"

echo "check: a query through Grafana's datasource proxy returns gateway data"
via_grafana="$(curl -sSf -G "$GRAFANA_URL/api/datasources/proxy/uid/prometheus/api/v1/query" \
  --data-urlencode 'query=sum(gateway_http_requests_total)')" || fail "Grafana could not query Prometheus through the datasource"
total="$(jq -r '.data.result[0].value[1] // empty' <<<"$via_grafana")"
[[ -n "$total" ]] || fail "query through Grafana returned no data: $via_grafana"
echo "  -> sum(gateway_http_requests_total) = $total"

# ---------------------------------------------------------------------------
# summary + teardown
# ---------------------------------------------------------------------------

echo
echo "================================================================"
echo "14-prometheus passed."
if [[ "${KEEP_STACK:-}" == "1" ]]; then
  echo "KEEP_STACK=1: the stack is still running."
  echo "Metrics:     $METRICS_URL"
  echo "Prometheus:  $PROMETHEUS_URL"
  echo "Grafana:     $GRAFANA_URL  (dashboard 'AI Gateway')"
  echo "Console:     http://localhost:8081"
  echo "Admin key:   $GATEWAY_ADMIN_KEY"
  echo "Stop it with: ${PROM_COMPOSE[*]} down"
else
  echo "Tearing the stack down (volumes are kept)."
  "${PROM_COMPOSE[@]}" down
fi
echo "================================================================"
