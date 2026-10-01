#!/usr/bin/env bash
#
# 13-claude-desktop: the GATEWAY side of the Claude Desktop example -- no Mac,
# no Claude Desktop, no claude-desktop-utility needed. It sends the same kind
# of batch the utility sends (one gzipped POST /api/v1/ingest carrying one
# LLM call and one connector tool call), then proves what the console shows:
#
#   - a key with the built-in `interceptor` role can ingest; no key is 401
#     and an `agent` key is 403
#   - the first POST is stored, the identical re-POST is counted as a
#     duplicate (dedup by request_id), and nothing is stored twice
#   - /analytics/llm-logs and /analytics/logs return the rows with
#     source=interceptor and the user label
#   - /analytics/sessions/{id}/timeline merges both rows into one session
#
# Ingest needs two things the shipped compose file leaves off: the ClickHouse
# sink (analytics profile) and ingest.enabled. This script brings the stack up
# with docker-compose.ingest.yml for the second and restores the gateway to
# the config it found when it exits.
#
# Safe to re-run: the records use fixed request ids, so a second run reports
# the first POST as duplicates (the stored rows are the ones from run one).
# The two API keys it mints are revoked in the exit trap.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.yml"
INGEST_COMPOSE_FILE="$SCRIPT_DIR/docker-compose.ingest.yml"
# Only used to put the gateway back as examples/08-observability left it.
OBSERVABILITY_COMPOSE_FILE="$ROOT_DIR/examples/08-observability/docker-compose.observability.yml"

# Ingest is served on the API plane (the same port as the console and the
# control-plane API), not on the MCP or LLM plane.
CONTROL_BASE="http://localhost:8081/api/v1"

SESSION_ID="13-claude-desktop-example-session"
USER_LABEL="example.user@example.com"
LLM_REQUEST_ID="icp_13-example-llm-1"
TOOL_REQUEST_ID="icp_13-example-tool-1"

INGEST_COMPOSE=(docker compose -f "$COMPOSE_FILE" -f "$INGEST_COMPOSE_FILE" --profile analytics)

WORK_DIR="$(mktemp -d)"
ADMIN_KEY_MINTED=""
INTERCEPTOR_KEY_ID=""
AGENT_KEY_ID=""
STACK_CHANGED=false
WAS_CLICKHOUSE=false
WAS_OBSERVABILITY=false

wait_for_health_soft() {
  local url="$1"
  for _ in $(seq 1 60); do
    if curl -sSf "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "WARN: $url did not become healthy during cleanup" >&2
  return 0
}

revoke_key() {
  local id="$1"
  [[ -n "$id" && -n "${GATEWAY_ADMIN_KEY:-}" ]] || return 0
  curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
    "$CONTROL_BASE/api-keys/$id" || echo "WARN: could not revoke key $id" >&2
}

# restore_stack puts the gateway back the way this script found it: with
# ClickHouse and the OTel collector up (examples/08-observability's stack),
# with just the analytics profile, or the plain two-container quick start.
restore_stack() {
  [[ "$STACK_CHANGED" == true ]] || return 0
  if [[ "$WAS_OBSERVABILITY" == true ]]; then
    echo "cleanup: restoring the gateway to examples/08-observability's config (ingest off)"
    GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose -f "$COMPOSE_FILE" -f "$OBSERVABILITY_COMPOSE_FILE" --profile analytics up -d gateway \
      || echo "WARN: restore failed -- re-run examples/08-observability/run.sh" >&2
  elif [[ "$WAS_CLICKHOUSE" == true ]]; then
    echo "cleanup: restoring the gateway to the analytics profile (ingest off)"
    GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose -f "$COMPOSE_FILE" --profile analytics up -d gateway \
      || echo "WARN: restore failed -- run 'docker compose -f $COMPOSE_FILE --profile analytics up -d gateway' by hand" >&2
  else
    echo "cleanup: restoring the plain quick-start gateway (ClickHouse and ingest off)"
    docker compose -f "$COMPOSE_FILE" up -d gateway \
      || echo "WARN: restore failed -- run 'docker compose -f $COMPOSE_FILE up -d gateway' by hand" >&2
    docker compose -f "$COMPOSE_FILE" --profile analytics stop clickhouse adminer >/dev/null 2>&1 || true
  fi
  wait_for_health_soft "$CONTROL_BASE/health"
}

