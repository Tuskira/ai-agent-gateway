#!/usr/bin/env bash
#
# 06-credentials-and-headers: store a backend API key ONCE, as a
# credential, and have the gateway inject it into a connector's outbound
# calls -- proving the console/API only ever show "***" for it while the
# backend receives the real value. Also shows a token_field header (the
# caller's own identity, forwarded to the backend), a static header, and
# a per-tool tool_arg_overrides that the gateway stamps onto every call,
# overwriting whatever argument the caller supplied.
#
# Self-contained: does not assume 01-quickstart or 05-profiles has been
# run first, but reuses whatever they already left running (the compose
# stack, an admin key) instead of duplicating it.
#
# Safe to re-run: every step checks current state first and reuses or
# rotates it instead of failing or duplicating work.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.yml"

ECHO_PORT=23015
ECHO_PID_FILE="$SCRIPT_DIR/.header-echo.pid"
ECHO_LOG_FILE="$SCRIPT_DIR/.header-echo.log"

CONTROL_BASE="http://localhost:8081/api/v1"
MCP_URL="http://localhost:8080/mcp"
CONSOLE_URL="http://localhost:8081"

CREDENTIAL_NAME="06-backend-key"
CONNECTOR_SLUG="header-echo"

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

echo "check: required tools (docker, curl, jq, python3)"
require_cmd docker "Install Docker Desktop or Docker Engine."
require_cmd curl "Install curl."
require_cmd jq "Install jq (e.g. 'brew install jq' or 'apt-get install jq')."
require_cmd python3 "Install Python 3 -- it runs the tiny header-echo MCP server this example registers."
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
# 1. start the header-echo MCP server -- the connector's backend
# ---------------------------------------------------------------------------

if port_open "$ECHO_PORT"; then
  echo "check: header-echo MCP server already listening on :$ECHO_PORT -- reusing it"
else
  echo "check: starting header-echo MCP server on :$ECHO_PORT"
  python3 "$SCRIPT_DIR/header-echo-server.py" "$ECHO_PORT" >"$ECHO_LOG_FILE" 2>&1 &
  echo $! >"$ECHO_PID_FILE"

  ready=false
  for _ in $(seq 1 30); do
    if port_open "$ECHO_PORT"; then
      ready=true
      break
    fi
    sleep 1
  done
  if [[ "$ready" != true ]]; then
    echo "FAIL: header-echo MCP server did not start listening on :$ECHO_PORT within 30s" >&2
    echo "---- last 50 lines of $ECHO_LOG_FILE ----" >&2
    tail -n 50 "$ECHO_LOG_FILE" >&2 || true
    exit 1
  fi
  echo "  -> listening on :$ECHO_PORT (pid $(cat "$ECHO_PID_FILE"))"
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
  # this script (or of 01-quickstart/05-profiles against the same stack)
  # hits that case, and the earlier key's plaintext is gone, so mint
  # another one.
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
# 5. store the backend's API key ONCE, as a credential
# ---------------------------------------------------------------------------
#
# POST /credentials is not idempotent by name: a second POST for a name
# already in the tenant is a 409 (internal/secrets/store.go's Create
# wraps a unique-constraint violation as store.ErrConflict, mapped to 409
# by internal/api/handlers/errors.go's writeStoreErr). A re-run of this
# script rotates the existing credential's payload (PUT) instead of
# failing.
#
# The value is a per-run placeholder, never a real secret -- generated
# fresh every run so "the backend saw the exact value we stored" actually
# proves something instead of matching a hardcoded string by accident.

BACKEND_KEY_VALUE="demo-$(date +%s)"

echo "check: storing the backend's API key as credential '$CREDENTIAL_NAME'"
credential_body="$(jq -n --arg name "$CREDENTIAL_NAME" --arg v "$BACKEND_KEY_VALUE" \
  '{name: $name, type: "api_key", payload: {api_key: $v}}')"

create_status="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$CONTROL_BASE/credentials" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d "$credential_body")"

case "$create_status" in
  201)
    echo "  -> created (field: api_key)"
    ;;
  409)
    echo "  -> already exists; rotating its payload"
    curl -sSf -X PUT "$CONTROL_BASE/credentials/$CREDENTIAL_NAME" \
      -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
      -d "$(jq -n --arg v "$BACKEND_KEY_VALUE" '{payload: {api_key: $v}}')" >/dev/null
    echo "  -> rotated (field: api_key)"
    ;;
  *)
    echo "FAIL: credential create got unexpected HTTP $create_status" >&2
    exit 1
    ;;
esac

