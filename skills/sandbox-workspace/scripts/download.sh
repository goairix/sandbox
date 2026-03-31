#!/usr/bin/env bash
# Download a file from sandbox
# Usage: download.sh <sandbox_id> <remote_path> [local_output]
set -euo pipefail

SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"

SANDBOX_ID="${1:?Usage: download.sh <sandbox_id> <remote_path> [local_output]}"
REMOTE_PATH="${2:?Missing remote path (e.g. /workspace/result.json)}"
LOCAL_OUTPUT="${3:-$(basename "$REMOTE_PATH")}"

curl -s "${SANDBOX_API_URL}/api/v1/sandboxes/${SANDBOX_ID}/files/download?path=${REMOTE_PATH}" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -o "${LOCAL_OUTPUT}"

echo "Downloaded to ${LOCAL_OUTPUT}"
