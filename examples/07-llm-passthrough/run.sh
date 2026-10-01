#!/usr/bin/env bash
#
# 07-llm-passthrough: the same client shape hitting Anthropic, OpenAI, Gemini,
# and Bedrock through the gateway's LLM plane (:8082) with the caller's own
# provider credentials (BYOK) -- streaming works unchanged, and every call is
# captured with tokens + estimated cost.
#
# There is no fake upstream here (this repo tests against real services, not
# mocks), so this script runs in two tiers:
#
#   1. Always (this is what CI runs, no provider keys needed): for each
#      provider, send a request through the gateway to the REAL provider
#      with a deliberately BOGUS provider key, and assert that the
#      provider's OWN error comes back -- proving the request actually
#      reached the provider, and that the gateway key and the provider key
#      are two separate credentials (a request with no gateway key at all is
#      rejected by the gateway itself, before ever reaching a provider).
#   2. With real keys exported (ANTHROPIC_API_KEY / OPENAI_API_KEY /
#      GEMINI_API_KEY / AWS credentials): for each one present, run the
#      matching curl/*.sh for real and assert HTTP 200 + expected content.
#      Whichever aren't set are skipped with a clear "skipped: X not set"
#      line -- this is expected to be partial in CI.
#
# Safe to re-run: every setup step checks current state first and reuses it,
# same exception as 05-profiles/04-python-agent -- the agent API key is
# minted fresh every run (its plaintext is only ever returned once).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.yml"

CONTROL_BASE="http://localhost:8081/api/v1"
LLM_URL="http://localhost:8082"
CONSOLE_URL="http://localhost:8081"

AGENT_KEY_NAME="07-llm-passthrough-agent"

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

echo "check: required tools (docker, curl, jq)"
require_cmd docker "Install Docker Desktop or Docker Engine."
require_cmd curl "Install curl."
require_cmd jq "Install jq (e.g. 'brew install jq' or 'apt-get install jq')."
if ! docker compose version >/dev/null 2>&1; then
  echo "FAIL: 'docker compose' (the v2 plugin) is required." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 1. compose up + health (control plane for admin/API calls, LLM plane for
#    everything this example actually exercises)
# ---------------------------------------------------------------------------

echo "check: starting the compose stack (deploy/docker-compose.yml)"
compose_up_preserving_analytics

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

wait_for_health "control plane" "$CONTROL_BASE/health"
wait_for_health "LLM plane" "$LLM_URL/health"

# ---------------------------------------------------------------------------
# 2. bootstrap an admin API key
# ---------------------------------------------------------------------------

if [[ -n "${GATEWAY_ADMIN_KEY:-}" ]]; then
  echo "check: reusing GATEWAY_ADMIN_KEY from the environment"
else
  echo "check: bootstrapping an admin API key (tenant 'default')"
  # bootstrap-key prints only the plaintext key on stdout; it refuses to mint
  # a second admin key for a tenant that already has one unless --force is
  # passed (cmd/gateway/main.go, runBootstrapKey). A re-run of this script
  # (or of 01-quickstart/05-profiles/04-python-agent against the same stack)
  # hits that case, and the earlier key's plaintext is gone, so mint another.
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
# 3. mint an agent-role API key
# ---------------------------------------------------------------------------
#
# Not idempotent by design, same as 05-profiles/04-python-agent: an API
# key's plaintext (the "key" field) is only ever returned once
# (internal/api/handlers/apikeys.go, APIKeys.Create). The LLM plane's auth
# middleware (cmd/gateway/main.go wraps llmplane.Handler with it directly)
# accepts either role -- an agent key is used here since it's the least
# privilege that can reach the LLM plane (pkg/auth: "agent": {"mcp.*",
# "llm.*", "*.read"}).

echo "check: minting a fresh agent-role API key ('$AGENT_KEY_NAME')"
agent_key_resp="$(curl -sSf -X POST "$CONTROL_BASE/api-keys" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg n "$AGENT_KEY_NAME" '{name: $n, role: "agent"}')")"
AGENT_KEY="$(jq -r '.key' <<<"$agent_key_resp")"
if [[ -z "$AGENT_KEY" || "$AGENT_KEY" == "null" ]]; then
  echo "FAIL: api-key creation did not return a plaintext key" >&2
  exit 1
fi
echo "  -> minted $(jq -r '.prefix' <<<"$agent_key_resp")... (role: $(jq -r '.role' <<<"$agent_key_resp"))"

# ---------------------------------------------------------------------------
# Tier 1 helpers
# ---------------------------------------------------------------------------

HEADERS_TMP="$(mktemp)"
trap 'rm -f "$HEADERS_TMP"' EXIT

LLM_STATUS=""
LLM_BODY=""

