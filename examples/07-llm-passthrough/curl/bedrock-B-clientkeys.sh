#!/usr/bin/env bash
#
# Bedrock Flow B: the caller supplies AWS credentials as X-Bedrock-* headers;
# the gateway signs the request with them (SigV4) and strips the credential
# headers before forwarding -- they never reach Bedrock, and Bedrock never
# sees the gateway's own key either (internal/llmplane/bedrock.go's
# buildSigned/credsFromHeaders; provider.go's skipForward). This is the BYOK
# mode for Bedrock: your own temporary creds, forwarded as a signature only.
# X-Bedrock-Region overrides llm_proxy.bedrock.region for this request
# (bedrock.go's regionFor). MODEL defaults to an inference-profile id (the
# form most Claude-on-Bedrock models require); pricing looks it up by
# stripping the leading region prefix (pkg/pricing/pricing.go's reRegion).
set -euo pipefail
: "${GATEWAY_URL:=http://localhost:8082}"
: "${GATEWAY_KEY:?set GATEWAY_KEY to an agent or admin API key (POST /api/v1/api-keys)}"
: "${AWS_ACCESS_KEY_ID:?set AWS_ACCESS_KEY_ID (your own, temporary AWS credentials)}"
: "${AWS_SECRET_ACCESS_KEY:?set AWS_SECRET_ACCESS_KEY}"
MODEL="${MODEL:-us.anthropic.claude-haiku-4-5-20251001-v1:0}"
BEDROCK_REGION="${BEDROCK_REGION:-${AWS_REGION:-us-east-1}}"

extra_headers=()
if [[ -n "${AWS_SESSION_TOKEN:-}" ]]; then
  extra_headers+=(-H "X-Bedrock-Session-Token: $AWS_SESSION_TOKEN")
fi

curl -sS -X POST "$GATEWAY_URL/bedrock/model/$MODEL/converse" \
  -H "X-Gateway-Key: $GATEWAY_KEY" \
  -H "X-Bedrock-Region: $BEDROCK_REGION" \
  -H "X-Bedrock-Access-Key-Id: $AWS_ACCESS_KEY_ID" \
  -H "X-Bedrock-Secret-Access-Key: $AWS_SECRET_ACCESS_KEY" \
  ${extra_headers[@]+"${extra_headers[@]}"} \
  -H 'content-type: application/json' \
  -H "X-Session-Id: 07-bedrock-B-$(date +%s)" \
  -d '{"messages":[{"role":"user","content":[{"text":"Say hi."}]}],
       "inferenceConfig":{"maxTokens":40}}'
echo
