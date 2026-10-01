# Deploying the gateway

Two ways to run this: `deploy/docker-compose.yml` for a single-host/local
stack, or `deploy/k8s/` for Kubernetes. Same image, same config surface
(`configs/base/default.yml`, `GATEWAY_<SECTION>_<FIELD>` env overrides —
see `internal/config/config.go`); Kubernetes just splits the one process
into three independently-scalable Deployments (mcp / api / llm_proxy —
`internal/config.MCP.Enabled` / `API.Enabled` / `LLMProxy.Enabled`) instead
of running all three planes in one container the way compose does.

## Compose vs Kubernetes

| | `deploy/docker-compose.yml` | `deploy/k8s/` |
|---|---|---|
| Planes | all three in one container | one Deployment per plane, same image |
| Postgres | compose service | bring your own (required) |
| ClickHouse | `--profile analytics` | `overlays/analytics` |
| Redis (MCP sessions, lets mcp scale out) | `--profile redis` | `overlays/redis` |
| Use case | local dev, quickstart | real clusters, and local kind testing (`overlays/kind`) |

## Layout

```
deploy/k8s/
  base/               namespace, ConfigMaps, Secret template, the three
                       Deployments + Services, the migrate Job, HPAs, PDBs,
                       NetworkPolicy, Ingress -- a generic reference for a
                       real cluster (BYO Postgres, BYO ingress-nginx)
  overlays/kind/       local testing: adds a dev Postgres, dev secret
                       values, dev image tag, NodePort Services instead of
                       Ingress
  overlays/analytics/  adds ClickHouse + wires GATEWAY_SINKS_CLICKHOUSE_*
  components/redis/    a kustomize Component: dev Redis, gateway-mcp on
                       GATEWAY_SESSIONS_STORE=redis with 2 replicas, the
                       appended NetworkPolicy egress rule -- add it to any
                       overlay
  overlays/redis/      base + components/redis (production shape)
  overlays/kind-redis/ overlays/kind + components/redis (local testing)
  overlays/eks/        base + components/redis on EKS behind an internal
                       ALB; Namespace and Secret owned by IaC, env-specific
                       values filled in at deploy time (see its README.md)
```

All of them are kustomize builds (`kubectl kustomize <dir>`, or
`kubectl apply -k <dir>`; no separate `kustomize` binary needed --
`kubectl` has it built in).

## Generate the master key

`secret_store.master_key_env` (default `GATEWAY_MASTER_KEY`) encrypts every
row in the credential store; the gateway will not start without it. Never
reuse the value baked into `docker-compose.yml` (`DEV-ONLY DEFAULT KEY`)
outside that local stack. Generate a real one:

```sh
# from a checkout
go run ./cmd/gateway secrets genkey
# or against the image
docker run --rm ghcr.io/tuskira/tusk-ai-secured-gateway:latest secrets genkey
```

Put the result in `deploy/k8s/base/secret.yaml`'s `GATEWAY_MASTER_KEY`
(replacing `REPLACE_ME`), alongside your real
`GATEWAY_DATABASE_PASSWORD` — or, better, don't edit that file in place;
generate the Secret out-of-band instead (`kubectl create secret generic
gateway-secrets -n ai-gateway --from-literal=GATEWAY_MASTER_KEY=... \
--from-literal=GATEWAY_DATABASE_PASSWORD=... --dry-run=client -o yaml |
kubectl apply -f -`, or a sealed-secrets/external-secrets controller) so
the real values never touch the manifest tree.

Rotating an existing key ring is `gateway secrets rekey --tenant <slug|all>`
— see `configs/base/default.yml`'s `secret_store` docs and the command's
own doc comment in `cmd/gateway/main.go`.

## Local testing (kind)

```sh
kind create cluster --name ossgw
docker build -t tusk-ai-secured-gateway:dev .
kind load docker-image tusk-ai-secured-gateway:dev --name ossgw
kubectl apply -k deploy/k8s/overlays/kind
```

This brings up a dev Postgres (StatefulSet, `emptyDir`, no persistence),
the `gateway-migrate` Job, and the three plane Deployments with the dev
master key/DB password baked in (identical to compose's dev-only default —
see `overlays/kind/secret-dev-patch.yaml`).