# llm_post issues one LLM-plane POST. $1=label, $2=path (under $LLM_URL),
# $3=JSON body, $4...=extra "Name: Value" headers. Leaves the HTTP status in
# LLM_STATUS, the body in LLM_BODY, and the response headers in $HEADERS_TMP
# -- never asserts anything itself (negative-path callers need the raw
# status/body to make their own call).
llm_post() {
  local label="$1" path="$2" body="$3"
  shift 3
  local hdr_args=()
  for h in "$@"; do
    hdr_args+=(-H "$h")
  done
  local full
  full="$(curl -sS -D "$HEADERS_TMP" -X POST "$LLM_URL$path" \
    ${hdr_args[@]+"${hdr_args[@]}"} \
    -H 'content-type: application/json' \
    -d "$body" \
    -w '\n%{http_code}')"
  LLM_STATUS="${full##*$'\n'}"
  LLM_BODY="${full%$'\n'*}"
  echo "  -> [$label] HTTP $LLM_STATUS: $(echo "$LLM_BODY" | head -c 300)"
}

# ---------------------------------------------------------------------------
# Tier 1 (always, no provider keys): bogus-provider-key negative paths
# ---------------------------------------------------------------------------
#
# Every check below sends the REAL agent key minted in step 3 to the
# gateway, plus a deliberately bogus provider credential, straight through
# to the REAL provider (there is no fake upstream). The provider's own
# error coming back through the gateway is the proof the request actually
# reached it, byte-for-byte, with the caller's credential intact
# (internal/llmplane/provider.go's copyHeaders keeps a BYOK Authorization/
# x-api-key/x-goog-api-key; only a gateway "Bearer gk_..." is stripped
# before forwarding -- isGatewayBearer).

echo
echo "=== Tier 1: no-keys checks (bogus provider credentials; this is what CI runs) ==="

echo "check: LLM plane /health"
health_body="$(curl -sSf "$LLM_URL/health")"
echo "  -> $health_body"
if [[ "$(jq -r '.status' <<<"$health_body")" != "ok" || "$(jq -r '.plane' <<<"$health_body")" != "llm" ]]; then
  echo "FAIL: /health did not report {\"status\":\"ok\",\"plane\":\"llm\"}: $health_body" >&2
  exit 1
fi

echo "check: no gateway key at all -> 401 (the gateway's own auth, ahead of any provider)"
llm_post "no gateway key" "/anthropic/v1/messages" \
  '{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}'
if [[ "$LLM_STATUS" != "401" ]]; then
  echo "FAIL: expected 401 with no gateway key, got $LLM_STATUS: $LLM_BODY" >&2
  exit 1
fi
if [[ "$LLM_BODY" != *"authentication_error"* || "$LLM_BODY" != *"authentication required"* ]]; then
  echo "FAIL: expected the gateway's own \"authentication required\" error, got: $LLM_BODY" >&2
  exit 1
fi

echo "check: bare unrouted path (/v1/chat/completions) -> 404, gateway key accepted but no provider matches"
llm_post "unrouted path" "/v1/chat/completions" \
  '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  "X-Gateway-Key: $AGENT_KEY"
if [[ "$LLM_STATUS" != "404" ]]; then
  echo "FAIL: expected 404 for an unrouted path, got $LLM_STATUS: $LLM_BODY" >&2
  exit 1
fi
if [[ "$LLM_BODY" != *"not_found_error"* ]]; then
  echo "FAIL: expected a not_found_error envelope, got: $LLM_BODY" >&2
  exit 1
fi

echo "check: Anthropic, bogus x-api-key -> Anthropic's own 401 authentication_error"
llm_post "anthropic bogus key" "/anthropic/v1/messages" \
  '{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}' \
  "X-Gateway-Key: $AGENT_KEY" \
  "x-api-key: sk-ant-bogus-ci-0000000000000000" \
  "anthropic-version: 2023-06-01"
if [[ "$LLM_STATUS" != "401" ]]; then
  echo "FAIL: expected Anthropic to reject a bogus key with 401, got $LLM_STATUS: $LLM_BODY" >&2
  exit 1
fi
if [[ "$LLM_BODY" != *"authentication_error"* ]]; then
  echo "FAIL: expected Anthropic's own authentication_error, got: $LLM_BODY" >&2
  exit 1
fi

echo "check: OpenAI, bogus Authorization -> OpenAI's own 401 invalid_api_key"
llm_post "openai bogus key" "/openai/v1/chat/completions" \
  '{"model":"gpt-4o-mini","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}' \
  "X-Gateway-Key: $AGENT_KEY" \
  "Authorization: Bearer sk-bogus-ci-0000000000000000"
if [[ "$LLM_STATUS" != "401" ]]; then
  echo "FAIL: expected OpenAI to reject a bogus key with 401, got $LLM_STATUS: $LLM_BODY" >&2
  exit 1
