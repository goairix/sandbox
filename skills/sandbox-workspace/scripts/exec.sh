#!/usr/bin/env bash
# Execute code in an existing sandbox
# Usage: exec.sh <sandbox_id> <language> '<code>'
set -euo pipefail

SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:?SANDBOX_API_KEY must be set}"

SANDBOX_ID="${1:?Usage: exec.sh <sandbox_id> <language> '<code>'}"
LANGUAGE="${2:?Missing language argument (python|nodejs|bash)}"
CODE="${3:?Missing code argument}"

curl -s -X POST "${SANDBOX_API_URL}/api/v1/sandboxes/${SANDBOX_ID}/exec" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg lang "$LANGUAGE" --arg code "$CODE" '{language: $lang, code: $code}')"
