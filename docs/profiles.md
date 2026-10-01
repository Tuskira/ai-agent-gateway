# Agent profiles

An **agent profile** is a named allow-list of `(connector, tool)` pairs (a
connector is an MCP server registered with the gateway; the console's
**MCPs** page) — what one class of agent may see and run, independent of which API key it
authenticates with. Profiles are created and edited under
`/api/v1/profiles` and enforced by `internal/dataplane/profile.Enforcer`.

Two things make this an enforcement mechanism rather than a filter:

- The allow-list is checked on `tools/call`, not only on `tools/list`.
  Filtering a list an agent can simply ignore is a suggestion; a denial on
  the call is a control.
- **A profile name that does not resolve grants nothing.** There is no
  fallback to "every tool in the tenant" for an unknown or misspelled
  profile — a typo in the header narrows access to zero, it never widens
  it.

## `X-Agent-Profile-Name`

Every MCP request may carry `X-Agent-Profile-Name: <name>`. The gateway
normalizes it (lowercase, non-`[a-z0-9]` runs collapsed to one hyphen —
`"SOC Analyst (L1)"` and `"soc_analyst-l1"` both become
`soc-analyst-l1`) and looks up `agent_profiles` by
`(tenant, "<tenant-id>-<normalized-name>")`. The resolved allow-list is
cached for 30 seconds per `(tenant, profile)`.

The slug is fixed when the profile is created: renaming a profile does
not change it, so the header must keep using the original name (a key
bound to the profile accepts either name). Send the profile's `name`, not
the `slug` the API returns: the slug already carries the `<tenant-id>-`
prefix and resolves to nothing as a header.

| Situation | Behavior |
|---|---|
| No `X-Agent-Profile-Name` header, `mcp.require_profile: false` (default) | Every tool in the tenant is visible and callable — profile scoping is opt-in per request. |
| No header, `mcp.require_profile: true` | Rejected: JSON-RPC `-32003`, `"an agent profile is required: send the X-Agent-Profile-Name header"`. |
| Header names a profile that doesn't exist | Not an error — resolves to an `AllowList` with `Found: false`, which grants **nothing**. `tools/list` returns `[]`; any `tools/call` is denied with `-32003`. |
| Header names a real profile | Only the `(connector, tool)` pairs on that profile's allow-list are visible/callable. |

The header is **chosen by the caller**, so on its own it scopes cooperative
agents rather than confining a hostile one: any key can name any profile its
tenant owns, and `require_profile` only forces the header to be present.
To confine a key, bind it to a profile (next section).

### Binding a key to a profile

Create the key with `profile_id` (`POST /api/v1/api-keys`, or the console's
"Bind to profile" select), or set/clear it later with
`PATCH /api/v1/api-keys/{id}` (`{"profile_id":"<id>"}` / `{"profile_id":null}`).
The profile must belong to the key's tenant.

| Situation (key bound to profile A) | Behavior |
|---|---|
| No `X-Agent-Profile-Name` header | Profile A is enforced (even when `require_profile` is `false`). |
| Header names A (any spelling that normalizes to it) | Same as above. |
| Header names a different profile | Rejected: JSON-RPC `-32003`. |
| A was deleted | Rejected with `-32003` (never falls back to "every tool"). |

Rotating a key keeps its binding. A binding change applies at once on the
process that served the `PATCH`; other gateway processes pick it up within
`auth.api_keys.cache_ttl` (30 s), since unlike a revoke it is not
broadcast. A key with no `profile_id` behaves exactly
as described above. Binding applies to the MCP plane only; the LLM plane does
not use profiles.

`tools/list` filters silently (an empty result for a profile that grants
nothing); `tools/call` returns the explicit error:

```json
{"jsonrpc": "2.0", "id": 1,
 "error": {"code": -32003, "message": "tool not allowed by profile",
           "data": {"tool": "github__delete_repo", "profile": "read-only-analyst"}}}
```

The profile check runs **after** routing, because a grant is per
`(connector, tool)` pair — `"search"` on one connector is a different
grant from `"search"` on another, and only routing resolves which one a
call means.

## Prompts and resources

A profile's allow-list names tools only. Connectors' prompts and
resources ride on it with one **default-all** rule: a connector's prompts
and resources are visible and usable exactly when the profile grants
**at least one tool of that connector** — then all of them, none
otherwise (`profile.AllowList.AllowsConnector`).

| Situation | `prompts/list`, `resources/list`, `resources/templates/list` | `prompts/get`, `resources/read` |
|---|---|---|
| No header, `mcp.require_profile: false` | every connector's | allowed on every connector |
| No header, `mcp.require_profile: true` | `-32003` | `-32003` |
| Header names a profile that doesn't exist | `[]` | `-32003` |
| Header names a real profile | only connectors with ≥1 granted tool | allowed on those connectors, `-32003` elsewhere |

The denial names the connector:

```json
{"jsonrpc": "2.0", "id": 1,
 "error": {"code": -32003,
           "message": "prompt not allowed by profile: the profile grants no tool on connector \"github\"",
           "data": {"prompt": "github__triage", "profile": "read-only-analyst"}}}
```

