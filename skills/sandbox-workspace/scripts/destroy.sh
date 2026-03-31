#!/usr/bin/env bash
# Destroy a sandbox
# Usage: destroy.sh <sandbox_id>
set -euo pipefail

SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"

SANDBOX_ID="${1:?Usage: destroy.sh <sandbox_id>}"

curl -s -X DELETE "${SANDBOX_API_URL}/api/v1/sandboxes/${SANDBOX_ID}" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}"
