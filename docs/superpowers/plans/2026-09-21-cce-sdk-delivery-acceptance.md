# CCE SDK Delivery Acceptance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a standalone SDK smoke-test binary, document its build/deployment workflow, and use it to validate ordinary and FUSE sandboxes on the current CCE cluster.

**Architecture:** The smoke test will use only the existing public Go SDK methods, create one ordinary and one FUSE sandbox, execute commands, verify workspace sync/mount state, and always destroy resources with a bounded cleanup context. Operational documentation will provide reproducible cross-architecture builds, safe port-forward/API-key handling, Helm guidance, and post-test cleanup checks.

**Tech Stack:** Go 1.22+, existing `sdk/go` client, `httptest`, Kubernetes `kubectl`, Helm, Markdown runbooks.

---

### Task 1: Add the SDK smoke-test command with unit coverage

**Files:**
- Create: `sdk/go/cmd/sandbox-sdk-smoke/main.go`
- Create: `sdk/go/cmd/sandbox-sdk-smoke/main_test.go`

- [ ] **Step 1: Write failing tests** for ordinary and FUSE workflows using an `httptest.Server`; assert that the public SDK creates, executes, syncs, reads workspace info, and destroys sandboxes, and that an unmounted FUSE response fails validation.
- [ ] **Step 2: Run `cd sdk/go && go test ./cmd/sandbox-sdk-smoke -count=1` and confirm the new package fails because the command implementation is absent.
- [ ] **Step 3: Implement the command.** Read `SANDBOX_API_URL`, `SANDBOX_API_KEY`, and optional `SANDBOX_TEST_PREFIX`; use a five-minute overall timeout and a 30-second cleanup timeout. Run ordinary mode with sync workspace and FUSE mode with persistent workspace. Require successful command output; for FUSE require `mounted`, `mount_type=fuse`, `mount_state=ready`, and `flushed`; never print credentials.
- [ ] **Step 4: Run `gofmt -w sdk/go/cmd/sandbox-sdk-smoke/*.go && cd sdk/go && go test ./cmd/sandbox-sdk-smoke -count=1`; expect PASS.

### Task 2: Update delivery and SDK documentation

**Files:**
- Modify: `tools/apparmor-hce/deploy-runbook.md`
- Modify: `docs/deployment/helm-deployment-upgrade.md`
- Modify: `sdk/go/README.md`

- [ ] **Step 1:** Add amd64 and arm64 smoke-test build commands, SHA256 verification, port-forward/API-key environment setup, invocation, and cleanup/status checks to the AppArmor delivery runbook.
- [ ] **Step 2:** Remove `--atomic` from built-in Redis Sentinel install/upgrade examples and explain that standalone/external Redis may use it only when its readiness behavior is understood.
- [ ] **Step 3:** Link the smoke-test command from the SDK README and document the ordinary/FUSE acceptance criteria and expected idle FUSE pool `1/2` readiness state.
- [ ] **Step 4:** Run `git diff --check` and render the changed command snippets with `sed` to verify there are no contradictory instructions or credential-printing examples.

### Task 3: Build and run live SDK acceptance on CCE

**Files:**
- Build artifact only: `/tmp/sandbox-sdk-smoke-amd64` or `/tmp/sandbox-sdk-smoke-arm64`

- [ ] **Step 1:** Run `cd sdk/go && go test ./... -count=1`.
- [ ] **Step 2:** Build the Linux arm64 smoke binary with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o /tmp/sandbox-sdk-smoke-arm64 ./cmd/sandbox-sdk-smoke`; verify it with `file` and `sha256sum`.
- [ ] **Step 3:** With `KUBECONFIG=var/tmp/cce/cce-sandbox-fuse-v2-kubeconfig.yaml`, port-forward `svc/sandbox-fuse-api`, read the API key only into an environment variable, and run the smoke binary using a timestamped prefix.
- [ ] **Step 4:** Verify the process exits zero, the log contains ordinary and FUSE success markers but no API key, and the test prefix has no remaining sandbox Pods or NetworkPolicies. Check API, AppArmor loader, Sentinel, and PVC readiness without changing the release.

### Task 4: Final verification and commit

- [ ] **Step 1:** Run `git diff --check`, `cd sdk/go && go test ./... -count=1`, and inspect `git status --short`; preserve the pre-existing unrelated modification to `docs/superpowers/plans/2026-09-17-sentinel-kubernetes-129-compatibility.md`.
- [ ] **Step 2:** Commit only the new plan, SDK command/tests, and documentation with `git add ... && git commit -m "feat: add SDK delivery acceptance smoke test"`.
- [ ] **Step 3:** Report exact test/build/live-cluster evidence and the files delivered to operations.
