# FUSE Mount Diagnostics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve a bounded, credential-safe failure category from an early s3fs exit through the mounter control socket and both runtimes so an API failure identifies the actionable FUSE mount stage.

**Architecture:** The command runner captures at most 16 KiB of s3fs stderr and converts it to one of six fixed codes when `Wait` returns. The supervisor transports that process result without a race, the existing socket `error_code` field carries only an allow-listed code, and the CLI/runtimes recognize only a fixed stderr token. Ready-status value mismatches report field names only; all readiness, cleanup, and network-policy rules remain unchanged.

**Tech Stack:** Go, `os/exec`, Unix socket framed JSON, Docker Engine exec API, Kubernetes SPDY exec, testify.

---

## File map

- Create `internal/mounter/diagnostics.go`: bounded stderr writer, safe diagnostic error type, s3fs classification, and fixed CLI token parsing/formatting shared by mounter and runtimes.
- Create `internal/mounter/diagnostics_test.go`: classification, size bound, and secret non-disclosure tests.
- Modify `internal/mounter/runner.go`: attach the bounded diagnostic writer and return a categorized process error.
- Modify `internal/mounter/supervisor.go`: carry the exact process `Wait` result to readiness checks.
- Modify `internal/mounter/supervisor_test.go`: exercise early-exit result propagation.
- Modify `internal/fuseprotocol/protocol.go`: define the allow-listed socket error codes while retaining the existing response schema.
- Modify `internal/fuseprotocol/protocol_test.go`: verify the error-code allow list.
- Modify `internal/mounter/server.go`: map typed internal diagnostics to fixed socket codes and decode them in the client.
- Modify `internal/mounter/server_test.go`: verify known-code propagation and unknown-error redaction.
- Modify `cmd/workspace-mounter/main.go`: emit only a fixed machine-readable diagnostic token for known safe failures.
- Modify `cmd/workspace-mounter/main_test.go`: verify CLI output never prints internal error text.
- Modify `internal/runtime/docker/control.go`: parse only the fixed diagnostic token from bounded stderr on a failed control exec.
- Modify `internal/runtime/docker/runtime.go`: preserve status-read errors and report only mismatched field names.
- Modify `internal/runtime/docker/control_test.go` and `internal/runtime/docker/runtime_test.go`: verify safe propagation and mismatch redaction.
- Modify `internal/runtime/kubernetes/control.go`: parse only the fixed diagnostic token from bounded stderr on a failed SPDY exec.
- Modify `internal/runtime/kubernetes/runtime.go`: preserve status-read errors and report only mismatched field names.
- Modify `internal/runtime/kubernetes/control_test.go` and `internal/runtime/kubernetes/runtime_test.go`: verify safe propagation and mismatch redaction.

### Task 1: Bounded s3fs diagnostic classification

**Files:**
- Create: `internal/mounter/diagnostics.go`
- Create: `internal/mounter/diagnostics_test.go`
- Modify: `internal/mounter/runner.go`
- Modify: `internal/mounter/runner_unix_test.go`

- [ ] **Step 1: Write failing classification and redaction tests**

Add table tests which feed representative stderr messages for `/dev/fuse` permission, DNS resolution, certificate verification, access denial, missing bucket, and an unknown exit. Assert `DiagnosticCode(err)` equals respectively `fuse-permission`, `endpoint-dns`, `endpoint-tls`, `storage-auth`, `storage-bucket`, and `s3fs-exited`. Include values such as `AKIA-DO-NOT-LEAK`, `secret-value`, and a signed URL, then assert none occur in `err.Error()`.

Add a writer test that writes more than 16 KiB and asserts its stored length is exactly 16 KiB while `Write` returns the original input length and no error.

- [ ] **Step 2: Run the focused tests and confirm the red state**

Run:

```bash
go test ./internal/mounter -run 'Test(BoundedDiagnostic|ClassifyS3FS|CommandRunner)' -count=1
```

Expected: build failure because the diagnostic types and helpers do not exist.

- [ ] **Step 3: Implement the bounded classifier and runner integration**

Define a non-exported 16 KiB writer that discards overflow without short writes. Define an exported diagnostic error through helpers:

