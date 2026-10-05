# Control-plane API

The API plane (`:8081`, `api.enabled`) serves the control-plane REST API
under `/api/v1/*`, and optionally the embedded console at `/`
(`api.serve_ui`). Every route is declared once, in
`internal/api/router.go`'s `buildRoutes`, and both the mounted chi routes
and the generated OpenAPI document are built from that same table — they
cannot drift apart (`TestOpenAPI_CoversEveryRoute` asserts this by walking
the live route tree).

## Auth

Every non-public route runs behind `internal/auth.Middleware`: a gateway
API key (`Authorization: Bearer gk_...` or `X-Gateway-Key: gk_...`) or, for
the admin console, a logged-in user's session cookie (`gw_session`; writes
must also send `X-CSRF-Token`, and a user who must change their password is
limited to a few routes), resolved to a `Principal` — see
[authentication.md](authentication.md). A route with a non-empty `Permission` is
additionally gated by `internalauth.RequirePermission` — see
[security-model.md](security-model.md) for the role/permission model.
Permission is checked **before** any path-parameter id is parsed
(via `requireUUIDParam`), so an unauthorized caller learns nothing about
which ids exist — a non-UUID `{id}` is a `404`, not a `400`, for the same
reason.

## Error envelope

Every error response, from every plane's REST-shaped surface, has this
shape:

```json
{"error": {"type": "validation_error", "message": "name must be between 1 and 120 characters"}}
```

