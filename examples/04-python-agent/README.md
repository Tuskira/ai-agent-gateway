# 04 - Python agent

## Goal

A small (~250-line) Python agent that talks to the gateway on **both** planes at
once, for one task: "what is 20250926 + 4102? Use the tool."

- The **LLM plane** (`:8082`): the official `anthropic` SDK, pointed at the
  gateway instead of `api.anthropic.com`. This is BYOK (bring your own key)
  -- your own `ANTHROPIC_API_KEY` still goes all the way to Anthropic, the
  gateway just sits in the middle to log and route it.
- The **MCP plane** (`:8080`): the official `mcp` Python client, pointed at
  the gateway instead of a real tool server. The gateway resolves
  `everything__get-sum` to the same "everything" connector 01-quickstart and
  05-profiles use.
- **One session id** ties the two together: the agent reads the
  `Mcp-Session-Id` the gateway mints on MCP `initialize`, then sends that
  same value as `X-Session-Id` on every Anthropic call. The console's
  Access Logs (MCP) and LLM Logs (Anthropic) pages both key off it, so one
  id finds the whole run on both pages.

`--mcp-only` mode exercises just the MCP half (no Anthropic key needed) --
this is what CI runs, and what `run.sh` runs by default.

**Switching model at runtime.** `agent.py --chat` keeps one conversation
open and, like Claude Code's `/model`, lets you switch model between
questions: ask on Claude, type `/model kimi-k3` and keep going on the
open-weight **Kimi K3**, then `/model glm-5.3` for **GLM 5.3** -- all with
the same history.
Those names are registered in the gateway's model registry as
OpenAI-compatible targets on Nebius (or Together AI) with the vendor key
stored as a credential, so the gateway translates each call and sends that
key -- no `ANTHROPIC_API_KEY`.

## Prerequisites

- Docker and the `docker compose` plugin
- `curl`, `jq`, and Node.js (`npx`) -- to bring up and register the
  reference "everything" MCP server, same as 01-quickstart and 05-profiles
