#!/usr/bin/env bash
#
# 04-python-agent: bring up the gateway's compose stack, register the
# reference "everything" MCP server as a connector (reusing 01-quickstart's
# and 05-profiles' setup if it's already there), create a "04-python-agent"
# profile granting exactly get-sum and echo, mint a fresh agent key, then
# run agent.py against both planes -- full tool-use mode if ANTHROPIC_API_KEY
# is set, --mcp-only otherwise (which is what CI runs: no LLM credential is
# available there). With NEBIUS_API_KEY (or TOGETHER_API_KEY) it also
# registers the open-weight models kimi-k3 and glm-5.3 and runs agent.py
# --chat, switching model mid-conversation (/model): Claude (with
# ANTHROPIC_API_KEY), then kimi-k3 and glm-5.3.
#
# Safe to re-run: every setup step checks current state first and reuses it
# instead of failing or duplicating work. The one exception is the agent API
# key, minted fresh every run for the same reason 05-profiles' is (its
# plaintext is only ever returned once) -- see that example's README.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.yml"

EVERYTHING_PORT=23014
EVERYTHING_PID_FILE="$SCRIPT_DIR/.everything.pid"
EVERYTHING_LOG_FILE="$SCRIPT_DIR/.everything.log"

CONTROL_BASE="http://localhost:8081/api/v1"
CONSOLE_URL="http://localhost:8081"

PROFILE_NAME="04-python-agent"
AGENT_KEY_NAME="04-python-agent"
VENV_DIR="$SCRIPT_DIR/.venv"

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

echo "check: required tools (docker, curl, jq, npx, python3)"
require_cmd docker "Install Docker Desktop or Docker Engine."
require_cmd curl "Install curl."
require_cmd jq "Install jq (e.g. 'brew install jq' or 'apt-get install jq')."
require_cmd npx "Install Node.js -- npx ships with npm."
require_cmd python3 "Install Python 3.10+."
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

# See 01-quickstart's README for the note on this floor: server-everything's
# exact tool count isn't pinned anywhere in this repo, and `npx -y` always
# pulls the latest published version.
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
# 7. create (or reuse) the "04-python-agent" profile and grant its 2 tools
# ---------------------------------------------------------------------------
#
# Same pattern as 05-profiles: POST /profiles isn't idempotent by itself (a
# second POST with the same name creates a second profile, since only the
# derived slug is unique), so look it up by name first. PUT
# /profiles/{id}/tools fully replaces the allow-list, so it's called
# unconditionally.

echo "check: ensuring the '$PROFILE_NAME' profile exists"
PROFILE_ID="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/profiles?limit=500" | jq -r --arg n "$PROFILE_NAME" '.items[] | select(.name==$n) | .id' | head -n1)"

if [[ -n "$PROFILE_ID" ]]; then
  echo "  -> reusing profile '$PROFILE_NAME' ($PROFILE_ID)"
else
  PROFILE_ID="$(curl -sSf -X POST "$CONTROL_BASE/profiles" \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg n "$PROFILE_NAME" '{name: $n}')" | jq -r '.id')"
  if [[ -z "$PROFILE_ID" || "$PROFILE_ID" == "null" ]]; then
    echo "FAIL: profile creation for '$PROFILE_NAME' did not return an id" >&2
    exit 1
  fi
  echo "  -> created profile '$PROFILE_NAME' ($PROFILE_ID)"
fi

echo "check: granting '$PROFILE_NAME' exactly 2 tools (get-sum, echo)"
tools_json="$(jq -n --arg cid "$CONNECTOR_ID" \
  '{tools: [{connector_id: $cid, tool_name: "get-sum"}, {connector_id: $cid, tool_name: "echo"}]}')"
curl -sSf -X PUT "$CONTROL_BASE/profiles/$PROFILE_ID/tools" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d "$tools_json" >/dev/null
echo "  -> granted 2 tool(s)"

# ---------------------------------------------------------------------------
# 8. mint an agent-role API key
# ---------------------------------------------------------------------------
#
# Not idempotent by design, same as 05-profiles step 3: an API key's
# plaintext is only ever returned once (internal/api/handlers/apikeys.go,
# APIKeys.Create), so there is nothing to "look up and reuse". Every run
# mints a fresh "04-python-agent" key and leaves any previous one active but
# orphaned -- see the README's Cleanup section.

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
# 9. create the example's venv and install its two dependencies
# ---------------------------------------------------------------------------

if [[ -x "$VENV_DIR/bin/python3" ]]; then
  echo "check: reusing existing venv ($VENV_DIR)"
else
  echo "check: creating venv ($VENV_DIR)"
  python3 -m venv "$VENV_DIR"
fi

echo "check: pip install -r requirements.txt"
"$VENV_DIR/bin/pip" install -q --upgrade pip >/dev/null
"$VENV_DIR/bin/pip" install -q -r "$SCRIPT_DIR/requirements.txt"

