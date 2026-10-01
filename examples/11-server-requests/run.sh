#!/usr/bin/env bash
#
# 11-server-requests: a connector asks the agent for input mid-call, and
# the gateway relays it. Registers the @modelcontextprotocol/server-everything
# reference MCP server as its own connector ("everything-requests") with
# metadata.server_requests.elicitation and .roots turned on, initializes
# an MCP session that DECLARES the elicitation and roots capabilities,
# holds the session's GET /mcp/stream open with curl in the background,
# and then:
#
#   - calls everything-requests__trigger-elicitation-request, reads the
#     relayed elicitation/create off the stream, answers it with a
#     JSON-RPC response POSTed to /mcp, and checks the tool result carries
#     the answer
#   - calls everything-requests__get-roots-list and answers roots/list the
#     same way
#   - turns the connector's elicitation policy off and shows the same
#     call refused (-32601) without anything reaching the agent
#
# Self-contained: does not assume 01-quickstart has been run first, but
# reuses whatever it already left running (the "everything" MCP server,
# the compose stack).
#
# This example registers its OWN connector ("everything-requests") on
# that same "everything" MCP server rather than reusing the "everything"
# connector 01/05/08 share, so that turning its elicitation policy on and
# off (step 10) never touches theirs. Unlike "everything" (shared,
# left running) or "header-echo" (06 owns it alone but keeps it running
# for a future re-run), this connector is torn down at the end of every
# run instead (see cleanup() below): it exists only to prove the
# elicitation/roots round trip, and a second connector left sitting on
# the same backend would grow the tenant's tool-list count for every
# OTHER example run afterwards (05-profiles' no-header tools/list check,
# in particular, assumes "everything" is the only connector in the
# tenant -- see that script's own note).
#
# Slugs are unique among live connectors only
# (internal/store/postgres/migrations/000009_live_slugs.up.sql), so the
# next run re-creates it under the same slug.
#
# Safe to re-run: every step checks current state first and reuses it
# instead of failing or duplicating work; the connector's policy is put
# back ON at the start of every run (a fresh connector is always created
# ON, so this only matters if a prior run's cleanup didn't get to run).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.yml"

EVERYTHING_PORT=23014
EVERYTHING_PID_FILE="$SCRIPT_DIR/.everything.pid"
EVERYTHING_LOG_FILE="$SCRIPT_DIR/.everything.log"

CONTROL_BASE="http://localhost:8081/api/v1"
MCP_URL="http://localhost:8080/mcp"
STREAM_URL="http://localhost:8080/mcp/stream"
CONSOLE_URL="http://localhost:8081"

CONNECTOR_NAME="everything-requests"
CONNECTOR_SLUG="everything-requests"

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
# e2e run caught exactly this -- this example runs right after
# examples/08-observability in `make examples-smoke` (see the Makefile), so
# it hit the bug directly. A running "clickhouse" container (only ever
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
# 5. register (or update) the "everything-requests" connector, policy ON
# ---------------------------------------------------------------------------
#
# A connector of its own, so turning its policy on and off never touches
# the "everything" connector other examples share. Same server, same
# tools, qualified "everything-requests__<tool>".
#
# PUT /connectors/{id} is a full replace of name/endpoint/timeout_ms, and
# metadata is only touched when sent -- so re-sending this same body is
# naturally idempotent (docs/connectors-and-credentials.md). It is sent on
# every run, which is also what puts the policy back on after step 11
# turned elicitation off.

# connector_body renders the connector with the given
# metadata.server_requests object.
connector_body() {
  jq -n --arg name "$CONNECTOR_NAME" --arg slug "$CONNECTOR_SLUG" --argjson sr "$1" '{
    name: $name,
    slug: $slug,
    endpoint: "http://host.docker.internal:23014/mcp",
    timeout_ms: 30000,
    metadata: {server_requests: $sr}
  }'
}
POLICY_ON='{"elicitation": true, "roots": true}'
POLICY_NO_ELICITATION='{"elicitation": false, "roots": true}'

echo "check: looking for an existing '$CONNECTOR_SLUG' connector"
CONNECTOR_ID="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/connectors?limit=500" | jq -r --arg s "$CONNECTOR_SLUG" '.items[] | select(.slug==$s) | .id' | head -n1)"

