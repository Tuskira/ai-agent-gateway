# Architecture

## One binary, three planes

`cmd/gateway` starts up to three independent HTTP listeners from one
process, one `Config` (`internal/config`), and one Postgres connection.
Each plane is gated by its own `enabled` flag and each has its own listen
address, so a deployment can run all three, or any subset, on one binary:

| Plane | Default address | Enable flag | Package |
|---|---|---|---|
| MCP (tool-server proxy) | `:8080` | `mcp.enabled` (`GATEWAY_MCP_ENABLED`) | `internal/dataplane` |
| API (control plane + console) | `:8081` | `api.enabled` (`GATEWAY_API_ENABLED`) | `internal/api` |
| LLM proxy | `:8082` | `llm_proxy.enabled` (`GATEWAY_LLM_PROXY_ENABLED`) | `internal/llmplane` |

`Config.Validate` refuses to start if two enabled planes share an address,
and if a plane is disabled its listener is simply never started — with
all three off it opens no listener: it stays up running only its
background API-key revocation listener (API keys on, the default), or
otherwise logs `"no planes enabled; nothing to serve"` and exits 0. Each
plane's `Deps` struct is separately constructed in
`cmd/gateway/main.go`'s `run()`, so planes have no compile-time dependency
on each other; they share only the collaborators listed below.

```
                         cmd/gateway (one process)
        ┌───────────────────────┬───────────────────────┬───────────────────────┐
        │  MCP plane :8080      │  API plane :8081       │  LLM plane :8082      │
        │  internal/dataplane   │  internal/api          │  internal/llmplane    │
        │  POST /mcp            │  /api/v1/*  + console  │  /v1/messages         │
        │  GET  /mcp/stream     │                        │  /{provider}/*        │
        │  DELETE /mcp          │                        │  /model/*             │
        └───────────┬───────────┴───────────┬────────────┴───────────┬───────────┘
                     │                       │                        │
                     └──────────┬────────────┴─────────┬──────────────┘
                                 │                       │
                      shared core (auth, secrets, store, sinks)
                                 │                       │
                        Postgres                 ClickHouse (2 tables, optional)
```

Because each plane has its own enable flag and address, an operator
can also run three separate deployments of the same image (MCP-only,
API-only, LLM-only) behind three different services — toggling only
`GATEWAY_MCP_ENABLED`/`GATEWAY_API_ENABLED`/`GATEWAY_LLM_PROXY_ENABLED`;
see [configuration.md](configuration.md)'s single-plane examples for the
bare env-var form, or [`deploy/README.md`](https://github.com/Tuskira/ai-agent-gateway/blob/main/deploy/README.md) for the
`deploy/k8s` shape of the same idea (one image, three Deployments —
`gateway-mcp`/`gateway-api`/`gateway-llm`). This works because `main.go`'s
`run()` builds the MCP **data plane** whenever `mcp.enabled`
OR `api.enabled` is true — the API plane's health/discover/cache routes
for connectors (MCP servers registered with the gateway; the console's
**MCPs** page) come from it (`pkg/ops.ConnectorOps`/`CacheOps`) — but only opens
the `:8080` listener and starts its session/tool-cache background routines
when `mcp.enabled` is true, so a `gateway-api` pod with
`GATEWAY_MCP_ENABLED=false` still serves connector health/discover.
`gateway-mcp` runs `replicas: 1` in the base, because the default `memory`
session store keeps sessions in-process; the `redis` overlay
(`deploy/k8s/overlays/redis`, which adds the `deploy/k8s/components/redis`
component) moves them to a Redis-protocol server (a
bundled Valkey pod) and runs two replicas. `api`/`llm` scale via HPA, and
the NetworkPolicy's egress allowlist (DNS, Postgres, ClickHouse, and TCP
80/443 to public addresses only, plus 6379 to the session store with the
`redis` component) covers all three planes, since api pods dial out for
connector health/discovery too.

## Shared core

Every plane is built over the same collaborators, constructed once in
`main.go` and passed in as `Deps`:

- **Auth** (`internal/auth`, `pkg/auth`) — the `Authenticator` chain (API
  keys, optionally dev-mode; the API plane also accepts console session
  cookies) and the `RoleAuthorizer`. The MCP and LLM planes wrap their
  handler in `internal/auth.Middleware`; each plane has its own failed-auth
  limiter (per IP and per key prefix), and the API plane adds a
  requests-per-minute cap. The LLM plane also gates on
  `llm.access` inside its own chain — see the LLM call flow below.
- **Secrets** (`internal/secrets`) — the AES-256-GCM credential store and
  the header-resolver `Registry` (`internal/dataplane/headers`) that
  connector configs and the API's `/credentials` routes both use.
- **Store** (`pkg/store`, `internal/store/postgres`) — the one Postgres
  connection, opened once and shared by every plane and by `gateway
  migrate`/`bootstrap-key`/`secrets rekey`.
- **Sinks** (`pkg/sink` and its `stdout`/`otel`/`clickhouse` siblings) —
  combined into one `sink.Multi` that both the MCP plane's access log and
  the LLM plane's analytics tee write through.
- **Client IP** (`internal/clientip`) — one resolver (`api.trusted_proxies`)
  behind every plane's limiter and every captured `client_ip`.