Wait for everything to be ready, then bootstrap an admin key:

```sh
kubectl -n ai-gateway rollout status statefulset/postgres
kubectl -n ai-gateway wait --for=condition=complete job/gateway-migrate --timeout=120s
kubectl -n ai-gateway rollout status deploy/gateway-api deploy/gateway-mcp deploy/gateway-llm

KEY=$(kubectl -n ai-gateway exec deploy/gateway-api -- /gateway bootstrap-key 2>/dev/null)
```

Then reach it via port-forward (no ingress controller in this overlay —
see `overlays/kind/services-nodeport-patch.yaml`'s comment for the NodePort
alternative):

```sh
kubectl -n ai-gateway port-forward svc/gateway-api 8081:8081 &
curl -s localhost:8081/api/v1/health
curl -s -H "Authorization: Bearer $KEY" localhost:8081/api/v1/auth/me

kubectl -n ai-gateway port-forward svc/gateway-mcp 8080:8080 &
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8080/mcp \
  -H 'content-type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'   # 401, no key
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8080/mcp \
  -H "Authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'                                        # 200
```

Tear down with `kind delete cluster --name ossgw`.

## In-cluster MCP servers and the NetworkPolicy

`base/networkpolicy.yaml` allows gateway-pod egress to DNS, Postgres,
ClickHouse and 443 only. An MCP server running inside the cluster on any
other port is therefore unreachable until you add an egress rule for it —
and this bites on kind too, whose kindnet CNI does enforce NetworkPolicy
(kind ≥ 0.20 embeds kube-network-policies), so the connector handshake
just times out with no obvious cause.

Append the rule rather than merging it:
[`examples/10-kubernetes/overlay/networkpolicy-everything-patch.yaml`](../examples/10-kubernetes/overlay/networkpolicy-everything-patch.yaml)
is the pattern — a JSON patch adding to `/spec/egress/-`, because
NetworkPolicy rules have no merge key, so a strategic-merge patch would
replace the whole `egress` list instead of extending it.

Connector Health (`GET /connectors/{id}/health`) and Discover
(`POST /connectors/{id}/discover`) reach those servers from the **api**
Deployment, not the mcp one, and they work there even though it runs with
`GATEWAY_MCP_ENABLED=false`: `cmd/gateway` builds the data plane whenever
either plane needs it, without opening the MCP listener or running its
background refresh loops. So the egress rule has to cover the api pods as
well — the shipped policy's `podSelector` already matches all three planes
via `app.kubernetes.io/name: gateway`.

## Production: Ingress instead of port-forward

