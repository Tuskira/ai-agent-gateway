#!/usr/bin/env bash
#
# 10-kubernetes: bring up the gateway on a local kind cluster from
# deploy/k8s/overlays/kind plus this example's own tiny overlay (an
# in-cluster reference MCP server), bootstrap an admin key, register that
# server as a connector, drive it end to end over the MCP plane, then scale
# the llm plane to show each Deployment scales independently.
#
# Reuses deploy/k8s -- see overlay/kustomization.yaml for exactly what this
# example adds on top of it, and ../../deploy/README.md for what the kind
# overlay it builds on already gives you.
#
# Safe to re-run: the cluster and every object are reused/reconciled, not
# recreated, if they already exist.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
OVERLAY_DIR="$SCRIPT_DIR/overlay"

CLUSTER="${CLUSTER:-ai-gateway-example}"
NAMESPACE="ai-gateway"
LOCAL_IMAGE="tusk-ai-secured-gateway:example"

# NodePorts (30080/30081/30082, per deploy/k8s/overlays/kind) aren't used
# here -- port-forward works the same against a real cluster and a kind
# one, and 01-quickstart's compose stack already owns 8080-8082 on the
# host, so this example forwards to 1808x instead to avoid a collision if
# both examples are running at once.
# Local ports for the kubectl port-forwards. Deliberately far from the
# compose stack's 8080-8082 and from common dev ports; override if they
# clash with something on your machine.
MCP_LOCAL_PORT="${MCP_LOCAL_PORT:-28080}"
API_LOCAL_PORT="${API_LOCAL_PORT:-28081}"
LLM_LOCAL_PORT="${LLM_LOCAL_PORT:-28082}"

CONTROL_BASE="http://localhost:$API_LOCAL_PORT/api/v1"
MCP_URL="http://localhost:$MCP_LOCAL_PORT/mcp"
CONSOLE_URL="http://localhost:$API_LOCAL_PORT"

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "FAIL: '$1' is required but was not found in PATH. $2" >&2
    exit 1
  fi
}

echo "check: required tools (kind, kubectl, docker, curl, jq)"
require_cmd kind "Install kind, e.g. 'brew install kind'."
require_cmd kubectl "Install kubectl, e.g. 'brew install kubectl'."
require_cmd docker "Install Docker Desktop or Docker Engine."
require_cmd curl "Install curl."
require_cmd jq "Install jq (e.g. 'brew install jq' or 'apt-get install jq')."

port_open() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# ---------------------------------------------------------------------------
# 1. kind cluster (reuse if it already exists)
# ---------------------------------------------------------------------------

if [[ "$(kind get clusters 2>/dev/null | grep -cx "$CLUSTER")" -gt 0 ]]; then
  echo "check: kind cluster '$CLUSTER' already exists -- reusing it"
else
  echo "check: creating kind cluster '$CLUSTER'"
  kind create cluster --name "$CLUSTER"
fi

# ---------------------------------------------------------------------------
# 2. the gateway image: build+load locally, or pull an existing one
# ---------------------------------------------------------------------------

if [[ -n "${GATEWAY_IMAGE:-}" ]]; then
  IMAGE="$GATEWAY_IMAGE"
  echo "check: GATEWAY_IMAGE set -- using '$IMAGE' from its registry (no local build, no kind load)"
else
  IMAGE="$LOCAL_IMAGE"
  echo "check: building '$IMAGE' from $ROOT_DIR (Dockerfile builds the web console + the Go binary; ~3-4 min)"
  docker build -t "$IMAGE" "$ROOT_DIR"
  echo "check: loading '$IMAGE' into kind cluster '$CLUSTER'"
  kind load docker-image "$IMAGE" --name "$CLUSTER"
fi

# ---------------------------------------------------------------------------
# 3. render + apply the overlay
# ---------------------------------------------------------------------------

echo "check: rendering overlay/ (kubectl kustomize)"
MANIFEST="$(kubectl kustomize "$OVERLAY_DIR")"

if [[ -n "${GATEWAY_IMAGE:-}" ]]; then
  # overlay/kustomization.yaml's `images:` override always renders
  # "$LOCAL_IMAGE" for the three plane Deployments + the migrate Job;
  # swap it for the registry image at apply time instead of maintaining a
  # second overlay just to change one string.
  MANIFEST="${MANIFEST//image: $LOCAL_IMAGE/image: $IMAGE}"
fi

echo "check: applying to namespace '$NAMESPACE'"
kubectl apply -f - <<<"$MANIFEST"

