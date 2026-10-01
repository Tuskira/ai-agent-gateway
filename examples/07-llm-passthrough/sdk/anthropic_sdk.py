#!/usr/bin/env python3
"""Anthropic SDK through the gateway's LLM plane -- BYOK. base_url is bare;
the SDK appends /v1/messages itself. See ../README.md's route table.

    pip install anthropic
    GATEWAY_KEY=gk_... ANTHROPIC_API_KEY=sk-ant-... python3 sdk/anthropic_sdk.py
"""
import os

import anthropic

client = anthropic.Anthropic(
    api_key=os.environ["ANTHROPIC_API_KEY"],  # BYOK: forwarded upstream unchanged
    base_url=os.environ.get("GATEWAY_URL", "http://localhost:8082"),
    default_headers={"X-Gateway-Key": os.environ["GATEWAY_KEY"]},  # gateway's OWN credential
)

resp = client.messages.create(
    model=os.environ.get("MODEL", "claude-sonnet-5"),
    max_tokens=40,
    messages=[{"role": "user", "content": "Say hi in one sentence."}],
)
print(resp.content[0].text)
print(f"usage: input={resp.usage.input_tokens} output={resp.usage.output_tokens}")
