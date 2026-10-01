#!/usr/bin/env bash
#
# Gemini through the gateway's LLM plane (:8082). BYOK: your own
# GEMINI_API_KEY rides through untouched on x-goog-api-key; the gateway's own
# key travels on X-Gateway-Key. Gemini is prefix-only, like OpenAI --
# `/gemini/*` (internal/llmplane/gemini.go, internal/llmplane/provider.go's
# pickProvider).
set -euo pipefail
: "${GATEWAY_URL:=http://localhost:8082}"
: "${GATEWAY_KEY:?set GATEWAY_KEY to an agent or admin API key (POST /api/v1/api-keys)}"
: "${GEMINI_API_KEY:?set GEMINI_API_KEY to your own Gemini key (BYOK, AI Studio or Vertex)}"
MODEL="${MODEL:-gemini-2.5-flash}"

curl -sS -X POST "$GATEWAY_URL/gemini/v1beta/models/$MODEL:generateContent" \
  -H "X-Gateway-Key: $GATEWAY_KEY" \
  -H "x-goog-api-key: $GEMINI_API_KEY" \
  -H 'content-type: application/json' \
  -H "X-Session-Id: 07-gemini-$(date +%s)" \
  -d '{"contents":[{"parts":[{"text":"Say hi in one sentence."}]}],
       "generationConfig":{"maxOutputTokens":40}}'
echo
