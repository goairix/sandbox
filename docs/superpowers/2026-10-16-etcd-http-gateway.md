# Explicit etcd HTTP mode

Run `sandbox --serve-etcd /etc/sandbox/deployment.json` to select the fixed-profile etcd API before legacy configuration, Redis and Manager initialization. It rejects every other explicitly supplied startup flag and positional argument. Startup failure is an error; it never falls back to Redis. The default startup path remains legacy until the rest of the migration is complete.

Use the existing [mounted deployment manifest](2026-10-16-etcd-startup-loader.md), including its operator-provisioned identity, preparation inventory, pinned launcher profile, credentials and reachable signed clock. Add this object to the version1 manifest:

```json
"http": {
  "host": "127.0.0.1",
  "port": 8080,
  "api_key_file": "http/api-key",
  "env": {"PATH": "/usr/local/bin:/usr/bin:/bin"},
  "exec_timeout_seconds": 30,
  "max_exec_timeout_seconds": 600,
  "max_active": 32,
  "requests_per_second": 100
}
```

Host must be a literal IP, port1..65535. The mounted API key file is at most4096bytes and must contain a nonempty printable token; surrounding ASCII whitespace is trimmed. `env` is required and may be empty: it is the complete operator execution base environment. Per-request env overrides individual names. The API process environment is never inherited. UID/GID and network policy come from the mounted launcher.

Omitted/zero numeric settings use the defaults shown above. Execution default must be positive and no greater than the maximum; maximum is at most3600seconds. HTTP active requests are limited to1..32 and the single-principal token bucket to1..10000requests/second. These settings do not increase the original service/Kubernetes/launcher budgets. The older `--check-etcd-startup` command still accepts manifests without an HTTP object and starts no listener.

| Method/path | Behavior |
| --- | --- |
| GET /health | Process availability |
| GET /ready | Bounded original service/trust/durable readiness |
| POST /api/v1/sandboxes | Original plain CreateBusiness and stable key/IDs |
| GET /api/v1/sandboxes/:id | Original Lookup and authenticated current runtime health |
| POST /api/v1/sandboxes/:id/exec | Original execution with bounded combined output |
| POST /api/v1/sandboxes/:id/exec/stream | Original execution through SSE |

All v1 routes require the configured API key in Authorization, normally `Bearer <key>`. Workspace, file, upload, network/TTL mutation, destroy, oneshot and other unmigrated routes return typed501. This explicit mode does not answer them through legacy Manager or Redis.

Creation supports the original plain deployment profile: finite TTL, matching resources/network, no workspace or dependencies. Incompatible requests are rejected. A successful response uses current Lookup metadata, the original CreatedAt and current expiry. `state:running` means an active authenticated healthy runtime; it does not report global user-process busy/idle state.

Creation can omit Idempotency-Key; the service generates one and the response echoes the actual key. Preserve the key and original body. Busy admission before original creation IDs exist returns503/unavailable with Retry-After1 and the actual key. For202/pending or uncertain creation with original IDs, repeat POST with the **same key and original body**.409 means a conflicting idempotent request. There is no request-ID polling index, and a historical creation receipt alone is not a current health proof.

Each Exec invocation creates a new operation. **Do not automatically resend unknown execution.** Idempotency-Key, including an empty header, is rejected before effects; X-Request-ID is attribution only. Verified nonzero exit is a normal result. Unknown execution returns an explicit error and original diagnostic references where available, never a fabricated exit0 or SSEdone. Client disconnect cancels local consumption and joins its owners; it does not prove remote command termination.

SSE verifies socket deadlines and flushes headers before invoking Exec, so HTTP200 means the stream opened. If the original service rejects execution before effects, an `error` event has `state:rejected` and matching sanitized `error`, `code` and `reason` values such as not_found, unsupported_profile, capacity or unavailable. It contains no execution references or terminal result. Capacity/unavailable frames include `retry_after:1` seconds; committed HTTP headers cannot be changed to404/429/503 or gain a Retry-After header. Genuinely unknown outcomes retain exec_unknown, original references and the no-automatic-retry warning. Their reason preserves a specific guard_expired/conflict cause over cancellation caused by that failure, without claiming remote termination.

Execution uses the original bash/Python/Node command builder through `/bin/sh -c`, with `/workspace` as working directory. Timeout0 uses the configured default; negative or excessive values and unsupported tty/line_buffered are rejected. Request bodies are limited to2MiB, with stricter original descriptor stdin/metadata limits retained. Sync stdout+stderr together are limited to1MiB. SSE has one16-entry queue with chunks at most32KiB, backpressure, socket write deadlines and heartbeat.

Shutdown stops admission and listening, cancels requests, releases blocked sockets and joins HTTP/execution owners before closing Deployment/Backend/signer. This mode remains a limited migration slice. Large-resident-N idle costs, long-term retention, remaining APIs, production deployment/restore and final Redis removal still need implementation and validation. Verification scope and results are recorded separately in the HTTP gateway Task report.