```go
const DiagnosticLimit = 16 << 10

type DiagnosticError struct {
    Code     string
    ExitCode int
}

func (e *DiagnosticError) Error() string { return "workspace mounter failure: " + e.Code }
func DiagnosticCode(err error) string
```

The classifier lowercases the captured text and applies ordered patterns, returning only allow-listed codes. `commandProcess` owns the bounded buffer; its `Wait` calls `command.Wait`, extracts `exec.ExitError.ExitCode()` when available, classifies the buffer, clears the retained bytes, and returns `*DiagnosticError`. Keep stdin/stdout attached to `/dev/null` and attach only stderr to the bounded writer.

- [ ] **Step 4: Run the focused tests and confirm the green state**

Run:

```bash
go test ./internal/mounter -run 'Test(BoundedDiagnostic|ClassifyS3FS|CommandRunner)' -count=1
```

Expected: PASS, including the existing process-group cleanup test.

- [ ] **Step 5: Commit Task 1**

```bash
git add internal/mounter/diagnostics.go internal/mounter/diagnostics_test.go internal/mounter/runner.go internal/mounter/runner_unix_test.go
git commit -m "fix: classify bounded s3fs startup failures"
```

### Task 2: Preserve early process exit through the supervisor and socket

**Files:**
- Modify: `internal/fuseprotocol/protocol.go`
- Modify: `internal/fuseprotocol/protocol_test.go`
- Modify: `internal/mounter/supervisor.go`
- Modify: `internal/mounter/supervisor_test.go`
- Modify: `internal/mounter/server.go`
- Modify: `internal/mounter/server_test.go`

- [ ] **Step 1: Write failing supervisor and socket tests**

Add a fake process whose `Wait` returns `&DiagnosticError{Code: "endpoint-tls", ExitCode: 1}`. Authorize it, wait until it exits, call `ReadyStatus`, and assert the returned error has code `endpoint-tls` without any raw stderr.

Add a server/client test where ready status receives that error and assert `Client.Do` returns the same safe code. Add another dispatch error containing `AKIA-DO-NOT-LEAK` and assert the client sees only `rejected` and never the internal text.

- [ ] **Step 2: Run focused tests and confirm they fail**

Run:

```bash
go test ./internal/fuseprotocol ./internal/mounter -run 'Test.*(ErrorCode|EarlyExit|Diagnostic)' -count=1
```

Expected: FAIL because the supervisor drops `Wait` errors and the server always sends `rejected`.

- [ ] **Step 3: Add allow-listed protocol codes and result transport**

Keep `SocketResponse` unchanged and define the six diagnostic constants plus `rejected` in `internal/fuseprotocol/protocol.go`. Add:

```go
func ValidMounterErrorCode(code string) bool
```

In `Supervisor`, add `processResult chan error`. During authorization initialize it with capacity 1. In `reap`, call `Wait`, send the result, close `processDone`, then update state under the mutex. In `waitForMountLocked`, when `processDone` closes, read the buffered result and return its safe diagnostic error; use `s3fs-exited` if no categorized result is available. Reset the result field whenever a new process is assigned or shutdown clears process state.

- [ ] **Step 4: Map only safe errors across the control socket**

Replace the server-local rejection constant with protocol constants. On dispatch failure, call `DiagnosticCode`; send the result only if `ValidMounterErrorCode` accepts it, otherwise send `rejected`. `Client.Do` returns `*DiagnosticError{Code: response.ErrorCode}` only for an allowed code and treats malformed or contradictory responses as invalid.

- [ ] **Step 5: Run focused tests and confirm they pass**

Run:

