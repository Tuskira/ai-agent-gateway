#!/usr/bin/env python3
"""04-python-agent: a tiny tool-use loop that talks to the gateway on both
planes at once -- the Anthropic SDK through the LLM plane (BYOK: your own
ANTHROPIC_API_KEY, forwarded upstream unchanged) and the official `mcp`
Python client through the MCP plane -- and ties the two together with one
session id, so the console's Access Logs and LLM Logs pages show the same
run.

Env vars:
  GATEWAY_URL        Gateway base URL (default http://localhost). The MCP
                      and LLM planes are derived from its host on :8080 and
                      :8082 respectively.
  GATEWAY_KEY        Agent-role API key (gk_...). Required.
  PROFILE             X-Agent-Profile-Name to send (default 04-python-agent).
  ANTHROPIC_API_KEY  Your own Anthropic key (BYOK) for Claude models.
                      Required when MODEL is a Claude model, unless
                      --mcp-only. Not for a model name registered in the
                      gateway with its own vendor key (kimi-k3, glm-5.3):
                      the gateway sends that key.
  MODEL              Model to start with (default claude-sonnet-5).

--mcp-only connects to the MCP plane, prints the session id and the
profile's tools, calls everything__get-sum, and exits non-zero on failure
-- no Anthropic key needed. Full mode additionally drives an Anthropic
tool-use loop that answers a question requiring that same tool.

--chat keeps one conversation open and switches model at runtime, like
Claude Code's /model: each line on stdin is a question, except
  /model NAME   answer the next questions with NAME (history kept)
  /model        show the current model
  /clear        start a fresh conversation
  /exit         quit (as does end of input, Ctrl-D)
Switching to a Claude model after another vendor's turns: /clear first --
Anthropic refuses the other vendor's reasoning and tool ids in history.
"""
from __future__ import annotations

import argparse
import asyncio
import os
import sys
from urllib.parse import urlsplit, urlunsplit

import httpx2
from mcp import Client
from mcp.client.streamable_http import streamable_http_client

QUESTION = "What is 20250926 + 4102? Use the tool."
MAX_TURNS = 5
# Thinking models (Kimi K3, GLM 5.3) spend part of this on reasoning.
MAX_TOKENS = 4096


def _plane_url(gateway_url: str, port: int, path: str = "") -> str:
    """Re-host gateway_url's scheme/host onto one plane's port (:8080 MCP, :8082 LLM)."""
    host = urlsplit(gateway_url).hostname or "localhost"
    return urlunsplit(("http", f"{host}:{port}", path, "", ""))


def _connect_mcp(gateway_url: str, gateway_key: str, profile: str):
    """Build an unentered MCP transport plus a box the session id lands in.

    mcp.Client (the high-level wrapper) never hands back the transport it
    connects through, so there is no client.session_id property to read --
    the Mcp-Session-Id the gateway mints on `initialize` is only ever seen
    as a response header. An httpx2 response event hook is the supported
    way to reach it (see streamable_http_client's own docstring: "To
    configure headers, authentication, or other HTTP settings, create an
    httpx2.AsyncClient and pass it here").
    """
    session_id_box: dict[str, str] = {}

    async def capture_session_id(response: httpx2.Response) -> None:
        session_id = response.headers.get("mcp-session-id")
        if session_id:
            session_id_box["id"] = session_id

    http_client = httpx2.AsyncClient(
        headers={
            "Authorization": f"Bearer {gateway_key}",
            "X-Agent-Profile-Name": profile,
        },
        event_hooks={"response": [capture_session_id]},
    )
    transport = streamable_http_client(_plane_url(gateway_url, 8080, "/mcp"), http_client=http_client)
    return transport, session_id_box


def _to_anthropic_tools(tools) -> list[dict]:
    return [{"name": t.name, "description": t.description or "", "input_schema": t.input_schema} for t in tools]


def _tool_text(result) -> str:
    return "".join(block.text for block in result.content if block.type == "text")


async def run_mcp_only(args: argparse.Namespace) -> int:
    transport, session_id_box = _connect_mcp(args.gateway_url, args.gateway_key, args.profile)
    async with Client(transport) as client:
        print(f"MCP session id: {session_id_box.get('id', '')}")

        tools = (await client.list_tools()).tools
        print(f"Tools ({len(tools)}): {', '.join(sorted(t.name for t in tools))}")

        a, b = 20250926, 4102
        result = await client.call_tool("everything__get-sum", {"a": a, "b": b})
        text = _tool_text(result)
        print(f"everything__get-sum({a}, {b}) -> {text}")

        expected = str(a + b)
        if result.is_error or expected not in text:
            print(f"FAIL: expected the sum {expected} in the tool result", file=sys.stderr)
            return 1
        print("OK")
        return 0


async def ask(llm, mcp_client, tools: list[dict], model: str, messages: list[dict]) -> str | None:
    """Runs the tool-use loop for the question last appended to messages (the
    conversation so far) on model, appending every turn to messages. Returns
    the final text, or None after MAX_TURNS without one."""
    for _turn in range(MAX_TURNS):
        response = await llm.messages.create(model=model, max_tokens=MAX_TOKENS, tools=tools, messages=messages)
        messages.append({"role": "assistant", "content": _blocks_to_params(response.content)})

        if response.stop_reason != "tool_use":
            return "".join(b.text for b in response.content if b.type == "text")

        tool_results = []
        for block in response.content:
            if block.type != "tool_use":
                continue
            result = await mcp_client.call_tool(block.name, block.input)
            tool_results.append(
                {
                    "type": "tool_result",
                    "tool_use_id": block.id,
                    "content": _tool_text(result),
                    "is_error": result.is_error,
                }
            )
        messages.append({"role": "user", "content": tool_results})
    return None


