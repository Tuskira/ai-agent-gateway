# 05 - Agent profiles

## Goal

One MCP server, two agent profiles, one agent key. Show that:

- A **connector** is a server the gateway proxies (here, the reference
  `@modelcontextprotocol/server-everything`, same as 01-quickstart).
- A **profile** is a named allow-list of tools -- "Echo Only" grants 2 of
  that server's tools, "Full Access" grants every tool on every connector
  in the tenant (just this one server's, on a stack where nothing else
  has registered a connector of its own).
- A **header** (`X-Agent-Profile-Name`) picks which allow-list a given MCP
  session uses, per request.
- A **key** is who you are (an `agent`-role API key here), separate from
  which allow-list you're using.

Calling a tool your profile doesn't grant fails with a specific JSON-RPC
error (`-32003`); naming a profile that doesn't exist grants nothing, not
everything; and leaving the header off serves every tool the tenant owns
-- unless the gateway is put into **strict mode**, which then requires the
header on every MCP request.

## Prerequisites

- Docker and the `docker compose` plugin
- `curl` and `jq`
- Node.js (`npx` ships with npm) -- only needed to run the reference
  "everything" MCP server locally
- Nothing else: no API keys, no cloud account, no LLM provider credentials.

This example is self-contained: it does not assume 01-quickstart has been
run first, but reuses whatever it already left running (the "everything"
server, the compose stack, the "everything" connector) instead of
duplicating it.

## Steps

Run the whole thing with:

```sh
./examples/05-profiles/run.sh
```

It's safe to re-run: every step checks what already exists and reuses it,
with one exception -- step 3 always mints a fresh agent API key, since an
API key's plaintext is only ever returned once (see that step below).

What `run.sh` does, in order:

**1. Bring up the "everything" MCP server, the compose stack, an admin
key, and the "everything" connector.** Identical to 01-quickstart's steps
1-6 -- see [01-quickstart/README.md](../01-quickstart/README.md) for the
full detail. `run.sh` reuses each one if it's already there (server
listening on `:23014`, compose already up, connector already registered)
rather than duplicating it.

**2. Create the two profiles and grant their tools.** `POST /profiles`
isn't idempotent on its own -- a second `POST` with the same name is a
`409`, since the slug is derived from the name and is unique among live
profiles (`000009_live_slugs.up.sql`) -- so `run.sh` looks a profile up by
name first, the same pattern 01-quickstart uses to look up the connector
by slug:

```sh
# look up by name; only create if missing
curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "http://localhost:8081/api/v1/profiles?limit=500" \
  | jq -r '.items[] | select(.name=="Echo Only") | .id'

curl -sSf -X POST http://localhost:8081/api/v1/profiles \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "Echo Only"}'
# -> {"id": "...", "name": "Echo Only", "slug": "<tenant id>-echo-only", ...}
```

`PUT /profiles/{id}/tools` **fully replaces** the allow-list (it's not a
merge), which makes it naturally idempotent -- `run.sh` calls it every
run, whether or not the profile already existed:

```sh
curl -sSf -X PUT http://localhost:8081/api/v1/profiles/$ECHO_PROFILE_ID/tools \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"tools": [
        {"connector_id": "'$CONNECTOR_ID'", "tool_name": "echo"},
        {"connector_id": "'$CONNECTOR_ID'", "tool_name": "get-sum"}
      ]}'
```

"Full Access" is granted every tool on every connector currently in the
tenant (whatever that count turns out to be -- see the note on the "10"
floor in 01-quickstart; `run.sh` loops `GET /connectors` and discovers
each one live rather than hardcoding anything), instead of the 2-tool
list above. It's scoped tenant-wide, not just to "everything", because
the no-header check two steps down compares against it, and a caller
with no `X-Agent-Profile-Name` header sees every tool the tenant owns
across every connector -- not just this one.

**3. Mint an agent-role API key.** Unlike the profiles and connector
above, this step is **not** idempotent by design: an API key's plaintext
(the `"key"` field) is only returned once, at creation
(`internal/api/handlers/apikeys.go`, `APIKeys.Create`) -- there's nothing
to "look up and reuse". Every run mints a fresh key named
`05-profiles-agent`:

```sh
curl -sSf -X POST http://localhost:8081/api/v1/api-keys \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "05-profiles-agent", "role": "agent"}'
# -> {"id": "...", "name": "05-profiles-agent", "role": "agent",
#     "prefix": "gk_...", "key": "gk_...", ...}   <- "key" is the plaintext
```

The `agent` role only grants the MCP and LLM planes plus read-only access
to the control plane (`pkg/auth/auth.go`: `"agent": {"mcp.*", "llm.*",
"*.read"}`) -- it cannot create profiles or API keys itself, which is why
setup above runs on the admin key and only the calls in the next step run
on this one.

**4. `tools/list`, scoped per profile.** Same MCP handshake as
01-quickstart (`initialize` mints a session, `notifications/initialized`,
then calls carrying `Mcp-Session-Id`), authenticated as the agent key from
step 3. The profile header is **not** remembered on the session -- it's
read fresh off every request -- so one session is reused for every call
below, varying only the header:

```sh
curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $AGENT_KEY" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -H "X-Agent-Profile-Name: Echo Only" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
# -> exactly 2 tools: everything__echo, everything__get-sum

curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $AGENT_KEY" -H "Mcp-Session-Id: $SESSION_ID" \
  -H "X-Agent-Profile-Name: Full Access" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/list"}'
# -> every discovered tool

curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $AGENT_KEY" -H "Mcp-Session-Id: $SESSION_ID" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":4,"method":"tools/list"}'
# -> no header: same count as "Full Access" -- every tool the tenant owns
```

