# 13 - Claude Desktop

## Goal

See what people do in **Claude Desktop** (macOS) in the gateway's console,
without changing how Claude Desktop talks to Anthropic.

Claude Desktop does not use the gateway as its proxy. Instead, a small
companion tool, [claude-desktop-utility](https://github.com/Tuskira/claude-desktop-utility)
("the utility"), runs on the Mac as a local proxy. You route Claude Desktop's
traffic through it. It passes everything on unchanged and sends a copy of the
Code tab and Chat tab activity to the gateway's
[`POST /api/v1/ingest`](../../docs/api.md#ingest) endpoint. The gateway stores
those records in the same tables as gateway-proxied calls, tagged
`source: interceptor`, so they show up on the console's existing pages:

| What happens in Claude Desktop | What the gateway stores | Where you see it |
|---|---|---|
| A model reply in the **Code tab** or **Chat tab** | One LLM call record (model, tokens in the Code tab, messages) | **LLM Logs** |
| A **connector (MCP) tool call** in the Code tab | One access-log record (connector, tool, status, size) | **Access Logs** |
| Both of the above in one conversation | Joined by session id | **Session Timeline** |

Everything else Claude Desktop does (presence, heartbeats, telemetry), and
built-in tools such as Bash or Edit, is only proxied, never forwarded. Chat tab
token counts are always 0 (the Chat tab does not expose them), and the utility
never sends a cost; ingested rows have no cost.

Today this is **macOS only**. The gateway side of the example (steps 1 and 2,
and `run.sh`) runs on any machine that runs Docker.

## Prerequisites

- A Mac with Claude Desktop installed, and your Mac admin password (needed once
  to trust the utility's local certificate authority).
- Go 1.27 or newer (the utility's `go.mod` requires `go 1.27`) and `jq` on the Mac, to build the utility.
- Docker and the `docker compose` plugin, and `curl` and `jq`, for the gateway.
  The gateway can run on the same Mac or anywhere the Mac can reach.

## Steps

### 1. Start the gateway with ingest enabled

Ingest is **off by default**, and it needs the ClickHouse sink (the analytics
profile) because that is where it writes. The shipped
`deploy/docker-compose.yml` has no ingest setting, so this example adds one
with a small overlay,
[`docker-compose.ingest.yml`](docker-compose.ingest.yml), which sets
`GATEWAY_INGEST_ENABLED=true`. From the repository root:

```sh
GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose \
  -f deploy/docker-compose.yml \
  -f examples/13-claude-desktop/docker-compose.ingest.yml \
  --profile analytics up --build -d
curl -s http://localhost:8081/api/v1/health
```

The health response should show `"clickhouse":{"enabled":true,...}`. Ingest is
served on the **API plane (`:8081`)**, the same port as the console, not on the
MCP (`:8080`) or LLM (`:8082`) plane.

For a deployed gateway, set `ingest.enabled: true` in the config file (or
`GATEWAY_INGEST_ENABLED=true`) and enable the ClickHouse sink. With ingest
disabled, a caller holding a valid `interceptor` key gets a plain `404`.
Limits, all configurable under `ingest.*`: 1000 records per batch, 32 MiB
decompressed, 8 MiB compressed (fixed), and 120 requests per minute per key.

### 2. Create a key with the `interceptor` role

The built-in `interceptor` role holds exactly one permission, `ingest.write`.
A leaked key can add ingest records but cannot read logs, connectors or
credentials (see [security-model.md](../../docs/security-model.md)). Create the
key with an admin key. For a fresh local stack, mint the first admin key with:

```sh
GATEWAY_ADMIN_KEY="$(docker compose -f deploy/docker-compose.yml \
  -f examples/13-claude-desktop/docker-compose.ingest.yml \
  --profile analytics exec -T gateway /gateway bootstrap-key)"
```

Then create the interceptor key and save **only the key itself** to a
mode-0600 file (the utility refuses to start otherwise):

```sh
mkdir -p ~/.interceptor
curl -sf -X POST http://localhost:8081/api/v1/api-keys \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"claude-desktop interceptor","role":"interceptor"}' \
  | jq -r .key | tr -d '\n' > ~/.interceptor/gateway.key
chmod 600 ~/.interceptor/gateway.key
```

Note the key's `id` (from the same response, or `GET /api/v1/api-keys`) so you
can revoke it in the cleanup step. Create one key per person or machine: the
rate limit is per key.

### 3. Install the utility

Clone and build the utility first:

```sh
git clone https://github.com/Tuskira/claude-desktop-utility.git
cd claude-desktop-utility
make build
```

Run the remaining parts of the guide (the CA trust, `make install-agent
GATEWAY_URL=http://localhost:8081`, and the `egressProxyUrl` setting) from that
`claude-desktop-utility` directory. Then follow the utility's own guide, which
is the supported install and is kept current there:
**[deploy/macos/README.md](https://github.com/Tuskira/claude-desktop-utility/blob/main/deploy/macos/README.md)**.
In short: Part 1 builds it (done above), Part 2 creates a local certificate authority and
trusts it system-wide, Part 3 is the gateway key (you already did it above, so
skip to saving it at `~/.interceptor/gateway.key`), and Part 4 installs a
launchd service that starts at login. Do not duplicate those commands from
here.

The one thing this example decides for you is the gateway flag in Part 4. For
the **local** gateway from step 1, pass the API-plane URL:

```sh
make install-agent GATEWAY_URL=http://localhost:8081
```

For any gateway that is not on this Mac, use an `https://` URL
(`GATEWAY_URL=https://gateway.example.com`). The gateway key travels in a
header on every batch, so `http://` is only acceptable for `localhost`.

Check the service is up:

```sh
tail -5 ~/Library/Logs/interceptor.log
```

You should see `proxy listening on 127.0.0.1:9090` and
`forwarding Claude Desktop traffic to http://localhost:8081`.

### 4. Point Claude Desktop at the utility

The utility's Part 5 sets Claude Desktop's own network proxy setting. Claude
Desktop reads it from the applied configuration (**Developer → Configure
Third-Party Inference** opens it; the file is under
`~/Library/Application Support/Claude-3p/configLibrary/`). Set:

```json
"egressProxyUrl": "http://127.0.0.1:9090"
```

Part 5 of the guide has a `jq` one-liner that edits the right file and keeps your
other settings. Then quit Claude Desktop with **Cmd+Q** and reopen it from the
Dock; it reads the setting only at startup. While `egressProxyUrl` is set,
Claude Desktop cannot connect unless the utility is running.

### 5. Use it, then look at the gateway

1. In Claude Desktop, ask something in the **Chat tab**, then use the **Code
   tab** and, if you have a connector (MCP) configured there, let it call one
   tool.
2. Wait up to a minute. The utility batches records (up to 500, or every 2
   seconds) and prints a status line every 60 seconds:
   ```sh
   grep 'forward status' ~/Library/Logs/interceptor.log | tail -3
   ```
   `sent=` should rise; `rejected=` and `dropped=` should stay 0.
3. In the console (`http://localhost:8081`):
   - **LLM Logs**: set **Source** to *Interceptor*. Each row has an
     *Interceptor* badge, and the **User** column shows your Claude account
     email. The email is learned from traffic at app start, so if it is empty,
     restart Claude Desktop once.
   - **Access Logs**: set **Source** to *Interceptor* to see connector tool
     calls from the Code tab.
   - **Session Timeline**: paste a `session_id` from either page to see the
     model replies and tool calls of one conversation in order.
4. The same data over the API (the `source` and `user` filters exist on both
   routes):
   ```sh
   curl -s -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
     'http://localhost:8081/api/v1/analytics/llm-logs?source=interceptor&limit=5' | jq '.items[] | {ts: .timestamp, model, source, user}'
   curl -s -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
     'http://localhost:8081/api/v1/analytics/logs?source=interceptor&limit=5' | jq '.items[] | {tool: .tool_name, source, user}'
   ```

### 6. Check the gateway side without a Mac

[`run.sh`](run.sh) tests everything on the gateway end of this example with no
Mac, no Claude Desktop and no utility. It does what the utility does: it sends
a gzipped batch with one LLM call and one connector tool call to
`POST /api/v1/ingest` and then checks the result.

```sh
./examples/13-claude-desktop/run.sh
```

It brings up the stack with ingest enabled (reusing it if it is running), mints
an `interceptor` key and an `agent` key, and asserts:

- no key gets `401`, an `agent` key gets `403`, the `interceptor` key gets `200`;
- the first POST is stored and an identical re-POST is reported as duplicates
  (dedup by `request_id`), so retries never double-count;
- a wrong `schema_version` gets `400`;
- `/analytics/llm-logs` and `/analytics/logs` return the rows with
  `source=interceptor` and the user label, with token counts and no cost;
- the interceptor key cannot read the logs back (`403`);
- `/analytics/sessions/{id}/timeline` contains both rows.

It uses fixed request ids, so on a re-run the first POST reports duplicates of
the earlier run's rows instead of accepted ones; the assertions allow both.
When it exits it revokes the keys it minted and restores the gateway to the
configuration it found. [`check.sh`](check.sh) is the static twin: file layout,
README links and `shellcheck`, with no Docker needed.

## Troubleshooting

Start with the utility's own
[Troubleshooting](https://github.com/Tuskira/claude-desktop-utility#troubleshooting)
section (service not starting, Claude not routing through the proxy,
certificate errors). Gateway-side checks, in the order to try them:

- **`404 not found` from `/api/v1/ingest`.** Ingest is disabled. Set
  `GATEWAY_INGEST_ENABLED=true` (step 1) and recreate the gateway.
- **`401` in the utility's log.** The key is wrong, revoked, or has trailing
  whitespace or a newline in `~/.interceptor/gateway.key` (write it with `tr -d '\n'`
  or `echo -n`).
- **`403`.** The key's role is not `interceptor` (or admin). Check
  `GET /api/v1/api-keys`.
- **`413`.** A batch is over the compressed (8 MiB) or decompressed
  (`ingest.max_body_bytes`) limit. Lower the utility's `--max-body`.
- **`429`.** More than `ingest.rate_per_minute` requests from one key. The
  utility backs off and keeps the batch spooled on disk; give each machine its
  own key.
- **`503`.** ClickHouse is not configured or not reachable. Ingest has no
  Postgres fallback. Check `GATEWAY_SINKS_CLICKHOUSE_ENABLED=true` and the
  `clickhouse` container.
- **`400`.** The batch itself is malformed (for example `schema_version` is not
  `1`). Records the gateway refuses one by one come back in the `rejected` array
  of a `200` response and show up as `rejected=` in the utility's status line.
- **Rows exist but the User column is empty.** The account email is learned from
  traffic at app start; restart Claude Desktop. Or set `--forward-user` in the
  service arguments.
- **A Code tab session does not join its tool calls on the timeline.** The
  timeline only joins events written by the same key; use one key per machine.

## Security notes

- The utility is a TLS-intercepting proxy. It works because you trust its local
  certificate authority system-wide. Anything holding `~/.interceptor/ca-key.pem`
  can read HTTPS traffic from a machine that trusts the CA. Keep it mode 0600
  and remove the trust when you are finished (cleanup below).
- The capture file (`~/claude-capture.jsonl`) holds full chats and session
  tokens in the clear. Treat it like a password file.
- The gateway key's scope is the `interceptor` role only: it can write ingest
  records and nothing else. Even so, use a dedicated key per machine and revoke it
  when done. Use `https://` for any gateway that is not on `localhost`.
- The utility's local proxy (`127.0.0.1:9090`) has no authentication; do not
  expose it beyond localhost.
- The gateway never forwards ingested traffic anywhere. Stored bodies follow the
  normal capture settings (`capture.store_bodies`, `llm_proxy.capture.store_bodies`).
  Do not point this at conversations you are not allowed to log.

## Cleanup

Reverse the steps, in this order (the utility's
[Part 7](https://github.com/Tuskira/claude-desktop-utility/blob/main/deploy/macos/README.md#part-7-undo-everything)
has the exact commands for the first five):

1. Quit Claude Desktop (Cmd+Q).
2. Remove `egressProxyUrl` from the applied configuration.
3. Stop and remove the launchd service (`make uninstall-agent`).
4. Reopen Claude Desktop from the Dock; it now connects directly.
5. Remove the local CA from system trust and delete `~/.interceptor` and the
   capture file.
6. Revoke the gateway key:
   ```sh
   curl -s -X DELETE -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
     http://localhost:8081/api/v1/api-keys/<key id>
   ```
7. Optional, for the local stack: put the gateway back to its default config
   (ingest and ClickHouse off) with
   `docker compose -f deploy/docker-compose.yml up -d gateway`, or remove the
   stack with `docker compose -f deploy/docker-compose.yml --profile analytics down -v`
   (this deletes the stored logs).

Rows already ingested stay in ClickHouse until you delete the volume or let
your retention policy remove them.
