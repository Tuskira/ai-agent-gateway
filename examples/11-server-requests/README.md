# 11 - Server-initiated requests (elicitation, roots)

## Goal

A tool asks the agent for something mid-call, and the gateway relays the
question and the answer. Show that:

- A **connector** only gets to ask when its policy says so:
  `metadata.server_requests` turns `sampling`, `elicitation` and `roots`
  on one by one, and all three are off by default.
- An **agent** only gets asked for what it declared in its own
  `initialize` (`capabilities.elicitation`, `capabilities.roots`); the
  gateway declares to the connector exactly what both allow.
- The request arrives on the agent's **`GET /mcp/stream`** as a JSON-RPC
  request with a gateway id (`gw-<16 hex>`), and the agent answers with a
  JSON-RPC **response** `POST`ed to `/mcp` (`202 Accepted`). The tool's
  result then carries the answer.
- With the policy **off**, the same call is refused (`-32601`) and
  nothing reaches the agent.

The server is the reference `@modelcontextprotocol/server-everything`,
which offers `trigger-elicitation-request`, `trigger-sampling-request`
and `get-roots-list` only to a client that declared the matching
capability. This example uses elicitation and roots; sampling works the
same way (see
[connectors-and-credentials.md](../../docs/connectors-and-credentials.md#server-initiated-requests-sampling-elicitation-roots)).

## Prerequisites

- Docker and the `docker compose` plugin
- `curl` and `jq`
- Node.js (`npx` ships with npm) -- only needed to run the reference
  "everything" MCP server locally
- Nothing else: no API keys, no cloud account, no LLM provider credentials.

If your compose stack was built before this release, rebuild the gateway
image first (`docker compose -f deploy/docker-compose.yml build gateway`):
an older gateway answers every server-initiated request `-32601`.

This example is self-contained: it does not assume 01-quickstart has been
run first, but reuses whatever it already left running (the "everything"
server, the compose stack) instead of duplicating it. It registers a
connector of its own, `everything-requests`, so switching its policy on
and off never changes the `everything` connector other examples use. That
connector is deleted again when `run.sh` exits (see
[Cleanup](#cleanup)); slugs are unique among live connectors only, so the
next run re-creates it under the same slug.

## Steps

Run the whole thing with:

```sh
./examples/11-server-requests/run.sh
```

It's safe to re-run: every run registers its own fresh connector (policy
on) and deletes it again on exit, so nothing from one run carries into
the next.

What `run.sh` does, in order:

**1. Bring up the "everything" MCP server, the compose stack and an admin
key.** Identical to 01-quickstart's steps 1-4 -- see
[01-quickstart/README.md](../01-quickstart/README.md).

**2. Register `everything-requests` with elicitation and roots allowed.**
Same server as `everything`, its own slug, and the policy in
`metadata.server_requests` (`PUT` on a re-run, which is a full replace of
what it sends):

```sh
curl -sSf -X POST http://localhost:8081/api/v1/connectors \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "everything-requests", "slug": "everything-requests",
       "endpoint": "http://host.docker.internal:23014/mcp", "timeout_ms": 30000,
       "metadata": {"server_requests": {"elicitation": true, "roots": true}}}'
```

The console shows the same switches under **MCPs ->
everything-requests -> Server-initiated requests**.

**3. Initialize, declaring the capabilities.** The agent says what it can
be asked for; the gateway records it on the session and, at its handshake
with each connector, declares exactly what the agent declared **and** the
connector allows -- here, to `everything-requests`,
`{"elicitation":{},"roots":{"listChanged":true}}`:

```sh
curl -sS -D - http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",
       "capabilities":{"elicitation":{},"roots":{"listChanged":true}},
       "clientInfo":{"name":"11-server-requests","version":"0.1.0"}}}'
# -> Mcp-Session-Id: <SESSION_ID>
```

followed by `notifications/initialized`, as in 01-quickstart.

**4. Hold the session's stream open.** Relayed requests arrive on
`GET /mcp/stream` with the session header; `run.sh` runs this curl in the
background and records its PID:

```sh
curl -sSN http://localhost:8080/mcp/stream \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Mcp-Session-Id: $SESSION_ID"
```

**5. Call the tool that asks.** `tools/call` of
`everything-requests__trigger-elicitation-request` does not return yet: the
server asks the gateway -- inside the call's streamed reply -- for
`elicitation/create`, and the gateway puts it on the stream:

```
data: {"jsonrpc":"2.0","id":"gw-60b1e2478466025c","method":"elicitation/create",
       "params":{"message":"Please provide inputs for the following fields:","requestedSchema":{...}}}
```

The `params` are the server's, untouched (never decoded or re-encoded); the id is the gateway's,
not the server's.

**6. Answer it.** A JSON-RPC response under that id, on the same session:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Mcp-Session-Id: $SESSION_ID" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":"gw-60b1e2478466025c",
       "result":{"action":"accept","content":{"name":"Ada Lovelace","check":true}}}'
# -> 202
```

The gateway hands the result to the server under the server's own id,
and the `tools/call` from step 5 returns with it:

```
✅ User provided the requested information! User inputs: - Name: Ada Lovelace - Agreed to terms: true ...
```

The time the call spent waiting for this answer does not count against
the connector's `timeout_ms`; the wait is bounded by
`mcp.server_request_timeout` (5m) instead.

**7. The same for roots.** `everything-requests__get-roots-list` asks
`roots/list` (this server sends it on its long-lived stream to the
gateway rather than inside the call's reply -- the agent cannot tell the
difference); `run.sh` answers
`{"roots":[{"uri":"file:///work/11-server-requests","name":"demo"}]}` and
checks the tool lists that root.

**8. Policy off.** `run.sh` `PUT`s the connector with
`{"elicitation": false, "roots": true}` and calls
`trigger-elicitation-request` again. The policy is re-read on every
request, so the session needs no re-initialize. The gateway answers the
server `-32601`, which the tool reports:

```
MCP error -32601: elicitation/create is not permitted for this connector
```

and nothing new appears on the agent's stream. `run.sh` then turns the
policy back on.

## Expected output

- Steps 1-4 from 01-quickstart succeed identically (health on all three
  planes); `everything-requests` is healthy with
  `metadata.server_requests = {"elicitation":true,"roots":true}`.
- An `elicitation/create` with a `gw-` id reaches the stream; the answer
  POST gets `202`; the tool result contains `Ada Lovelace`.
- A `roots/list` reaches the stream; the tool result contains
  `file:///work/11-server-requests`.
- With elicitation off, the tool result is
  `MCP error -32601: elicitation/create is not permitted for this connector`
  and the stream stays quiet.
- Access Logs in the console (needs ClickHouse, the Compose `analytics`
  profile) show one `elicitation/create` and one
  `roots/list` record for `everything-requests`, plus the refused
  `elicitation/create` with error `-32601` -- by method, connector and
  duration, never the question or the answer.
- `run.sh` ends with:

  ```
  ================================================================
  11-server-requests passed.
  Console:    http://localhost:8081  (Access Logs show one elicitation/create and
              one roots/list record for connector everything-requests --
              the connector itself is deleted on exit; see cleanup())
  ================================================================
  ```

## Cleanup

`run.sh` cleans up after itself on every exit (success or failure): it
stops the stream and any waiting `tools/call` by the PIDs it recorded,
removes its scratch files, and deletes the `everything-requests`
connector it registered -- unlike `everything` or `header-echo`, this
connector is this example's own and has no reason to outlive the run (a
second connector left on the same "everything" server would inflate the
tenant's tool count for every other example that runs afterwards). To
stop the rest:

```sh
# Stop the local "everything" MCP server, if this run started it
kill "$(cat examples/11-server-requests/.everything.pid)" 2>/dev/null || true
rm -f examples/11-server-requests/.everything.pid examples/11-server-requests/.everything.log

# Stop the gateway stack
docker compose -f deploy/docker-compose.yml down
```

If `run.sh` was killed before it could clean up (e.g. `kill -9`, or the
machine went down mid-run), its connector is left behind (the next run
finds and reuses it by slug). To remove it by hand:

```sh
curl -sS -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "http://localhost:8081/api/v1/connectors?limit=500" \
  | jq -r '.items[] | select(.slug == "everything-requests") | .id' \
  | while read -r id; do
      curl -sS -X DELETE "http://localhost:8081/api/v1/connectors/$id" \
        -H "Authorization: Bearer $GATEWAY_ADMIN_KEY"
    done
```
