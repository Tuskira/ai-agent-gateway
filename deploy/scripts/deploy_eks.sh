#!/usr/bin/env bash
# Deploys one image tag of the gateway to an EKS cluster using the
# deploy/k8s/overlays/eks kustomize overlay. Runnable by hand, or from
# any CD system that provides the environment below (this repository
# itself does not deploy anywhere).
#
# Order, mirroring what a Helm pre-upgrade hook would do (see
# deploy/README.md, "Upgrading"):
#   1. render the overlay and fill its ${GW_*} placeholders (envsubst,
#      restricted to exactly those names so nothing else in the manifests
#      is touched), then server-side dry-run the result;
#   2. run the gateway-migrate Job for this image and wait for it;
#   3. apply everything else and wait for the three plane rollouts;
#      a failed rollout is rolled back (kubectl rollout undo) and fails
#      the deploy;
#   4. probe each plane's health endpoint through the ALB hostnames
#      (warning only: the very first deploy can outrun ALB/DNS creation).
#
# AWS credentials must already be in the environment. Nothing printed
# here includes account ids, ARNs, subnets or database endpoints, so
# the output is safe for a shared log.
#
# Required env: AWS_REGION, K8S_CLUSTER, ENVIRONMENT, plus every GW_*
# variable listed in REQUIRED_GW_VARS below.
# Optional env: DEPLOYMENT_FREEZE (true blocks the deploy),
# SLACK_WEBHOOK_URL, DORA_PUSHGATEWAY_URL, GIT_COMMIT, RUN_URL.
set -euo pipefail

REQUIRED_GW_VARS=(
  GW_IMAGE_REPOSITORY GW_IMAGE_TAG
  GW_API_HOST GW_MCP_HOST GW_LLM_HOST
  GW_ALB_GROUP_NAME GW_ALB_SUBNETS GW_ALB_CERT_ARN GW_ALB_SSL_POLICY GW_ALB_IDLE_TIMEOUT
  GW_VPC_CIDR GW_NODE_TOLERATION_KEY
)

NAMESPACE=ai-gateway
DEPLOYMENTS=(gateway-api gateway-mcp gateway-llm)
ROLLOUT_TIMEOUT=${ROLLOUT_TIMEOUT:-10m}
MIGRATE_TIMEOUT_SECONDS=${MIGRATE_TIMEOUT_SECONDS:-300}
INGRESS_PROBE_SECONDS=${INGRESS_PROBE_SECONDS:-300}

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OVERLAY="${REPO_ROOT}/deploy/k8s/overlays/eks"
WORK_DIR="${RUNNER_TEMP:-$(mktemp -d)}/gateway-deploy"
mkdir -p "${WORK_DIR}"

log() { echo "[deploy] $*" >&2; }
fail() {
  echo "::error::$*"
  notify failure "$*"
  exit 1
}

