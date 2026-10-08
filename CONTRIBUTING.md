# Contributing

Please read our [Code of Conduct](CODE_OF_CONDUCT.md) before participating.
If you're looking for help using the gateway rather than contributing code,
see [SUPPORT.md](SUPPORT.md). [MAINTAINERS.md](MAINTAINERS.md) explains who
reviews and merges pull requests.

## Dev setup

Requires:

- Go 1.27 (matches `go.mod`)
- Node 26+ and npm 11+ (for `web/`; no pnpm)
- Docker + Docker Compose (for Postgres/ClickHouse locally)

```sh
git clone <repo>
cd ai-agent-gateway
docker compose -f deploy/docker-compose.yml up --build -d   # Postgres + gateway
make ui-install && make ui-dev                                # console, separately, with hot reload
```

`make build` compiles the React console into `internal/api/ui/dist` first
(`ui-build`), then the Go binary (`build-go-only`), so `ui.Handler()`
serves the real app instead of a `503 UI-not-built` response. Use
`make build-go-only` alone for a fast Go-only iteration loop when you
don't need the console rebuilt.

## `make` targets

| Target | Does |
|---|---|
| `make build` | UI build + Go build → `bin/gateway` |
| `make build-go-only` | Go build only, using whatever's already in `internal/api/ui/dist` |
| `make run` | `build` then run the binary |
| `make test` | `go test -race ./...` |
| `make lint` | `golangci-lint run ./...` (skipped with a message if not installed) |
| `make docker-build` | Builds the release image (`Dockerfile`) |
| `make compose-up` / `compose-down` | Local Postgres + gateway stack |
| `make tidy` | `go mod tidy` |
| `make ui-install` / `make ui-dev` / `make ui-build` | `web/` npm lifecycle |

## Worktree, branch, and PR conventions

- Use a `git worktree` per task rather than switching branches in place.
- Branch names: `<type>/<short-description>` (e.g. `fix/gemini-thinking-tokens`,
  `feat/pricing`), matching this repo's existing history.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `chore:`, `docs:`, …), scoped where it helps
  (`fix(llmplane): ...`).
- **One topic per PR.** A PR that mixes an unrelated refactor with a
  feature is harder to review and harder to revert; split them.
- **Tests are required** for behavior changes — see `make test`. A new
  store backend must pass `pkg/store/storetest` (below), a new session
  backend `pkg/session/sessiontest` (below); a new sink or
  provider should have its own package-level tests mirroring the shipped
  ones (`pkg/sink/stdout/stdout_test.go`, `internal/secrets/providers_test.go`).
- **No inline styles in `web/`.** Styling goes through Tailwind utility
  classes and the design-token system (`web/tokens.json` →
  `npm run gen:tokens`) — see `web/README.md`'s "Design tokens" section.

## Contributor License Agreement

Every contributor signs a Contributor License Agreement (CLA) once, before
their first pull request is merged. We use a **license-grant** CLA, not a
copyright assignment: you keep ownership of your contribution. What you
grant Tuskira is a broad, permanent license to use it — including the
right to sublicense it and to offer it later under different license
terms (for example, as part of a commercial or hosted edition), alongside
the project's continuing open-source availability to everyone else under
its current license. See [.github/cla/INDIVIDUAL_CLA.md](.github/cla/INDIVIDUAL_CLA.md)
for the full terms, including what this means for your patents.

**Why a CLA at all, when the project is Apache-2.0?** Apache-2.0 already
gives every user a copyright and patent license to Contributions. What the
CLA adds is Tuskira's own explicit right to relicense (so the project can
later ship a commercial or otherwise differently-licensed edition without
having to track down every past contributor individually), your written
representation that the contribution is yours to give, and — if your
employer owns your work — your employer's sign-off via the Corporate CLA.

**Signing as an individual.** Open your pull request as usual. On your
first pull request, the CLA Assistant bot comments with a link to
[.github/cla/INDIVIDUAL_CLA.md](.github/cla/INDIVIDUAL_CLA.md) and asks
you to reply with a fixed line of text to sign. You only need to do this
once; it covers this and future pull requests.

