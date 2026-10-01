#!/usr/bin/env python3
"""OpenAI SDK through the gateway's LLM plane -- BYOK. base_url MUST include
/openai/v1 (the SDK appends only /chat/completions). See ../README.md's
route table for why the gateway's own key needs a header of its own here.

    pip install openai
    GATEWAY_KEY=gk_... OPENAI_API_KEY=sk-... python3 sdk/openai_sdk.py
"""
import os

import openai

gateway_url = os.environ.get("GATEWAY_URL", "http://localhost:8082")

client = openai.OpenAI(
    api_key=os.environ["OPENAI_API_KEY"],  # BYOK: forwarded upstream unchanged
    base_url=f"{gateway_url}/openai/v1",
    default_headers={"X-Gateway-Key": os.environ["GATEWAY_KEY"]},  # gateway's OWN credential
)

resp = client.chat.completions.create(
    model=os.environ.get("MODEL", "gpt-4o-mini"),
    max_tokens=40,
    messages=[{"role": "user", "content": "Say hi in one sentence."}],
)
print(resp.choices[0].message.content)
print(f"usage: prompt={resp.usage.prompt_tokens} completion={resp.usage.completion_tokens}")
