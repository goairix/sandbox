#!/usr/bin/env bash
# Execute code in sandbox
# Usage: execute.sh <language> '<code>' [timeout]
set -euo pipefail

SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"

LANGUAGE="${1:?Usage: execute.sh <python|nodejs|bash> '<code>' [timeout]}"
CODE="${2:?Missing code argument}"
TIMEOUT="${3:-30}"

curl -s -X POST "${SANDBOX_API_URL}/api/v1/execute" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n \
    --arg lang "$LANGUAGE" \
    --arg code "$CODE" \
    --argjson timeout "$TIMEOUT" \
    '{language: $lang, code: $code, timeout: $timeout}')"
