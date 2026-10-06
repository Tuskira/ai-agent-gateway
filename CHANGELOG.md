# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Breaking

- **`pkg/analytics` seam:** `Reader.Overview`, `SkillsSummary`, `SkillUsage`,
  `MCPToolUsage` and `MCPServerCalls` take an `analytics.Period`
  (a resolved window with its previous period and granularity) instead of
  an `analytics.Range`, and `SankeyQuery` carries the `Period` to read. An
  out-of-tree `Reader` must read `Period` rather than resolve `Range` itself;
  the response's `Range` is set by the handler.
- **Connector headers are validated when the connector is saved.** A header
  that could never be sent (unknown `type`, a denylisted `incoming_field`
  header such as `Authorization`, a disabled `env`/`file` provider, an
  invalid header name, a `static` value or `prefix` with a line break) is
  refused with `400 validation_error` on `POST`/`PUT`; it used to save and
  then be dropped on every call. On `PUT` only headers the request adds or
  changes are checked, so a connector saved before this release can still
  be edited, enabled and disabled.
- `PUT /api/v1/profiles/{id}/tools` returns `{items, total}`, the same shape
  as `GET`; it used to return a bare array.
- `connectors.default_timeout_ms` must be between 100 and 600000 (the range
  the API accepts for `timeout_ms`); a value outside it fails startup.
- **Unknown config keys now fail startup.** The YAML config is decoded
  strictly: a key the gateway has no field for (a typo such as
  `mcp.require_profle`, a stale key from an older release) stops the
  process with `config <file>: unknown key "mcp.require_profle" at line 3`
  instead of being silently ignored, which used to leave a security setting
  at its default. Every shipped example, deploy manifest and docs snippet is
  checked by a test. Fix: correct or remove the named key. Unknown
  `GATEWAY_*` environment variables are logged as a warning, not rejected.
- **LLM capture bodies are capped at 1 MiB each by default**
  (`llm_proxy.capture.max_request_bytes` / `max_response_bytes`, previously
  `0` = unbounded). A larger body is stored cut at the cap and flagged
  `truncated`; the parsed `messages` / `system` / `tools` parts obey the
  same cap. Set the keys to `0` to keep unbounded storage.
- The stdout sink no longer prints captured bodies (request/response bodies,
  `messages`, `system`, `tools`) unless `sinks.stdout.include_bodies: true`.
  Postgres, ClickHouse and the body store are unaffected.
- **The built-in `admin` role is now a tenant admin only.** It no longer holds
  `platform.admin` or `platform.catalog.manage`, so an admin API key can no
  longer list or create tenants (`GET`/`POST /tenants`), write platform MCP
  catalog entries, or manage the model catalog. A new built-in role,
  `platform-admin` (`platform.*`), holds those; a platform-admin key also
  carries `admin`. Upgrade note: existing admin keys lose platform rights on
  upgrade (no data migration). Create a platform key with
  `gateway bootstrap-key --platform` (use `--force` if the tenant already has
  an admin key). The first key ever created in an empty database still gets
  both roles automatically.
- `auth.dev_mode` now refuses to start when an enabled plane listens on a
  non-loopback address (the default `0.0.0.0` included) unless
  `auth.dev_mode.allow_remote: true`, and `auth.dev_mode.tenant` is now
  resolved by slug (or UUID) at startup and must exist. The dev principal is
  a tenant admin; set `auth.dev_mode.platform: true` for platform-admin.
- Database migration `000008_key_profiles` adds `api_keys.profile_id`.
- **`X-Forwarded-For` is no longer trusted as sent on the MCP and LLM
  planes.** Their logged `client_ip` used the left-most `X-Forwarded-For`
  entry from any caller. All planes now share one resolver: the TCP peer
  address unless the peer is in `api.trusted_proxies`, in which case the
  header is walked from the right. Deployments behind a load balancer or
  ingress must set `api.trusted_proxies` (the `deploy/` overlays now do) or
  `client_ip` and the rate limiters see the proxy's address.
- `api.trusted_proxies` now accepts CIDRs as well as IPs, and with a trusted
  peer the client is the right-most address NOT in the trusted set, not the
  left-most `X-Forwarded-For` entry. A malformed entry fails startup. It can
  also be set as a comma-separated `GATEWAY_API_TRUSTED_PROXIES`.
- `auth.api_keys.cache_ttl` defaults to `30s` (was `60s`).
- Outbound requests now refuse internal destinations by default (SSRF
  guard). Connector endpoints, MCP catalog URLs, model registry
  `base_url`s and model catalog providers can no longer point at loopback,
  link-local (including cloud metadata `169.254.169.254`), private
  (RFC 1918), CGNAT, unique-local, multicast or unspecified addresses, in
  any spelling (`2130706433`, `0x7f000001`, `[::ffff:127.0.0.1]`, a
  hostname that resolves to one, or a redirect to one). Literal addresses,
  and a hostname (e.g. `localhost`) that resolves only to a blocked
  address, are rejected at create time with `400 egress_blocked`; the
  dial-time check remains the actual boundary, so a name that can't be
  resolved yet (or times out) is still accepted at create time and
  checked when dialed. To reach a private service, list it under the new
  `egress.allowed_cidrs` / `egress.allowed_hosts`. `deploy/docker-compose.yml`
  and the examples already allow `host.docker.internal` and loopback.