(`resources/read` says `resource` and carries `"resource": "gw://…"`.)
Per-prompt and per-resource grants are not supported.

## Managing a profile's tools

```sh
curl -X PUT http://localhost:8081/api/v1/profiles/$PROFILE_ID/tools \
  -H "Authorization: Bearer $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d '{"tools": [
        {"connector_id": "'$CONNECTOR_ID'", "tool_name": "search"},
        {"connector_id": "'$CONNECTOR_ID'", "tool_name": "get_issue"}
      ]}'
```

`SetTools` fully replaces the allow-list (not a merge). `tool_name` is the
backend's own, unprefixed name (`search`, not `github__search`); the
handler validates that every `connector_id` exists in the caller's own
tenant before writing, so a profile can never reference another tenant's
connector even by guessing its id. The response is the new list in the
same `{items, total}` shape as `GET .../tools`. Each entry may also carry
an optional `tool_namespace`.

## Skills and commands

A profile can also carry free-text **instructions** and a set of
attached **skills** and **commands** from the [skills & commands
registry](skills.md) (`/api/v1/skills`). Both ride on the same
`internal/dataplane/profile.Enforcer` resolution as the tool allow-list,
and the same rule applies: **attachment is a grant** -- a skill or
command not attached to the profile in play is invisible and uncallable,
whether or not it exists in the registry.

| Field | Set via | Carried into |
|---|---|---|
| `instructions` | `PUT /profiles/{id}` (≤ 8 KiB; a full replace, so send `name` and `description` too) | prepended to `initialize`'s `instructions` |
| skills/commands | `PUT /profiles/{id}/skills` (`{"items":[{"skill_id", "version"?}]}`, replace semantics like `/tools`) | the native `gateway__skill` tool and native command prompts, below |

```sh
curl -X PUT http://localhost:8081/api/v1/profiles/$PROFILE_ID/skills \
  -H "Authorization: Bearer $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d '{"items": [{"skill_id": "'$SKILL_ID'"}, {"skill_id": "'$COMMAND_ID'", "version": 2}]}'
```

Every `skill_id` must be visible to the caller's tenant (a tenant row or
a platform row) and, when `version` is set, that version must already
exist -- both checked before the write, the same way `SetTools` checks
`connector_id` ownership. Omitting `version` floats the attachment to
the skill's current `latest_version`; pinning one lets it fall behind
(see [skills.md](skills.md), "outdated").

### `initialize`: instructions and the skill index

A resolved profile's `instructions` are prepended to the gateway's static
pointer text, and a skill index is appended after it:

```
Always cite the alert id.

Tool gateway. Tools and prompts are named "<connector>__<name>"; call them by that name. ...

Skills available to this profile. Load one with the tool `gateway__skill` (argument `name`, optional `path`, default SKILL.md):
- triage-guide: How to triage an alert
- summarize-case: Summarize a case for handoff
```

The index lists only attached **skills** (not commands), sorted by name,
capped at 50 entries or 8 KiB of text, whichever comes first,
with a trailing `"… and N more"` past the cap. Commands don't need an
index entry: they show up in `prompts/list` instead (below). A profile
with a command attached also gets the server's `prompts` capability
advertised, even if no connector has one.

With no profile in play (no header, or one that doesn't resolve),
`initialize`'s instructions are exactly the static pointer text -- no
addition, no skill index, matching the "no profile" rule for tools.

### The native `gateway__skill` tool

Listed in `tools/list` only when the resolved profile has at least one
attached skill:

```json
{"name": "gateway__skill",
 "inputSchema": {"type": "object",
   "properties": {"name": {"type": "string"}, "path": {"type": "string"}},
   "required": ["name"]}}
```

`path` defaults to `SKILL.md`. It is never routed to a connector --
`"gateway"` is a reserved connector name/slug (`400` on
create/update) precisely so a real connector's tools can never be
qualified `gateway__*` and collide with it:

```json
{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
 "params": {"name": "gateway__skill", "arguments": {"name": "triage-guide"}}}
→ {"content": [{"type": "text", "text": "---\nname: triage-guide\n...\n---\nStep 1. ..."}]}
```

An unattached or unknown skill name is `-32003` (the same code a denied
tool uses), with `{"skill": "...", "profile": "..."}`; an unknown `path`
within an attached skill is `-32602`.

### Commands as native prompts

A profile's attached `kind: "command"` rows are served as native MCP
prompts, merged into `prompts/list` alongside connectors' own (plain
names, no `__` -- that's how the gateway tells a native command apart
from `<connector>__<prompt>` on `prompts/get`):

```json
{"jsonrpc": "2.0", "id": 1, "method": "prompts/get",
 "params": {"name": "summarize-case", "arguments": {"case_id": "CASE-42"}}}
→ {"description": "Summarize a case for handoff",
   "messages": [{"role": "user", "content": {"type": "text", "text": "Summarize case CASE-42...."}}]}
```

`{{name}}` placeholders in the command's `SKILL.md` body are substituted
from `arguments`; a missing required argument is `-32602`, and a command
name not attached to the profile is `-32003` -- same shape as
`gateway__skill`'s denial, with `"skill"` naming the command.