# mcp's own Requires-Python is the source of truth for the minimum
# interpreter version -- read it from the venv's installed package metadata
# rather than hardcoding "3.10" here, so a future mcp release tightening (or
# loosening) it is caught automatically.
py_check="$("$VENV_DIR/bin/python3" - <<'PY'
import re
import sys
from importlib import metadata

spec = metadata.metadata("mcp")["Requires-Python"] or ""
m = re.search(r">=\s*(\d+)\.(\d+)", spec)
if not m:
    print(f"UNKNOWN|{spec}")
else:
    need = (int(m.group(1)), int(m.group(2)))
    print(f"{'OK' if sys.version_info[:2] >= need else 'TOO_OLD'}|{spec}")
PY
)"
py_status="${py_check%%|*}"
py_spec="${py_check#*|}"
venv_py_version="$("$VENV_DIR/bin/python3" -c 'import sys; print(".".join(map(str, sys.version_info[:3])))')"
if [[ "$py_status" == "TOO_OLD" ]]; then
  echo "FAIL: venv python3 is $venv_py_version, but mcp requires Requires-Python $py_spec" >&2
  exit 1
fi
echo "  -> venv python3 $venv_py_version satisfies mcp's Requires-Python ($py_spec)"

# ---------------------------------------------------------------------------
# 10. run agent.py: full tool-use mode with a real ANTHROPIC_API_KEY, else
#     --mcp-only (this is the path CI takes, since no LLM credential is set
#     there)
# ---------------------------------------------------------------------------

EXPECTED_SUM=20255028 # 20250926 + 4102, the question agent.py asks / the sum --mcp-only computes directly

EXTRA_ARGS=()
if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "check: ANTHROPIC_API_KEY is set -- running agent.py in full mode (real Anthropic calls)"
  mode="full"
else
  echo "check: ANTHROPIC_API_KEY is not set -- running agent.py --mcp-only (no LLM call)"
  mode="mcp-only"
  EXTRA_ARGS+=(--mcp-only)
fi

set +e
agent_output="$(GATEWAY_URL="http://localhost" GATEWAY_KEY="$AGENT_KEY" PROFILE="$PROFILE_NAME" \
  "$VENV_DIR/bin/python3" "$SCRIPT_DIR/agent.py" ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"} 2>&1)"
agent_rc=$?
set -e

echo "---- agent.py output ----"
echo "$agent_output"
echo "--------------------------"

if [[ $agent_rc -ne 0 ]]; then
  echo "FAIL: agent.py ($mode mode) exited $agent_rc" >&2
  exit 1
fi
if [[ "$agent_output" != *"$EXPECTED_SUM"* ]]; then
  echo "FAIL: agent.py ($mode mode) output did not contain the expected sum ($EXPECTED_SUM)" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 11. open-weight models: register kimi-k3 and glm-5.3, then one agent.py
#     --chat conversation that switches model at runtime (/model): Claude
#     (with ANTHROPIC_API_KEY), then kimi-k3, then glm-5.3
# ---------------------------------------------------------------------------
#
# The same thing the console's Add model form does with Vendor "Nebius" (or
# "Together AI") and Credential "Accept API key": the vendor key is stored
# once as an encrypted credential, and each model name is an
# OpenAI-compatible target that references it by name. The gateway
# translates each /v1/messages call to Chat Completions and sends the stored
# key, so those two need no ANTHROPIC_API_KEY (only a Claude model does).
# Skipped without a vendor key -- CI sets none.

open_weight="skipped (set NEBIUS_API_KEY or TOGETHER_API_KEY)"
OW_LABEL=""
if [[ -n "${NEBIUS_API_KEY:-}" ]]; then
  OW_LABEL="nebius" OW_BASE_URL="https://api.tokenfactory.eu-west2.nebius.com/v1/" OW_KEY="$NEBIUS_API_KEY"
elif [[ -n "${TOGETHER_API_KEY:-}" ]]; then
  OW_LABEL="together" OW_BASE_URL="https://api.together.ai/v1" OW_KEY="$TOGETHER_API_KEY"
else
  echo "skipped: open-weight models (set NEBIUS_API_KEY or TOGETHER_API_KEY to switch agent.py between kimi-k3 and glm-5.3)"
fi