| `type` | HTTP status |
|---|---|
| `validation_error` | 400 |
| `egress_blocked` (a URL pointing at an internal address, see [security-model.md](security-model.md#outbound-requests-ssrf)) | 400 |
| `bearer_forwarding_disabled` | 400 |
| `authentication_error` (auth middleware; a failed `POST /auth/login`) | 401 |
| `permission_error` | 403 |
| `csrf_error` (missing/wrong `X-CSRF-Token` on a cookie-authenticated write) | 403 |
| `password_change_required` (the user must change their password first) | 403 |
| `not_found` | 404 |
| `conflict` | 409 |
| `last_admin` (cannot disable, delete or demote a tenant's last active admin) | 409 |
| `self_action` (cannot disable, delete or demote yourself) | 409 |
| `already_added` (MCP catalog entry already added) | 409 |
| `oauth_not_supported` (MCP catalog add) | 422 |
| `internal` | 500 |
| `not_implemented` | 501 |
| `unavailable` | 503 (502 when a provider's pricing API cannot be reached) |
| `too_large` | 413 |
| `rate_limited` | 429 |

A `500` never echoes the underlying error to the caller — it's logged
server-side via the request-scoped logger and the client gets a generic
`"internal error"`. A `401` likewise never says *why* a credential failed
(unknown vs. revoked vs. expired are indistinguishable to the caller), so
responses can't be used to enumerate which keys exist.

## Pagination

The list routes `/users`, `/tenants`, `/api-keys`, `/credentials`,
`/connectors`, `/mcp-catalog`, `/profiles`, `/models`, `/skills` and
`/auth/audit` accept `?limit=&offset=`, clamped to `[1, 500]` /
`[0, +inf)` (defaults: `limit=50`, `offset=0`); an invalid value falls
back to the default rather than erroring. The response shape is:

```json
{"items": [...], "total": 42}
```

`total` is the full matching count, independent of `limit`/`offset`.
Sub-resource lists (`/connectors/{id}/tools`, `/profiles/{id}/tools`,
`/profiles/{id}/skills`, `/skills/{id}/versions`) use the same shape but
return every item and ignore `limit`/`offset`.

## Route table

All paths are relative to `/api/v1`. **Public** routes need no
authentication.

| Method | Path | Permission | What it does |
|---|---|---|---|
| GET | `/` | *(public)* | Service info. |
| GET | `/health` | *(public)* | Liveness probe; includes sink status, rate-limiter lockout count, and (when this process runs the LLM plane) `limits: {budget_denials, rpm_denials}`. |
| GET | `/docs` | *(public)* | HTML API reference page (Scalar, loads `/openapi.json`). |
| GET | `/openapi.json` | *(public)* | Generated OpenAPI 3.0 document. |
| GET | `/auth/config` | *(public)* | What the login page should offer: `{password_login, api_key_login, single_tenant, default_tenant, has_users}`. `has_users` is `false` only on a single-tenant install whose default tenant has no active (live, not disabled) user, so the login page can show first-run guidance; with several tenants it is always `true`. |
| POST | `/auth/login` | *(public)* | Body `{tenant, username, password}` (tenant slug; omitted = the default tenant). `200 {user, must_change_password, csrf_token}` and sets the `gw_session` cookie. Any failure is the same `401`. Behind the per-IP lockout, plus a per-user one. |
| POST | `/auth/logout` | *(authenticated only)* | Revokes the calling session and clears the cookie; `204`. |
| GET | `/auth/me` | *(authenticated only)* | The caller's own `Principal` (`subject`, `tenant_id`, `email`, `roles`, `auth_method`, `key_id`) plus `kind` (`user` \| `api_key`); for a console user also `user`, `must_change_password` and `csrf_token`. |
| POST | `/auth/password` | *(authenticated only)* | A console user changes their own password: `{current_password, new_password}`; `204`, revokes their other sessions. API keys get `403`. |
| GET | `/auth/audit` | `users.manage` | The tenant's authentication audit trail, newest first (`?action=`, `?user_id=`, `limit`/`offset`). |
| POST | `/users` | `users.manage` | Create a console user `{username, display_name?, role: admin\|viewer, password?}`; `201 {user, temporary_password?}` (no password: one is generated, returned once, and must be changed at first login). |
| GET | `/users` | `users.manage` | List the tenant's users (`?role=`). |
| GET | `/users/{id}` | `users.manage` | Get one user. |
| PATCH | `/users/{id}` | `users.manage` | Update `display_name`, `role`, `disabled`. `409` `self_action` / `last_admin`. The username is immutable. |
| DELETE | `/users/{id}` | `users.manage` | Soft-delete; revokes sessions. Same `409` guards. |
| POST | `/users/{id}/reset-password` | `users.manage` | `200 {temporary_password}`; forces a change and revokes all the user's sessions. Admin API keys may call it. |
| POST | `/users/{id}/revoke-sessions` | `users.manage` | Revoke all of the user's sessions; `204`. |
| POST | `/tenants` | `platform.admin` | Create a tenant. |
| GET | `/tenants` | `platform.admin` | List every tenant (cross-tenant by nature). |
| POST | `/api-keys` | `admin.manage` | Create an API key; returns the plaintext once. Optional `expires_at`: an RFC 3339 timestamp, or a `YYYY-MM-DD` date meaning the end of that day (UTC); must be in the future. Optional `limits` (see [API key limits](#api-key-limits)). Optional `profile_id`: bind the key to an agent profile of the tenant (see [profiles.md](profiles.md#binding-a-key-to-a-profile)). `role: "platform-admin"` needs the caller to hold `platform.admin`. |
| GET | `/api-keys` | `admin.manage` | List the tenant's API keys (no plaintext; includes `limits`, and `profile_id`/`profile_name` for a bound key). |
| PATCH | `/api-keys/{id}` | `admin.manage` | Set (`{"limits":{...}}`, replaces wholesale) or clear (`{"limits":null}`) a key's LLM limits; set (`{"profile_id":"<id>"}`) or clear (`{"profile_id":null}`) its profile binding. At least one of the two fields is required (`400` otherwise). |
| DELETE | `/api-keys/{id}` | `admin.manage` | Revoke a key; every gateway replica evicts its cached lookup within milliseconds (Postgres `LISTEN/NOTIFY`; `auth.api_keys.cache_ttl` is the backstop). |
| POST | `/api-keys/{id}/rotate` | `admin.manage` | Revoke and reissue; returns the new plaintext once. |
| POST | `/credentials` | `admin.manage` | Create an encrypted credential. |
| GET | `/credentials` | `admin.manage` | List credential metadata (no payload). |
| GET | `/credentials/{name}` | `admin.manage` | Get one credential's metadata. |
| PUT | `/credentials/{name}` | `admin.manage` | Rotate a credential's payload. |
| DELETE | `/credentials/{name}` | `admin.manage` | Delete a credential. |
| POST | `/connectors` | `connector.create` | Register a connector (an MCP server registered with the gateway; the console's **MCPs** page). `name`/`slug` of `gateway` (case-insensitive) is rejected: it's reserved for the gateway's own native `gateway__skill` tool and command prompts. |
| GET | `/connectors` | `connector.read` | List connectors. |
| GET | `/connectors/{id}` | `connector.read` | Get one connector. |
| PUT | `/connectors/{id}` | `connector.update` | Update a connector. Same `gateway` reservation as create. A full replace: omitting `timeout_ms` resets it to `connectors.default_timeout_ms`. |
| DELETE | `/connectors/{id}` | `connector.delete` | Soft-delete a connector (its slug becomes re-usable). |
| GET | `/connectors/{id}/tools` | `connector.read` | List its cached tools. |
| GET | `/connectors/{id}/health` | `connector.read` | Live health probe (works with `mcp.enabled: false`). |
| POST | `/connectors/{id}/discover` | `connector.update` | Live tool discovery, writes the cache. |
| GET | `/mcp-catalog` | `connector.read` | List the [MCP catalog](connectors-and-credentials.md#mcp-catalog): platform entries plus the tenant's own (`scope` is `platform` or `tenant`; the tenant's wins on a shared slug), each with per-tenant `added` / `connector_id`, and `supported: false` plus `unsupported_reason` for OAuth entries. Disabled entries are shown only to callers who can create connectors. |
| GET | `/mcp-catalog/{slug}` | `connector.read` | One catalog entry, same shape. |
| POST | `/mcp-catalog/{slug}/add` | `connector.create` | Add an entry to the tenant: stores the supplied credential, creates a connector with `catalog_id`, probes health and discovers tools. `201 {connector, discovery}`; `422 oauth_not_supported`; `409 already_added` unless a different `slug` is sent. |
| POST | `/mcp-catalog` | `connector.create` | Create a catalog entry in the caller's tenant; `scope: "platform"` creates a platform entry and needs `platform.admin`. Validated like the config seed (https or loopback `url`, auth kind and field count). `409` on a slug already live in that scope. |
| PUT | `/mcp-catalog/{slug}` | `connector.update` | Full replace of an entry's fields (slug and owner never change). Targets the caller's entry, else the platform one; `?scope=tenant\|platform` picks explicitly. A platform entry needs `platform.admin`, otherwise `403`. |
| DELETE | `/mcp-catalog/{slug}` | `connector.delete` | Soft-delete an entry, same scope rules as PUT. Connectors already added from it are untouched. |
| POST | `/profiles` | `profile.create` | Create an agent profile. |
| GET | `/profiles` | `profile.read` | List profiles. |
| GET | `/profiles/{id}` | `profile.read` | Get one profile. |
| PUT | `/profiles/{id}` | `profile.update` | Full replace: `name` is required, and an omitted `description` or `instructions` is cleared (`metadata` is kept when omitted). `instructions` (free text, ≤ 8 KiB) is prepended to the resolved profile's MCP `initialize` instructions. The slug never changes, so after a rename `X-Agent-Profile-Name` must still use the original name. |
| DELETE | `/profiles/{id}` | `profile.delete` | Soft-delete a profile (its slug becomes re-usable). |
| PUT | `/profiles/{id}/tools` | `profile.update` | Replace a profile's tool allow-list. |
| GET | `/profiles/{id}/tools` | `profile.read` | Get a profile's tool allow-list. |
| PUT | `/profiles/{id}/skills` | `profile.update` | Replace a profile's attached [skills/commands](skills.md): `{items:[{skill_id, version?}]}` (replace semantics like `/tools`); every `skill_id` must be visible to the caller's tenant (tenant row or platform row) and a pinned `version` must already exist for it. |
| GET | `/profiles/{id}/skills` | `profile.read` | Get a profile's attached skills/commands: `{items:[{skill_id, name, kind, version?, latest_version, description}]}`. |
| POST | `/models` | `model.create` | Register a model in the LLM plane's [model registry](llm-plane.md#model-registry): `{name, description?, enabled?, targets[], price?, limits?, metadata?}`; a target is `{vendor, model, base_url?, credential?, region?, allow_caller_key?, label?}` (`label`: the vendor behind an `openai_compat` target, see [Labels](llm-plane.md#labels)). A target's `credential` must exist in the tenant and cannot be combined with `allow_caller_key`. `limits` has the shape and validation of [API key limits](#api-key-limits) and is enforced per (tenant, model name) — see [llm-plane.md, "Model-level limits"](llm-plane.md#model-level-limits). |
| GET | `/models` | `model.read` | List registered models: the tenant's rows plus the platform defaults (`"scope": "tenant"` \| `"platform"`, tenant wins on a shared name). Never includes credential values. |
| GET | `/models/{id}` | `model.read` | Get one registered model (tenant or platform row). |
| PUT | `/models/{id}` | `model.update` | Full-replace update of a tenant row, `limits` included (omitted = cleared; `metadata` kept when omitted). Platform rows are `403` — override one by creating a tenant row of the same name. |
| DELETE | `/models/{id}` | `model.delete` | Soft-delete a tenant row (its name becomes re-usable). Platform rows are `403`. |
| GET | `/model-catalog` | `model.read` | The platform [model catalog](llm-plane.md#model-registry): every provider and its models (each with `capabilities` and a `notes` free-text field explaining a false/limited one), each annotated with the caller's own registered model created from it, if any (`tenant_model_id`/`tenant_model_name`, both `null` otherwise). |
| POST | `/model-catalog/providers` | `platform.catalog.manage` | Create a catalog provider: `{slug, display_name, base_url, docs_url?, enabled?}`. `201 {..., warnings: []}` — a non-fatal `warnings` entry when `base_url` looks like it is missing an API version segment. |
| PUT | `/model-catalog/providers/{id}` | `platform.catalog.manage` | Full-replace update of a provider (same body and `warnings` as create). |
| DELETE | `/model-catalog/providers/{id}` | `platform.catalog.manage` | Delete a provider (and its models); `409 {..., tenant_models: n}` if any tenant model was connected from one of its models, unless `?force=true`. |
| POST | `/model-catalog/providers/{id}/models` | `platform.catalog.manage` | Add a model to a provider: `{model_id, display_name?, suggested_name?, price?, capabilities?, notes?, enabled?}`; `suggested_name` defaults to `<sanitized last path segment of model_id>-<provider slug>`. `notes` is a free-text admin note explaining a false/limited capability (e.g. "Multi-turn tool use through Chat Completions translation is not supported yet; plain chat works."). |
| PUT | `/model-catalog/models/{id}` | `platform.catalog.manage` | Full-replace update of a catalog model (same body as create; the provider is immutable). |
| DELETE | `/model-catalog/models/{id}` | `platform.catalog.manage` | Delete a catalog model; same `409`-with-usage-count rule as deleting a provider. |
| GET | `/model-catalog/models/{id}/usage` | `platform.catalog.manage` | `{tenant_models: n}` — how many tenant models (across every tenant) were connected from this catalog model. |
| POST | `/model-catalog/providers/{id}/test` | `model.create` | Test a provider's `base_url` and a key: body `{credential?, api_key?}` (exactly one; an existing credential's key is read from its `api_key` field, falling back to a single-field credential's sole value). Calls `GET {base_url}/models` with a 10s timeout, no redirects followed; always `200 {ok, status, error?, models_found, catalog_matches}` — a network/HTTP failure reaching the provider is reported in the body, never as a `5xx`, and the key is never echoed. `catalog_matches` is keyed by the catalog model's vendor `model_id` string (e.g. `"zai-org/GLM-5.3"`), not the catalog row's `id`. |
| POST | `/model-catalog/providers/{id}/connect` | `model.create` (+ `credential.create`, and `credential.read` when reusing an existing credential) | Connect a provider: body `{credential: {"name": "<existing>"} \| {"new": {"name"?, "api_key"}}, models: ["<catalog model id>", ...]}` (`models` may omit catalog models the tenant already connected — the console only sends newly picked ones; re-sending an already-connected one is still `status: "exists"`, not an error). One transaction: creates (or reuses) the credential — a new one is type `api_key`, payload `{"api_key": "<key>"}`, matching the console's own "Accept API key" credential exactly — then registers each selected catalog model as a tenant model (`targets` built from the provider's `base_url` and `model_id`, `label` set to the provider's slug so it matches the console's own vendor presets). `201 {credential: {name, created}, models: [{catalog_model_id, model_id, name, status: "created"\|"exists"}]}`. A disabled provider/model is `400`; a name clash with a non-catalog tenant model is `409` naming it; every `409` from a catalog delete/connect conflict includes the usual error envelope. |
| POST | `/model-catalog/providers/{id}/prices/preview` | `platform.catalog.manage` | Preview a price refresh (see [Refreshing catalog prices](models.md#refreshing-catalog-prices)): body `{credential?, api_key?}` (both optional for `nebius`, whose pricing feed is public; exactly one required for `together`). Read-only — fetches the provider's CURRENT prices from its own pricing API and diffs them against the catalog, writing nothing. `200 {source_url, fetched_at, items: [{catalog_model_id, model_id, current_price, new_price, changed}], unmatched_provider_models: n}` — `new_price` is `null` (and `changed: false`) for a catalog model the feed didn't mention; `unmatched_provider_models` counts vendor model ids the feed priced that match none of this provider's catalog models. A provider with no registered price source is `400` ("price refresh not supported for `<slug>`"); a failure reaching the provider's pricing API is `502`, key never echoed. |
| POST | `/model-catalog/providers/{id}/prices/apply` | `platform.catalog.manage` | Apply a price refresh: body `{items: [{catalog_model_id, price}], update_tenant_models: bool}`. One transaction: writes each item's `price` onto its catalog model, and — when `update_tenant_models` is `true` — also onto every tenant model (across every tenant) connected from that catalog model whose own price still equals the catalog model's OLD price (so a tenant admin's manual override survives a platform-wide refresh). `200 {catalog_updated: n, tenant_updated: n}`. A `catalog_model_id` not belonging to this provider, or a negative price field, is `400`. |
| POST | `/skills` | `skill.create` | Register a skill or command in the [skills & commands registry](skills.md): `{name, kind, enabled?, metadata?, arguments?, files:[{path, content}]}`. `files` must include `SKILL.md` at the root; every file is validated (path shape, extension allow-list, size caps, secret scan) and `SKILL.md`'s YAML frontmatter is parsed and validated (allowed keys only, `hooks` rejected, `name` must equal the request's `name`, `description` 1..1024 chars). `description` is always derived from that frontmatter — it is read-only through this API; a `description` in the request body is accepted (no "unknown field" error) but ignored. `kind: "command"` additionally validates `arguments` (`{name, description?, required?}`, ≤10) and that every `{{placeholder}}` in `SKILL.md`'s body is a declared argument. Response is a skill view with `latest_version: 1`. |
| GET | `/skills` | `skill.read` | List registered skills/commands: the tenant's rows plus the platform defaults (`"scope": "tenant"` \| `"platform"`, tenant wins on a shared name); `?kind=skill\|command` filters. |
| GET | `/skills/{id}` | `skill.read` | Get one skill/command (tenant or platform row), plus its `latest` version's files (`{version, files:[{path, content, sha256, size}]}`). |
| PUT | `/skills/{id}` | `skill.update` | Partially update `enabled?`/`metadata?`/`arguments?` (each omitted field is left as-is). `description` is not settable here — it is accepted in the body (no error) but ignored; only `POST .../versions` changes it, by deriving it from the new version's `SKILL.md`. Platform rows are `403` — override one by creating a tenant row of the same name. |
| DELETE | `/skills/{id}` | `skill.delete` | Soft-delete a tenant row (its name becomes re-usable). Platform rows are `403`. |
| POST | `/skills/{id}/versions` | `skill.update` | Add a new version: `{files:[{path, content}]}`, same file/frontmatter validation as create. The skill's `description`/frontmatter are refreshed from the new `SKILL.md`; `name`, `kind` and `arguments` carry over unchanged (use `PUT /skills/{id}` to change `arguments`). Platform rows are `403`. Response is the new version (`{version, files, created_by, created_at}`). |
| GET | `/skills/{id}/versions` | `skill.read` | List a skill/command's versions, newest first, without file bodies: `{items:[{version, created_by, created_at, file_count}]}`. |
| GET | `/skills/{id}/versions/{v}` | `skill.read` | Get one version's files: `{version, files, created_by, created_at}`. |
| GET | `/headers/providers` | `headers.read` | List header resolver types and registered external providers. |
| GET | `/cache/stats` | `cache.read` | Per-connector tool-cache counts. |
| GET | `/cache/search` | `cache.read` | Full-text search over cached tools. |
| POST | `/cache/refresh` | `cache.manage` | Re-discover and rewrite the cache for every connector. |
| POST | `/cache/refresh/connectors/{id}` | `cache.manage` | Re-discover and rewrite the cache for one connector. |
| DELETE | `/cache` | `cache.manage` | Invalidate the cache for every connector. |
| DELETE | `/cache/connectors/{id}` | `cache.manage` | Invalidate the cache for one connector. |
| GET | `/analytics/overview` | `analytics.read` | Dashboard KPIs/usage/traffic for a time range (404 without ClickHouse). |
| GET | `/analytics/models` | `analytics.read` | Per-model usage for `?range=24h\|7d\|30d` (default `7d`): one row per requested model name with calls, tokens, nullable `cost_usd`, `used_by`, `last_seen` and the vendor that served most of its calls (404 without ClickHouse). |
| GET | `/analytics/skills/usage` | `analytics.read` | Skills the model was observed *using* in LLM traffic (`llm_calls.skills_used`), `?range=24h\|7d\|30d` (default `7d`): `{range, skills:[{name, calls, used_by, last_seen, registered, kind}]}`; `registered` is true when a tenant or platform skill/command of that name exists, `kind` is `skill`, `command` or `""`. See [observability.md](observability.md#discovered-skills-and-mcp-servers) (404 without ClickHouse). |
| GET | `/analytics/mcps/usage` | `analytics.read` | MCP servers/tools the model was observed using (`llm_calls.mcp_tools_used`), same `range`: `{range, servers:[{server, tools, calls, used_by, last_seen, registered_connector_slug, via_gateway}]}` sorted by `calls` desc; `calls` counts distinct LLM calls that used at least one tool of the server (not per-tool uses); `via_gateway` rows are attributed to the tenant connector whose slug prefixes the tool (404 without ClickHouse). |
| GET | `/analytics/skills` | `analytics.read` | Per skill/command usage for `?range=24h\|7d\|30d` (default `7d`): one row per `skill_name` seen in `mcp_access_logs` (a `gateway__skill` call or a native command's `prompts/get`), with `kind` joined in from the skill/command registry by name, `calls`, `used_by`, `last_seen`, plus a `most_used` summary (404 without ClickHouse). |
| GET | `/analytics/client-models` | `analytics.read` | Client → model → provider usage graph for `?range=24h\|7d\|30d` (default `7d`), `?metric=calls\|tokens\|cost` (default `calls`), `?limit=` (max `MODEL` nodes before the rest fold into one `other-models` node, default 10) and an optional `?client_name=` filter: `nodes`/`links` ready for a Sankey chart (404 without ClickHouse). |
| GET | `/analytics/traffic-flow` | `analytics.read` | Agent traffic flow across both planes for the same `?range=`, `?metric=`, `?limit=` and `?client_name=` parameters as `/analytics/client-models`: a CLIENT → PATH (`llm` \| `mcp`) → MODEL \| CONNECTOR graph joining `llm_calls` and `tools/call` rows from `mcp_access_logs`, in the same `nodes`/`links` shape; `limit` folds the tail of each terminal column into `other-models` / `other-connectors` and `MODEL` nodes carry their provider as `sublabel`; `agents` lists every client family in the window (unaffected by `client_name`). `metric=tokens\|cost` return the LLM branch only (404 without ClickHouse). |
| GET | `/analytics/token-monitoring` | `analytics.read` | Token usage and cost by model, caller and caller role for `window=today\|7d\|30d` (default `today`), with the previous period and a usage series. See [observability.md](observability.md#token-monitoring). |
| GET | `/analytics/token-monitoring/model` | `analytics.read` | One model (`?model=`): usage by caller and by session (`limit`/`offset` page the sessions). |
| GET | `/analytics/token-monitoring/keys/{id}` | `analytics.read` | One API key: usage by model and by session. |
| GET | `/analytics/logs` | `analytics.read` | Paginated MCP access-log rows (no bodies). |
| GET | `/analytics/logs/{request_id}` | `admin.manage` | One access-log row, with bodies. |
| GET | `/analytics/llm-logs` | `analytics.read` | Paginated LLM-call rows (no bodies). Without ClickHouse, served from the Postgres capture table (`llm_calls`). |
| GET | `/analytics/llm-logs/{request_id}` | `admin.manage` | One LLM-call row, with bodies. Same Postgres fallback as the list. |
| GET | `/analytics/sessions/{session_id}/timeline` | `analytics.read` | Merged MCP+LLM event timeline for one gateway session, under the [ownership rule](observability.md#session-timeline-ownership) (`owner_key_id`, `excluded_foreign_events`). Query `order=asc\|desc` (default `asc`; anything else is 400), sorted by `(ts, id)` and echoed as `order`; 404 without ClickHouse. |
| POST | `/ingest` | `ingest.write` | Ingest externally captured LLM/access-log records — see [Ingest](#ingest). |

## API key limits

An API key can carry LLM-plane budgets and caps, enforced by the LLM
plane before a request is forwarded (algorithm, caching and error shapes:
[llm-plane.md, "Budgets and limits"](llm-plane.md#budgets-and-limits)):

```json
{"limits": {"daily_usd": 5.0, "monthly_usd": 100.0, "rpm": 60, "max_tokens": 4096}}
```

| Field | Type | Meaning |
|---|---|---|
| `daily_usd` | number ≥ 0 | Captured `cost_usd` for this key since 00:00 UTC today. |
| `monthly_usd` | number ≥ 0 | Captured `cost_usd` for this key since the 1st of the month, 00:00 UTC. |
| `rpm` | integer 0–100000 | Requests per rolling minute (per replica). |
| `max_tokens` | integer ≥ 0 | Largest `max_tokens` a request may ask for (larger → 400). |

Every field is optional, but a `limits` object must set at least one;
`0` is a real limit (it blocks). Accepted on `POST /api-keys`, replaced
wholesale by `PATCH /api-keys/{id}` (send `null` to clear; the LLM plane
picks the change up within 10 s), returned on
create, list, rotate and patch, and carried over by rotate (spend is
tracked per key id, so the rotated key starts at $0). Example:

```
PATCH /api/v1/api-keys/3f0c…
{"limits": {"daily_usd": 0.5, "rpm": 30}}

200 OK
{"id":"3f0c…","name":"ci-bot","role":"agent","prefix":"gk_ab12cd34",
 "created_at":"2026-09-27T10:00:00Z","limits":{"daily_usd":0.5,"rpm":30}}
```

For per-field request/response shapes and worked examples, see
[connectors-and-credentials.md](connectors-and-credentials.md) and
[profiles.md](profiles.md). For a live, browsable copy of this table,
run the gateway and open `GET /api/v1/docs` (or fetch
`GET /api/v1/openapi.json` directly).

## Ingest

`POST /ingest` accepts externally captured LLM/access-log records and
writes them into the SAME `llm_calls`/`mcp_access_logs` tables a normal
gateway-proxied call writes — tagged `source: "interceptor"` (see
[security-model.md, "Identity: keys, roles, permissions"](security-model.md#identity-keys-roles-permissions)
for the `interceptor` role) — so they show up on the existing `/analytics/logs` and
`/analytics/llm-logs` pages with no new page. The gateway never forwards
this traffic to any provider; it only stores it. Records come from an external
capture client; any client that sends this schema can use it.

Off by default: `ingest.enabled: false` (`GATEWAY_INGEST_ENABLED`) makes
this route 404 with the exact body an unmounted route would — see
"Enabling" below for what that means for auth ordering.

```
POST /api/v1/ingest
X-Gateway-Key: gk_...          key's role must grant "ingest.write"
Content-Type: application/json
Content-Encoding: gzip         optional
```

**Limits.** Compressed body at most 8 MiB (fixed, not configurable); JSON
decompresses to at most `ingest.max_body_bytes` (default 32 MiB,
`GATEWAY_INGEST_MAX_BODY_BYTES`); at most `ingest.max_records` (default
1000, `GATEWAY_INGEST_MAX_RECORDS`) records total across both arrays.
Requests are additionally capped per API key at `ingest.rate_per_minute`
(default 120, `GATEWAY_INGEST_RATE_PER_MINUTE`) — per key, not per IP, so
many interceptor installs behind one office NAT don't throttle each other;
this route is exempt from the control plane's per-IP rate limiter.

**Request body:**

```json
{
  "schema_version": 1,
  "user": "person@example.com",
  "llm_calls":   [ { "...": "one entry per model response" } ],
  "access_logs": [ { "...": "one entry per connector tool call" } ]
}
```

`schema_version` must be `1`. `user` is a self-reported caller identity
(e.g. the Claude account email), capped at 256 chars, stamped onto every
record in the batch as `sink.AccessLog`/`LLMCall.User`.

An **`llm_calls`** entry: required `timestamp` (RFC 3339), `request_id`
(≤128 chars, unique per call), `status_code` (100–599). Optional:
`session_id`, `client_ip`, `client_name`, `user_agent`, `provider`,
`upstream_host`, `model`, `requested_model`, `path`, `duration_ms`,
`stream`, `input_tokens`, `output_tokens`, `cache_read_tokens`,
`cache_creation_tokens`, `stop_reason`, `provider_request_id`, `error`,
`messages`, `request_body`, `response_body`, `truncated`. `client_name`,
left empty, is classified from `user_agent` the same way the LLM plane
classifies a live call ([`sink.ClientFamily`](../pkg/sink/client.go)).
`cost_usd` is never accepted (always stored `NULL` — subscription usage
isn't billed per token).

An **`access_logs`** entry: same required trio. Optional: `session_id`,
`client_session_id`, `method`, `json_rpc_id`, `connector_id`, `tool_name`,
`skill_name`, `error_code`, `duration_ms`, `bytes_in`, `bytes`,
`client_ip`, `user_agent`, `request_body`, `response_body`, `truncated`.

Body fields (`request_body`/`response_body`/`messages`) are plain JSON/text
strings, not base64. They're stored only when the matching capture setting
is on — `llm_proxy.capture.store_bodies` for `llm_calls`,
`capture.store_bodies` for `access_logs` — and bounded by that setting's
own `max_request_bytes`/`max_response_bytes`, exactly like a
gateway-proxied call's capture; a cut body is flagged `truncated` in the
stored row. `tenant_id`, `key_id`, and `principal` are always taken from
the authenticated key — any such field in a record is silently ignored,
never trusted from the payload.

**Response `200`:**

```json
{
  "accepted":   { "llm_calls": 12, "access_logs": 3 },
  "duplicates": { "llm_calls": 0,  "access_logs": 0 },
  "rejected":   [ { "kind": "llm_call", "index": 4, "reason": "missing request_id" } ]
}
```

A record whose `(tenant_id, request_id)` already exists is skipped and
counted as a duplicate (client retries are safe) — this check is
best-effort, not a database constraint (ClickHouse has no unique index).
A record failing validation is skipped and listed in `rejected`, never
counted as a duplicate. The write is synchronous: a `200` means the batch
is durably stored (minus any `rejected`/`duplicates`).

**Error codes:** `400` malformed body, unknown `schema_version`, or a
record-count/validation failure; `401`/`403` auth; `404` when
`ingest.enabled` is `false`; `413` compressed/decompressed body too large;
`429` rate limited (`Retry-After` header); `503` storage failed (retry) or
no ClickHouse sink is configured on this instance.

**Enabling.** Auth and permission checks (`Middleware` +
`RequirePermission("ingest.write")`) run exactly as for every other route,
ahead of the `ingest.enabled` check — so an unauthenticated or
wrong-role caller still gets the usual `401`/`403` regardless of whether
ingest is enabled. Only for a caller that already holds a valid
`interceptor`-role key does a disabled deployment answer with the *exact*
body an unmounted route's `404` would, so that caller learns nothing about
whether the feature exists on this deployment versus not being rolled out
yet.
