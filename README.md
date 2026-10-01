# AI Agent Gateway

[![CI](https://github.com/Tuskira/ai-agent-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/Tuskira/ai-agent-gateway/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/Tuskira/tusk-ai-secured-gateway/graph/badge.svg)](https://codecov.io/gh/Tuskira/tusk-ai-secured-gateway)
[![Go Report Card](https://goreportcard.com/badge/github.com/Tuskira/tusk-ai-secured-gateway)](https://goreportcard.com/report/github.com/Tuskira/tusk-ai-secured-gateway)
[![Release](https://img.shields.io/github/v/release/Tuskira/tusk-ai-secured-gateway)](https://github.com/Tuskira/ai-agent-gateway/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/Tuskira/tusk-ai-secured-gateway)](go.mod)
[![License](https://img.shields.io/github/license/Tuskira/tusk-ai-secured-gateway)](LICENSE)

Open-source **AI Agent Gateway**: one binary in front of MCP tool servers and LLM models
(Claude direct or via AWS Bedrock, plus OpenAI and Gemini). Authenticates every request, injects per-request
credentials, scopes tools per agent profile, and logs every call.

## Prerequisites

To run the gateway (this Quickstart):

- Docker with the Compose plugin (Docker Desktop, Colima, or Docker Engine).
- Free local ports: `8080` (MCP plane), `8081` (API and console), `8082`
  (LLM plane), and `8090` (Adminer) if you use the `analytics` profile.
- `curl` and `jq`.
- At least 4 GB free for Docker; 8 GB is recommended if you add the
  `analytics` profile (ClickHouse).

To build and run outside Docker (`make build`, `make run`):

- Go 1.27 (see [go.mod](go.mod)).
- Node.js 26, to build the console.
- `make`.
- Optional, for linting and releases: `golangci-lint`, `govulncheck`,
  `goreleaser`.

Only for specific examples (see [examples/](examples/)):

- `npx` (ships with Node.js), for the sample MCP server used by examples
  01, 04, 05, 08, and 11.
- Python 3, for examples 03 (`check.sh`) and 06; Python 3.10+ for example 04.
- `kind` and `kubectl`, for example 10.
- A vendor API key, for the optional live parts of some examples — each
  example's README lists which.

Supported platforms: macOS and Linux. Windows works through WSL2 but is
untested.

## Quickstart

From a clone of this repository, with Docker and the `docker compose`
plugin installed:

```sh
docker compose -f deploy/docker-compose.yml up --build -d
until curl -sf localhost:8081/api/v1/health >/dev/null; do sleep 1; done   # wait for the gateway to start
curl localhost:8080/health          # MCP plane
curl localhost:8081/api/v1/health   # control plane (REST + console)
curl localhost:8082/health          # LLM plane
```

With the Overview dashboard, Access Logs and Session Timeline (adds
ClickHouse; LLM Logs works without it):

```sh
GATEWAY_SINKS_CLICKHOUSE_ENABLED=true \
  docker compose -f deploy/docker-compose.yml --profile analytics up --build -d
```

> **Privacy note.** The demo stack stores LLM request/response bodies in
> Postgres (each capped at 1 MiB) and, with the `analytics` profile, LLM
> and MCP request/response bodies in ClickHouse, so the console can show them, but
> does **not** print them to container logs. Turn bodies off entirely
> with `GATEWAY_LLM_PROXY_CAPTURE_STORE_BODIES=false
> GATEWAY_CAPTURE_STORE_BODIES=false`; see
> [docs/observability.md](docs/observability.md).

The admin console is at **http://localhost:8081**. It ships with no users
and no API key. Create the tenant and an admin API key (printed once; save
it, [getting-started](docs/getting-started.md) reuses it), then the first
console user (prints a one-time temporary password you change at first
login), and sign in:

```sh
docker compose -f deploy/docker-compose.yml exec gateway /gateway bootstrap-key   # creates the tenant + an admin API key
docker compose -f deploy/docker-compose.yml exec gateway /gateway create-user -tenant default -username admin -role admin
```

In the console, **MCPs → Catalog** lists ready-made MCP servers you can add.

See [docs/authentication.md](docs/authentication.md) for roles, sessions,
and password reset. API keys remain how programs authenticate, and the
console still accepts one as an emergency fallback.

**Next:** register an MCP tool server (a *connector* in the API, an
**MCP** in the console) and call one of its tools — continue with
[docs/getting-started.md](docs/getting-started.md), reusing the admin key
you just saved.

### MCP servers on localhost or private networks

The gateway refuses to connect to loopback, private, link-local, and
other internal addresses by default — including cloud metadata
(`169.254.169.254`), which always stays blocked — so a connector, MCP
catalog entry, or model target pointed at one of them is rejected rather
than silently reaching the node or another workload. At create time this
also catches a hostname (e.g. `localhost`) that resolves only to blocked
addresses (`400 egress_blocked`); a name that does not resolve yet is
accepted and checked when dialed. See
[docs/security-model.md#outbound-requests-ssrf](docs/security-model.md#outbound-requests-ssrf).

To reach an MCP server or model endpoint on an internal network on
purpose, set `egress.allowed_cidrs` / `egress.allowed_hosts`
(`GATEWAY_EGRESS_ALLOWED_CIDRS` / `GATEWAY_EGRESS_ALLOWED_HOSTS`, see
[docs/configuration.md#egress-outbound-request-policy](docs/configuration.md#egress-outbound-request-policy)):

- **The binary run directly on your machine** (`make run`, not Compose):
  `GATEWAY_EGRESS_ALLOWED_CIDRS=127.0.0.0/8,::1/128` to reach a server you
  started on your own loopback.
- **The shipped Compose stack** already sets `GATEWAY_EGRESS_ALLOWED_HOSTS`
  to `host.docker.internal` and `GATEWAY_EGRESS_ALLOWED_CIDRS` to
  `127.0.0.0/8,::1/128`, so a server on your laptop (via
  `host.docker.internal`) or inside the compose network's own loopback is
  already reachable — remove both for anything shared.
- **Kubernetes**, reaching an in-cluster `Service`: allowlist the DNS
  names you call (`GATEWAY_EGRESS_ALLOWED_HOSTS=my-mcp-server.my-ns.svc`)
  or the cluster's pod/service CIDR, rather than opening every private
  range.
- **A private network or VPN-only server:** allowlist its specific CIDR
  or hostname — never the broad ranges those addresses fall in.

## What it does

The gateway sits between your agents and everything they call — MCP tool
servers and LLM providers alike — as one authenticated, logged surface,
so credentials and access policy live in one place instead of being
copied into every agent config.

- **Authenticates every request** with a gateway-issued API key, scoped
  to a tenant and a role.
- **Injects per-request credentials** into outbound MCP calls from an
  encrypted secret store, so a tool server credential never has to be
  handed to the caller.
- **Scopes tools per agent profile**, enforced on every `tools/call`, not
  just filtered on `tools/list`. Bind a key to a profile to enforce it:
  without a binding the caller names its own profile in a header.
- **Passes prompts and resources through** from every backend under one
  namespace (`<connector>__<prompt>`, `gw://<connector>/<uri>`), scoped by
  the same profiles.
- **Serves skills and commands** attached to a profile natively over MCP
  (a `gateway__skill` tool, commands as prompts, and the MCP Skills
  Extension), for reference text and reusable prompt templates that never
  need a connector — see [docs/skills.md](docs/skills.md).
- **Proxies LLM calls** to Anthropic, Bedrock, OpenAI, and Gemini behind
  one surface, capturing token usage and an estimated cost for every call.
- **Logs and exposes every call** — access logs, LLM logs, and an
  analytics dashboard, via an embedded admin console.

Listeners: `:8080` MCP · `:8081` control REST + UI · `:8082` LLM.
Required dependency: PostgreSQL. Optional: a Redis-protocol session store
(`sessions.store: redis`; the bundled deploys use Valkey), so the MCP plane
can run more than one replica, and ClickHouse.

## Documentation

| Doc | Covers |
|---|---|
| [community.tuskira.ai](https://community.tuskira.ai) | The same `docs/` folder as a website with a sidebar (see [website/](website/README.md)) |
| [docs/README.md](docs/README.md) | Full documentation index |
| [docs/getting-started.md](docs/getting-started.md) | After the Quickstart: register an MCP server and make a first tool call |
| [docs/architecture.md](docs/architecture.md) | The three planes, shared core, plugin seams, request flows, data model |
| [docs/configuration.md](docs/configuration.md) | Every config key, type, default, and env var |
| [docs/security-model.md](docs/security-model.md) | Identity, tenant isolation, secrets handling, known limits |
| [docs/authentication.md](docs/authentication.md) | API keys vs. console users, roles, sessions/CSRF, lockout, password reset |
| [docs/connectors-and-credentials.md](docs/connectors-and-credentials.md) | Registering connectors, header types, credential rotation, `tool_arg_overrides`, cache management |
| [docs/profiles.md](docs/profiles.md) | Agent profiles, `X-Agent-Profile-Name`, Profile Studio |
| [docs/models.md](docs/models.md) | Model catalog vs. registry, the Models page and Connect flow, permissions |
| [docs/skills.md](docs/skills.md) | Skills & commands registry: versions, attachment, native tool/prompts, MCP Skills Extension, file sync |
| [docs/llm-plane.md](docs/llm-plane.md) | Provider routes, BYOK setup, Bedrock credential modes, capture, cost |
| [docs/observability.md](docs/observability.md) | Sinks, analytics API, console pages, sample ClickHouse queries |
| [docs/api.md](docs/api.md) | Control-plane API: auth, error envelope, pagination, full route table |
| [deploy/README.md](deploy/README.md) | Docker Compose and Kubernetes (kustomize `base` + `kind`/`analytics` overlays), scaling per plane |
| [SECURITY.md](SECURITY.md) | Supported versions, how to report a vulnerability |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Dev setup, `make` targets, PR conventions, adding a store backend/sink/provider |
| [CODEOWNERS](CODEOWNERS) | Who reviews what |
| [web/README.md](web/README.md) | The embedded admin console: dev setup, design tokens, folder layout |

## Examples

Worked, end-to-end examples that run against a real gateway live under
[examples/](examples/) — see [examples/README.md](examples/README.md) for
the index. Start with
[examples/01-quickstart](examples/01-quickstart): compose up, health-check all
three planes, bootstrap an admin key, register an MCP server as a
connector, and call one of its tools. `make examples-smoke` runs every
example that needs no external credential.

## Status

The gateway is pre-1.0 (alpha: APIs and config may still change between
minor versions) — see
[docs/architecture.md](docs/architecture.md#what-is-and-isnt-supported)
for exactly what is and isn't in the current release. CI
(`.github/workflows/ci.yml`) and tagged
releases (`release.yml` → GoReleaser → `ghcr.io/tuskira/ai-agent-gateway`)
are in place; see [CONTRIBUTING.md](CONTRIBUTING.md#release-process).
Kubernetes manifests live under [deploy/k8s](deploy/README.md); a Helm
chart is planned.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
"AI Agent Gateway" and "Tuskira" are trademarks of Tuskira, Inc. — see
[TRADEMARKS.md](TRADEMARKS.md) for how you may use them.