# ---------------------------------------------------------------------------
# 6. register (or update) the header-echo connector
# ---------------------------------------------------------------------------
#
# Three header types on one connector, plus a per-tool argument override:
#   X-Backend-Key  external / secret_store -> credential $CREDENTIAL_NAME, field "api_key"
#   X-Caller       token_field / subject   -> the calling principal's own id
#   X-Static-Env   static                  -> a fixed value ("demo")
#   tool_arg_overrides."echo-args".region  -> stamped onto every echo-args
#                                              call, overwriting whatever
#                                              the caller sent for "region"
#
# PUT /connectors/{id} is a full replace of name/endpoint/timeout_ms, and
# metadata is only touched when sent -- so re-sending this same body is
# naturally idempotent (docs/connectors-and-credentials.md).

connector_body="$(jq -n \
  --arg name "Header Echo" \
  --arg slug "$CONNECTOR_SLUG" \
  --arg endpoint "http://host.docker.internal:$ECHO_PORT/mcp" \
  --arg credential "$CREDENTIAL_NAME" \
  '{
    name: $name,
    slug: $slug,
    endpoint: $endpoint,
    metadata: {
      headers: {
        "X-Backend-Key": {type: "external", provider: "secret_store",
                           config: {credential: $credential, field: "api_key"}},
        "X-Caller":      {type: "token_field", field: "subject"},
        "X-Static-Env":  {type: "static", value: "demo"}
      },
      tool_arg_overrides: {
        "echo-args": {region: "eu-west-1"}
      }
    }
  }')"

echo "check: looking for an existing '$CONNECTOR_SLUG' connector"
CONNECTOR_ID="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/connectors?limit=500" | jq -r --arg s "$CONNECTOR_SLUG" '.items[] | select(.slug==$s) | .id' | head -n1)"

if [[ -n "$CONNECTOR_ID" ]]; then
  echo "  -> updating existing connector $CONNECTOR_ID"
  curl -sSf -X PUT "$CONTROL_BASE/connectors/$CONNECTOR_ID" \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
    -d "$connector_body" >/dev/null
else
  echo "check: registering the '$CONNECTOR_SLUG' connector"
  CONNECTOR_ID="$(curl -sSf -X POST "$CONTROL_BASE/connectors" \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
    -d "$connector_body" | jq -r '.id')"
fi
if [[ -z "$CONNECTOR_ID" || "$CONNECTOR_ID" == "null" ]]; then
  echo "FAIL: connector create/update did not return an id" >&2
  exit 1
fi
echo "  -> connector id $CONNECTOR_ID"

# ---------------------------------------------------------------------------
# 7. connector health check + discover
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
echo "  -> discovered $discovered_count tool(s)"
if [[ "$discovered_count" != "2" ]]; then
  echo "FAIL: expected exactly 2 tools (headers, echo-args), got $discovered_count" >&2
  exit 1
fi
if ! jq -e '[.items[].tool_name] | index("headers")' >/dev/null 2>&1 <<<"$discover_resp"; then
  echo "FAIL: discovery did not include the 'headers' tool" >&2
  exit 1
fi
if ! jq -e '[.items[].tool_name] | index("echo-args")' >/dev/null 2>&1 <<<"$discover_resp"; then
  echo "FAIL: discovery did not include the 'echo-args' tool" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 8. the console/API show the secret masked, never plain
# ---------------------------------------------------------------------------
#
# maskConnectorMetadata (internal/api/handlers/connectors.go) replaces
# EVERY "static" header's value, and every value inside an "external"
# header's "config" object, with "***" -- uniformly, not just for values
# that are actually secret. That's why X-Static-Env's plain, non-secret
# "demo" also reads back as "***" below: the mask doesn't try to guess
# which config is sensitive, it treats all of it that way.

echo "check: console/API mask the credential reference and the static value"
connector_view="$(curl -sSf "$CONTROL_BASE/connectors/$CONNECTOR_ID" -H "Authorization: Bearer $GATEWAY_ADMIN_KEY")"

backend_key_cfg="$(jq -c '.metadata.headers["X-Backend-Key"]' <<<"$connector_view")"
echo "  -> X-Backend-Key: $backend_key_cfg"
if [[ "$(jq -r '.config.credential' <<<"$backend_key_cfg")" != "***" ]] || \
   [[ "$(jq -r '.config.field' <<<"$backend_key_cfg")" != "***" ]]; then
  echo "FAIL: expected X-Backend-Key's external config to read {\"credential\":\"***\",\"field\":\"***\"}, got $backend_key_cfg" >&2
  exit 1
fi

