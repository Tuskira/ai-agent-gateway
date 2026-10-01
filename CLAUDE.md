# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go AI gateway: one binary (`cmd/gateway`) that sits in front of MCP tool servers and LLM providers (Anthropic, Bedrock, OpenAI, Gemini). It authenticates every call with a gateway API key, injects per-connector credentials from an encrypted store, enforces per-agent-profile tool allow-lists, and logs every call. It embeds a React admin console (`web/`). Pre-1.0, so APIs and config can still change.

## Commands

Go (module `github.com/Tuskira/tusk-ai-secured-gateway`, Go version per `go.mod`):

```sh
make build-go-only                 # fast Go build -> bin/gateway (no UI rebuild)
make build                         # UI build into internal/api/ui/dist, then Go build
make test                          # go test -race ./...
go test -race ./internal/llmplane/ -run TestName   # single test
make lint                          # golangci-lint (only govet + staticcheck + gofmt enabled)
make ci                            # everything CI's go + web jobs run: gofmt -l, vet (incl. -tags e2e), test, lint, govulncheck, web checks
gofmt -w .                         # CI fails on any unformatted file
```

Web console (`web/`, npm only — no pnpm):

```sh
make ui-install && make ui-dev     # Vite dev server; proxies /api to :8081
npm run typecheck|lint|test|build --prefix web
npm run test --prefix web -- path/to/file.test.tsx   # single vitest file
npm run gen:tokens --prefix web    # regenerate design tokens from web/tokens.json
```

Local stack and subcommands:

```sh
make compose-up                    # Postgres + gateway (deploy/docker-compose.yml)
docker compose -f deploy/docker-compose.yml exec gateway /gateway bootstrap-key   # first admin key (plaintext on stdout)
go run ./cmd/gateway secrets genkey        # generate a GATEWAY_MASTER_KEY
# other subcommands: version, migrate, secrets rekey
```

Tests needing live services (they skip when the env var is unset; this repo does **not** fake external services or LLM vendors):

```sh
GATEWAY_TEST_DATABASE_URL=postgres://gateway:gateway@localhost:5432/gateway?sslmode=disable \
GATEWAY_MASTER_KEY=$(go run ./cmd/gateway secrets genkey) \
  go test -tags e2e ./test/e2e/...                                  # end-to-end, real socket + Postgres
go test ./internal/store/postgres/... -run TestConformance          # needs GATEWAY_TEST_DATABASE_URL
GATEWAY_TEST_REDIS_ADDR=127.0.0.1:6379 go test ./pkg/session/...    # redis session driver
GATEWAY_TEST_CLICKHOUSE_ADDR=localhost:9000 go test -tags integration ./pkg/sink/clickhouse/... -run Integration
```

LLM provider conformance uses `LLMTEST_*` env vars (see CONTRIBUTING.md for a local Ollama recipe). `make examples-smoke` runs the self-checking walkthroughs in `examples/`.

## Architecture

**One process, three planes**, each with its own listener and `enabled` flag, built separately in `cmd/gateway/main.go`'s `run()` with no compile-time dependency between them:

| Plane | Port | Package | Surface |
|---|---|---|---|
| MCP proxy | `:8080` | `internal/dataplane` | `POST/DELETE /mcp`, `GET /mcp/stream` (SSE) |
| Control API + console | `:8081` | `internal/api` | `/api/v1/*`, embedded UI from `internal/api/ui/dist` |
| LLM proxy | `:8082` | `internal/llmplane` | `/v1/messages`, `/{provider}/*`, `/model/*` (Bedrock) |

The MCP data plane object is built whenever MCP **or** API is enabled (the API plane uses it for connector health/discover/cache via `pkg/ops`), but its listener only opens when `mcp.enabled` is true.

**Shared core**, constructed once in `main.go` and passed as `Deps`: auth (`internal/auth`, `pkg/auth` — API-key authenticator chain + `RoleAuthorizer`), secrets (`internal/secrets`, AES-256-GCM, plus the header-resolver registry in `internal/dataplane/headers`), store (`pkg/store` interface, `internal/store/postgres` implementation, migrations in `internal/store/postgres/migrations`), and sinks (`pkg/sink` → stdout/otel/clickhouse/postgres, combined in `sink.Multi`).

**`internal/` vs `pkg/`:** `pkg/` holds the plugin seams — small interfaces a separate Go module can implement (store, session, sink, body store, headers, ops, analytics, auth, LLM `Dialect`/`Provider`). Backends register through driver registries from `init()` and are enabled by a blank import in `cmd/gateway/main.go`. New store/session backends must pass `pkg/store/storetest` / `pkg/session/sessiontest`; LLM adapters must pass `pkg/llm/llmtest`. See CONTRIBUTING.md for the exact steps per seam.

**MCP tool-call flow** (`internal/dataplane`): authenticate → check `mcp.access` → resolve session (`session.Manager` over `pkg/session` store: `memory` default, `redis` for multi-replica with pub/sub for `tools/list_changed`) → resolve `X-Agent-Profile-Name` to an allow-list → route `<connector>__<tool>` (`router`) → check the allow-list per (connector, tool) **after** routing → stamp `tool_arg_overrides` → resolve connector headers, then set gateway-owned `X-Tenant-Id`/`Mcp-Session-Id` last → call backend (on 401: evict cached creds and replay once; on timeout: return `TOOL_TIMEOUT` as tool content, not a protocol error) → write one access log. Prompts are namespaced `<connector>__<prompt>`, resources `gw://<connector>/<uri>`. Names with no `__` and `skill://` URIs are served natively by the gateway (skills/commands attached to profiles, `internal/skills`, `internal/dataplane/skillsext`).

**LLM flow** (`internal/llmplane`): auth + `llm.access` → session/request tagging → body cap → per-tenant concurrency limit → stream deadline → pick provider by path → forward body **byte-for-byte** (keeps prompt caching working) or SigV4-sign for Bedrock → tee response to client and a bounded capture → parse usage → price via `pkg/pricing` (`prices.json`) → write `llm_calls` to Postgres synchronously → best-effort copy to sinks. When the client format differs from the vendor's, `internal/llmplane/translate` converts through the neutral `pkg/llm` types.

**Tenancy:** every Postgres table except `tenants` is scoped by `tenant_id`; keep that filter on any new query.

**Config** (`internal/config`): defaults → optional YAML (`--config` or `CONFIG_PATH`) → env vars `GATEWAY_<SECTION>_<FIELD>` generated from YAML tags (leaf fields only; bad values fail startup). `configs/base/default.yml` is a commented reference and is not auto-loaded. `docs/configuration.md` lists every key.

`docs/` is the detailed reference (architecture, security model, LLM plane, profiles, skills, API routes) and is also published as a site from `website/` (Docusaurus). Update the matching doc when changing behavior it describes.

## Conventions

- Branches `<type>/<short-description>`; Conventional Commits, scoped where useful (`fix(llmplane): ...`); one topic per PR; use a git worktree per task.
- Behavior changes need tests. Add a `CHANGELOG.md` entry under `[Unreleased]`.
- `web/`: no inline styles — Tailwind classes and design tokens only (`web/tokens.json` → `npm run gen:tokens`). shadcn/ui components live in `web/src/components/ui/`; `@/*` maps to `web/src/*`.
- Security-sensitive options (e.g. `env`/`file` header providers) stay off by default behind an explicit config flag.