# ---------------------------------------------------------------------------
# 4. wait for postgres, the migrate Job, and all four Deployments
# ---------------------------------------------------------------------------

echo "check: waiting for postgres (up to 2m)"
kubectl -n "$NAMESPACE" rollout status statefulset/postgres --timeout=120s

echo "check: waiting for the migrate Job to complete (up to 3m)"
kubectl -n "$NAMESPACE" wait --for=condition=complete job/gateway-migrate --timeout=180s

echo "check: waiting for the plane Deployments and the everything server (up to 3m each)"
for d in gateway-api gateway-mcp gateway-llm everything; do
  kubectl -n "$NAMESPACE" rollout status "deploy/$d" --timeout=180s
done

echo "check: pods in '$NAMESPACE'"
kubectl -n "$NAMESPACE" get pods

# ---------------------------------------------------------------------------
# 5. port-forward the api and mcp planes
# ---------------------------------------------------------------------------

PF_PIDS=()
HEADERS_TMP="$(mktemp)"
cleanup() {
  local pid
  for pid in ${PF_PIDS[@]+"${PF_PIDS[@]}"}; do
    kill "$pid" >/dev/null 2>&1 || true
  done
  rm -f "$HEADERS_TMP"
}
trap cleanup EXIT

start_port_forward() {
  local svc="$1" local_port="$2" remote_port="$3"
  # The port-forwards this script starts are killed by its exit trap, so
  # a busy port is never ours -- it is some other local service. Refuse
  # rather than talk to the wrong thing.
  if port_open "$local_port"; then
    echo "FAIL: :$local_port is already in use by something else on this machine; set MCP_LOCAL_PORT / API_LOCAL_PORT / LLM_LOCAL_PORT to a free port and re-run" >&2
    exit 1
  fi
  kubectl -n "$NAMESPACE" port-forward "svc/$svc" "$local_port:$remote_port" >/dev/null 2>&1 &
  PF_PIDS+=("$!")
}

echo "check: port-forwarding gateway-api ($API_LOCAL_PORT->8081), gateway-mcp ($MCP_LOCAL_PORT->8080), gateway-llm ($LLM_LOCAL_PORT->8082)"
start_port_forward gateway-api "$API_LOCAL_PORT" 8081
start_port_forward gateway-mcp "$MCP_LOCAL_PORT" 8080
start_port_forward gateway-llm "$LLM_LOCAL_PORT" 8082

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
wait_for_health "MCP plane" "http://localhost:$MCP_LOCAL_PORT/health"
wait_for_health "LLM plane" "http://localhost:$LLM_LOCAL_PORT/health"

# ---------------------------------------------------------------------------
# 6. bootstrap an admin API key
# ---------------------------------------------------------------------------

if [[ -n "${GATEWAY_ADMIN_KEY:-}" ]]; then
  echo "check: reusing GATEWAY_ADMIN_KEY from the environment"
else
  echo "check: bootstrapping an admin API key (tenant 'default')"
  # Same refuses-a-second-key-without-force fallback as 01-quickstart's
  # run.sh -- see its comment on bootstrap-key for why.
  if out="$(kubectl -n "$NAMESPACE" exec deploy/gateway-api -- /gateway bootstrap-key 2>/dev/null)" && [[ -n "$out" ]]; then
    echo "  -> bootstrapped a new admin key"
  else
    echo "  -> tenant 'default' already has an admin key; minting another with --force"
    out="$(kubectl -n "$NAMESPACE" exec deploy/gateway-api -- /gateway bootstrap-key --force 2>/dev/null)" || true
    if [[ -z "$out" ]]; then
      echo "FAIL: could not bootstrap an admin API key. Check 'kubectl -n $NAMESPACE logs deploy/gateway-api'." >&2
      exit 1
    fi
  fi
  GATEWAY_ADMIN_KEY="$out"
fi
export GATEWAY_ADMIN_KEY

# ---------------------------------------------------------------------------
# 7. register the in-cluster "everything" connector (idempotent: reuse by
#    slug), then probe its health and discover its tools
# ---------------------------------------------------------------------------

echo "check: looking for an existing 'everything' connector"
existing="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "$CONTROL_BASE/connectors?limit=500" | jq -r '.items[] | select(.slug=="everything") | .id' | head -n1)"

if [[ -n "$existing" ]]; then
  CONNECTOR_ID="$existing"
  echo "  -> reusing connector $CONNECTOR_ID"