### The MCP Skills Extension

A client that speaks the [MCP Skills
Extension](https://modelcontextprotocol.io/extensions/skills/overview)
(SEP-2640) natively can discover and read a profile's attached **skills**
(never commands) the same way it already discovers resources:
`skills/list`, `skills/get`, and `resources/read` on `skill://<name>/<path>`
URIs, gated by the same attachment rule as `gateway__skill` above --
unattached or unknown is `-32003`. See
[architecture.md, "MCP Skills Extension
(SEP-2640)"](architecture.md#mcp-skills-extension-sep-2640) for the wire
shapes, and [skills.md, "Delivery
paths"](skills.md#delivery-paths) for how this fits alongside the
native tool/prompts path above and the optional file-sync path.

### Cache and invalidation

Every write that could change a resolved profile -- `PUT /profiles/{id}`
(name, instructions), `.../tools`, `.../skills`, `DELETE
/profiles/{id}` -- drops that profile's cached resolution immediately on
the replica that served the write (`pkg/ops.ProfileOps`, the same seam
`CacheOps` uses for the tool cache). A skill/command registry write (a
new version, enable/disable, delete) drops *every* cached profile
resolution in the tenant instead: finding exactly which profiles
reference a given skill isn't worth a query, and the cache's own 30s TTL
already bounds the cost. **In a split deployment** (separate `api` and
`mcp` replicas, or several `mcp` replicas), only the replica that served
the write is invalidated immediately -- every other replica still serves
its cached resolution for up to the TTL, the same limit that already
applies to a tool-allow-list change. With separate `api` and `mcp`
deployments the write is served by an `api` pod, so no MCP replica is
invalidated immediately: the change reaches them within the 30 s TTL.

### Live updates over the SSE stream

`PUT /profiles/{id}/skills`, `PUT /profiles/{id}/tools`, `PUT
/profiles/{id}` (instructions) and `DELETE /profiles/{id}` also push a
live `notifications/tools/list_changed` **and**
`notifications/prompts/list_changed` to any `GET /mcp/stream` currently
open with a matching `X-Agent-Profile-Name` header (or opened with a key
bound to that profile, whatever the header says), in addition to
dropping the cached resolution above. Both notifications are sent
unconditionally (a client that never declared the `prompts` capability
simply ignores the one it doesn't understand, per the MCP spec), since a
skill attach/detach can affect `tools/list` (the `gateway__skill` tool's
own advertised presence), `prompts/list` (native commands), or both.

This rides the same per-process reach as the cache invalidation above --
`internal/dataplane/transport.Hub.NotifyProfileListChanged`, keyed by
`(tenant, profile slug)` exactly like `Enforcer.InvalidateSlug`. A stream
opened against a different replica than the one that served the write
does not receive it; that replica's own streams still catch up once its
local enforcer cache TTL lapses, or when the client independently
re-lists. There is no cross-replica relay for this signal (unlike
the tenant-wide `tools/list_changed` used for connector cache
invalidation, which the `redis` session driver *does* relay across
replicas) -- a split multi-replica `mcp` deployment should treat this as
a same-replica fast path on top of the TTL, not a substitute for it.

## Example: Claude Code

Claude Code connects to an MCP server over HTTP. Point it at the gateway
and add the profile header via its transport's custom-header support (or
front it with any HTTP client that adds headers), e.g. in
`.mcp.json`:

```json
{
  "mcpServers": {
    "gateway": {
      "type": "http",
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer gk_...",
        "X-Agent-Profile-Name": "soc-analyst-l1"
      }
    }
  }
}
```

## Example: curl

```sh
# initialize (mints a session)
curl -i -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_KEY" \
  -H "X-Agent-Profile-Name: soc-analyst-l1" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize",
       "params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"curl","version":"0"},"capabilities":{}}}'
# → response carries Mcp-Session-Id: <id> in a response header

# tools/list, scoped to the same profile
curl -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_KEY" \
  -H "X-Agent-Profile-Name: soc-analyst-l1" \
  -H "Mcp-Session-Id: <id>" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

The profile header must be sent on every request that should be scoped by
it — it is not remembered on the session; a session merely remembers
backend handles and protocol versions per connector.

## Profile Studio

The embedded console's Profiles page (`/profiles`) includes **Profile
Studio**, a full-screen editor (`web/src/components/app/profiles/ProfileStudioModal.tsx`)
for building a profile's tool allow-list: the tenant's connectors and
their cached tools on one side, the profile's current grants on the
other. It reads from the same `GET /connectors/{id}/tools` and
`GET/PUT /profiles/{id}/tools` routes documented above and in
[connectors-and-credentials.md](connectors-and-credentials.md) — there is
no separate profile-authoring API.

Its "Skills & commands" tab (`ProfileSkillsTab.tsx`) shows the same
version-pin dropdown described above, plus an **"Outdated"** badge on any
row pinned to a version behind the registry's current `latest_version`
and a one-click **"Update to latest"** button that clears the pin back to
"track latest" (`version: null`) -- like every other edit in this tab,
that's a local draft change, not applied until Save PUTs the full
attached set.
