#!/usr/bin/env bash
#
# 09-roles-keys-and-rate-limits: a custom "reader" role from auth.roles,
# key revoke/rotate, and the control plane's failed-auth lockout -- all
# against a real gateway.
#
# Brings the compose stack up with an overlay (docker-compose.roles.yml)
# that mounts config.roles.yaml and points CONFIG_PATH at it -- the
# shipped deploy/docker-compose.yml has no config-file mount by design,
# and auth.roles (a map) isn't reachable via GATEWAY_* env vars; the
# rate-limit tuning rides along in the same file (see config.roles.yaml). The overlay is dropped again in a trap
# (both on success and on failure) so the stack is left in its default,
# GATEWAY_*-env-only state, ready for another example.
#
# No MCP connector is needed here (unlike 01-quickstart/05-profiles):
# this example's only MCP-plane interaction is one denied `initialize`
# call, to show that "reader" cannot reach that plane at all.
#
# Self-contained: does not assume any other example has been run first.
#
# Safe to re-run: the roles overlay, the admin key, and the rate-limit
# lockout are all reused/re-derived from scratch every run. The one
# exception is the "09-reader" and "09-agent" API keys, minted fresh every
# run -- an API key's plaintext is only ever returned once, so there is
# nothing to "reuse" (same reasoning as 05-profiles' agent key; see this
# script's step 3 and the README's Cleanup section).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.yml"
ROLES_COMPOSE_FILE="$SCRIPT_DIR/docker-compose.roles.yml"
# Only referenced by restore_default_compose below, to put the gateway
# back exactly as examples/08-observability left it, if that example ran
# first against this same stack (see that function's comment).
OBSERVABILITY_COMPOSE_FILE="$ROOT_DIR/examples/08-observability/docker-compose.observability.yml"

CONTROL_BASE="http://localhost:8081/api/v1"
MCP_URL="http://localhost:8080/mcp"
CONSOLE_URL="http://localhost:8081"

GATEWAY_COMPOSE=(docker compose -f "$COMPOSE_FILE")
GATEWAY_COMPOSE_ROLES=(docker compose -f "$COMPOSE_FILE" -f "$ROLES_COMPOSE_FILE")

HEADERS_TMP="$(mktemp)"
# ROLES_OVERLAY_UP gates restore_default_compose (below): it's flipped on
# right after the roles overlay actually comes up, so the EXIT trap only
# tries to restore the default compose config if that overlay was really
# applied -- mirrors 05-profiles' STRICT_MODE_ON gate on
# restore_strict_mode, for the same reason.
ROLES_OVERLAY_UP=false

# wait_for_health_soft is wait_for_health's (below) best-effort twin, used
# only in the cleanup path (restore_default_compose): it warns instead of
# exiting, so a cleanup hiccup never masks whatever error actually
# triggered the cleanup.
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

# restore_default_compose drops the roles overlay by re-applying the
# unmounted, env-only "gateway" service from deploy/docker-compose.yml
# alone -- Compose recreates the container automatically once its merged
# config differs, no --force-recreate needed (see docker-compose.roles.yml).
# It is both called directly (the happy path, at the end of this script)
# and registered as part of the EXIT trap below, so a failure partway
# through leaves the local stack back in its default state rather than
# stuck on this example's config.
#
# "Default" isn't always the base file alone, though: if
# examples/08-observability ran earlier against this same persistent
# stack, the gateway is currently up under ITS overlay (docker-compose
# .observability.yml, --profile analytics, GATEWAY_SINKS_CLICKHOUSE_
# ENABLED=true -- see that example's run.sh). Recreating "gateway" with
# just $COMPOSE_FILE would silently drop that config back to the
# ClickHouse-off, OTel-off baseline -- a real bug an e2e run caught: this
# example's cleanup was recreating the gateway container without the
# analytics profile it had been started with. The "clickhouse" service
# only ever comes up under --profile analytics (deploy/docker-compose.yml),
# and nothing else in examples-smoke starts it, so a running clickhouse
# container is a reliable signal that 08-observability's overlay is what
# this example needs to restore, not the plain base file.
restore_default_compose() {
  if [[ "$ROLES_OVERLAY_UP" != true ]]; then
    return 0
  fi
  if [[ -n "$(docker compose -f "$COMPOSE_FILE" --profile analytics ps -q --status running clickhouse 2>/dev/null)" ]]; then
    echo "cleanup: dropping the roles overlay (restoring the analytics-profile gateway examples/08-observability started)"
    GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose -f "$COMPOSE_FILE" -f "$OBSERVABILITY_COMPOSE_FILE" --profile analytics up -d gateway \
      || echo "WARN: failed to restore the gateway to its analytics-profile config -- run 'GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose -f $COMPOSE_FILE -f $OBSERVABILITY_COMPOSE_FILE --profile analytics up -d gateway' by hand" >&2
  else
    echo "cleanup: dropping the roles overlay (restoring deploy/docker-compose.yml's gateway alone)"
    "${GATEWAY_COMPOSE[@]}" up -d gateway \
      || echo "WARN: failed to restore the gateway to its default config -- run '${GATEWAY_COMPOSE[*]} up -d gateway' by hand" >&2
  fi
  wait_for_health_soft "control plane" "$CONTROL_BASE/health"
  ROLES_OVERLAY_UP=false
}

