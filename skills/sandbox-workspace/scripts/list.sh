#!/usr/bin/env bash
# List files in sandbox
# Usage: list.sh <sandbox_id> [path]
set -euo pipefail

SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"

SANDBOX_ID="${1:?Usage: list.sh <sandbox_id> [path]}"
DIR_PATH="${2:-/workspace}"

curl -s "${SANDBOX_API_URL}/api/v1/sandboxes/${SANDBOX_ID}/files/list?path=${DIR_PATH}" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}"
