# Connectors and credentials

A **connector** is an MCP server registered with the gateway (the console's
**MCPs** page; the API and permissions (`connector.*`) keep the name
connector). The gateway presents one flat tool namespace across every
connector in a tenant, named
`"<connector slug>__<tool name>"` (`internal/dataplane/client.QualifyTool`).

## Register a connector

```sh
curl -X POST http://localhost:8081/api/v1/connectors \
  -H "Authorization: Bearer $GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
        "name": "GitHub MCP",
        "endpoint": "https://github-mcp.internal:8443/mcp",
        "timeout_ms": 30000,
        "metadata": {
          "headers": {
            "Authorization": {"type": "static", "value": "Bearer ghp_...", "prefix": ""}
          }
        }
      }'
```

Required fields: `name` (1–120 chars) and `endpoint` (an absolute
`http`/`https` URL). `timeout_ms`, if omitted, defaults to
`connectors.default_timeout_ms` (`30000` unless configured) and must be
`100`–`600000` when given. `slug` is optional — if omitted, it's
derived from `name` (lowercased, non-alphanumeric runs collapsed to a
single `-`); if given explicitly it must match `^[a-z0-9-]+$` and be
unique per tenant (`internal/api/handlers/validate.go`). A `description`
field, if sent, is stored at `metadata.description` — there is no
dedicated column.

`PUT /connectors/{id}` is a full replace for `name`/`endpoint`/`timeout_ms`
(a `PUT` without `timeout_ms` resets it to `connectors.default_timeout_ms`);
`metadata` is left untouched if omitted from the request body (JSON `null`
and "field absent" are indistinguishable, so this is the safer default for
a field that usually holds hand-authored header configs). `slug` is kept
unless the request sends a new one. Changing it renames every tool, prompt
and resource URI the connector exposes (`<slug>__<tool>`, `gw://<slug>/…`).
Reads mask secret material: a `static` header's `value` and every value in
a header's `config` come back as `"***"`, and sending `"***"` back on `PUT`
keeps the stored value.