static_cfg="$(jq -c '.metadata.headers["X-Static-Env"]' <<<"$connector_view")"
echo "  -> X-Static-Env: $static_cfg (masked too -- see note above)"
if [[ "$(jq -r '.value' <<<"$static_cfg")" != "***" ]]; then
  echo "FAIL: expected X-Static-Env's value to read \"***\", got $static_cfg" >&2
  exit 1
fi

if grep -qF "$BACKEND_KEY_VALUE" <<<"$connector_view"; then
  echo "FAIL: the generated secret value leaked into GET /connectors/$CONNECTOR_ID's response" >&2
  exit 1
fi
echo "  -> generated secret ($BACKEND_KEY_VALUE) is absent from the API response"

# ---------------------------------------------------------------------------
# 9. MCP handshake
# ---------------------------------------------------------------------------

HEADERS_TMP="$(mktemp)"
trap 'rm -f "$HEADERS_TMP"' EXIT

SESSION_ID=""
MCP_RESP=""

# mcp_call POSTs one JSON-RPC request to $MCP_URL, authenticated as the
# admin key, carrying the session id from a prior initialize (once one
# exists). The response body is left in MCP_RESP (not echoed: a $(...)
# capture would run this in a subshell and lose the SESSION_ID it
# records). It fails loudly on a non-200 HTTP status or a JSON-RPC-level
# "error" in the response -- per internal/dataplane/transport/http.go,
# every non-auth failure the gateway itself produces is still HTTP 200
# with a JSON-RPC error object, so the HTTP status alone can't tell
# success from failure. See 01-quickstart/run.sh for the same helper.
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

# mcp_notify is for a JSON-RPC notification (no "id"): the gateway
# answers with an empty HTTP 204, never a JSON-RPC body (see http.go's
# rpc handler: "A notification gets no body at all, per JSON-RPC.").
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
mcp_call "initialize" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"06-credentials-and-headers","version":"0.1.0"}}}'
echo "  -> protocolVersion=$(jq -r '.result.protocolVersion' <<<"$MCP_RESP"), session=$SESSION_ID"
if [[ -z "$SESSION_ID" ]]; then
  echo "FAIL: initialize did not return an Mcp-Session-Id header" >&2
  exit 1
fi

echo "check: MCP notifications/initialized"
mcp_notify "notifications/initialized" '{"jsonrpc":"2.0","method":"notifications/initialized"}'

# ---------------------------------------------------------------------------
# 10. tools/list -- both tools advertised, "region" scrubbed from echo-args
# ---------------------------------------------------------------------------

echo "check: MCP tools/list"
mcp_call "tools/list" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
if ! jq -e '[.result.tools[].name] | index("header-echo__headers")' >/dev/null 2>&1 <<<"$MCP_RESP"; then
  echo "FAIL: tools/list did not include header-echo__headers" >&2
  exit 1
fi
if ! jq -e '[.result.tools[].name] | index("header-echo__echo-args")' >/dev/null 2>&1 <<<"$MCP_RESP"; then
  echo "FAIL: tools/list did not include header-echo__echo-args" >&2
  exit 1
fi
echo "  -> both tools advertised"

echo "check: 'region' is scrubbed from echo-args' advertised schema (it's stamped server-side)"
echo_args_schema="$(jq -c '.result.tools[] | select(.name=="header-echo__echo-args") | .inputSchema' <<<"$MCP_RESP")"
if jq -e '.properties | has("region")' >/dev/null 2>&1 <<<"$echo_args_schema"; then
  echo "FAIL: echo-args' advertised inputSchema still lists 'region': $echo_args_schema" >&2
  exit 1
fi
if ! jq -e '.properties | has("q")' >/dev/null 2>&1 <<<"$echo_args_schema"; then
  echo "FAIL: echo-args' advertised inputSchema lost the untouched 'q' argument: $echo_args_schema" >&2
  exit 1
fi
echo "  -> region absent, q still advertised"

# ---------------------------------------------------------------------------
# 11. tools/call header-echo__headers -- proves header injection
# ---------------------------------------------------------------------------

# assert_header_equals looks up $1 in $headers_seen (the JSON object the
# backend echoed back) case-insensitively and fails unless it equals $2.
assert_header_equals() {
  local label="$1" want="$2"
  local got
  got="$(jq -r --arg k "$label" 'with_entries(.key |= ascii_downcase)[$k | ascii_downcase] // empty' <<<"$headers_seen")"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL: backend saw $label = '$got', want '$want'" >&2
    exit 1
  fi
  echo "  ok · $label = $got"
}

