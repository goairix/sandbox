#!/usr/bin/env bash
# Upload a file to sandbox
# Usage: upload.sh <sandbox_id> <local_file> <dest_path>
set -euo pipefail

SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"

SANDBOX_ID="${1:?Usage: upload.sh <sandbox_id> <local_file> <dest_path>}"
LOCAL_FILE="${2:?Missing local file path}"
DEST_PATH="${3:?Missing destination path (e.g. /workspace/data.csv)}"

curl -s -X POST "${SANDBOX_API_URL}/api/v1/sandboxes/${SANDBOX_ID}/files/upload" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -F "file=@${LOCAL_FILE}" \
  -F "path=${DEST_PATH}"
