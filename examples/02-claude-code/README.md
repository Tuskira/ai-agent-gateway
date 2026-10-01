# 02 - Claude Code

## Goal

Use the gateway as Claude Code's only MCP server, and -- optionally -- as
its LLM proxy too, so every tool call *and* every model call Claude Code
makes lands behind the same gateway.

- **MCP leg.** A project-scoped `.mcp.json` (`type: "http"`) points Claude
  Code at the gateway's MCP plane, scoped to one agent profile via
  `X-Agent-Profile-Name`.
- **LLM leg (optional).** `ANTHROPIC_BASE_URL` plus a header carrying the
  gateway's own key routes Claude Code's own model calls through the
  gateway too, without disturbing the Anthropic credential Claude Code
  already manages on its own.

## Prerequisites

- [Claude Code](https://claude.com/claude-code) installed and already able
  to run on its own -- a `claude login` session or `ANTHROPIC_API_KEY` set.
  This is independent of the gateway.
- A gateway running with a connector already registered (see
  [01-quickstart](../01-quickstart/)) and `GATEWAY_ADMIN_KEY` exported.

This example needs an agent-role API key and a profile. Rather than
sending you to [05-profiles](../05-profiles/) first, here are the two
calls inline:

```sh
# 1. Mint an agent-role API key (the plaintext "key" is shown once)
curl -sSf -X POST http://localhost:8081/api/v1/api-keys \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "claude-code", "role": "agent"}'
# -> {"id": "...", "key": "gk_...", ...}  -- save "key" as GATEWAY_KEY below

# 2. Create a profile for Claude Code to use
curl -sSf -X POST http://localhost:8081/api/v1/profiles \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "Full Access"}'
```

```sh
export GATEWAY_KEY=gk_...   # the "key" value from step 1
```

`(tenant_id, slug)` is unique among live (non-deleted) `agent_profiles`
rows (`000009_live_slugs.up.sql`), and the slug is derived from the name
(`internal/dataplane/profile.Slug`) -- so step 2 returns `409` if a
profile named "Full Access" already exists in this tenant. That's fine:
reuse it (`GET /api/v1/profiles`). Either way, a freshly created profile
grants **no** tools until you `PUT` its allow-list -- see
[05-profiles](../05-profiles/) step 2, or just run that example once
against this stack: it leaves a "Full Access" profile already granted
every tool the "everything" connector has.

## Steps

### 1. Register the gateway as an MCP server

Two ways to reach the same `.mcp.json` entry:

**Option A -- `.mcp.json`, checked into a shared repo.** This directory's
[`.mcp.json`](.mcp.json) is exactly this; copy it into your own project
and export `GATEWAY_KEY`:

```json
{
  "mcpServers": {
    "tusk-ai-gateway": {
      "type": "http",
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer ${GATEWAY_KEY}",
        "X-Agent-Profile-Name": "Full Access"
      }
    }
  }
}
```

`${GATEWAY_KEY}` is real
[env-var expansion Claude Code's `.mcp.json` supports](https://code.claude.com/docs/en/mcp)
in `headers` and `url` values (`${VAR}`, or `${VAR:-default}`) -- export
`GATEWAY_KEY` before starting Claude Code and it's substituted at connect
time, so no plaintext key needs to sit in a file you might commit. Two
things worth knowing before you rely on it:

- Claude Code deliberately does **not** expand a short list of its own
  credential env-var names here (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
  and a couple of cloud-provider ones), even if you happened to reuse one
  of those names for the gateway key -- another reason to call it
  `GATEWAY_KEY`, not one of those.
- `.mcp.json` header expansion has had version-specific bugs upstream. If
  `${GATEWAY_KEY}` shows up literally in a failed connection instead of
  being substituted, paste the plaintext key directly as a fallback:

  ```json
  "Authorization": "Bearer gk_the_actual_key_here"
  ```

**Option B -- `claude mcp add`**, via [`claude-mcp-add.sh`](claude-mcp-add.sh):

```sh
export GATEWAY_KEY=gk_...
./examples/02-claude-code/claude-mcp-add.sh
```

which runs:

```sh
claude mcp add --transport http tusk-ai-gateway http://localhost:8080/mcp \
  --header "Authorization: Bearer ${GATEWAY_KEY}" \
  --header "X-Agent-Profile-Name: Full Access"
```

Scope differs from Option A by default: `claude mcp add` with no `--scope`
adds a **local**-scope server -- private to you, stored in
`~/.claude.json`, not the project's `.mcp.json`. Add `--scope project` to
write the same entry into `.mcp.json` instead, matching Option A.

### 2. (Optional) Route Claude Code's own model calls through the gateway too

```sh
export ANTHROPIC_BASE_URL=http://localhost:8082
export ANTHROPIC_CUSTOM_HEADERS="X-Gateway-Key: $GATEWAY_KEY"
```

Why `X-Gateway-Key` and not `Authorization: Bearer $GATEWAY_KEY`? Claude
Code already puts its own real Anthropic credential on `Authorization` or
`x-api-key` -- an OAuth-derived bearer token from `claude login`, or
`x-api-key` if `ANTHROPIC_API_KEY` is set -- and that credential has to
reach Anthropic untouched. `X-Gateway-Key` is the dedicated header the
gateway's own authenticator also accepts for its key
(`internal/auth/apikey.ParseBearer`, checked *before* `Authorization`), so
the two credentials never collide on the same header
(`docs/security-model.md`: "the latter takes precedence, and does not
shadow a BYOK `Authorization` header on the LLM plane"). `ANTHROPIC_CUSTOM_HEADERS`
is a documented Claude Code setting for exactly this -- one or more
`Name: Value` pairs, newline-separated --
that adds a header without touching the ones Claude Code manages itself.

Claude Code also sends `X-Claude-Code-Session-Id` on every model call
automatically -- nothing to configure. The
gateway's LLM plane reads it as the call's session tag, falling back to
`X-Session-Id` if that's set instead, and strips both before forwarding
(`internal/llmplane/middleware.go`'s `withTags`):

```go
session := r.Header.Get("X-Session-Id")
if session == "" {
    session = r.Header.Get("X-Claude-Code-Session-Id")
}
```

That id is **separate** from the MCP plane's `Mcp-Session-Id` (minted by
the gateway itself, on `initialize`, for the MCP leg) -- Claude Code
doesn't tie the two together. So for a Claude Code session, Access Logs
and LLM Logs correlate by request time and principal, not by a shared id
-- unlike [04-python-agent](../04-python-agent/), a hand-written client
that deliberately reuses one id across both planes.

### 3. Verify

```sh
claude -p "list your MCP tools"
```

## Expected output

- `claude mcp list` shows `tusk-ai-gateway` under the `http` transport.
- `claude -p "list your MCP tools"` prints only the tools the profile
  (`X-Agent-Profile-Name`) grants -- not every tool the tenant owns, and
  not an error if the profile grants nothing (see
  [05-profiles](../05-profiles/) for exactly what an unknown or
  empty-allow-list profile returns).
- Every tool call shows up in **Access Logs** (`/access-logs`), grouped by
  the MCP session id the gateway minted at `initialize`.
- With step 2's env vars set, every model call also shows up in
  **LLM Logs** (`/llm-logs`) with tokens and cost, grouped by
  `X-Claude-Code-Session-Id`. Access Logs needs the ClickHouse sink, which
  is off by default locally (LLM Logs falls back to the Postgres capture
  table without it):

  ```sh
  GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose -f deploy/docker-compose.yml --profile analytics up -d
  ```

## Cleanup

```sh
claude mcp remove tusk-ai-gateway   # Option B; for Option A, just delete/revert .mcp.json
unset ANTHROPIC_BASE_URL ANTHROPIC_CUSTOM_HEADERS GATEWAY_KEY
```

The agent key and profile minted in Prerequisites live in the gateway's
Postgres, not in this directory -- see
[05-profiles](../05-profiles/)'s Cleanup section if you want to remove
them.