**5. `tools/call` on a granted vs. ungranted tool.** The allow-list is
enforced on the call, not just the list (`docs/profiles.md`): listing the
tenant's tools and then trying to call one your profile doesn't grant
fails with JSON-RPC error `-32003`
(`pkg/mcp/jsonrpc.go`, `ErrorCodeToolNotAllowed`; enforced in
`internal/dataplane/orchestrator/orchestrator.go`'s `handleToolsCall`,
*after* routing, since a grant is per `(connector, tool)` pair):

```sh
curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $AGENT_KEY" -H "Mcp-Session-Id: $SESSION_ID" \
  -H "X-Agent-Profile-Name: Echo Only" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"everything__get-env","arguments":{}}}'
# -> {"error": {"code": -32003, "message": "tool not allowed by profile", ...}}

curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $AGENT_KEY" -H "Mcp-Session-Id: $SESSION_ID" \
  -H "X-Agent-Profile-Name: Echo Only" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"everything__echo","arguments":{"message":"hello from Echo Only"}}}'
# -> succeeds, echoes the message back
```

**6. An unknown profile name grants nothing.** This is the one
counter-intuitive rule the whole example exists to demonstrate: a typo or
a stale profile name does not fall back to "every tool" -- it resolves to
an allow-list with nothing on it
(`internal/dataplane/profile/profile.go`'s doc comment spells out why:
falling back to every tenant tool on an unresolved name would silently
*widen* access on a typo instead of narrowing it).

```sh
curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $AGENT_KEY" -H "Mcp-Session-Id: $SESSION_ID" \
  -H "X-Agent-Profile-Name: does-not-exist" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":7,"method":"tools/list"}'
# -> {"result": {"tools": []}}  -- not an error, just nothing
```

### Strict mode

By default (`mcp.require_profile: false`, the same on
`GATEWAY_MCP_REQUIRE_PROFILE`), a request with no `X-Agent-Profile-Name`
header is served every tool its tenant owns -- profile scoping is opt-in.
Setting `mcp.require_profile: true` makes it mandatory: a request with no
header is rejected outright with the same `-32003` error, rather than
being served or silently narrowed
(`internal/dataplane/orchestrator/orchestrator.go`'s `resolveProfile`;
message: `"an agent profile is required: send the X-Agent-Profile-Name
header"`).

**7. `run.sh` flips this on, checks it, then flips it back off.** There is
no separate config file for this -- `deploy/docker-compose.yml` forwards
the env var, defaulted off, and this step overrides it for one `up -d
gateway`:

```sh
GATEWAY_MCP_REQUIRE_PROFILE=true docker compose -f deploy/docker-compose.yml up -d gateway
```

Recreating the `gateway` container drops every in-memory MCP session along
with it, so `run.sh` re-`initialize`s before sending anything else. It
then confirms:

- No header -- denied, `-32003` (same error as an ungranted `tools/call`,
  since both go through `resolveProfile`).
- `X-Agent-Profile-Name: Echo Only` -- still works, still exactly 2 tools.

...and restores `GATEWAY_MCP_REQUIRE_PROFILE=false` (another `up -d
gateway` + health wait) whether the checks above passed or the script
failed partway through -- `run.sh` registers this restore as a `trap` on
exit specifically so a failed assertion never leaves your local stack
stuck in strict mode.

## Expected output

- Steps 1-6 from 01-quickstart succeed identically (health on all three
  planes, connector healthy, tools discovered).
- `Echo Only` grants exactly `everything__echo` and `everything__get-sum`
  -- 2 tools.
- `Full Access` grants every tool discovery found, on every connector in
  the tenant.
- No header matches `Full Access`'s count (every tenant tool).
- `everything__get-env` under `Echo Only` fails with `-32003`;
  `everything__echo` under `Echo Only` succeeds and echoes the message.
- `X-Agent-Profile-Name: does-not-exist` returns `{"tools": []}`, not an
  error and not every tool.
- Under strict mode, no header is denied with `-32003`; `Echo Only` still
  works.
- `run.sh` finishes by printing the console URL, the headers a client
  should send, and the agent key once:

  ```
  ================================================================
  05-profiles passed.
  Console:    http://localhost:8081
  Agent key:  gk_...

  A client should send, on every MCP request:
    Authorization: Bearer gk_...
    X-Agent-Profile-Name: Echo Only
  ================================================================
  ```

## Cleanup

```sh
# Stop the local "everything" MCP server, if this run started it
kill "$(cat examples/05-profiles/.everything.pid)" 2>/dev/null || true
rm -f examples/05-profiles/.everything.pid examples/05-profiles/.everything.log

# Stop the gateway stack
docker compose -f deploy/docker-compose.yml down
```

Profiles, connectors, and API keys all live in the stack's Postgres, not
in this directory -- `down` alone leaves them in the volume for the next
run to reuse. `down -v` also drops that volume, which wipes them (and
01-quickstart's connector) for good.

Every re-run of this example mints one more `05-profiles-agent` API key
(step 3) without revoking the previous one -- by design, to keep the
script simple (see that step above). They're harmless to leave, but if you
want to tidy them up:

```sh
curl -sS -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "http://localhost:8081/api/v1/api-keys?limit=500" \
  | jq -r '.items[] | select(.name=="05-profiles-agent") | .id' \
  | while read -r id; do
      curl -sS -X DELETE "http://localhost:8081/api/v1/api-keys/$id" \
        -H "Authorization: Bearer $GATEWAY_ADMIN_KEY"
    done
```
