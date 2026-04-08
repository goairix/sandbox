#!/usr/bin/env bash
# Create a persistent sandbox and print the sandbox ID
# Usage: create.sh
set -euo pipefail

SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:?SANDBOX_API_KEY must be set}"

curl -s -X POST "${SANDBOX_API_URL}/api/v1/sandboxes" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"mode": "persistent"}' \
  | jq -r '.id'
