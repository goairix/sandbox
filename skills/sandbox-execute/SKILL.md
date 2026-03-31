---
name: sandbox-execute
description: Execute code in a secure sandbox. Use when the user asks to run, test, or execute Python, Node.js, or Bash code, perform calculations, process data, or verify code behavior. Supports one-shot execution, streaming output, and network access with domain whitelisting.
compatibility: Requires a running Sandbox API service (default http://localhost:8080) and curl
allowed-tools: Bash(curl:*) Bash(jq:*)
metadata:
  author: goairix
  version: "1.0"
---

# Sandbox Code Execution

Execute untrusted code safely in isolated containers. The sandbox automatically creates a container, runs the code, and destroys the container.

## Configuration

Set these environment variables before use (or use defaults):

- `SANDBOX_API_URL` — Sandbox API base URL (default: `http://localhost:8080`)
- `SANDBOX_API_KEY` — API authentication key (default: `***REDACTED_API_KEY***`)

## One-shot Execution

Run code and get the result in a single call. The sandbox is created and destroyed automatically.

### Python

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/execute" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg code "$CODE" '{language: "python", code: $code}')"
```

### Node.js

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/execute" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg code "$CODE" '{language: "nodejs", code: $code}')"
```

### Bash

```bash
curl -s -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/execute" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg code "$CODE" '{language: "bash", code: $code}')"
```

## Request Format

```json
{
  "language": "python | nodejs | bash",
  "code": "source code to execute",
  "timeout": 30,
  "network": {
    "enabled": true,
    "whitelist": ["www.example.com", "10.0.0.0/8"]
  }
}
```

## Response Format

```json
{
  "exit_code": 0,
  "stdout": "output text",
  "stderr": "error text",
  "duration": 0.142
}
```

- `exit_code` — 0 means success, non-zero means error
- Check `stderr` when `exit_code` is non-zero for error details

## Streaming Execution (SSE)

For long-running code, use the streaming endpoint to get real-time output:

```bash
curl -s -N -X POST "${SANDBOX_API_URL:-http://localhost:8080}/api/v1/execute/stream" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-***REDACTED_API_KEY***}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg code "$CODE" '{language: "python", code: $code}')"
```

Response is Server-Sent Events:

```
event: stdout
data: {"content":"line of output\n"}

event: stderr
data: {"content":"error output\n"}

event: done
data: {"exit_code":0,"elapsed":1.5}
```

## Network Access

By default, sandboxes have no network access. To enable:

- `network.enabled: true` — allow outbound network
- `network.whitelist` — restrict to specific destinations (domain names, IPs, or CIDRs)
- Empty whitelist with `enabled: true` — allow all outbound traffic

## Using the Helper Script

Instead of crafting curl commands manually, use the helper script:

```bash
scripts/execute.sh python 'print("Hello!")'
scripts/execute.sh nodejs 'console.log(42)'
scripts/execute.sh bash 'echo $SHELL'
```

## Common Patterns

### Run calculations

```bash
scripts/execute.sh python 'print(2 ** 100)'
```

### Process data with network access

Use `scripts/execute_with_network.sh` when the code needs to fetch external data:

```bash
scripts/execute_with_network.sh python 'import urllib.request; print(urllib.request.urlopen("http://example.com").read()[:200])' 'example.com'
```

### Install and use packages

```bash
scripts/execute.sh python 'import subprocess; subprocess.run(["pip","install","requests"], capture_output=True); import requests; print(requests.get("http://httpbin.org/get").status_code)'
```

## Error Handling

- If the sandbox service is not running, curl will return a connection error
- If `exit_code` is non-zero, the code had a runtime error — check `stderr`
- If `exit_code` is 137, the process was killed (timeout or OOM)
- Timeout default is 30 seconds, max is 3600 seconds
