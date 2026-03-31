#!/usr/bin/env bash
# Execute code in an existing sandbox
# Usage: exec.sh <sandbox_id> '<code>'
set -euo pipefail

SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"

SANDBOX_ID="${1:?Usage: exec.sh <sandbox_id> '<code>'}"
CODE="${2:?Missing code argument}"

curl -s -X POST "${SANDBOX_API_URL}/api/v1/sandboxes/${SANDBOX_ID}/exec" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg code "$CODE" '{code: $code}')"