`endpoint` on an internal address (loopback, private, link-local, cloud
metadata, etc.) is rejected — including a hostname like `localhost` that
resolves only to such addresses (a name that does not resolve yet is
accepted and checked at dial time), not just a literal IP — unless that range or host is
allowlisted via `egress.allowed_cidrs` / `egress.allowed_hosts`. This is
the gateway's outbound-request (SSRF) guard; see the [README's "MCP
servers on localhost or private networks"](https://github.com/Tuskira/ai-agent-gateway/blob/main/README.md#mcp-servers-on-localhost-or-private-networks)
for how to allow a local or private MCP server, and
[security-model.md#outbound-requests-ssrf](security-model.md#outbound-requests-ssrf)
for what it blocks and why.

## Enabling and disabling a connector

A connector is enabled unless `metadata.enabled` is `false`. A disabled
connector stays registered (credentials, tools and profile grants are kept) but
the MCP plane neither lists its tools nor routes calls to it, so a call to it
fails as "connector not found". Toggle it from the MCPs page (Disable / Enable
row action) or with `PUT /connectors/{id}` and `metadata.enabled: false`; note
the PUT replaces `metadata` wholesale, so send the rest of it back. The console
shows it as **Disabled** in the State column, which is separate from health.

## Health and discovery

```sh
# Probe: runs an MCP `initialize` against the connector and persists the result.
curl -X GET http://localhost:8081/api/v1/connectors/$ID/health \
  -H "Authorization: Bearer $GATEWAY_KEY"

# Discover: runs `tools/list` against the connector and writes the tool cache.
curl -X POST http://localhost:8081/api/v1/connectors/$ID/discover \
  -H "Authorization: Bearer $GATEWAY_KEY"
```

`GET .../health` returns `{"status": "healthy"|"unhealthy", "latency_ms":
…, "capabilities": {...}, "checked_at": "...", "error": "..."}`.
`capabilities` mirrors the backend's `initialize` result:
`tools`/`resources`/`prompts` booleans, `protocol_version`, and
`server_info: {name, version}`. Both routes need the live MCP client the
data plane owns, so the gateway builds that data plane whenever the API
plane is enabled — they work on a control-plane-only instance
(`mcp.enabled: false`, the per-plane Kubernetes deployment) too, without
opening the MCP listener.

## MCP catalog

The platform keeps a catalog of well-known MCP servers. Platform entries are
curated by the operator (config `mcp_catalog.seed`, which ships with ten
well-known servers; see [configuration.md](configuration.md)) and **inert**: nothing in the catalog
is callable until a tenant admin adds it. Adding creates a normal tenant
connector, so everything above (health, discovery, profiles, deletion)
applies unchanged. Removing an MCP is an ordinary connector delete, after
which the entry shows as available again; adding it again creates a new
connector (a new id), so profile tool grants must be set again.

```sh
curl http://localhost:8081/api/v1/mcp-catalog -H "Authorization: Bearer $GATEWAY_KEY"

curl -X POST http://localhost:8081/api/v1/mcp-catalog/langfuse/add \
  -H "Authorization: Bearer $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d '{"fields": {"public_key": "pk-lf-...", "secret_key": "sk-lf-..."}}'
```

Each entry carries `added` and `connector_id` (this tenant's connector, if
any) and `supported` / `unsupported_reason`. A connector created from an
entry returns `catalog_id`, so a client can tell catalog-derived connectors
from custom ones.

**Auth kinds.** An entry's `auth.kind` says what the tenant supplies, via
`auth.fields` (`name`, `label`, `secret`, `required`, `placeholder`, `help`):

| Kind | Fields | Outbound header |
| --- | --- | --- |
| `none` | none | none |
| `bearer` | one | `Authorization: Bearer <token>` |
| `basic` | two | `Authorization: Basic base64(first:second)` |
| `header` | one | `auth.header_template.name: <prefix><value>` |
| `oauth` | none | not supported: add answers `422 oauth_not_supported` |

The finished header value is stored as a tenant credential named
`mcp-<slug>` (type `mcp_catalog`; its value is never returned, but its
metadata is listed by `GET /credentials` and it is kept when the MCP is
removed) and the
connector's header is a `secret_store` reference to it, exactly like a
hand-written connector. A field with `query` set is not a credential: it is
appended to the connector URL as that query parameter (for example
Supabase's `project_ref`). An entry with `url_overridable: true` lets the
tenant send `url` to point at another region or a self-hosted server; it
must be https (http only for a loopback host, which the egress guard blocks
unless allowlisted).

**Add request.** `POST /mcp-catalog/{slug}/add` takes `name`, `slug`,
`fields` and `url`, all optional except the entry's required fields. It
validates, stores the credential, creates the connector, then probes health
and runs tool discovery. A failed probe (bad credentials, say) does not
fail the add: the connector exists, and the `discovery` object in the `201`
response (`{status, error?, tools_discovered, discovery_error?}`) and the
connector's health say what happened. Discovery runs only when the probe
comes back `healthy`; each step is capped at 35 s. Adding an entry
twice is `409 already_added` unless a different `slug` is sent; a `slug`
already used by another connector is a plain `409 conflict`. A
credential named `mcp-<slug>` that already exists is reused only when it is
an unused leftover of an earlier add; one created by hand, or used by a
live connector, is never touched and the next free `mcp-<slug>-N` is used.

**Platform and tenant entries.** Like the model registry, the catalog has
two scopes. PLATFORM entries are seeded from config (or written by a
`platform.admin` caller) and visible to every tenant. TENANT entries belong
to one tenant's catalog and are private to it; a tenant entry with the same
slug as a platform entry replaces it for that tenant (delete it to get the
platform one back). Platform entries from `mcp_catalog.seed` are re-applied
at every startup: an API edit to one is reverted and a deleted one comes
back, so change seeded entries in config (or set `disabled: true` there);
`mcp_catalog.seed: []` stops seeding but leaves existing rows. Any caller
who can create connectors (the `admin` role) manages tenant entries:

```sh
curl -X POST http://localhost:8081/api/v1/mcp-catalog \
  -H "Authorization: Bearer $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d '{"slug": "internal-search", "name": "Internal search",
       "url": "https://search.internal.example.com/mcp",
       "auth": {"kind": "bearer",
                "fields": [{"name": "token", "label": "Token", "secret": true, "required": true}]}}'
```

`PUT /mcp-catalog/{slug}` replaces the entry's fields and `DELETE` removes
it (soft delete; MCPs already added from it keep working). A platform entry
can be edited or deleted only with `platform.admin`, which console users
never hold, so the console shows platform entries read-only with a Platform
badge. A bearer, header or basic field left `required: false` is optional:
when it is blank the add sends no auth header at all (Context7 and Hugging
Face work anonymously). Connectors that already existed before the catalog
stay custom connectors (`catalog_id` empty); nothing links them to an entry
by URL.

## Header types

A connector's `metadata.headers` map names each outbound header and how to
compute it. Four resolver types exist; `external` dispatches further to a
named provider.
Saving a connector validates every header config: an unknown `type`, an
invalid header name, a line break in a `static` value or a `prefix`, a
`metadata.headers` that is not an object, or a config its resolver rejects
(the cases below) is `400 validation_error` on `POST`/`PUT`. On `PUT` the
check runs after `"***"` values are unmasked and covers only the headers
the request adds or changes, so a connector saved before this check
existed can still be edited, enabled and disabled.

**`static`** — a fixed value, with an optional prefix:

```json
{"type": "static", "value": "my-fixed-token", "prefix": "Bearer "}
```

**`token_field`** — one field of the caller's own authenticated
`Principal`:

```json
{"type": "token_field", "field": "bearer_token"}
```

`field` is one of `email` | `subject` | `tenant_id` | `bearer_token`
(`bearer_token` forwards the caller's own raw gateway credential to the
backend). **`bearer_token` is off by default**: it hands every caller's
gateway API key to whoever runs the connector endpoint. Turn it on only
for backends you fully trust, with `connectors.allow_bearer_token_forwarding:
true`; otherwise creating such a connector is `400 bearer_forwarding_disabled`
and a call through an existing one fails instead of forwarding.

**`incoming_field`** — copies one header from the inbound request being
proxied:

```json
{"type": "incoming_field", "header": "X-Correlation-Id"}
```

Refused at save time (`400 validation_error` on `POST`/`PUT`) for an
invalid header name or any header on a fixed denylist — `Authorization`, `X-Tenant-Id`, `Tenantid`,
`X-Gateway-Key`, hop-by-hop headers (`Connection`, `Upgrade`, …), and anything prefixed `mcp-` — so a
connector config can never be used to forge the caller's identity or
tamper with MCP session state.

**`external`** — dispatches to a registered `ExternalProvider`:

```json
{"type": "external", "provider": "secret_store",
 "config": {"credential": "github-pat", "field": "token"}}
```

```json
{"type": "external", "provider": "env", "config": {"var": "GITHUB_TOKEN"}}
```

```json
{"type": "external", "provider": "file",
 "config": {"path": "okta/client-secret", "field": "client_secret"}}
```

`env` and `file` are off by default (`secret_store.allow_env_provider`;
`secret_store.allow_file_provider` plus `secret_store.file_provider_root`;
see [security-model.md](security-model.md#envfile-header-providers-off-by-default-and-why)).
While off, a connector header that uses one is refused with `400 validation_error`.

`GET /api/v1/headers/providers` lists every registered provider (id, name,
JSON-Schema `config_schema`) plus the four resolver type names, for a
UI to build a schema-driven form.

An `external` value (a stored credential, `env` or `file`) that contains
CR or LF, such as a multi-line PEM key, has its trailing newline trimmed
and each remaining line break escaped to the two characters `\n`. A
`static`, `token_field` or `incoming_field` value containing a raw CR or
LF byte is refused instead,
which prevents header injection. Header resolution is best-effort per
header: one that fails to resolve is logged and skipped, not a whole-call failure — the
backend decides whether the missing header matters, by answering 401.

## Credentials and rotation

Credentials referenced by the `secret_store` provider live in the
gateway's own encrypted store, addressed by `(tenant, name)`:

```sh
curl -X POST http://localhost:8081/api/v1/credentials \
  -H "Authorization: Bearer $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d '{"name": "github-pat", "type": "pat", "payload": {"token": "ghp_..."}}'
```

`GET /credentials` and `GET /credentials/{name}` return metadata only
(`field_names`, `key_id`, timestamps, and `used_by` — every connector
whose headers reference this credential) — the decrypted payload is never
serialized in a response. `PUT /credentials/{name}` rotates the payload
in place (re-encrypts under the ring's current active key, sets
`rotated_at`); `DELETE` removes it. The new value is used at once by the
process that served the `PUT`; other gateway processes keep their cached
copy for up to 5 minutes, or until a backend answers 401 (see
Troubleshooting).

**Master-key rotation** is a separate, ring-level operation from
credential rotation above:

```sh
# 1. add the new key alongside the old one, and flip active_key_id
export GATEWAY_MASTER_KEY="k1:<old-key>,k2:$(gateway secrets genkey)"
# (set secret_store.active_key_id: k2 in config)

# 2. re-encrypt every credential of a tenant (or every tenant) under k2
gateway secrets rekey --tenant acme
gateway secrets rekey --tenant all

# 3. once every row is confirmed rewritten, drop k1 from the ring
```

`secrets rekey` only rewrites rows whose stored `key_id` differs from the
ring's current active key, so it's safe to run repeatedly. Progress goes
to stderr; there is no plaintext to capture on stdout for this
subcommand.

## `tool_arg_overrides`

Connector `metadata.tool_arg_overrides` stamps constant arguments onto
every call to that connector's tools — server-side, so a model can never
guess (or be tricked into supplying) a tenant constant like a project id.
Two shapes are accepted and merged, with per-tool entries winning:

```json
// flat: applied to every tool call on this connector
{"tool_arg_overrides": {"customerId": "acme"}}
```

```json
// nested: applied only to the tool named "search"
{"tool_arg_overrides": {"search": {"customerId": "acme"}}}
```

A flat value is anything that is *not* a JSON object; a JSON-object value
is treated as a per-tool block — so a flat argument whose own value must
be an object has to be expressed under a tool name.
`router.StampOverrides` applies these after the profile/routing checks,
overwriting anything the caller supplied for the same argument names. The
matching scrub happens in `tools/list`: every overridden argument name is
removed from the advertised `input_schema` (and from `required`), so
nothing invites a model to guess at it in the first place.

## Server-initiated requests (sampling, elicitation, roots)

MCP lets a backend send requests **back** to the caller, not just answer
`tools/call`:

- **`sampling/createMessage`** — asks the caller to run an LLM completion
  and return the result, on the caller's own model and budget. Lets a
  backend delegate part of its own reasoning to the agent's model instead
  of calling out to an LLM itself.
- **`elicitation/create`** — asks the human operating the agent for a
  piece of input mid-call (a missing parameter, a confirmation).
- **`roots/list`** — asks which local directories/filesystem roots the
  agent is working in.

Each of these is a trust decision (see
[security-model.md](security-model.md) for why), so the gateway relays
none of them unless the specific connector is configured to allow it.
The policy lives at `connector.metadata.server_requests`:

```json
{"server_requests": {"sampling": false, "elicitation": false, "roots": false}}
```

All three keys default to `false` when the object, or an individual key
within it, is absent — a connector that has never set this field allows
none of them. An unrecognized key inside `server_requests`, or a
non-boolean value, is rejected with a `400 validation_error` on
`POST`/`PUT` (see `internal/api/handlers/connectors.go`). Unlike
`headers`, this field carries no secret material and is never masked on
read. `internal/dataplane/client.ServerRequests` is the accessor the data
plane reads it through, and `ServerRequestPolicy.Allows(method)` is what
gates a specific relayed request.

### Enabling it

Set the flags on the connector (`PUT /api/v1/connectors/{id}` sends
`metadata` only when you include it, so send the whole object you want),
or tick them under **Server-initiated requests** in the console's
connector form:

```sh
curl -sS -X PUT "http://localhost:8081/api/v1/connectors/$CONNECTOR_ID" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name":"everything","endpoint":"http://host.docker.internal:23014/mcp",
       "metadata":{"server_requests":{"elicitation":true,"roots":true}}}'
```

### What the agent must do

1. **Declare the capability in its own `initialize`.** Only `sampling`,
   `elicitation` and `roots` under `params.capabilities` count, each as the
   object the spec defines (`{}` is enough; `roots.listChanged` and any
   other sub-field is passed through). The gateway never invents a
   capability the agent didn't offer: a connector allowed to send
   `sampling/createMessage` still gets nothing relayed if the agent never
   declared `sampling`.
2. **Hold `GET /mcp/stream` open with its `Mcp-Session-Id`.** Relayed
   requests arrive there, as JSON-RPC **requests** (they have an `id`),
   with a gateway-minted id `gw-<16 hex>` and the connector's `params`
   untouched (never decoded and re-encoded; key order kept, insignificant
   whitespace may be compacted):

   ```json
   {"jsonrpc":"2.0","id":"gw-2a43d566b7317370","method":"elicitation/create",
    "params":{"message":"Which project?","requestedSchema":{"type":"object","properties":{"project":{"type":"string"}}}}}
   ```
3. **Answer with a `POST /mcp` carrying a JSON-RPC response** on the same
   session, under that id — a `result` or an `error`, exactly as MCP
   defines it for the method. The gateway answers `202 Accepted` with no
   body, and relays the result (or error) to the connector under the
   connector's own id:

   ```json
   {"jsonrpc":"2.0","id":"gw-2a43d566b7317370","result":{"action":"accept","content":{"project":"apollo"}}}
   ```

   A response whose id is unknown (never issued, already answered, timed
   out) is still `202` and dropped. One sent on a different session than
   the request went to is ignored, so no session can answer another's
   request. A response with no `Mcp-Session-Id` is `-32600`, an unknown
   session `-32000`, and a malformed one (wrong `jsonrpc`, both or neither
   of `result`/`error`) `-32600`.
4. **Expect `notifications/cancelled`** on the stream (`requestId` = the
   gateway id) when the gateway stops waiting: the `tools/call` the
   request arose from was cancelled or ended, or the wait timed out.

### What the connector sees

At its `initialize` the gateway declares, as its client, exactly the
capabilities the agent declared **and** the policy allows — nothing else
(the `capabilities` object is `{}` when that is empty). When it is not
empty, the gateway also opens the connector's long-lived `GET` stream
for the session right away, so a request the connector sends outside a
call's reply has somewhere to arrive. A request can therefore come:

- **inside the streamed reply to a `tools/call`** (a tool asking mid-call,
  the usual case) — the gateway keeps reading the reply while the agent
  answers, answers up to 4 such requests of one call at once, and
  refuses more with `-32603`;
- **on the long-lived stream** — answered the same way, up to 8 at once
  per stream.

Either way, the answer is `POST`ed to the connector's endpoint on the
backend session, with the connector's resolved headers, as the JSON-RPC
response to the connector's own id. What the connector can get back:

| Answer | When |
|---|---|
| the agent's `result` or `error`, verbatim | the agent answered |
| `-32601` `method not found: …` | a method other than the three |
| `-32601` `<method> is not permitted for this connector` | the policy flag is off (re-checked on every request, so turning it off applies to open sessions) |
| `-32601` `client did not declare <capability>` | the agent didn't declare it (or the call had no session) |
| `-32603` `client has no open stream` | the session has no `GET /mcp/stream` open on any replica |
| `-32603` `too many requests pending for this client` | `mcp.max_pending_server_requests_per_session` (default 32) requests are already awaiting this session's answers |
| `-32603` `timed out waiting for the client` | no answer within `mcp.server_request_timeout` (default 5m) |
| `-32603` `client session has ended` | the session was deleted or expired while the request waited |

None of these ever reaches the agent. The time a `tools/call` spends
waiting on the agent is **not** charged against the connector's
`timeout_ms`: the call's timer is paused while one of its relayed
requests is pending and resumes, with what was left, when the answer is
in. The wait is bounded by `mcp.server_request_timeout` instead.

Every relayed or refused request writes its own access-log record:
`method` is the MCP method (`elicitation/create`), `json_rpc_id` and
`request_id` the gateway id, `connector_id` and `tool_name` (= the
connector's slug) the asking connector, `error_code` set when the
connector got an error, and `duration_ms` the time to the agent's
answer. Params and results — prompts, completions, whatever a human typed
— are never logged. The agent's answering `POST` has its own record with
`method` = `response`.

A tool that a server offers only to clients with a given capability
(the reference `server-everything` lists `trigger-sampling-request`,
`trigger-elicitation-request` and `get-roots-list` only then) shows up in
a `tools/list` fanned out live on the agent's session. With the tool
cache on, the cached list is built by a background refresh that declares
no capability, so such a tool may be missing from `tools/list` — it can
still be called by its qualified name.

## Slug-qualified tool names

Every advertised tool name is `"<qualifier>__<tool>"`, where the qualifier
is the connector's `slug` (falling back to `name` for pre-slug rows) —
`internal/dataplane/client.Qualifier`. The split on `tools/call` is on the
**first** `__`, so a backend tool can itself contain `__`
(`get__all__groups`) as long as no connector name does (the admin API's
`slugify` already enforces `[a-z0-9-]+`, which cannot contain `_`).

### Prompt names and resource URIs

A connector's prompts are qualified exactly like its tools:
`"<qualifier>__<prompt>"`, split on the first `__` by `prompts/get`, which
forwards the backend's own prompt name and the arguments unchanged.

Resource URIs are opaque and already carry a scheme of their own, so they
are **wrapped** rather than prefixed: every `uri` from `resources/list`
and every `uriTemplate` from `resources/templates/list` is advertised as
`gw://<qualifier>/<backend uri>`.

| Backend advertises | Gateway advertises |
|---|---|
| `test://static/resource/1` | `gw://everything/test://static/resource/1` |
| `file:///var/data/report.md` | `gw://files/file:///var/data/report.md` |
| `demo://resource/dynamic/text/{resourceId}` (template) | `gw://everything/demo://resource/dynamic/text/{resourceId}` |

`resources/read` requires the `gw://<qualifier>/` prefix (anything else is
`-32602`; an unknown qualifier is `-32004`), strips it, and forwards the
remainder byte for byte. URIs *inside* a result — `contents[].uri`, or an
embedded resource in a prompt message — are the backend's own and are
passed through unrewritten. `tool_arg_overrides` do not apply to prompt
arguments.

### Resource subscriptions

An agent subscribes with the gateway URI, on a session (a subscription is
session state; without an `Mcp-Session-Id` it is `-32600`):

```json
{"jsonrpc":"2.0","id":5,"method":"resources/subscribe",
 "params":{"uri":"gw://everything/demo://resource/static/document/architecture.md"}}
```

The profile rule is the one `resources/read` uses (a connector outside the
profile is `-32003`). The gateway then opens — once per session and
connector, and only now — a long-lived server-to-client stream to the
connector: a `GET` on its `endpoint` with `Accept: text/event-stream`,
the backend's `Mcp-Session-Id` and `Mcp-Protocol-Version`, and **the same
resolved headers as any call** (`metadata.headers`, `X-Tenant-Id`). It
forwards `resources/subscribe` with the backend's own URI
(`demo://resource/static/document/architecture.md`), and relays every
`notifications/resources/updated` the connector sends for it to the
session's `GET /mcp/stream`, with the URI wrapped back:

```json
{"jsonrpc":"2.0","method":"notifications/resources/updated",
 "params":{"uri":"gw://everything/demo://resource/static/document/architecture.md"}}
```

What a connector needs to support subscriptions through the gateway:

- declare `resources.subscribe` in its `initialize` result (the gateway
  advertises `subscribe` only when a reachable connector does);
- serve `resources/subscribe`/`resources/unsubscribe`;
- answer `GET` on its MCP endpoint with a Streamable HTTP SSE stream.
  A connector that answers `405` or `404` has no stream: the subscribe
  fails with `-32601` (`"cannot push resource updates: it offers no
  server-to-client stream"`), nothing is forwarded, and the gateway does
  not ask that connector again for the session.

The stream is not a health signal. An idle stream has no timeout
(`timeout_ms` bounds only the wait for its response headers), and a
stream that drops is reopened with backoff (1s → 30s, jittered),
resuming with `Last-Event-ID` when the connector numbers its events;
neither marks the connector unhealthy. It closes when the session's last
subscription on that connector is unsubscribed, or the session ends
(`DELETE /mcp`, or idle expiry). Per session, at most
`mcp.max_subscriptions_per_session` subscriptions (default 256) and
`mcp.max_upstream_streams_per_session` streams (default 16, one per
connector) are held; past either, the subscribe is `-32600`.

The same stream also carries the connector's `notifications/*/list_changed`
(relayed; see
[architecture.md](architecture.md#upstream-streams-and-subscriptions)),
and any request the connector sends on it (`sampling/createMessage`,
`roots/list`, …) is relayed to the agent or refused as described under
[Server-initiated requests](#server-initiated-requests-sampling-elicitation-roots).

## Tool cache

The tool cache (Postgres-backed, `tool_cache` table) makes `tools/list` a
single read instead of a live fan-out to every backend. It holds tools
only; `prompts/list`, `resources/list` and `resources/templates/list`
are always fetched live. Its admin routes:

```sh
curl http://localhost:8081/api/v1/cache/stats     -H "Authorization: Bearer $GATEWAY_KEY"
curl "http://localhost:8081/api/v1/cache/search?q=deploy&limit=20&include_stale=false" \
     -H "Authorization: Bearer $GATEWAY_KEY"
curl -X POST http://localhost:8081/api/v1/cache/refresh \
     -H "Authorization: Bearer $GATEWAY_KEY"
curl -X POST http://localhost:8081/api/v1/cache/refresh/connectors/$ID \
     -H "Authorization: Bearer $GATEWAY_KEY"
curl -X DELETE http://localhost:8081/api/v1/cache \
     -H "Authorization: Bearer $GATEWAY_KEY"
curl -X DELETE http://localhost:8081/api/v1/cache/connectors/$ID \
     -H "Authorization: Bearer $GATEWAY_KEY"
```

`refresh` re-discovers tools live and rewrites the cache (bumping
`cached_at`/`expires_at`); `invalidate`/`DELETE` only marks/drops rows,
it does not re-discover. `search` excludes stale rows unless
`include_stale=true` is passed, and is full-text indexed (Postgres
`tsvector`, weighted tool name > namespace > description). Serving
policy is stale-while-revalidate: with `tool_cache.serve_stale: true`
(the default) an expired entry is still served while a background
refresh runs, rather than blocking the caller's `tools/list` on a slow
backend.

**Deleting a connector removes its cached tools immediately.**
`DELETE /api/v1/connectors/{id}` invalidates that connector's
`tool_cache` rows as part of the same request, so its tools stop
appearing in `tools/list` right away instead of lingering until the
cache's TTL (default 30 minutes) or the next cleanup pass. As a second
safeguard, every tool-cache read also excludes rows whose
connector has been soft-deleted, so a deleted connector's tools never
surface even if that invalidation fails (it is best-effort and only
logged) or the process crashes between the two writes.

## Troubleshooting

**Connector shows `unhealthy`.** A failed `initialize` (handshake or
`tools/list`, depending on where it happened) marks a connector
unhealthy and starts a 30-second cool-down
(`internal/dataplane/router.defaultCoolDown`). During the cool-down every
`tools/call` to it is rejected with JSON-RPC `-32005`
(`ErrorCodeConnectorUnhealthy`) rather than retried; after it elapses, the
**next** call is let through as a half-open probe — if it succeeds, the
connector flips back to healthy immediately, no manual action needed.
`GET .../health` or `POST .../discover` also probe on demand and update
status right away, bypassing the cool-down wait.

**`TOOL_TIMEOUT` in a tool's result.** A backend call that exceeds its
timeout (`connector.timeout_ms`, which defaults to
`connectors.default_timeout_ms`, 30000 ms unless configured) is reported as tool **content**,
not a protocol error — the response has `isError: true` and text starting
`TOOL_TIMEOUT: the call to "..." exceeded its time limit and was stopped.
It was NOT retried, to avoid duplicating load on the backend.` A timeout
is deliberately never retried by the gateway (a retry would double the
load on a backend that's already too slow, and risks executing a
side-effecting tool twice). If this recurs for one connector, raise its
`timeout_ms`, or ask the caller to narrow the query per the message's own
suggestions.

**A connector answers 401 mid-session.** The client automatically evicts
that connector's cached credentials (if the header's provider implements
`Invalidator` — `secret_store` does) and replays the call exactly once.
A second consecutive 401 is surfaced as a tool-execution error
(`-32002`) rather than retried further — this usually means the
underlying credential (in the secret store, or the `env`/`file` source)
is genuinely stale and needs rotating (see "Credentials and rotation"
above), not a gateway-side caching bug.
