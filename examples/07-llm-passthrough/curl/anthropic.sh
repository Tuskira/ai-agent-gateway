#!/usr/bin/env bash
#
# Anthropic through the gateway's LLM plane (:8082). BYOK: your own
# ANTHROPIC_API_KEY rides through untouched on x-api-key; the gateway's own
# key travels separately on X-Gateway-Key. `/anthropic/*` is the explicit
# prefix route -- Anthropic also answers on the bare `/v1/messages` (what
# Claude Code's ANTHROPIC_BASE_URL sends), see
# internal/llmplane/provider.go's pickProvider/isAnthropicNativePath.
set -euo pipefail
: "${GATEWAY_URL:=http://localhost:8082}"
: "${GATEWAY_KEY:?set GATEWAY_KEY to an agent or admin API key (POST /api/v1/api-keys)}"
: "${ANTHROPIC_API_KEY:?set ANTHROPIC_API_KEY to your own Anthropic key (BYOK)}"
MODEL="${MODEL:-claude-sonnet-5}"

curl -sS -X POST "$GATEWAY_URL/anthropic/v1/messages" \
  -H "X-Gateway-Key: $GATEWAY_KEY" \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H 'anthropic-version: 2023-06-01' \
  -H 'content-type: application/json' \
  -H "X-Session-Id: 07-anthropic-$(date +%s)" \
  -d "{\"model\":\"$MODEL\",\"max_tokens\":40,
       \"messages\":[{\"role\":\"user\",\"content\":\"Say hi in one sentence.\"}]}"
echo
