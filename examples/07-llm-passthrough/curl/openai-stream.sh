#!/usr/bin/env bash
#
# Same route as openai.sh, streamed. `stream_options.include_usage` asks
# OpenAI to put a final usage-only chunk at the end of the SSE stream --
# without it, OpenAI omits usage on a streaming response entirely
# (internal/llmplane/openai.go's ParseUsage reads exactly this chunk). The
# gateway relays the stream frame-by-frame, unmodified (docs/llm-plane.md,
# "Streaming").
set -euo pipefail
: "${GATEWAY_URL:=http://localhost:8082}"
: "${GATEWAY_KEY:?set GATEWAY_KEY to an agent or admin API key (POST /api/v1/api-keys)}"
: "${OPENAI_API_KEY:?set OPENAI_API_KEY to your own OpenAI key (BYOK)}"
MODEL="${MODEL:-gpt-4o-mini}"

curl -sS -N -X POST "$GATEWAY_URL/openai/v1/chat/completions" \
  -H "X-Gateway-Key: $GATEWAY_KEY" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'content-type: application/json' \
  -H "X-Session-Id: 07-openai-stream-$(date +%s)" \
  -d "{\"model\":\"$MODEL\",\"stream\":true,\"stream_options\":{\"include_usage\":true},\"max_tokens\":40,
       \"messages\":[{\"role\":\"user\",\"content\":\"Count to five.\"}]}"
echo
