# Configuration

The gateway loads config in three layers, low to high precedence:
built-in defaults (`internal/config.Default()`) → an optional YAML file
(`--config` flag or `CONFIG_PATH` env var; skipped entirely if neither is
set) → environment variables of the form `GATEWAY_<SECTION>_<FIELD>`,
derived automatically from each field's YAML tag. `configs/base/default.yml`
documents the same fields with comments; it is a reference file, not
auto-loaded — point `CONFIG_PATH` at it (or a copy) to actually use it.

Every environment variable below is checked with `strconv`/`time.ParseDuration`;
an unparsable value fails startup rather than being silently ignored. Leaf
fields (string, int, bool, `time.Duration`) and `[]string` lists
(comma-separated) get an env var. A map or a list of objects has none (see
"YAML-only keys" below).

The YAML file is decoded strictly: an unknown key fails startup
(`config <file>: unknown key "mcp.require_profle" at line 3`), so a typo
cannot silently leave a setting at its default. An unknown `GATEWAY_*`
variable is only logged as a warning when the gateway starts serving,
because the environment may carry unrelated `GATEWAY_*` names.

Duration values use Go's duration syntax (`30s`, `5m`, `1h`).

## service

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `service.name` | string | `tusk-ai-secured-gateway` | `GATEWAY_SERVICE_NAME` | Reported as `service` on the MCP plane's `GET /health`. |
| `service.version` | string | `dev` | `GATEWAY_SERVICE_VERSION` | Ignored. The gateway reports its build version (`-ldflags -X main.version=...`; `dev` when built without it) instead. Accepted so existing config files still load. |

