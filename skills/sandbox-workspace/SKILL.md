---
name: sandbox-workspace
description: Manage persistent sandbox workspaces with file operations. Use when the user needs a long-lived environment to upload files, run multi-step code, maintain state across executions, or download results. Supports creating sandboxes, executing code, uploading/downloading files, and listing workspace contents.
compatibility: Requires a running Sandbox API service (default http://localhost:8080) and curl
allowed-tools: Bash(curl:*) Bash(jq:*)
metadata:
  author: goairix
  version: "1.0"
---

# Sandbox Workspace Management

Manage persistent sandbox environments for multi-step tasks that require state, file operations, and iterative development.

## Configuration

- `SANDBOX_API_URL` — Sandbox API base URL (default: `http://localhost:8080`)
- `SANDBOX_API_KEY` — API authentication key (default: `***REDACTED_API_KEY***`)

## Workflow

A typical workspace session:

1. **Create** a persistent sandbox
2. **Upload** files to `/workspace/`
3. **Execute** code (state persists across calls)
4. **List/Download** result files
5. **Destroy** the sandbox when done

Always destroy sandboxes after use to free resources.

## Create Sandbox

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}" \
  -H "Content-Type: application/json" \
  -d '{"language": "python", "mode": "persistent"}'
```

Response:

```json
{
  "id": "sb-python-x7k2m",
  "language": "python",
  "mode": "persistent",
  "state": "ready",
  "created_at": "2026-03-30T12:00:00Z"
}
```

Save the `id` for all subsequent operations.

Supported languages: `python`, `nodejs`, `bash`.

## Execute Code

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/exec" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg code "$CODE" '{code: $code}')"
```

Response:

```json
{
  "exit_code": 0,
  "stdout": "output",
  "stderr": "",
  "duration": 0.05
}
```

State persists between calls — variables, files, and installed packages remain available.

## Streaming Execution

```bash
curl -s -N -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/exec/stream" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg code "$CODE" '{code: $code}')"
```

## Upload File

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/files/upload" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}" \
  -F "file=@/path/to/local/file.csv" \
  -F "path=/workspace/file.csv"
```

Response:

```json
{"path": "/workspace/file.csv", "size": 1024}
```

Path must start with `/workspace/` or `/tmp/`.

## List Files

```bash
curl -s "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/files/list?path=/workspace" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"
```

Response:

```json
{
  "files": [
    {"name": "data.csv", "path": "/workspace/data.csv", "size": 1024, "is_dir": false},
    {"name": "output", "path": "/workspace/output", "size": 4096, "is_dir": true}
  ],
  "path": "/workspace"
}
```

## Download File

```bash
curl -s "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/files/download?path=/workspace/result.json" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}" \
  -o result.json
```

## Get Sandbox Status

```bash
curl -s "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"
```

## Destroy Sandbox

```bash
curl -s -X DELETE "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}"
```

## Using Helper Scripts

```bash
# Create sandbox and get ID
SANDBOX_ID=$(scripts/create.sh python)

# Execute code
scripts/exec.sh "$SANDBOX_ID" 'x = 42; print(x)'

# Upload a file
scripts/upload.sh "$SANDBOX_ID" ./data.csv /workspace/data.csv

# List files
scripts/list.sh "$SANDBOX_ID"

# Download a file
scripts/download.sh "$SANDBOX_ID" /workspace/result.json ./result.json

# Destroy
scripts/destroy.sh "$SANDBOX_ID"
```

## Common Patterns

### Data analysis pipeline

```bash
SANDBOX_ID=$(scripts/create.sh python)
scripts/upload.sh "$SANDBOX_ID" ./sales.csv /workspace/sales.csv
scripts/exec.sh "$SANDBOX_ID" '
import csv
with open("/workspace/sales.csv") as f:
    rows = list(csv.DictReader(f))
total = sum(float(r["amount"]) for r in rows)
print(f"Total sales: ${total:,.2f}")
print(f"Number of transactions: {len(rows)}")
'
scripts/destroy.sh "$SANDBOX_ID"
```

### Multi-step development

```bash
SANDBOX_ID=$(scripts/create.sh python)

# Step 1: Install packages
scripts/exec.sh "$SANDBOX_ID" 'import subprocess; subprocess.run(["pip","install","requests"], capture_output=True)'

# Step 2: Use installed packages (they persist)
scripts/exec.sh "$SANDBOX_ID" 'import requests; print(requests.__version__)'

# Step 3: Write and run a script
scripts/exec.sh "$SANDBOX_ID" '
with open("/workspace/app.py", "w") as f:
    f.write("print(\"Hello from app.py\")")
'
scripts/exec.sh "$SANDBOX_ID" 'exec(open("/workspace/app.py").read())'

scripts/destroy.sh "$SANDBOX_ID"
```

## Important Notes

- Sandbox default timeout is 3600 seconds (1 hour) — destroy before that if done early
- File paths must start with `/workspace/` or `/tmp/`
- File paths cannot contain `..` (path traversal prevention)
- Max request body: 64MB, max single file: 32MB