cleanup() {
  local rc=$?
  revoke_key "$INTERCEPTOR_KEY_ID"
  revoke_key "$AGENT_KEY_ID"
  revoke_key "$ADMIN_KEY_MINTED"
  rm -rf "$WORK_DIR"
  restore_stack
  exit "$rc"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    fail "'$1' is required but was not found in PATH. $2"
  fi
}

echo "check: required tools (docker, curl, jq, gzip)"
require_cmd docker "Install Docker Desktop or Docker Engine."
require_cmd curl "Install curl."
require_cmd jq "Install jq (e.g. 'brew install jq' or 'apt-get install jq')."
require_cmd gzip "gzip ships with every macOS/Linux base install."
docker compose version >/dev/null 2>&1 || fail "'docker compose' (the v2 plugin) is required."

# ---------------------------------------------------------------------------
# 1. bring up the stack with ClickHouse and ingest enabled
# ---------------------------------------------------------------------------

if [[ -n "$(docker compose -f "$COMPOSE_FILE" --profile analytics ps -q --status running clickhouse 2>/dev/null)" ]]; then
  WAS_CLICKHOUSE=true
fi
if [[ -n "$(docker compose -f "$COMPOSE_FILE" -f "$OBSERVABILITY_COMPOSE_FILE" --profile analytics ps -q --status running otel-collector 2>/dev/null)" ]]; then
  WAS_OBSERVABILITY=true
fi

echo "check: starting the compose stack with ClickHouse and ingest enabled (docker-compose.ingest.yml)"
STACK_CHANGED=true
GATEWAY_SINKS_CLICKHOUSE_ENABLED=true "${INGEST_COMPOSE[@]}" up -d

echo "check: control plane health ($CONTROL_BASE/health)"
health_body=""
for _ in $(seq 1 90); do
  if health_body="$(curl -sSf "$CONTROL_BASE/health" 2>/dev/null)" \
    && jq -e '.sinks.clickhouse.enabled == true' >/dev/null 2>&1 <<<"$health_body"; then
    break
  fi
  health_body=""
  sleep 1
done
[[ -n "$health_body" ]] || fail "control plane never became healthy with the ClickHouse sink enabled"
echo "  -> $(jq -c '{status, sinks}' <<<"$health_body")"

# ---------------------------------------------------------------------------
# 2. admin key, then the two keys this example needs
# ---------------------------------------------------------------------------

if [[ -n "${GATEWAY_ADMIN_KEY:-}" ]]; then
  echo "check: reusing GATEWAY_ADMIN_KEY from the environment"
else
  echo "check: bootstrapping an admin API key (tenant 'default')"
  # bootstrap-key refuses a second admin key for a tenant unless --force is
  # given; the earlier key's plaintext is gone, so mint another.
  if out="$("${INGEST_COMPOSE[@]}" exec -T gateway /gateway bootstrap-key 2>/dev/null)" && [[ -n "$out" ]]; then
    echo "  -> bootstrapped a new admin key"
  else
    echo "  -> tenant 'default' already has an admin key; minting another with --force"
    out="$("${INGEST_COMPOSE[@]}" exec -T gateway /gateway bootstrap-key --force 2>/dev/null)" || true
    [[ -n "$out" ]] || fail "could not bootstrap an admin API key. Check '${INGEST_COMPOSE[*]} logs gateway'."
  fi
  GATEWAY_ADMIN_KEY="$out"
  # Revoked again in the exit trap (found by its plaintext prefix below).
  ADMIN_KEY_MINTED="pending"
