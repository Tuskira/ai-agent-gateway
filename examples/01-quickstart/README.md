# 01 - Quickstart

## Goal

Stand up the gateway locally, register one real MCP tool server (the
reference `@modelcontextprotocol/server-everything`) as a connector (the
console's **MCPs** page), and call one of its tools -- then fetch one of
its prompts and read one of its resources -- through the gateway's MCP
plane, with a health check on every plane along the way.

## Prerequisites

- Docker and the `docker compose` plugin
- `curl` and `jq`
- Node.js (`npx` ships with npm) -- only needed to run the reference
  "everything" MCP server locally
- Nothing else: no API keys, no cloud account, no LLM provider credentials.

## Steps

Run the whole thing with:

```sh
./examples/01-quickstart/run.sh
```

It's safe to re-run: every step below checks what already exists and
reuses it instead of failing or duplicating work. Run it a second time
against the same stack to see that for yourself.

What `run.sh` does, in order:

**1. Start the MCP server this example registers as a connector** -- the
reference `@modelcontextprotocol/server-everything`, over Streamable HTTP,
on port 23014:

```sh
PORT=23014 npx -y @modelcontextprotocol/server-everything streamableHttp &
echo $! > examples/01-quickstart/.everything.pid
```

If something is already listening on `:23014`, `run.sh` reuses it instead
of starting a second copy.

**2. Start the gateway's compose stack** (Postgres and the gateway):

```sh
docker compose -f deploy/docker-compose.yml up -d
```

**3. Check health on all three planes:**

```sh
curl -sSf http://localhost:8080/health          # MCP plane
curl -sSf http://localhost:8081/api/v1/health   # control plane (REST + console)
curl -sSf http://localhost:8082/health          # LLM plane
```

**4. Bootstrap an admin API key.** `bootstrap-key` prints exactly one line
to stdout -- the plaintext key -- so it's safe to capture directly:

```sh
GATEWAY_ADMIN_KEY=$(docker compose -f deploy/docker-compose.yml exec -T gateway /gateway bootstrap-key 2>/dev/null)
```

Already bootstrapped this stack before? `bootstrap-key` refuses to create
a second admin key for the same tenant unless you pass `--force` (its
plaintext can't be recovered once printed) -- `run.sh` detects that
refusal and mints a new one with `--force` automatically. You can also
skip bootstrapping entirely by exporting an existing key first:
`export GATEWAY_ADMIN_KEY=gk_...`.

The API key is how this walkthrough (and any program) talks to the
gateway. To sign in to the console as a person instead, create a user:
`docker compose -f deploy/docker-compose.yml exec gateway /gateway create-user -tenant default -username admin -role admin`
prints a one-time temporary password you change at first login (see
[docs/authentication.md](../../docs/authentication.md)). The console still
accepts the key above as an emergency fallback.

**5. Register the "everything" server as a connector.** From inside the
gateway container, the host machine is `host.docker.internal`:

```sh
CONNECTOR_ID=$(curl -sSf -X POST http://localhost:8081/api/v1/connectors \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
        "name": "everything",
        "slug": "everything",
        "endpoint": "http://host.docker.internal:23014/mcp"
      }' | jq -r .id)
```

`run.sh` looks the connector up by slug first and reuses it. By hand, a
second `POST` with the same slug is a `409`; get the existing id with:

```sh
CONNECTOR_ID=$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  http://localhost:8081/api/v1/connectors | jq -r '.items[] | select(.slug=="everything") | .id')
```

**6. Probe its health and discover its tools** (discovery writes the tool
cache):

```sh
curl -sSf -X GET http://localhost:8081/api/v1/connectors/$CONNECTOR_ID/health \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY"

curl -sSf -X POST http://localhost:8081/api/v1/connectors/$CONNECTOR_ID/discover \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY"
```

**7. Call it over the MCP plane.** Tools are namespaced
`<connector slug>__<tool name>`, so the "everything" server's `echo` tool
is `everything__echo`. `initialize` mints a session id, returned in the
`Mcp-Session-Id` response header:

```sh
curl -sS -D /tmp/mcp-headers.txt http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"01-quickstart","version":"0.1.0"}}}'

SESSION_ID=$(grep -i '^Mcp-Session-Id:' /tmp/mcp-headers.txt | sed -E 's/^[^:]+:[[:space:]]*//' | tr -d '\r\n')
```

Then the client's `notifications/initialized`, `tools/list`, and
`tools/call`, all carrying that session id:

```sh
curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'

curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'

curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"everything__echo","arguments":{"message":"hello from the gateway"}}}'
```

The server's prompts and resources come through the same session.
Prompt names are namespaced exactly like tools, so its `simple-prompt`
is `everything__simple-prompt`. Resource URIs already carry a scheme of
their own, so the gateway wraps the whole URI instead:
`demo://resource/static/document/architecture.md` is advertised as
`gw://everything/demo://resource/static/document/architecture.md`, and
`resources/read` takes that wrapped URI back:

```sh
curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -d '{"jsonrpc":"2.0","id":4,"method":"prompts/list"}'

curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -d '{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":{"name":"everything__simple-prompt"}}'

curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -d '{"jsonrpc":"2.0","id":6,"method":"resources/list"}'

curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -d '{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"gw://everything/demo://resource/static/document/architecture.md"}}'
```

`run.sh` sends these same requests in this order through two small bash
helpers that also check the HTTP status and fail on a JSON-RPC `"error"`
in the body (a tool-level failure is still HTTP `200` with an `error`
object, so the status alone isn't enough). The gateway doesn't strictly
require `initialize` before `tools/list` -- it handshakes with each
connector on demand -- but it's the standard MCP lifecycle, so the
example follows it.

### Progress and cancellation

A long tool call can report progress and be cancelled. Open the
session's notification stream in one terminal:

```sh
curl -sN http://localhost:8080/mcp/stream \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Mcp-Session-Id: $SESSION_ID"
```

then, in another, call server-everything's long-running tool with a
`progressToken`:

```sh
curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"everything__trigger-long-running-operation","arguments":{"duration":5,"steps":5},"_meta":{"progressToken":"demo"}}}'
```

The stream prints one `notifications/progress` frame per step for token
`demo`. To stop a call early, send (from a third terminal, while it runs)
`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":4}}`
on the same session: the gateway answers `204`, forwards the cancellation
to server-everything, and the `tools/call` returns error `-32800`
("request cancelled"). `run.sh` does not exercise this.

## Expected output

- `http://localhost:8080/health` and `:8082/health` each answer
  `{"status":"ok","plane":"mcp"|"llm",...}`.
- `http://localhost:8081/api/v1/health` answers
  `{"status":"ok","plane":"api",...}`.
- The connector health probe answers `{"status":"healthy",...}`.
- `tools/list` returns the server's tools named `everything__*`,
  including `everything__echo`. The count varies by release (`npx -y`
  pulls the latest), so `run.sh` asserts at least 10.
- `tools/call` on `everything__echo` returns content whose text contains
  `hello from the gateway`.
- `prompts/list` returns the server's prompts named `everything__*`
  (e.g. `everything__simple-prompt`); `run.sh` asserts at least one.
- `prompts/get` on `everything__simple-prompt` returns one user message,
  "This is a simple prompt without arguments."
- `resources/list` returns URIs under `gw://everything/` (the server's
  `demo://resource/static/document/*.md` files); `run.sh` reads the first
  one back with `resources/read` and asserts non-empty `contents`.
- `run.sh` finishes by printing the console URL and the admin key once,
  clearly labelled:

  ```
  ================================================================
  01-quickstart passed.
  Console:    http://localhost:8081
  Admin key:  gk_...
  ================================================================
  ```

## Cleanup

```sh
# Stop the local "everything" MCP server started in step 1
kill "$(cat examples/01-quickstart/.everything.pid)" 2>/dev/null || true
rm -f examples/01-quickstart/.everything.pid examples/01-quickstart/.everything.log

# Stop the gateway stack (add -v to also drop its Postgres volume)
docker compose -f deploy/docker-compose.yml down
```

## Next

- [02-claude-code](../02-claude-code/) -- point a real agent at the
  gateway instead of `curl`.
- [05-profiles](../05-profiles/) -- scope which of a connector's tools
  each agent can see and call.
- [examples/README.md](../README.md) -- the full index.