if [[ -n "$CONNECTOR_ID" ]]; then
  echo "  -> updating existing connector $CONNECTOR_ID (server_requests: $POLICY_ON)"
  curl -sSf -X PUT "$CONTROL_BASE/connectors/$CONNECTOR_ID" \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
    -d "$(connector_body "$POLICY_ON")" >/dev/null
else
  echo "check: registering the '$CONNECTOR_SLUG' connector (server_requests: $POLICY_ON)"
  CONNECTOR_ID="$(curl -sSf -X POST "$CONTROL_BASE/connectors" \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
    -d "$(connector_body "$POLICY_ON")" | jq -r '.id')"
  if [[ -z "$CONNECTOR_ID" || "$CONNECTOR_ID" == "null" ]]; then
    echo "FAIL: connector creation did not return an id" >&2
    exit 1
  fi
  echo "  -> created connector $CONNECTOR_ID"
fi

stored_policy="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/connectors/$CONNECTOR_ID" | jq -c '.metadata.server_requests')"
echo "  -> stored metadata.server_requests = $stored_policy"
if [[ "$(jq -r '.elicitation' <<<"$stored_policy")" != "true" ]]; then
  echo "FAIL: the connector's elicitation policy is not on" >&2
  exit 1
fi

echo "check: connector health probe"
health_resp="$(curl -sSf -X GET "$CONTROL_BASE/connectors/$CONNECTOR_ID/health" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY")"
echo "  -> $health_resp"
if [[ "$(jq -r '.status' <<<"$health_resp")" != "healthy" ]]; then
  echo "FAIL: connector health is '$(jq -r '.status' <<<"$health_resp")', expected 'healthy'" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 6. MCP initialize, declaring the elicitation and roots capabilities
# ---------------------------------------------------------------------------

WORK_DIR="$(mktemp -d)"
HEADERS_TMP="$WORK_DIR/headers"
STREAM_FILE="$WORK_DIR/stream"
STREAM_PID_FILE="$WORK_DIR/stream.pid"
CALL_FILE="$WORK_DIR/call"
CALL_PID_FILE="$WORK_DIR/call.pid"

# cleanup stops the background curls this script started (the stream and
# any tools/call still waiting) by the PIDs it recorded, removes the
# scratch directory, and deletes the "$CONNECTOR_SLUG"
# connector -- unlike "everything" (shared) or "header-echo" (owned but
# left running for a re-run), this connector exists only to prove the
# elicitation/roots round trip and must not linger: a second connector on
# the same "everything" MCP server would grow the tenant's tool-list count
# for every OTHER example run afterwards (05-profiles' no-header
# tools/list check, in particular, assumes "everything" is the only
# connector in the tenant -- see that script's own note). Best-effort and
# always attempted, on both the happy path and any failure, so a stack
# left after `make examples-smoke` looks exactly like it did before this
# example ran. The "everything" MCP server and the compose stack
# themselves are left running, as every example leaves them.
cleanup() {
  local pid_file pid
  for pid_file in "$CALL_PID_FILE" "$STREAM_PID_FILE"; do
    if [[ -f "$pid_file" ]]; then
      pid="$(cat "$pid_file")"
      kill "$pid" 2>/dev/null || true
      # Reap it quietly, so bash does not report the job as terminated.
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$WORK_DIR"
  if [[ -n "${CONNECTOR_ID:-}" ]]; then
    curl -sS -o /dev/null -X DELETE "$CONTROL_BASE/connectors/$CONNECTOR_ID" \
      -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
      || echo "WARN: failed to delete connector $CONNECTOR_ID ('$CONNECTOR_SLUG') -- remove it by hand" >&2
  fi
}
trap cleanup EXIT

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

echo "check: MCP initialize, declaring capabilities {elicitation, roots}"
mcp_call "initialize" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"elicitation":{},"roots":{"listChanged":true}},"clientInfo":{"name":"11-server-requests","version":"0.1.0"}}}'
echo "  -> protocolVersion=$(jq -r '.result.protocolVersion' <<<"$MCP_RESP"), session=$SESSION_ID"
if [[ -z "$SESSION_ID" ]]; then
  echo "FAIL: initialize did not return an Mcp-Session-Id header" >&2
  exit 1
