# 10 · Kubernetes

## Goal

Run the gateway on a local [kind](https://kind.sigs.k8s.io/) cluster using
the production-shaped manifests that already live in
[`deploy/k8s`](../../deploy/k8s) — one image, three independently-scalable
Deployments (mcp / api / llm) — then register a real MCP server as a
connector and drive it end to end over the MCP plane, the same way
[01-quickstart](../01-quickstart/) does against compose.

This example does **not** duplicate `deploy/k8s`. It applies
[`deploy/k8s/overlays/kind`](../../deploy/k8s/overlays/kind) directly and
layers one small overlay ([`overlay/`](overlay/)) on top that adds only what
`deploy/k8s` doesn't already have: an in-cluster copy of the reference
`@modelcontextprotocol/server-everything` MCP server to register as a
connector, a NetworkPolicy egress rule and an `egress.allowed_hosts` entry
so the gateway may reach it, and a further image-tag override. See
[`overlay/kustomization.yaml`](overlay/kustomization.yaml) for the
reasoning behind each.

Everything else — the namespace, ConfigMaps, Secret, the three plane
Deployments, Services, the migrate Job, HPAs, PDBs, NetworkPolicy, the dev
Postgres StatefulSet, and the NodePort/dev-secret patches — comes from
`deploy/k8s/overlays/kind` unchanged (apart from the NetworkPolicy and
`gateway-env` egress patches above). Read [`deploy/README.md`](../../deploy/README.md)
first; this README only covers what's specific to running it as an example.

## What this shows

- **One image, three Deployments.** `gateway-api`, `gateway-mcp`, and
  `gateway-llm` all run the same container image; which plane(s) a
  Deployment runs is set purely by env vars —
  `GATEWAY_API_ENABLED` / `GATEWAY_MCP_ENABLED` / `GATEWAY_LLM_PROXY_ENABLED`
  (`deploy/k8s/base/deployment-{api,mcp,llm}.yaml`). That's why they scale
  independently: `gateway-api` defaults to 1 replica, `gateway-mcp` is
  pinned to 1, and `gateway-llm` defaults to 2 (matching its HPA's
  `minReplicas`) — see "Scaling per plane" below.
- **The migrate Job runs once, before the planes serve traffic.** Each
  plane Deployment sets `GATEWAY_DATABASE_MIGRATE=false`, so three replicas
  don't race each other applying schema changes; `gateway-migrate`
  (`deploy/k8s/base/job-migrate.yaml`) runs `gateway migrate` exactly once
  instead. `run.sh` waits for it to complete before checking the planes.
- **Config and secrets are split.** Non-secret config lives in two
  ConfigMaps (`gateway-config` — the mounted `config.yaml`; `gateway-env` —
  plane-agnostic env like the Postgres host); `GATEWAY_MASTER_KEY` (encrypts
  the credential store) and the DB password live in the `gateway-secrets`
  Secret. `overlays/kind` patches both ConfigMap and Secret with dev-only
  values (`deploy/k8s/overlays/kind/gateway-env-dev-patch.yaml`,
  `secret-dev-patch.yaml`) — never reuse those values outside local kind
  testing.
- **A real MCP connector, running in-cluster.** `overlay/everything.yaml`
  runs the reference `@modelcontextprotocol/server-everything` as its own
  Deployment+Service inside the cluster (`node:22-alpine`, `npx -y
  @modelcontextprotocol/server-everything streamableHttp`, port 3001 — see
  `overlay/kustomization.yaml`'s comment for why in-cluster rather than on
  the host).
  `run.sh` registers it and calls `everything__echo` through the
  port-forwarded MCP plane (which discovers the tools itself on the first
  `tools/list`).
- **Connector Health / Discover work from the split `gateway-api` pod.**
  `GET /connectors/{id}/health` and `POST /connectors/{id}/discover` (and
  the console's Health/Discover buttons) need the data plane, not the MCP
  listener: `cmd/gateway/main.go` builds the data plane whenever *either*
  plane needs it, so `gateway-api` serves them with
  `GATEWAY_MCP_ENABLED=false` and without opening `:8080` or running the
  MCP plane's background refresh loops. `run.sh` asserts both, exactly as
  [01-quickstart](../01-quickstart/README.md) does against compose.
- **Reaching an in-cluster connector needs a NetworkPolicy rule.** The
  shipped `deploy/k8s/base/networkpolicy.yaml` allows gateway-pod egress
  only to DNS, Postgres, ClickHouse and 443 — and kind's kindnet enforces
  NetworkPolicy — so this example appends one egress rule for the
  `everything` server on `:3001`
  ([`overlay/networkpolicy-everything-patch.yaml`](overlay/networkpolicy-everything-patch.yaml),
  a JSON patch rather than a strategic merge). Do the same for every MCP
  server you run inside the cluster; see
  [`deploy/README.md`'s "In-cluster MCP servers and the NetworkPolicy"](../../deploy/README.md#in-cluster-mcp-servers-and-the-networkpolicy).
- **The mcp plane stays at 1 here — deliberately.** With the default
  `sessions.store: memory`, MCP sessions live in-process, so a second
  replica wouldn't see sessions the first one created. Scaling it needs
  the Redis-protocol session store: `deploy/k8s/overlays/kind-redis` (or
  `components/redis`) sets `GATEWAY_SESSIONS_STORE=redis` and 2 replicas.
  `run.sh`'s scaling demo only scales
  `gateway-llm`, and prints why `gateway-mcp` doesn't move. Full
  explanation: [`deploy/README.md`'s "Scaling per plane"](../../deploy/README.md#scaling-per-plane).
- **Ingress, HPA, and the analytics (ClickHouse) overlay are documented,
  not exercised here.** `deploy/k8s/base/ingress.yaml` needs a real
  ingress-nginx controller and DNS, which `kind`'s NodePort-based dev
  overlay deliberately doesn't set up (`delete-ingresses-patch.yaml`); the
  HPA objects exist in the cluster but kind ships no metrics-server, so
  they never act on load here — `run.sh`'s scaling step is a manual
  `kubectl scale`, not the HPA reacting. See
  [`deploy/README.md`](../../deploy/README.md) for Ingress, HPA behavior on
  a real cluster, and the analytics overlay.

## Prerequisites

- Docker
- [`kind`](https://kind.sigs.k8s.io/) and `kubectl`:
  ```sh
  brew install kind kubectl
  ```
- `curl` and `jq`
- Outbound internet access **from inside the kind node**, not just the
  host: the default path builds the gateway image locally, but the
  in-cluster `everything` server still pulls `node:22-alpine` and runs
  `npx -y @modelcontextprotocol/server-everything`, both of which need the
  node to reach a registry/npm at pod-start time.
- Nothing else: no API keys, no cloud account, no LLM provider credentials.

## Steps

```sh
./run.sh
```

It's safe to re-run: the cluster, the image, and every object are
reused/reconciled rather than recreated. What it does, in order:

1. **Create (or reuse) a kind cluster** named `ai-gateway-example`.
2. **Build the gateway image locally** — `docker build -t
   tusk-ai-secured-gateway:example .` from the repo root (the Dockerfile
   builds the React console, then the Go binary; ~3-4 minutes) — and
   `kind load docker-image` it into the cluster, so this example never
   depends on GHCR being reachable or public. Set `GATEWAY_IMAGE` to skip
   the build and pull an existing image instead:
   ```sh
   GATEWAY_IMAGE=ghcr.io/tuskira/tusk-ai-secured-gateway:0.3.0 ./run.sh
   ```
3. **Render and apply `overlay/`** (`kubectl kustomize overlay/ | kubectl
   apply -f -`) — the local-build path uses the image tag the overlay
   already sets; the `GATEWAY_IMAGE` path swaps it in after rendering.
4. **Wait** for the dev Postgres StatefulSet, the `gateway-migrate` Job
   (`--for=condition=complete`), and all four Deployments
   (`gateway-api`, `gateway-mcp`, `gateway-llm`, `everything`) to roll out,
   then prints `kubectl get pods`.
5. **Port-forward** `gateway-api`, `gateway-mcp`, and `gateway-llm` to
   `localhost:28081/28080/28082` (not `8080-8082` — those are
   01-quickstart's compose ports; using different ones lets both examples
   run at once) and wait for `/health` on all three.
6. **Bootstrap an admin key**: `kubectl exec deploy/gateway-api --
   /gateway bootstrap-key` (falls back to `--force` on a re-run, same as
   01-quickstart).
7. **Register the in-cluster `everything` connector**
   (`http://everything.ai-gateway.svc.cluster.local:3001/mcp`), probe its
   health and discover its tools through the split `gateway-api` pod
   (asserts `healthy` and ≥ 10 discovered), then run the standard MCP
   lifecycle over the port-forwarded MCP plane — `initialize` →
   `notifications/initialized` → `tools/list` (asserts ≥ 10 tools incl.
   `everything__echo`) → `tools/call everything__echo` (asserts the echoed
   text comes back). Identical assertions and helpers to
   [01-quickstart](../01-quickstart/README.md).
8. **Scale `gateway-llm` to 3 replicas**, wait for rollout, and assert 3
   ready. `gateway-llm` already defaults to 2 replicas (its HPA's
   `minReplicas` — see `deploy/k8s/base/deployment-llm.yaml`), so this
   step scales past that default rather than repeating it, to actually
   demonstrate a change; a re-run resets it to 2 first (`kubectl apply`
   reconciles `replicas` back to the manifest's value) before scaling to 3
   again. Prints `gateway-mcp`'s replica count alongside it, and why it
   doesn't move.
9. Prints the console URL, the admin key, and the cleanup command.

## Expected output

While Postgres starts, the migrate Job and the plane pods can fail a few
times (`Error` pods for `gateway-migrate`, a few restarts on the planes,
"lookup postgres ... no such host" in their logs) and then recover on their
own; `run.sh` waits for the rollouts, so this is expected.

```
check: required tools (kind, kubectl, docker, curl, jq)
check: creating kind cluster 'ai-gateway-example'
check: building 'tusk-ai-secured-gateway:example' from ... (~3-4 min)
check: loading 'tusk-ai-secured-gateway:example' into kind cluster 'ai-gateway-example'
check: rendering overlay/ (kubectl kustomize)
check: applying to namespace 'ai-gateway'
namespace/ai-gateway created
configmap/gateway-config created
... (21 objects)
check: waiting for postgres (up to 2m)
check: waiting for the migrate Job to complete (up to 3m)
check: waiting for the plane Deployments and the everything server (up to 3m each)
check: pods in 'ai-gateway'
  ... (postgres-0, gateway-api-*, gateway-mcp-*, gateway-llm-* x2, everything-*, gateway-migrate-* Completed)
check: port-forwarding gateway-api (28081->8081), gateway-mcp (28080->8080), gateway-llm (28082->8082)
check: control plane health (http://localhost:28081/api/v1/health)
  -> {"status":"ok","plane":"api",...}
check: MCP plane health (http://localhost:28080/health)
  -> {"status":"ok","plane":"mcp",...}
check: LLM plane health (http://localhost:28082/health)
  -> {"status":"ok","plane":"llm",...}
check: bootstrapping an admin API key (tenant 'default')
  -> bootstrapped a new admin key
check: registering the 'everything' connector
  -> created connector <uuid>
check: connector health probe
  -> {"status":"healthy","latency_ms":N,"capabilities":{...},"checked_at":"..."}
check: connector tool discovery
  -> discovered N tool(s), written to the tool cache
check: MCP initialize
  -> protocolVersion=2025-06-18, session=<uuid>
check: MCP notifications/initialized
check: MCP tools/list
  -> N tool(s) advertised under the 'everything' connector
check: MCP tools/call everything__echo
  -> hello from the gateway
check: scaling gateway-llm to 3 replicas
  -> gateway-llm: 3/3 ready
  -> gateway-mcp stays at 1/1: ...

================================================================
10-kubernetes passed.
Console:    http://localhost:28081
Admin key:  gk_...
Cleanup:    kind delete cluster --name ai-gateway-example
================================================================
```

## Cleanup

`run.sh` never deletes the cluster on its own — tear it down explicitly:

```sh
kind delete cluster --name ai-gateway-example
```

Any `kubectl port-forward` processes it started exit automatically when
the script exits (or is interrupted) via its own cleanup trap.