- Python 3.10+ (`mcp`'s own minimum; `run.sh` checks the installed
  package's actual `Requires-Python` rather than a hardcoded number).
  `run.sh` creates a `.venv` in this folder and installs
  `requirements.txt` from PyPI; if your pip is configured for a private
  index that lacks `mcp`/`anthropic`, run it as
  `PIP_INDEX_URL=https://pypi.org/simple ./examples/04-python-agent/run.sh`.
- An `ANTHROPIC_API_KEY` **only** for full mode (a real tool-use
  conversation). `--mcp-only` needs no LLM credential at all.
- A `NEBIUS_API_KEY` (or `TOGETHER_API_KEY`) **only** for the open-weight
  models (`kimi-k3`, `glm-5.3`); without one that step is skipped.

This example is self-contained: like 05-profiles, it does not assume
01-quickstart has been run first, but reuses whatever it already left
running (the "everything" server, the compose stack, the "everything"
connector) instead of duplicating it.

## Steps

Run the whole thing with:

```sh
./examples/04-python-agent/run.sh
```

It's safe to re-run, with the same one exception 05-profiles has: the agent
API key is minted fresh every run (its plaintext is only ever returned
once). What `run.sh` does, in order:

**1. Bring up the "everything" MCP server, the compose stack, an admin key,
and the "everything" connector.** Identical to 01-quickstart's steps 1-6 --
see [01-quickstart/README.md](../01-quickstart/README.md) for the full
detail.

**2. Create (or reuse) the `04-python-agent` profile, granting it exactly
`get-sum` and `echo`** -- the same by-name-lookup-then-`PUT`-tools pattern
05-profiles uses for its two profiles:

```sh
curl -sSf -X PUT http://localhost:8081/api/v1/profiles/$PROFILE_ID/tools \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"tools": [
        {"connector_id": "'$CONNECTOR_ID'", "tool_name": "get-sum"},
        {"connector_id": "'$CONNECTOR_ID'", "tool_name": "echo"}
      ]}'
```

**3. Mint a fresh agent-role API key** named `04-python-agent` (not
idempotent, same reasoning as 05-profiles step 3 -- see that README).

**4. Create the example's own `.venv` and `pip install -r requirements.txt`**
(`anthropic`, `mcp`) if it doesn't already exist, then confirm the venv's
Python satisfies whatever `Requires-Python` the installed `mcp` package
actually declares.

**5. Run `agent.py`** -- full mode if `ANTHROPIC_API_KEY` is set in the
environment, `--mcp-only` otherwise -- and assert its output contains the
correct sum, `20255028`.

**6. Open-weight models** (only with `NEBIUS_API_KEY` or `TOGETHER_API_KEY`;
Nebius is used if both are set): store the key as the credential `04-nebius-key` (or
`04-together-key`), register `kimi-k3` and `glm-5.3`, then run one
`agent.py --chat` conversation that asks the question on Claude (only when
`ANTHROPIC_API_KEY` is set), switches with `/model kimi-k3` and asks again,
then `/model glm-5.3` and asks again, and assert every answer carries the
sum.
Re-runs rotate the credential and replace the two models.

### What `agent.py` does

**MCP plane: connect, and capture the session id.** The high-level
`mcp.Client` never hands back the transport it connects through, so there's
no `client.session_id` to read -- the `Mcp-Session-Id` header the gateway
mints on `initialize` is only ever visible as a raw HTTP response header.
`streamable_http_client`'s own docstring names the way to reach it: build
your own `httpx2.AsyncClient` (headers, auth, an event hook) and hand it in
as `http_client`:

```python
import httpx2
from mcp import Client
from mcp.client.streamable_http import streamable_http_client

session_id_box = {}

async def capture_session_id(response: httpx2.Response) -> None:
    if sid := response.headers.get("mcp-session-id"):
        session_id_box["id"] = sid

http_client = httpx2.AsyncClient(
    headers={"Authorization": f"Bearer {gateway_key}", "X-Agent-Profile-Name": profile},
    event_hooks={"response": [capture_session_id]},
)
transport = streamable_http_client("http://localhost:8080/mcp", http_client=http_client)

async with Client(transport) as client:
    tools = (await client.list_tools()).tools
    result = await client.call_tool("everything__get-sum", {"a": 20250926, "b": 4102})
```

**LLM plane: the same session id, on every call.** `AsyncAnthropic()` takes a
`base_url` and `default_headers` exactly like any other `httpx`-backed
client:

```python
import anthropic

llm = anthropic.AsyncAnthropic(   # async, so it never blocks the MCP client
    api_key=anthropic_api_key,          # BYOK: your own key, forwarded upstream unchanged
    base_url="http://localhost:8082",   # the gateway's LLM plane, not api.anthropic.com
    default_headers={
        "X-Gateway-Key": gateway_key,   # the gateway's OWN credential -- see below
        "X-Session-Id": session_id,     # ties this call to the MCP session above
    },
)
```

Why `X-Gateway-Key` and not `Authorization: Bearer gk_...`? The Anthropic
SDK already puts your real Anthropic key on `x-api-key` (or `Authorization`
for some auth flows) -- that header has to reach Anthropic untouched
(`internal/llmplane/provider.go`'s `copyHeaders`/`skipForward`: an
`Authorization: Bearer gk_...` is recognized as the gateway's own
credential and stripped before forwarding, but any other `Authorization` or
an `x-api-key` rides along). `X-Gateway-Key` is a second, dedicated header
the gateway also accepts for its own key
(`internal/auth/apikey/key.go`), so BYOK and the gateway's own auth never
collide on the same header.

**The loop.** Every tool the profile grants is exposed to the model as an
Anthropic `tools` entry (`name`, `description`, `input_schema` straight off
`list_tools()`); the question ("What is 20250926 + 4102? Use the tool.") is
sent once, and each `stop_reason == "tool_use"` turn is answered by calling
that same tool over the MCP plane and feeding the result back as a
`tool_result`, up to 5 turns, until the model returns its final text
answer.

### Switching to open-weight models

Registering a model is what the console's **Models -> Add model** form does
with **Vendor** `Nebius` (or `Together AI`), **Model id**
`moonshotai/Kimi-K3`, the pre-filled **Base URL**, and **Credential**
`Accept API key`. Through the API, as `run.sh` does it:

```sh
# 1. the vendor key, stored once, encrypted (never returned by the API)
curl -sSf -X POST http://localhost:8081/api/v1/credentials \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "04-nebius-key", "type": "api_key", "payload": {"api_key": "<your Nebius key>"}}'

# 2. the model name agents send, routed to Kimi K3 on Nebius with that key
curl -sSf -X POST http://localhost:8081/api/v1/models \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "kimi-k3", "targets": [{"vendor": "openai_compat", "label": "nebius",
       "base_url": "https://api.tokenfactory.eu-west2.nebius.com/v1/",
       "model": "moonshotai/Kimi-K3", "credential": "04-nebius-key"}]}'
```

(`glm-5.3` is the same with `"model": "zai-org/GLM-5.3"`; on Together AI the
label is `together` and the base URL `https://api.together.ai/v1`.) Then
each call the agent makes with `"model": "kimi-k3"` goes:

```
agent  --POST /v1/messages {"model":"kimi-k3"}-->  gateway :8082
         registry: kimi-k3 -> moonshotai/Kimi-K3 on Nebius, key 04-nebius-key
         translated to Chat Completions -> {base_url}/chat/completions
agent  <--  the answer (thinking, tool_use, text) in Anthropic's format
agent  --tools/call everything__get-sum-->  gateway :8080 (MCP), unchanged
```

**Switching at runtime.** Start the chat on one model and switch whenever
you like; the conversation (and its session id) carries over:

```sh
MODEL=claude-sonnet-5 examples/04-python-agent/.venv/bin/python3 examples/04-python-agent/agent.py --chat
```

```
Session id: <a Mcp-Session-Id string>
model: claude-sonnet-5
> What is 20250926 + 4102? Use the tool.
[claude-sonnet-5] Answer: <Claude's answer: 20255028>
> /model kimi-k3
model: kimi-k3
> Now add 1000 to that. Use the tool.
[kimi-k3] Answer: <Kimi K3's answer, same conversation: 20256028>
> /model glm-5.3
model: glm-5.3
> And 1000 more. Use the tool.
[glm-5.3] Answer: <GLM 5.3's answer, same conversation: 20257028>
> /exit
```

(Claude uses your `ANTHROPIC_API_KEY`; `kimi-k3` and `glm-5.3` use the key
stored in the gateway. Without an Anthropic key, start with
`MODEL=kimi-k3`.)

`/model` alone shows the current model, `/clear` starts a fresh
conversation, `/exit` (or Ctrl-D) quits. Nothing is pinned per
session in the gateway: every call names its model and is resolved on its
own, so the switch is just the next call's `model`. Switching to a Claude
model after Kimi/GLM turns needs `/clear` first -- Anthropic refuses
another vendor's reasoning and tool ids in the history.

`agent.py` sends those models' thinking back on each tool-use turn, as Kimi
and GLM expect. Claude Code can use the same names -- see
[docs/llm-plane.md, Translated targets](../../docs/llm-plane.md#translated-targets).
These models have no rate-card price: LLM Logs shows their cost as empty
unless you set the model's **Pricing override**.

## Expected output

`--mcp-only` (no `ANTHROPIC_API_KEY`, what CI runs):

```
MCP session id: <a Mcp-Session-Id string>
Tools (2): everything__echo, everything__get-sum
everything__get-sum(20250926, 4102) -> The sum of 20250926 and 4102 is 20255028.
OK
```

Full mode (`ANTHROPIC_API_KEY` set):

```
Answer: <the model's final text, containing 20255028>
Session id: <the same Mcp-Session-Id string>
Check Access Logs and LLM Logs in the console for this session id.
```

Open-weight models (`NEBIUS_API_KEY` or `TOGETHER_API_KEY` set): one
`agent.py --chat` conversation that switches model partway through -- the
Claude lines only with `ANTHROPIC_API_KEY`:

```
Session id: <a Mcp-Session-Id string>
model: claude-sonnet-5
[claude-sonnet-5] Answer: <Claude's text, containing 20255028>
model: kimi-k3
[kimi-k3] Answer: <Kimi K3's text, containing 20255028>
model: glm-5.3
[glm-5.3] Answer: <GLM 5.3's text, containing 20255028>
```

Without either key, `run.sh` prints `skipped: open-weight models (...)`.

`run.sh` finishes by printing the console URL, the agent key, and what to
export to run full mode yourself:

```
================================================================
04-python-agent passed (mcp-only mode).
Model switch: skipped (set NEBIUS_API_KEY or TOGETHER_API_KEY)
Console:      http://localhost:8081
Agent key:    gk_...

To run the full mode yourself (real Anthropic calls, your own key):
  export GATEWAY_URL=http://localhost
  export GATEWAY_KEY=gk_...
  export PROFILE=04-python-agent
  export ANTHROPIC_API_KEY=sk-ant-...
  examples/04-python-agent/.venv/bin/python3 examples/04-python-agent/agent.py
================================================================
```

**Seeing it in the console.** The Access Logs page reads from ClickHouse,
which is off by default locally (LLM Logs works without it, from the
Postgres capture table). Bring the stack up with the `analytics` profile to
see both:

```sh
GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose -f deploy/docker-compose.yml --profile analytics up -d
```

This is optional -- both modes of `agent.py` work, and `run.sh` passes,
without it.

## Cleanup

```sh
# Stop the local "everything" MCP server, if this run started it
kill "$(cat examples/04-python-agent/.everything.pid)" 2>/dev/null || true
rm -f examples/04-python-agent/.everything.pid examples/04-python-agent/.everything.log

# Remove the example's venv
rm -rf examples/04-python-agent/.venv

# Remove the open-weight models and their stored key, if step 6 ran
for name in kimi-k3 glm-5.3; do
  id="$(curl -sSf -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" "http://localhost:8081/api/v1/models?limit=500" |
    jq -r --arg n "$name" '.items[] | select(.name==$n and .scope=="tenant") | .id')"
  [ -n "$id" ] && curl -sSf -X DELETE -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" "http://localhost:8081/api/v1/models/$id"
done
for cred in 04-nebius-key 04-together-key; do
  curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" "http://localhost:8081/api/v1/credentials/$cred"
done

# Stop the gateway stack
docker compose -f deploy/docker-compose.yml down
```

Every re-run of this example mints one more `04-python-agent` API key
without revoking the previous one, same as 05-profiles -- see that
example's README for the cleanup query if you want to tidy them up (just
swap the `name` it filters on for `"04-python-agent"`).