echo "check: MCP tools/call header-echo__headers"
mcp_call "tools/call headers" '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"header-echo__headers","arguments":{}}}'
headers_seen="$(jq -r '.result.content[0].text' <<<"$MCP_RESP")"

assert_header_equals "X-Backend-Key" "$BACKEND_KEY_VALUE"
assert_header_equals "X-Static-Env" "demo"

caller_seen="$(jq -r 'with_entries(.key |= ascii_downcase)["x-caller"] // empty' <<<"$headers_seen")"
if [[ -z "$caller_seen" ]]; then
  echo "FAIL: backend saw no X-Caller header (token_field/subject)" >&2
  exit 1
fi
echo "  ok · X-Caller = $caller_seen (the admin key's own principal id)"

# The caller authenticates to the GATEWAY with its own "Authorization:
# Bearer gk_..." -- that credential is never handed to a backend
# connector. internal/dataplane/client/client.go's exchange() builds the
# outbound request from scratch (newRequest, http.NewRequestWithContext)
# and only ever sets Content-Type, Accept, the protocol-version header, a
# traceparent when the call carries a trace, the connector's own
# configured headers, then the gateway-owned
# X-Tenant-Id/Mcp-Session-Id -- there is no code path that copies the
# inbound Authorization header onto it.
if jq -e 'with_entries(.key |= ascii_downcase) | has("authorization")' >/dev/null 2>&1 <<<"$headers_seen"; then
  echo "FAIL: the caller's own Authorization header was forwarded to the backend -- it should never be" >&2
  exit 1
fi
echo "  ok · Authorization was NOT forwarded to the backend"

# ---------------------------------------------------------------------------
# 12. tools/call header-echo__echo-args -- proves the argument override
# ---------------------------------------------------------------------------

echo "check: MCP tools/call header-echo__echo-args"
mcp_call "tools/call echo-args" '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"header-echo__echo-args","arguments":{"region":"us-east-1","q":"x"}}}'
args_seen="$(jq -r '.result.content[0].text' <<<"$MCP_RESP")"
region_seen="$(jq -r '.region' <<<"$args_seen")"
q_seen="$(jq -r '.q' <<<"$args_seen")"

if [[ "$region_seen" != "eu-west-1" ]]; then
  echo "FAIL: expected tool_arg_overrides to force region=eu-west-1 (caller sent us-east-1), backend saw '$region_seen'" >&2
  exit 1
fi
if [[ "$q_seen" != "x" ]]; then
  echo "FAIL: expected the untouched argument q=x to pass through unchanged, backend saw '$q_seen'" >&2
  exit 1
fi
echo "  ok · region = $region_seen (server-side override beat the caller's 'us-east-1')"
echo "  ok · q      = $q_seen (untouched argument passed through)"

# ---------------------------------------------------------------------------
# 13. a masked round trip does not corrupt the stored secret reference
# ---------------------------------------------------------------------------
#
# $connector_view (step 8) is exactly what a console "edit connector"
# screen would have: credential/field/value already replaced by "***".
# PUT-ing it back unchanged must NOT store the literal "***" -- Update
# (internal/api/handlers/connectors.go) runs unmaskHeaderConfigs first,
# which restores every "***" from the connector's own current metadata
# before saving. If that ever regressed, the backend would start seeing
# a broken credential reference (or "***" itself) instead of the real
# secret on the very next call.

echo "check: a masked round trip (GET, then PUT back unchanged)"
roundtrip_body="$(jq '{name, endpoint, timeout_ms, metadata, slug} + (if .description then {description} else {} end)' <<<"$connector_view")"
curl -sSf -X PUT "$CONTROL_BASE/connectors/$CONNECTOR_ID" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d "$roundtrip_body" >/dev/null
echo "  -> PUT the masked body back unchanged"

mcp_call "tools/call headers (after masked round trip)" '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"header-echo__headers","arguments":{}}}'
headers_seen="$(jq -r '.result.content[0].text' <<<"$MCP_RESP")"
assert_header_equals "X-Backend-Key" "$BACKEND_KEY_VALUE"
echo "  -> the stored secret survived the round trip -- the backend still sees the real value, not \"***\""

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

echo
echo "================================================================"
echo "06-credentials-and-headers passed."
echo "Console:     $CONSOLE_URL"
echo "Admin key:   $GATEWAY_ADMIN_KEY"
echo "Credential:  $CREDENTIAL_NAME (masked in the API/UI, plain to the backend)"
echo "Connector:   $CONNECTOR_SLUG (id $CONNECTOR_ID)"
echo "================================================================"