fi
export GATEWAY_ADMIN_KEY

admin_curl() {
  curl -sS -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" "$@"
}

# An API key's plaintext is only returned once, so a key from an aborted
# earlier run can never be reused. Revoke any leftovers by name first.
echo "check: revoking leftover '13-*' keys from an earlier aborted run"
while IFS= read -r stale_id; do
  [[ -n "$stale_id" ]] || continue
  revoke_key "$stale_id"
  echo "  -> revoked $stale_id"
done < <(admin_curl "$CONTROL_BASE/api-keys?limit=500" \
  | jq -r '.items[]? | select(.name=="13-interceptor" or .name=="13-agent") | .id')

if [[ "$ADMIN_KEY_MINTED" == "pending" ]]; then
  ADMIN_PREFIX="${GATEWAY_ADMIN_KEY:0:8}"
  ADMIN_KEY_MINTED="$(admin_curl "$CONTROL_BASE/api-keys?limit=500" \
    | jq -r --arg p "$ADMIN_PREFIX" '[.items[]? | select(.prefix | startswith($p)) | .id][0] // empty')"
fi

mint_key() {
  local name="$1" role="$2" resp
  resp="$(admin_curl -f -X POST "$CONTROL_BASE/api-keys" -H "Content-Type: application/json" \
    -d "{\"name\": \"$name\", \"role\": \"$role\"}")" || fail "could not create the '$name' key"
  KEY_PLAINTEXT="$(jq -r '.key' <<<"$resp")"
  KEY_ID="$(jq -r '.id' <<<"$resp")"
  [[ -n "$KEY_PLAINTEXT" && "$KEY_PLAINTEXT" != null && -n "$KEY_ID" && "$KEY_ID" != null ]] \
    || fail "key creation for role '$role' returned no key/id: $resp"
}

echo "check: minting a key with the built-in 'interceptor' role"
mint_key "13-interceptor" "interceptor"
INTERCEPTOR_KEY="$KEY_PLAINTEXT"
INTERCEPTOR_KEY_ID="$KEY_ID"
echo "  -> minted (role: interceptor, id $INTERCEPTOR_KEY_ID)"

echo "check: minting a key with the 'agent' role (to prove it cannot ingest)"
mint_key "13-agent" "agent"
AGENT_KEY="$KEY_PLAINTEXT"
AGENT_KEY_ID="$KEY_ID"
echo "  -> minted (role: agent, id $AGENT_KEY_ID)"

# ---------------------------------------------------------------------------
# 3. build the batch the way the utility does: one JSON object, gzipped
# ---------------------------------------------------------------------------

NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
BATCH_JSON="$WORK_DIR/batch.json"
BATCH_GZ="$WORK_DIR/batch.json.gz"

jq -n \
  --arg now "$NOW" --arg user "$USER_LABEL" --arg sid "$SESSION_ID" \
  --arg llm_id "$LLM_REQUEST_ID" --arg tool_id "$TOOL_REQUEST_ID" '
  {
    schema_version: 1,
    user: $user,
    llm_calls: [{
      timestamp: $now,
      request_id: $llm_id,
      status_code: 200,
      session_id: $sid,
      user_agent: "claude-desktop-example/1.0",
      upstream_host: "api.anthropic.com",
      model: "claude-sonnet-5-5",
      requested_model: "claude-sonnet-5-5",
      path: "/v1/messages",
      duration_ms: 1840,
      stream: true,
      input_tokens: 42,
      output_tokens: 17,
      cache_read_tokens: 0,
      cache_creation_tokens: 0,
      stop_reason: "end_turn",
      provider_request_id: "msg_13example",
      messages: ([{role: "user", content: "List the files in this folder."}] | tojson),
      request_body: ({model: "claude-sonnet-5-5", messages: [{role: "user", content: "List the files in this folder."}]} | tojson),
      response_body: ({id: "msg_13example", type: "message", role: "assistant", model: "claude-sonnet-5-5",
                       content: [{type: "text", text: "There are three files."}],
                       stop_reason: "end_turn", usage: {input_tokens: 42, output_tokens: 17}} | tojson)
    }],
    access_logs: [{
      timestamp: $now,
      request_id: $tool_id,
      status_code: 200,
      session_id: $sid,
      client_session_id: $sid,
      method: "tools/call",
      json_rpc_id: "toolu_13example",
      connector_id: "example-files",
      tool_name: "list_files",
      duration_ms: 95,
      bytes_in: 31,
      bytes: 120,
      user_agent: "claude-desktop-example/1.0"
    }]
  }' >"$BATCH_JSON"
