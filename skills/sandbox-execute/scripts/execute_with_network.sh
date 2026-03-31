#!/usr/bin/env bash
# Execute code with network access and optional whitelist
# Usage: execute_with_network.sh <language> '<code>' [whitelist_domains...]
set -euo pipefail

SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"

LANGUAGE="${1:?Usage: execute_with_network.sh <python|nodejs|bash> '<code>' [domain1 domain2 ...]}"
CODE="${2:?Missing code argument}"
shift 2

# Build whitelist array
WHITELIST="[]"
if [ $# -gt 0 ]; then
  WHITELIST=$(printf '%s\n' "$@" | jq -R . | jq -s .)
fi

curl -s -X POST "${SANDBOX_API_URL}/api/v1/execute" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n \
    --arg lang "$LANGUAGE" \
    --arg code "$CODE" \
    --argjson whitelist "$WHITELIST" \
    '{language: $lang, code: $code, network: {enabled: true, whitelist: $whitelist}}')"