fi
if [[ "$LLM_BODY" != *"invalid_api_key"* ]]; then
  echo "FAIL: expected OpenAI's own invalid_api_key error, got: $LLM_BODY" >&2
  exit 1
fi

echo "check: Gemini, bogus x-goog-api-key -> Gemini's own 400 API_KEY_INVALID"
llm_post "gemini bogus key" "/gemini/v1beta/models/gemini-2.5-flash:generateContent" \
  '{"contents":[{"parts":[{"text":"hi"}]}]}' \
  "X-Gateway-Key: $AGENT_KEY" \
  "x-goog-api-key: bogus-gemini-key-ci-0000000000"
if [[ "$LLM_STATUS" != "400" ]]; then
  echo "FAIL: expected Gemini to reject a bogus key with 400, got $LLM_STATUS: $LLM_BODY" >&2
  exit 1
fi
if [[ "$LLM_BODY" != *"API_KEY_INVALID"* ]]; then
  echo "FAIL: expected Gemini's own API_KEY_INVALID error, got: $LLM_BODY" >&2
  exit 1
fi

echo "check: Bedrock Flow B, bogus AWS keys -> AWS's own signature-rejection error"
llm_post "bedrock bogus keys" "/bedrock/model/anthropic.claude-haiku-4-5-20251001-v1:0/converse" \
  '{"messages":[{"role":"user","content":[{"text":"hi"}]}],"inferenceConfig":{"maxTokens":10}}' \
  "X-Gateway-Key: $AGENT_KEY" \
  "X-Bedrock-Region: us-east-1" \
  "X-Bedrock-Access-Key-Id: AKIAFAKEFAKEFAKEFAKE" \
  "X-Bedrock-Secret-Access-Key: fakeSecretKeyValueNotReal00000000000000"
bedrock_errtype="$(grep -i '^x-amzn-errortype:' "$HEADERS_TMP" | tail -n1 | sed -E 's/^[^:]+:[[:space:]]*//' | tr -d '\r\n' || true)"
echo "  -> X-Amzn-Errortype: ${bedrock_errtype:-<none>}"
if [[ "$LLM_STATUS" != "400" && "$LLM_STATUS" != "403" ]]; then
  echo "FAIL: expected AWS to reject bogus credentials with 400 or 403, got $LLM_STATUS: $LLM_BODY" >&2
  exit 1
fi
if [[ "$bedrock_errtype" != *"UnrecognizedClientException"* && "$LLM_BODY" != *"security token"* && "$LLM_BODY" != *"UnrecognizedClientException"* ]]; then
  echo "FAIL: expected an AWS credential-rejection error (UnrecognizedClientException or similar), got: $LLM_BODY (errtype: $bedrock_errtype)" >&2
  exit 1
fi

echo
echo "Tier 1 passed: every provider's OWN error came back through the gateway, and the gateway's own auth (no key -> 401) is a separate check from the provider's (valid gateway key + bogus provider key -> provider error) -- proving the two credentials never collide."

# ---------------------------------------------------------------------------
# Tier 2 (best-effort, only if real keys are exported)
# ---------------------------------------------------------------------------

echo
echo "=== Tier 2: real-key checks (skipped per-provider if its key isn't set) ==="

if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
  out="$(GATEWAY_URL="$LLM_URL" GATEWAY_KEY="$AGENT_KEY" "$SCRIPT_DIR/curl/anthropic.sh")"
  if ! echo "$out" | jq -e '.content[0].text' >/dev/null 2>&1; then
    echo "FAIL: Anthropic response had no .content[0].text: $out" >&2
    exit 1
  fi
  echo "check: Anthropic (real key) -- ok, tokens: $(echo "$out" | jq -c '.usage')"
else
  echo "skipped: ANTHROPIC_API_KEY not set"
fi

if [[ -n "${OPENAI_API_KEY:-}" ]]; then
  out="$(GATEWAY_URL="$LLM_URL" GATEWAY_KEY="$AGENT_KEY" "$SCRIPT_DIR/curl/openai.sh")"
  if ! echo "$out" | jq -e '.choices[0].message.content' >/dev/null 2>&1; then
    echo "FAIL: OpenAI response had no .choices[0].message.content: $out" >&2
    exit 1
  fi
  echo "check: OpenAI (real key) -- non-stream ok, tokens: $(echo "$out" | jq -c '.usage')"

  echo "check: OpenAI (real key) -- streaming"
  stream_out="$(GATEWAY_URL="$LLM_URL" GATEWAY_KEY="$AGENT_KEY" "$SCRIPT_DIR/curl/openai-stream.sh")"
  if [[ "$stream_out" != *"data:"* ]]; then
    echo "FAIL: expected an SSE stream (data: ... lines), got: $stream_out" >&2
    exit 1
  fi
  usage_line="$(echo "$stream_out" | grep '^data:' | grep '"usage"' | tail -n1 | sed 's/^data: *//')"
  if [[ -z "$usage_line" ]]; then
    echo "FAIL: streaming response never carried a usage chunk (stream_options.include_usage) -- got: $stream_out" >&2
    exit 1
  fi
  echo "  -> tokens (final stream chunk): $(echo "$usage_line" | jq -c '.usage')"
