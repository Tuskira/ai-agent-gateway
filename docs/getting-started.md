# Getting started

This page continues from the [README Quickstart](https://github.com/Tuskira/tusk-ai-secured-gateway#quickstart):
with the gateway running and your admin key saved, it registers one MCP
tool server as a connector (the console's **MCPs** page) and calls one of
its tools. It needs no cloud account and no LLM provider credentials.

## Prerequisites

- Docker and the `docker compose` plugin
- `curl` and `jq`
- Node.js (`npx` ships with npm) — only to run the reference MCP server
  this walkthrough registers as a connector

See the [README's Prerequisites](https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/README.md#prerequisites)
section for the full list, including what building from source or the
other examples need.

## Run it

```sh
export GATEWAY_ADMIN_KEY=gk_...   # the key bootstrap-key printed
./examples/01-quickstart/run.sh
```

The script is safe to re-run. In order, it:

1. Starts the reference MCP server
   (`@modelcontextprotocol/server-everything`) on port 23014.
2. Starts the gateway and its Postgres with Docker Compose.
3. Checks health on all three planes — `:8080` MCP, `:8081` control
   (REST + console), `:8082` LLM.
4. Reuses your admin key from `GATEWAY_ADMIN_KEY` (if it isn't set, it
   bootstraps a new one).
5. Registers the MCP server as a connector named `everything`, probes its
   health, and discovers its tools.
6. Calls `everything__echo` through the gateway's MCP plane, then fetches
   one of the server's prompts and reads one of its resources the same
   way.

It finishes by printing the console URL and the admin key:

```
================================================================
01-quickstart passed.
Console:    http://localhost:8081
Admin key:  gk_...
================================================================
```

Keep that key: a key's plaintext is shown once and can't be recovered
(without `GATEWAY_ADMIN_KEY` set, each run mints a new one).

To run the same steps by hand, one `curl` at a time, follow
[examples/01-quickstart](https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/examples/01-quickstart/README.md) — it lists
every command the script runs and the output to expect.

## Open the console

Go to <http://localhost:8081> and sign in as the user you created in the
[README Quickstart](https://github.com/Tuskira/tusk-ai-secured-gateway#quickstart)
(the admin key also works, as an emergency fallback — see
[authentication.md](authentication.md#the-first-user)). The **MCPs** page
shows the `everything` connector and its tools.

The Overview dashboard, Access Logs and Session Timeline need ClickHouse
for their data (LLM Logs falls back to Postgres). To get them, bring the stack up with the `analytics` profile:

```sh
GATEWAY_SINKS_CLICKHOUSE_ENABLED=true \
  docker compose -f deploy/docker-compose.yml --profile analytics up --build -d
```

See [observability.md](observability.md#console-pages) for which page
reads what.

## Clean up

```sh
# Stop the reference MCP server the script started
kill "$(cat examples/01-quickstart/.everything.pid)" 2>/dev/null || true
rm -f examples/01-quickstart/.everything.pid examples/01-quickstart/.everything.log

# Stop the gateway stack (add -v to also drop its Postgres volume)
docker compose -f deploy/docker-compose.yml down
```

## Next steps

- **Connect an agent.** [examples/02-claude-code](https://github.com/Tuskira/tusk-ai-secured-gateway/tree/main/examples/02-claude-code)
  and [examples/03-cursor-and-vscode](https://github.com/Tuskira/tusk-ai-secured-gateway/tree/main/examples/03-cursor-and-vscode)
  point Claude Code, Cursor, and VS Code at the gateway.
- **Scope tools per agent.** [profiles.md](profiles.md) and
  [examples/05-profiles](https://github.com/Tuskira/tusk-ai-secured-gateway/tree/main/examples/05-profiles).
- **Store a backend credential once.**
  [connectors-and-credentials.md](connectors-and-credentials.md) and
  [examples/06-credentials-and-headers](https://github.com/Tuskira/tusk-ai-secured-gateway/tree/main/examples/06-credentials-and-headers).
- **Proxy LLM calls.** [llm-plane.md](llm-plane.md) and
  [examples/07-llm-passthrough](https://github.com/Tuskira/tusk-ai-secured-gateway/tree/main/examples/07-llm-passthrough).
- **Run it on Kubernetes.** [deploy/README.md](https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/deploy/README.md) and
  [examples/10-kubernetes](https://github.com/Tuskira/tusk-ai-secured-gateway/tree/main/examples/10-kubernetes).
- **Everything else.** The [documentation index](README.md) and the
  [examples index](https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/examples/README.md).