fi

echo "check: MCP notifications/initialized"
mcp_notify "notifications/initialized" '{"jsonrpc":"2.0","method":"notifications/initialized"}'

# ---------------------------------------------------------------------------
# 7. hold the session's GET /mcp/stream open in the background
# ---------------------------------------------------------------------------
#
# Relayed requests arrive here, as JSON-RPC requests with a gateway id
# ("gw-<16 hex>"). The gateway subscribes the stream before it sends its
# headers, so once the 200 is in, nothing sent to the session is missed.

echo "check: opening GET /mcp/stream for the session (curl, in the background)"
curl -sSN -D "$WORK_DIR/stream.headers" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  "$STREAM_URL" >"$STREAM_FILE" 2>/dev/null &
echo $! >"$STREAM_PID_FILE"

stream_ready=false
for _ in $(seq 1 50); do
  if grep -q '^HTTP/[0-9.]* 200' "$WORK_DIR/stream.headers" 2>/dev/null; then
    stream_ready=true
    break
  fi
  sleep 0.2
done
if [[ "$stream_ready" != true ]]; then
  echo "FAIL: GET /mcp/stream did not answer 200 within 10s" >&2
  exit 1
fi
echo "  -> stream open (curl pid $(cat "$STREAM_PID_FILE"))"

# wait_for_request waits up to 15s for a relayed request of the given
# method to appear on the stream after line $2 of it, and leaves it in
# RELAYED (the JSON-RPC request) and RELAYED_ID (its gateway id).
wait_for_request() {
  local method="$1" after="$2"
  RELAYED=""
  for _ in $(seq 1 75); do
    RELAYED="$(tail -n +"$((after + 1))" "$STREAM_FILE" | sed -n 's/^data: //p' \
      | jq -c --arg m "$method" 'select(.method == $m and .id != null)' 2>/dev/null | head -n1 || true)"
    if [[ -n "$RELAYED" ]]; then
      RELAYED_ID="$(jq -r '.id' <<<"$RELAYED")"
      return 0
    fi
    sleep 0.2
  done
  echo "FAIL: no $method request reached the agent's stream within 15s" >&2
  exit 1
}

# call_tool_in_background starts a tools/call whose tool will wait for
# the agent's answer, so it cannot be a blocking mcp_call.
call_tool_in_background() {
  local body="$1"
  rm -f "$CALL_FILE"
  curl -sS \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -H "Mcp-Session-Id: $SESSION_ID" \
    -d "$body" \
    "$MCP_URL" >"$CALL_FILE" &
  echo $! >"$CALL_PID_FILE"
}

# wait_for_call waits up to 15s for the background tools/call and leaves
# its response in MCP_RESP.
wait_for_call() {
  local pid
  pid="$(cat "$CALL_PID_FILE")"
  for _ in $(seq 1 75); do
    if ! kill -0 "$pid" 2>/dev/null; then
      rm -f "$CALL_PID_FILE"
      MCP_RESP="$(cat "$CALL_FILE")"
      return 0
    fi
    sleep 0.2
  done
  echo "FAIL: the tools/call did not return within 15s" >&2
  exit 1
}

# answer POSTs the agent's JSON-RPC response for RELAYED_ID and requires
# the gateway's 202 Accepted with no body.
answer() {
  local result="$1" http_code
  http_code="$(curl -sS -o "$WORK_DIR/answer" -w '%{http_code}' \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
    -H "Content-Type: application/json" \
    -H "Mcp-Session-Id: $SESSION_ID" \
    -d "$(jq -nc --arg id "$RELAYED_ID" --argjson r "$result" '{jsonrpc: "2.0", id: $id, result: $r}')" \
    "$MCP_URL")"
  if [[ "$http_code" != "202" || -s "$WORK_DIR/answer" ]]; then
    echo "FAIL: the answer POST got HTTP $http_code ($(cat "$WORK_DIR/answer")), expected 202 with no body" >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# 8. elicitation: the tool asks, the agent answers, the tool gets it
# ---------------------------------------------------------------------------

echo "check: tools/call ${CONNECTOR_SLUG}__trigger-elicitation-request (runs until the agent answers)"
seen="$(wc -l <"$STREAM_FILE")"
call_tool_in_background "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"${CONNECTOR_SLUG}__trigger-elicitation-request\",\"arguments\":{}}}"