if [[ -n "$OW_LABEL" ]]; then
  OW_CREDENTIAL="04-$OW_LABEL-key"

  # Same create-or-rotate as 06-credentials-and-headers step 5 (a second
  # POST for a name is a 409). The real key only travels through the
  # environment and stdin, never a command line another user could read
  # in ps.
  echo "check: storing the $OW_LABEL API key as credential '$OW_CREDENTIAL'"
  create_status="$(OW_KEY="$OW_KEY" jq -n --arg name "$OW_CREDENTIAL" \
      '{name: $name, type: "api_key", payload: {api_key: env.OW_KEY}}' |
    curl -sS -o /dev/null -w '%{http_code}' -X POST "$CONTROL_BASE/credentials" \
      -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
      --data-binary @-)"
  case "$create_status" in
    201)
      echo "  -> created (field: api_key)"
      ;;
    409)
      OW_KEY="$OW_KEY" jq -n '{payload: {api_key: env.OW_KEY}}' |
        curl -sSf -o /dev/null -X PUT "$CONTROL_BASE/credentials/$OW_CREDENTIAL" \
          -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
          --data-binary @-
      echo "  -> already existed; rotated to this key"
      ;;
    *)
      echo "FAIL: credential create got unexpected HTTP $create_status" >&2
      exit 1
      ;;
  esac

  # ensure_model NAME VENDOR_MODEL: register NAME, or replace this tenant's
  # row of that name on a re-run (PUT replaces the whole row, so enabled is
  # sent explicitly).
  ensure_model() {
    local name="$1" vendor_model="$2" body model_id
    body="$(jq -n --arg name "$name" --arg model "$vendor_model" --arg base "$OW_BASE_URL" \
      --arg label "$OW_LABEL" --arg cred "$OW_CREDENTIAL" \
      '{name: $name, enabled: true, description: "04-python-agent: open-weight model",
        targets: [{vendor: "openai_compat", model: $model, base_url: $base, label: $label, credential: $cred}]}')"
    model_id="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" "$CONTROL_BASE/models?limit=500" |
      jq -r --arg n "$name" '.items[] | select(.name==$n and .scope=="tenant") | .id' | head -n1)"
    if [[ -n "$model_id" ]]; then
      curl -sSf -o /dev/null -X PUT "$CONTROL_BASE/models/$model_id" \
        -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" -d "$body"
      echo "  -> updated model '$name' -> $vendor_model on $OW_LABEL"
    else
      curl -sSf -o /dev/null -X POST "$CONTROL_BASE/models" \
        -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" -d "$body"
      echo "  -> registered model '$name' -> $vendor_model on $OW_LABEL"
    fi
  }

  echo "check: registering the open-weight models"
  ensure_model "kimi-k3" "moonshotai/Kimi-K3"
  ensure_model "glm-5.3" "zai-org/GLM-5.3"

  # One conversation, the model switched at runtime like Claude Code's
  # /model: the question on Claude (when ANTHROPIC_API_KEY is set, as in
  # step 10), then /model kimi-k3 and /model glm-5.3 in addition, over the
  # same history. kimi-k3 and glm-5.3 need no ANTHROPIC_API_KEY: the gateway
  # drops the caller's key and sends the stored $OW_LABEL one.
  question="What is 20250926 + 4102? Use the tool."
  chat_models=(kimi-k3 glm-5.3)
  if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
    chat_models=(claude-sonnet-5 "${chat_models[@]}")
  fi
  chat_lines=("$question")
  for model in "${chat_models[@]:1}"; do
    chat_lines+=("/model $model" "$question")
  done
  echo "check: agent.py --chat on ${chat_models[0]}, then /model ${chat_models[*]:1}"
  set +e
  ow_output="$(printf '%s\n' "${chat_lines[@]}" |
    GATEWAY_URL="http://localhost" GATEWAY_KEY="$AGENT_KEY" PROFILE="$PROFILE_NAME" MODEL="${chat_models[0]}" \
      "$VENV_DIR/bin/python3" "$SCRIPT_DIR/agent.py" --chat 2>&1)"
  ow_rc=$?
  set -e

  echo "---- agent.py --chat output ----"
  echo "$ow_output"
  echo "--------------------------------"

  if [[ $ow_rc -ne 0 ]]; then
    echo "FAIL: agent.py --chat exited $ow_rc" >&2
    exit 1
  fi
  for model in "${chat_models[@]}"; do
    # Kimi and GLM may write the sum with thousands separators (20,255,028).
    answer="$(grep -F "[$model] Answer:" <<<"$ow_output" | tr -d ',')"
    if [[ "$answer" != *"$EXPECTED_SUM"* ]]; then
      echo "FAIL: no [$model] answer with the expected sum ($EXPECTED_SUM)" >&2
      exit 1
    fi
  done
  open_weight="passed: ${chat_models[*]} in one --chat conversation (kimi-k3, glm-5.3 on $OW_LABEL)"
fi

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

echo
echo "================================================================"
echo "04-python-agent passed ($mode mode)."
echo "Model switch: $open_weight"
echo "Console:      $CONSOLE_URL"
echo "Agent key:    $AGENT_KEY"
echo
echo "To run the full mode yourself (real Anthropic calls, your own key):"
echo "  export GATEWAY_URL=http://localhost"
echo "  export GATEWAY_KEY=$AGENT_KEY"
echo "  export PROFILE=$PROFILE_NAME"
echo "  export ANTHROPIC_API_KEY=sk-ant-..."
echo "  $VENV_DIR/bin/python3 $SCRIPT_DIR/agent.py"
echo "================================================================"