cleanup() {
  rm -f "$HEADERS_TMP"
  restore_default_compose
}
trap cleanup EXIT

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
# 1. bring up the compose stack with the roles overlay
# ---------------------------------------------------------------------------

echo "check: starting the compose stack with the roles overlay (docker-compose.roles.yml)"
"${GATEWAY_COMPOSE_ROLES[@]}" up -d
ROLES_OVERLAY_UP=true

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
wait_for_health "control plane" "$CONTROL_BASE/health"
wait_for_health "LLM plane" "http://localhost:8082/health"

# ---------------------------------------------------------------------------
# 2. bootstrap an admin API key
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
# helpers shared by every check below
# ---------------------------------------------------------------------------

# expect_status runs curl with the given extra arguments, asserting the
# response's HTTP status equals want. Used for every plain status-code
# assertion below (200/403/401/429) so each one is a single line instead
# of a repeated curl-then-compare block; the two checks that also need to
# inspect a header or the body (Retry-After, locked_ips, the MCP JSON-RPC
# error code) are written out separately further down.
expect_status() {
  local want="$1" label="$2"; shift 2
  local got
  got="$(curl -sS -o /dev/null -w '%{http_code}' "$@")"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL: $label got HTTP $got, wanted $want" >&2
    exit 1
  fi
  echo "  -> $label: HTTP $got"
}

AUTH() { echo "Authorization: Bearer $1"; }

# ---------------------------------------------------------------------------
# 3. mint the "09-reader" and "09-agent" API keys
# ---------------------------------------------------------------------------
#
# Not idempotent by design, like 05-profiles' agent key: an API key's
# plaintext ("key") is only ever returned once, at creation
# (internal/api/handlers/apikeys.go, APIKeys.Create) -- there's nothing to
# "look up and reuse". Every run mints a fresh pair; see the README's
# Cleanup section.
#
# RID/AID (this run's own row ids) are captured directly from these
# create responses, not re-derived later by name -- POST /api-keys'
# response carries its own "id" right alongside "key", and by the second
# run against the same persistent stack, "GET /api-keys | select(.name==
# ...)" matches more than one row: the READMEs's own Cleanup section
# documents that every run's revoked "09-agent" and rotated-out "09-reader"
# predecessor are left behind on purpose. Steps 6-7 below used to
# re-look-up "the" key by name and take the first match, which a real e2e
# run caught silently grabbing an EARLIER run's already-revoked "09-agent"
# row instead of this run's freshly-minted one -- the DELETE "succeeded"
# (some row really was revoked), but the row THIS run's $AKEY actually
# authenticates as was never touched, so the very next assertion (GET
# /connectors with the revoked key expecting 401) failed.

echo "check: minting a fresh 'reader'-role API key ('09-reader')"
reader_resp="$(curl -sSf -X POST "$CONTROL_BASE/api-keys" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "09-reader", "role": "reader"}')"
RKEY="$(jq -r '.key' <<<"$reader_resp")"
RID="$(jq -r '.id' <<<"$reader_resp")"
if [[ -z "$RKEY" || "$RKEY" == "null" || -z "$RID" || "$RID" == "null" ]]; then
  echo "FAIL: api-key creation for role 'reader' did not return a plaintext key and id: $reader_resp" >&2
  exit 1
fi
echo "  -> minted $(jq -r '.prefix' <<<"$reader_resp")... (role: reader, id $RID)"

echo "check: minting a fresh 'agent'-role API key ('09-agent')"
agent_resp="$(curl -sSf -X POST "$CONTROL_BASE/api-keys" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "09-agent", "role": "agent"}')"
AKEY="$(jq -r '.key' <<<"$agent_resp")"
AID="$(jq -r '.id' <<<"$agent_resp")"
if [[ -z "$AKEY" || "$AKEY" == "null" || -z "$AID" || "$AID" == "null" ]]; then
  echo "FAIL: api-key creation for role 'agent' did not return a plaintext key and id: $agent_resp" >&2
  exit 1
fi
echo "  -> minted $(jq -r '.prefix' <<<"$agent_resp")... (role: agent, id $AID)"