wait_for_request "elicitation/create" "$seen"
echo "  -> stream <- elicitation/create id=$RELAYED_ID message=$(jq -c '.params.message' <<<"$RELAYED")"
if [[ "$RELAYED_ID" != gw-* ]]; then
  echo "FAIL: the relayed request's id '$RELAYED_ID' is not a gateway id (gw-...)" >&2
  exit 1
fi

echo "check: answering the elicitation (POST /mcp with a JSON-RPC response)"
answer '{"action": "accept", "content": {"name": "Ada Lovelace", "check": true}}'
echo "  -> 202 Accepted"

wait_for_call
if jq -e '.error' >/dev/null 2>&1 <<<"$MCP_RESP"; then
  echo "FAIL: tools/call returned a JSON-RPC error: $(jq -c '.error' <<<"$MCP_RESP")" >&2
  exit 1
fi
elicit_text="$(jq -r '[.result.content[]?.text // empty] | join(" ")' <<<"$MCP_RESP")"
echo "  -> tool result: $(tr '\n' ' ' <<<"$elicit_text" | cut -c1-160)"
if [[ "$elicit_text" != *"Ada Lovelace"* ]]; then
  echo "FAIL: the tool result does not carry the agent's answer" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 9. roots: the same round trip for roots/list
# ---------------------------------------------------------------------------

echo "check: tools/call ${CONNECTOR_SLUG}__get-roots-list"
seen="$(wc -l <"$STREAM_FILE")"
call_tool_in_background "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"${CONNECTOR_SLUG}__get-roots-list\",\"arguments\":{}}}"

wait_for_request "roots/list" "$seen"
echo "  -> stream <- roots/list id=$RELAYED_ID"
answer '{"roots": [{"uri": "file:///work/11-server-requests", "name": "demo"}]}'
wait_for_call
roots_text="$(jq -r '[.result.content[]?.text // empty] | join(" ")' <<<"$MCP_RESP")"
echo "  -> tool result: $(tr '\n' ' ' <<<"$roots_text" | cut -c1-160)"
if [[ "$roots_text" != *"file:///work/11-server-requests"* ]]; then
  echo "FAIL: the tool result does not carry the agent's roots" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 10. policy off: the same elicitation is refused and never reaches the agent
# ---------------------------------------------------------------------------
#
# The policy is re-read on every request, so the session already open
# needs no re-initialize.

echo "check: turning the connector's elicitation policy off (server_requests: $POLICY_NO_ELICITATION)"
curl -sSf -X PUT "$CONTROL_BASE/connectors/$CONNECTOR_ID" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d "$(connector_body "$POLICY_NO_ELICITATION")" >/dev/null

echo "check: tools/call ${CONNECTOR_SLUG}__trigger-elicitation-request again (expect refusal)"
seen="$(wc -l <"$STREAM_FILE")"
call_tool_in_background "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"${CONNECTOR_SLUG}__trigger-elicitation-request\",\"arguments\":{}}}"
wait_for_call
refused_text="$(jq -r '[.result.content[]?.text // empty] | join(" ")' <<<"$MCP_RESP")"
echo "  -> tool result: $refused_text"
if [[ "$refused_text" != *"-32601"* || "$refused_text" != *"not permitted for this connector"* ]]; then
  echo "FAIL: expected the connector to be refused with -32601 'not permitted for this connector'" >&2
  exit 1
fi
if tail -n +"$((seen + 1))" "$STREAM_FILE" | grep -q '"elicitation/create"'; then
  echo "FAIL: a refused elicitation reached the agent's stream" >&2
  exit 1
fi
echo "  -> nothing reached the agent's stream"

echo "check: turning the elicitation policy back on"
curl -sSf -X PUT "$CONTROL_BASE/connectors/$CONNECTOR_ID" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d "$(connector_body "$POLICY_ON")" >/dev/null

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

echo
echo "================================================================"
echo "11-server-requests passed."
echo "Console:    $CONSOLE_URL  (Access Logs show one elicitation/create and"
echo "            one roots/list record for connector $CONNECTOR_SLUG --"
echo "            the connector itself is deleted on exit; see cleanup())"
echo "================================================================"
