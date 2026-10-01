#!/usr/bin/env bash
#
# OpenAI through the gateway's LLM plane (:8082). BYOK: your own
# OPENAI_API_KEY rides through untouched on Authorization: Bearer; the
# gateway's own key travels on the separate X-Gateway-Key header, since
# Authorization is already spoken for by the OpenAI credential
# (internal/llmplane/provider.go's copyHeaders/isGatewayBearer only strips an
# Authorization that itself carries the gateway's "Bearer gk_..."). OpenAI is
# prefix-only -- there is no bare `/v1/chat/completions` route, since that
# path is shared namespace with other providers (internal/llmplane/
# provider.go's pickProvider).
set -euo pipefail
: "${GATEWAY_URL:=http://localhost:8082}"
: "${GATEWAY_KEY:?set GATEWAY_KEY to an agent or admin API key (POST /api/v1/api-keys)}"
: "${OPENAI_API_KEY:?set OPENAI_API_KEY to your own OpenAI key (BYOK)}"
MODEL="${MODEL:-gpt-4o-mini}"

curl -sS -X POST "$GATEWAY_URL/openai/v1/chat/completions" \
  -H "X-Gateway-Key: $GATEWAY_KEY" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'content-type: application/json' \
  -H "X-Session-Id: 07-openai-$(date +%s)" \
  -d "{\"model\":\"$MODEL\",\"max_tokens\":40,
       \"messages\":[{\"role\":\"user\",\"content\":\"Say hi in one sentence.\"}]}"
echo