# ---------------------------------------------------------------------------
# 4. reader lens: connectors read/write, and the MCP plane
# ---------------------------------------------------------------------------
#
# "reader" (config.roles.yaml) is granted exactly connector.read,
# profile.read, cache.read, analytics.read -- so GET /connectors matches
# (connector.read) but POST /connectors doesn't (connector.create), per
# internal/api/router.go's route table. Neither pattern matches
# "mcp.access", the one permission internal/dataplane/transport/auth.go
# requires for the MCP plane -- so a reader-authenticated MCP request is
# rejected there, not at the connector layer.

echo
echo "== reader lens =="

expect_status 200 "GET /connectors (reader)" \
  -H "$(AUTH "$RKEY")" "$CONTROL_BASE/connectors"

expect_status 403 "POST /connectors (reader)" \
  -H "$(AUTH "$RKEY")" -H "Content-Type: application/json" \
  -X POST "$CONTROL_BASE/connectors" -d '{"name":"x","slug":"x","endpoint":"http://x/"}'

# mcp_expect_denied POSTs one JSON-RPC request to the MCP plane and
# asserts both the HTTP status and the JSON-RPC error code the body
# carries. It is a sibling of 01-quickstart/05-profiles' mcp_call /
# mcp_call_expect_error helpers, needed because a denial at
# authentication/authorization time (internal/dataplane/transport/auth.go,
# AuthMiddleware) is a non-200 HTTP status carrying the JSON-RPC error
# object -- unlike an ungranted tools/call or a missing profile header
# (internal/dataplane/orchestrator/orchestrator.go), which 05-profiles'
# helper handles and which are always HTTP 200 with an "error" field. No
# session is established either way, so this needs none of mcp_call's
# Mcp-Session-Id bookkeeping.
mcp_expect_denied() {
  local label="$1" body="$2" bearer="$3" want_status="$4" want_code="$5"
  local full http_code resp code
  full="$(curl -sS \
    -H "$(AUTH "$bearer")" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -d "$body" \
    -w '\n%{http_code}' \
    "$MCP_URL")"
  http_code="${full##*$'\n'}"
  resp="${full%$'\n'*}"
  if [[ "$http_code" != "$want_status" ]]; then
    echo "FAIL: $label got HTTP $http_code, wanted $want_status: $resp" >&2
    exit 1
  fi
  code="$(jq -r '.error.code // empty' <<<"$resp")"
  if [[ "$code" != "$want_code" ]]; then
    echo "FAIL: $label got JSON-RPC error code '$code', wanted $want_code: $resp" >&2
    exit 1
  fi
  echo "  -> $label: HTTP $http_code, JSON-RPC error $code"
}

mcp_expect_denied "MCP initialize (reader)" \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"09","version":"1"}}}' \
  "$RKEY" 403 "-32006"

# ---------------------------------------------------------------------------
# 5. agent lens: platform.admin vs. the built-in "*.read" grant
# ---------------------------------------------------------------------------
#
# agent's built-in grant is {"mcp.*", "llm.*", "*.read"} (pkg/auth.go,
# NewRoleAuthorizer). "*.read" matches connector.read (a plain two-segment
# permission), but GET /tenants is gated on "platform.admin" -- a
# namespace matchPermission carves out from every wildcard rule, "*.read"
# included -- so it's denied regardless of the blanket read grant.

echo
echo "== agent lens =="

expect_status 403 "GET /tenants (agent)" \
  -H "$(AUTH "$AKEY")" "$CONTROL_BASE/tenants"

expect_status 200 "GET /connectors (agent, via *.read)" \
  -H "$(AUTH "$AKEY")" "$CONTROL_BASE/connectors"

# ---------------------------------------------------------------------------
# 6. revoke: immediate, not on the 60s cache TTL
# ---------------------------------------------------------------------------
#
# Revoking updates the store AND evicts the key's cached lookup from the
# running api-key Authenticator in the same request (Deps.KeyInvalidator,
# wired in cmd/gateway/main.go from the same *apikey.Authenticator
# instance the auth chain runs) -- so the very next request with the
# revoked key is already a 401, rather than surviving up to
# auth.api_keys.cache_ttl (default 30s).

echo
echo "== revoke =="

expect_status 204 "DELETE /api-keys/$AID (revoke 09-agent)" \
  -X DELETE -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" "$CONTROL_BASE/api-keys/$AID"

expect_status 401 "GET /connectors (revoked 09-agent)" \
  -H "$(AUTH "$AKEY")" "$CONTROL_BASE/connectors"

# ---------------------------------------------------------------------------
# 7. rotate: old plaintext dead, new plaintext live
# ---------------------------------------------------------------------------

echo
echo "== rotate =="