- **Egress guard** (`internal/netguard`) — every outbound dial (connectors,
  catalog probes, model targets, LLM upstreams) is checked against
  `egress.allowed_hosts` / `egress.allowed_cidrs`; see
  [configuration.md](configuration.md#egress-outbound-request-policy).

## Seams in `pkg/`: how a plugin extends the gateway

`pkg/` is the only part of this module a *separate* Go module (a
plugin) can import — everything under `internal/` is unreachable from
outside this module by Go's own visibility rules. The gateway defines
these seams, each a small interface with one or more open-source
implementations:

| Seam | Interface | Shipped implementation | Used by |
|---|---|---|---|
| Authorization | `pkg/auth.Authorizer` | `pkg/auth.RoleAuthorizer` | every plane's permission checks |
| Log sinks | `pkg/sink.LogSink` / `BatchSink` | `pkg/sink/{stdout,otel,clickhouse,postgres}` | access log, LLM capture |
| Outbound headers | `pkg/headers.ExternalProvider` | `internal/secrets` (`secret_store`, `env`, `file`) | connector credential injection |
| Persistence | `pkg/store.Store` (driver registry) | `internal/store/postgres` | every plane |
| MCP sessions | `pkg/session.Store` + `Notifier` (driver registry) | `pkg/session/memory`, `pkg/session/redis` | MCP plane's session manager and cross-replica notifications (`tools/list_changed`, per-session messages) |
| Connector/cache/profile ops | `pkg/ops.ConnectorOps` / `CacheOps` / `ProfileOps` | `internal/dataplane`'s `opsAdapter` | API plane's health/discover/cache routes and profile-cache invalidation |
| Analytics read | `pkg/analytics.Reader` / `LLMCallReader` | `pkg/sink/clickhouse.Sink`; `pkg/sink/postgres.Sink` (`LLMCallReader` only) | `GET /api/v1/analytics/*`; LLM Logs without ClickHouse |
| Body offload | `pkg/sink.BodyStore` | `pkg/sink/bodystore/{fs,s3}` | LLM capture (`llm_proxy.capture.body_store`), LLM-log detail |
| LLM translation | `pkg/llm.Dialect` / `Provider` (registry) | `pkg/llm/anthropic`, `pkg/llm/openaicompat` | model-registry targets in another wire format |
| Ingest | `pkg/sink.IngestSink` | `pkg/sink/clickhouse.Sink` | `POST /api/v1/ingest` |

The LLM plane's detection tee (`llm_proxy.detection`) is not a
`pkg/sink.LogSink`: a sink only sees the `LLMCall` record, whose bodies are
cut to the capture cap and absent when `store_bodies` is off, while the
detection agent needs the full request body. It lives beside the recorder
in `internal/llmplane` and gets the body the router already holds.

A plugin implements one of these interfaces, registers it (a driver name
for `pkg/store.Register` or `pkg/session.Register`, or is wired directly
into the relevant `Deps` struct in a fork of `main.go`), and the rest of
the gateway is unaware it's not the built-in one. For example, adding a
SQLite store backend means implementing `pkg/store.Store` and passing
`pkg/store/storetest`'s conformance suite — no change to any plane.
Likewise a session backend implements `pkg/session.Store` (and
`pkg/session.Notifier` if it can carry a broadcast), passes
`pkg/session/sessiontest`, and is selected with `sessions.store: <name>`.
See [CONTRIBUTING.md](https://github.com/Tuskira/ai-agent-gateway/blob/main/CONTRIBUTING.md) for the concrete steps.

The session seam is what lets the MCP plane scale out. `internal/
dataplane/session.Manager` keeps the session logic — minting the id,
sliding the idle window, the tenant check, the per-connector backend
handles — and persists a plain `pkg/session.Record` through whichever
`Store` `sessions.store` names, converting on every read and write. The
`memory` driver (default) hands out copies, so the same Save discipline
that a serializing backend needs is enforced under the default too. With
the `redis` driver any replica can serve any session, and the driver's
`Notifier` (Redis pub/sub) relays two kinds of message between replicas:
`tools/list_changed` (channel `gw:tools_changed`) — `transport.Bridge`
signals the local SSE hub directly and publishes for the others — and
messages for one session's stream (channel `gw:session_msgs`) —
`Bridge.Notify` delivers to the session's local streams and publishes
only when there are none here (see
[Upstream streams and subscriptions](#upstream-streams-and-subscriptions)).
A `Notifier` never delivers a publish to its own subscriber, so each
stream receives a message exactly once.

## Request flow: an MCP tool call

```
Client              MCP plane (:8080)                        Backend connector
  │  POST /mcp                │                                      │
  │  Mcp-Session-Id?           │                                      │
  │  X-Agent-Profile-Name?     │                                      │
  ├──────────────────────────►│                                      │
  │                            │ 1 authenticate (api key)             │
  │                            │ 2 authorize (mcp.access)             │
  │                            │ 3 resolve session (or run stateless) │
  │                            │ 4 resolve agent profile → allow-list │
  │                            │ 5 route "<connector>__<tool>"        │
  │                            │ 6 check allow-list for this call     │
  │                            │ 7 stamp tool_arg_overrides           │
  │                            │ 8 resolve connector headers          │
  │                            ├─────────── tools/call ──────────────►│
  │                            │◄────────── result / error ───────────┤
  │                            │ 9 on 401: evict creds, replay once   │
  │                            │ 10 on timeout: TOOL_TIMEOUT content  │
  │                            │ 11 write AccessLog to sink           │
  │◄──────────────────────────┤                                      │
  │  200 + JSON-RPC result     │                                      │
```

1. `internal/auth.Middleware` / `transport.AuthMiddleware` resolves the
   `Authorization`/`X-Gateway-Key` header to a `Principal` (401 with
   JSON-RPC code `-32001` on failure).
2. The `RoleAuthorizer` checks the `mcp.access` permission (403 / `-32006`
   if the role lacks it).
3. If `Mcp-Session-Id` is present, `session.Manager.Resolve` reads it
   from the configured `pkg/session.Store` (`memory` by default; `redis`
   for more than one replica), checks tenant and expiry, and slides the
   idle window with a Save (unknown/expired/other tenant → `-32000`,
   "re-initialize"); otherwise the call runs against the process-wide
   anonymous backend-handle store. After the call, any backend handle the
   request changed (a lazy handshake, a rotated backend session id, a
   dropped one) is saved back through the same Store.
4. If the API key is bound to a profile (`profile_id`),
   `orchestrator.bindProfile` uses that profile; a header naming a
   different one, or a binding whose profile was deleted, is `-32003`.
   Otherwise `orchestrator.resolveProfile` resolves `X-Agent-Profile-Name` (if
   present) to an `AllowList`; a header naming no profile grants every
   tool in the tenant unless `mcp.require_profile` is on, in which case
   it's required (`-32003`).
5. `router.Resolve` splits `"<connector>__<tool>"` and looks the connector
   up by slug (falling back to name) within the tenant; a missing or
   disabled connector is `-32004`.
6. `AllowList.Allows(connectorID, toolName)` is checked *after* routing,
   since the grant is per (connector, tool) pair.
7. `router.StampOverrides` merges the connector's
   `metadata.tool_arg_overrides` into the call arguments, overwriting
   anything the caller supplied for those names.
8. `client.applyConnectorHeaders` resolves each configured header
   (static/token_field/incoming_field/external) and writes it onto the
   outbound request, after which the gateway-owned `X-Tenant-Id` and
   `Mcp-Session-Id` are set last so no connector config can override them.
9. A `401` from the backend evicts that connector's cached credentials (if
   the provider implements `Invalidate`) and replays the call once.
10. A timeout is reported as tool **content** (`TOOL_TIMEOUT: …`,
    `isError: true`), not a protocol error — so the calling model sees
    actionable text instead of a swallowed error.
11. One `sink.AccessLog` is written per request, regardless of outcome.

### Served MCP methods

| Method | What the gateway does |
|---|---|
| `initialize` | mints a session; handshakes every connector concurrently (5s each); advertises `tools` always, `prompts`/`resources` when at least one reachable backend has them (or the profile has an attached command), and each of `resources.subscribe`, `resources.listChanged`, `prompts.listChanged` when at least one reachable backend reports it; `instructions` is the resolved profile's own text, then the static pointer text, then a capped skill index when it has attached skills (see [profiles.md](profiles.md)) |
| `notifications/initialized`, `ping` | answered locally |
| `tools/list` | profile-filtered union of every connector's tools, from the Postgres tool cache when warm, plus the native `gateway__skill` tool when the profile has ≥1 attached skill |
| `tools/call` | routed by `<connector>__<tool>`, as above; `gateway__skill` is intercepted first and never routed (see below) |
| `prompts/list` | live fan-out, merged, names `<connector>__<prompt>`, plus the profile's attached commands as native prompts (plain names, no `__`) |
| `prompts/get` | a name with no `__` is a native command, rendered from the profile's attachment (see below); otherwise routed by `<connector>__<prompt>`, the un-prefixed name and the arguments forwarded verbatim, the result returned verbatim |
| `resources/list` | live fan-out, merged, URIs wrapped as `gw://<connector>/<backend uri>` |
| `resources/templates/list` | live fan-out, merged, `uriTemplate` wrapped the same way |
| `resources/read` | a `gw://<connector>/` URI strips the prefix and forwards the backend's own URI, result returned verbatim; a `skill://<name>/<path>` URI is served by the gateway itself (see below) |
| `resources/subscribe`, `resources/unsubscribe` | on a session only; routed like `resources/read`, forwarded with the backend's own URI; updates relayed to the session's stream (see [below](#upstream-streams-and-subscriptions)) |
| `skills/list`, `skills/get` | MCP Skills Extension (SEP-2640); describe the skills attached to the caller's agent profile, served entirely by the gateway (see below) |

Anything else is `-32601`, including `completion/complete` and
`logging/setLevel`. `POST /mcp` also accepts a JSON-RPC **response** (an
`id` and a `result` or `error`, no `method`): the agent's answer to a
request relayed to it from a connector, acknowledged `202` with no body
(see [Server-initiated requests](#server-initiated-requests-sampling-elicitation-roots)).

**Namespacing.** Tools and prompts share one rule: `<connector
slug>__<name>`, split on the first `__`. Resource URIs cannot take a
prefix that way -- they already carry a scheme of their own
(`file://`, `test://`, `postgres://`) and are opaque to the gateway -- so
the whole URI is wrapped: `test://static/resource/1` on the `everything`
connector is advertised as `gw://everything/test://static/resource/1`,
and stripping `gw://everything/` gives back the backend's bytes exactly.
A client that expands a wrapped `uriTemplate` gets a URI that routes the
same way. URIs *inside* results (`contents[].uri` of a read, an embedded
resource in a prompt message) are the backend's own and are not rewritten.

**Prompts and resources vs. tools.**

- *Profile scope.* A connector's prompts and resources are visible and
  usable exactly when the caller's allow-list grants at least one tool of
  that connector (`profile.AllowList.AllowsConnector`); with no profile
  and `mcp.require_profile` off, everything is. A denied `prompts/get` or
  `resources/read` is `-32003`. See [profiles.md](profiles.md).
- *No cache.* Both families are fetched live on every request; they are
  not written to the Postgres tool cache.
- *No gateway pagination.* Each `*/list` follows every backend's
  `nextCursor` to the end and returns one merged page with no
  `nextCursor`; an inbound `cursor` is ignored.
- *Skipped connectors.* As with `tools/list`, a connector in its
  unhealthy cool-down is skipped, one whose handshake reported no such
  capability is not asked, and one whose list fails is logged and left
  out without failing the whole answer.
- *Errors.* A JSON-RPC error the backend itself answers with (an unknown
  prompt, `-32002` resource not found) is forwarded unchanged and does
  not mark the connector unhealthy; a transport failure is `-32002` and
  does, as for `tools/call`. A timeout is a `-32002` protocol error --
  there is no tool content to carry `TOOL_TIMEOUT` in.
- *Access log.* `prompts/get` and `resources/read` are logged like
  `tools/call`, with `tool_name` set to the namespaced prompt name or the
  namespaced (`gw://`) URI and `connector_id` to the connector served.
- *Capabilities.* `resources.subscribe`, `resources.listChanged` and
  `prompts.listChanged` are each advertised only when a reachable backend
  reports them: the gateway relays what its backends send and promises
  nothing none of them will.

**Skills and commands (native, never forwarded).** A profile's attached
rows from the [skills & commands registry](skills.md) are served
entirely by the gateway itself, not proxied to any connector:

- `gateway__skill` (a `tools/call`) loads one attached skill's file
  content (`{name, path?}` → `{content:[{type:"text", text}]}`),
  intercepted before routing -- `"gateway"` is a reserved connector
  name/slug so a real connector's tools can never collide with it.
- A `prompts/get` name with no `__` is a native command: its
  `SKILL.md` body is rendered with `{{arg}}` substitution
  (`internal/skills.RenderTemplate`) and returned as one `user` message.
- Both deny with `-32003` (`{skill, profile}`) when the name isn't
  attached to the resolved profile; `gateway__skill` denies an unknown
  `path` with `-32602`, and a native command denies a missing required
  argument the same way.
- Both are logged like `tools/call`/`prompts/get`, with an additional
  `skill_name` field (`sink.AccessLog.SkillName`, ClickHouse
  `mcp_access_logs.skill_name`) naming the skill or command.

See [profiles.md](profiles.md), "Skills and commands", for the full
shapes and the cache-invalidation contract.

### MCP Skills Extension (SEP-2640)

`skills/list` and `skills/get`, plus `resources/read` under
`skill://<name>/<path>` URIs, implement the
[MCP Skills Extension](https://modelcontextprotocol.io/extensions/skills/overview)
for the skills (not commands) attached to the caller's agent profile. Unlike
prompts and resources, skills never route to a connector: they are resolved
and served by the gateway itself, by `internal/dataplane/skillsext`, against
`pkg/store.SkillStore`/`AgentProfileStore.GetSkills` directly -- independently
of `internal/dataplane/profile`'s tool allow-list, with the same 30s cache.

- *Capability.* `initialize` advertises `resources: {}` and
  `extensions: {"io.modelcontextprotocol/skills": {}}` only when the resolved
  profile has at least one attached, enabled skill; a resolution failure here
  degrades to "not advertised" rather than failing the handshake. No profile
  header, an unknown profile, or a profile with no attachments all advertise
  nothing -- "no profile header -> no skills" is unconditional here, unlike
  tools' `mcp.require_profile`-gated fallback to every tool in the tenant.
- *Shape.* Both `skills/list` and `skills/get` answer with
  `{"uri": "skill://<name>/SKILL.md", "frontmatter": {...}, "resources": [{"uri", "digest": "sha256:<hex>", "size"}, ...]}`
  per skill, with SKILL.md always first in the manifest; `resources/read`
  returns the file's text with `mimeType` `text/markdown` (`.md`) or
  `text/plain` (everything else the registry allows). All three carry
  `"resultType":"complete"`, `"ttlMs":30000` and `"cacheScope":"private"`
  (never `"public"`: every response depends on the caller's agent profile).
  `skills/list` paginates with a decimal-offset `cursor`/`nextCursor`
  (page size 50).
- *Errors.* A `skill://` URI naming a skill the resolved profile does not
  grant -- including one that does not exist at all -- is `-32003` with data
  `{"skill", "profile"}`, the same code and shape `prompts/get`/
  `resources/read` use for a connector the profile does not grant. An unknown
  file within a granted skill, or a malformed `skill://` URI, is `-32602`.
  `skills/get` accepts only a skill's `SKILL.md` URI (its identity), never a
  supporting file's.
- *Access log.* `skills/get` and `resources/read` on a `skill://` URI are
  logged like `prompts/get`, `tool_name` set to the URI; `skills/list` is not
  (list methods never are).

### MCP JSON-RPC error codes

`pkg/mcp/jsonrpc.go` defines the standard JSON-RPC codes plus
gateway-specific ones, drawn from the `-32000..-32099` implementation-defined
range. `-32000`, `-32001`, and `-32003` are fixed by spec and must not be
renumbered; a client is expected to branch on them.

| Code | Name | Meaning |
|---|---|---|
| `-32700` | ParseError | malformed JSON |
| `-32600` | InvalidRequest | bad envelope (wrong `jsonrpc`, missing `method`) |
| `-32601` | MethodNotFound | unknown MCP method |
| `-32602` | InvalidParams | malformed `params` |
| `-32603` | InternalError | gateway-side failure |
| `-32000` | SessionNotFound | unknown/expired `Mcp-Session-Id` — re-initialize |
| `-32001` | Unauthorized | no usable gateway credential (HTTP 401) |
| `-32002` | ToolExecution | backend transport/JSON-RPC failure calling the tool (or fetching a prompt / reading a resource) |
| `-32003` | ToolNotAllowed | agent profile doesn't grant this tool, or no tool of the prompt's/resource's connector (incl. unknown profile name); also a `gateway__skill` call, native command `prompts/get`, or Skills Extension `skills/get`/`resources/read` on `skill://` naming a skill/command not attached to the profile |
| `-32004` | ConnectorNotFound | `<connector>__<tool>`/`<connector>__<prompt>` prefix or `gw://<connector>/` names no connector in the tenant, or a disabled one |
| `-32005` | ConnectorUnhealthy | connector is in its half-open recovery cool-down |
| `-32006` | Forbidden | caller authenticated but lacks `mcp.access` (HTTP 403) |
| `-32029` | RateLimited | IP or key prefix locked out after repeated failed authentication (HTTP 429 + `Retry-After`) |
| `-32800` | RequestCancelled | the caller cancelled this `tools/call` with `notifications/cancelled` (see below) |

`-32800` is outside the implementation-defined range on purpose: it is
LSP's `RequestCancelled`, the established JSON-RPC convention, since MCP
defines no code for a cancelled request.

### Server-sent notifications, cancellation and progress

`GET /mcp/stream` is the agent's server→client channel. It carries:

| Notification | Scope | When |
|---|---|---|
| `notifications/tools/list_changed` | every stream of the tenant | the tenant's tool cache was refreshed (relayed across replicas by the `redis` driver) |
| `notifications/progress` | the streams of **one session** | a connector reports progress on a `tools/call` that carried `params._meta.progressToken` |
| `notifications/resources/updated` | the streams of **one session** | a connector reports a change to a resource the session subscribed to (URI re-namespaced) |
| `notifications/resources/list_changed`, `notifications/prompts/list_changed` | the streams of **one session** | a connector the session holds an upstream stream to announced it |
| `sampling/createMessage`, `elicitation/create`, `roots/list` (**requests**, id `gw-…`), and `notifications/cancelled` for them | the streams of **one session** | a connector asked, its policy allows it and the session declared the capability (see [below](#server-initiated-requests-sampling-elicitation-roots)) |

A stream opened with an `Mcp-Session-Id` header subscribes to that
session's messages as well as the tenant's; the id is resolved within the
caller's tenant exactly as on `POST /mcp` (unknown → `-32000`). The stream
subscribes before it sends its headers, so a client that has the `200` is
already listening.

**Progress.** `params._meta` of a `tools/call` is forwarded to the
connector byte for byte, so the connector sees the caller's
`progressToken`. When the connector answers with a `text/event-stream`,
`client.Do` reads it frame by frame and hands every notification ahead of
the final response to `Call.OnNotification`; the orchestrator relays each
`notifications/progress` whose token matches the caller's to that
session's streams via `transport.Hub.Notify`. It is best-effort: with no
stream open (or one more than 64 messages behind) progress is dropped, and
it is never relayed for a stateless (session-less) call. Other upstream
notifications (`notifications/message`, …) are not relayed.

**Cancellation.** A `tools/call` made on a session registers itself, under
its inbound JSON-RPC id, in a per-process in-flight registry (bounded at
1024 per session; beyond that a call still runs but cannot be cancelled).
Ids are normalised so `7`, `7.0` and `7e0` match, while `7` and `"7"` stay
different ids, as JSON-RPC requires. `notifications/cancelled
{"requestId", "reason"?}` on the same session:

1. is answered `204`, like every notification, whether or not it matched;
2. if the id is in flight, forwards `notifications/cancelled` to the
   connector naming the **upstream** request id (each upstream
   `tools/call` gets a unique `tools/call:<tool>:<nonce>` id for this),
   fire-and-forget; a connector that ignores it is fine;
3. cancels the call's context, which aborts the upstream exchange
   mid-stream. The client never retries a cancelled call, the connector is
   **not** marked unhealthy and its backend session handle is kept.

The cancelled `tools/call` then answers HTTP `200` with JSON-RPC error
`-32800` `"request cancelled"` (`data.reason` when one was given). MCP
2025-06-18 (basic/utilities/cancellation) says a receiver SHOULD NOT
respond to a cancelled request, but over Streamable HTTP the call's POST
is still open and must end with something; a client that has already
stopped waiting discards it. An unknown id, an id that already finished,
or a cancellation without a session is a no-op, which the spec allows.

**Known limitation: same replica.** The in-flight registry is per
process. With the `redis` session driver, a `notifications/cancelled`
must reach the replica executing the call; otherwise it is a no-op (the
call runs to completion or its timeout). Session affinity at the load
balancer (on `Mcp-Session-Id`) avoids it. Progress is not limited this
way: like every per-session message it is relayed to the replica holding
the session's stream (below).

### Upstream streams and subscriptions

A Streamable HTTP MCP server sends what is not a reply to anything — a
subscribed resource changed, a list changed, a request of its own — on a
long-lived `GET` stream its client holds. The gateway is that client, per
agent session, since what arrives belongs to one backend session and so
to one agent session. `internal/dataplane/upstream.Manager` owns these
streams, one per (agent session, connector):

- **Lazy.** A stream exists only while something needs it; each need is
  a string the feature holding it acquires and releases
  (`Manager.Acquire`/`Release`). There are two: a subscription (one per
  subscribed URI), so a stream opens on a session's first
  `resources/subscribe` to that connector and closes on its last
  unsubscribe; and `server-requests`, held from the connector's handshake
  for as long as the session lives whenever the gateway declared it any
  client capability (see
  [below](#server-initiated-requests-sampling-elicitation-roots)). Either
  way it closes when the session ends: `DELETE /mcp` closes its
  streams on the replica that serves it, and a sweep every 30s closes
  those of sessions that expired or were deleted on another replica.
- **Opened like a call.** `client.OpenStream` sends `GET <endpoint>` with
  `Accept: text/event-stream`, the backend's `Mcp-Session-Id` and
  `Mcp-Protocol-Version`, and the connector's resolved headers plus the
  gateway-owned ones through the same `newRequest` path as every call
  (a `401` evicts cached credentials and is replayed once).
- **Kept.** A stream that ends or fails is reopened with backoff (1s
  doubling to 30s, ±25% jitter), with `Last-Event-ID` when the server
  numbered its events, and on the session's current backend handle.
  It gives up when the session is gone.
- **Never a health signal.** The connector's `timeout_ms` bounds only the
  wait for the stream's headers; an idle stream is healthy for as long
  as it lasts, and no stream failure marks a connector unhealthy (only
  real calls do). There is no `TOOL_TIMEOUT` on a stream.
- **Honest about support.** `405` or `404` to the `GET` means the
  connector has no stream (for that session): it is recorded, never
  retried for the session, and the subscribe is `-32601`.
- **Bounded.** `mcp.max_upstream_streams_per_session` (16) streams and
  `mcp.max_subscriptions_per_session` (256) subscriptions per session;
  past either the request is `-32600`. At most 8 connector requests per
  stream are answered concurrently; the excess is refused `-32603`.

Every frame is a JSON-RPC message, dispatched by shape. A **request**
(`id` + `method`) goes to an `upstream.RequestHandler`,
`HandleRequest(ctx, SessionRef, ConnectorRef, mcp.Request) (json.RawMessage, *mcp.Error)`,
whose answer is POSTed back to the connector as the response to that id;
the orchestrator's handler relays it to the agent (next section), and
`upstream.Unsupported`, the Manager's default when none is set, answers
`-32601`. A **notification** goes to the orchestrator:

| From the connector | What the gateway does |
|---|---|
| `notifications/resources/updated {uri}` | if the session subscribed to that uri on that connector: relays it to the session with `uri` re-wrapped as `gw://<connector>/<uri>` (other params verbatim); otherwise drops it |
| `notifications/resources/list_changed`, `notifications/prompts/list_changed` | relays it to the session whose stream carried it |
| `notifications/tools/list_changed` | marks that connector's cached tools stale, re-lists them (as the session's principal) into the cache, then sends the tenant-wide `tools/list_changed` as a cache refresh does; a re-list already running for the connector absorbs the next announcement |
| anything else | dropped |

A response on the stream (to nothing the gateway sent there) is dropped.

**Replica ownership.** With the `redis` session driver an agent's
`GET /mcp/stream` and its `POST /mcp` may land on different replicas. The
rule: the replica that serves `resources/subscribe` owns the upstream
stream (and the subscription record). A notification it reads is
delivered through `transport.Bridge.Notify`: straight to the session's
streams if it holds any, otherwise published on the `Notifier`'s session
channel (`gw:session_msgs`, a `pkg/session.SessionMessage` carrying
tenant, session id and the encoded notification); every other replica
hands it to its own streams for that tenant and session, if any. A
`resources/unsubscribe` that lands elsewhere still forwards the
unsubscribe to the connector (the backend session is shared through the
session store), so updates stop, but the owner's stream and subscription
record stay until the owner sees the session end; and a uri subscribed
through two replicas is relayed twice. Session affinity on
`Mcp-Session-Id` makes the owner the only replica involved and avoids
both.

List-changed notifications ride the same streams, so in this release a
session is told of a connector's `prompts`/`resources` list changes only
while it holds a stream to that connector — that is, while it has a
subscription there, or was declared a server-request capability to it.

### Server-initiated requests (sampling, elicitation, roots)

A connector may ask its client for something: `sampling/createMessage`
(a completion on the client's model), `elicitation/create` (input from
the human), `roots/list` (the client's working directories). The gateway
is that client, but only the agent can answer, so
`internal/dataplane/orchestrator/relay.go` relays. Two gates, both
required: the connector's policy (`metadata.server_requests`, all off by
default; see [security-model.md](security-model.md#server-initiated-mcp-requests-off-by-default-and-why))
and the agent's own `initialize`, whose `params.capabilities.sampling`,
`.elicitation` and `.roots` are recorded, raw, on the session
(`pkg/session.Record.ClientCapabilities`, so every replica has them).

**Handshake.** `client.Initialize` declares to a connector exactly the
intersection — capabilities the agent declared and the policy allows,
each as the agent wrote it (`roots.listChanged` passes through) — and
`{}` otherwise; the unconditional `roots` earlier releases sent is gone.
When the intersection is not empty the handshake also acquires the
`server-requests` need on that connector's upstream stream for the
session, since a request may come on it.

**Two sources, one relay.**

1. *Inside a `tools/call` reply* (the usual case: a tool that needs the
   agent mid-call). `readSSE` hands each request frame to
   `Call.OnRequest`, which runs on its own goroutine — at most 4 per call,
   the excess refused `-32603` — while the reply keeps being read, so
   progress and the final response still flow.
2. *On the upstream stream* — `upstream.RequestHandler`, at most 8 at once
   per stream.

Both end in `Orchestrator.relay`, and both answers go back with
`client.Reply`: a `POST` of the JSON-RPC response under the connector's
own id, on the backend session, with the connector's resolved headers.

```
connector ──request (id 7)──▶ gateway ──checks: method ∈ {3}, policy, declared capability
                               │  (any fails → -32601 to the connector; the agent sees nothing)
                               ├─ pending[gw-<16 hex>] (≤ max_pending per session)
                               ├─▶ agent's GET /mcp/stream: {"id":"gw-…","method":…,"params":<verbatim>}
agent ──POST /mcp {"id":"gw-…","result"|"error"} ──▶ 202; matched on (tenant, session, id)
connector ◀──POST {"id":7,"result"|"error"}── gateway
```

**Pending table.** Per replica, keyed by gateway id, bounded per session
by `mcp.max_pending_server_requests_per_session` (32). A request that
finds no stream for the session anywhere is `-32603` "client has no open
stream"; one not answered within `mcp.server_request_timeout` (5m) is
`-32603` "timed out…" and the agent gets `notifications/cancelled`
naming the gateway id; ending the session (`DELETE /mcp`, or the sweep
finding it gone) fails every pending request with `-32603`. A relayed
request lives under the context of what it came from, so cancelling the
`tools/call` (`notifications/cancelled`, or the caller going away)
cancels it too — the agent is sent `notifications/cancelled`, and the
connector is not answered, its call being over.

**Tool timeout.** The connector's `timeout_ms` is applied by `client.Do`
as a pausable timer rather than a context deadline: while one of the
call's relayed requests is pending, the clock stops, and it resumes with
what was left once the last is answered. Time spent with a human filling
in an elicitation is not the backend being slow; the wait is bounded by
`server_request_timeout` instead. A call's own backend time still
counts in full (before and after the answer).

**Replica ownership.** The replica running the `tools/call`, or holding
the upstream stream, owns the pending entry. The request reaches the
agent through `transport.Bridge.DeliverRequest`: to the session's local
streams, or, with none here, published on the session channel as a
`pkg/session.SessionMessage` of `Kind: "request"`; the replica that
writes it to a stream publishes `Kind: "ack"`, and the owner treats no
ack within 2s as "client has no open stream". The agent's answer may be
POSTed to any replica: one without a matching pending entry publishes it
as `Kind: "response"`, and the owner settles it (a replica never
re-publishes a relayed response). The `memory` driver has no Notifier,
so there everything is local.

**Access log.** One record per relayed or refused request: `method` the
MCP method, `json_rpc_id`/`request_id` the gateway id, `connector_id`,
`tool_name` = the connector's slug, `error_code` when the connector got
an error, `duration_ms` to the agent's answer; the agent's answering
`POST` is its own record with `method` `response`. No params or results.

## Request flow: an LLM call

```
Client (Claude Code / SDK)     LLM plane (:8082)                Provider
  │  POST /v1/messages           │                                  │
  │  Authorization / X-Gateway-Key│                                  │
  │  x-api-key: sk-... (BYOK)     │                                  │
  ├─────────────────────────────►│                                  │
  │                               │ 1 authenticate                     │
  │                               │ 2 tag session id, check llm.access │
  │                               │ 3 cap request body (413)          │
  │                               │ 4 per-tenant concurrency limit     │
  │                               │ 5 stream deadline starts           │
  │                               │ 6 pick provider by route           │
  │                               │ 7 build upstream request           │
  │                               │   (byte-for-byte / SigV4 sign)     │
  │                               ├────────── forward ───────────────►│
  │                               │◄───────── stream response ────────┤
  │                               │ 8 tee to client + bounded capture │
  │                               │ 9 parse usage (head+tail scan)     │
  │                               │ 10 price via rate card             │
  │                               │ 11 write llm_calls (Postgres)      │
  │                               │ 12 tee to sinks (best-effort)      │
  │                               │ 13 detection tee (async, opt-in)   │
  │◄─────────────────────────────┤                                  │
  │  streamed response            │                                  │
```

1. `internal/auth.Middleware` resolves the credential to a `Principal`
   (same mechanism as the MCP flow's step 1).
2. `withTags` reads `X-Session-Id` (or `X-Claude-Code-Session-Id`) for
   capture correlation and mints a request id; both headers are stripped
   before forwarding. Then `llmplane`'s own `requirePermission` checks
   `llm.access` (403 if ungranted; `agent` holds it via `llm.*`, `admin`
   via `*`, a custom role must be granted explicitly). `llmplane.Handler`
   refuses to build without an `Authorizer` — a nil one previously let
   every key through.
3. `limitBody` enforces `llm_proxy.limits.max_request_bytes` (413 on
   overflow).
4. `tenantLimiter` enforces `llm_proxy.limits.max_concurrent_per_tenant`
   (429 over the cap).
5. `streamDeadline` bounds the whole request at
   `llm_proxy.limits.max_stream_duration`.
6. `pickProvider` matches a `/{provider-id}/…` prefix, or one of the bare
   native paths (`/v1/messages*`, `/v1/complete*` → Anthropic; `/model/*`
   → Bedrock). Per-key limits (budgets, requests-per-minute, `max_tokens`)
   are checked next, then the model registry resolves the requested model:
   a hit (after that model's own limits) replaces the upstream with the
   row's ordered targets, a miss is the passthrough below — see
   [llm-plane.md](llm-plane.md#model-registry).
7. `Provider.BuildUpstream` forwards the body byte-for-byte
   (Anthropic/OpenAI/Gemini, so prompt caching survives; the one opt-in
   exception is `inject_stream_usage`, see below) or SigV4-signs it
   (Bedrock — region/role hardening below). A registry target in another
   wire format is translated through `pkg/llm` instead; targets are tried
   in order, moving on after a build or dial failure or a retryable status
   (429/408/5xx) only while nothing has reached the client.
8. The response relays to the client via `io.MultiWriter` (a redirect is
   relayed, never followed) while a bounded copy is captured for storage
   only when `store_bodies` is on — the client is never blocked on capture.
9. Usage (tokens, stop reason) is parsed from a head+tail scan, correct
   even when the body exceeds the capture cap. The same relayed bytes feed
   skill/MCP-tool discovery (`internal/discovery`, see
   [observability.md](observability.md#discovered-skills-and-mcp-servers)).
10. `pkg/pricing.Card.Cost` prices the call from the rate card (`NULL` for
    a free utility endpoint or unreadable usage), frozen at write time.
11. The record is written synchronously to Postgres `llm_calls` (text
    sanitized for invalid UTF-8/NUL; idempotent, `request_id` PK) before
    the request is considered captured — see [llm-plane.md](llm-plane.md).
    With a `sink.BodyStore` configured, the bodies are offloaded first and
    the row carries a `body_ref` instead (inline again if the offload fails).
12. A best-effort copy also goes to the shared `sink.Multi`
    (stdout/otel/clickhouse), independent of step 11.
13. With `llm_proxy.detection.agent_url` set, the detection tee queues the
    full request body and the first 1 MiB of the response (a complete 2xx
    relay only) and posts them to a local detection agent asynchronously;
    nothing waits on it — see [llm-plane.md](llm-plane.md#detection-agent).

### LLM plane hardening

Beyond the `llm.access` gate above, the LLM plane closes gaps a raw
passthrough proxy would otherwise have: the Bedrock region (header,
config, or SigV4 credential scope) is validated against
`config.ReAWSRegion`; `X-Bedrock-Role-Arn` assumption is off unless the
account is in `llm_proxy.bedrock.allowed_role_accounts`, always pins STS
`ExternalId` to the caller's tenant (never the caller's choice), and
caches assumed credentials per role|tenant|region; an upstream redirect
is relayed to the client rather than followed (it would otherwise re-send
the body and provider key to `Location`'s host); a transport error is
recorded with its URL's query string and userinfo stripped; captured text
(Postgres columns and OTel span attributes alike) is sanitized for invalid
UTF-8/NUL before it's written; free utility endpoints (token counting,
Anthropic Message Batches) are never priced, and a call with no readable
usage stores `NULL` cost rather than `$0`; and provider pricing modifiers
(service tiers, fast/US-inference-geo multipliers, Bedrock regional
premiums) apply on top of the base rate card. See
[llm-plane.md](llm-plane.md) for the full detail.

## Data model

**Postgres** (`internal/store/postgres/migrations/`; `schema_migrations`
records which have run). A tenant's rows carry its `tenant_id`, directly
or through their parent row; a NULL `tenant_id` (`models`, `skills`,
`mcp_catalog`) marks a platform row every tenant sees:

| Table | Holds |
|---|---|
| `tenants` | the isolation boundary |
| `api_keys` | hashed, role-scoped bearer credentials (with optional limits and a bound profile) |
| `credentials` | AES-256-GCM-encrypted secret payloads |
| `connectors` | registered MCP backends |
| `agent_profiles` | named tool allow-lists |
| `agent_profile_tools` | the (profile, connector, tool) grants |
| `tool_cache` | cached `tools/list` results, full-text indexed |
| `models` | the model registry |
| `skills`, `skill_versions` | the skills & commands registry and its immutable versions |
| `profile_skills` | the (profile, skill) attachments |
| `users`, `user_sessions`, `auth_audit` | console users, their browser sessions, the auth audit trail |
| `model_catalog_providers`, `model_catalog_models` | the platform model catalog |
| `mcp_catalog` | the curated MCP catalog |

`000003_limits`, `000008_key_profiles` and `000009_live_slugs` only alter
these tables; `000009_live_slugs` makes connector and profile slugs unique
among live rows, so a deleted one's slug can be reused.

The LLM plane's durable capture store (`llm_calls`) is a separate table in
the same Postgres database, created on demand by `pkg/sink/postgres` (its
own `CREATE TABLE IF NOT EXISTS`) rather than by a migration — see
[llm-plane.md](llm-plane.md).

**ClickHouse — 2 tables** (`pkg/sink/clickhouse`, optional, only when
`sinks.clickhouse.enabled`): `mcp_access_logs` and `llm_calls`, both
`MergeTree`, partitioned by month, with a 90-day TTL. These back
`GET /api/v1/analytics/*` and the console's log pages; see
[observability.md](observability.md).

## Examples and CI

The examples under `examples/` are runnable, self-checking walkthroughs
against a real gateway — see
[examples/README.md](https://github.com/Tuskira/ai-agent-gateway/blob/main/examples/README.md). `make examples-smoke` drives
every example except `10-kubernetes` (compose); `make examples-k8s` drives
10 (`kind`). CI runs `go`, `web`, `integration`, `security` (govulncheck,
gitleaks), `docker` and `examples` on every PR, plus `examples-k8s` on
pushes to `main` or PRs labelled `k8s`. Tagged releases build with GoReleaser and publish
multi-arch, distroless images to `ghcr.io/tuskira/ai-agent-gateway`;
`v0.1.0`, `v0.2.0`, and `v0.3.0` are released.

## What is and isn't supported

**Supported** (v0.3.0, plus items marked *unreleased*, which are on `main` only):

- All three planes, independently enableable; `deploy/k8s` runs them as
  three Deployments from one image, with connector health/discover/cache
  ops working on an API-only instance.
- API-key authentication, role-based authorization, a separate
  `platform-admin` role (*unreleased*; in v0.3.0 `admin` also held
  `platform.*`), and an `llm.access` gate on the LLM plane (`admin`/`agent`
  hold it by default; `llmplane.Handler` refuses to build without an
  `Authorizer`).
- Agent profiles enforced on `tools/call`, not just filtered on
  `tools/list`; *unreleased*: binding a key to a profile (`profile_id`)
  confines it there; otherwise the caller-supplied header picks it.
- The encrypted credential store with key rotation (`gateway secrets
  rekey`) and a pluggable outbound-header resolver framework.
- *Unreleased:* an SSRF guard (`internal/netguard`) on every outbound dial
  — connectors, catalog probes, model-registry targets, LLM upstreams —
  that refuses internal addresses unless `egress.allowed_cidrs`/
  `egress.allowed_hosts` allow them, and failed-auth lockout on the MCP
  and LLM planes (per plane).
- MCP protocol 2025-06-18 with a one-shot fallback to 2024-11-05, session
  handling, a half-open health/probe cycle, a Postgres-backed tool cache.
- MCP sessions behind the `pkg/session` seam, with an in-process driver
  (default) and a Redis driver (`sessions.store: redis`; any
  Redis-protocol server, Valkey in the bundled deploys) that shares
  sessions and relays `tools/list_changed` and per-session notifications
  across MCP replicas, so the MCP plane can scale out
  (`deploy/k8s/overlays/redis`).
- MCP resource subscriptions, with a per-session upstream stream to each
  subscribed connector that also relays its list-changed notifications.
- Native passthrough for Anthropic, OpenAI, Gemini, and three Bedrock
  credential modes, hardened against region/role abuse, redirect replay,
  and credential-leaking error text (see "LLM plane hardening" above).
- stdout/OTel/ClickHouse sinks, an analytics read API, an embedded React
  console, runnable examples, CI, and tagged GHCR releases via GoReleaser.
- A model registry: a tenant registers a model *name* once and points it
  at an ordered list of upstream targets across providers (Anthropic,
  OpenAI-compatible, Bedrock, Gemini), served natively in the client's own
  wire format or translated through the `pkg/llm` seam (Anthropic
  Messages to an OpenAI-compatible target), plus a Model Catalog of known providers and
  models with a Connect flow and price refresh.
- Per-API-key and per-model limits on the LLM plane — request-size caps,
  requests-per-minute, and USD budgets — enforced before a request is
  forwarded, in addition to the per-tenant concurrency cap.
- Skills & commands on agent profiles: a skills registry, gateway-native
  MCP delivery, the MCP Skills Extension, and a curated platform MCP
  catalog a tenant admin can add from.
- Discovery of skills and MCP tools actually used in LLM traffic (read
  from Anthropic, Bedrock, Gemini, and OpenAI chat completions and
  Responses) and a Session
  Timeline console page for MCP sessions.
- An ingest endpoint (`POST /api/v1/ingest`) that accepts externally
  captured LLM/access-log records, gated behind a new `interceptor` role
  that can only write to it.
- Console username/password login alongside API-key authentication, and
  a Docusaurus docs site (`website/`) built from this folder.

**Not supported** (see [security-model.md](security-model.md#known-limits)
for the security-relevant ones in detail):

- No OIDC/SSO authenticator: API keys and console username/password only.
- No Redis-backed tool cache or connector health: the `redis` section
  feeds only the session store. The tool cache stays on Postgres (already
  shared by every replica) and router health stays per process (a
  connector one replica marks unhealthy is probed independently by
  another). Redis Cluster mode is not supported (`redis.cluster: true`
  is rejected at startup).
- Profile binding is opt-in: a key created without `profile_id` can
  present any `X-Agent-Profile-Name` its tenant owns; `mcp.require_profile`
  only forces the header. A key bound to a profile cannot.
- `admin` is a tenant admin; `platform.*` (tenants, platform catalog,
  model catalog) needs the `platform-admin` role (`pkg/auth.NewRoleAuthorizer`).
  In a single-tenant deployment a console admin can also manage the model catalog.
- The MCP and LLM planes limit failed authentication (per IP and per key
  prefix) but have no general request-rate cap: the LLM plane's quota is
  per-tenant concurrency and per-key limits, and MCP has none. Limiter
  state is per process.
- No Helm chart (kustomize only; a Helm chart is planned).