```bash
go test ./internal/fuseprotocol ./internal/mounter -run 'Test.*(ErrorCode|EarlyExit|Diagnostic|ServerClient)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit Task 2**

```bash
git add internal/fuseprotocol/protocol.go internal/fuseprotocol/protocol_test.go internal/mounter/supervisor.go internal/mounter/supervisor_test.go internal/mounter/server.go internal/mounter/server_test.go
git commit -m "fix: propagate safe mounter failure codes"
```

### Task 3: Carry the safe code through the CLI and runtime exec layers

**Files:**
- Modify: `cmd/workspace-mounter/main.go`
- Modify: `cmd/workspace-mounter/main_test.go`
- Modify: `internal/mounter/diagnostics.go`
- Modify: `internal/mounter/diagnostics_test.go`
- Modify: `internal/runtime/docker/control.go`
- Modify: `internal/runtime/docker/control_test.go`
- Modify: `internal/runtime/kubernetes/control.go`
- Modify: `internal/runtime/kubernetes/control_test.go`

- [ ] **Step 1: Write failing CLI-token tests**

Define the exact stderr grammar as:

```text
workspace-mounter-error:<allow-listed-code>\n
```

Test formatting and parsing for every allowed code. Test that surrounding text, multiple lines, unknown codes, credential-looking values, oversized input, and missing newline are rejected.

Test the CLI error printer with a safe diagnostic error and with `errors.New("AKIA-DO-NOT-LEAK")`: the former prints only the fixed token, while the latter prints only `workspace-mounter: request failed`.

- [ ] **Step 2: Run CLI and control tests and confirm they fail**

Run:

```bash
go test ./cmd/workspace-mounter ./internal/runtime/docker ./internal/runtime/kubernetes -run 'Test.*(Diagnostic|ControlError)' -count=1
```

Expected: FAIL because failed exec stderr is currently discarded.

- [ ] **Step 3: Implement the fixed CLI token**

Add `FormatDiagnosticToken(error) (string, bool)` and `ParseDiagnosticToken([]byte) (string, bool)` in `diagnostics.go`. Both use exact matching and `fuseprotocol.ValidMounterErrorCode`; they never interpolate arbitrary text. Refactor `main` through a testable `printFailure(io.Writer, error)` helper which prints the token only for a recognized diagnostic and otherwise retains the generic message.

- [ ] **Step 4: Implement Docker and Kubernetes safe parsing**

In Docker `execFixed`, retain the existing bounded stderr buffer. When exit code is nonzero, parse the exact token and return `workspace mounter control failed: <code>` only when recognized; otherwise keep `Docker workspace control command failed`.

In Kubernetes SPDY execution, after `StreamWithContext` fails, parse the exact token from its bounded stderr and return `workspace mounter control failed: <code>` only when recognized; otherwise keep the existing generic wrapped execution error. Do not return other stderr.

- [ ] **Step 5: Run CLI and control tests and confirm they pass**

Run:

```bash
go test ./cmd/workspace-mounter ./internal/runtime/docker ./internal/runtime/kubernetes -run 'Test.*(Diagnostic|ControlError)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit Task 3**

```bash
git add cmd/workspace-mounter/main.go cmd/workspace-mounter/main_test.go internal/mounter/diagnostics.go internal/mounter/diagnostics_test.go internal/runtime/docker/control.go internal/runtime/docker/control_test.go internal/runtime/kubernetes/control.go internal/runtime/kubernetes/control_test.go
git commit -m "fix: surface safe fuse diagnostics from runtime exec"
```

### Task 4: Preserve ready errors and redact mismatch values

**Files:**
- Modify: `internal/runtime/docker/runtime.go`
- Modify: `internal/runtime/docker/runtime_test.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Modify: `internal/runtime/kubernetes/runtime_test.go`

- [ ] **Step 1: Write failing Docker and Kubernetes readiness tests**

For each runtime, make the health-ready control operation return `workspace mounter control failed: endpoint-tls` and assert `WaitSandboxReady` retains both `read workspace ready status` and `endpoint-tls`.

For each runtime, return a syntactically valid ready status with wrong state, generation, and cache limit. Assert the error reports only sorted field names such as `state,generation,cache_limit` and does not contain runtime UID, pool key, numeric field values, endpoint, or prefix.

- [ ] **Step 2: Run focused readiness tests and confirm they fail**

Run:

```bash
go test ./internal/runtime/docker ./internal/runtime/kubernetes -run 'TestWait.*(Preserves|Mismatch)' -count=1
```

Expected: FAIL because Docker replaces the read error and both runtimes use one generic mismatch message.

- [ ] **Step 3: Implement explicit status-read handling and mismatch field collection**

In each `WaitSandboxReady`, split transport failure from value validation:

```go
status, err := r.readMounterStatus(ctx, ref, "ready")
if err != nil {
    return nil, fmt.Errorf("read workspace ready status: %w", err)
}
if fields := readyStatusMismatches(status, expected...); len(fields) != 0 {
    return nil, fmt.Errorf("workspace ready status mismatch: %s", strings.Join(fields, ","))
}
```

The helper appends fixed field names in source-defined order and never formats values. Preserve the Kubernetes exact runtime-UID rejection and all existing probe/cache conditions.

- [ ] **Step 4: Run focused readiness tests and confirm they pass**

Run:

```bash
go test ./internal/runtime/docker ./internal/runtime/kubernetes -run 'TestWait.*(Preserves|Mismatch|Ready)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit Task 4**