**Signing as a company.** If your employer owns the intellectual property
in your contributions, a person authorized to bind the company signs the
[Corporate CLA](.github/cla/CORPORATE_CLA.md) instead (or in addition),
listing which employees are authorized to contribute on the company's
behalf (its "Schedule A"), and emails it to the contact address in
[NOTICE](NOTICE). Each authorized employee then still signs pull requests
individually through the bot, as their employer's representative under
the Corporate CLA — update the company's Schedule A at that same address
when the authorized list changes.

Either way, your contribution remains available to everyone else under
this project's current open-source license — see [LICENSE](LICENSE). The
CLA only adds rights that Tuskira, specifically, also holds.

## Release process

Releases are cut from `main` and driven entirely by
`.github/workflows/release.yml` + GoReleaser (`.goreleaser.yaml`):

1. Make sure `main` is green (the `CI` workflow passing on the commit
   you're releasing).
2. Update `CHANGELOG.md`: move the `[Unreleased]` section's entries
   under a new `## [X.Y.Z] - YYYY-MM-DD` heading.
3. Bump the kustomize image pin: in `deploy/k8s/base/kustomization.yaml`,
   set `images[].newTag` to the new `X.Y.Z` (the tag GoReleaser's
   `docker_manifests` will publish — no `v` prefix). Verify every overlay
   still builds (`kubectl kustomize deploy/k8s/overlays/<name>` for each
   one under `deploy/k8s/overlays/`).
4. Tag that commit on `main` and push the tag:

   ```sh
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

5. The workflow does the rest: builds the React admin console, then
   runs GoReleaser to cross-compile `cmd/gateway` for
   linux/darwin × amd64/arm64, publish checksummed archives and a
   GitHub Release with generated notes, and build + push multi-arch
   Docker images to `ghcr.io/tuskira/ai-agent-gateway` tagged
   `X.Y.Z` and `latest`.

A tag that doesn't match `v*` (e.g. a pre-release branch build) never
triggers the workflow, so nothing is published by accident.

To dry-run the release build locally before tagging:

```sh
make snapshot   # goreleaser build --snapshot --clean --single-target
```

(This only exercises the `builds` stage — binaries for your host
platform — not the archives, GitHub Release, or Docker images, which
need `goreleaser release` and real credentials.)

## Maintainers

### Bumping the pinned Scalar build

`internal/api/handlers/docs.go` pins `@scalar/api-reference` (`scalarVersion`,
`scalarIntegrity`); see
[docs/security-model.md](docs/security-model.md#browser-security-headers) for
why. To bump it:

```sh
npm view @scalar/api-reference version --registry=https://registry.npmjs.org/
curl -s -o /tmp/scalar.js \
  https://cdn.jsdelivr.net/npm/@scalar/api-reference@<new-version>/dist/browser/standalone.min.js \
  && openssl dgst -sha384 -binary /tmp/scalar.js | openssl base64 -A
```

Set `scalarVersion` to the first command's output and `scalarIntegrity` to
`sha384-` + the second's. Before shipping, confirm that
`https://data.jsdelivr.com/v1/packages/npm/@scalar/api-reference@<new-version>/entrypoints`
still resolves `dist/browser/standalone.min.js` as the `js` entrypoint;
jsDelivr's resolution can move between major versions.

## Adding a store backend

`pkg/store` defines the persistence contract (`store.Store` and its
sub-interfaces) plus a small driver registry; `internal/store/postgres` is
the reference implementation. To add another backend (SQLite, MySQL, …):

1. Implement `store.Store`: one facet method (`Tenants()`, `APIKeys()`,
   `Credentials()`, `Connectors()`, `AgentProfiles()`, `ToolCache()`) per
   sub-interface, plus `Migrate`, `Ping`, and `Close`. Map your driver's
   errors onto `store.ErrNotFound` / `store.ErrConflict` — callers rely on
   `errors.Is` against those, not on driver-specific types.
2. Register your driver name from an `init()`:

   ```go
   func init() {
       store.Register("sqlite", func(ctx context.Context, cfg config.Database) (store.Store, error) {
           // open + return your store.Store
       })
   }
   ```

   Callers pick it up with `store.Open(ctx, "sqlite", cfg.Database)` once
   your package is blank-imported (see `cmd/gateway/main.go`'s import of
   `internal/store/postgres`).
3. Pass the conformance suite:

   ```go
   func TestConformance(t *testing.T) {
       storetest.Run(t, func(t *testing.T) store.Store {
           // return a Store over a fresh, isolated dataset
       })
   }
   ```

   `pkg/store/storetest.Run` drives every method on every sub-interface —
   tenant isolation, unique-constraint conflicts, not-found errors,
   soft-delete visibility, search. A backend isn't done until this suite
   passes against it.

## Adding a session backend

`pkg/session` is the seam MCP sessions are persisted through: a plain
`session.Record`, the `session.Store` that keeps Records, the optional
`session.Notifier` that relays `tools/list_changed` and per-session
notifications between MCP replicas,
and a driver registry. `pkg/session/memory` (the default) and
`pkg/session/redis` are the reference drivers. To add another (a SQL
table, NATS, …):

1. Implement `session.Store`: `Get` (→ `session.ErrNotFound` for an
   unknown **or expired** id — a Store must never return a Record whose
   `ExpiresAt` has passed, so use a native TTL or filter on read), `Save`
   (an unconditional upsert that honours `ExpiresAt`; call
   `Record.Validate` first), `Delete` (idempotent) and `Close`. `Get` must
   hand back a copy the caller may mutate. If your backend cannot expire
   entries itself, also implement `session.Sweeper` and the gateway will
   call `Sweep` on `sessions.cleanup_interval`.
2. Optionally implement `session.Notifier` so the MCP plane can run more
   than one replica: `Publish(ctx, tenantID)` must reach every *other*
   instance's `Subscribe` callback and never the publisher's own (stamp
   an instance id on the message and skip it on receipt, as the redis
   driver does); `Subscribe` blocks until its context is done and then
   returns nil. `PublishSession`/`SubscribeSessions` follow the same rules
   for a `session.SessionMessage` (tenant, session id and an encoded
   notification, delivered verbatim). Return a nil Notifier if your
   backend is single-process.
3. Register your driver name from an `init()`:

   ```go
   func init() {
       session.Register("mydriver", func(ctx context.Context, cfg session.Config) (session.Store, session.Notifier, error) {
           // cfg carries the gateway's redis.* connection fields and
           // sessions.options verbatim; read what you need.
       })
   }
   ```

   Operators select it with `sessions.store: mydriver` once your package
   is blank-imported by the binary (see `cmd/gateway/main.go`'s imports of
   `pkg/session/memory` and `pkg/session/redis`).
4. Pass the conformance suites:

   ```go
   func TestConformance(t *testing.T) {
       sessiontest.Run(t, func(t *testing.T) session.Store { /* a Store over the real backend */ })
   }

   func TestNotifierConformance(t *testing.T) { // only if you return a Notifier
       sessiontest.RunNotifier(t, func(t *testing.T) session.Notifier { /* a NEW instance each call */ })
   }
   ```

   `sessiontest.Run` checks every Record field round-trips, `ErrNotFound`,
   idempotent `Delete`, upsert semantics, the expiry contract, that `Get`
   returns an independent copy, and concurrent `Save`/`Get`;
   `sessiontest.RunNotifier` checks cross-instance delivery exactly once
   and no self-delivery, for both kinds of message. Run them against the real backend and skip when
   it is not reachable (the redis driver reads `GATEWAY_TEST_REDIS_ADDR`);
   this repository does not use fakes for external services.

## Adding a metrics exporter

`pkg/metrics` is the seam the gateway's operational metrics leave the
process through. The gateway records every metric with the
OpenTelemetry metric API (`go.opentelemetry.io/otel/metric`); a driver
supplies the `MeterProvider` those instruments come from and decides
how the numbers are exported. `none` (built into `pkg/metrics`, the
default) and `pkg/metrics/prometheus` are the reference drivers. To add
another (OTLP push, StatsD, a vendor agent, …):

1. Implement `metrics.Exporter`: `MeterProvider()` returns the same
   non-nil provider on every call; `Handler()` returns the scrape surface
   of a pull exporter, or `nil` for a push exporter (the gateway then
   opens no listener); `Shutdown(ctx)` flushes and releases, and must be
   idempotent (a second call returns nil, and recording after it must not
   panic). Apply `Config.Namespace` as the prefix of every exported name.
2. Register your driver name from an `init()`:

   ```go
   func init() {
       metrics.Register("mydriver", func(ctx context.Context, cfg metrics.Config) (metrics.Exporter, error) {
           // cfg carries the gateway's metrics.* fields (driver, address,
           // path, namespace) and metrics.options verbatim.
       })
   }
   ```

   Operators select it with `metrics.driver: mydriver` once your package
   is blank-imported by the binary (see `cmd/gateway/main.go`'s import of
   `pkg/metrics/prometheus`). An unregistered name fails startup.
3. Pass the conformance suite:

   ```go
   func TestConformance(t *testing.T) {
       metricstest.Run(t, "gateway", func(t *testing.T) metrics.Exporter {
           /* a fresh Exporter built with Namespace "gateway" */
       })
   }
   ```

   `metricstest.Run` checks a stable `MeterProvider`, that counters,
   histograms, up-down counters and observable gauges can be created and
   recorded from many goroutines, that a pull driver's `Handler` answers
   `200 text/plain` with a recorded counter under `<namespace>_<name>`,
   and that `Shutdown` is idempotent. A push driver skips the scrape
   check; run it against the real collector and skip when that is not
   reachable, since this repository does not use fakes for external
   services.

## Adding an LLM provider adapter

`pkg/llm` is the seam the LLM plane translates calls through: neutral
request/response/stream types (a superset of Anthropic's content blocks and
stream events), a `Dialect` (the format a client speaks) and a `Provider`
(the format a vendor speaks), and a registry. `pkg/llm/anthropic` (dialect +
provider) and `pkg/llm/openaicompat` (provider) are the reference adapters;
`internal/llmplane/translate` is the engine that runs them, reached through
a model registry target whose vendor speaks another format than the client
(`internal/llmplane/translated.go`, `translatorFor`). To add a vendor
format (Gemini's native API, Bedrock Converse, …):

1. Implement `llm.Provider` in its own package:
   - `Capabilities()` — set a flag only for what you really carry. The engine
     refuses (`400 unsupported_by_route: <field>`) anything a flag says you
     cannot, so do not drop a field yourself; `Passthrough` is only for a
     vendor that speaks the client's own wire.
   - `BuildRequest(ctx, req, target)` — build the vendor request from the
     neutral form with a **fresh** header set: `target.Auth` and what the
     neutral request carries, nothing else. Return `*llm.ErrUnsupported` for a
     limit that depends on the target model, `*llm.RequestError` for a request
     the vendor cannot take, any other error for a gateway failure.
   - `ParseResponse` / `ParseError` — map a 2xx body onto `llm.Response`
     (usage in Anthropic's convention: input tokens exclude cached ones) and an
     error body onto `llm.Error` (types from `llm.ErrorTypeForStatus`, and
     `llm.CodeContextLength` for a context overflow).
   - `NewStreamDecoder(body)` — emit events in the order documented on
     `llm.EventType` (one block open at a time, indices 0, 1, 2, …), return
     `io.EOF` after `message_stop`, `io.ErrUnexpectedEOF` when the vendor stops
     early, and a vendor error as an `EventError`. Hold at most
     `llm.MaxFrameBytes` per pending block (and `llm.MaxBufferedBytes` in
     all); read SSE lines through a capped reader.
   - Optionally `llm.TokenEstimator`, for a better `count_tokens` estimate than
     `llm.EstimateTokens`.
2. Register it from an `init()`:

   ```go
   func init() { llm.RegisterProvider(Provider{}) }
   ```

   and blank-import the package from `cmd/gateway/main.go` next to
   `pkg/llm/openaicompat`. A new client format is the same with
   `llm.Dialect` / `llm.RegisterDialect` (and wire samples under
   `pkg/llm/llmtest/testdata/<name>/`). To make a format readable (its
   requests and responses decoded into the neutral types), implement
   `llm.Reader`, register it with `llm.RegisterReader`, and pass
   `llmtest.RunReader` with goldens under
   `pkg/llm/llmtest/testdata/readers/<name>/`.
3. Pass the conformance suite against the **real** vendor, skipping when no
   endpoint is configured (this repository does not fake vendors):

   ```go
   func TestProviderConformance(t *testing.T) {
       key := os.Getenv("LLMTEST_MYVENDOR_API_KEY")
       if key == "" {
           t.Skip("LLMTEST_MYVENDOR_API_KEY not set")
       }
       llmtest.RunProvider(t, Provider{}, llm.Target{BaseURL: "...", Model: "...", Auth: llm.Auth{APIKey: key}})
   }
   ```

   `llmtest.RunProvider` checks plain and streamed text, a tool-use round trip
   (streamed and not), usage, the stream event order (`llmtest.ValidateStream`)
   and, with `llmtest.WithThinking()`, thinking blocks. For a quick local run of
   the OpenAI-compatible provider:

   ```sh
   docker run -d --rm --name llmtest-ollama -p 127.0.0.1:21435:11434 ollama/ollama
   docker exec llmtest-ollama ollama pull qwen2.5:1.5b
   LLMTEST_OPENAI_COMPAT_BASE_URL=http://127.0.0.1:21435/v1 LLMTEST_OPENAI_COMPAT_MODEL=qwen2.5:1.5b \
     go test ./pkg/llm/openaicompat/ ./internal/llmplane/...
   docker rm -f llmtest-ollama
   ```

   A dialect runs `llmtest.RunDialect(t, d)` against its wire samples.

## Adding a sink or a header provider

These are the other two pluggable seams (see
[docs/architecture.md](docs/architecture.md#seams-in-pkg-how-a-plugin-extends-the-gateway)):

- **A sink** implements `pkg/sink.LogSink` (`WriteAccess`, `WriteLLMCall`,
  `Close`, all non-blocking) or `pkg/sink.BatchSink` (`WriteBatch`,
  synchronous, for a durable capture destination like the Postgres LLM
  sink). Wire the constructor into `cmd/gateway/main.go`'s `buildSinks`
  (or `buildBatchSink`) behind its own config section.
- **A header provider** implements `pkg/headers.ExternalProvider`
  (`ProviderID`, `ProviderName`, `ConfigSchema`, `Validate`, `Resolve`,
  and optionally `Invalidator` if resolved values can go stale). Register
  it with `dataplaneheaders.Registry.RegisterExternal` — see
  `internal/secrets.NewSecretStoreProvider`/`NewEnvProvider`/`NewFileProvider`
  for the shape, and gate anything security-sensitive behind an explicit
  config flag the way `env`/`file` are (off by default — see
  [docs/security-model.md](docs/security-model.md)).

## CI

Every push and PR runs `.github/workflows/ci.yml`: Go build/vet/test
(`-race`), `golangci-lint`, `govulncheck`, the web lint/test/build, the
Postgres integration tests, `gitleaks` (config in `.gitleaks.toml`), and
a Docker image build. The Go, web, integration and e2e test steps also
upload coverage to Codecov (flags `unit`, `web`, `integration`, `e2e`;
config in `codecov.yml`); coverage is informational and never fails a
PR. To see it locally: `go test -coverpkg=./... -coverprofile=c.out ./...`
then `go tool cover -html=c.out`, or `npm run test:coverage` in `web/`.
`make ci` runs the Go and web checks locally
(everything except the Postgres integration job, gitleaks, and the
image build). `VERSION`
for a local build comes from `git describe` (see the `Makefile`'s
`LDFLAGS`).