else
  echo "check: registering the 'everything' connector"
  # everything.ai-gateway.svc.cluster.local: the everything Deployment/
  # Service this example's overlay adds (overlay/everything.yaml),
  # reachable in-cluster from the gateway pods -- see that file's comment
  # on why this runs in-cluster instead of on the host.
  CONNECTOR_ID="$(curl -sSf -X POST "$CONTROL_BASE/connectors" \
    -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
    -H "Content-Type: application/json" \
    -d '{
          "name": "everything",
          "slug": "everything",
          "endpoint": "http://everything.ai-gateway.svc.cluster.local:3001/mcp"
        }' | jq -r '.id')"

  if [[ -z "$CONNECTOR_ID" || "$CONNECTOR_ID" == "null" ]]; then
    echo "FAIL: connector creation did not return an id" >&2
    exit 1
  fi
  echo "  -> created connector $CONNECTOR_ID"
fi

# Both of these run against gateway-api, which has GATEWAY_MCP_ENABLED=false:
# cmd/gateway builds the data plane whenever EITHER plane needs it, so an
# api-only pod still serves connector ops (and the console's Health/Discover
# buttons) without opening an MCP listener. Identical assertions to
# 01-quickstart/run.sh, where both planes share one process.
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

# Same floor as the tools/list check below, and for the same reason:
# server-everything's tool count isn't pinned and `npx -y` pulls latest.
if [[ "$discovered_count" -lt 10 ]]; then
  echo "FAIL: expected at least 10 tools from the 'everything' connector, got $discovered_count" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 8. MCP handshake: initialize -> notifications/initialized -> tools/list -> tools/call
#    (same helpers and same sequence as 01-quickstart/run.sh)
# ---------------------------------------------------------------------------

SESSION_ID=""
MCP_RESP=""

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
mcp_call "initialize" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"10-kubernetes","version":"0.1.0"}}}'
echo "  -> protocolVersion=$(jq -r '.result.protocolVersion' <<<"$MCP_RESP"), session=$SESSION_ID"
if [[ -z "$SESSION_ID" ]]; then
  echo "FAIL: initialize did not return an Mcp-Session-Id header" >&2
  exit 1
fi

echo "check: MCP notifications/initialized"
mcp_notify "notifications/initialized" '{"jsonrpc":"2.0","method":"notifications/initialized"}'

echo "check: MCP tools/list"
mcp_call "tools/list" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
list_resp="$MCP_RESP"
tool_count="$(jq '[.result.tools[] | select(.name | startswith("everything__"))] | length' <<<"$list_resp")"
echo "  -> $tool_count tool(s) advertised under the 'everything' connector"

# See 01-quickstart/run.sh's identical comment: server-everything's tool
# count isn't pinned in this repo and `npx -y` always pulls latest, so
# assert a floor, not an exact number.
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

# ---------------------------------------------------------------------------
# 9. scale demo: llm plane up, mcp plane stays at 1
# ---------------------------------------------------------------------------

# gateway-llm's own manifest (deploy/k8s/base/deployment-llm.yaml) already
# defaults to 2 replicas (matching its HPA's minReplicas) -- scale past
# that, to 3, so this step demonstrates an actual change. A re-run of this
# script resets it back to 2 first (kubectl apply reconciles `replicas`
# back to what the manifest declares) before scaling to 3 again, so the
# demo is idempotent.
echo "check: scaling gateway-llm to 3 replicas"
kubectl -n "$NAMESPACE" scale deploy/gateway-llm --replicas=3
kubectl -n "$NAMESPACE" rollout status deploy/gateway-llm --timeout=120s
ready="$(kubectl -n "$NAMESPACE" get deploy/gateway-llm -o jsonpath='{.status.readyReplicas}')"
if [[ "$ready" != "3" ]]; then
  echo "FAIL: expected 3 ready replicas on gateway-llm, got $ready" >&2
  exit 1
fi
echo "  -> gateway-llm: 3/3 ready"

mcp_replicas="$(kubectl -n "$NAMESPACE" get deploy/gateway-mcp -o jsonpath='{.status.readyReplicas}')"
echo "  -> gateway-mcp stays at $mcp_replicas/1: MCP sessions are in-process with sessions.store: memory --"
echo "     scaling needs the Redis session store (deploy/k8s/overlays/kind-redis); see deploy/README.md's 'Scaling per plane'."

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

echo
echo "================================================================"
echo "10-kubernetes passed."
echo "Console:    $CONSOLE_URL"
echo "Admin key:  $GATEWAY_ADMIN_KEY"
echo "Cleanup:    kind delete cluster --name $CLUSTER"
echo "================================================================"