else
  echo "skipped: OPENAI_API_KEY not set (also skips its streaming check)"
fi

if [[ -n "${GEMINI_API_KEY:-}" ]]; then
  out="$(GATEWAY_URL="$LLM_URL" GATEWAY_KEY="$AGENT_KEY" "$SCRIPT_DIR/curl/gemini.sh")"
  if ! echo "$out" | jq -e '.candidates[0].content.parts[0].text' >/dev/null 2>&1; then
    echo "FAIL: Gemini response had no .candidates[0].content.parts[0].text: $out" >&2
    exit 1
  fi
  echo "check: Gemini (real key) -- ok, tokens: $(echo "$out" | jq -c '.usageMetadata')"
else
  echo "skipped: GEMINI_API_KEY not set"
fi

# AWS creds: static keys win; fall back to resolving AWS_PROFILE via the AWS
# CLI (same pattern deploy/docker-compose.yml's own header comment
# describes for Flow C: `aws configure export-credentials --format env`).
aws_ak="${AWS_ACCESS_KEY_ID:-}"
aws_sk="${AWS_SECRET_ACCESS_KEY:-}"
aws_st="${AWS_SESSION_TOKEN:-}"
if [[ -z "$aws_ak" && -n "${AWS_PROFILE:-}" ]] && command -v aws >/dev/null 2>&1; then
  echo "check: AWS_PROFILE=$AWS_PROFILE set, no static keys -- resolving via 'aws configure export-credentials'"
  if creds_json="$(aws configure export-credentials --profile "$AWS_PROFILE" --format json 2>/dev/null)"; then
    aws_ak="$(jq -r '.AccessKeyId // empty' <<<"$creds_json")"
    aws_sk="$(jq -r '.SecretAccessKey // empty' <<<"$creds_json")"
    aws_st="$(jq -r '.SessionToken // empty' <<<"$creds_json")"
  fi
fi

if [[ -n "$aws_ak" && -n "$aws_sk" ]]; then
  out="$(GATEWAY_URL="$LLM_URL" GATEWAY_KEY="$AGENT_KEY" AWS_ACCESS_KEY_ID="$aws_ak" AWS_SECRET_ACCESS_KEY="$aws_sk" AWS_SESSION_TOKEN="$aws_st" "$SCRIPT_DIR/curl/bedrock-B-clientkeys.sh")"
  if ! echo "$out" | jq -e '.output.message.content' >/dev/null 2>&1; then
    echo "FAIL: Bedrock response had no .output.message.content: $out" >&2
    exit 1
  fi
  echo "check: Bedrock Flow B (real AWS creds) -- ok, tokens: $(echo "$out" | jq -c '.usage')"
else
  echo "skipped: AWS credentials not set (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY[/AWS_SESSION_TOKEN], or AWS_PROFILE)"
fi

# ---------------------------------------------------------------------------
# Analytics: LLM Logs (ClickHouse, else the Postgres capture table)
# ---------------------------------------------------------------------------

echo
echo "check: GET /api/v1/analytics/llm-logs"
analytics_full="$(curl -sS -w '\n%{http_code}' "$CONTROL_BASE/analytics/llm-logs?limit=5" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY")"
analytics_status="${analytics_full##*$'\n'}"
analytics_body="${analytics_full%$'\n'*}"
if [[ "$analytics_status" == "200" ]]; then
  echo "  -> last rows (provider/model/tokens/cost):"
  echo "$analytics_body" | jq -c '.items[] | {provider, model, input_tokens, output_tokens, cost_usd}'
else
  echo "  -> not available (HTTP $analytics_status: $analytics_body)"
  echo "  -> without ClickHouse, LLM Logs needs llm_proxy.capture.store: postgres (the compose default); or enable ClickHouse: GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose -f deploy/docker-compose.yml --profile analytics up -d"
fi

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

echo
echo "================================================================"
echo "07-llm-passthrough passed."
echo "Console:    $CONSOLE_URL"
echo "Agent key:  $AGENT_KEY"
echo
echo "To run the real-key tier yourself:"
echo "  export GATEWAY_URL=$LLM_URL"
echo "  export GATEWAY_KEY=$AGENT_KEY"
echo "  export ANTHROPIC_API_KEY=sk-ant-...   # and/or OPENAI_API_KEY, GEMINI_API_KEY"
echo "  export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # and/or AWS_PROFILE=..."
echo "  ./examples/07-llm-passthrough/run.sh"
echo "================================================================"