async def chat(llm, mcp_client, tools: list[dict], model: str, has_anthropic_key: bool) -> int:
    """--chat: one conversation, the model switched at runtime with /model."""
    import anthropic

    messages: list[dict] = []
    while True:
        if sys.stdin.isatty():
            print("> ", end="", flush=True)
        # Read off the event loop: a blocking read would stall the MCP
        # client's background stream, like a blocking LLM call would.
        line = await asyncio.to_thread(sys.stdin.readline)
        if not line:  # end of input
            return 0
        line = line.strip()
        if not line:
            continue
        if line == "/exit":
            return 0
        if line == "/clear":
            messages.clear()
            print("conversation cleared")
            continue
        if line == "/model" or line.startswith("/model "):
            wanted = line.removeprefix("/model").strip()
            if wanted.startswith("claude") and not has_anthropic_key:
                print(f"{wanted} needs ANTHROPIC_API_KEY (Claude models use your own key); still on {model}")
                continue
            model = wanted or model
            print(f"model: {model}")
            continue
        if line.startswith("/"):
            print("commands: /model NAME, /model, /clear, /exit")
            continue
        asked_at = len(messages)
        messages.append({"role": "user", "content": line})
        try:
            answer = await ask(llm, mcp_client, tools, model, messages)
        except anthropic.APIError as err:
            # Keep the chat going: drop the unanswered question and any
            # turns it got to, then try again or switch model.
            del messages[asked_at:]
            print(f"[{model}] error: {err}")
            continue
        print(f"[{model}] Answer: {answer}" if answer is not None else f"[{model}] no answer after {MAX_TURNS} turns")


async def run_full(args: argparse.Namespace) -> int:
    import anthropic

    transport, session_id_box = _connect_mcp(args.gateway_url, args.gateway_key, args.profile)
    async with Client(transport) as mcp_client:
        anthropic_tools = _to_anthropic_tools((await mcp_client.list_tools()).tools)
        session_id = session_id_box.get("id", "")

        # Same session id on every Anthropic call as the one the MCP plane
        # minted above -- that's what lets Access Logs (MCP) and LLM Logs
        # (Anthropic) be read side by side for this one run.
        # Async client: a blocking call here would stall the MCP client's
        # background stream while the model is thinking.
        llm = anthropic.AsyncAnthropic(
            # A registered model's target carries its own vendor key: the
            # gateway drops whatever key the caller sends and uses that one,
            # so any placeholder does. The SDK only insists on some value.
            api_key=args.anthropic_api_key or "gateway-held-key",
            base_url=_plane_url(args.gateway_url, 8082),
            default_headers={"X-Gateway-Key": args.gateway_key, "X-Session-Id": session_id},
        )

        if args.chat:
            print(f"Session id: {session_id}")
            print(f"model: {args.model}")
            return await chat(llm, mcp_client, anthropic_tools, args.model, bool(args.anthropic_api_key))

        final_text = await ask(llm, mcp_client, anthropic_tools, args.model, [{"role": "user", "content": QUESTION}])
        if final_text is None:
            print("FAIL: exceeded max turns without a final answer", file=sys.stderr)
            return 1

        print(f"Answer: {final_text}")
        print(f"Session id: {session_id}")
        print("Check Access Logs and LLM Logs in the console for this session id.")
        return 0


def _blocks_to_params(blocks) -> list[dict]:
    """Assistant content blocks -> plain dicts, so the next turn's request body
    doesn't depend on the SDK auto-serializing response model objects.
    Thinking is kept: Kimi and GLM expect their earlier reasoning back on
    tool-use turns (Claude answers without thinking here, so none comes)."""
    out = []
    for b in blocks:
        if b.type == "text":
            out.append({"type": "text", "text": b.text})
        elif b.type == "thinking":
            out.append({"type": "thinking", "thinking": b.thinking, "signature": b.signature or ""})
        elif b.type == "tool_use":
            out.append({"type": "tool_use", "id": b.id, "name": b.name, "input": b.input})
    return out


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--mcp-only", action="store_true", help="Only exercise the MCP plane; no Anthropic key needed")
    parser.add_argument("--chat", action="store_true", help="One conversation from stdin; /model NAME switches model")
    parser.add_argument("--gateway-url", default=os.environ.get("GATEWAY_URL", "http://localhost"))
    parser.add_argument("--gateway-key", default=os.environ.get("GATEWAY_KEY", ""))
    parser.add_argument("--profile", default=os.environ.get("PROFILE", "04-python-agent"))
    parser.add_argument("--anthropic-api-key", default=os.environ.get("ANTHROPIC_API_KEY", ""))
    parser.add_argument("--model", default=os.environ.get("MODEL", "claude-sonnet-5"))
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if not args.gateway_key:
        print("FAIL: GATEWAY_KEY is required", file=sys.stderr)
        return 1
    # Claude models go to Anthropic with the caller's own key (BYOK); a name
    # registered in the gateway with a vendor key (kimi-k3, glm-5.3) needs none.
    if not args.mcp_only and args.model.startswith("claude") and not args.anthropic_api_key:
        print("FAIL: ANTHROPIC_API_KEY is required for a Claude model unless --mcp-only", file=sys.stderr)
        return 1
    return asyncio.run(run_mcp_only(args) if args.mcp_only else run_full(args))


if __name__ == "__main__":
    sys.exit(main())