gzip -c "$BATCH_JSON" >"$BATCH_GZ"
echo "check: built a batch of 1 llm_call + 1 access_log ($(wc -c <"$BATCH_JSON" | tr -d ' ') bytes, $(wc -c <"$BATCH_GZ" | tr -d ' ') gzipped)"

# ingest POSTs the gzipped batch with the given key ("" = no key) and leaves
# the HTTP status in INGEST_STATUS and the body in INGEST_BODY.
ingest() {
  local key="$1" key_args=()
  if [[ -n "$key" ]]; then
    key_args=(-H "X-Gateway-Key: $key")
  fi
  local out
  out="$(curl -sS -w '\n%{http_code}' -X POST "$CONTROL_BASE/ingest" \
    -H "Content-Type: application/json" -H "Content-Encoding: gzip" \
    ${key_args[@]+"${key_args[@]}"} --data-binary "@$BATCH_GZ")"
  INGEST_STATUS="$(tail -n1 <<<"$out")"
  INGEST_BODY="$(sed '$d' <<<"$out")"
}

# ---------------------------------------------------------------------------
# 4. auth: who may ingest
# ---------------------------------------------------------------------------

echo "check: POST /ingest with no key is refused (401)"
ingest ""
[[ "$INGEST_STATUS" == 401 ]] || fail "expected 401 with no key, got $INGEST_STATUS: $INGEST_BODY"
echo "  -> 401"

echo "check: POST /ingest with an 'agent'-role key is refused (403)"
ingest "$AGENT_KEY"
[[ "$INGEST_STATUS" == 403 ]] || fail "expected 403 for the agent key, got $INGEST_STATUS: $INGEST_BODY"
echo "  -> 403"

# ---------------------------------------------------------------------------
# 5. ingest, then re-ingest the identical batch
# ---------------------------------------------------------------------------

echo "check: POST /ingest with the interceptor key"
ingest "$INTERCEPTOR_KEY"
[[ "$INGEST_STATUS" == 200 ]] || fail "expected 200, got $INGEST_STATUS: $INGEST_BODY"
echo "  -> 200 $INGEST_BODY"
jq -e '.rejected | length == 0' >/dev/null <<<"$INGEST_BODY" || fail "records were rejected: $INGEST_BODY"
# Fixed request ids: on a stack that already holds them (a re-run) the same
# records come back as duplicates instead of accepted.
jq -e '(.accepted.llm_calls + .duplicates.llm_calls) == 1 and (.accepted.access_logs + .duplicates.access_logs) == 1' \
  >/dev/null <<<"$INGEST_BODY" || fail "expected exactly 1 llm_call and 1 access_log accepted-or-duplicate: $INGEST_BODY"

echo "check: re-POSTing the identical batch is deduplicated by request_id"
ingest "$INTERCEPTOR_KEY"
[[ "$INGEST_STATUS" == 200 ]] || fail "expected 200 on the re-POST, got $INGEST_STATUS: $INGEST_BODY"
echo "  -> 200 $INGEST_BODY"
jq -e '.accepted.llm_calls == 0 and .accepted.access_logs == 0 and .duplicates.llm_calls == 1 and .duplicates.access_logs == 1' \
  >/dev/null <<<"$INGEST_BODY" || fail "expected 0 accepted and 1+1 duplicates: $INGEST_BODY"