rotate_resp="$(curl -sSf -X POST -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" "$CONTROL_BASE/api-keys/$RID/rotate")"
NEWKEY="$(jq -r '.key' <<<"$rotate_resp")"
if [[ -z "$NEWKEY" || "$NEWKEY" == "null" ]]; then
  echo "FAIL: rotate did not return a new plaintext key: $rotate_resp" >&2
  exit 1
fi
echo "  -> rotated: new key $(jq -r '.prefix' <<<"$rotate_resp")..."

expect_status 200 "GET /connectors (new rotated key)" \
  -H "$(AUTH "$NEWKEY")" "$CONTROL_BASE/connectors"

expect_status 401 "GET /connectors (old, rotated-out key)" \
  -H "$(AUTH "$RKEY")" "$CONTROL_BASE/connectors"

# ---------------------------------------------------------------------------
# 8. rate-limit lockout
# ---------------------------------------------------------------------------
#
# internal/auth/ratelimit.go: MaxFailures auth failures from the same IP
# within Window trip a Lockout during which EVERY request from that IP on
# a non-exempt control-plane route gets 429 + Retry-After, even one
# carrying a valid credential (the check runs in RateLimitMiddleware,
# ahead of the authenticator -- a locked-out request never even reaches
# it). config.roles.yaml pins max_failures at 10 (Default()'s own value)
# and shortens lockout to 20s so this step doesn't have to wait 5 minutes
# (Default()'s lockout) before its last assertion.
#
# Each plane has its own lockout: cmd/gateway/main.go's run() builds one
# limiter per plane (same thresholds; only the API plane also applies the
# per-IP requests-per-minute cap) -- a bad key against the MCP or LLM
# plane never counts toward this lockout, and being locked out of the
# control plane does not block MCP/LLM traffic.
#
# The checks above already logged a couple of auth failures against this
# IP (the revoked and rotated-out key checks) -- ReportSuccess resets the
# failure counter on every successful auth, so start this step's count
# from a known-clean zero with one throwaway successful request first,
# then fire exactly MaxFailures (10) bogus-key requests.

echo
echo "== rate-limit lockout =="

curl -sSf -H "$(AUTH "$NEWKEY")" "$CONTROL_BASE/connectors" >/dev/null

for i in $(seq 1 10); do
  expect_status 401 "bogus key #$i" \
    -H "Authorization: Bearer gk_bogus_deadbeef_$i" "$CONTROL_BASE/connectors"
done

expect_status 429 "GET /connectors (valid key, IP locked out)" \
  -D "$HEADERS_TMP" \
  -H "$(AUTH "$NEWKEY")" "$CONTROL_BASE/connectors"

RETRY_AFTER="$(grep -i '^Retry-After:' "$HEADERS_TMP" | tail -n1 | sed -E 's/^[^:]+:[[:space:]]*//' | tr -d '\r\n')"
if [[ -z "$RETRY_AFTER" ]]; then
  echo "FAIL: 429 response carried no Retry-After header" >&2
  exit 1
fi
echo "  -> Retry-After: ${RETRY_AFTER}s"

# /api/v1/health is exempt from the lockout itself (isRateLimitExempt,
# internal/api/router.go) but not from reporting it: it's a public route
# (no auth), so it stays reachable for a locked-out caller, and its
# "rate_limit.locked_ips" counter (internal/auth.RateLimiter.LockedIPCount)
# reflects this IP's active lockout.
health_resp="$(curl -sSf "$CONTROL_BASE/health")"
locked_ips="$(jq -r '.rate_limit.locked_ips' <<<"$health_resp")"
echo "  -> GET /health: $health_resp"
if [[ "$locked_ips" -lt 1 ]]; then
  echo "FAIL: expected rate_limit.locked_ips >= 1, got $locked_ips" >&2
  exit 1
fi

echo "  -> waiting out the lockout (Retry-After: ${RETRY_AFTER}s)..."
sleep "$RETRY_AFTER"

expect_status 200 "GET /connectors (valid key, lockout expired)" \
  -H "$(AUTH "$NEWKEY")" "$CONTROL_BASE/connectors"

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

echo
echo "================================================================"
echo "09-roles-keys-and-rate-limits passed."
echo "Console:    $CONSOLE_URL"
echo "Admin key:  $GATEWAY_ADMIN_KEY"
echo
printf '%-12s %-8s %s\n' "KEY" "ROLE" "CAN DO"
printf '%-12s %-8s %s\n' "----------" "------" "------"
printf '%-12s %-8s %s\n' "(rotated)" "reader" "GET connectors/profiles/cache/analytics; no writes; no MCP plane"
printf '%-12s %-8s %s\n' "(revoked)" "agent" "GET any *.read route + the MCP/LLM planes; not /tenants"
echo
echo "The gateway's default compose config (no roles overlay) is restored"
echo "below by this script's cleanup trap."
echo "================================================================"
