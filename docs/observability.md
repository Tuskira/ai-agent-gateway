# Observability

## Sinks

Every MCP request and every LLM call produces one record
(`sink.AccessLog` / `sink.LLMCall`), written to every sink configured
under `sinks.*`, combined into a single `sink.Multi` — an operator can
enable any combination. Sinks never block the request path: writes are
contractually non-blocking (`pkg/sink.LogSink`), doing any I/O
asynchronously.

| Sink | Config | What it does |
|---|---|---|
| `stdout` | `sinks.stdout.enabled` (default `true`), `sinks.stdout.include_bodies` (default `false`) | One JSON line per record on stdout. Captured bodies (`request_body`, `response_body`, `messages`, `system`, `tools`) are **left out** unless `sinks.stdout.include_bodies` is `true`. |
| `otel` | `sinks.otel.*` | One OTLP span per record, exported over http or grpc. Connects lazily — a collector that's down at startup doesn't fail boot. |
| `clickhouse` | `sinks.clickhouse.*` | Batched inserts into `mcp_access_logs`/`llm_calls`. The **only** sink that also backs a read API (`GET /api/v1/analytics/*`) and the console's Overview, Access Logs and Session Timeline pages and the traffic columns of the MCPs, Models and Skills pages. |

> **Privacy.** Bodies are customer prompts, completions and tool arguments.
> With `store_bodies` on they are written to Postgres / ClickHouse / the body
> store (where access is controlled by the admin API) but, by default, not to
> stdout, because container logs are routinely shipped to systems with
> broader access. Turning on `sinks.stdout.include_bodies` together with
> `store_bodies` prints them unredacted and the gateway logs a startup
> warning. LLM bodies are capped at 1 MiB each by default
> (`llm_proxy.capture.max_request_bytes` / `max_response_bytes`, `0` =
> unbounded); a cut body is flagged `truncated` in the console.