echo "check: a batch with the wrong schema_version is refused (400)"
bad_status="$(jq '.schema_version = 2' "$BATCH_JSON" | curl -sS -o /dev/null -w '%{http_code}' -X POST "$CONTROL_BASE/ingest" \
  -H "Content-Type: application/json" -H "X-Gateway-Key: $INTERCEPTOR_KEY" --data-binary @-)"
[[ "$bad_status" == 400 ]] || fail "expected 400 for schema_version 2, got $bad_status"
echo "  -> 400"

# ---------------------------------------------------------------------------
# 6. the rows the console reads
# ---------------------------------------------------------------------------

# wait_for_rows polls a list endpoint until it returns exactly one row with
# the given request id, source interceptor and the user label.
wait_for_rows() {
  local label="$1" path="$2" request_id="$3" body=""
  for _ in $(seq 1 30); do
    body="$(admin_curl -f "$CONTROL_BASE/$path?source=interceptor&session_id=$SESSION_ID&limit=50" 2>/dev/null || true)"
    if [[ -n "$body" ]] && jq -e --arg id "$request_id" --arg user "$USER_LABEL" \
      '[.items[] | select(.request_id == $id and .source == "interceptor" and .user == $user)] | length == 1' \
      >/dev/null 2>&1 <<<"$body"; then
      echo "  -> $label: 1 row, source=interceptor, user=$USER_LABEL"
      return 0
    fi
    sleep 1
  done
  fail "$label: expected exactly one $request_id row with source=interceptor and user=$USER_LABEL, got: $body"
}

echo "check: LLM Logs (GET /analytics/llm-logs?source=interceptor)"
wait_for_rows "llm-logs" "analytics/llm-logs" "$LLM_REQUEST_ID"
echo "check: Access Logs (GET /analytics/logs?source=interceptor)"
wait_for_rows "logs" "analytics/logs" "$TOOL_REQUEST_ID"

echo "check: the LLM row carries the model, token counts and no cost"
llm_row="$(admin_curl -f "$CONTROL_BASE/analytics/llm-logs/$LLM_REQUEST_ID")"
jq -e '.model == "claude-sonnet-5-5" and .input_tokens == 42 and .output_tokens == 17 and (.cost_usd == null)' \
  >/dev/null <<<"$llm_row" || fail "unexpected LLM row: $(jq -c 'del(.request_body, .response_body, .messages)' <<<"$llm_row")"
echo "  -> $(jq -c '{model, input_tokens, output_tokens, cost_usd, source, user}' <<<"$llm_row")"

echo "check: the interceptor key cannot read the logs back (ingest.write only)"
read_status="$(curl -sS -o /dev/null -w '%{http_code}' -H "X-Gateway-Key: $INTERCEPTOR_KEY" "$CONTROL_BASE/analytics/llm-logs?limit=1")"
[[ "$read_status" == 403 ]] || fail "expected 403 reading logs with the interceptor key, got $read_status"
echo "  -> 403"

echo "check: Session Timeline merges both rows (GET /analytics/sessions/$SESSION_ID/timeline)"
timeline="$(admin_curl -f "$CONTROL_BASE/analytics/sessions/$SESSION_ID/timeline")"
timeline_ids="$(jq -r '.events[]?.id' <<<"$timeline" | sort -u | tr '\n' ' ')"
case "$timeline_ids" in
  *"$LLM_REQUEST_ID"*"$TOOL_REQUEST_ID"* | *"$TOOL_REQUEST_ID"*"$LLM_REQUEST_ID"*) echo "  -> timeline holds $timeline_ids" ;;
  *) fail "timeline is missing the example rows: $(jq -c . <<<"$timeline" | cut -c1-600)" ;;
esac

echo
echo "OK: 13-claude-desktop gateway-side checks passed."
echo "Console: http://localhost:8081 -> LLM Logs / Access Logs (filter Source: Interceptor)"
