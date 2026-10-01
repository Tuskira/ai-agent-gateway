# LLM plane

The LLM plane (`:8082`, `llm_proxy.enabled`) fronts LLM providers behind
one authenticated surface: every request needs a gateway API key
(`internal/auth.Middleware`, ahead of the plane), and every call is
captured for usage/cost. Authentication is *not* the plane's own concern —
`llmplane.Handler` builds only the provider routing, limits, and capture;
`cmd/gateway/main.go` wraps it with the shared auth middleware and an
unauthenticated `GET /health` ahead of it.

Unregistered calls are forwarded **byte-for-byte** for Anthropic/OpenAI/Gemini
(no re-marshaling), so Anthropic prompt caching survives the proxy hop. A call
to a [registered](#model-registry) same-format target has only its top-level
members rewritten (`model`; nested content stays byte-identical). Beyond that,
a body changes only with the opt-in
`llm_proxy.providers.openai.inject_stream_usage`, off by default (see
[OpenAI streams](#cost-estimates-and-pricing_file)), and a call naming a
model whose [registry](#model-registry) target speaks another wire format
is translated rather than forwarded (see
[Translated targets](#translated-targets)).
Bedrock requests are SigV4-signed or passed through already-signed,
depending on credential mode.

## Routes per provider

| Provider | Bare route | Prefixed route | Enable flag |
|---|---|---|---|
| Anthropic | `/v1/messages*`, `/v1/complete*` | `/anthropic/*` | always on when the LLM plane is (no separate flag) |
| Bedrock | `/model/*` | `/bedrock/*` | `llm_proxy.bedrock.enabled` |
| OpenAI | — (prefix only) | `/openai/*` | `llm_proxy.providers.openai.enabled` |
| Gemini | — (prefix only) | `/gemini/*` | `llm_proxy.providers.gemini.enabled` |

A `/{provider-id}/…` prefix always selects that provider explicitly
(useful for a client whose base URL can't be a bare match). The bare
routes exist for SDKs that append a fixed native path to a configured base
URL and can't add a prefix — each is scoped to that provider's own
endpoints only, so a shared namespace (OpenAI's `/v1/chat/completions`,
also under `/v1`) is never misrouted; OpenAI only has the prefixed form
for exactly this reason. Anthropic is the only provider with no enable
flag: it is present whenever the LLM plane itself is on, configured via
`llm_proxy.upstream_base_url`.

## BYOK setup

**Claude Code / Anthropic SDK:**

```sh
export ANTHROPIC_BASE_URL=http://localhost:8082
export ANTHROPIC_CUSTOM_HEADERS="X-Gateway-Key: $GATEWAY_KEY"   # gk_... — the gateway's own key
export ANTHROPIC_API_KEY=sk-ant-...        # your own Anthropic key (BYOK)
```

Claude Code sets `ANTHROPIC_BASE_URL` and appends `/v1/messages`. The
gateway's own key authenticates to the gateway (as
`Authorization: Bearer gk_...` or `X-Gateway-Key`); the Anthropic key
rides through as `x-api-key`/its own `Authorization` untouched — the
gateway strips only its *own* bearer credential before forwarding, never
a provider one. (`ANTHROPIC_AUTH_TOKEN=$GATEWAY_KEY` also works when
`ANTHROPIC_API_KEY` is set, but it replaces a `claude login` credential.)

```sh
curl -X POST http://localhost:8082/v1/messages \
  -H "X-Gateway-Key: $GATEWAY_KEY" \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-5","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}'
```

**OpenAI SDK:**

```sh
export OPENAI_BASE_URL=http://localhost:8082/openai/v1   # /v1 included: the SDK appends only /chat/completions
export OPENAI_API_KEY=sk-...   # your own OpenAI key (BYOK); send the gateway key as an
                                # X-Gateway-Key header (e.g. the SDK's default_headers)
```

```sh
curl -X POST http://localhost:8082/openai/v1/chat/completions \
  -H "X-Gateway-Key: $GATEWAY_KEY" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

**Gemini:**

```sh
curl -X POST "http://localhost:8082/gemini/v1beta/models/gemini-2.5-pro:generateContent" \
  -H "X-Gateway-Key: $GATEWAY_KEY" \
  -H "x-goog-api-key: $GEMINI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"contents":[{"parts":[{"text":"hi"}]}]}'
```

**Bedrock** — see the three credential modes below; `llm_proxy.bedrock.enabled`
must be `true`.

## Bedrock credential modes

`llm_proxy.bedrock.credential_mode` selects (or `auto`-detects) how a
request is authenticated to AWS:

| Mode | How it works | Headers |
|---|---|---|
| `passthrough` (Flow A) | The client already SigV4-signed the request; the gateway forwards it **untouched**, no re-signing. Region is parsed from the signed credential scope. | Client's own `Authorization: AWS4-HMAC-SHA256 ...` |
| `client_keys` (Flow B) | The client supplies AWS credentials or a role ARN; the gateway signs with them and strips the credential headers before forwarding. | `X-Bedrock-Access-Key-Id` + `X-Bedrock-Secret-Access-Key` [+ `X-Bedrock-Session-Token`], **or** `X-Bedrock-Role-Arn` (assumed via STS with the gateway's own base credentials; the `ExternalId` is always the caller's **tenant ID**, set by the gateway — see below) |
| `gateway` (Flow C) | The gateway signs with its **own** AWS identity (the default credential chain, loaded once, credentials refreshed lazily per request). | none required |

`auto` (the default) picks passthrough if `Authorization` starts with
`AWS4-HMAC-SHA256`, `client_keys` if either credential header is present,
else falls back to `gateway`. `X-Bedrock-Region` overrides
`llm_proxy.bedrock.region` per request in the signed modes (B/C). The
region (header, config, or Flow A's credential scope) must be a
well-formed AWS region name (`us-east-1`, `us-gov-west-1`, ...); anything
else is rejected with `400`, because the region becomes part of the
upstream host.

**Role assumption and the external ID.** `AssumeRole` runs with the
gateway's own identity, so letting callers name any role would let one key
holder use a role meant for someone else (the confused-deputy problem).
Role mode is therefore **off by default**: the operator lists the AWS
accounts whose roles may be named in
`llm_proxy.bedrock.allowed_role_accounts` (comma-separated), and any other
or malformed `X-Bedrock-Role-Arn` is rejected with `400`. Each entry is one
of:

| Entry | Matches |
|---|---|
| `123456789012` | that account in the **`aws` partition only** |
| `aws-us-gov:123456789012` | that account in GovCloud |
| `aws-cn:123456789012` | that account in the China partition |
| `*` | every account in **every** partition |

The partition is part of the identity: account `123456789012` in `aws` and
account `123456789012` in `aws-us-gov` are two different AWS accounts owned
by potentially different people, so a bare entry never authorizes a role
ARN outside the commercial partition. The gateway always sends
`ExternalId = <caller's tenant ID>`
and `RoleSessionName = gw-<tenant ID>` (so the role account's CloudTrail
shows which tenant called); a role meant for tenant `T` must require the
external ID in its trust policy:

```json
"Condition": {"StringEquals": {"sts:ExternalId": "<tenant id T>"}}
```

That condition is what binds the role to tenant `T`: a role in an allowed
account that trusts the gateway **without** it can be assumed by any
tenant, so keep the allowlist to tenant accounts and scope the gateway's
own `sts:AssumeRole` permission accordingly. `X-Bedrock-External-Id` is
optional and, if sent, must equal the tenant ID (`400` otherwise); a key
with no tenant cannot use role mode. Assumed credentials are cached per
role/tenant/region and refreshed a minute before expiry, so STS is called
once per credential lifetime, not per request.
Native Bedrock feature headers (`X-Amzn-Bedrock-Guardrail*`, `-Trace`,
`-PerformanceConfig-Latency`) are preserved through a re-sign in modes B/C;
Flow A forwards everything untouched already.

```sh
# Flow C — simplest: gateway signs with its own AWS identity
curl -X POST http://localhost:8082/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/converse \
  -H "X-Gateway-Key: $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":[{"text":"hi"}]}]}'

# Flow B — client-supplied temporary credentials
curl -X POST http://localhost:8082/bedrock/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/converse \
  -H "X-Gateway-Key: $GATEWAY_KEY" \
  -H "X-Bedrock-Access-Key-Id: $AWS_ACCESS_KEY_ID" \
  -H "X-Bedrock-Secret-Access-Key: $AWS_SECRET_ACCESS_KEY" \
  -H "X-Bedrock-Session-Token: $AWS_SESSION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":[{"text":"hi"}]}]}'
```

## Request limits

| Limit | Config key | Default | Response on breach |
|---|---|---|---|
| Max request body | `llm_proxy.limits.max_request_bytes` | 10 MiB | `413` |
| Max total request duration (incl. streaming) | `llm_proxy.limits.max_stream_duration` | 15m | `504` if it hits before the response starts; after that the stream is cut (captured as `stream_deadline`) |
| Max concurrent requests per tenant | `llm_proxy.limits.max_concurrent_per_tenant` | 64 | `429` |

All three limits are enforced by middleware ahead of the router
(`internal/llmplane/server.go`'s `chain`), outermost tag-then-limit order:
tags → `llm.access` permission (`403`) → body cap → per-tenant concurrency →
stream deadline. Errors the plane raises itself (`403`, `413`, `429`, `502`,
`504`) use Anthropic's error envelope on every route. A missing or invalid
gateway key gets `401` before the plane; repeated failures lock the client
IP out of this plane with `429` + `Retry-After` in the route's own dialect
envelope (see
[security-model.md](security-model.md#rate-limiting-and-lockout)).

## Budgets and limits

Per API key, an admin can set `limits` on the key
([api.md, "API key limits"](api.md#api-key-limits)):
`{"daily_usd", "monthly_usd", "rpm", "max_tokens"}`, each optional.
The plane enforces them in the router (`internal/llmplane/limits.go`,
called from `router.ServeHTTP`) after `llm.access` is checked and the
bounded body is read, **before anything is forwarded**. Only callers
authenticated by a gateway API key have key limits; a dev-mode
Principal has no key id. [Model-level limits](#model-level-limits) apply
to every caller.

Checks, in order (the first failure answers):

| Check | Breach | Response |
|---|---|---|
| `max_tokens` | the request asks for more than the limit | `400 invalid_request_error` — never silently clamped |
| `monthly_usd` | spend since the 1st, 00:00 UTC ≥ limit | `429 rate_limit_error` "monthly budget exceeded for this key", `Retry-After` = seconds to the 1st of next month, 00:00 UTC |
| `daily_usd` | spend since 00:00 UTC today ≥ limit | `429 rate_limit_error` "daily budget exceeded for this key", `Retry-After` = seconds to the next 00:00 UTC |
| `rpm` | `rpm` requests already admitted in the last 60 s | `429 rate_limit_error`, `Retry-After: 1` |

Every body is the usual Anthropic envelope, e.g.
`{"type":"error","error":{"type":"rate_limit_error","message":"daily budget exceeded for this key"},"request_id":"…"}`.
`0` is a real limit: `daily_usd: 0` or `rpm: 0` blocks the key on this
plane.

- **Spend** is `SUM(cost_usd)` over the key's rows in the Postgres
  `llm_calls` capture table (`pkg/sink/postgres.Sink.KeySpend`, one
  indexed query for both windows; the sink creates the
  `(tenant_id, key_id, timestamp)` index at startup). A call whose cost is
  unknown (`cost_usd` NULL: model not in the rate card, usage not
  readable) counts as **$0**. Spend is per key id, so a rotated key starts
  at $0.
- **Caching**: a key's limits and spend are re-read at most every **10 s**
  per replica. A call this replica serves adds its cost to the cached
  spend as soon as it is captured, so a replica's own traffic counts at
  once; other replicas' spend is seen on the next refresh. A PATCHed limit
  takes effect within 10 s.
- **Soft by design**: calls already in flight when the budget is crossed
  finish, and a multi-replica deployment can overshoot by up to 10 s of
  the other replicas' spend. The budget stops *further* calls; it is not a
  hard cap on the last one.
- **`max_tokens`** reads the cap from whichever field the dialect uses
  (`max_tokens`, `max_completion_tokens`, `max_output_tokens`,
  `generationConfig.maxOutputTokens`, `inferenceConfig.maxTokens`; the
  largest wins). A request that sets none is not rejected — Anthropic
  requires the field, and elsewhere the provider default applies.
- **RPM** is an exact sliding window **per replica** (a ring of the last
  `rpm` admission times per key, `rpm` ≤ 100000). With N replicas behind a
  load balancer a key can reach up to N × `rpm`; a shared (Redis-backed)
  window is future work.
- **Failure behaviour**: USD budgets need the Postgres capture store
  (`llm_proxy.capture.store: postgres`). Without it a key that **has** a
  USD budget is refused with `503` (logged at startup) rather than served
  unchecked. If the limits/spend read fails, the last cached values are
  used; a key with nothing cached is refused with `503` + `Retry-After: 1`.
- **Visibility**: a refused call is still captured as an `llm_calls` row
  with its status (`429`/`400`/`503`), no cost, and `error` set to
  `budget_exceeded: …`, `rpm_exceeded: …`, `max_tokens_exceeded: …` or
  `limits_unavailable: …`, and `fallback_index: -1` (no target was
  tried), so it appears in LLM Logs. `GET /api/v1/health`
  (and the LLM plane's own `GET /health`) report
  `"limits": {"budget_denials": N, "rpm_denials": N}` since process start.
  In the per-plane deployment the counters live in the `gateway-llm`
  process, so read them from its `/health`.

### Model-level limits

A registered model carries the same `limits` object
(`POST /api/v1/models`, `PUT /api/v1/models/{id}`; column `models.limits`), enforced **after the
registry lookup** and in addition to the calling key's own limits: the key
is checked first, then the model the request names.

- The subject is **(tenant, requested model name)**: spend is
  `SUM(cost_usd)` over the tenant's `llm_calls` rows whose
  `requested_model` is that name (`pkg/sink/postgres.Sink.ModelSpend`,
  index `(tenant_id, requested_model, timestamp)`), whichever key called
  and whichever target answered. The RPM window is per model name, per
  replica. A platform row's limits apply to each tenant separately.
- Messages name the model: `"daily budget exceeded for model <name>"`,
  `"rate limit exceeded for model <name> (requests per minute)"`,
  `"max_tokens N exceeds model <name>'s limit of M"`. The captured row has
  `fallback_index: -1` (no target was tried).
- The limits are the registry row's own, so an edit takes effect as soon
  as the registry cache sees it (at once on the replica that served the
  write, within 30 s elsewhere); the spend they are checked against is
  re-read for the new limits, and then at most every 10 s.
- A disabled row and an unregistered name have no model limits.
- A request the key's limits admit but the model's refuse has already
  taken one of the key's RPM slots.
- `budget_denials` / `rpm_denials` in `/health` count key and model
  denials together.

## Streaming

The plane relays the upstream response to the client via
`io.MultiWriter`, flushing after every write so SSE frames arrive as
produced rather than buffering until the response ends
(`internal/llmplane.newFlushWriter`). A bounded copy is teed off
simultaneously for storage — the client is never blocked on capture. Token
usage is parsed from independent head/tail scan buffers (64 KiB each, one
holding the first bytes for `message_start`, one holding the last bytes
for the final usage frame), so usage/cost accounting stays correct even
when a response body far exceeds the configured capture cap.

## Capture: durable write vs. analytics tee

Every call is recorded through **two independent paths**
(`internal/capture.Recorder`):

1. **Durable capture** (`llm_proxy.capture.store: postgres`, the
   default) — a synchronous, idempotent insert into Postgres `llm_calls`
   (`request_id` primary key, `ON CONFLICT DO NOTHING`), committed
   **before** the request completes. This is a direct write, not a
   background-relayed outbox: if it fails, `Record` returns an error that
   is logged loudly (`"possible data loss"`) — nothing is silently
   dropped. This table is created on demand by `pkg/sink/postgres`
   itself (its own `CREATE TABLE IF NOT EXISTS`), separately from the
   gateway's main migration-managed schema — see
   [architecture.md](architecture.md#data-model). Anything other than
   `"postgres"` for `llm_proxy.capture.store` falls back to best-effort
   stdout, which is **not** durable.
2. **Analytics tee** — the same record is also handed, best-effort, to
   every sink under `sinks.*` (stdout/otel/clickhouse). This feeds the
   console's Overview dashboard, and its LLM Logs page when the ClickHouse
   sink is on (without it, LLM Logs reads Postgres `llm_calls`). It can drop
   under load (a full ClickHouse buffer) independently of step 1
   succeeding.

Because the tee is best-effort, Postgres `llm_calls` and ClickHouse
`llm_calls` can differ briefly under load. Postgres is authoritative for
audit; ClickHouse is authoritative for the dashboard.

Request/response bodies, parsed `messages`/`system`/`tools`, are only
captured when `llm_proxy.capture.store_bodies: true`, each capped at
`llm_proxy.capture.max_request_bytes` / `max_response_bytes` (1 MiB by
default; a cut body is flagged `truncated`) — usage, latency, and
identifiers are always recorded regardless. The caller's `client_ip` (resolved
by the shared client-IP resolver: the socket address, unless that peer is in
`api.trusted_proxies`, in which case `X-Forwarded-For` is walked from the
right to the first untrusted address, as on the MCP plane) is carried on the analytics tee: it is a ClickHouse
`llm_calls` column and appears in stdout JSON lines. It is not stored in
the Postgres ledger.

### Body offload

By default captured bodies are stored inline: Postgres `llm_calls.request_body`
/ `response_body` (`bytea`) and the same columns in ClickHouse. Long agent
sessions make those rows large. `llm_proxy.capture.body_store` moves the raw
bodies to a `sink.BodyStore` instead:

```yaml
llm_proxy:
  capture:
    store_bodies: true
    body_store:
      type: s3                 # none (default) | filesystem | s3
      inline_max_bytes: 0      # calls whose req+resp total <= this stay inline
      s3: { bucket: gateway-llm-bodies, prefix: llm-bodies }
```

- **What moves.** Only `request_body` and `response_body`. Before the
  durable write, the recorder calls `Put(request_id, req, resp)`, sets the
  row's `body_ref`, and clears the two bodies; the row then goes to Postgres
  and the analytics tee as usual. Usage, cost, headers and the parsed
  `messages`/`system`/`tools` summaries stay inline. Postgres `llm_calls`
  remains the audit record; the store holds only its bodies.
- **Ref format.** `fs://<request_id>` for `filesystem` (files at
  `<root>/<request_id[0:2]>/<request_id>.request` / `.response`), and
  `s3://<bucket>/<prefix>/<request_id>` for `s3` (objects `.../request` and
  `.../response`, `application/octet-stream`). An empty `body_ref` means the
  bodies are inline.
- **Fallback to inline.** A failed `Put` (store down, bad credentials, no
  such bucket) never loses the record: the bodies stay inline in that row,
  a warning is logged, and `body_store.fallbacks` on
  [`/api/v1/health`](observability.md#drop-counters-on-health) counts it.
  `Put` gets at most half of the 5 s capture budget, so a hung store
  cannot starve the Postgres write.
- **Reading back.** `GET /api/v1/analytics/llm-logs/{request_id}` resolves
  `body_ref` through the store and returns the bodies in the usual
  `request_body`/`response_body` fields, so the console needs nothing
  different. If this instance has no body store, or the objects are gone
  (e.g. a bucket lifecycle rule expired them), the row still comes back with
  `body_ref` and a `bodies_unavailable` reason. In a per-plane deployment,
  give the API pods the same `body_store` config as the LLM pods.
- Retention of offloaded bodies is the store's job (an S3 lifecycle rule,
  a cron on the directory); the gateway never deletes them.

## Model registry

The model registry lets a client keep calling the endpoint it uses today
(`/v1/messages`, `/openai/v1/...`, `/model/...`, `/gemini/...`) with a
**name of the tenant's choosing** in the request's `model` field, and have
the gateway forward the call to a registered upstream: a different host,
a different credential, a different Bedrock region, a different vendor
model id. Rows live in Postgres (`models`, migration `000002`), are
tenant-scoped, and are managed through
[`/api/v1/models`](api.md#route-table) (or seeded from YAML, below).

```json
POST /api/v1/models
{
  "name": "sonnet",
  "description": "Team default",
  "targets": [
    {"vendor": "anthropic", "model": "claude-sonnet-4-5", "credential": "anthropic-prod"},
    {"vendor": "bedrock",   "model": "us.anthropic.claude-sonnet-4-5-20250929-v1:0", "region": "us-east-1"}
  ],
  "price": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}
}
```

A request naming `"model": "sonnet"` then goes to Anthropic with the
`anthropic-prod` credential; if Anthropic is unreachable or throttled the
same request is re-sent to Bedrock (see fallback below). A name that is
**not registered** behaves exactly as before: byte-for-byte passthrough,
the caller's own key riding along.

**Rows.** `name` is lowercase `[a-z0-9._-]{1,64}`, unique per tenant among
live rows (a soft-deleted name can be re-used). `targets` is an ordered
list of one to eight upstreams; each has a `vendor` (`anthropic`,
`bedrock`, `openai_compat`, `gemini`), the vendor's own `model` id, and
optionally a `base_url` (required for `openai_compat`; for `anthropic` and
`gemini` it defaults to the gateway's configured upstream for that vendor;
`https` only, except plain `http` to loopback hosts and
`host.docker.internal` (a gateway in Docker reaching a model server on the
same machine); no query, fragment or userinfo; a trailing slash is
trimmed. For `openai_compat` it **includes the API version segment**, the
same convention the OpenAI SDKs use for their own base URL -- e.g.
`https://api.groq.com/openai/v1` for a client calling
`/openai/v1/chat/completions`: the client's own leading `/v1` is stripped
before joining, so the upstream request is `{base_url}/chat/completions`,
never `{base_url}/v1/chat/completions` (which would double the version
segment). This applies to the same-dialect OpenAI passthrough only -- a
**translated** target (below) already builds `{base_url}/chat/completions`
directly and needs no stripping, and the gateway's own non-registry
`llm_proxy.providers.openai.base_url` (no target at all) is unaffected: it
does not carry a version segment, so appending the client's path verbatim
is still correct), a `credential` (the name
of a row in the tenant's [credential store](connectors-and-credentials.md);
only ever the name — the value is resolved at call time and never
returned or logged), `allow_caller_key` (see
[Whose key reaches the vendor](#whose-key-reaches-the-vendor)), for
`openai_compat` a `label` (see [Labels](#labels)), and for `bedrock` a
`region` (required). `price`,
when set, is a flat USD-per-million-token rate that overrides the rate
card for calls resolved through this row. `enabled: false` makes the row
behave as if it did not exist. A row with `"scope": "platform"` in a
response is a platform default (created by the YAML seed, `tenant_id
NULL`) every tenant sees; it is read-only through the API — a tenant
overrides it by creating its own row of the same name.

A loopback or `host.docker.internal` `base_url` must also pass the
[egress guard](security-model.md#outbound-requests-ssrf). The compose stack
allows both by default; elsewhere, list them in `egress.allowed_hosts` /
`egress.allowed_cidrs`.

**Resolution.** The plane picks the client's dialect from the path as
before, reads the `model` (body for Anthropic/OpenAI, path for
Bedrock/Gemini), and looks it up for the caller's tenant with a 30-second
per-tenant cache (hits and misses alike; a `/models` write drops the
cache on the replica that served it, other replicas wait out the TTL). On
a hit, the targets the client's dialect can be sent to are used, in order
-- those of the same wire format with only the host, credential, region
and model id rewritten, those of another wire format through translation:

| Client dialect | Target vendor | What is rewritten |
|---|---|---|
| `anthropic` (`/v1/messages*`, `/anthropic/*`) | `anthropic` | host (`base_url`, default `llm_proxy.upstream_base_url`), `x-api-key` from the credential, body `model` |
| `anthropic`, `/v1/messages` only | `bedrock` with an Anthropic model id (`anthropic.…` or `<prefix>.anthropic.…`) | becomes a SigV4-signed `POST /model/{id}/invoke` (or `invoke-with-response-stream`) in the target's region; body loses `model`/`stream`, gains `anthropic_version`; a streamed answer is re-framed from Bedrock's event stream to the SSE the client expects — the events themselves are Anthropic's own, relayed byte-for-byte |
| `openai` (`/openai/*`) | `openai_compat` | host (`base_url`, with the client's leading `/v1` stripped before joining -- see the `base_url` convention above), `Authorization: Bearer` from the credential, body `model` |
| `bedrock` (`/model/*`, `/bedrock/*`) | `bedrock` | model id in the path, region, signing credential |
| `gemini` (`/gemini/*`) | `gemini` | host (`base_url`), `x-goog-api-key` from the credential (a `?key=` query is dropped), model id in the path |
| `anthropic`, `/v1/messages` and `/v1/messages/count_tokens` only | `openai_compat` | **translated** (see [Translated targets](#translated-targets)): the call becomes `POST {base_url}/chat/completions` with a fresh header set and the resolved key as `Authorization: Bearer`; the answer is translated back. `count_tokens` is answered locally with an estimate |

For a same-format target everything else in the request is the client's
own bytes (the top-level JSON members of a rewritten body are re-emitted
in sorted key order; every member's *content* — messages, `cache_control`
markers, tool schemas — is byte-identical, so prompt caching survives).

A target of another wire format is served when `pkg/llm` has an adapter
for both sides — today an Anthropic-dialect client on an `openai_compat`
target. A registered row none of whose targets can be reached either way
is refused before any upstream call with
`400 {"type":"error","error":{"type":"invalid_request_error",
"message":"model <name> requires translation (not available)"}}`: an
OpenAI-dialect client on an Anthropic target (there is no OpenAI dialect
adapter yet), any cross-format pair involving `gemini`, a non-Anthropic
Bedrock model from the Anthropic dialect, and Anthropic paths other than
the two above (Message Batches, the legacy `/v1/complete`, Bedrock
`count_tokens`).

### Whose key reaches the vendor

Decided per target, from the target's
configuration alone:

| Target has | Target host | Key sent upstream |
|---|---|---|
| a `credential` | any | the target's credential; the caller's credential headers are removed |
| no `credential` | the gateway's configured default host for the vendor | the caller's own key (BYOK), as on the passthrough path |
| no `credential`, `allow_caller_key: true` | any | the caller's own key (BYOK) |
| no `credential`, no `allow_caller_key` | any other host | **none — the target is unusable**: it is never called; the next target is tried |
| `bedrock` (any) | `bedrock-runtime.<region>.amazonaws.com` (computed) | caller's `X-Bedrock-*` headers, else the target's credential, else the gateway's own identity |
| **translated**, a `credential` | any | the target's credential, and nothing of the caller's |
| **translated**, no `credential`, `allow_caller_key: true` | any | the caller's key for this vendor (below); **none at all** when the caller sent none |
| **translated**, no `credential`, no `allow_caller_key` | any | **none — the target is unusable** (there is no default host for a translated target) |

The default host is where this gateway already sends that vendor's keys:
the host of `llm_proxy.upstream_base_url` for `anthropic`, of
`llm_proxy.providers.openai.base_url` for `openai_compat`, of
`llm_proxy.providers.gemini.base_url` for `gemini`. A target without
`base_url` goes to that same configured URL. `allow_caller_key` (default
`false`, mutually exclusive with `credential`) is the explicit opt-in for
everything else — without it, anyone allowed to register a model could
point a name callers already use at a host of their choosing and collect
every caller's vendor key. A target for an upstream that needs no
authentication at all (a local Ollama or vLLM) also sets
`allow_caller_key: true`: a caller that sends no vendor key is served, and
the upstream request carries no `Authorization` header. When no target of a row is usable the call is
refused before any upstream call, in the client dialect's own error
envelope: `400 "model <name>: no credential for target <i> (set a
credential or allow_caller_key)"`.

In every case the gateway's own key (`Bearer gk_…`, `X-Gateway-Key`) and
any other vendor's key header (`x-goog-api-key` on an Anthropic call,
`x-api-key` on an OpenAI one, ...) are removed from the upstream request,
and so is every `X-Provider-Key*` header.

**The caller's key for a translated target.** The client speaks one
vendor's format and the target is another vendor, so the key in the
client's usual header is normally not the target's (Claude Code always
sends an Anthropic credential). With `allow_caller_key: true` the key is,
in order:

1. `X-Provider-Key-<label>` when the target has a [label](#labels), else
   `X-Provider-Key` — the key the caller addressed to this vendor. With a
   label only the labelled header is read, so a session that uses several
   vendors sends one header per vendor:

   ```sh
   export ANTHROPIC_CUSTOM_HEADERS=$'X-Provider-Key-groq: gsk_...\nX-Provider-Key-zhipu: ...'
   ```
2. The credential header of the client's own dialect (`x-api-key`, or a
   Bearer `Authorization`) — unless it is the credential that
   authenticated the caller to the gateway (`gk_…`, an OIDC token) or an
   Anthropic credential (`sk-ant-…`, API key or OAuth token). Those never
   leave for another vendor.
3. Nothing: the call goes out without a credential. A vendor that wants
   one answers `401` itself, relayed in the client's envelope.

A target's `credential` is decrypted from the tenant's secret store per
call. For `anthropic`, `openai_compat` and `gemini` the key is its
`api_key` field (or the sole field of a single-field credential), sent as
`x-api-key`, `Authorization: Bearer` and `x-goog-api-key` respectively; for
`bedrock`
the fields `access_key_id`, `secret_access_key` and optionally
`session_token` sign the request. Bedrock keeps its
[credential modes](#bedrock-credential-modes); a request the caller
pre-signed cannot be re-targeted (its signature covers the path being
rewritten) and is refused with `400`. `X-Bedrock-Region`, when sent, wins
over the target's region. Credential values are never logged, captured
or returned by the API.

### Labels

`openai_compat` is one wire format spoken by many vendors. A target's
optional `label` says which one: lowercase `[a-z0-9._-]{1,32}`, anything
but `anthropic` and `bedrock` (the gateway's native wires own those names,
and their rows are priced and region-tagged by wire). Only an
`openai_compat` target takes a label. It is used for three things:

| | With `label: groq` | Without a label |
|---|---|---|
| Capture `resolved_vendor` (the console's Provider column) | `groq` | `openai_compat` |
| Provider the call is priced as | `groq` | `openai` |
| Header a caller sends its key in (translated target, `allow_caller_key`) | `X-Provider-Key-groq` | `X-Provider-Key` |

The rate itself is looked up by the target's `model` id (add the model to
`pricing_file`, or set the row's `price`, if it is not in the embedded
card; otherwise `cost_usd` is `NULL`). The provider decides the token
convention: a call priced as `openai` or `gemini` is stored with cached
tokens **included** in `input_tokens` (OpenAI's convention, and what the
rate card expects for those two); under any other label `input_tokens`
and `cache_read_tokens` are separate counts (Anthropic's split). The
convention follows the label whichever dialect the client spoke.
The adapter also uses the label for vendor quirks: earlier-turn reasoning
is sent back, unsigned, when the label is `deepseek`, `moonshot`, `kimi` or
`zhipu`, when the `base_url` host is (a subdomain of) `deepseek.com`,
`moonshot.ai`, `moonshot.cn`, `z.ai` or `bigmodel.cn`, or when the model id
after its last `/` starts with `kimi-` or `glm-` (Nebius, Together, ...).
It is never sent with label `groq` or to a `groq.com` host, which reject
the field.

### Translated targets

Claude Code (or any Anthropic SDK client) can drive a non-Anthropic model
through the Anthropic route: register a name with an `openai_compat`
target, and a `/v1/messages` call naming it is translated to OpenAI Chat
Completions, sent to the target's endpoint (OpenAI, xAI, DeepSeek, GLM,
Kimi, Mistral, Groq, Gemini's OpenAI endpoint, Ollama, vLLM, ...), and the
JSON or SSE answer is translated back. For a translated target `base_url`
is the API root that `/chat/completions` is appended to. The registry is
the only way to define such a model.

```json
POST /api/v1/models
{"name": "gpt-5", "targets": [
  {"vendor": "openai_compat", "base_url": "https://api.openai.com/v1", "model": "gpt-5", "credential": "openai-prod"}]}

POST /api/v1/models
{"name": "glm-4.6", "targets": [
  {"vendor": "openai_compat", "base_url": "https://api.z.ai/api/paas/v4", "model": "glm-4.6",
   "label": "zhipu", "allow_caller_key": true}]}

POST /api/v1/models
{"name": "local-coder", "targets": [
  {"vendor": "openai_compat", "base_url": "http://host.docker.internal:11434/v1", "model": "qwen3-coder",
   "label": "ollama", "allow_caller_key": true}]}
```

```sh
export ANTHROPIC_BASE_URL=http://localhost:8082
export ANTHROPIC_AUTH_TOKEN=$GATEWAY_KEY
export ANTHROPIC_MODEL=gpt-5
export ANTHROPIC_DEFAULT_OPUS_MODEL=gpt-5
export ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5
export ANTHROPIC_DEFAULT_HAIKU_MODEL=glm-4.6
export CLAUDE_CODE_MAX_OUTPUT_TOKENS=16000       # at most the target model's output cap
export CLAUDE_CODE_AUTO_COMPACT_WINDOW=128000    # the target model's context window
```

Claude Code assumes a 200K window and asks for up to 32000 output tokens for a
model id it does not know; set the last two to the target model's limits or
its vendor rejects the request. A vendor's `context_length_exceeded` is
relayed as Anthropic's "prompt is too long", so Claude Code still compacts.
Claude Code depends on multi-turn tool use; the default
[model catalog](models.md#price-capabilities-and-notes) marks its OpenAI and
Gemini models as not supporting that through translation (Kimi K3 and
GLM 5.3 do).

A model on a hosted OpenAI-compatible provider is the same shape with its
provider's label and API root (`/v1` included: the gateway appends only
`/chat/completions`, so `https://api.openai.com` without `/v1` answers
`404`). The console's model form fills these in (editable) for **OpenAI**,
**Google Gemini** (Google's OpenAI-compatible endpoint), **Nebius** and
**Together AI**, and its **Accept API key** credential option stores the
key as the credential the target names:

```json
POST /api/v1/models
{"name": "kimi-k3", "targets": [
  {"vendor": "openai_compat", "base_url": "https://api.tokenfactory.eu-west2.nebius.com/v1",
   "model": "moonshotai/Kimi-K3", "label": "nebius", "credential": "kimi-k3-nebius"}]}

POST /api/v1/models
{"name": "glm-5.3", "targets": [
  {"vendor": "openai_compat", "base_url": "https://api.together.ai/v1",
   "model": "zai-org/GLM-5.3", "label": "together", "credential": "glm-5.3-together"}]}
```

Slots may point at different vendors, and a slot left on a Claude model still
goes to Anthropic (set `ANTHROPIC_API_KEY` for it; the gateway key is never
forwarded); subagents and background tasks are separate conversations.
Moving one conversation from Claude to a translated model, or between
translated models, works. Moving it from a translated model back to Claude
(`/model`, `opusplan`) is not supported: Anthropic rejects the unsigned
thinking and vendor tool ids in its history (start a new conversation, e.g.
`/clear`).

- **Request.** The vendor gets a fresh header set: `Content-Type`, `Accept`,
  and — when there is a key — `Authorization: Bearer <key>` (see
  [Whose key reaches the vendor](#whose-key-reaches-the-vendor)). No client
  header (`anthropic-beta`, cookies, session ids, `X-Provider-Key*`) and no
  query string (`?beta=true`) is forwarded.
- **Responses.** Only `Retry-After`, `request-id`, `x-request-id` and
  `x-should-retry` of the vendor's headers reach the client; a vendor redirect
  becomes `502`, and a vendor `401`/`403` on the target's own `credential` is
  reported without the vendor's message. Every error envelope carries the
  gateway's `request_id`. Streams get a `ping` every 15 s while the vendor is
  silent, and a stream cut before its finish ends with an `error` event
  (captured as a failure). Streams always request
  `stream_options.include_usage`.
- **`/v1/messages/count_tokens`** is answered locally with an estimate,
  `{"input_tokens": N, "estimated": true}` — another vendor's tokenizer can't
  be reached, and Anthropic does not know the model. `N` is one token per four
  characters of the system prompt, message text, tool inputs and results, and
  tool names, descriptions and schemas (images and PDFs are not counted);
  expect it to be off by tens of percent. The call is captured with status
  `200`, `stop_reason` `estimated`, and no cost.

#### How translation works

A call on a translated target runs through the translation engine
(`internal/llmplane/translate`) over the public seam in `pkg/llm`:

1. The client's dialect (`pkg/llm/anthropic`) parses the body into a neutral
   request (a superset of Anthropic's content blocks).
2. The engine checks every field against the vendor provider's declared
   capabilities (`pkg/llm/openaicompat` for `openai_compat`) **before anything is
   sent**. A field the vendor cannot carry is refused, never silently dropped:
   `400` with Anthropic's envelope, `invalid_request_error`, message
   `unsupported_by_route: <field>` (for example
   `unsupported_by_route: messages[0].content[1] (document source base64)`).
3. The provider builds the vendor request; the answer is translated back —
   JSON whole (capped at 16 MiB), SSE event by event. Text and thinking stream
   through as they arrive; tool calls are held until the vendor finishes and
   then sent whole, in the order they began (vendors may interleave the
   argument fragments of parallel calls, which Anthropic's stream cannot
   express). One upstream stream line, and each held tool call, is capped at
   1 MiB; exceeding it ends the stream with an `error` event.

Text is relayed byte for byte (no trimming); several text blocks of one turn
are joined with a newline, because some of these vendors accept only a string
for system and assistant content. When a vendor names the stop string that
matched (vLLM), the answer's `stop_reason` is `stop_sequence` with that
string; OpenAI does not say, so its matches read `end_turn`. A streamed
`message_start` carries the prompt tokens only if the vendor reports them on
its first chunk; otherwise (OpenAI and most others) they arrive with
`message_delta`, and capture is correct either way.

If a vendor returns tool-call arguments that are not a JSON object, the
`tool_use` block still reaches the client, with the vendor's raw text under
one key: `{"_gateway_invalid_arguments": "<raw text>"}` — never replaced by
`{}`. The tool rejects that input, so the model sees its own mistake and can
retry.

#### Refused, not dropped

A field the vendor cannot carry is refused with `unsupported_by_route`
(`400`, before any upstream call; on a row with several targets the next
target is then tried, so a request only Anthropic can serve still reaches
an Anthropic target further down). For an `openai_compat` target:

| Field | Why |
|---|---|
| `document` blocks with a PDF, URL or file source | Chat Completions has no document input |
| `document` `title`/`context`/`citations` | not expressible |
| images given by `file_id`, or an `image` block holding a non-image | not expressible |
| vendor-defined tools (`web_search_*`, `bash_*`, `text_editor_*`, `code_execution_*`, …) | they run on Anthropic's side |
| unknown tool fields (`input_examples`, …) and unknown `tool_choice` types | not expressible |
| content blocks outside text, image, document, tool_use, tool_result, thinking (`server_tool_use`, `web_search_tool_result`, `search_result`, `container_upload`, …) | not expressible |
| `output_config` keys other than `effort` (structured output `format`) | not expressible |
| any other top-level field (`container`, `mcp_servers`, `service_tier`, `inference_geo`, …) | not expressible |
| `safeguards` (Claude Code's auto-mode safety check) | it runs on Anthropic's side, and dropping it would let the client believe it ran; Claude Code retries without it (its first call of a session shows as a `400` in LLM Logs) |
| `temperature`/`top_p` other than 1 on OpenAI reasoning models (o-series, gpt-5+) | the vendor accepts only the default |

These are **dropped** instead, because they never change what the model is
asked (the complete list; anything not here and not carried is refused):

| Field | Why dropping is safe |
|---|---|
| `metadata` | opaque client metadata |
| `top_k` | sampling cut-off most Chat vendors do not take |
| `cache_control` (request, block, tool) | caching hint; the vendor's automatic caching, if any, is reported as `cache_read_input_tokens` |
| `context_management` | server-side trimming hint; the vendor sees the untrimmed context |
| tool `strict`, `defer_loading`, `eager_input_streaming` | delivery and validation hints |
| `output_config.effort` on a model with no reasoning-effort control | a hint (mapped to `reasoning_effort` on OpenAI reasoning models) |
| a `thinking` budget | reasoning models reason on their own; their reasoning comes back as unsigned `thinking` blocks |
| earlier-turn `thinking`/`redacted_thinking` blocks | signed by, or opaque to, another vendor (unsigned reasoning is sent back to some vendors; see [Labels](#labels)) |
| `citations` on earlier assistant text | annotations; the text itself is sent |
| Claude Code's `x-anthropic-billing-header` system block | client attribution, not instructions |

Text, images (inline and URL), plain-text documents, custom tools, tool
results (with images and `tool_reference`), `tool_choice` (`auto`, `any`,
`tool`, `none`, `disable_parallel_tool_use`), stop sequences, thinking output
and streaming are translated. A new vendor format is one `pkg/llm` provider
package; see [CONTRIBUTING.md, "Adding an LLM provider adapter"](https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/CONTRIBUTING.md#adding-an-llm-provider-adapter).

### Fallback, capture, seed

**Fallback.** Targets are tried in order. A target is skipped for the
next one when it fails **before any byte has reached the client**: the
request could not be built (credential missing), the dial/TLS failed, or
the upstream answered `408`, `429` or any `5xx` (its response is
discarded). Same-format and translated targets mix freely in one row. Once the first target's response headers have been relayed
there is no fallback — a stream that breaks mid-way ends as it always
did. The last target's outcome is what the client gets, whatever it is:
its `429` relayed as-is, or a `502` in the Anthropic envelope when it was
unreachable. A `4xx` other than `408`/`429` is the request's fault and is
relayed from the first target that says so.

**Capture.** Every `llm_calls` row (Postgres and ClickHouse) carries
`requested_model` (what the client sent, always set), `resolved_vendor` /
`resolved_model` (the target that answered — its `label` when it has one,
else its vendor type — empty when unregistered),
`fallback_index` (0-based index of that target in the row's `targets`;
`0` for an unregistered call, `-1` when no target could be tried) and
`translated` (`true` when the body was translated between wire formats).
`provider` stays the client's dialect, and `model` is the vendor model
actually called. `cost_usd` is priced on the resolved model
against the rate card, or the row's `price` when set (flat rates, no
tiers or regional multipliers). The console's LLM Logs rows expose
`requested_model`/`resolved_model`.

**YAML seed.** `llm_proxy.models` (YAML only, no env-var form) upserts
platform rows by name at startup — a GitOps convenience for a
deployment's default aliases. Rows the file does not mention are left
alone; a platform row may not name a `credential` (credentials are
tenant-scoped). The database remains the source of truth.

```yaml
llm_proxy:
  models:
    - name: sonnet
      description: Default Sonnet
      targets:
        - {vendor: anthropic, model: claude-sonnet-4-5}
        - {vendor: bedrock, model: us.anthropic.claude-sonnet-4-5-20250929-v1:0, region: us-east-1}
      price: {input: 3, output: 15, cache_read: 0.3, cache_write: 3.75}
    - name: local-coder
      targets:
        - {vendor: openai_compat, model: qwen3-coder, base_url: "http://host.docker.internal:11434/v1", label: ollama, allow_caller_key: true}
```

## LLM Logs

The console's LLM Logs page (`/llm-logs`) and
`GET /api/v1/analytics/llm-logs` (`analytics.read`) read the ClickHouse
`llm_calls` table when the ClickHouse sink is on, else the Postgres
`llm_calls` capture table (`llm_proxy.capture.store: postgres`), and answer
`404` with neither. The list is filterable by model, session, principal,
client name, status and time range; `GET .../llm-logs/{request_id}`
(`admin.manage`) returns one row including bodies (fetched from the body
store when they were offloaded; see [Body offload](#body-offload)).

## Cost estimates and `pricing_file`

Every `cost_usd` is computed at capture time from parsed token counts and
a static per-model rate card (`pkg/pricing`, embedded from
`pkg/pricing/prices.json`) — **never** a provider-reported invoice
amount. Cache-token accounting is provider-aware (Anthropic/Bedrock
report cache tokens separately from input; OpenAI/Gemini fold them into
the input count — getting this wrong double-bills cache reads), and a
model with an `above_200k` tier bills the whole request at the higher rate
once the true prompt size passes 200K tokens. Anthropic 1-hour cache
writes bill at 2× base input (5-minute writes at the card's
`cache_write`), and each `web_search` server-tool request adds $0.01. For
models flagged `regional_premium` (Claude 4.5+), Bedrock geographic
inference profiles (`us.`/`eu.`/`apac.`/`au.`/`jp.`) bill 1.1× and
GovCloud (a `us-gov.` profile or any call served from a `us-gov-*`
region) 1.2×; `global.` and bare in-region ids in commercial regions bill
the listed rate. A Bedrock rate-card key may also be region-specific — an
exact profile id (`eu.amazon.nova-lite-v1:0`) or `<region>/<id>`
(`us-gov-west-1/amazon.nova-lite-v1:0`) wins over the prefix-stripped id —
for models whose price differs by region.

Provider pricing modifiers are applied from what the response reports,
falling back to the same field in the request body: OpenAI `service_tier`
and Gemini `serviceTier` `priority`/`flex` use the model's
`priority`/`flex` rates (standard when the card has none); Anthropic
`speed: "fast"` and `inference_geo: "us"` multiply every token cost by the
model's `fast_multiplier` / `us_geo_multiplier`. Those two multipliers
apply to exactly the models that carry the field in the rate card — today
`fast_multiplier` 2× on `claude-opus-5-5`, and `us_geo_multiplier` 1.1× on
the Claude 5 family (`claude-fable-5-1`, `claude-opus-5-5`,
`claude-sonnet-5`); a model without the field is unaffected, and a
`pricing_file` override adds or removes it per model. Batch APIs are not
priced (see below).

`cost_usd` is `NULL` — unknown, never a guess — when the model is not in
the card, when a successful response carried no readable usage, and for
free utility endpoints (Anthropic `count_tokens`, Bedrock `count-tokens`,
Gemini `:countTokens`, OpenAI `responses/input_tokens`, Anthropic Message
Batches management/results; batch work is billed by the batch, which
this plane does not see). An error response with no usage is `$0`.

Bedrock token counts come from the response body, falling back to
Bedrock's own model-agnostic counts (the `X-Amzn-Bedrock-*-Token-Count`
InvokeModel headers, or `amazon-bedrock-invocationMetrics` in a stream)
for models whose native body carries none (e.g. Mistral).

**OpenAI streams (opt-in).** A Chat Completions stream reports usage only
with `stream_options.include_usage`, so without it the call's cost is
`NULL`. Setting
`llm_proxy.providers.openai.inject_stream_usage: true` makes the gateway add
`"stream_options": {"include_usage": true}` to a streaming request that
sets no `stream_options` at all (the rest of the body is byte-identical)
and remove the resulting usage-only chunk from the client's stream, so the
call is priced. The client's other chunks carry the `"usage": null` member
OpenAI adds when usage is on; the captured `response_body` is the upstream
stream, usage chunk included.

It is **off by default** because it is the plane's only departure from
byte-for-byte passthrough: it rewrites the upstream request body. Plenty of
OpenAI-*compatible* backends behind `base_url` (vLLM, Ollama, Azure, Bedrock-hosted OpenAI models, …) reject unknown request fields with
a `400`, so a gateway that injected by default would break those
deployments to buy a cost estimate. Turn it on when the upstream is OpenAI
itself, or a backend you know accepts the field, and priced streams follow.
The stripper is also skipped — the stream is relayed verbatim — when the
upstream response carries a `Content-Length`, since removing bytes would
contradict the length the client was promised.

Override the embedded card with `llm_proxy.pricing_file`
(`GATEWAY_LLM_PROXY_PRICING_FILE`), a JSON file in the same shape:

```json
{
  "models": {
    "claude-sonnet-4-5": {"input": 3.00, "output": 15.00, "cache_read": 0.30, "cache_write": 3.75,
                          "regional_premium": true,
                          "above_200k": {"input": 6.00, "output": 22.50, "cache_read": 0.60, "cache_write": 7.50}}
  }
}
```

At startup, the file's models are merged **over** the embedded card
(per-model replace, not whole-card replace — an overridden model must
restate every field it needs, including `regional_premium`, `above_200k`,
`priority`/`flex` and the multipliers); a malformed file fails
startup rather than being silently ignored. `GET
/api/v1/analytics/overview` reports which card is active via
`pricingSource: "embedded" | "file"`, alongside `costEstimated: true`. A
captured call's `cost_usd` is frozen at write time — changing
`pricing_file` later never rewrites already-captured rows, only calls
recorded after the change use the new rate. An unpriced/unknown model
stores `cost_usd` as `NULL` (never a guessed price), logging one `WARN`
per distinct provider/model the first time it's seen.