ClickHouse is the analytics read source; stdout and OTel are write-only
exhaust. The one exception is LLM Logs, which falls back to the Postgres
capture table when ClickHouse is off (see [Analytics API](#analytics-api)).

## Drop counters on `/health`

`GET /api/v1/health` (control plane) reports each enabled sink's live
state:

```json
{
  "status": "ok", "plane": "api", "version": "0.3.0",
  "sinks": {
    "stdout": {"enabled": true, "dropped": 0},
    "clickhouse": {"enabled": true, "dropped": 3},
    "body_store": {"type": "s3", "offloaded": 1520, "fallbacks": 0}
  },
  "rate_limit": {"locked_ips": 0},
  "limits": {"budget_denials": 0, "rpm_denials": 0}
}
```

`limits` (present only when this process runs the LLM plane) counts calls
refused by per-API-key budgets and requests-per-minute limits since
process start; see [llm-plane.md](llm-plane.md#budgets-and-limits).

`dropped` is a running count of records lost to a full internal buffer
(`sinks.clickhouse.buffer_size`, default 1000) since process start — the
sink drops a record rather than blocking the request that produced it.
This is the only visibility into analytics-tee loss; the LLM plane's
durable Postgres write (see [llm-plane.md](llm-plane.md#capture-durable-write-vs-analytics-tee))
never drops. `otel` reports `enabled`/`endpoint`/`protocol` but no drop
counter (its own SDK batches internally). `body_store` appears only when
`llm_proxy.capture.body_store.type` is not `none`: `offloaded` counts calls
whose bodies went to the store since process start, and `fallbacks` counts
calls whose offload failed and were stored inline instead — non-zero means
the store is unhealthy, though no record was lost (see
[llm-plane.md](llm-plane.md#body-offload)). Both are `0` on an instance
not running the LLM plane. `sinks` is omitted from the
response entirely if no `SinksStatus` closure was wired (never happens in
the shipped binary, but is a valid embedding state).

## Metrics (Prometheus)

Besides the per-request records above, the gateway can export its own
operational metrics. They are off by default (`metrics.driver: none`).
With `metrics.driver: prometheus` the gateway opens a fourth listener,
separate from the three planes, and serves the Prometheus text format
on it:

```yaml
metrics:
  driver: prometheus
  address: ":9464"   # default
  path: /metrics     # default
  namespace: gateway # default: every gateway metric is gateway_*
```

```sh
curl -s localhost:9464/metrics | grep gateway_build_info
# gateway_build_info{go_version="go1.27.1",version="0.3.0"} 1
```

- **The endpoint is unauthenticated.** It carries no request or
  credential data, but it does reveal traffic shape and tenant ids.
  Bind it to a private interface or restrict it with a network policy so
  only your Prometheus can reach it. `api.dev_mode`'s loopback check does
  not cover it.
- The listener also answers `GET /health` (`{"status":"ok"}`), and it
  starts first and stops last, so it can be scraped while the planes
  drain.
- What it serves: `gateway_build_info` (labels `version`,
  `go_version`), the [HTTP metrics](#http-metrics) below, the MCP and LLM
  series listed [below](#metrics-from-access-log-and-llm-call-records),
  the [health and capacity series](#health-and-capacity-series), and the
  standard Go runtime (`go_*`) and process (`process_*`) series.
- **Driver seam.** Metrics go through `pkg/metrics`: the gateway records
  with the OpenTelemetry metric API, and the driver named by
  `metrics.driver` exports them. `prometheus` is a pull driver; a push
  driver (OTLP, StatsD, a vendor agent) registered by a custom binary
  opens no listener. See CONTRIBUTING.md, "Adding a metrics exporter".
- Metrics are aggregates. For per-request detail (who called which tool,
  tokens and cost per call) use the ClickHouse sink and the analytics
  API below. The admin console does not read these metrics.

### HTTP metrics

Every plane (MCP, API and LLM) is wrapped in the same middleware, so each
records three series. With the default namespace:

| Series | Type | Labels |
|---|---|---|
| `gateway_http_requests_total` | counter | `plane`, `method`, `route`, `status` |
| `gateway_http_request_duration_seconds` (`_bucket`, `_sum`, `_count`) | histogram | `plane`, `method`, `route` |
| `gateway_http_requests_in_flight` | gauge | `plane` |

- `plane` is `mcp`, `api` or `llm`. `method` is the HTTP method, with
  `OTHER` for anything that is not a standard method. `status` is the
  HTTP status code the handler wrote (`200` when it wrote a body or
  nothing; `101` for an upgraded connection). A JSON-RPC error on the MCP
  plane is still HTTP `200`: this counts HTTP, not tool outcomes.
- `route` is bounded and never a raw path:
  - **mcp:** `/mcp`, `/mcp/stream`, `/health`, or `other`.
  - **api:** the matched route pattern, such as
    `/api/v1/connectors/{id}` (ids never reach the label). The console's
    static files show as `/*`, and a request no route matched as
    `unmatched`.
  - **llm:** the provider only (`anthropic`, `openai`, `gemini`,
    `bedrock`), `/health`, or `other`. The path carries model ids, so it
    is not used.
- Requests are counted when the handler returns. A server-sent-event or
  streamed LLM response therefore lands in the histogram at the end of
  the stream, with the stream's whole lifetime as its duration; filter on
  `route="/mcp/stream"` to separate those. While a stream is open it is
  visible in `gateway_http_requests_in_flight`.
- Requests rejected before routing (a failed API-key check, a rate limit)
  are counted too, because the middleware is the outermost layer of each
  plane.
- Duration buckets, in seconds: 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
  1, 2.5, 5, 10, 30, 60, 300.

Example queries:

```promql
sum by (plane, status) (rate(gateway_http_requests_total[5m]))
histogram_quantile(0.95, sum by (le, plane) (rate(gateway_http_request_duration_seconds_bucket{route!="/mcp/stream"}[5m])))
```

### Metrics from access-log and LLM-call records

A metrics sink rides the same `sink.Multi` as the storage sinks (it is
added when `metrics.driver` is not `none`), so it sees every MCP access-log
record and every LLM call, including the LLM plane's best-effort copy.
Names below carry the default `gateway` namespace; counters end in
`_total`, and histograms export `_bucket`, `_sum` and `_count`.

| Metric | Type | Labels | Recorded when |
|---|---|---|---|
| `gateway_mcp_requests_total` | counter | `tenant`, `method`, `outcome` | Every access-log record with a JSON-RPC method. |
| `gateway_mcp_tool_calls_total` | counter | `tenant`, `connector`, `tool`, `outcome` | A `tools/call` that was routed to a connector. |
| `gateway_mcp_tool_call_duration_seconds` | histogram | `connector` | Same records, `DurationMS` in seconds. |
| `gateway_mcp_errors_total` | counter | `tenant`, `method`, `error_code` | The response carried a JSON-RPC error. |
| `gateway_llm_calls_total` | counter | `tenant`, `provider`, `model`, `status`, `stream` | Every LLM call that was routed to a provider. |
| `gateway_llm_tokens_total` | counter | `tenant`, `provider`, `model`, `kind` | Per non-zero token kind: `input`, `output`, `cache_read`, `cache_creation`. |
| `gateway_llm_cost_usd_total` | counter | `tenant`, `provider`, `model` | The call was priced (`cost_usd` is not null). |
| `gateway_llm_call_duration_seconds` | histogram | `provider`, `model` | Every LLM call; a streaming call's duration is the whole stream. |
| `gateway_llm_fallbacks_total` | counter | `tenant`, `model` | A registered model was answered by a fallback target (index above 0). |

Label rules:

- `tenant` is the tenant id (a bounded set). A record with no tenant is
  labelled `unknown`. Key id, session id, request id, principal, user and
  connector id (a UUID) are never labels.
- `method` is one of `initialize`, `ping`, `tools/list`, `tools/call`,
  `prompts/list`, `prompts/get`, `resources/list`, `resources/read`,
  `resources/templates/list`, `resources/subscribe`,
  `resources/unsubscribe`, `skills/list`, `skills/get`,
  `completion/complete`, `logging/setLevel`, the known `notifications/*`
  names, `response` (the agent's answer to a relayed request),
  `sampling/createMessage`, `elicitation/create`, `roots/list` (requests a
  connector makes of the agent), `stream` (a `GET /mcp/stream` that ended),
  or `other`. The method on a request is chosen by the caller, so anything
  not listed, including an unrecognised notification, is `other`.
- `outcome` is `rpc_error` when the response carried a JSON-RPC error
  (these arrive with HTTP 200), else `http_4xx` or `http_5xx` by status,
  else `ok`. A tool that ran and reported `isError` is `ok`: the record
  does not carry that.
- `error_code` is one of the codes the gateway produces (`-32700`,
  `-32600` to `-32603`, `-32800`, `-32000` to `-32006`, `-32029`), else
  `other`; a connector or agent can send any code.
- `connector` is the slug before the first `__` of the qualified tool
  name, `tool` the full qualified name. Both are only recorded when a
  connector was resolved, because the name is client-supplied before
  routing. Names without `__` (the gateway's native skills and commands)
  are not tool calls here. At most 500 distinct `tool` values are kept per
  process; the rest, and any name over 128 bytes, are `other`.
- `model` is the name the client requested. A name from the model registry
  is always used; an unregistered name is used only when the upstream
  accepted the call (2xx), otherwise it is `unknown`, so a client cannot
  mint series with made-up names. At most 500 distinct values are kept per
  process, the rest are `other`. `provider` is the client's dialect
  (`anthropic`, `bedrock`, `openai`, `gemini`); `status` is the HTTP status
  as a string; `stream` is `true` or `false`.
- Rows ingested from the interceptor (`source: interceptor`) are not
  counted: they are not this gateway's traffic.
- Token kinds are the provider's own: `input` includes cache reads for
  OpenAI and Gemini but not for Anthropic. Compare them within a provider,
  or add `cache_read` yourself for Anthropic.
- `gateway_llm_calls_total` and the HTTP request counters of the plane
  middleware are different series on purpose. This sink only sees calls
  that reached a provider, so pre-route denials (401, `llm.access`
  denied, body too large, the per-tenant concurrency limit) are in the
  HTTP counters only.
- `GET /mcp/stream` records are written when the stream ends, so they
  count as `method="stream"` in `gateway_mcp_requests_total` but never
  reach a duration histogram. Health probes, 401s and session `DELETE`
  carry no method and are not recorded.

### Health and capacity series

These are read when Prometheus scrapes, from counters the gateway already
keeps, so they add nothing to the request path. Counters are cumulative
since process start and per process: use `rate()` or `increase()`, and
sum across replicas. All names carry the namespace (`gateway_` by
default), and counters end in `_total`.

| Series | Type | Labels | Meaning |
|---|---|---|---|
| `auth_locked_ips` | gauge | `plane` (`api`, `mcp`, `llm`) | Client IPs locked out right now after repeated auth failures. Always `0` when `auth.rate_limit.enabled` is false. |
| `auth_failures_total` | counter | `plane` | Requests that presented a credential the gateway rejected. Requests with no credential are not counted. Counted even when the rate limiter is disabled. |
| `llm_limit_denials_total` | counter | `reason` (`budget`, `rpm`) | LLM requests refused by a per-key limit. Not split by tenant. LLM plane only. |
| `llm_detection_turns_total` | counter | `result` (`sent`, `dropped`, `failed`) | Turns copied to the detection agent: accepted, discarded (queue full, byte budget, shutdown), or refused or unreachable. Present only when `llm_proxy.detection.agent_url` is set. |
| `llm_detection_queue_depth` | gauge |  | Turns waiting to be posted to the detection agent. |
| `sink_records_dropped_total` | counter | `sink` (`stdout`, `clickhouse`) | Records a sink dropped because its queue was full. The OTel sink keeps no count. |
| `body_store_offloads_total` | counter |  | LLM calls whose bodies went to the body store. LLM plane only. |
| `body_store_fallbacks_total` | counter |  | Offloads that failed, so the bodies were stored inline. Non-zero means the body store is unhealthy; no record is lost. |
| `db_connections` | gauge | `state` (`open`, `idle`, `in_use`) | Database pool connections. Postgres store only. |
| `db_wait_duration_seconds_total` | counter |  | Total time callers waited for a database connection. A rising rate means the pool is too small. |
| `mcp_sessions_active` | gauge |  | MCP sessions in this process's session store. Absent with `sessions.store: redis`, which cannot count its sessions cheaply; use `mcp_sessions_created_total` there. Counts expired sessions until the cleanup sweep reclaims them. |
| `mcp_sessions_created_total` | counter |  | MCP sessions created by this process. |
| `mcp_tool_cache_lookups_total` | counter | `result` (`hit`, `stale`, `miss`) | `tools/list` reads of the tool cache: served fresh, served past its TTL (`tool_cache.serve_stale`), or answered by a live fan-out because nothing usable was cached or the read failed. Present only with `tool_cache.enabled`. |

Series for a part of the gateway that is switched off (the LLM plane, the
MCP plane, a sink) are not registered, so they do not appear at all.

## Analytics API

`GET /api/v1/analytics/*` is backed by `pkg/analytics.Reader`, satisfied
by the ClickHouse sink — every route under it answers `404`
(`"analytics requires the ClickHouse sink"`) when ClickHouse isn't
enabled, rather than erroring.

The exception is LLM Logs: without ClickHouse, `GET /analytics/llm-logs`
and `GET /analytics/llm-logs/{request_id}` read the LLM plane's Postgres
capture table (`llm_calls`, written when `llm_proxy.capture.store:
postgres`, the default), with the same pagination, tenant scoping and
filters except `source`/`user` (those describe ingested rows, which only
ClickHouse stores). With ClickHouse enabled, ClickHouse still serves them. Every other
analytics route (overview, models, skills, client-models, MCP access logs,
session timeline, token monitoring) still needs ClickHouse.

| Route | Returns |
|---|---|
| `GET /analytics/overview?range=24h\|7d\|30d` (default `24h`) or `?from=&to=` | KPI tiles, per-model usage, traffic split, top connectors (MCP servers registered with the gateway; the console's **MCPs** page), tools and profiles, latency, status-code breakdown — everything the Overview dashboard renders. |
| `GET /analytics/models?range=24h\|7d\|30d` (default `7d`) | Per-model usage summary — calls, tokens, nullable cost, distinct callers (`used_by`), last-seen — one row per requested model name (`requested_model`), with the vendor that served most of its calls (`resolved_vendor`, else the client's dialect) as `provider`; feeds the console's Models page (`/models`). |
| `GET /analytics/skills?range=24h\|7d\|30d` (default `7d`) | Per skill/command usage of the gateway's own skill loads and native commands (from MCP access logs): calls, distinct callers, last seen. |
| `GET /analytics/skills/usage`, `GET /analytics/mcps/usage` | Skills and MCP servers seen in LLM traffic — see [Discovered skills and MCP servers](#discovered-skills-and-mcp-servers). |
| `GET /analytics/logs` | Paginated MCP access-log rows (no bodies), filterable by `method`, `connector_id`, `tool`, `session_id`, `principal`, `status`, `source` (`gateway`/`interceptor`), `user`, `from`, `to`. |
| `GET /analytics/logs/{request_id}` | One access-log row, including bodies/headers. Admin-only (`admin.manage`, not `analytics.read`). |
| `GET /analytics/llm-logs` | Paginated LLM-call rows (no bodies), including `client_name`/`user_agent`, filterable by `model`, `session_id`, `principal`, `client_name` (see [Client classification](#client-classification)), `status`, `source`, `user`, `from`, `to`. |
| `GET /analytics/llm-logs/{request_id}` | One LLM-call row, including bodies. Admin-only. |
| `GET /analytics/sessions/{session_id}/timeline` | Merged MCP+LLM event timeline for one gateway session, `?order=asc|desc` — see [Session timeline ownership](#session-timeline-ownership). |
| `GET /analytics/client-models?range=24h\|7d\|30d` or `from=&to=`, `metric=calls\|tokens\|cost&limit=&client_name=` (default `7d`, `calls`, limit `10`) | Client → model → provider usage graph — see [Client → model Sankey](#client--model-sankey). |
| `GET /analytics/traffic-flow?range=24h\|7d\|30d&metric=calls\|tokens\|cost&limit=&client_name=` (same defaults) | Agent traffic flow across both planes — see [Agent traffic flow](#agent-traffic-flow). |
| `GET /analytics/token-monitoring?range=24h\|7d\|30d` (default `24h`) or `?from=&to=` | Token usage and cost by model, by caller (API key) and by caller role, with the previous period and a usage series — see [Token monitoring](#token-monitoring). |
| `GET /analytics/token-monitoring/model?model=&range=&limit=&offset=` | One model's usage by caller and by session (sessions paged). |
| `GET /analytics/token-monitoring/keys/{id}?range=&limit=&offset=` | One API key's usage by model and by session. |

**Custom dates.** Overview, client-models, traffic-flow, the token
monitoring routes, `skills`, `skills/usage` and `mcps/usage` also take `from=YYYY-MM-DD&to=YYYY-MM-DD` in place of
`range`: whole UTC days, both inclusive, ending no later than now, at most
366 days. The response's `range` is then `custom`, and deltas compare with
the same number of days just before. A bad or reversed date, a `from` in the
future, or a longer span answers `400`.

`Overview`'s status-code breakdown buckets by **outcome**, not raw HTTP
status: every MCP call answers HTTP 200 whether or not the JSON-RPC call
itself carried an error, so a raw 2xx/4xx/5xx split would show 100% "2xx"
even when most calls failed at the protocol level. The three buckets are
`200` (success), `204` (a notification — excluded from the success-rate
denominator, since it carries no verdict of its own), and `error`
(non-empty error code, or status ≥ 400).

### Client classification

Every request on either plane carries the caller's raw `User-Agent`
(`sink.AccessLog.UserAgent` / `sink.LLMCall.UserAgent`, the latter capped
at 256 bytes), and `sink.ClientFamily` classifies it into one named
family, case-insensitive substring match, first match wins:

| Family | Matches |
|---|---|
| `claude-code` | `claude-cli`, `claude-code` — the real Claude Code CLI User-Agent is `claude-cli/<version> (external, cli)`, not `claude-code/...` |
| `claude-desktop` | `claude-desktop`, `claude desktop`, `claude/` — conservative: Claude Desktop's exact gateway-mode User-Agent isn't published, so this also catches any other `Claude/<version>`-shaped UA |
| `cursor` | `cursor` |
| `vscode` | `vscode`, `visual studio code` — VS Code's native MCP client sends the literal `Visual Studio Code`, with no `vscode` substring |
| `codex` | `codex` |
| `other` | anything else, non-empty |
| `""` | no `User-Agent` header sent at all |

The MCP plane's Overview `requestsByClient` chart uses the same function,
folding `""` into `other` (no separate "no client info" bar); the LLM
plane's `sink.LLMCall.ClientName` keeps `""` as its own value instead, so
a `client_name=` filter can distinguish "no client info" from "a real,
unrecognized client". `GET /analytics/llm-logs` accepts a `client_name`
filter on this same value set (see the route table above).

### Client → model Sankey

`GET /analytics/client-models` aggregates `llm_calls` into a three-column
CLIENT → MODEL → PROVIDER usage graph: nodes are keyed by `key` with a
`type` column, and links reference nodes by `(key, type)` pairs rather
than by `id`. The shape plugs straight into common Sankey libraries —
`@nivo/sankey` (via `id`) and `recharts`' `Sankey` (which the console
uses) — so any dashboard can consume this route directly.

```json
{
  "range": "7d",
  "metric": "calls",
  "total": 1234,
  "nodes": [
    { "id": "CLIENT:cursor", "key": "cursor", "label": "Cursor", "type": "CLIENT", "value": 400 },
    { "id": "MODEL:claude-sonnet-5", "key": "claude-sonnet-5", "label": "claude-sonnet-5", "type": "MODEL", "value": 900 },
    { "id": "PROVIDER:anthropic", "key": "anthropic", "label": "anthropic", "type": "PROVIDER", "value": 900 }
  ],
  "links": [
    { "source": "cursor", "sourceType": "CLIENT", "target": "claude-sonnet-5", "targetType": "MODEL", "value": 300 },
    { "source": "claude-sonnet-5", "sourceType": "MODEL", "target": "anthropic", "targetType": "PROVIDER", "value": 300 }
  ]
}
```

- **Columns.** `CLIENT` is `sink.LLMCall.ClientName` (see [Client
  classification](#client-classification) above), with `""` folded into
  the key `unknown` (label "Unknown") — the same empty-User-Agent bucket
  `client_name=` already distinguishes on `/analytics/llm-logs`. `MODEL` is
  the name the client asked for (`requested_model`, falling back to
  `model`, exactly like `/analytics/models`' `ModelSummaryRow.Name`).
  `PROVIDER` is `resolved_vendor` when set, else `provider` (the client's
  dialect) — the same fallback `/analytics/models` uses for
  `ModelSummaryRow.Provider`.
- **Node id vs key.** `id` is `"TYPE:key"`, unique across the whole
  response (nivo requires a globally unique node id); `key` is unique only
  within its column and is what `Link.source`/`target` reference, so a
  node is identified by its `key`+`type` pair.
- **`limit` / model folding.** Only the top `limit` models by the selected
  metric (default 10) get their own `MODEL` node; every other model's
  traffic — still split across its own clients and providers — folds into
  one synthetic node, key `other-models`, label "Other models". A `MODEL`
  node's `value` is the sum of its incoming (`CLIENT`) links, which always
  equals the sum of its outgoing (`PROVIDER`) links, since every row
  contributes the same value to exactly one link on each side.
- **`metric`.** `calls` (row count, default), `tokens` (input + output +
  cache read + cache creation), or `cost` (`cost_usd`, unpriced rows
  contributing 0 — this route has no nullable-cost field, unlike
  `/analytics/models`' `ModelSummaryRow.CostUSD`, since a Sankey value
  can't be "unknown", only zero).
- Links with `value` 0 are omitted rather than included as a zero-width
  edge; `total` is the metric's sum across the whole window, computed
  before folding, so it equals the sum of every `CLIENT` node's `value`.

### Agent traffic flow

`GET /analytics/traffic-flow` joins both planes into one CLIENT → PATH →
MODEL | CONNECTOR graph, in the same node/link shape as
`/analytics/client-models` (so the same renderer draws either), and takes
the same `range`, `metric`, `limit` and `client_name` parameters with the
same defaults. It backs the console's Overview "Agent traffic flow" card.

```json
{
  "range": "24h",
  "metric": "calls",
  "total": 862,
  "nodes": [
    { "id": "CLIENT:claude-code", "key": "claude-code", "label": "Claude Code", "type": "CLIENT", "value": 620 },
    { "id": "PATH:llm", "key": "llm", "label": "LLM calls", "type": "PATH", "value": 602 },
    { "id": "PATH:mcp", "key": "mcp", "label": "MCP calls", "type": "PATH", "value": 260 },
    { "id": "MODEL:claude-sonnet-5", "key": "claude-sonnet-5", "label": "claude-sonnet-5", "type": "MODEL", "value": 240, "sublabel": "anthropic" },
    { "id": "CONNECTOR:6f1d0c2e-1b7a-4c1e-9a52-0d3c8e2f4b11", "key": "6f1d0c2e-1b7a-4c1e-9a52-0d3c8e2f4b11", "label": "github", "type": "CONNECTOR", "value": 118 }
  ],
  "agents": [
    { "key": "claude-code", "label": "Claude Code", "value": 620 },
    { "key": "cursor", "label": "Cursor", "value": 150 }
  ],
  "links": [
    { "source": "claude-code", "sourceType": "CLIENT", "target": "llm", "targetType": "PATH", "value": 500 },
    { "source": "claude-code", "sourceType": "CLIENT", "target": "mcp", "targetType": "PATH", "value": 120 },
    { "source": "llm", "sourceType": "PATH", "target": "claude-sonnet-5", "targetType": "MODEL", "value": 240 },
    { "source": "mcp", "sourceType": "PATH", "target": "6f1d0c2e-1b7a-4c1e-9a52-0d3c8e2f4b11", "targetType": "CONNECTOR", "value": 118 }
  ]
}
```

- **Columns.** `CLIENT` is the agent family, exactly as on
  `/analytics/client-models` — for the LLM branch it is the stored
  `llm_calls.client_name`; for the MCP branch, `mcp_access_logs` stores
  only the raw `user_agent`, so each distinct header is classified at query
  time with the same `sink.ClientFamily` rule (see [Client
  classification](#client-classification)). One agent is therefore one
  node whichever plane it called through; `""` folds into `unknown` on
  both. `PATH` has at most two nodes, `llm` ("LLM calls") and `mcp` ("MCP
  calls"), always in that order, each present only when its branch has
  traffic. `MODEL` matches `/analytics/client-models` (requested model,
  falling back to `model`) and carries the provider that served most of
  its traffic as `sublabel` (`resolved_vendor`, else `provider`).
  `CONNECTOR` is `mcp_access_logs.connector_id` over `method =
  'tools/call'` rows only (list/initialize traffic is not a "call"); a
  `tools/call` with no `connector_id` — a gateway-native tool such as
  `gateway__skill` — lands on the key `gateway` ("Gateway tools"). The
  handler resolves each connector node's `label` to the connector's
  display name (best effort, like Overview's `topConnectors`); `key` stays
  the connector id, and an id whose connector was deleted keeps the id as
  its label.
- **`limit`** folds each terminal column separately: the top `limit`
  models keep a node and the rest fold into `other-models`; the top `limit`
  connectors keep a node and the rest fold into `other-connectors`. Folded
  nodes have no `sublabel`.
- **`metric`.** `calls` (default) counts `llm_calls` rows on the LLM branch
  and `tools/call` rows on the MCP branch. `tokens` and `cost` are LLM-only
  measures, so those metrics return the LLM branch alone — no `PATH:mcp`
  node and no `CONNECTOR` column — rather than a zero-width MCP path.
- **`agents`.** Every client family with traffic in the window, as
  `{key, label, value}` sorted by `value` desc and summed across both planes
  (MCP counted only under `metric=calls`). It is computed *before*
  `client_name` is applied — it is what the console's agent filter offers,
  so it must not shrink to the selected family — while `nodes`, `links` and
  `total` honour the filter.
- `sublabel` is omitted (never `""`) on every node that has none, and is
  never set on `/analytics/client-models` nodes, whose wire shape is
  unchanged.

## Discovered skills and MCP servers

The gateway records which skills and MCP servers agents **actually used**,
read from LLM traffic, so a console can list the ones that were used but are
not registered ("discovered"). Only names exist: no inputs, outputs or bodies
are stored for this.

**Where it reads.** Only the RESPONSE the model produced, never request
history (history replays every past tool call on each turn and would count one
use many times). `internal/discovery` scans the bytes the gateway relays to the
client, in the client's dialect (a translated response is scanned after
translation), and never affects the relay: it is a tee next to usage parsing,
works with body storage off, and swallows its own errors. Only 2xx responses
are scanned. The interceptor ingest route (`POST /api/v1/ingest`) scans each
record's `response_body` the same way when the interceptor sends one; a record
with no `response_body` contributes nothing.

**Providers and formats.** The format is sniffed from the response bytes, so
the same scanner serves every route and the ingest endpoint. Tool input is
capped at 64 KiB per call or block; an oversized or malformed one is ignored.

| Provider / route | Formats read |
|---|---|
| Anthropic Messages (`/v1/messages`) | JSON `content[]` `tool_use`; SSE `content_block_start` (name) + `input_json_delta` (accumulated per block index) |
| Bedrock `invoke` (Anthropic models) | the same Anthropic JSON |
| Bedrock `invoke-with-response-stream` | AWS event-stream frames; each `chunk` carries `{"bytes": base64 Anthropic event}`, decoded and fed to the Anthropic stream reader |
| Bedrock `converse` / `converse-stream` | JSON `output.message.content[].toolUse`; frames `contentBlockStart` / `contentBlockDelta` (`toolUse.input` fragments) / `contentBlockStop` |
| OpenAI chat completions | JSON `choices[].message.tool_calls`; stream `choices[].delta.tool_calls` |
| OpenAI Responses (`/v1/responses`) | JSON `output[]` `function_call` (`name`, `arguments`) and `mcp_call` (`server_label`, `name`); stream `output_item.added`, `function_call_arguments.delta`/`.done`, `output_item.done` |
| Gemini | `candidates[].content.parts[].functionCall` (`name`, `args`) in `generateContent`, `streamGenerateContent?alt=sse` and the JSON-array stream |

A corrupt event-stream frame (bad CRC) is skipped; a corrupt frame header ends
scanning for that response. Non-Anthropic Bedrock models (Llama, Titan) used
through `invoke` are not read; use `converse` for those.

**Naming caveat.** Detection relies on Claude Code's conventions: a skill is a
tool call named `Skill` with `{"skill": name}` input, and an MCP tool is named
`mcp__<server>__<tool>`. A client that exposes skills or MCP tools under other
names is not detected. The one exception is an OpenAI Responses `mcp_call`,
which names its server (`server_label`) itself. Other function calls are
ignored.

**Extraction rules.**

| Tool name | Recorded as |
|---|---|
| `Skill`, input `{"skill": name}` (also `"command"` or `"name"`) | skill `name`, trimmed and lowercased, a leading `/` dropped, at most 128 chars |
| `mcp__<server>__<tool>` (`^mcp__(.+?)__(.+)$`) | `server__tool`, server lowercased; the server ends at the first `__`, so `mcp__gw__langfuse__get_trace` is server `gw`, tool `langfuse__get_trace` |

Per call: deduplicated, at most 50 skills and 200 MCP refs; malformed input is
ignored.

**Storage.** `sink.LLMCall.SkillsUsed` / `MCPToolsUsed` become ClickHouse
`llm_calls.skills_used Array(LowCardinality(String))` and `mcp_tools_used
Array(String)` and Postgres `llm_calls.skills_used text[]` /
`mcp_tools_used text[]`, all added with `ADD COLUMN IF NOT EXISTS` (older rows
read back empty). The OTLP sink emits counts only (`skills_used_count`,
`mcp_tools_used_count`), never names.

**Endpoints** (`analytics.read`, ClickHouse, `?range=24h|7d|30d`, default `7d`):

- `GET /analytics/skills/usage` returns per skill `name`, `calls`, `used_by`
  (distinct `key_id`), `last_seen`, `registered` (a tenant or platform
  skill/command of that name exists) and `kind` (`skill`, `command` or `""`).
  This is different from `GET /analytics/skills`, which counts the gateway's
  own skill loads from MCP access logs.
- `GET /analytics/mcps/usage` returns per server `server`, `tools`, `calls`
  (sum of per-tool counts), `used_by`, `last_seen`,
  `registered_connector_slug` (the server name matches a tenant connector's
  slug or name, case-insensitive) and `via_gateway`. The gateway's own MCP
  plane names tools `<connector-slug>__<tool>`, so a client alias such as
  `mcp__gw__langfuse__get_trace` with a registered `langfuse` connector is
  attributed to `langfuse` with `via_gateway: true` instead of to `gw`. Rows
  are sorted by `calls`, descending.

A skill or server with `registered: false` / an empty
`registered_connector_slug` and `via_gateway: false` is "discovered". The
registry join is best effort: if the registry lookup fails, every row reads as
unregistered rather than failing the request.

## Session timeline ownership

A gateway session's `session_id` (`sink.AccessLog.SessionID` /
`sink.LLMCall.SessionID`) means something different on each plane:

- **MCP plane**: always the gateway-negotiated `Mcp-Session-Id` — never a
  client header. A caller-supplied `X-Session-Id` or
  `X-Claude-Code-Session-Id` is recorded separately, unvalidated, as
  `client_session_id` (`sink.AccessLog.ClientSessionID`, max 128 chars,
  control characters stripped). It exists purely to correlate an agent's
  MCP calls with its LLM calls (below); it is never trusted as an identity
  or used to look anything up. An earlier version of this code let that
  header override `session_id` directly, which meant any API key could
  write its calls into another session's timeline just by sending that
  session's `Mcp-Session-Id` as its own `X-Session-Id` — this is why the
  split exists.
- **LLM plane**: has no gateway-negotiated session of its own (there is no
  handshake), so it records `X-Session-Id` (falling back to
  `X-Claude-Code-Session-Id`) as `session_id` directly, same as before.

`GET /api/v1/analytics/sessions/{session_id}/timeline` (`analytics.read`)
merges both planes for one MCP `session_id` under a strict ownership rule,
computed server-side (`pkg/sink/clickhouse.Sink.SessionTimeline`) so it
cannot be bypassed by a client that builds its own view from
`/analytics/logs` + `/analytics/llm-logs`:

1. The timeline's own MCP events are every `mcp_access_logs` row with that
   `session_id`.
2. An LLM event belongs to it when its `session_id` equals the MCP
   session's id, **or** equals a `client_session_id` seen on one of that
   session's own MCP events.
3. In every case, the event's `key_id` must also equal `owner_key_id` —
   the key that owns the MCP session (its earliest MCP event's `key_id`).
   Matching by tag (step 1/2) alone is never sufficient: a foreign key
   sending a real MCP/LLM call while claiming another session's tag is
   excluded, not just relabeled. Excluded events are not returned, only
   counted (`excluded_foreign_events`).
4. An **LLM-only** session (a client that only ever calls the LLM plane
   with a given tag, no MCP traffic under it) has no MCP session to own
   it: `owner_key_id` is the `key_id` of the *earliest* LLM event with
   that exact `session_id`, and every other key's events under the same
   tag are excluded the same way.
5. A **client tag** (the id is no MCP session's, but MCP calls carried it
   as `X-Session-Id`/`X-Claude-Code-Session-Id`): the timeline is those
   MCP calls plus the LLM calls with that `session_id`, so an agent that
   sends one id on both planes is found by that id. `owner_key_id` is the
   key of the earliest event on either plane, and every other key's events
   are excluded the same way.

6. **Linked by tool call** (neither of the above joined the other plane):
   an LLM session and an MCP session are joined when the same key made, on
   the MCP plane, a call one of the session's replies asked for, from a
   minute before the conversation's first call to five minutes after its
   last. Claude Code copies the reply's tool-use id (`toolu_…`) into each MCP
   call's `_meta` (`claudecode/toolUseId`), so its calls link to exactly the
   conversation that issued them, even with several running at once on one
   key. A call without that id links by tool name (`mcp__<server>__<tool>`
   in the reply, `<tool>` in the access log); two such conversations on one
   key calling the same tool in the same window cannot be told apart. This
   works from either id and needs stored LLM bodies
   (`llm_proxy.capture.store_bodies`); linking by tool-use id also needs
   stored MCP bodies (`capture.store_bodies`), otherwise calls link by tool
   name. Claude Code (which sends `X-Claude-Code-Session-Id` on model
   requests only) needs no other setup.

A client can also send one tag on both planes, which links them exactly
(the LLM plane prefers `X-Session-Id` over `X-Claude-Code-Session-Id`):

```sh
export GATEWAY_SESSION=$(uuidgen)
# Model requests: Claude Code adds these headers to every model call
# (append to any headers you already set here, one per line).
export ANTHROPIC_CUSTOM_HEADERS="X-Session-Id: $GATEWAY_SESSION"
claude
```

and on the gateway's MCP server entry (`.mcp.json` or `~/.claude.json`,
which expand environment variables in headers):

```json
"headers": { "X-Session-Id": "${GATEWAY_SESSION}" }
```

Search the Session Timeline by that tag to see both planes.

Response shape:

```json
{
  "session_id": "negotiated-or-llm-only-session-id",
  "owner_key_id": "key-abc123",
  "order": "asc",
  "events": [
    {"ts": "2026-09-27T10:00:00Z", "plane": "mcp", "kind": "tools/call", "name": "read_file", "status": "success", "duration_ms": 42, "key_id": "key-abc123", "id": "req-1"},
    {"ts": "2026-09-27T10:00:01Z", "plane": "llm", "kind": "llm_call", "name": "claude-sonnet-4-5", "status": "success", "duration_ms": 810, "key_id": "key-abc123", "id": "req-2"}
  ],
  "total_events": 2,
  "excluded_foreign_events": 0
}
```

Events are sorted server-side by the composite key `(ts, id)` (`id` is the
request id), in the direction of the `order` query parameter: `asc`
(oldest first, the default) or `desc` (newest first). Any other value is a
`400 validation_error`. The response echoes the direction as `order`.
Events with equal timestamps are ordered by `id`, so they never swap
between calls, and `desc` is exactly `asc` reversed. The order changes
nothing else: `total_events`, `excluded_foreign_events` and `owner_key_id`
are identical either way, and clients must not re-sort. When pagination is
added, pages will follow this same `(ts, id)` order in the requested
direction.

`status` is `"success"`, `"error"`, or (MCP only) `"notification"` (a
JSON-RPC notification, status 204), following the same outcome-bucket
convention as `Overview`'s status-code breakdown. An unknown or
never-used `session_id` is not a `404` — it is a valid, empty timeline
(`events: []`, `owner_key_id: ""`).

## Token monitoring

The console's **Token Monitoring** page (`/token-monitoring`) shows who
used how many tokens, on which model, at what cost. It is visibility only:
no limits are enforced from it (the LLM plane's own budgets are in
[llm-plane.md](llm-plane.md#budgets-and-limits)).

Every figure comes from one ClickHouse view, **`llm_usage_canonical`**,
created over `llm_calls` on startup (`pkg/sink/clickhouse/migrate.go`), so
totals, breakdowns and the chart always add up. The ClickHouse user therefore
needs `CREATE VIEW` as well as the table privileges (see
[configuration](configuration.md#sinks)):

| Column | Definition |
|---|---|
| `model` | The name the caller asked for (`requested_model`, else `model`), so a registry alias is one row whatever target answered. |
| `provider` | The provider the call was costed as: the registry target's vendor (`openai_compat` → `openai`, a label as itself), else the client's dialect. |
| `prompt_tokens` | Input tokens **excluding cache reads**. `openai` and `gemini` count cached tokens inside input, so they are subtracted back (never below 0), the same rule pricing uses. |
| `completion_tokens` | Output tokens. |
| `cache_read_tokens`, `cache_write_tokens` | Cache reads and writes, reported separately. |
| `total_tokens` | `prompt_tokens + completion_tokens` — what the page calls **tokens**. |
| `cost_usd`, `priced` | The gateway's estimated cost; `priced` is false for a call with no known cost (shown as unpriced, never as $0). |

Only usage is counted: calls refused before any target (budget, RPM or
`max_tokens` denials), failed calls, and the free token-count and
batch-management endpoints are excluded. Ingested (interceptor) rows are
included and attributed to the interceptor.

The headline **Total tokens** tile counts input + output + cache reads +
cache writes (`totals.tokens_with_cache`, with its own
`tokens_with_cache_delta_pct` against the same measure over the previous
period); every other figure — per model, per caller, per session, the chart
— is `total_tokens` (input + output), with cache shown separately.

**Windows** are **Last 24h**, **Last 7d** and **Last 30d** (rolling,
ending now), or custom dates (`from`/`to`, see below); each is compared with
the period of the same length just before. The change is `null` — shown as
*New* — when the previous period had no usage, rather than 0%. The series is
hourly for windows up to 48 hours and daily otherwise, with empty buckets as
zeros.

**Callers** are API keys, named from the key table, with a role: `agent`,
`admin` (admin and platform-admin keys), `interceptor` (an interceptor
key, or any ingested row), or `other` (viewer keys, keys since deleted,
and calls with no key). Clicking a model or a caller opens its breakdown
and its sessions; a session links to the Session Timeline.

Small values are never shown as zero: a cost under one cent keeps two
significant digits (`$0.0000097`), and a share under 0.1% reads `<0.1%`.

```sql
-- Tokens by model for one tenant over the last 7 days
SELECT model, sum(total_tokens) AS tokens, sum(cache_read_tokens) AS cache_read,
       toFloat64(sumIf(ifNull(cost_usd, 0), priced)) AS cost_usd
FROM llm_usage_canonical
WHERE tenant_id = '<tenant id>' AND timestamp >= now() - INTERVAL 7 DAY
GROUP BY model ORDER BY tokens DESC;
```

## Console pages

The embedded React console (served at `/` on the API plane when
`api.serve_ui: true`) has these routes (`web/src/lib/nav.ts`):

| Page | Path | Reads |
|---|---|---|
| Overview | `/` | `GET /analytics/overview` |
| MCPs (incl. Catalog) | `/connectors` | `/connectors`, `/connectors/{id}/health`, `/connectors/{id}/discover`, `/mcp-catalog`, `/analytics/mcps/usage` |
| Models | `/models` | `/analytics/models`, `/models`, `/credentials`, `/model-catalog` |
| Model catalog | `/model-catalog` | `/model-catalog` |
| Profiles (incl. Profile Studio) | `/profiles` | `/profiles`, `/profiles/{id}/tools`, `/profiles/{id}/skills` |
| Skills | `/skills` | `/skills`, `/analytics/skills/usage`, `/analytics/skills` |
| Access Logs | `/access-logs` | `/analytics/logs` |
| LLM Logs | `/llm-logs` | `/analytics/llm-logs` |
| Session Timeline | `/session-timeline` | `/analytics/sessions/{id}/timeline` |
| Token Monitoring | `/token-monitoring` (and `/token-monitoring/model`, `/token-monitoring/keys/{id}`) | `/analytics/token-monitoring`, `/analytics/token-monitoring/model`, `/analytics/token-monitoring/keys/{id}` |
| Tool Search | `/tool-search` | `/cache/search` |
| Cache | `/cache` | `/cache/stats`, `/cache/refresh`, `/cache` (invalidate) |
| API Keys | `/api-keys` | `/api-keys` |
| Credentials | `/credentials` | `/credentials` |
| Users (admins only) | `/users` | `/users`, `/auth/audit` |

The MCPs, Models and Skills pages give each row a lifecycle state:
**Available** (MCPs and Models only: in the platform catalog, not set up
for this tenant),
**Registered** (enabled, no traffic in the selected range), **Active**
(enabled, with traffic), **Disabled** (turned off; on the MCPs and Skills pages this wins over
traffic, while the Models page still shows a disabled model with traffic
as Active) and
**Discovered** (seen in LLM traffic but not registered; see
[Discovered skills and MCP servers](#discovered-skills-and-mcp-servers)).
Traffic comes from ClickHouse, so without it only Available, Registered
and Disabled appear.

The Overview, Models, Access Logs, Session Timeline and Token Monitoring pages
need the ClickHouse sink for their usage data (Models' `/models` registry
call still works on Postgres alone; only its traffic columns need
ClickHouse) — everything else, LLM Logs included (it falls back to the
Postgres capture table), works against Postgres alone. Sign-in
(`/login`) is a console username and password; an **admin** API
key (`gk_...`) is accepted as a fallback while
`auth.console_api_key_login` is on (the default). Agent keys can't open
the console; they are for MCP/LLM calls. This is a console check: the API still honors each
role's permissions.

Log rows show the calling key as `name (prefix)`, so one key per person
or device gives per-user/per-device attribution. Session Timeline is
served by `GET /api/v1/analytics/sessions/{id}/timeline`, which merges
both planes server-side under a strict ownership rule — see
[Session timeline ownership](#session-timeline-ownership) below.

## Traceparent propagation

On the MCP plane, the gateway implements the W3C Trace Context grammar
(`pkg/trace`) for correlation, not a full OTel SDK: an inbound `traceparent` header is
parsed and validated strictly (a malformed one starts a fresh trace rather
than being forwarded, since it's echoed into logs and to backends); a
fresh trace/span id is minted when none is present. The same trace id
flows through the access-log record (`TraceID`) and the outbound backend
call's own `traceparent` header (`internal/dataplane/client.exchange`
sets it via `call.Trace.Traceparent()`), so one trace id ties the inbound
request, the log row, and the outbound backend call together. The LLM
plane does not parse `traceparent` and records no trace id.

## Compose `analytics` profile

The default `docker compose up` starts only Postgres + the gateway
(stdout sink; `/api/v1/analytics/*` answers 404, except LLM Logs, which
reads Postgres). ClickHouse and Adminer
(a Postgres browser on `:8090`) are gated behind the `analytics` Compose
profile:

```sh
GATEWAY_SINKS_CLICKHOUSE_ENABLED=true \
  docker compose -f deploy/docker-compose.yml --profile analytics up --build -d
```

## Sample ClickHouse queries

Against `mcp_access_logs` (columns: see `pkg/sink/clickhouse/migrate.go`) and
`llm_calls`, both `ORDER BY (tenant_id, timestamp, request_id)`, 90-day TTL:

```sql
-- Slowest MCP tool calls in the last hour, one tenant
SELECT tool_name, connector_id, duration_ms, timestamp
FROM mcp_access_logs
WHERE tenant_id = '<tenant-id>' AND timestamp > now() - INTERVAL 1 HOUR
ORDER BY duration_ms DESC
LIMIT 20;

-- Error rate by JSON-RPC method, last 24h
SELECT method,
       countIf(error_code != '') AS errors,
       count()                    AS total,
       round(100 * errors / total, 1) AS error_pct
FROM mcp_access_logs
WHERE tenant_id = '<tenant-id>' AND timestamp > now() - INTERVAL 1 DAY
GROUP BY method
ORDER BY total DESC;

-- LLM spend by model, last 7 days
SELECT model, sum(cost_usd) AS cost, sum(input_tokens + output_tokens) AS tokens, count() AS calls
FROM llm_calls
WHERE tenant_id = '<tenant-id>' AND timestamp > now() - INTERVAL 7 DAY
GROUP BY model
ORDER BY cost DESC;
```