`base/ingress.yaml` and `base/ingress-stream.yaml` expect an nginx ingress
controller (`ingressClassName: nginx`) and a `gateway-tls` TLS Secret
covering `gateway.example.com`, `mcp.gateway.example.com`, and
`llm.gateway.example.com` (swap in your real hostnames). The mcp/llm hosts
carry `proxy-read-timeout: "900"` and `proxy-buffering: "off"` — both
planes serve long-lived SSE/chunked responses (MCP's `GET /mcp/stream`, the
llm plane's streamed completions) that nginx's defaults would sever or
stall.

On EKS with the AWS Load Balancer Controller, use `overlays/eks` instead:
one `alb` Ingress on an internal ALB, per-plane target-group health
checks, and `deploy/scripts/deploy_eks.sh` to run migrate → rollout →
health probes in order. See
[`k8s/overlays/eks/README.md`](k8s/overlays/eks/README.md).

## Scaling per plane

Each plane scales independently since it's its own Deployment:

- **api**: HPA'd, CPU 70%, 1–3 replicas. Stateless; scales freely.
- **llm**: HPA'd, CPU 70%, 2–10 replicas. Stateless; scales freely. Note
  `terminationGracePeriodSeconds: 900` on its Deployment, matching
  `llm_proxy.limits.max_stream_duration`'s default (15m) — a rolling
  update or node drain lets in-flight streams finish rather than cutting
  them off. Raise both together if you raise that limit.
- **mcp**: **`replicas: 1` in `base/`, and scalable only with the Redis
  session store.** With the default `sessions.store: memory`, MCP
  sessions live in-process: a second replica wouldn't see sessions created
  on the first one, so requests bouncing between pods would get JSON-RPC
  `-32000` and re-initialize (correct, but every backend handshake is
  redone). `components/redis` switches `gateway-mcp` to
  `GATEWAY_SESSIONS_STORE=redis` — any replica then serves any session,
  backend handles included, and a `tools/list_changed` raised on one
  replica reaches the SSE streams open on the others — and sets
  `replicas: 2`; add an HPA on top if you want more. `pdb-mcp.yaml` uses
  `maxUnavailable: 1` rather than `minAvailable: 1` so that at one replica
  it still permits voluntary evictions (node drains included) rather than
  blocking them forever; at two or more it keeps one replica up through
  an eviction.

  The Redis pieces are a kustomize *Component* rather than an overlay
  because kustomize refuses to combine two overlays of the same base, so
  an overlay could never be added to `kind` or `analytics` without
  hand-composing files; a component drops into any of them. Three ways to
  use it:

  ```sh
  # production: base + Redis
  kubectl kustomize deploy/k8s/overlays/redis      # inspect
  kubectl apply -k deploy/k8s/overlays/redis
  # local kind: the kind overlay (dev Postgres, local image) + Redis
  kubectl apply -k deploy/k8s/overlays/kind-redis
  # anything else (e.g. analytics + Redis): your own overlay
  #   resources:  [<path to>/deploy/k8s/overlays/analytics]
  #   components: [<path to>/deploy/k8s/components/redis]
  kubectl -n ai-gateway logs deploy/gateway-mcp | grep 'sessions store'
  ```

  The component's Redis server is a dev-grade single Valkey pod (BSD-3,
  Redis-compatible; no persistence, no auth). Compose's `--profile redis`
  runs the same image. In production point `GATEWAY_REDIS_ADDR` at a
  managed Redis, put `GATEWAY_REDIS_PASSWORD` in `gateway-secrets`, set
  `GATEWAY_REDIS_TLS=true`, and swap the component's `podSelector` egress
  rule for an `ipBlock` to that endpoint. Redis Cluster mode is not
  supported yet (`redis.cluster: true` is rejected at startup). Losing
  Redis costs every connected agent one re-initialize, nothing more.

## Upgrading

1. Update the image tag/digest in the Deployments (and the `gateway-migrate`
   Job — keep them in sync).
2. Run the migrate Job first: `kubectl apply -f deploy/k8s/base/job-migrate.yaml`
   after deleting the previous run (`kubectl -n ai-gateway delete job
   gateway-migrate`; Job objects aren't re-runnable via re-apply), then wait
   for it to complete.
3. Roll the three Deployments (`kubectl -n ai-gateway rollout restart
   deploy/gateway-api deploy/gateway-mcp deploy/gateway-llm`, or re-apply
   with the new tag).

This mirrors what Helm would do as a pre-install/pre-upgrade hook; kustomize
has no hook mechanism, so it's a manual (or CI-scripted) two-step instead of
one `helm upgrade`.

## Run the analytics overlay

```sh
kubectl apply -k deploy/k8s/overlays/analytics
```

Adds a ClickHouse StatefulSet + Service and turns on
`GATEWAY_SINKS_CLICKHOUSE_*` for all three planes (patches the shared
`gateway-env`/`gateway-secrets` they already read via `envFrom` — see that
overlay's `gateway-env-clickhouse-patch.yaml`), so the admin console's
Overview and LLM Logs pages start working
(`GET /api/v1/analytics/*` — see the root README's "LLM plane" section).
Replace the `REPLACE_ME` ClickHouse password in
`overlays/analytics/gateway-secrets-clickhouse-patch.yaml` first.

This overlay extends `base/`, same as `overlays/kind/` does — the two
aren't stacked. To test analytics inside a local kind cluster, apply both
against the same cluster (`kubectl apply -k overlays/kind && kubectl apply
-k overlays/analytics`); both are plain `kubectl apply`, so the second run
merges onto the first via the usual `kubectl.kubernetes.io/last-applied-configuration`
3-way merge rather than needing a single combined kustomize build.
