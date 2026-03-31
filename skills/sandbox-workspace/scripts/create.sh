#!/usr/bin/env bash
# Create a persistent sandbox and print the sandbox ID
# Usage: create.sh <python|nodejs|bash>
set -euo pipefail

SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"

LANGUAGE="${1:?Usage: create.sh <python|nodejs|bash>}"

curl -s -X POST "${SANDBOX_API_URL}/api/v1/sandboxes" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg lang "$LANGUAGE" '{language: $lang, mode: "persistent"}')" \
  | jq -r '.id'
