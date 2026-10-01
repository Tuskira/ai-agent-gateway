#!/usr/bin/env bash
#
# Bedrock Flow C: the gateway signs with its OWN AWS identity (the default
# credential chain, loaded once -- internal/llmplane/bedrock.go's
# buildSigned(client=false)/gatewayProvider). No caller credentials at all;
# useful for an internal service that shouldn't handle AWS keys itself. This
# only succeeds if the gateway's own environment has AWS credentials (see
# deploy/docker-compose.yml's AWS_ACCESS_KEY_ID/SECRET/SESSION_TOKEN
# passthrough) -- this example's run.sh never asserts on it for that reason
# (it can't tell what the already-running gateway container was started
# with); it's here to run by hand once your gateway has an identity.
set -euo pipefail
: "${GATEWAY_URL:=http://localhost:8082}"
: "${GATEWAY_KEY:?set GATEWAY_KEY to an agent or admin API key (POST /api/v1/api-keys)}"
MODEL="${MODEL:-amazon.nova-lite-v1:0}"
BEDROCK_REGION="${BEDROCK_REGION:-}"

extra_headers=()
if [[ -n "$BEDROCK_REGION" ]]; then
  extra_headers+=(-H "X-Bedrock-Region: $BEDROCK_REGION")
fi

curl -sS -X POST "$GATEWAY_URL/bedrock/model/$MODEL/converse" \
  -H "X-Gateway-Key: $GATEWAY_KEY" \
  ${extra_headers[@]+"${extra_headers[@]}"} \
  -H 'content-type: application/json' \
  -H "X-Session-Id: 07-bedrock-C-$(date +%s)" \
  -d '{"messages":[{"role":"user","content":[{"text":"What is 2+2? Number only."}]}],
       "inferenceConfig":{"maxTokens":40}}'
echo
