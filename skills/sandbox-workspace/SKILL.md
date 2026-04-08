---
name: sandbox-workspace
description: Manage persistent sandbox workspaces with file operations. Use when the user needs a long-lived environment to upload files, run multi-step code, maintain state across executions, or download results. Supports creating sandboxes, executing code, uploading/downloading files, and listing workspace contents.
compatibility: Requires a running Sandbox API service (default http://localhost:8080) and curl
allowed-tools: Bash(curl:*) Bash(jq:*)
metadata:
  author: goairix
  version: "2.0"
---

# Sandbox Workspace Management

Manage persistent sandbox environments for multi-step tasks that require state, file operations, and iterative development. A single sandbox supports Python, Node.js/TypeScript, and Bash simultaneously.

## Configuration

- `SANDBOX_API_URL` — Sandbox API base URL (default: `http://localhost:8080`)
- `SANDBOX_API_KEY` — API authentication key

## Workflow

A typical workspace session:

1. **Create** a persistent sandbox (no language needed — supports all languages)
2. **Upload** files to `/workspace/`
3. **Execute** code in any language (state persists across calls)
4. **List/Download** result files
5. **Destroy** the sandbox when done

Always destroy sandboxes after use to free resources.

## Create Sandbox

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"mode": "persistent"}'
```

Response:

```json
{
  "id": "sandbox-a7k2mxb3f9",
  "mode": "persistent",
  "state": "ready",
  "created_at": "2026-04-08T12:00:00Z"
}
```

Save the `id` for all subsequent operations.

### With dependencies (pip + npm):

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{
    "mode": "persistent",
    "dependencies": [
      {"name": "flask", "version": "3.0.0", "manager": "pip"},
      {"name": "express", "version": "4.18.2", "manager": "npm"}
    ]
  }'
```

### With workspace (auto-mount persistent storage):

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"mode": "persistent", "workspace_path": "user123/project-a"}'
```

## Execute Code

Specify the language at execution time. The same sandbox can run Python, Node.js, and Bash.

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/exec" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg code "$CODE" --arg lang "$LANG" '{language: $lang, code: $code}')"
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

Supported languages: `python`, `nodejs`, `bash`.

State persists between calls — variables, files, and installed packages remain available.

## Streaming Execution

```bash
curl -s -N -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/exec/stream" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg code "$CODE" --arg lang "$LANG" '{language: $lang, code: $code}')"
```

## Upload File

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/files/upload" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -F "file=@/path/to/local/file.csv" \
  -F "path=/workspace/file.csv"
```

Path must start with `/workspace/` or `/tmp/`.

## List Files

```bash
curl -s "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/files/list?path=/workspace" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}"
```

## Download File

```bash
curl -s "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/files/download?path=/workspace/result.json" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -o result.json
```

## Get Sandbox Status

```bash
curl -s "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}"
```

## Destroy Sandbox

```bash
curl -s -X DELETE "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}"
```

## Workspace Operations

```bash
# Mount workspace
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/workspace/mount" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"root_path":"user123/project-a"}'

# Sync from container (incremental — only changed files)
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/workspace/sync" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"direction":"from_container"}'

# Unmount (auto incremental sync back to storage)
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/sandboxes/${SANDBOX_ID}/workspace/unmount" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}"
```

## Using Helper Scripts

```bash
# Create sandbox and get ID
SANDBOX_ID=$(scripts/create.sh)

# Execute code (specify language)
scripts/exec.sh "$SANDBOX_ID" python 'x = 42; print(x)'
scripts/exec.sh "$SANDBOX_ID" nodejs 'console.log("hello")'
scripts/exec.sh "$SANDBOX_ID" bash 'echo $PATH'

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

### Multi-language data pipeline

```bash
SANDBOX_ID=$(scripts/create.sh)

# Step 1: Python data processing
scripts/exec.sh "$SANDBOX_ID" python '
import json
data = [{"name": "Alice", "score": 95}, {"name": "Bob", "score": 87}]
with open("/workspace/data.json", "w") as f:
    json.dump(data, f)
print("Data written")
'

# Step 2: Node.js transformation
scripts/exec.sh "$SANDBOX_ID" nodejs '
const fs = require("fs");
const data = JSON.parse(fs.readFileSync("/workspace/data.json"));
const avg = data.reduce((s, d) => s + d.score, 0) / data.length;
console.log(`Average score: ${avg}`);
'

# Step 3: Bash file inspection
scripts/exec.sh "$SANDBOX_ID" bash 'wc -c /workspace/data.json'

scripts/destroy.sh "$SANDBOX_ID"
```

## Important Notes

- Sandbox default timeout is 3600 seconds (1 hour) — destroy before that if done early
- File paths must start with `/workspace/` or `/tmp/`
- File paths cannot contain `..` (path traversal prevention)
- Max request body: 64MB, max single file: 32MB
- Persistent sandboxes survive API restarts (state stored in Redis)
- Workspace sync from container is incremental — only modified files are written back to storage