- A connector header of `{"type": "token_field", "field": "bearer_token"}`
  (which forwards the caller's own gateway credential to the backend) is
  rejected at create/update with `400 bearer_forwarding_disabled`, and a
  call through an existing connector that still has it fails with a clear
  error instead of forwarding. Set `connectors.allow_bearer_token_forwarding:
  true` to restore the old behaviour for backends you fully trust.

### Changed

- The Overview, Token Monitoring, MCPs and Skills pages share one time
  filter: Last 24h / 7d / 30d as segmented buttons (instead of a dropdown)
  plus **custom dates** (a two-month calendar; whole UTC days, up to 366),
  kept in the URL. `GET /api/v1/analytics/overview`, `client-models`,
  `traffic-flow`, the token monitoring routes, `skills`, `skills/usage` and
  `mcps/usage` accept `from=YYYY-MM-DD&to=YYYY-MM-DD` in place of `range`
  (the response's `range` is then `custom`). The console sidebar's first
  group is now **Dashboards**: Overview, then Token Monitoring.
- **Product name: "AI Agent Gateway".** Visible references to the project
  (README, docs prose, the console, the docs site) now read "AI Agent
  Gateway" instead of "AI Gateway" / "tusk-ai-secured-gateway". The repo
  name, Go module path, binary name, container image, Kubernetes
  resources, env vars, and config keys are unchanged.
- **Contributor License Agreement replaces the DCO.** Every contributor
  now signs a license-grant CLA (`.github/cla/INDIVIDUAL_CLA.md` /
  `CORPORATE_CLA.md`, adapted from the Apache Software Foundation's
  ICLA/CCLA) via the CLA Assistant bot on their first pull request,
  instead of adding a `Signed-off-by:` trailer to every commit. See
  CONTRIBUTING.md's "Contributor License Agreement" section. The DCO
  workflow and sign-off requirement are removed.
- Added `TRADEMARKS.md`, Tuskira's trademark policy for the "Tuskira" and
  "AI Agent Gateway" marks, linked from `README.md`, `NOTICE`, and the
  website footer.

### Fixed

- A credential pasted with a trailing newline is sent without it; only line
  breaks inside the value (a PEM key) are escaped.
- An MCP catalog entry's `default_headers` need a non-empty, single-line
  value (`400`/startup error), instead of storing a header that is dropped
  on every call.
- Validating the shipped MCP catalog seed shares one 2s DNS budget, so a
  hanging resolver no longer delays every gateway command by up to 40s.
- The `file` header provider's path error no longer echoes the server's
  `file_provider_root` or the stored path.
- The shipped MCP catalog (Langfuse, Atlassian, GitHub and others) is now
  seeded by default. It was read only from `configs/base/default.yml`,
  which is not loaded unless `CONFIG_PATH` points at it, so a stock
  deployment showed an empty catalog. Set `mcp_catalog.seed: []` to turn
  it off.
- A deleted MCP or agent profile can be added again under the same slug.
  The slug's unique constraint also counted soft-deleted rows, so the add
  failed with `409` although nothing by that slug was visible. Database
  migration `000009_live_slugs` makes both unique among live rows only.
- A multi-line credential (a PEM key) injected through an `external` header
  is sent with its line breaks escaped to `\n`. It used to be dropped.
  `static`, `token_field` and `incoming_field` values containing CR/LF are
  still refused.
- A profile whose name is longer than the slug column (or has no letters or
  digits) can be selected with `X-Agent-Profile-Name`. The header used to
  resolve to a different slug than the one stored, granting no tools.
- `connectors.default_timeout_ms` is the `timeout_ms` of a connector created
  without one (API or catalog). It used to be ignored in favour of 30000.
- Two replicas seeding `skills.seed_dir` at the same moment no longer fail
  startup on the duplicate name.
- A connector endpoint, MCP catalog URL, or model/catalog `base_url` of a
  hostname like `http://localhost:1234` (as opposed to a literal IP such
  as `127.0.0.1`) is now rejected at create time with the same `400
  egress_blocked` the SSRF guard above gives a literal internal address,
  instead of being accepted and only failing later, opaquely, as
  "connector is unhealthy" once the gateway tries to dial it.
  `localhost`/`*.localhost` are judged without a DNS round trip; any other
  hostname is resolved (2s budget) and judged on every address it returns.
- `test/e2e` now runs under the SSRF guard added above: the suite's
  backends listen on loopback, which the guard blocks by default, so
  without this every e2e test failed with `connector is unhealthy` /
  `egress_blocked: destination ::1 is in the internal range ::1/128`. The
  suite now installs the test-only loopback allowance
  (`internal/netguard/netguardtest.AllowLoopback`) the unit tests already
  used; production's default stays blocked.

### Security

- `deploy/docker-compose.yml` no longer writes prompts, completions and tool
  arguments to container logs: bodies are still stored for the console, but
  the stdout sink runs without them. The gateway logs a startup warning when
  `sinks.stdout.include_bodies` and `store_bodies` are both on.
- `SECURITY.md` now lists 0.3.x as supported (0.2.x best effort until 0.4.0), documents the reporting
  channels and a pre-publication checklist (see `docs/security-model.md`).
- Separated platform administration from tenant administration: a tenant admin
  key could list and create every tenant. Creating (or rotating) a key with
  role `platform-admin`, or any custom role granting `platform.*`, now needs the
  caller to hold `platform.admin`.
- Dev mode no longer silently serves unauthenticated admin requests to the
  network: it logs a startup `WARN`, refuses non-loopback binds without an
  explicit opt-in, and no longer grants platform rights by default.
- API keys can be bound to an agent profile (`profile_id` on
  `POST /api-keys` / `PATCH /api-keys/{id}`, and a "Bind to profile" select in
  the console). A bound key has that profile enforced on the MCP plane, with
  `X-Agent-Profile-Name` ignored when absent and rejected (`-32003`) when it
  names another profile. Previously the caller chose the profile by header, so
  `mcp.require_profile` did not confine a key. Unbound keys behave as before.
- Rate limiting is proxy-aware. The old limiter keyed on `RemoteAddr`
  (behind a load balancer one bad client locked out everyone) and, with a
  trusted proxy, on the spoofable left-most `X-Forwarded-For` entry (rotating
  it evaded the lockout and framed other IPs). The client IP is now the first
  untrusted address walking `X-Forwarded-For` from the right, via one shared
  `internal/clientip` resolver used by the API, MCP and LLM planes and by the
  captured `client_ip`. The kind and EKS overlays and the Docker Compose file
  ship trusted-proxy ranges.
- A second limiter dimension counts failed lookups of unknown keys per key
  prefix (`auth.rate_limit.credential_max_failures`, default 20 per
  `credential_window` 5m), so rotating IPs cannot guess one key without
  limit, and no IP is charged for it. A locked prefix still serves keys the
  process has cached, so the legitimate holder is unaffected.
- The auth-failure lockout is per plane. The API, MCP and LLM planes each keep
  their own per-IP failure counter and per-key-prefix counter (same
  thresholds), so a wrong key on one plane (say a stale MCP key in an IDE) no
  longer locks that IP out of the others (model calls keep working). The
  API-key lookup and negative caches stay shared.
- The MCP and LLM planes now lock out an IP after repeated failed
  authentication, like the API plane, answering `429` + `Retry-After` in each
  plane's own error format (JSON-RPC `-32029`; the Anthropic, OpenAI, Gemini
  or Bedrock envelope). Unknown API keys are cached negatively for 5 s
  (bounded LRU), so a flood of bad keys no longer costs one database lookup
  each (300 bad requests: 300 lookups before, 1 to 20 now).
- API-key revocation now reaches every gateway process (API, MCP, LLM) at
  once. A revoke/rotate publishes `pg_notify('gateway_key_revoked', <hash>)`
  in the same statement as the write and each process evicts the key on
  receipt (about 12 ms measured across two processes; before, another
  replica kept accepting the key for up to the 60 s cache TTL). A listener
  that reconnects flushes its cache, and the TTL remains the backstop. Stores
  without the new optional `store.RevocationNotifier` interface keep the TTL
  only. No schema migration.
- Added `internal/netguard`: every upstream HTTP client (MCP connectors,
  MCP catalog add probe, model registry targets, LLM upstreams, model
  catalog test) now dials through a guard that checks the address actually
  connected to, so DNS tricks and redirects cannot reach internal
  addresses; redirects are capped at 5 hops. The Kubernetes NetworkPolicy
  allows 443/80 egress to public addresses only (RFC 1918 and link-local
  excluded).
- The gateway logs a loud `WARN` at startup when `database.password` is
  still the shipped default `gateway` on a non-local database host or with
  TLS enabled.

### Added

- **`pkg/llm` readers:** a new optional `llm.Reader` interface
  (`DecodeRequest`, `DecodeResponse`, `NewResponseDecoder`) reads what a
  client sent and what it received into the neutral types, with a registry
  (`llm.RegisterReader`, `llm.ReaderByName`). Readers ship for every
  generation format the LLM plane relays: `anthropic` (Messages) and
  `anthropic_complete` (legacy Text Completions) in `pkg/llm/anthropic`;
  `openai_chat` (Chat Completions, the inverse of the `openai_compat`
  provider's), `openai_responses` (Responses API, SSE included) and
  `openai_completions` (legacy Completions) in `pkg/llm/openaicompat`;
  `gemini` (`generateContent`, streams as SSE or a JSON array) in the new
  `pkg/llm/gemini`; and `bedrock_converse` (Converse, ConverseStream) and
  `bedrock_invoke` (InvokeModel bodies of Titan, prompt-style, chat-style
  and Nova models) in the new `pkg/llm/bedrock`, which reads AWS event
  streams through `internal/eventstream`. A request whose messages are not
  the whole conversation carries `Extra[llm.HistoryKey]` (`server_side` for
  a Responses call that continues a stored one, `prompt` for a flat prompt);
  a Reader that sets it refuses a body that does. Readers are strict: a
  repeated key, two keys differing only by case, or an unknown role is
  refused. A `llmtest.RunReader` conformance suite runs them against golden
  files (binary streams as `stream_b64`). Serving behaviour is unchanged.
- **Detection tee: route capability and a canonical conversation.** Each
  LLM provider describes its endpoints (generate, batch, count, utility,
  and the wire format the client speaks there); the tee sends generation
  and batch calls only (it used to send every `POST` but a list of utility
  paths: unknown endpoints such as embeddings are no longer sent) and adds
  `dialect`, `op`, and, read through the format's `llm.Reader` in the tee's
  worker, `conversation` and `answer` (with `truncated` for a cut
  response), or `normalize_error` when the route has no reader (a batch)
  or the body does not parse. Every generation route of every provider has
  a reader; `history` says when the request is not the whole conversation;
  a text document keeps its text (other attachments only their type and
  size). The raw bodies are still sent, and `queue_bytes` now counts each
  turn as posted (raw bodies and canonical JSON). The contract stays
  `v: 1` and the agent's `detection/wire` gains the matching types. See
  `docs/llm-plane.md`, "Detection agent".
- **Detection agent reads the canonical conversation.** When a turn
  carries `conversation` and `answer` (and no `normalize_error`), the agent
  extracts the new turn and the reply from them, so Gemini, Bedrock
  Converse and invoke, OpenAI Responses and both legacy completion formats
  are judged like Messages and Chat Completions; otherwise it reads the raw
  bodies as before. Secrets are still found in the raw bodies and removed
  from whatever is sent. `harness_text` also carries a text document's
  text and any block the canonical shape has no slot for; with partial
  history, `user_goal` is only the turn's own text. See
  `docs/detection-agent.md`.
- **Token Monitoring** console page (`/token-monitoring`): token usage and
  cost by model, by caller (API key, with its role: agent, admin,
  interceptor or other) and by role, for the last 24h / 7d / 30d or custom
  dates (UTC), with the change against the previous period (the headline
  Total tokens includes cache reads and writes; breakdowns are input + output), a usage series, Cost
  by model cards (input / output / cache read / cache write split) and
  drill-downs per model and per key down to sessions. Backed by a new
  ClickHouse view, `llm_usage_canonical` (one definition of usage: input
  excludes cache reads for every provider, total = input + output, refused
  and failed calls excluded), and three read-only routes under
  `GET /api/v1/analytics/token-monitoring`. Visibility only: no limits.
  **Upgrade note:** the gateway creates the view on startup, so its
  ClickHouse user now also needs `CREATE VIEW`; without it the gateway does
  not start.

- Detection tee for the LLM plane (`llm_proxy.detection.agent_url`,
  `timeout`, `queue_size`, `queue_bytes`, `max_in_flight`). Each relayed
  call is posted, after it completes, as one turn (full request body, first
  1 MiB of a completed 2xx response) to a local detection agent at
  `POST {agent_url}/v1/turns`. Detection only: the post is asynchronous and
  bounded, nothing on the request path waits for the agent, and a turn that
  cannot be queued or posted is dropped and counted in the LLM plane's
  `/health`. Off unless `agent_url` is set; the contract is in
  `docs/llm-plane.md`.
- Detection agent, shipped as a nested Go module (`detection/`, versioned
  with `detection/vX.Y.Z` tags, image
  `ghcr.io/tuskira/ai-agent-gateway-detection-agent`). It is the local
  sidecar the gateway's detection tee posts each completed turn to
  (`POST /v1/turns`): it extracts the new turn, replaces secrets with
  `[REDACTED:<kind>]` on the host, and sends the request stage, then the
  response stage when there is one, to a remote detection engine from a
  background queue. Detection only: on `/v1/turns` it answers `202` at once.
  The agent and
  the agent-to-engine contract are included, an engine is not. See
  [docs/detection-agent.md](docs/detection-agent.md). `make notices` now also
  covers it, with a new `-allow` flag on `tools/notices` for the two extra
  licenses its dependencies carry.
- `examples/13-claude-desktop`: Claude Desktop (macOS) in the console via the
  public [claude-desktop-utility](https://github.com/Tuskira/claude-desktop-utility)
  and `POST /api/v1/ingest`. The walkthrough covers enabling ingest, an
  `interceptor`-role key, installing the utility and pointing Claude Desktop
  at it; `run.sh` checks the gateway side (roles, dedup, logs API, session
  timeline) with no Mac, and runs in `make examples-smoke`.
- Third-party license notices now ship everywhere the gateway does.
  `make notices` (`tools/notices`) classifies every Go module's license,
  fails the build if one falls outside an allow list (MIT, BSD-2-Clause,
  BSD-3-Clause, Apache-2.0, ISC), and writes `THIRD_PARTY_NOTICES` (full
  license + NOTICE text per module); it's included in every release
  archive (`.goreleaser.yaml`), checked in CI, and copied into the
  container image at `/licenses/` alongside `LICENSE` and `NOTICE`. The
  embedded React console carries its own `licenses.txt` (generated by
  `rollup-plugin-license`, `web/vite.config.ts`), served at `/licenses.txt`
  and linked from the console's sidebar. Every tagged release also gets a
  CycloneDX SBOM attached to its archives (`syft`, via goreleaser's
  `sboms` pipe). See the new `docs/third-party-notices.md` for the full
  picture.

### Changed

- The console's fonts (Inter, JetBrains Mono) are now bundled via
  `@fontsource/*` instead of loaded from `fonts.googleapis.com` /
  `fonts.gstatic.com` at runtime; the console's Content-Security-Policy no
  longer allows either host. The `shadcn` CLI (never imported at runtime,
  only used for `npx shadcn add ...` during development) moved from
  `dependencies` to `devDependencies` in `web/package.json`.
- The container image's `org.opencontainers.image.licenses` OCI label
  (still `Apache-2.0`, describing the gateway's own code) is now paired
  with an `org.opencontainers.image.description` label pointing at
  `/licenses/` for the third-party components also bundled in the image.

## [0.3.0] - 2026-10-01

### Highlights

- **Model registry**: register a model name once and route it to an
  ordered list of upstream targets across providers, translated through
  a shared `pkg/llm` seam (Anthropic, OpenAI-compatible, Bedrock, and
  Gemini wire formats).
- **Model Catalog**: a platform-wide list of known providers and models,
  a Connect flow for adding a model with a stored credential, and a
  price-refresh action that previews and applies current vendor pricing.
- **Budgets and limits**: per-API-key and per-model limits, including USD
  budgets, enforced on the LLM plane before a request is made.
- **Session Timeline**: a console page showing every event in an MCP
  session with exact and relative timestamps, sortable server-side.
- **Redis-backed MCP sessions** (`pkg/session`), so the MCP plane can run
  more than one replica, with notifications relayed across them.
- **Skills & commands on agent profiles**: a skills registry,
  gateway-native MCP delivery, the MCP Skills Extension, and
  documentation with a runnable example.
- **MCP catalog**: a platform-curated list of connectors a tenant admin
  can add with one click.
- **Discovery**: skills and MCP tools actually used in LLM traffic are
  now read from Bedrock, Gemini, and OpenAI Responses, in addition to
  Anthropic.
- **Ingest endpoint** (`POST /api/v1/ingest`) for externally captured
  LLM/access-log traffic, gated behind a new `interceptor` role.
- **Console username/password login**, alongside the existing API-key
  authentication.
- **Docs site**: a Docusaurus site (`website/`) built from the existing
  `docs/` folder, published to GitHub Pages.

### Changed

- The bundled session-store server is now Valkey (`valkey/valkey:9.0-alpine`,
  BSD-3) instead of Redis 7.4 (RSALv2/SSPLv1), in Compose's `redis`
  profile, `deploy/k8s/components/redis` (and so the `redis`, `kind-redis`
  and `eks` overlays) and CI. Valkey is Redis-compatible: no config or code
  changes, and `sessions.store: redis` and `GATEWAY_REDIS_*` are unchanged.
  An existing in-cluster Redis pod is replaced on the next apply; its
  sessions are lost, which costs each connected agent one re-initialize.
- The model form's "Accept API key" option and the Model Catalog Connect
  dialog's "Enter API key" now manage one credential per provider
  (`<label>-api-key`, e.g. `nebius-api-key`), not one per model: typing a
  key for a provider that already has one on file replaces its value in
  place instead of creating a duplicate, and switching a target between
  two providers that both already have a key on file repoints it to the
  right one automatically (a credential you picked by hand is left
  alone). Deleting a model whose target(s) reference a credential now
  says whether that credential is still used elsewhere, or offers
  (checkbox, off by default) to delete it too when it would otherwise be
  orphaned. The model form's vendor dropdown lists the Model Catalog's
  enabled providers, base URLs included, when there are any, falling back
  to the built-in Nebius/Together AI/OpenAI/Google Gemini presets
  otherwise.

### Added

- Ingest endpoint for externally captured LLM/access-log traffic:
  `POST /api/v1/ingest` writes gateway-shaped records into the existing
  log tables (tagged `source: "interceptor"`, with a self-reported user
  label) so they show up on the existing console pages. Gated behind a
  new built-in `interceptor` role, which grants only `ingest.write`. See
  [docs/api.md](docs/api.md#ingest) and
  [docs/security-model.md](docs/security-model.md).
- Contributor process for the public release: DCO sign-off (enforced by a
  `DCO` CI check), issue and pull request templates, Dependabot for the
  Go/npm/Docker/GitHub Actions dependencies, `SUPPORT.md`, and
  `MAINTAINERS.md`. See [CONTRIBUTING.md](CONTRIBUTING.md).
- Discovered skills and MCP tools are now also read from Bedrock (`invoke`,
  `invoke-with-response-stream` event streams, `converse`, `converse-stream`),
  Gemini (`generateContent`, SSE and JSON-array streams) and OpenAI Responses
  (`function_call` and `mcp_call`, JSON and stream) responses. See
  [docs/observability.md](docs/observability.md).
- Model Catalog price refresh: a "Refresh prices" action on a `nebius` or
  `together` provider row fetches that provider's CURRENT prices from its
  own pricing API (Nebius's public pricing feed; Together's `GET /models`
  with a key), previews the diff against the catalog
  (`POST .../prices/preview`), and applies the reviewed prices in one
  transaction (`POST .../prices/apply`), optionally also updating every
  tenant model still using the catalog's old price while leaving a
  tenant's own manual override alone. See
  [models.md](docs/models.md#refreshing-catalog-prices).
- Lifecycle states for skills and MCPs in the console: Available, Registered,
  Active, Disabled and Discovered, one shared vocabulary (`web/src/lib/lifecycle.ts`).
  The Skills and MCPs pages take a time range, show a Status/State column and a
  state filter, and list names seen in LLM traffic but not registered as
  Discovered rows with a Register / Add from catalog / Add MCP action. Disabled
  wins over traffic. With ClickHouse off, states fall back to Registered,
  Disabled and Available and Discovered rows are hidden. A connector can now be
  disabled (`metadata.enabled=false`, Disable/Enable row action): a disabled
  connector is not listed or called by the MCP plane.
- Platform MCP catalog. Operators curate entries (config `mcp_catalog.seed`,
  table `mcp_catalog`, migration 000007); a tenant admin sees them on the
  MCPs page's new Catalog tab and can "Add to tenant", which stores the
  supplied credential encrypted, creates an ordinary connector that
  remembers its `catalog_id`, and probes health and discovers tools. Nothing
  in the catalog is callable until added. New routes `GET /mcp-catalog`,
  `GET /mcp-catalog/{slug}` and `POST /mcp-catalog/{slug}/add`. Ships
  Langfuse, Supabase, Draw.io, Google Drive (OAuth, listed but not yet
  addable), Atlassian, Context7, GitHub, Stripe, Hugging Face and DeepWiki.
  Catalog entries follow the model-registry pattern: platform entries
  (seeded, or written with `platform.admin`) and tenant entries a tenant
  admin creates, edits and deletes through `POST`/`PUT`/`DELETE`
  `/mcp-catalog`, and from the Catalog tab's "New catalog entry". See
  docs/connectors-and-credentials.md.
- Discovered skills and MCP servers. The gateway now records which skills
  and MCP tools agents actually used, read from the model's response (never
  request history; Anthropic Messages JSON/SSE and OpenAI chat completions,
  and ingested `response_body`): `llm_calls.skills_used` and
  `mcp_tools_used` (ClickHouse and Postgres, added in place, names only; the
  OTLP sink gets counts only). New `GET /analytics/skills/usage` and
  `GET /analytics/mcps/usage` (`analytics.read`) report usage with
  `registered` state, and attribute tools reached through the gateway's own
  MCP plane (`mcp__gw__langfuse__get_trace`) to the registered connector
  (`via_gateway`). See [docs/observability.md](docs/observability.md).
- Model Catalog: a platform-wide list of known providers (Nebius, Together
  AI, OpenAI, Google Gemini, seeded by default) and their models — Kimi K3
  and GLM 5.3 (`capabilities: {"tools": true, "streaming": true}`) and
  GPT-6/GPT-6.1/Gemini 3 Flash (`{"tools": false, "streaming": true}`, with
  a `notes` field explaining that multi-turn tool use through Chat
  Completions translation isn't supported yet; plain chat works) — that
  tenants can connect with one click. A tenant "connects" a provider by
  supplying an API key once (`POST
  /api/v1/model-catalog/providers/{id}/connect`, credential type `api_key`
  payload `{"api_key": "<key>"}`, matching the console's own "Accept API
  key" credential) and selecting which catalog models to register; the
  gateway creates ordinary tenant models from them (targets labelled with
  the provider's slug, same convention as the console's vendor presets),
  and `GET /api/v1/model-catalog` lists the catalog merged with the
  caller's own registrations. Providers/models are managed through
  `POST`/`PUT`/`DELETE /api/v1/model-catalog/providers[/models]`, gated on
  the new `platform.catalog.manage` permission (API-key `admin` always; a
  console `admin` only in a single-tenant deployment — see
  [security-model.md](docs/security-model.md)). A connectivity test route
  (`POST .../providers/{id}/test`) calls the provider's `/models` endpoint
  with a raw or existing credential and reports which catalog models it
  actually lists (`catalog_matches`, keyed by vendor model id), without
  ever echoing the key. See [api.md](docs/api.md)'s new `/model-catalog/*`
  rows.

  On the console, a new admin page (`/model-catalog`, under Configure)
  lists providers and, per provider, their catalog models — add/edit/
  delete both, with base-URL warnings shown under the field and a
  usage-count confirmation before a forced delete of one still referenced
  by tenant models. It's read-only (a banner explains why) for anyone
  without `platform.catalog.manage`. The Models page gains an "Available"
  status for a catalog model this tenant hasn't set up yet (shown with its
  catalog provider's name), and a "Set up" row action — also offered on a
  "Discovered" row whose name or model id matches one — that opens a
  "Connect <Provider>" dialog: pick which of the provider's models to
  register, reuse an existing credential or enter a new API key, test the
  connection against the real provider first (shows which selected models
  it actually lists), and see capability/price gaps (no tool calling, tool
  support unknown, no price set) before committing — including a catalog
  model's admin-set `notes` explaining a limited capability, shown next to
  the warnings, in the Connect dialog and in the admin catalog page's
  models table, and editable there in a new Notes field. Deleting a
  credential now also warns about, and lists, any models whose targets
  reference it. See [docs/models.md](docs/models.md) for the catalog
  concept, the Models page's five states, and the Connect flow end to end.
- **Fixed**: an `openai_compat` registry target's `base_url` now follows
  the OpenAI SDKs' own convention (it includes the API version segment,
  e.g. `https://api.groq.com/openai/v1`) so the gateway strips the
  client's leading `/v1` before joining — the previous convention (no
  version segment in `base_url`) silently doubled it
  (`{base_url}/v1/chat/completions` instead of
  `{base_url}/chat/completions`) for a same-dialect OpenAI passthrough to
  a registry target with an explicit `base_url`. Existing registry rows
  with a `base_url` that does not already include a version segment need
  it added. The translated-target path and the non-registry default
  OpenAI provider path are unaffected. See
  [llm-plane.md](docs/llm-plane.md)'s `base_url` convention.
- **Fixed**: the Connect dialog's "Found on provider" / "Not seen on
  provider" indicator, and its "N of M found" summary after a test
  connection, now look up each selected model by its vendor model id
  (`model.model_id`, e.g. `zai-org/GLM-5.3`) instead of the catalog row's
  internal uuid — `catalog_matches` is keyed by the former, so this never
  matched before and every selected model silently showed "Not seen on
  provider" regardless of the test result.
- Models page: a model name seen in gateway traffic with no matching
  registry entry now shows status "Discovered" (a muted pill with a
  tooltip explaining calls pass straight through to the vendor) instead
  of "Active", so it reads as observed-but-unmanaged rather than a
  registered model with traffic. It has no row actions, same as before.
  Registered models are unaffected.
- Session Timeline can be sorted server-side: `GET /analytics/sessions/{id}/timeline?order=asc|desc`
  (default `asc`, anything else is 400) sorts by `(ts, id)` so equal-timestamp
  events never swap, and echoes `order`. The console gets an "Oldest first" /
  "Newest first" toggle (`?order=desc`, remembered per browser) that sends the
  parameter instead of re-sorting in the browser; pagination can follow the
  same order without changing the contract.
- Session Timeline rows now show the date and time of every event in the
  browser's local time zone (`30 Sep 20:59:52`, with the year only when it is
  not the current one), a hover tooltip with the exact time to the
  millisecond plus UTC, the zone once in the header, and readable durations
  (`984 ms`, `44.9 s`, `3m 53.8s`, `1h 02m 05s`) in fixed-width, right-aligned
  columns so times no longer shift with the duration badge.
- Console username/password login. The admin console now signs people in
  with a tenant, username and password instead of only an API key. Users
  belong to exactly one tenant and have role `admin` (everything except
  `platform.admin`) or the new read-only `viewer` (`*.read` only). Sessions
  are an opaque `gw_session` cookie (`HttpOnly`, `SameSite=Strict`, `Secure`
  over TLS), stored hashed, with an 8 h idle and 24 h absolute limit
  (`auth.session_idle` / `auth.session_max`); cookie-authenticated writes
  need a matching `X-CSRF-Token`, API-key requests are exempt, and the MCP
  and LLM planes never accept the cookie. Passwords are argon2id with a
  12-character minimum. Failed logins are indistinguishable (one generic
  `401`), count toward the existing per-IP lockout, and lock an account for
  15 minutes after 5 consecutive failures. New routes: `GET /auth/config`,
  `POST /auth/login`, `POST /auth/logout`, `POST /auth/password`, `GET
  /auth/audit`, and `/users` (create, list, get, patch, delete,
  `reset-password`, `revoke-sessions`), the latter gated on a new
  `users.manage` permission (admin role and admin API keys only; agent keys
  get `403`). `GET /auth/me` now also reports `kind` and, for users, the
  account, `must_change_password` and `csrf_token`. A user with a temporary
  password can do nothing but change it (`403 password_change_required`).
  An admin resets a password by API or console (one-time temporary password,
  all sessions revoked); `gateway create-user` and `gateway reset-password`
  do the same from the host, including as break-glass. Every
  authentication action is written to a new `auth_audit` table (migration
  `000005_users`). The console's API-key sign-in stays as an emergency
  fallback behind `auth.console_api_key_login` (default `true`, planned for
  removal). The control-plane rate limiter no longer counts a request that
  carries no credential at all as a failed attempt. See
  [docs/authentication.md](docs/authentication.md).
- Console first-run guidance for password login. `GET /auth/config` gains
  `has_users` (false only on a single-tenant install with no active user;
  always true with several tenants). When false, the login page shows the
  API-key form first with "No console users yet...", or points at `gateway
  create-user` when API-key login is off. An admin signed in with an API key
  sees a dismissible banner to create a personal login, and the Users page
  warns an API-key session when no active admin user exists. With
  `auth.console_api_key_login` off, startup logs a WARN for each tenant with
  no active admin user. See [docs/authentication.md](docs/authentication.md).
- Agent traffic flow analytics: `GET /api/v1/analytics/traffic-flow`
  (`analytics.read`, ClickHouse-backed, same `range`/`metric`/`limit`/
  `client_name` parameters and defaults as `/analytics/client-models`)
  joins both planes into one CLIENT → PATH (`llm` | `mcp`) → MODEL |
  CONNECTOR graph: `llm_calls` by client family and requested model on the
  LLM branch, `tools/call` rows from `mcp_access_logs` by User-Agent family
  (classified with the same `sink.ClientFamily` rule) and `connector_id` on
  the MCP branch, with each terminal column's tail folded into
  `other-models` / `other-connectors` and `MODEL` nodes carrying their
  provider as a new optional `sublabel`, plus an `agents` list of every
  client family in the window for the console's filter — see
  [docs/observability.md, "Agent traffic flow"](docs/observability.md#agent-traffic-flow).
  The console's Overview page replaces its "Clients → models" card with an
  "Agent traffic flow" Sankey of this graph, following the page's range
  picker, with an LLM/MCP path legend and an agent filter (`GET
  /analytics/client-models` itself is unchanged).
- Docs site: `website/` is a Docusaurus shell over the unchanged `docs/`
  folder (CommonMark mode, explicit sidebar, no front matter added to the
  docs), published to GitHub Pages at <https://community.tuskira.ai> by
  `.github/workflows/docs.yml` on every merge to `main` that touches
  `docs/` or `website/`; pull requests get a build check, and a broken
  link or anchor fails it. The few links in `docs/` that pointed outside
  the folder (`SECURITY.md`, `CONTRIBUTING.md`, `CODEOWNERS`,
  `deploy/README.md`, `examples/`) are now full GitHub URLs so they work
  from the site as well as on GitHub.
- Client → model Sankey analytics: `GET /api/v1/analytics/client-models`
  (`analytics.read`, ClickHouse-backed, default `range=7d`,
  `metric=calls|tokens|cost`, `limit` default 10, optional `client_name`
  filter) aggregates `llm_calls` into a three-column CLIENT → MODEL →
  PROVIDER usage graph, in a `nodes`/`links` shape Sankey chart libraries
  render directly, with models beyond `limit` folded into one `other-models` node — see
  [docs/observability.md, "Client → model Sankey"](docs/observability.md#client--model-sankey).
  The console's Overview page gets a "Clients → models" card rendering the
  same graph with `recharts`' `Sankey` component and a calls/tokens/cost
  metric toggle.
- Client attribution on LLM calls: `sink.LLMCall` gains `ClientName` (the
  caller's User-Agent classified into a named family — `claude-code`,
  `cursor`, `vscode`, `codex`, `claude-desktop`, `other`, or `""` when no
  User-Agent was sent) and `UserAgent` (the raw header, capped at 256
  bytes), set on every LLM-plane capture path (`internal/llmplane`'s
  normal, error, and translated-estimate emits). The classifier
  (`sink.ClientFamily`) is shared with the MCP plane's `requestsByClient`
  Overview chart, so the same caller classifies identically on both
  planes — see [docs/observability.md, "Client classification"](docs/observability.md#client-classification).
  Both fields persist through Postgres and ClickHouse (`llm_calls.client_name`/
  `user_agent`, idempotent `ADD COLUMN IF NOT EXISTS` upgrades) and the OTel
  LLM span (`client_name` attribute). `GET /api/v1/analytics/llm-logs`
  accepts a `?client_name=` filter. The console's LLM Logs page gets a
  "Client" badge column, a client filter, and the detail drawer shows the
  client family plus the raw User-Agent.
- Skill usage analytics, live list-changed notifications and version
  pinning UX (Phase 5 of skills & commands on agent profiles, building on
  Phase 2): `GET /api/v1/analytics/skills?range=24h|7d|30d` (`analytics.read`,
  ClickHouse-backed, default `7d` like `/analytics/models`) aggregates
  `mcp_access_logs` rows carrying a `skill_name` (a `gateway__skill` call or
  a native command's `prompts/get`) into per skill/command `calls`,
  `used_by` (distinct `key_id`) and `last_seen`, with `kind` joined in from
  the skill/command registry by name (a name with traffic but no matching
  registry row — deleted since — still lists, with `kind` left `""`) and a
  `most_used` summary. The console's Skills page gains Calls/Used
  by/Last seen columns and a "Most used (7d)" tile, plus the same
  analytics-off and empty states `/models` uses. Live updates: attaching,
  detaching, or editing a profile's skills/commands/instructions
  (`PUT /profiles/{id}/skills|tools`, `PUT /profiles/{id}`, `DELETE
  /profiles/{id}`) now also pushes a live `notifications/tools/list_changed`
  **and** `notifications/prompts/list_changed` to any `GET /mcp/stream`
  currently open with a matching `X-Agent-Profile-Name` header
  (`internal/dataplane/transport.Hub.NotifyProfileListChanged`, keyed by
  tenant + profile slug) — in-process and same-replica only, same reach as
  the existing cache invalidation it rides alongside; see docs/profiles.md,
  "Live updates over the SSE stream" for the exact limit on a split
  multi-replica deployment. Profile Studio's "Skills & commands" tab shows
  an "Outdated" badge on a pin behind the registry's current
  `latest_version`, with a one-click "Update to latest" that clears it back
  to tracking latest (still needs Save to persist).
- Skills & commands documentation and a runnable example (Phase 4 of
  skills & commands on agent profiles): new
  [`docs/skills.md`](docs/skills.md) (skill vs. command vs. instructions,
  the registry and its versions, platform vs. tenant, validation rules
  and limits, attachment-as-grant and pinned/"outdated" versions, all
  three delivery paths, security notes, and the seed); updated
  `docs/profiles.md` (a Skills Extension cross-reference), `docs/api.md`,
  `docs/architecture.md`, `docs/configuration.md` (`skills.seed_dir`,
  previously undocumented), `docs/README.md`, and this file's feature
  list. New [`examples/12-skills-and-commands`](examples/12-skills-and-commands/)
  registers a skill and a command, attaches both to a profile, and drives
  `initialize`'s skill index, the native `gateway__skill` tool, a
  `fix-lint` command as a native prompt, and the MCP Skills Extension's
  `skills/list`/`resources/read` with a verified `sha256` digest, plus a
  no-profile-header negative; added to `make examples-smoke`. Also ships
  `sync-skills.sh`, an optional third delivery path that pulls a
  profile's attached skills into `.claude/skills/` via the control-plane
  API, with a `SessionStart` hook snippet.
- Profiles + gateway-native MCP delivery (Phase 2 of skills & commands on
  agent profiles, building on the Phase 1 registry): `PUT/GET
  /api/v1/profiles/{id}/skills` attaches/reads a profile's skills and
  commands (replace semantics like `/tools`; every `skill_id` must be
  visible to the tenant, a pinned `version` must exist), and `PUT/GET
  /api/v1/profiles/{id}` gain `instructions` (free text, ≤ 8 KiB,
  prepended to the resolved profile's MCP `initialize` instructions).
  `internal/dataplane/profile.Enforcer` now resolves a profile's
  `Instructions`, enabled-only `Skills` and `Commands` (pinned version or
  current latest) alongside its tool allow-list, cached on the same 30s
  TTL; profile/tool/skill writes invalidate it immediately on this
  replica via a new `pkg/ops.ProfileOps` seam (a split api/mcp
  deployment still relies on the TTL on other replicas — see
  docs/profiles.md). `initialize`'s instructions gain a capped skill
  index ("Skills available to this profile...") when the profile has
  skills, and the `prompts` capability is advertised when it has
  commands. A native tool `gateway__skill` (listed only when the profile
  has ≥1 skill) loads one skill's file content by name
  (`{name, path?}` → `{content:[{type:"text", text}]}`); an
  unattached/unknown skill is `-32003` with `{skill, profile}`, an
  unknown path `-32602`. A profile's attached commands are served as
  native MCP prompts: `prompts/list` merges them (plain names, no
  `__`) alongside connectors' own, and `prompts/get` on a name with no
  `__` renders the command's template (`{{arg}}` substitution via the
  new `internal/skills.RenderTemplate`; a missing required argument is
  `-32602`, an unattached command `-32003`) — connector prompts keep
  working unchanged. The connector name/slug `gateway` is now reserved
  (`400` on create/update) so `gateway__*` can never collide with a real
  connector's tools. Capture: `sink.AccessLog` gains `SkillName`, set for
  a `gateway__skill` call or a native command's `prompts/get`; ClickHouse
  `mcp_access_logs` gains the matching `skill_name` column (`ADD COLUMN
  IF NOT EXISTS`, plus the OTel span attribute) — the LLM-call tables
  (Postgres and ClickHouse) are untouched, since they don't relate to
  skill/command invocation.
- MCP Skills Extension (SEP-2640, Phase 3 of skills & commands on agent
  profiles): `skills/list`, `skills/get`, and `resources/read` under
  `skill://<name>/<path>` URIs, serving the skills (not commands) attached to
  the caller's agent profile. `initialize` advertises `resources` and
  `extensions: {"io.modelcontextprotocol/skills": {}}` only when the resolved
  profile has at least one attached, enabled skill. Both `skills/list` and
  `skills/get` return `{uri, frontmatter, resources: [{uri, digest:
  "sha256:<hex>", size}]}` per skill (SKILL.md first in the manifest) with
  `resultType: "complete"`, `ttlMs: 30000` and `cacheScope: "private"`;
  `skills/list` paginates (page size 50, decimal-offset cursor). An
  unattached or unknown skill is `-32003` with `{skill, profile}`; an unknown
  file or malformed `skill://` URI is `-32602`. Resolution
  (`internal/dataplane/skillsext`) reads `pkg/store.SkillStore` and
  `AgentProfileStore.GetSkills` directly, independently of
  `internal/dataplane/profile`'s tool allow-list (Phase 2, landing in
  parallel), with the same 30s cache. `pkg/mcp` gains the `skills/list`/
  `skills/get` method names, `ServerCapabilities.Extensions`, the
  `SkillEntry`/`SkillsListResult`/`SkillsGetResult` wire types, and
  `resultType`/`ttlMs`/`cacheScope` on `ResourcesReadResult`.
- Skills registry (Phase 1 of skills & commands on agent profiles):
  `POST/GET /api/v1/skills`, `GET/PUT/DELETE /api/v1/skills/{id}`, and
  `POST/GET /api/v1/skills/{id}/versions` + `GET .../versions/{v}`
  (permissions `skill.create|read|update|delete`, covered by the
  built-in admin `*` and agent `*.read` grants). A skill or command is a
  versioned bundle of text files (`SKILL.md` required, plus up to 19
  supporting files; internal/skills validates path shape, extensions,
  UTF-8, size caps and a secret-content scan, and parses+validates
  `SKILL.md`'s YAML frontmatter against an allowed-key list with `hooks`
  explicitly rejected). A skill's `description` is always derived from
  that frontmatter and is read-only through the API (a `description` in
  a create/update request body is accepted but ignored). `kind:
  "command"` additionally declares named
  `arguments` and validates every `{{placeholder}}` in its template
  body against them. Migration `000004_skills` adds `skills`,
  `skill_versions` and `profile_skills` (tenant_id NULL = a
  PLATFORM row, visible to every tenant and read-only through the API,
  same convention as `models`) and `agent_profiles.instructions`.
  `pkg/store` gains `SkillStore`, `Store.Skills()`, and
  `AgentProfileStore.SetSkills`/`GetSkills` (Phase 1 implements these;
  Phase 2 exposes them on `/profiles/{id}/skills`). Config-driven seed:
  `skills.seed_dir` (env `GATEWAY_SKILLS_SEED_DIR`) walks a directory of
  skill bundles into platform rows at startup, idempotent like
  `llm_proxy.models`'s `SeedModels`.
- Console **Models** page (`/models`, sidebar under Configure): tiles
  (Total Models, Highest traffic (7d)), a table of every model seen in
  gateway traffic (Name, Provider, Calls/Tokens/Cost (7d), Used by,
  Status) merged with the model registry's own entries (registered but
  unseen models show as `calls: 0`, status "registered"; platform-scope
  rows are read-only with a "platform" badge), and an Add/Edit Model
  dialog (name, description, enabled, an ordered target list —
  vendor/model id/base URL/region/credential — and optional per-1M-token
  pricing) against the model-registry API (`/api/v1/models`, landing
  alongside this in the same release). Empty, admin-required, and
  analytics-off states match the designer's mock. New endpoint backing
  the traffic side: `GET /api/v1/analytics/models?range=24h|7d|30d`
  (default `7d`; `analytics.read`), returning per-model calls, tokens,
  nullable cost, distinct callers (`used_by`), and last-seen: one row
  per requested model name (`llm_calls.requested_model`), whose `provider`
  is the vendor that served most of its calls (`resolved_vendor`, else the
  client's dialect). The dialog also edits each target's
  `allow_caller_key` and the model's `limits`.
- API key limits: `api_keys.limits` (migration `000003_limits`, nullable
  JSONB) holds optional `daily_usd`, `monthly_usd`, `rpm` and
  `max_tokens`. `POST /api/v1/api-keys` accepts `limits`, the new
  `PATCH /api/v1/api-keys/{id}` (`admin.manage`) replaces or clears
  them, list/create/rotate responses include them, and rotate carries
  them over. Validation: at least one field, every field non-negative,
  `rpm` at most 100000. `pkg/store.APIKeyStore` gains `SetLimits`
  (conformance case in `pkg/store/storetest`).
- Model limits: `models.limits` (same migration `000003_limits`, same
  shape and validation). `POST`/`PUT /api/v1/models` accept `limits`
  (PUT replaces them; omitted = cleared) and every model response carries
  `"limits"` (`null` when none). The LLM plane enforces them after the
  registry lookup, on top of the calling key's limits, per
  (tenant, requested model name): `400` / `429` + `Retry-After` / `503`
  with messages naming the model, captured with `fallback_index: -1`.
  `pkg/store.Model.Limits` (conformance case `ModelLimits`);
  `pkg/sink.SpendReader` gains `ModelSpend`, and the Postgres LLM sink
  creates a `(tenant_id, requested_model, timestamp)` index at startup.
- LLM plane: per-API-key limits are enforced before a request is
  forwarded (`internal/llmplane/limits.go`, `llmplane.Config.Limiter`).
  `max_tokens` above the key's limit → `400` (never clamped); spend at or
  over `monthly_usd` / `daily_usd` (UTC month / day, from the Postgres
  `llm_calls` table, unknown cost counted as $0, cached 10 s per key) →
  `429 rate_limit_error` with `Retry-After` to the next boundary; more
  than `rpm` requests in a rolling minute (per replica) → `429` with
  `Retry-After: 1`. Refused calls are captured as `llm_calls` rows with
  their status and `error`. `GET /api/v1/health` and the LLM plane's
  `/health` report `limits: {budget_denials, rpm_denials}`. New
  `pkg/sink.SpendReader`, implemented by the Postgres LLM sink, which now
  creates a `(tenant_id, key_id, timestamp)` index on `llm_calls` at
  startup (a one-time build on an existing table). A key with a USD
  budget is refused (`503`) when the capture store is not Postgres.
- Model registry targets may use plain `http` to `host.docker.internal`
  (exact name) as well as loopback hosts, so a gateway running in Docker
  can point at a model server on the same machine.

- **Model registry** (LLM plane). A tenant registers a model *name* that
  maps onto an ordered list of upstream targets
  (`{vendor: anthropic|bedrock|openai_compat|gemini, model, base_url?,
  credential?, region?, allow_caller_key?, label?}`) plus an optional flat `price`; clients keep
  calling the endpoint they use today with that name in `model`, and the
  gateway rewrites only the upstream host, the credential, the Bedrock region and the model id.
  Whose key reaches the vendor is decided per target: its own
  `credential` (tenant secret store) when it names one; else the caller's
  own key (BYOK) **only** when the target is on the gateway's configured
  default host for that vendor or sets `allow_caller_key: true` (default
  `false`); any other target without a credential is unusable — never
  called, the next target is tried, and with none usable the call is
  refused with `400 "model <name>: no credential for target <i> (set a
  credential or allow_caller_key)"` in the client dialect's envelope. The
  gateway's own key and other vendors' key headers are never forwarded. Same-dialect pairs: Anthropic → Anthropic or Bedrock
  (Anthropic model ids; `/v1/messages` becomes a signed `InvokeModel`,
  streams re-framed from Bedrock's event stream to SSE), OpenAI →
  `openai_compat`, Bedrock → Bedrock, Gemini → Gemini. A target of another
  wire format is **translated** when `pkg/llm` has an adapter pair for
  it — Anthropic → `openai_compat`, so Claude Code (or any Anthropic SDK
  client) can drive OpenAI, Groq, DeepSeek, GLM, Ollama, vLLM, ... through
  `/v1/messages`, JSON and SSE, captured with `translated: true`; anything
  else is `400 "model <name> requires translation (not available)"`.
  Translation refuses what the vendor cannot carry instead of dropping it
  (`400 invalid_request_error "unsupported_by_route: <field>"`: PDF/URL/file
  documents, vendor-defined tools such as `web_search`, unknown content
  blocks and fields, structured output, non-default sampling on OpenAI
  reasoning models); only a documented list of non-semantic hints
  (`metadata`, `top_k`, `cache_control`, `context_management`, tool
  `strict`/`defer_loading`/`eager_input_streaming`, thinking budgets,
  earlier-turn thinking) is dropped. `count_tokens` on a translated target
  is `{"input_tokens": N, "estimated": true}` (characters/4), captured.
  Streamed tool calls are emitted whole when the vendor finishes, so
  interleaved parallel calls keep every argument fragment; tool-call
  arguments that are not a JSON object are kept raw under
  `_gateway_invalid_arguments`; one upstream stream line is capped at
  1 MiB. The vendor gets a fresh header set: no client header reaches it.
  Targets fall back in order on a build failure,
  dial/TLS error or `408`/`429`/`5xx` **before any byte reached the
  client**; the last target's outcome is relayed. Unregistered names are
  the byte-for-byte passthrough as before. See
  [docs/llm-plane.md](docs/llm-plane.md#model-registry).
  - Translated targets and keys: the target's `credential`, else — only
    with `allow_caller_key: true` — the caller's `X-Provider-Key-<label>`
    (`X-Provider-Key` for a target without a label), else its dialect's
    own credential header unless that is the gateway's (`gk_…`) or an
    Anthropic one (`sk-ant-…`); a caller that sends none is served without
    an `Authorization` header (a keyless local server). `X-Provider-Key*`
    headers are never forwarded, on any path.
  - `label` (`openai_compat` targets only; `[a-z0-9._-]{1,32}`, not
    `anthropic`/`bedrock`) names the vendor behind the endpoint: it is
    capture's `resolved_vendor`, the provider the call is priced as (and so
    its token convention), and the suffix of the caller's key header.
  - `pkg/store`: `Model`/`ModelTarget`/`ModelPrice`, `ModelStore`
    (`Create`/`Get`/`GetByName`/`List`/`Update`/`SoftDelete`; `tenant_id
    NULL` = platform default every tenant sees, a tenant row of the same
    name wins) and `Store.Models()`; migration `000002_models`;
    conformance section in `storetest`.
  - API: `POST/GET /api/v1/models`, `GET/PUT/DELETE /api/v1/models/{id}`
    (`model.create|read|update|delete`; platform rows read-only, `403`);
    responses carry `"scope"` and credential names only.
  - Capture: `llm_calls` (Postgres and ClickHouse, `ADD COLUMN IF NOT
    EXISTS`) gains `requested_model`, `resolved_vendor`,
    `resolved_model`, `translated`, `fallback_index`; `model` is the
    vendor model actually called; `cost_usd` is priced on the resolved
    model, or the row's `price` (`pricing.CostOverride`).
  - Config: `llm_proxy.models` (YAML only) seeds platform rows by name at
    startup, validated like an API write.
- `pkg/llm`: a public seam for translating LLM calls between wire formats —
  neutral request/response/stream-event types (a superset of Anthropic's
  content blocks and events), `Dialect` (client side: `ParseRequest`,
  `RenderResponse`, `RenderError`, `NewStreamEncoder`) and `Provider` (vendor
  side: `Capabilities`, `BuildRequest`, `ParseResponse`, `ParseError`,
  `NewStreamDecoder`, optional `TokenEstimator`) interfaces, and a registry
  (`RegisterDialect`/`RegisterProvider`, `DialectByName`/`ProviderByName`).
  `pkg/llm/llmtest` is the conformance suite: `RunProvider` against a real
  vendor (env-gated), `RunDialect` against recorded wire samples,
  `ValidateStream` for event order. Two adapters ship and are blank-imported
  by `cmd/gateway`: `pkg/llm/anthropic` (dialect + provider) and
  `pkg/llm/openaicompat` (provider: OpenAI Chat Completions and the
  vendors that speak it).
- `internal/llmplane/translate`: the translation engine
  (`Prepare`/`Call.Response` used by the router, `Run` as a standalone
  handler, `CallerKey` picking a caller's own vendor key). It is reached
  only through a model registry target.
- MCP resource subscriptions and upstream notification relay.
  `resources/subscribe`/`resources/unsubscribe` are now served for
  `gw://<connector>/<uri>` URIs on a session (same profile rule as
  `resources/read`, `-32003` when denied): the subscribe is forwarded
  with the connector's original URI, and the connector's
  `notifications/resources/updated` for it reach the session's
  `GET /mcp/stream` with the URI re-namespaced; anything the session did
  not subscribe to is dropped. To receive them the gateway holds one
  long-lived server-to-client stream per (session, connector) — new
  package `internal/dataplane/upstream` — opened lazily on the first
  subscribe with the connector's resolved headers, reconnected with
  1s→30s jittered backoff and `Last-Event-ID`, never a health signal (an
  idle stream has no timeout), recorded as "no stream" and not retried
  when the connector answers `GET` with `405`/`404` (the subscribe is then
  `-32601`), and closed with the session's last subscription on that
  connector or with the session. Requests a connector sends on it go to
  an `upstream.RequestHandler` (see the server-initiated requests entry
  below). The same streams relay
  `notifications/prompts/list_changed` and
  `notifications/resources/list_changed` to the session, and turn
  `notifications/tools/list_changed` into a re-list of that connector's
  cached tools plus the tenant-wide signal. `initialize` now advertises
  `resources.subscribe`, `resources.listChanged` and
  `prompts.listChanged` when a reachable connector reports them. New
  config: `mcp.max_upstream_streams_per_session` (16) and
  `mcp.max_subscriptions_per_session` (256). With the `redis` session
  driver, a notification for a session whose stream is on another
  replica is relayed there: `pkg/session.Notifier` gains
  `PublishSession`/`SubscribeSessions` and `SessionMessage` (redis
  channel `gw:session_msgs`; `sessiontest.RunNotifier` covers them), and
  `transport.Bridge.Notify` publishes when no local stream took the
  message — which also carries `tools/call` progress across replicas.
  `pkg/mcp` gains `NotificationResourcesUpdated`,
  `NotificationResourcesListChanged`, `NotificationPromptsListChanged`,
  `ResourcesSubscribeParams` and `ResourceUpdatedParams`. See
  [architecture.md](docs/architecture.md#upstream-streams-and-subscriptions).
- Server-initiated MCP requests (`sampling/createMessage`,
  `elicitation/create`, `roots/list`) relayed between connectors and
  agents, gated twice. **Policy:** each is a per-connector trust decision
  and off by default: `connector.metadata.server_requests =
  {"sampling": bool, "elicitation": bool, "roots": bool}`, validated on
  `POST`/`PUT /connectors` (unknown key or non-boolean value is a `400`),
  never masked; `internal/dataplane/client.ServerRequests`/
  `ServerRequestPolicy.Allows` read it. The console's connector form
  gains a "Server-initiated requests" section with one switch per type,
  and the detail view shows them as read-only badges. **Agent:** the
  capabilities an agent declares at `initialize` (`sampling`,
  `elicitation`, `roots`, kept raw) are recorded on the session
  (`pkg/session.Record.ClientCapabilities`, round-tripped by every store;
  `sessiontest` checks it). The gateway now declares to each connector
  exactly the intersection of the two — and no longer declares `roots`
  unconditionally — and, when that is not empty, opens the connector's
  upstream stream for the session at the handshake. **Relay:** a request
  arriving inside a `tools/call`'s streamed reply (new
  `client.Call.OnRequest`, up to 4 at once per call, answered while the
  reply keeps being read) or on the upstream stream
  (`upstream.RequestHandler`, now the orchestrator) is re-checked against
  both gates (`-32601` otherwise, never reaching the agent), sent to the
  session's `GET /mcp/stream` as a request with id `gw-<16 hex>` and the
  connector's params verbatim, and answered by the agent with a JSON-RPC
  response `POST`ed to `/mcp` (now accepted: `202`, no body; matched on
  tenant, session and id, so no other session can answer). The result or
  error goes back to the connector under its own id via `client.Reply`.
  `-32603` when the session has no stream open, has
  `mcp.max_pending_server_requests_per_session` (32) requests pending, or
  does not answer within `mcp.server_request_timeout` (5m, the agent is
  then sent `notifications/cancelled`), or ends meanwhile; cancelling the
  `tools/call` cancels its relayed requests. The connector's `timeout_ms`
  no longer runs while a call waits on the agent: `client.Do` applies it
  as a pausable timer. With the `redis` driver the replica running the
  call owns the pending entry: `pkg/session.SessionMessage` gains `Kind`
  (`request`, `ack`, `response`), a request for a stream on another
  replica is acknowledged by that replica, and an answer POSTed elsewhere
  is relayed back. Each relayed or refused request is access-logged
  (`method` = the MCP method, `tool_name` = the connector slug, no
  content). Example
  [11-server-requests](examples/11-server-requests/). See
  [connectors-and-credentials.md](docs/connectors-and-credentials.md#server-initiated-requests-sampling-elicitation-roots).
- MCP plane: `prompts/list`, `prompts/get`, `resources/list`,
  `resources/read` and `resources/templates/list` are now served (they
  were previously `-32601` even though `initialize` advertised the
  `prompts`/`resources` capabilities, so clients such as Claude Code
  failed on connect). Prompts are namespaced like tools,
  `<connector>__<prompt>`; resource URIs (and `uriTemplate`s) are wrapped
  as `gw://<connector>/<backend uri>`, and `resources/read` strips the
  wrapper and forwards the backend's own URI. Lists fan out live to the
  same connectors as `tools/list` (not cached; each backend's
  `nextCursor` is followed and one merged page returned). A connector's
  prompts and resources are in scope when the caller's profile grants at
  least one of its tools (`profile.AllowList.AllowsConnector`); a denied
  `prompts/get`/`resources/read` is `-32003`. Both are access-logged with
  `tool_name` set to the namespaced prompt name / URI. New:
  `mcp.MethodResourcesTemplatesList` and the 2025-06-18 prompt/resource
  types in `pkg/mcp`; `ListPrompts`, `GetPrompt`, `ListResources`,
  `ListResourceTemplates`, `ReadResource` on the connector client.
  `examples/01-quickstart` now round-trips a prompt and a resource. See
  [architecture.md](docs/architecture.md#served-mcp-methods).
- MCP request cancellation and progress for `tools/call`.
  `notifications/cancelled` on a session stops the named call in flight
  (answered `204`): its upstream exchange is aborted without a retry,
  `notifications/cancelled` is forwarded to the connector under the
  call's upstream id, the connector stays healthy, and the call answers
  JSON-RPC `-32800` "request cancelled". A `tools/call`'s
  `params._meta` (and so its `progressToken`) now reaches the connector,
  and the connector's `notifications/progress` for that token are
  relayed to the session's `GET /mcp/stream`, which now accepts an
  `Mcp-Session-Id` header (unknown id → `-32000`). Cancellation is per
  replica: with the `redis` session driver it must reach the replica
  running the call (see `docs/architecture.md`).
  `pkg/mcp` gains `NotificationCancelled`, `NotificationProgress`,
  `CancelledParams`, `ProgressParams`, `RequestMeta`,
  `ErrorCodeRequestCancelled` and `ToolsCallParams.Meta`.

- `deploy/k8s/overlays/eks`: base + `components/redis` on AWS EKS behind
  an internal ALB (AWS Load Balancer Controller) — one `alb` Ingress for
  the three planes, per-plane target-group health checks, a 900s drain
  on `gateway-llm`, a NetworkPolicy rule admitting the VPC CIDR
  (`target-type: ip`), a `preStop` sleep, and a node-pool toleration.
  The Namespace and `gateway-secrets` are expected to exist (created by
  infrastructure-as-code); every environment-specific value is a
  `${GW_*}` placeholder filled in at deploy time, so nothing
  account-specific is committed. `deploy/scripts/deploy_eks.sh` renders
  it, server-side dry-runs it, runs `gateway-migrate`, rolls the planes
  out (rolling all three back if one fails), and probes each host; run
  it by hand or from your own CD.
- CI: `.github/workflows/publish-image.yml` publishes
  `ghcr.io/tuskira/tusk-ai-secured-gateway:sha-<sha7>` (one immutable tag
  per commit) and `:main` (moving) from every push to `main`, on
  GitHub-hosted runners with the repository's own token — no cloud
  credentials or self-hosted runner. Tagged releases keep publishing
  `:<version>` and `:latest` via `release.yml`. This repository does
  not deploy anywhere; deployments run from a private fork that pulls
  the `sha-<sha7>` image for the commit it deploys.
- `pkg/session`: a public seam for MCP session persistence — a plain
  `Record`, a `Store` interface (`Get`/`Save`/`Delete`/`Close`, with an
  optional `Sweeper`), a `Notifier` interface for relaying
  `tools/list_changed` between replicas, and a driver registry
  (`Register`/`Open`, mirroring `pkg/store`). `pkg/session/sessiontest`
  is the conformance suite a driver must pass (`Run` for stores,
  `RunNotifier` for notifiers). `internal/dataplane/session.Manager` now
  persists through it, so a session backend can live in a separate Go
  module. Two drivers ship and are blank-imported by `cmd/gateway`:
  `pkg/session/memory` (default, unchanged behaviour) and
  `pkg/session/redis` (`github.com/redis/go-redis/v9`; keys
  `gw:sess:<id>` with the session's TTL, pub/sub channel
  `gw:tools_changed`).
- Config: `sessions.store: memory|redis|<registered driver>`
  (`GATEWAY_SESSIONS_STORE`, default `memory`) and `sessions.options`
  (YAML only, passed to third-party drivers). The `redis` section is now
  read by the `redis` store; `sessions.store: redis` requires
  `redis.enabled: true` and `redis.addr`, and `redis.cluster: true` is
  rejected ("not supported yet"). Startup logs `sessions store=<name>`.
- Deploy: `deploy/docker-compose.yml` gains a `redis` service under
  `--profile redis` (pinned `redis:7.4-alpine`, no host port) and forwards
  `GATEWAY_SESSIONS_STORE`, `GATEWAY_REDIS_ENABLED`, `GATEWAY_REDIS_ADDR`,
  `GATEWAY_REDIS_PASSWORD` (defaults keep the in-process store).
  `deploy/k8s/components/redis` (a kustomize Component, so it drops into
  any overlay) adds a dev-grade Redis, points `gateway-mcp` at it, scales
  it to 2 replicas and appends the NetworkPolicy egress rule for `:6379`;
  `overlays/redis` (base + component) and `overlays/kind-redis` (kind
  overlay + component) are the ready-made builds.
- CI `integration` job: a `redis:7-alpine` service and
  `GATEWAY_TEST_REDIS_ADDR`; the redis driver's conformance tests, the
  bridge test and the new two-replica e2e
  (`TestMCPRedisSessionsAcrossReplicas`) run against it and skip when the
  variable is unset. No fake Redis is used anywhere.
- Docs: `docs/architecture.md` lists the session seam and moves Redis
  from "not in" to "in"; `deploy/README.md` "Scaling per plane" covers
  scaling `mcp` with the Redis store; `CONTRIBUTING.md` gains "Adding a
  session backend"; `docs/configuration.md` documents `sessions.store`
  and the `redis` section.


- Body offload for captured LLM calls: `llm_proxy.capture.body_store`
  (`type: none | filesystem | s3`, plus `inline_max_bytes`) moves
  request/response bodies out of the capture row into a directory or an
  S3-compatible bucket (AWS S3, R2, MinIO), keyed by request id; the row
  keeps a `body_ref` (new `body_ref` column in ClickHouse `llm_calls`,
  added in place on startup; Postgres already had one). A failed offload
  falls back to inline bodies, never dropping the record. The LLM-log
  detail route resolves refs transparently, and `GET /api/v1/health`
  reports `sinks.body_store.{type,offloaded,fallbacks}`. The
  `pkg/sink.BodyStore` seam is now `Put(key, req, resp)` / `Get(ref)` with
  `sink.ErrBodyNotFound`, implemented by `pkg/sink/bodystore/fs` and
  `pkg/sink/bodystore/s3`.
- Console: a Session Timeline page (`/session-timeline`) showing one
  gateway session's MCP and LLM events, merged and time-ordered, from the
  new `GET /api/v1/analytics/sessions/{session_id}/timeline`
  (`analytics.read`) — see the Security entry below for the ownership rule
  this computes server-side. Stat tiles: Total events, Total duration,
  Errors, and Session ID (mono, truncated, full id in a tooltip, copy
  button). Start Date / End Date filters narrow the rendered list to a
  local calendar-day range, inclusive. Both log tables gain a Key column,
  and both detail drawers show the calling key as `name (prefix)`.
- LLM plane: each call records the caller's `client_ip` (the first
  `X-Forwarded-For` entry, else the socket address). This is a new
  `client_ip` column on ClickHouse `llm_calls`, added in place on startup.
  The Postgres ledger is unchanged.
- Console model form: the Vendor dropdown offers Anthropic, Amazon Bedrock,
  **Google Gemini**, **OpenAI**, **Nebius**, **Together AI** and **Others**.
  Picking one fills its base URL, editable afterwards:
  `https://api.anthropic.com`, Google's OpenAI-compatible endpoint
  `https://generativelanguage.googleapis.com/v1beta/openai/`,
  `https://api.openai.com/v1`,
  `https://api.tokenfactory.eu-west2.nebius.com/v1/`,
  `https://api.together.ai/v1`; Others (any other OpenAI-compatible
  endpoint) starts empty. Google Gemini, OpenAI, Nebius and Together AI are
  `openai_compat` targets labelled `gemini` / `openai` / `nebius` /
  `together`, so Anthropic clients (Claude Code, the Anthropic SDK) are
  translated to them; a row already on the native Gemini API still shows
  as "Google Gemini (native API)". The Credential dropdown gains **Accept
  API key**: the key typed there is stored as an encrypted credential
  (`POST /credentials`, field `api_key`, named `<model>-<label>`) before the
  model is saved, and the target references it by name; not offered for
  Bedrock. No API change.
- Example 04-python-agent: `agent.py --chat` keeps one conversation and
  switches model at runtime, like Claude Code's `/model` (`/model NAME`,
  `/model`, `/clear`, `/exit`), e.g. from Claude to the open-weight models
  Kimi K3 and GLM 5.3, registered as `kimi-k3` / `glm-5.3` on Nebius (or
  Together AI) with a stored key, so no `ANTHROPIC_API_KEY` is needed for
  them;
  `run.sh` registers both and switches between them in one conversation
  when `NEBIUS_API_KEY` or `TOGETHER_API_KEY` is set.

### Changed

- Admin console: connectors are now called "MCPs" everywhere a person sees
  the word — the sidebar item, the page title and its "Total MCPs" stat,
  the "Add MCP" button and dialog, the detail dialog, the "MCP" column on
  Access Logs and Tool Search, the "MCP ID" log filter, "Top MCPs" on the
  Overview, and the related empty states, confirmations, toasts and help
  text. Only labels changed: the `/connectors` console route, the
  `/api/v1/connectors` API, the `connector_id` fields and every identifier
  in code keep their names.
- Admin console: every list page (API keys, connectors, credentials, models,
  profiles, skills, tool search, access logs, LLM logs) now renders through
  one data table, `web/src/components/app/data-table`, replacing the two
  earlier table shells. Columns sort, resize and can be hidden, and those
  choices persist per table in `localStorage` under `gateway.table.<name>`.
  Row actions moved from inline icon buttons to a row menu, which also opens
  on right-click. The page no longer scrolls: rows scroll inside the table
  under a fixed header, and a table too wide for the window scrolls sideways
  with its row menu pinned in view. The log pages keep server-side paging
  and are not sortable, as their endpoints take only filters, `limit` and
  `offset`; a `limit` or `offset` in the URL that is not valid now falls
  back to a valid page. Adds `@tanstack/react-table` and `@dnd-kit/*`.
- `initialize` no longer advertises `listChanged` on `prompts` or
  `resources` (the gateway emits neither notification); both are
  advertised as `{}`, and only when a reachable backend has them.
- `session.Manager.Create` records the protocol version negotiated with
  the client on the session (`Record.ProtocolVersion`).
- The in-process session store now hands out copies on every read, so a
  mutation that is not saved is lost under the default driver exactly as
  it would be under Redis; every mutation site in the orchestrator saves.
- MCP access log: a caller's `X-Session-Id` / `X-Claude-Code-Session-Id` is
  now recorded as the new `client_session_id` field, alongside (never
  instead of) `session_id`, which is always the gateway-negotiated
  `Mcp-Session-Id`. See the Security entry below.
- Header capture: both planes allowlist more non-credential headers,
  including the `x-ratelimit-*` / `anthropic-ratelimit-*` families. Other
  headers are still recorded by name with a masked value, and the console
  no longer lists them.
- Console: log detail bodies open in a dialog as pretty-printed JSON, and
  captured `messages`/`system`/`tools` are decoded from base64 as UTF-8.
- `deploy/docker-compose.yml` sets `GATEWAY_CAPTURE_STORE_BODIES=true`.
  The code default stays `false`.
- Console: log detail bodies open in a viewer with syntax highlighting,
  folding, line numbers and find, which renders only the visible lines so a
  multi-MB body scrolls. It shows the text as captured, so large integers
  and duplicate keys stay exact, and a `truncated` body is indented rather
  than left on one line. In the LLM Logs drawer, `messages`/`system`/`tools`
  move from inline blocks to buttons that open the same viewer. The viewer
  is a separate chunk (CodeMirror 6, about 100 kB gzip) loaded when a body
  is first opened.
- Console: right-side panels (both log detail drawers, the skill detail
  sheet, the session timeline event sheet) are 650px wide, up from 384px.
- Console: the theme is rebuilt on the shadcn neutral palette with the
  product blue as `--primary`. The design-system surface tokens
  (`--bg-*`, `--text-*`, `--border-strong`) are now defined in terms of the
  theme variables, so they follow light and dark together, and the UI
  primitives move from Radix to Base UI.
- Console: the theme switch is a three-way System / Light / Dark control
  (the sign-in page has it too). "System" follows the operating system and
  is the default when nothing is saved; a saved Light or Dark choice is
  kept. Where the browser supports view transitions the new theme wipes in
  from the control. Toast notifications now change theme with the page.
- Console: the Overview dashboard is restyled. Every widget sits in a
  shared card of fixed height per row, so titles and contents line up and
  long lists scroll inside their card; KPI tiles have three fixed rows and
  an icon. Numbers count up, titles and bars animate in, the charts draw
  in, and skeletons stand in while metrics load. Bars in a list cycle
  through the five chart colours, and the donuts size to their card. All
  motion is off under `prefers-reduced-motion`.
- Console: every Overview KPI tile and widget has an info hint: a short
  explanation in a popover, and for widgets a "View full details" side
  panel. The text states what each figure counts, including why "MCP Tool
  Calls" (`tools/call` only) is lower than Traffic Distribution's MCP
  figure (every MCP request), and why the success rate (which leaves out
  204 notifications) is higher than the "200" slice.
- Console: the sign-in page is a single centred card with the logo and
  wordmark inside it, and errors show in a dismissible banner above the
  field. The sidebar's logo and product name are larger, and the account
  avatar uses the primary colour.

### Removed

- Console: the top bar's notification bell. It had no behaviour and
  always showed an unread dot.

### Security

- Session Timeline poisoning: the MCP access log's `session_id` could
  previously be overridden by a caller-supplied `X-Session-Id` /
  `X-Claude-Code-Session-Id` header, so any API key could write its calls
  into another session's timeline just by sending that session's id as a
  header. `session_id` is now always the gateway-negotiated
  `Mcp-Session-Id`; the header is captured separately as the new
  `client_session_id` (max 128 chars, sanitized), never used for
  ownership. The new `GET /api/v1/analytics/sessions/{session_id}/timeline`
  computes a session's timeline server-side under a strict rule: an
  LLM-plane event belongs to it only when its `session_id` matches (or
  matches a `client_session_id` seen on the session's own MCP events) AND
  its `key_id` equals the session's owning key — a foreign key's events
  are withheld, not relabeled, and counted as `excluded_foreign_events`.
  See [observability.md](docs/observability.md#session-timeline-ownership).
- Control plane: every response (JSON API, docs page, embedded admin
  console) now carries `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`,
  `Permissions-Policy: camera=(), microphone=(), geolocation=()`, and a
  `Content-Security-Policy` (`internal/api/security_headers.go`) — the
  console keeps the admin API key in `sessionStorage`, and nothing on this
  origin previously constrained which scripts could run against it.
- `GET /api/v1/docs`: the Scalar API reference viewer is now loaded from a
  pinned, version-locked jsDelivr URL with a Subresource Integrity hash
  (`internal/api/handlers/docs.go`'s `scalarVersion`/`scalarIntegrity`),
  instead of the unversioned `.../npm/@scalar/api-reference` URL, which
  served whatever the latest release was, unverified, on every request.
  The docs page sets its own, wider `Content-Security-Policy` (allowing
  jsDelivr) rather than the console's default. See
  [security-model.md](docs/security-model.md#browser-security-headers).
- Admin console: the inline dark-mode-flash-prevention `<script>` in
  `web/index.html` is now served same-origin from
  `web/public/theme-init.js`, so the console's `Content-Security-Policy`
  can use a plain `script-src 'self'` with no inline-script hash to keep
  in sync.
- Console sign-in accepts only keys with the `admin` role. This is a UI
  check: API permissions are unchanged, and an `agent` key's `*.read`
  grant still reads the API. See
  [security-model.md](docs/security-model.md).
- Dependency patches flagged by Dependabot: `google.golang.org/grpc`
  v1.83.1 to v1.83.2 (indirect, OTLP gRPC trace exporter; the gateway only
  uses the gRPC client), and the docs site's build/dev-server dependencies
  `serialize-javascript` (to 7.x) and `uuid` (to 11.x) via npm `overrides`.
  None of these ship in the gateway binary or image.

### Fixed

- LLM plane: Kimi and GLM models get their earlier reasoning back on later
  turns on any host. `WantsReasoningBack` matched only the vendors' own
  hosts or the labels `kimi` / `zhipu`, so the same models served by Nebius
  or Together lost it on tool-use turns; it now also matches the model id
  (`kimi-*`, `glm-*` after the last `/`). DeepSeek stays host/label-only,
  and Groq (label or host) never gets the field, whatever model it serves.
- MCP: a `tools/call` stopped by `notifications/cancelled` now always
  answers `-32800` "request cancelled", and never marks the connector
  unhealthy. `handleCancelled` used to forward the cancellation to the
  connector (`forwardCancel`, on its own goroutine) before recording it
  on the call's own context; a connector that closed its streamed reply
  the instant it heard about the cancellation could race that recording,
  so `CallTool` came back with a plain stream-closed error the gateway
  didn't recognize as a cancellation, and reported it as a generic
  `tools/call` failure instead (see `clientCancelled`,
  `internal/dataplane/orchestrator/inflight.go`). The call's own context
  is now cancelled first, which the Go memory model guarantees happens
  before the forwarding goroutine can have any observable effect.
- `DELETE /api/v1/connectors/{id}` now invalidates the connector's
  `tool_cache` rows as part of the delete, and every tool-cache read
  (`tools/list`'s cache, `GET .../connectors/{id}/tools`, `cache/search`)
  excludes rows belonging to a soft-deleted connector. Previously a
  deleted connector's tools could keep appearing in `tools/list`, and
  `tools/call` against them could fail oddly, for up to the cache's TTL.
- MCP access log: with `capture.store_bodies` on, the response body is
  now captured, bounded by `capture.max_response_bytes` and flagged
  `truncated`. Previously only the request body was stored.
- `make examples-smoke`: fixed five issues that only surfaced on a
  second run against the same persistent stack (a fresh stack always
  passed).
  - `examples/05-profiles`'s "Full Access" profile was built only from
    the "everything" connector's tools, but the check it feeds compares
    against a no-header `tools/list`, which returns every tool the
    TENANT owns across every connector (`mcp.require_profile` is off by
    default). That only happened to match on a fresh stack where
    "everything" is the tenant's only connector -- once any other
    example has registered a connector of its own that (by design)
    outlives its own run, e.g. `06-credentials-and-headers`'s
    "header-echo", the comparison fails. "Full Access" is now built from
    every connector currently in the tenant, which is what the check was
    always meant to compare against.
  - `examples/11-server-requests` registered an `everything-requests`
    connector on the shared "everything" MCP server and never removed
    it, compounding the same failure mode. The connector is now deleted
    in `run.sh`'s own cleanup trap, on both the happy path and any
    failure. Its slug also carries this run's pid
    (`everything-requests-<pid>`) rather than being fixed, since
    `connectors.slug`'s `UNIQUE(tenant_id, slug)` constraint is not
    scoped to `deleted_at IS NULL` -- re-creating a connector with a slug
    that was ever soft-deleted before would otherwise fail every run
    after the first with a 409.
  - `examples/06-credentials-and-headers/.header-echo.pid` and
    `.header-echo.log` were tracked in git despite being rewritten by
    every run, leaving the checkout dirty afterwards. Untracked and
    gitignored, matching the existing `.everything.pid`/`.everything.log`
    pattern.
  - `examples/09-roles-keys-and-rate-limits`'s cleanup recreated the
    gateway container from the base compose file alone, silently
    dropping the ClickHouse/OTel analytics profile `examples/08-observability`
    had started the gateway with earlier in the same `examples-smoke`
    run. Cleanup now restores through that same analytics-profile compose
    invocation when a running `clickhouse` container shows it's needed.
  - `examples/09-roles-keys-and-rate-limits`'s revoke and rotate steps
    re-looked-up "the" `09-agent`/`09-reader` key by name and took the
    first match from `GET /api-keys`, instead of using the id each
    mint's own response already returned. That's ambiguous the moment a
    same-named row from an earlier run coexists (revoke and rotate both
    leave one behind on purpose -- see the README's Cleanup section), and
    a real e2e run caught it picking an earlier run's already-revoked
    `09-agent` row: the DELETE "succeeded" against that stale row, but
    the run's own key was never actually revoked, so the next assertion
    (a 401 with the revoked key) failed. Both steps now use the id
    captured at mint time.
- `make examples-smoke`: the same failure mode fixed above for
  `examples/09-roles-keys-and-rate-limits`'s cleanup was still open in
  `examples/05-profiles`'s strict-mode toggle (`GATEWAY_MCP_REQUIRE_PROFILE=
  true`, and its restore back to `false`) and in `examples/12-skills-
  and-commands`'s compose-up -- both recreated the gateway container
  from the base compose file alone, silently dropping the ClickHouse/OTel
  analytics profile `examples/08-observability` had started the gateway
  with earlier in the same run. Auditing every example's own compose-up
  call found the identical gap in the plain, once-per-run "compose up"
  that `01-quickstart`, `04-python-agent`, `06-credentials-and-headers`,
  `07-llm-passthrough` and `11-server-requests` each use to bring the
  stack up -- every one of these examples is documented as runnable
  standalone and in any order, so any of them run after
  `08-observability` (in `examples-smoke`'s own order, this is
  `11-server-requests`) hit the same silent drop. All seven now route
  through the same guard as `09`'s fix: a running `clickhouse` container
  (only ever started via `--profile analytics`) means bring the gateway
  up through that analytics-profile compose invocation instead of the
  base file alone.

## [0.2.0] - 2026-09-26

### Added

- `examples/`: ten runnable, self-contained examples, each with a README
  (Goal · Prerequisites · Steps · Expected output · Cleanup) and a
  `run.sh` that asserts every step — 01 quickstart, 02 Claude Code,
  03 Cursor/VS Code/Codex configs, 04 Python agent on both planes (one
  session id across Access Logs and LLM Logs), 05 profiles and strict
  mode, 06 credentials and header injection proven at the backend,
  07 LLM passthrough against the real providers (bogus-key tier needs no
  keys), 08 ClickHouse + OTel collector + Jaeger with one `traceparent`
  followed end to end, 09 custom roles, revoke/rotate and lockout,
  10 the shipped kustomize manifests on `kind` with per-plane scaling.
  `make examples-smoke` runs 01–09 on every PR (CI job `examples`);
  `make examples-k8s` runs 10 (CI job `examples-k8s`, on pushes to
  `main` and PRs labelled `k8s`). `deploy/docker-compose.yml` now sets
  `extra_hosts: host.docker.internal:host-gateway` and forwards
  `GATEWAY_MCP_REQUIRE_PROFILE`.
- Docs: `deploy/README.md` gains "In-cluster MCP servers and the
  NetworkPolicy" — `deploy/k8s/base/networkpolicy.yaml` allows gateway-pod
  egress to DNS, Postgres, ClickHouse and 443 only, kind's kindnet enforces
  NetworkPolicy, and an in-cluster MCP server on any other port needs an
  appended egress rule (a JSON patch on `/spec/egress/-`, since a strategic
  merge replaces the list —
  `examples/10-kubernetes/overlay/networkpolicy-everything-patch.yaml` is
  the pattern).

### Security

- LLM plane: the Bedrock region (from `X-Bedrock-Region`, config, or the
  Flow A credential scope) is validated as an AWS region name; a malformed
  value is rejected with `400` instead of becoming part of the upstream host.
- LLM plane: Bedrock role assumption (`X-Bedrock-Role-Arn`) is **off by
  default** and limited to the AWS accounts in
  `llm_proxy.bedrock.allowed_role_accounts` (malformed ARNs are rejected), and
  always sends the caller's tenant ID as the STS `ExternalId` and
  `gw-<tenant>` as the session name. An allowlist entry is partition-scoped: a
  bare `123456789012` allows that account in the `aws` partition only, so a
  GovCloud or China role must be listed as `aws-us-gov:123456789012` /
  `aws-cn:123456789012` (`*` still matches every account in every partition).
  **Breaking** for Flow B role users: set the allowlist, and a role's trust
  policy must require `sts:ExternalId = <tenant id>`; a non-matching
  `X-Bedrock-External-Id` is rejected.
- LLM plane: `llmplane.Handler` now requires `Config.Authorizer` and returns an
  error without one; a nil authorizer previously skipped the `llm.access`
  check entirely, so the plane could be built to serve every authenticated key
  regardless of role.
- LLM plane: requests require the `llm.access` permission. **Breaking** for
  custom roles (`auth.roles`) without `llm.access` or `llm.*`, which now get
  `403`; the built-in `admin` and `agent` roles hold it.
- LLM plane: upstream redirects are relayed to the client, never followed
  (following one re-sent the provider key and body to the redirect target).
- LLM plane: transport errors are recorded without the URL query string
  (Gemini `?key=` credentials were persisted in the error column).
- Capture: text columns are sanitized (invalid UTF-8, NUL) before the
  Postgres insert, and LLM-call OTel span attributes likewise, so a crafted
  header can no longer drop a call's record or an OTLP export batch.

### Fixed

- Console: creating an API key with an expiry date no longer fails with
  400 -- the date picker now sends the end of the chosen day as an RFC
  3339 timestamp. The API also accepts a bare `YYYY-MM-DD` (end of that
  day, UTC) and rejects expiries in the past.

- Control plane: connector health probe, discovery and tool-cache management
  routes now work on an instance with `mcp.enabled: false` (per-plane
  Kubernetes deployments); the data plane is built for ops without opening
  the MCP listener or running its refresh loops.
- Cost: 1-hour cache writes (2x input) and web-search requests ($0.01 each)
  were dropped before pricing.
- Cost: Bedrock geographic inference profiles bill Claude 4.5+ at 1.1x
  (GovCloud 1.2x) over the global rate.
- Cost: free token-count endpoints (Bedrock `count-tokens`, Gemini
  `:countTokens`, OpenAI `responses/input_tokens`) and Anthropic Message
  Batches calls are no longer priced; a successful call with no readable
  usage stores `NULL` cost instead of `$0`.
- Cost: provider pricing modifiers — OpenAI `service_tier` / Gemini
  `serviceTier` priority and flex rates, Anthropic fast mode
  (`fast_multiplier`, 2x on Opus 5.5) and US-only inference
  (`inference_geo: "us"`, `us_geo_multiplier` 1.1x on the models that carry it:
  the Claude 5 family — Fable 5.1, Opus 5.5, Sonnet 5), Bedrock GovCloud (1.2x)
  and region-specific Nova rates. `inference_geo` is now also read from the
  request body when the response omits it, as the other two modifiers already
  were.
- Cost: `gemini-3.8-flash`'s `flex` rate no longer halves `cache_read`; every
  flex row halves input/output only, and a test pins the convention.
- Cost: the free-endpoint check matches `/messages/batches` on whole path
  segments, so a billable path merely prefixed by it (`…/batches-export`) is
  no longer stored at $0.
- Tokens: Bedrock Converse `cacheDetails` 1-hour writes take the max across
  repeated stream-metadata events instead of summing them (a buffer holding the
  event twice doubled the 1h cache count).
- Tokens: Bedrock models whose native body has no counts (e.g. Mistral) use
  Bedrock's `X-Amzn-Bedrock-*-Token-Count` headers / invocation metrics;
  Bedrock Converse 1-hour cache writes (`cacheDetails`) and Nova's
  `cache*InputTokenCount` fields are counted.
- ClickHouse: `cost_usd` is `Nullable(Decimal(38,12))` (unknown cost was
  stored as 0) and rounded, not truncated, to 12 places.
- Relay errors are labelled `stream_deadline` (504 before any response),
  `client_closed`, or `upstream_error: …` instead of always `client_closed`;
  client-side request errors (bad region, missing credential header) return
  `400`, not `502`; a malformed `llm_proxy.bedrock.region` fails startup.
- OpenAI stream stop reason, OpenAI `x-request-id`, and the Gemini
  `:streamGenerateContent` stream flag are captured.

### Changed

- OpenAI Chat Completions streams without `stream_options` can be priced: the
  gateway requests usage upstream and strips the extra chunk from the client's
  stream. **Behaviour change / opt-in:**
  `llm_proxy.providers.openai.inject_stream_usage` defaults to `false`, because
  when enabled it modifies the upstream request body — the plane's only
  departure from byte-for-byte passthrough — and an OpenAI-compatible
  `base_url` that rejects unknown fields would start answering `400`. Left off,
  such streams record a `NULL` cost as before. The chunk is also left in place
  when the upstream response declares a `Content-Length`.
- Bedrock assumed-role credentials are cached (one STS call per credential
  lifetime instead of per request).
- The response body is buffered for storage only when `store_bodies` is on.
- The single-call capture insert is one autocommit statement; the analytics
  tee now runs even when the durable write fails.

## [0.1.0] - 2026-09-25

First release: phase 1 of the open-source AI gateway.

### Added

- **MCP plane** (`:8080`): Streamable HTTP `POST /mcp` (JSON-RPC 2.0) and
  `GET /mcp/stream` (SSE) in front of any number of remote MCP servers.
  Tools are namespaced by connector slug (`<slug>__<tool>`), health is
  tracked per connector with half-open recovery, tool lists are cached in
  Postgres (stale-serve + background refresh), and `Mcp-Session-Id`
  sessions follow the MCP spec (2025-06-18, 2024-11-05 fallback).
- **Agent profiles**: per-tenant allow-lists of connectors/tools, selected
  with `X-Agent-Profile-Name` and enforced on every `tools/call`, plus
  `tool_arg_overrides` (flat or per-tool) that pin arguments server-side.
- **LLM plane** (`:8082`): byte-identical passthrough for Anthropic
  (`/v1/messages`), OpenAI (`/openai/v1/...`), Gemini (`/gemini/...`) and
  AWS Bedrock (`/model/{id}/invoke*`) with bring-your-own-key, three
  Bedrock credential modes (`passthrough`, `client_keys`, `gateway`),
  streaming, token capture on truncated streams, and an estimated cost
  per call from a bundled price card (`llm_proxy.pricing_file` override).
- **Control plane** (`:8081`): REST API under `/api/v1` (tenants, API
  keys, credentials, connectors, profiles, header providers, cache,
  analytics, health) with generated OpenAPI, and an embedded React admin
  console (Overview, Connectors, Profiles + Profile Studio, Tool Search,
  Cache, API Keys, Credentials, Access Logs, LLM Logs).
- **Auth**: gateway-issued `gk_` API keys (SHA-256 at rest, 60s cache
  with immediate revoke/rotate), built-in `admin`/`agent` roles plus
  YAML custom roles, `platform.admin` guard on `/tenants`, `mcp.access`
  guard on the MCP plane, brute-force lockout and per-IP rate limiting
  on the control plane, and a dev-mode authenticator for local work.
- **Secrets**: AES-256-GCM credential store with a rotatable key ring
  (`GATEWAY_MASTER_KEY`, `gateway secrets genkey|rekey`); header
  injection types `static`, `token_field`, `incoming_field`, `external`
  (`secret_store` by default; `env`/`file` providers opt-in).
- **Observability**: pluggable sinks — stdout, OpenTelemetry (spans joined
  to the caller's `traceparent`), ClickHouse (`mcp_access_logs`,
  `llm_calls`, 90-day TTL) — a durable Postgres outbox for LLM calls,
  and an analytics API (`/api/v1/analytics/overview|logs|llm-logs`).
- **Plugin seams** in `pkg/` (`auth`, `sink`, `headers`, `store`,
  `ops`, `analytics`, `pricing`, `mcp`, `trace`) so private extensions
  and alternative store backends can be built outside this repo;
  `pkg/store/storetest` is the conformance suite for new backends.
- **Config**: YAML (`CONFIG_PATH`) with `GATEWAY_<SECTION>_<FIELD>` env
  overrides; each plane can be switched off independently
  (`mcp.enabled`, `api.enabled`, `llm_proxy.enabled`) to scale from one
  image.
- **Deployment**: multi-stage distroless `Dockerfile`, Docker Compose
  stack (Postgres, optional ClickHouse + Adminer under `--profile
  analytics`), and kustomize Kubernetes manifests with one Deployment
  per plane (`deploy/k8s`, `kind` and `analytics` overlays).
- **Docs**: architecture, configuration reference, security model,
  connectors & credentials, profiles, LLM plane, observability, API;
  `SECURITY.md`, `CONTRIBUTING.md`, `CODEOWNERS`; Apache 2.0 license.
- CI workflow (`.github/workflows/ci.yml`): `go` (gofmt, vet, build,
  `go test -race`, golangci-lint), `web` (typecheck, lint, test, build of
  the React admin console), `integration` (Postgres conformance suite,
  `-tags e2e` MCP end-to-end test, `-tags integration` ClickHouse sink
  test, against real Postgres 16 and ClickHouse 24 services), `security`
  (`govulncheck`, gitleaks), and `docker` (multi-stage image build plus a
  live `/api/v1/health` smoke test) jobs, running on every pull request
  and on push to `main`.
- Release workflow (`.github/workflows/release.yml`), triggered on
  `v*` tags: builds the React console, then uses GoReleaser
  (`.goreleaser.yaml`) to cross-compile `cmd/gateway` for
  linux/darwin × amd64/arm64, publish checksummed archives and a GitHub
  Release with generated notes, and build + push multi-arch
  (linux/amd64, linux/arm64) Docker images to
  `ghcr.io/tuskira/tusk-ai-secured-gateway` tagged `{version}` and
  `latest` (`Dockerfile.goreleaser`).
- `Makefile`: `lint`, `vuln` (`govulncheck`), `ci` (runs the CI "go" and
  "web" jobs' checks locally), and `snapshot` (`goreleaser build
  --snapshot`) targets.
- `.golangci.yml`: pinned to the golangci-lint v2 config schema
  (`version: "2"`), enabling `govet` and `staticcheck` repo-wide, with a
  narrow scoped exclusion for two pre-existing test-file findings.
  `errcheck`/`revive`/`goimports` are left disabled pending a follow-up
  cleanup pass over existing `internal/`/`pkg/` code (see the CI setup
  PR description for the full findings list).
- `.gitleaks.toml`: a real ruleset (extending gitleaks' defaults) plus a
  custom `gk_[A-Za-z0-9]{40,}` rule for the gateway's own API key
  format, with a narrowly-scoped allowlist for the documented,
  git-committed local dev defaults in `deploy/docker-compose.yml`,
  `configs/base/default.yml`, and the sample key in `README.md`'s
  `bootstrap-key` transcript.
- CI badge in `README.md`.

[Unreleased]: https://github.com/Tuskira/tusk-ai-secured-gateway/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/Tuskira/tusk-ai-secured-gateway/releases/tag/v0.3.0
[0.2.0]: https://github.com/Tuskira/tusk-ai-secured-gateway/releases/tag/v0.2.0
[0.1.0]: https://github.com/Tuskira/tusk-ai-secured-gateway/releases/tag/v0.1.0