## mcp (MCP plane)

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `mcp.enabled` | bool | `true` | `GATEWAY_MCP_ENABLED` | Starts the MCP plane's listener. |
| `mcp.address` | string | `0.0.0.0:8080` | `GATEWAY_MCP_ADDRESS` | Listen address. |
| `mcp.read_timeout` | duration | `30s` | `GATEWAY_MCP_READ_TIMEOUT` | Bounds how long reading a request may take. |
| `mcp.write_timeout` | duration | `0` (unlimited) | `GATEWAY_MCP_WRITE_TIMEOUT` | Bounds how long writing a response may take. Left at `0` because `GET /mcp/stream` is a long-lived SSE channel that a non-zero write deadline would sever on a fixed schedule; bound the request side with `read_timeout` and the backend side with `connectors.default_timeout_ms` instead. |
| `mcp.require_profile` | bool | `false` | `GATEWAY_MCP_REQUIRE_PROFILE` | Rejects any MCP request with no `X-Agent-Profile-Name` header (JSON-RPC `-32003`). Default: a caller with no profile header gets every tool its tenant owns. |
| `mcp.max_upstream_streams_per_session` | int | `16` | `GATEWAY_MCP_MAX_UPSTREAM_STREAMS_PER_SESSION` | Most long-lived server-to-client streams the gateway holds open to connectors for one agent session (one per connector the session subscribed to a resource on, or was declared a server-request capability to). A `resources/subscribe` that would need another is `-32600`; a handshake that would need another skips the stream (the connector's requests can still arrive inside a call's reply). Must be at least 1. |
| `mcp.max_subscriptions_per_session` | int | `256` | `GATEWAY_MCP_MAX_SUBSCRIPTIONS_PER_SESSION` | Most `resources/subscribe` subscriptions one agent session may hold; past it a new subscribe is `-32600`. Must be at least 1. |
| `mcp.max_pending_server_requests_per_session` | int | `32` | `GATEWAY_MCP_MAX_PENDING_SERVER_REQUESTS_PER_SESSION` | Most server-initiated requests (`sampling/createMessage`, `elicitation/create`, `roots/list`) relayed to one agent session that may await its answer at once; past it the connector is answered `-32603` and the agent is not asked. Counted per replica. Must be at least 1. See [connectors-and-credentials.md](connectors-and-credentials.md#server-initiated-requests-sampling-elicitation-roots). |
| `mcp.server_request_timeout` | duration | `5m` | `GATEWAY_MCP_SERVER_REQUEST_TIMEOUT` | How long a relayed server-initiated request may await the agent's answer before the connector is answered `-32603` "timed out waiting for the client" and the agent is sent `notifications/cancelled`. Long by default because an elicitation waits on a human. The wait is not charged against the connector's `timeout_ms`. Must be positive. |

## api (control plane)

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `api.enabled` | bool | `true` | `GATEWAY_API_ENABLED` | Starts the API plane's listener. |
| `api.address` | string | `0.0.0.0:8081` | `GATEWAY_API_ADDRESS` | Listen address. |
| `api.serve_ui` | bool | `true` | `GATEWAY_API_SERVE_UI` | Mounts the embedded React console at `/`. When `false`, only `/api/v1/*` is served. |
| `api.trusted_proxies` | `[]string` | `nil` (empty) | `GATEWAY_API_TRUSTED_PROXIES` (comma-separated) | Proxies — single IPs or CIDRs — whose `X-Forwarded-For` is believed, for **every** plane's rate limiter and the logged `client_ip`. The client is the first address, walking the header from the right, that is not inside this set. Empty trusts no peer: `X-Forwarded-For` is ignored and clients are keyed on the TCP peer (behind a load balancer, that is one shared identity). A malformed entry fails startup. See [security-model.md](security-model.md#rate-limiting-and-lockout). |

## llm_proxy (LLM plane)

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `llm_proxy.enabled` | bool | `false` | `GATEWAY_LLM_PROXY_ENABLED` | Starts the LLM plane's listener. |
| `llm_proxy.address` | string | `0.0.0.0:8082` | `GATEWAY_LLM_PROXY_ADDRESS` | Listen address. |
| `llm_proxy.upstream_base_url` | string | `https://api.anthropic.com` | `GATEWAY_LLM_PROXY_UPSTREAM_BASE_URL` | Base URL the Anthropic provider forwards to. Must be an absolute URL (validated at startup). |
| `llm_proxy.pricing_file` | string | `""` (embedded card only) | `GATEWAY_LLM_PROXY_PRICING_FILE` | Path to a JSON file (shape of `pkg/pricing/prices.json`) that overrides the embedded per-model rate card; merged over it per-model at startup. A malformed file fails startup. |
| `llm_proxy.models` | list | `[]` | *(YAML only)* | Seed for the LLM plane's [model registry](llm-plane.md#model-registry): each `{name, description, targets[], price}` (a target is `{vendor, model, base_url, region, allow_caller_key, label}`) is upserted at startup as a platform row (visible to every tenant, overridable per tenant through `/api/v1/models`). Validated at startup like an API write (name shape, vendor, `openai_compat` needs `base_url`, `bedrock` needs `region`, `label` on `openai_compat` only and never `anthropic`/`bedrock`); a platform row may not name a `credential`, so a target on a host other than the vendor's configured default needs `allow_caller_key: true` to be usable. Not overridable via env vars (a list). |
| `llm_proxy.limits.max_request_bytes` | int64 | `10485760` (10 MiB) | `GATEWAY_LLM_PROXY_LIMITS_MAX_REQUEST_BYTES` | 413 threshold on the inbound request body. |
| `llm_proxy.limits.max_stream_duration` | duration | `15m` | `GATEWAY_LLM_PROXY_LIMITS_MAX_STREAM_DURATION` | Total request deadline, covering streaming responses. |
| `llm_proxy.limits.max_concurrent_per_tenant` | int | `64` | `GATEWAY_LLM_PROXY_LIMITS_MAX_CONCURRENT_PER_TENANT` | Per-tenant concurrent in-flight request cap (429 over the cap). |
| `llm_proxy.bedrock.enabled` | bool | `false` | `GATEWAY_LLM_PROXY_BEDROCK_ENABLED` | Mounts the `/model/*` (bare) and `/bedrock/*` (prefixed) Bedrock routes. |
| `llm_proxy.bedrock.allowed_role_accounts` | string | `""` (role mode off) | `GATEWAY_LLM_PROXY_BEDROCK_ALLOWED_ROLE_ACCOUNTS` | Comma-separated allowlist of the AWS accounts whose roles callers may name in `X-Bedrock-Role-Arn` (assumed with the gateway's identity, `ExternalId` = tenant ID). An entry is a bare `123456789012`, which matches that account in the **`aws` partition only**, a partition-qualified `aws-us-gov:123456789012` / `aws-cn:123456789012`, or `*` for every account in every partition. Empty rejects role mode with `400`. |
| `llm_proxy.bedrock.region` | string | `us-east-1` | `GATEWAY_LLM_PROXY_BEDROCK_REGION` | Default AWS region for the signed credential modes (client_keys/gateway); overridable per-request via `X-Bedrock-Region`. Must be a well-formed AWS region name (checked at startup); a malformed per-request value is rejected with `400`. |
| `llm_proxy.bedrock.credential_mode` | string | `auto` | `GATEWAY_LLM_PROXY_BEDROCK_CREDENTIAL_MODE` | `auto` \| `passthrough` \| `client_keys` \| `gateway` — see [llm-plane.md](llm-plane.md). |
| `llm_proxy.providers.openai.enabled` | bool | `false` | `GATEWAY_LLM_PROXY_PROVIDERS_OPENAI_ENABLED` | Mounts `/openai/*` (prefix-only; OpenAI has no bare route). |
| `llm_proxy.providers.openai.inject_stream_usage` | bool | `false` (opt-in) | `GATEWAY_LLM_PROXY_PROVIDERS_OPENAI_INJECT_STREAM_USAGE` | Adds `stream_options.include_usage` to Chat Completions streams that set no `stream_options`, and strips the resulting usage-only chunk from the client's stream, so streamed calls are priced. Off by default because it **modifies the upstream request body** — the plane's only departure from byte-for-byte passthrough — and OpenAI-compatible backends that reject unknown fields would `400`. Enable when the upstream accepts it; otherwise such streams record `NULL` cost. |
| `llm_proxy.providers.openai.base_url` | string | `https://api.openai.com` | `GATEWAY_LLM_PROXY_PROVIDERS_OPENAI_BASE_URL` | Upstream base URL. |
| `llm_proxy.providers.gemini.enabled` | bool | `false` | `GATEWAY_LLM_PROXY_PROVIDERS_GEMINI_ENABLED` | Mounts `/gemini/*` (prefix-only). |
| `llm_proxy.providers.gemini.base_url` | string | `https://generativelanguage.googleapis.com` | `GATEWAY_LLM_PROXY_PROVIDERS_GEMINI_BASE_URL` | Upstream base URL. |
| `llm_proxy.capture.store` | string | `postgres` | `GATEWAY_LLM_PROXY_CAPTURE_STORE` | Durable capture destination. `"postgres"` is lossless (synchronous, idempotent insert); anything else falls back to best-effort stdout. |
| `llm_proxy.capture.store_bodies` | bool | `false` | `GATEWAY_LLM_PROXY_CAPTURE_STORE_BODIES` | Persist request/response bodies and parsed messages/system/tools alongside the durable record. |
| `llm_proxy.capture.max_request_bytes` | int | `1048576` (1 MiB; `0` = unbounded) | `GATEWAY_LLM_PROXY_CAPTURE_MAX_REQUEST_BYTES` | Per-body storage cap when `store_bodies` is on; also bounds the parsed `messages` / `system` / `tools`. A cut body is flagged `truncated`. |
| `llm_proxy.capture.max_response_bytes` | int | `1048576` (1 MiB; `0` = unbounded) | `GATEWAY_LLM_PROXY_CAPTURE_MAX_RESPONSE_BYTES` | Bounded response-body storage cap. |
| `llm_proxy.capture.body_store.type` | string | `none` | `GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_TYPE` | Where captured bodies live: `none` keeps them inline in the capture row; `filesystem` or `s3` offloads them and stores a `body_ref` on the row instead (see [llm-plane.md](llm-plane.md#body-offload)). Only acts when `store_bodies` is on. |
| `llm_proxy.capture.body_store.inline_max_bytes` | int | `0` | `GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_INLINE_MAX_BYTES` | With a store configured, calls whose request + response bodies total at most this many bytes stay inline. `0` offloads every call that has a body. |
| `llm_proxy.capture.body_store.filesystem.root` | string | `""` | `GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_FILESYSTEM_ROOT` | Directory for `filesystem` (required for that type). Created if missing. Every instance that serves the API plane needs the same directory mounted to resolve refs. |
| `llm_proxy.capture.body_store.s3.bucket` | string | `""` | `GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_S3_BUCKET` | Bucket for `s3` (required for that type). Must already exist. |
| `llm_proxy.capture.body_store.s3.prefix` | string | `llm-bodies` | `GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_S3_PREFIX` | Object key prefix: bodies land at `<prefix>/<request_id>/request` and `.../response`. |
| `llm_proxy.capture.body_store.s3.region` | string | `""` | `GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_S3_REGION` | AWS region. Empty uses the default AWS chain's region (`AWS_REGION`, profile); with a custom `endpoint` and no region anywhere, `us-east-1`. |
| `llm_proxy.capture.body_store.s3.endpoint` | string | `""` | `GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_S3_ENDPOINT` | Absolute URL of an S3-compatible store (MinIO, R2, ...). Empty = AWS S3. |
| `llm_proxy.capture.body_store.s3.force_path_style` | bool | `false` | `GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_S3_FORCE_PATH_STYLE` | Address buckets as `<endpoint>/<bucket>`; MinIO and most self-hosted stores need `true`. |
| `llm_proxy.capture.body_store.s3.access_key_id` | string | `""` | `GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_S3_ACCESS_KEY_ID` | Static credentials. Empty uses the default AWS credential chain (env, shared profile, IRSA / instance role). |
| `llm_proxy.capture.body_store.s3.secret_access_key` | string | `""` | `GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_S3_SECRET_ACCESS_KEY` | Secret for `access_key_id`. Never logged; prefer the env var over the YAML file. |
| `llm_proxy.detection.agent_url` | string | `""` (off) | `GATEWAY_LLM_PROXY_DETECTION_AGENT_URL` | Base URL of a local [detection agent](llm-plane.md#detection-agent) (e.g. `http://127.0.0.1:8090`). Set, each relayed generation or batch call is posted to `{agent_url}/v1/turns` asynchronously after it completes; nothing waits on the agent and nothing is blocked. Must be an absolute `http(s)` URL without userinfo (validated at startup). |
| `llm_proxy.detection.timeout` | duration | `5s` | `GATEWAY_LLM_PROXY_DETECTION_TIMEOUT` | Bound on one post of a turn to the agent. |
| `llm_proxy.detection.queue_size` | int | `1024` | `GATEWAY_LLM_PROXY_DETECTION_QUEUE_SIZE` | Turns waiting to be posted. Past it a turn is dropped and counted (`/health` `detection.dropped`). |
| `llm_proxy.detection.queue_bytes` | int | `268435456` (256 MiB) | `GATEWAY_LLM_PROXY_DETECTION_QUEUE_BYTES` | Bytes held by turns waiting (their request and response bodies) or being posted (the turn as posted: raw bodies and canonical conversation). Past it a turn is dropped and counted. |
| `llm_proxy.detection.max_in_flight` | int | `8` | `GATEWAY_LLM_PROXY_DETECTION_MAX_IN_FLIGHT` | Turns being posted at once. |

## database (required)

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `database.driver` | string | `postgres` | `GATEWAY_DATABASE_DRIVER` | Registered store backend (`pkg/store.Open`); `postgres` is the only one shipped. |
| `database.host` | string | `localhost` | `GATEWAY_DATABASE_HOST` | Postgres host. |
| `database.port` | int | `5432` | `GATEWAY_DATABASE_PORT` | Postgres port. |
| `database.user` | string | `gateway` | `GATEWAY_DATABASE_USER` | Postgres user. |
| `database.password` | string | `gateway` | `GATEWAY_DATABASE_PASSWORD` | Postgres password. The default is for the local compose stack only: the gateway logs a `WARN` at startup when it is still `gateway` and the host is not loopback / a bare service name or `ssl_mode` is not `disable`. Set a strong value for any real deployment. |
| `database.database` | string | `gateway` | `GATEWAY_DATABASE_DATABASE` | Database name. |
| `database.ssl_mode` | string | `require` | `GATEWAY_DATABASE_SSL_MODE` | `require` \| `disable` \| `verify-ca` \| `verify-full`. The local compose stack overrides this to `disable`; use `require` or stronger anywhere shared. |
| `database.migrate` | bool | `true` | `GATEWAY_DATABASE_MIGRATE` | Applies pending migrations at startup. Also runnable standalone via `gateway migrate`. |

## auth

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `auth.api_keys.enabled` | bool | `true` | `GATEWAY_AUTH_API_KEYS_ENABLED` | Adds the API-key `Authenticator` to the chain — the gateway's primary auth method. |
| `auth.api_keys.cache_ttl` | duration | `30s` | `GATEWAY_AUTH_API_KEYS_CACHE_TTL` | How long a successful key lookup is cached in memory. A revoked/rotated key is evicted from every replica at once through Postgres `LISTEN/NOTIFY`; this TTL is the backstop for a replica whose listener connection is down (see [security-model.md](security-model.md#known-limits)). |
| `auth.default_tenant` | string | `default` | `GATEWAY_AUTH_DEFAULT_TENANT` | Tenant the console login form is prefilled with, and the one a login that omits `tenant` uses. When exactly one tenant exists, that tenant is used instead. `gateway bootstrap-key` does not read it: its `-tenant` flag defaults to `default`. |
| `auth.console_api_key_login` | bool | `true` | `GATEWAY_AUTH_CONSOLE_API_KEY_LOGIN` | Offer "sign in with an API key" on the console login page. An emergency fallback, planned for removal; it only steers the console, API keys keep authenticating API calls. See [authentication.md](authentication.md). |
| `auth.cookie_secure` | string | `auto` | `GATEWAY_AUTH_COOKIE_SECURE` | `Secure` flag on the console session cookie: `auto` (TLS or `X-Forwarded-Proto: https`), `true`, or `false` (plain-HTTP local development). |
| `auth.session_idle` | duration | `8h` | `GATEWAY_AUTH_SESSION_IDLE` | A console session expires after this long without a request. Must be positive and at most `session_max`. |
| `auth.session_max` | duration | `24h` | `GATEWAY_AUTH_SESSION_MAX` | Absolute console session lifetime from login. |
| `auth.dev_mode.enabled` | bool | `false` | `GATEWAY_AUTH_DEV_MODE_ENABLED` | Adds the dev-mode gap-filling `Authenticator`. **Never enable outside local development** — it grants a tenant-admin `Principal` to any request with no credential at all. Logs a startup `WARN`, and refuses to start if an enabled plane listens on a non-loopback address (see `allow_remote`). |
| `auth.dev_mode.tenant` | string | `default` | `GATEWAY_AUTH_DEV_MODE_TENANT` | Tenant **slug** (or UUID) the manufactured dev-mode principal is scoped to. Required when `dev_mode.enabled` is true; must exist at startup (e.g. created by `gateway bootstrap-key`) or the gateway fails to start. |
| `auth.dev_mode.allow_remote` | bool | `false` | `GATEWAY_AUTH_DEV_MODE_ALLOW_REMOTE` | Permit dev mode while an enabled plane listens on a non-loopback address. Without it startup is refused. |
| `auth.dev_mode.platform` | bool | `false` | `GATEWAY_AUTH_DEV_MODE_PLATFORM` | Also grant the dev principal `platform-admin`. Default: tenant admin only. |
| `auth.roles` | `map[string][]string` | `{}` (only the built-in `admin`/`platform-admin`/`agent`/`viewer`/`interceptor` exist) | *(YAML-only)* | Custom role → permission-pattern grants, merged over the built-ins at startup. A name matching a built-in **replaces** it outright. See [security-model.md](security-model.md) for pattern syntax and `platform.admin`. |
| `auth.rate_limit.enabled` | bool | `true` | `GATEWAY_AUTH_RATE_LIMIT_ENABLED` | Turns on the auth-failure limiter (API, MCP and LLM planes; each plane keeps its own independent counters and lockouts, all using the thresholds below) and the control plane's general per-IP cap. |
| `auth.rate_limit.max_failures` | int | `10` | `GATEWAY_AUTH_RATE_LIMIT_MAX_FAILURES` | Auth failures from one IP on one plane within `window` that trip that plane's lockout. |
| `auth.rate_limit.window` | duration | `1m` | `GATEWAY_AUTH_RATE_LIMIT_WINDOW` | Period failures are counted over. |
| `auth.rate_limit.lockout` | duration | `5m` | `GATEWAY_AUTH_RATE_LIMIT_LOCKOUT` | How long a tripped IP is locked out of that plane (429 + `Retry-After` on every request to it, even a valid one; other planes are unaffected). |
| `auth.rate_limit.requests_per_minute` | int | `600` | `GATEWAY_AUTH_RATE_LIMIT_REQUESTS_PER_MINUTE` | General per-IP request cap on the control plane, independent of auth outcome. `0` disables it. |
| `auth.rate_limit.credential_max_failures` | int | `20` | `GATEWAY_AUTH_RATE_LIMIT_CREDENTIAL_MAX_FAILURES` | Failed lookups of unknown keys sharing one key prefix (`gk_` + 8 chars), from any IPs, within `credential_window` that lock the prefix for `lockout`. A locked prefix still serves cached (legitimate) keys. |
| `auth.rate_limit.credential_window` | duration | `5m` | `GATEWAY_AUTH_RATE_LIMIT_CREDENTIAL_WINDOW` | Period `credential_max_failures` is counted over. |

## secret_store

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `secret_store.master_key_env` | string | `GATEWAY_MASTER_KEY` | `GATEWAY_SECRET_STORE_MASTER_KEY_ENV` | Name of the env var read for the master key ring. |
| `secret_store.master_key_file` | string | `""` | `GATEWAY_SECRET_STORE_MASTER_KEY_FILE` | File path read for the key ring instead, only consulted when the env var named above is unset. |
| `secret_store.active_key_id` | string | `k1` | `GATEWAY_SECRET_STORE_ACTIVE_KEY_ID` | Which key in the ring new `Encrypt` calls use. |
| `secret_store.allow_env_provider` | bool | `false` | `GATEWAY_SECRET_STORE_ALLOW_ENV_PROVIDER` | Enables the `env` header provider. Off by default — see [security-model.md](security-model.md). |
| `secret_store.allow_file_provider` | bool | `false` | `GATEWAY_SECRET_STORE_ALLOW_FILE_PROVIDER` | Enables the `file` header provider. |
| `secret_store.file_provider_root` | string | `""` | `GATEWAY_SECRET_STORE_FILE_PROVIDER_ROOT` | Directory the `file` provider's paths must resolve under. Required when `allow_file_provider` is true. |

## redis

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `redis.enabled` | bool | `false` | `GATEWAY_REDIS_ENABLED` | Must be `true` when `sessions.store` is `redis`; validation fails otherwise. |
| `redis.addr` | string | `localhost:6379` | `GATEWAY_REDIS_ADDR` | `host:port` of a single Redis node (a managed primary endpoint is fine). Required with the `redis` store. |
| `redis.password` | string | `""` | `GATEWAY_REDIS_PASSWORD` | Set via env or a Secret, never YAML. |
| `redis.db` | int | `0` | `GATEWAY_REDIS_DB` | Logical database. |
| `redis.tls` | bool | `false` | `GATEWAY_REDIS_TLS` | Dial with TLS 1.2+ (managed Redis with in-transit encryption). |
| `redis.cluster` | bool | `false` | `GATEWAY_REDIS_CLUSTER` | **Not supported**: `true` fails validation. Point `addr` at a single node. |

Any server that speaks the Redis protocol works: the bundled Compose
profile and Kubernetes component run Valkey (BSD-3), and Redis or a
managed service (ElastiCache, Memorystore, ...) works the same way.

The one consumer of this section is `sessions.store: redis` (below). It
keeps MCP sessions under `gw:sess:<id>` (JSON, with a TTL equal to the
session's idle window) and relays `tools/list_changed` between MCP
replicas over the pub/sub channel `gw:tools_changed`. The tool cache and
connector health do not use Redis; the tool cache is on Postgres, which
every replica already shares. The connection fields are also what a
third-party `pkg/session` driver receives, so a driver selected by name
under `sessions.store` may read them too.

## tool_cache

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `tool_cache.enabled` | bool | `true` | `GATEWAY_TOOL_CACHE_ENABLED` | Turns on the Postgres-backed tool-list cache. With it off, every `tools/list` fans out live to every healthy connector. |
| `tool_cache.l2_ttl` | duration | `30m` | `GATEWAY_TOOL_CACHE_L2_TTL` | How long a cached tool row stays fresh. |
| `tool_cache.refresh_interval` | duration | `10m` | `GATEWAY_TOOL_CACHE_REFRESH_INTERVAL` | How often the background refresher re-reads every tenant's connectors. |
| `tool_cache.cleanup_interval` | duration | `15m` | `GATEWAY_TOOL_CACHE_CLEANUP_INTERVAL` | How often expired rows are deleted. |
| `tool_cache.serve_stale` | bool | `true` | `GATEWAY_TOOL_CACHE_SERVE_STALE` | Serves expired rows (and kicks off a background refresh) rather than blocking `tools/list` on a live fan-out. |

## sessions

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `sessions.store` | string | `memory` | `GATEWAY_SESSIONS_STORE` | The `pkg/session` driver MCP sessions live in. `memory`: in-process, so the MCP plane must run as **one** replica. `redis`: shared via the `redis` section (requires `redis.enabled: true` and `redis.addr`), so `gateway-mcp` can scale out and `tools/list_changed` reaches every replica's SSE streams. Any other value names a driver a custom binary registered with `pkg/session.Register`; an unregistered name fails at startup listing the registered ones. Logged at startup as `sessions store=<name>`. |
| `sessions.ttl` | duration | `1h` | `GATEWAY_SESSIONS_TTL` | Idle lifetime of an MCP session (a sliding window: every request moves it forward). With the `redis` store this is also the key TTL. |
| `sessions.cleanup_interval` | duration | `5m` | `GATEWAY_SESSIONS_CLEANUP_INTERVAL` | How often the `memory` store sweeps expired sessions. Ignored by `redis`, whose keys expire on their own. |
| `sessions.options` | map[string]string | `{}` | — (YAML only) | Passed verbatim to the driver as `pkg/session.Config.Options`, for a third-party driver that needs settings beyond the `redis` connection fields. The built-in drivers ignore it. |

## connectors

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `connectors.default_timeout_ms` | int | `30000` | `GATEWAY_CONNECTORS_DEFAULT_TIMEOUT_MS` | The `timeout_ms` a connector gets when it is created without one (through the API or a catalog add). Also bounds a backend call for a connector whose own `timeout_ms` is unset. Must be `100`–`600000` (the range the API accepts for `timeout_ms`); a value outside it fails startup. |
| `connectors.tls.ca_bundle` | string | `""` | `GATEWAY_CONNECTORS_TLS_CA_BUNDLE` | PEM file of additional trusted roots, added to (not replacing) the system pool. |
| `connectors.tls.insecure_skip_verify` | bool | `false` | `GATEWAY_CONNECTORS_TLS_INSECURE_SKIP_VERIFY` | Disables certificate verification for **every** connector. Should stay `false` outside a lab; a single connector that needs it can set `metadata.tls.insecure_skip_verify` on its own row instead. |
| `connectors.allow_bearer_token_forwarding` | bool | `false` | `GATEWAY_CONNECTORS_ALLOW_BEARER_TOKEN_FORWARDING` | Permits a connector header `{"type": "token_field", "field": "bearer_token"}`, which sends the **caller's own gateway credential** (its gateway API key) to the connector's backend. Off by default: with it on, whoever operates a connector endpoint receives every caller's gateway credential. Off, creating such a connector is `400 bearer_forwarding_disabled` and a call through an existing one fails with an error rather than forwarding. Enable only for backends you fully trust. |

## egress (outbound request policy)

The gateway refuses to connect to internal addresses on behalf of a URL a tenant or operator supplied (connectors, MCP catalog, model targets, model catalog providers). The check is on the address actually dialed, so it holds against alternate IP spellings, hostnames that resolve internally, DNS rebinding and redirects. See [security-model.md](security-model.md#outbound-requests-ssrf).

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `egress.allowed_cidrs` | list of CIDR | `[]` | `GATEWAY_EGRESS_ALLOWED_CIDRS` (comma-separated) | Destination ranges exempt from the block, e.g. `["127.0.0.0/8", "::1/128"]` or `["10.20.0.0/16"]`. A bare IP is a single-address range. |
| `egress.allowed_hosts` | list of string | `[]` | `GATEWAY_EGRESS_ALLOWED_HOSTS` (comma-separated) | Exact hostnames exempt from the block, e.g. `["host.docker.internal"]`. A dial to that name is allowed whatever it resolves to; the literal IP it resolves to is not thereby allowed. |

The default blocks `0.0.0.0/8`, `10/8`, `100.64/10`, `127/8`, `169.254/16`, `172.16/12`, `192.168/16`, multicast, `240/4` (incl. broadcast), `::`, `::1`, `fe80::/10`, `fc00::/7` and `ff00::/8`, plus IPv4-mapped/compatible/NAT64 forms of those. The shipped `deploy/docker-compose.yml` sets `host.docker.internal` and `127.0.0.0/8,::1/128` for the local stack; remove them for anything shared. Outbound proxies set via `HTTP(S)_PROXY` (used by LLM-plane and model-catalog calls; connector calls ignore them) are dialed through the same guard, so a proxy on a private address needs to be allowlisted.

## skills (skills & commands registry seed)

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `skills.seed_dir` | string | `""` | `GATEWAY_SKILLS_SEED_DIR` | Directory of skill/command bundles upserted as platform rows at startup, after `llm_proxy.models` is seeded (see [skills.md](skills.md#seed)). Empty disables it. Idempotent and safe for every replica to run. |

## mcp_catalog (platform MCP catalog seed)

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `mcp_catalog.seed` | list | the shipped list | *(YAML only)* | Seed for the platform [MCP catalog](connectors-and-credentials.md#mcp-catalog). Each `{slug, name, description, icon, category, url, url_overridable, transport, docs_url, auth{kind, fields[], header_template}, default_headers, suggested_tools, disabled}` is upserted by slug at startup, and entries the list does not mention are left alone. Validated at startup: `url` and `docs_url` must be https (http only for a loopback host), `transport` is empty or `streamable-http`, `auth.kind` is one of `none`/`bearer`/`basic`/`header`/`oauth`, and the field count fits the kind (none/oauth 0, bearer/header 1, basic 2; a field with `query` goes into the URL and may not be secret), and each `default_headers` entry needs a valid header name and a non-empty, single-line value. These are platform entries; tenant admins add their own through `/api/v1/mcp-catalog`. The default is the list in `configs/base/default.yml`, built into the binary. A config file that sets `seed` replaces it. `seed: []` stops seeding but does not remove rows already seeded. |

## capture (MCP-plane request/response capture)

This is distinct from `llm_proxy.capture` above — it governs the MCP
plane's access-log body capture, not the LLM plane's.

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `capture.store_bodies` | bool | `false` | `GATEWAY_CAPTURE_STORE_BODIES` | Records request and response payloads, plus inbound headers (allowlisted values; others masked as `****`), on every logged MCP call, bounded by the two caps below. Latency and identifiers are always recorded regardless. `deploy/docker-compose.yml` turns it on. |
| `capture.max_request_bytes` | int | `300000` | `GATEWAY_CAPTURE_MAX_REQUEST_BYTES` | Bounds a captured request payload; a record cut short is flagged `truncated`. |
| `capture.max_response_bytes` | int | `300000` | `GATEWAY_CAPTURE_MAX_RESPONSE_BYTES` | Bounds a captured response payload; a record cut short is flagged `truncated`. |

## sinks

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `sinks.stdout.enabled` | bool | `true` | `GATEWAY_SINKS_STDOUT_ENABLED` | JSON lines on stdout for every `AccessLog`/`LLMCall`. |
| `sinks.stdout.include_bodies` | bool | `false` | `GATEWAY_SINKS_STDOUT_INCLUDE_BODIES` | Also print captured request/response bodies, `messages`, `system` and `tools` on those lines (unredacted prompts and completions). Off by default; a startup warning is logged when it is on together with `store_bodies`. |
| `sinks.otel.enabled` | bool | `false` | `GATEWAY_SINKS_OTEL_ENABLED` | One OTLP span per record. |
| `sinks.otel.endpoint` | string | `""` (required when enabled) | `GATEWAY_SINKS_OTEL_ENDPOINT` | OTLP collector address, e.g. `localhost:4318` (http) or `localhost:4317` (grpc). |
| `sinks.otel.protocol` | string | `http` | `GATEWAY_SINKS_OTEL_PROTOCOL` | `http` \| `grpc`. |
| `sinks.otel.insecure` | bool | `false` | `GATEWAY_SINKS_OTEL_INSECURE` | Disables TLS on the OTLP connection. |
| `sinks.otel.headers` | `map[string]string` | none | *(YAML-only)* | Extra headers sent with every OTLP export (e.g. an ingest API key). |
| `sinks.otel.service_name` | string | `""` (falls back to the binary's own name) | `GATEWAY_SINKS_OTEL_SERVICE_NAME` | Exported resource's `service.name`. |
| `sinks.clickhouse.enabled` | bool | `false` | `GATEWAY_SINKS_CLICKHOUSE_ENABLED` | Batched inserts into `mcp_access_logs`/`llm_calls`, and the read side of `GET /api/v1/analytics/*`. |
| `sinks.clickhouse.host` | string | `""` (required when enabled) | `GATEWAY_SINKS_CLICKHOUSE_HOST` | ClickHouse host. |
| `sinks.clickhouse.port` | int | `9000` | `GATEWAY_SINKS_CLICKHOUSE_PORT` | ClickHouse native port. |
| `sinks.clickhouse.database` | string | `default` | `GATEWAY_SINKS_CLICKHOUSE_DATABASE` | Database name. |
| `sinks.clickhouse.username` | string | `""` | `GATEWAY_SINKS_CLICKHOUSE_USERNAME` | On startup the gateway creates and upgrades its tables and the `llm_usage_canonical` view in `database`, so this user needs `CREATE TABLE`, `ALTER TABLE` and `CREATE VIEW` there, plus `INSERT` and `SELECT`. If any of these fails, the gateway does not start. |
| `sinks.clickhouse.password` | string | `""` | `GATEWAY_SINKS_CLICKHOUSE_PASSWORD` | — |
| `sinks.clickhouse.secure` | bool | `false` | `GATEWAY_SINKS_CLICKHOUSE_SECURE` | TLS to ClickHouse. |
| `sinks.clickhouse.batch_size` | int | `100` | `GATEWAY_SINKS_CLICKHOUSE_BATCH_SIZE` | Buffered record count that triggers an immediate flush. |
| `sinks.clickhouse.flush_interval` | duration | `5s` | `GATEWAY_SINKS_CLICKHOUSE_FLUSH_INTERVAL` | Longest a record waits before being flushed. |
| `sinks.clickhouse.buffer_size` | int | `1000` | `GATEWAY_SINKS_CLICKHOUSE_BUFFER_SIZE` | Bounded queue capacity; a record written once it's full is dropped (see `/health`'s drop counters), not blocked on. |

## ingest (`POST /api/v1/ingest`)

Configures the interceptor-ingest endpoint — see
[api.md, "Ingest"](api.md#ingest) for the full route contract. Requires
`sinks.clickhouse.enabled: true` (the route answers `503` without it,
since the durable write goes straight to ClickHouse, bypassing the
best-effort async sink queue).

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `ingest.enabled` | bool | `false` | `GATEWAY_INGEST_ENABLED` | Mounts the route's real behavior. `false` makes it `404` with the exact body an unmounted route would, for any caller that already holds a valid `interceptor`-role key. |
| `ingest.max_body_bytes` | int | `33554432` (32 MiB) | `GATEWAY_INGEST_MAX_BODY_BYTES` | Cap on the DEcompressed request body. The compressed body on the wire is separately capped at a fixed 8 MiB regardless of this setting (not configurable — a safety backstop against a decompression bomb even if this is set very high). Must be positive when enabled. |
| `ingest.max_records` | int | `1000` | `GATEWAY_INGEST_MAX_RECORDS` | Cap on `llm_calls` + `access_logs` entries combined, per request. Must be positive when enabled. |
| `ingest.rate_per_minute` | int | `120` | `GATEWAY_INGEST_RATE_PER_MINUTE` | Requests per minute, per API key (not per IP — this route is exempt from the control plane's per-IP rate limiter; see [security-model.md, "Rate limiting and lockout"](security-model.md#rate-limiting-and-lockout)). Must be positive when enabled. |

## logging

| Key | Type | Default | Env var | Meaning |
|---|---|---|---|---|
| `logging.level` | string | `info` | `GATEWAY_LOGGING_LEVEL` | `debug` \| `info` \| `warn` \| `error`. |
| `logging.development` | bool | `false` | `GATEWAY_LOGGING_DEVELOPMENT` | `true` selects human-readable text logs instead of JSON. |

## YAML-only keys

These have no `GATEWAY_*` environment variable, because `overlayEnv`
(`internal/config/config.go`) only sets string/int/bool/duration fields and
`[]string` lists (comma-separated, e.g. `api.trusted_proxies`). Setting the
derived name of one of these (e.g. `GATEWAY_AUTH_ROLES`) fails startup with
`unsupported field kind`. Set them from a YAML file passed via
`--config`/`CONFIG_PATH`:

- `auth.roles` (`map[string][]string`)
- `sessions.options` (`map[string]string`)
- `sinks.otel.headers` (`map[string]string`)
- `llm_proxy.models` (list of objects)
- `mcp_catalog.seed` (list of objects)

## Secrets never live in YAML

No password, API key, or credential payload belongs in `configs/base/default.yml`
or any config file: the master key (`secret_store.master_key_env`/`master_key_file`)
is read from the environment or a file *outside* the YAML tree, and every
connector credential is created via `POST /api/v1/credentials` and stored
encrypted in Postgres, never inlined into a connector's `metadata`. See
[security-model.md](security-model.md).

## Single-plane deployment examples

`GATEWAY_MASTER_KEY` is a key you generated once with `gateway secrets genkey`
and kept (for example in a secret manager). Pass the same value on every
start, because credentials stored under one key cannot be decrypted with
another.

**MCP-only** (a pure tool-proxy instance, no console, no LLM traffic):

```sh
GATEWAY_MCP_ENABLED=true \
GATEWAY_API_ENABLED=false \
GATEWAY_LLM_PROXY_ENABLED=false \
GATEWAY_DATABASE_HOST=postgres \
GATEWAY_MASTER_KEY="$GATEWAY_MASTER_KEY" \
  ./gateway
```

**API-only** (control plane + console, e.g. behind an internal ingress):

```sh
GATEWAY_MCP_ENABLED=false \
GATEWAY_API_ENABLED=true \
GATEWAY_LLM_PROXY_ENABLED=false \
GATEWAY_DATABASE_HOST=postgres \
GATEWAY_MASTER_KEY="$GATEWAY_MASTER_KEY" \
  ./gateway
```

**LLM-only** (a BYOK proxy in front of Anthropic/Bedrock/OpenAI/Gemini):

```sh
GATEWAY_MCP_ENABLED=false \
GATEWAY_API_ENABLED=false \
GATEWAY_LLM_PROXY_ENABLED=true \
GATEWAY_LLM_PROXY_BEDROCK_ENABLED=true \
GATEWAY_DATABASE_HOST=postgres \
GATEWAY_MASTER_KEY="$GATEWAY_MASTER_KEY" \
  ./gateway
```

Each still needs Postgres (`database.*`) — it is required by every plane,
since the LLM plane's default capture store and every plane's auth chain
depend on it.
