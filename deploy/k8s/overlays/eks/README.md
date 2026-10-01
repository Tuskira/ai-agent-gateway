# `overlays/eks` — AWS EKS behind an internal ALB

`base` + `components/redis`, reshaped for an EKS cluster running the
[AWS Load Balancer Controller](https://kubernetes-sigs.github.io/aws-load-balancer-controller/):

| Change vs `base` | Why |
|---|---|
| nginx Ingresses → one `alb` Ingress (`ingress-alb.yaml`), `scheme: internal` | private-only exposure, joins an existing ALB group |
| per-Service health-check paths, 900s drain on `gateway-llm` | the ALB's default `/` is not a health endpoint; streamed completions finish before a target is removed |
| NetworkPolicy also admits `${GW_VPC_CIDR}` on 8080–8082 | `target-type: ip` connects from the ALB's ENIs, not from an ingress-controller pod |
| Namespace and `gateway-secrets` removed from the build | created by infrastructure-as-code first (master key + DB credentials never live in this tree) |
| `gateway-env` DB placeholders removed | the whole `GATEWAY_DATABASE_*` block comes from `gateway-secrets` |
| `GATEWAY_API_TRUSTED_PROXIES=${GW_VPC_CIDR}` | the ALB appends the real client to `X-Forwarded-For`; trusting its VPC addresses lets the per-IP/per-key rate limiters see each client instead of one shared ALB address. Narrow it to the ALB subnets' CIDRs if other VPC workloads can reach pods directly |
| toleration for `${GW_NODE_TOLERATION_KEY}:NoSchedule` | lets pods use tainted node pools |
| `preStop: sleep 15s` on the planes | no request reaches a pod after it stops listening during a rollout |
| Redis component (2 × `gateway-mcp`) | MCP sessions shared across replicas |

## Placeholders

Nothing account-specific is committed. `kubectl kustomize` leaves these as
literal `${GW_*}` strings, and `deploy/scripts/deploy_eks.sh` substitutes
them with `envsubst`, limited to exactly this list. It fails if any placeholder is left unfilled:

| Variable | Example |
|---|---|
| `GW_IMAGE_REPOSITORY` / `GW_IMAGE_TAG` | `<acct>.dkr.ecr.us-east-1.amazonaws.com/dev/tusk-ai-secured-gateway` / `0.2.0-42-807a4a7` |
| `GW_API_HOST` / `GW_MCP_HOST` / `GW_LLM_HOST` | `ai-gateway.dev.example.com` … |
| `GW_ALB_GROUP_NAME` | `internal-alb` |
| `GW_ALB_SUBNETS` | `subnet-aaa,subnet-bbb,subnet-ccc` |
| `GW_ALB_CERT_ARN` | ACM certificate covering the three hosts |
| `GW_ALB_SSL_POLICY` | `ELBSecurityPolicy-TLS13-1-2-2021-06` |
| `GW_ALB_IDLE_TIMEOUT` | `3600` (must be ≥ `llm_proxy.limits.max_stream_duration`, 900s) |
| `GW_VPC_CIDR` | `10.0.0.0/16` |
| `GW_NODE_TOLERATION_KEY` | the taint key of your dedicated nodes |

Load-balancer-level annotations (scheme, subnets, idle timeout, listen
ports, SSL policy) must be **identical** across every Ingress in one
`group.name`, or the controller refuses to reconcile the group.

## Prerequisites

- Namespace `ai-gateway`, ideally labelled
  `elbv2.k8s.aws/pod-readiness-gate-inject=enabled` (rollouts then wait
  for ALB target health, not just pod readiness).
- Secret `ai-gateway/gateway-secrets` with `GATEWAY_MASTER_KEY` and
  `GATEWAY_DATABASE_HOST/PORT/USER/PASSWORD/DATABASE`.
- kubectl 1.35 (tested; kubectl 1.33's bundled kustomize v5.6.0 crashes
  on this tree's `$patch: delete` patches -- the kind overlay's too),
  cluster ≥ 1.30 (native `preStop` sleep).

## Deploy by hand

```sh
export AWS_REGION=us-east-1 K8S_CLUSTER=<cluster> ENVIRONMENT=dev
export GW_IMAGE_REPOSITORY=... GW_IMAGE_TAG=...   # and the rest of the table
./deploy/scripts/deploy_eks.sh
```

The script renders, server-side dry-runs, runs `gateway-migrate`, applies
the planes, waits for each rollout (rolling all three back if one fails),
and probes each host's health endpoint. This repository has no deploy
workflow of its own; run the script by hand or from your own CD.