```bash
git add internal/runtime/docker/runtime.go internal/runtime/docker/runtime_test.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
git commit -m "fix: preserve fuse ready failure stages"
```

### Task 5: Regression verification and online reproduction

**Files:**
- Modify only a file listed above if a verified regression requires a narrowly scoped correction.

- [ ] **Step 1: Format and run package tests**

Run:

```bash
gofmt -w internal/mounter/diagnostics.go internal/mounter/diagnostics_test.go internal/mounter/runner.go internal/mounter/runner_unix_test.go internal/mounter/supervisor.go internal/mounter/supervisor_test.go internal/mounter/server.go internal/mounter/server_test.go internal/fuseprotocol/protocol.go internal/fuseprotocol/protocol_test.go cmd/workspace-mounter/main.go cmd/workspace-mounter/main_test.go internal/runtime/docker/control.go internal/runtime/docker/control_test.go internal/runtime/docker/runtime.go internal/runtime/docker/runtime_test.go internal/runtime/kubernetes/control.go internal/runtime/kubernetes/control_test.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
go test ./internal/fuseprotocol ./internal/mounter ./cmd/workspace-mounter ./internal/runtime/docker ./internal/runtime/kubernetes -count=1
```

Expected: PASS.

- [ ] **Step 2: Run repository verification**

Run:

```bash
go test ./... -count=1
go vet ./...
docker compose -f docker/docker-compose.yml config >/dev/null
helm lint deploy/helm/sandbox
helm template sandbox deploy/helm/sandbox >/dev/null
```

The repository's FUSE contract is the Go package `./test/integration/workspacefuse`, already included by `go test ./...`; run it separately with `go test ./test/integration/workspacefuse -count=1` to retain explicit evidence. Expected: every command exits 0.

- [ ] **Step 3: Review the diff for secret and policy safety**

Run:

```bash
git diff --check
git diff --stat
git status --short
rg -n 'AKIA-DO-NOT-LEAK|secret-value' --glob '!**/*_test.go' .
```

Expected: no whitespace errors, no test sentinel outside tests, and no change to Docker/Kubernetes network-policy construction.

- [ ] **Step 4: Build and deploy the required versioned images**

Build only `sandbox-api`, `sandbox-fuse-mounter`, and `sandbox-fuse-docker` using their existing directory-local Dockerfiles and the user's version tag. Update the deployed Docker Compose image tags without adding environment variables, then run `docker compose up -d` through the normal deployment workflow.

Expected: API and both pool types become healthy; no Docker daemon restart is performed.

- [ ] **Step 5: Re-run the online API test safely**

Create a FUSE sandbox using the exact user-provided workspace path. If creation still fails, record the new fixed category and inspect only relevant container/runtime state. If it succeeds, write one unique hidden verification file, read back only that file, delete it, verify absence, and destroy the sandbox. Never enumerate or read existing object contents beyond the minimum mount readiness operation performed by the product.

- [ ] **Step 6: Commit any verification-only correction and report evidence**

If verification required a code correction, stage only those exact corrected files and commit them as `fix: complete fuse mount diagnostics`. Report test commands, image tags, online sandbox cleanup status, and the exact safe failure category or successful FUSE lifecycle result.