# notify <success|failure|blocked> <text>: Slack, when a webhook is set.
notify() {
  local status=$1 text=$2
  [ -n "${SLACK_WEBHOOK_URL:-}" ] || return 0
  local icon=":rocket:"
  [ "${status}" = "success" ] || icon=":rotating_light:"
  local payload
  payload=$(jq -n \
    --arg text "${icon} tusk-ai-secured-gateway ${status} on ${ENVIRONMENT}: ${text}
*Image tag:* ${GW_IMAGE_TAG:-n/a}
*Run:* <${RUN_URL:-n/a}|View workflow run>" \
    '{text: $text, username: "DeploymentBot"}')
  curl -sS -X POST -H 'Content-type: application/json' --data "${payload}" \
    "${SLACK_WEBHOOK_URL}" >/dev/null || log "Slack notification failed (non-fatal)"
}

# dora_metrics: same Pushgateway series the helm-charts-launchpad deploy
# scripts push (deployment_timestamp_seconds / lead_time / success), so
# this service shows up on the same DORA dashboard. Never fails a deploy.
dora_metrics() {
  [ -n "${DORA_PUSHGATEWAY_URL:-}" ] || return 0
  (
    set +e
    local now commit_time
    now=$(date +%s)
    commit_time=$(git -C "${REPO_ROOT}" log -1 --format=%ct "${GIT_COMMIT:-HEAD}" 2>/dev/null || echo "${now}")
    cat <<METRICS | curl -sf --data-binary @- \
      "${DORA_PUSHGATEWAY_URL}/metrics/job/deployments/service/tusk-ai-secured-gateway/environment/${ENVIRONMENT}/instance/tusk-ai-secured-gateway-${GITHUB_RUN_NUMBER:-manual}" \
      && log "DORA metrics pushed" || log "DORA push failed (non-fatal)"
# HELP deployment_timestamp_seconds Unix timestamp when deployment completed
# TYPE deployment_timestamp_seconds gauge
deployment_timestamp_seconds ${now}
# HELP deployment_lead_time_seconds Seconds from git commit to deployment
# TYPE deployment_lead_time_seconds gauge
deployment_lead_time_seconds $((now - commit_time))
# HELP deployment_success Whether the deployment succeeded (1) or failed (0)
# TYPE deployment_success gauge
deployment_success 1
METRICS
  ) || true
}

# --- 0. preconditions -------------------------------------------------------
: "${AWS_REGION:?AWS_REGION missing}"
: "${K8S_CLUSTER:?K8S_CLUSTER missing}"
: "${ENVIRONMENT:?ENVIRONMENT missing}"
for v in "${REQUIRED_GW_VARS[@]}"; do
  [ -n "${!v:-}" ] || fail "required variable ${v} is not set"
done
for bin in aws kubectl envsubst jq curl; do
  command -v "${bin}" >/dev/null || fail "${bin} is not installed on this runner"
done

if [ "${DEPLOYMENT_FREEZE:-false}" = "true" ]; then
  notify blocked "deployment freeze is active"
  echo "::error::Deployment freeze is active for ${ENVIRONMENT}; not deploying."
  exit 1
fi

# --- 1. cluster access + render ---------------------------------------------
export KUBECONFIG="${WORK_DIR}/kubeconfig"
aws eks update-kubeconfig --region "${AWS_REGION}" --name "${K8S_CLUSTER}" \
  --kubeconfig "${KUBECONFIG}" >/dev/null
# A namespace-scoped deploy identity cannot read the (cluster-scoped)
# Namespace object itself, so probe for the Secret instead: it fails the
# same way whether the namespace or the Secret is missing.
kubectl -n "${NAMESPACE}" get secret gateway-secrets >/dev/null 2>&1 ||
  fail "secret ${NAMESPACE}/gateway-secrets not found or not readable (the namespace and the Secret are created by infrastructure-as-code)"

log "rendering ${OVERLAY#"${REPO_ROOT}/"} for tag ${GW_IMAGE_TAG}"
RENDERED="${WORK_DIR}/rendered.yaml"
# shellcheck disable=SC2016 # literal ${NAME} list for envsubst's allowlist
SUBST_VARS=$(printf '${%s} ' "${REQUIRED_GW_VARS[@]}")
kubectl kustomize "${OVERLAY}" | envsubst "${SUBST_VARS}" >"${RENDERED}"
# shellcheck disable=SC2016 # matching a literal ${GW_
if grep -q '\${GW_' "${RENDERED}"; then
  fail "unresolved \${GW_*} placeholder in the rendered manifests"
fi

# The migrate Job is left out: the previous run's Job still exists and its
# pod template is immutable, so a dry run against it always fails. It is
# deleted and re-created in step 2 instead.
kubectl apply --dry-run=server -f "${RENDERED}" -l 'app.kubernetes.io/component!=migrate' >/dev/null ||
  fail "server-side dry run rejected the rendered manifests"

# --- 2. migrate ---------------------------------------------------------------
# The Job mounts gateway-config and reads gateway-env, so the shared
# objects that carry no plane/migrate component label go first
# (ConfigMaps, NetworkPolicy, Ingress, Redis) -- the plane Deployments,
# Services, HPAs and PDBs all carry one and wait for step 3.
kubectl apply -f "${RENDERED}" -l 'app.kubernetes.io/component notin (migrate,api,mcp,llm)' >/dev/null
# gateway-mcp pings its session store at startup and exits if it can't
# reach it, so let the overlay's in-cluster Redis become ready first.
if grep -q '^  name: redis$' "${RENDERED}"; then
  kubectl -n "${NAMESPACE}" rollout status deployment/redis --timeout="${ROLLOUT_TIMEOUT}" ||
    fail "redis (MCP session store) did not become ready"
fi

# Job pod templates are immutable, so the previous run is deleted first.
log "running gateway-migrate"
kubectl -n "${NAMESPACE}" delete job gateway-migrate --ignore-not-found --wait=true >/dev/null
kubectl apply -f "${RENDERED}" -l app.kubernetes.io/component=migrate >/dev/null

deadline=$(($(date +%s) + MIGRATE_TIMEOUT_SECONDS))
while :; do
  complete=$(kubectl -n "${NAMESPACE}" get job gateway-migrate \
    -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}')
  failed=$(kubectl -n "${NAMESPACE}" get job gateway-migrate \
    -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}')
  [ "${complete}" = "True" ] && break
  if [ "${failed}" = "True" ] || [ "$(date +%s)" -ge "${deadline}" ]; then
    kubectl -n "${NAMESPACE}" get pods -l app.kubernetes.io/component=migrate || true
    fail "gateway-migrate did not complete (inspect: kubectl -n ${NAMESPACE} logs job/gateway-migrate)"
  fi
  sleep 5
done
log "migrations applied"

# --- 3. roll out ----------------------------------------------------------------
# "component!=migrate" also re-selects step 2's unlabelled shared objects
# (a no-op unless they changed in between).
kubectl apply -f "${RENDERED}" -l 'app.kubernetes.io/component!=migrate'

rollout_failed=""
for d in "${DEPLOYMENTS[@]}"; do
  if ! kubectl -n "${NAMESPACE}" rollout status "deployment/${d}" --timeout="${ROLLOUT_TIMEOUT}"; then
    rollout_failed="${rollout_failed} ${d}"
  fi
done
if [ -n "${rollout_failed}" ]; then
  log "rollout failed for:${rollout_failed}; rolling all planes back"
  kubectl -n "${NAMESPACE}" get pods -l app.kubernetes.io/name=gateway || true
  for d in "${DEPLOYMENTS[@]}"; do
    kubectl -n "${NAMESPACE}" rollout undo "deployment/${d}" || true
  done
  fail "rollout failed for:${rollout_failed} (rolled back to the previous revision)"
fi
log "all planes rolled out"

# --- 4. probe through the ALB -------------------------------------------------
probe() {
  local url=$1 deadline=$(($(date +%s) + INGRESS_PROBE_SECONDS)) code
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${url}" || true)
    [ "${code}" = "200" ] && return 0
    sleep 10
  done
  return 1
}
probe_warnings=0
for url in "https://${GW_API_HOST}/api/v1/health" "https://${GW_MCP_HOST}/health" "https://${GW_LLM_HOST}/health"; do
  if probe "${url}"; then
    log "healthy: ${url}"
  else
    echo "::warning::${url} did not return 200 within ${INGRESS_PROBE_SECONDS}s (ALB/DNS may still be provisioning on a first deploy)"
    probe_warnings=$((probe_warnings + 1))
  fi
done

# --- 5. report ------------------------------------------------------------------
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### tusk-ai-secured-gateway → ${ENVIRONMENT}"
    echo ""
    echo "| | |"
    echo "|---|---|"
    echo "| Image tag | \`${GW_IMAGE_TAG}\` |"
    echo "| Commit | \`${GIT_COMMIT:-n/a}\` |"
    echo "| Console / API | https://${GW_API_HOST} |"
    echo "| MCP | https://${GW_MCP_HOST}/mcp |"
    echo "| LLM | https://${GW_LLM_HOST} |"
    echo "| Ingress probe warnings | ${probe_warnings} |"
  } >>"${GITHUB_STEP_SUMMARY}"
fi

dora_metrics
notify success "deployed"
log "done"
