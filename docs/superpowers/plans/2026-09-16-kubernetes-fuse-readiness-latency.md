# Kubernetes FUSE Readiness Latency Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the kubelet readiness-probe interval from warm-pool FUSE sandbox creation while preserving exact-runtime, mount-health, and propagation safety checks.

**Architecture:** Replace the blocking `PodReady` gate in `WaitSandboxReady` with a bounded exact-Pod/mounter readiness loop. Each attempt proves the immutable Pod identity and live container state before running the mounter health command; a successful health result is followed by another exact-Pod validation and the existing sandbox-side propagation probe.

**Tech Stack:** Go, client-go fake client, Kubernetes Pod exec control protocol, OpenTelemetry metrics, `testify`.

---

### Task 1: Lock the readiness behavior with failing tests

**Files:**
- Modify: `internal/runtime/kubernetes/runtime_test.go:1430-1520`

- [ ] **Step 1: Add a failing no-PodReady regression test**

Add a test that prepares and authorizes a FUSE runtime without ever setting `PodReady=True`, calls
`WaitSandboxReady`, and expects a successful exact RuntimeUID plus the final
`write-read-delete` command. Set `readyTimeout` to 20 milliseconds so the old implementation fails
quickly with `wait for FUSE Pod Ready`.

```go
func TestWaitReadyUsesMounterProofWithoutBlockingOnPodReady(t *testing.T) {
	script := preparedScript()
	rt, _ := newFakeKubernetesRuntime(t, script)
	rt.readyTimeout = 20 * time.Millisecond
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{
		RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey,
		WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1,
	}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))

	ready, err := rt.WaitSandboxReady(context.Background(), ref, 7)
	require.NoError(t, err)
	assert.Equal(t, ref.UID, ready.RuntimeUID)
	last := script.commands[len(script.commands)-1]
	assert.Equal(t, sandboxContainer, last.container)
	assert.Equal(t, []string{workspaceProbeBinary, "write-read-delete", "--runtime-uid", ref.UID, "--generation", "7"}, last.argv)
}
```

- [ ] **Step 2: Run the regression test and verify RED**

Run:

```bash
go test ./internal/runtime/kubernetes -run '^TestWaitReadyUsesMounterProofWithoutBlockingOnPodReady$' -count=1
```

Expected: FAIL with `wait for FUSE Pod Ready: context deadline exceeded`.

- [ ] **Step 3: Add failing retry and fail-closed tests**

Add focused tests using `commandScript` and fake Pod status updates:

- the first ready-health exec fails and the second returns canonical ready JSON; expect success and exactly two health calls;
- health never succeeds; expect `wait for workspace mounter ready`, `context deadline exceeded`, and no propagation command;
- Pod deletion or mounter restart observed before a retry; expect immediate failure and no propagation command;
- a ready response with wrong generation/cache contract; expect the existing field-only mismatch error and no propagation command.

Use `atomic.Int32` counters and the existing fake runtime. Do not add production test hooks or sleep
longer than the runtime's millisecond poll interval.

- [ ] **Step 4: Run the focused group and confirm failures are caused by the old PodReady gate**

Run:

```bash
go test ./internal/runtime/kubernetes -run 'TestWaitReady' -count=1
```

Expected: the new no-PodReady/retry tests fail at the PodReady timeout; existing PodReady-based tests
remain unchanged.

- [ ] **Step 5: Commit the RED tests**

```bash
git add internal/runtime/kubernetes/runtime_test.go
git commit -m "test: expose kubernetes fuse readiness delay"
```

### Task 2: Wait on exact mounter readiness instead of PodReady

**Files:**
- Modify: `internal/runtime/kubernetes/runtime.go:617-675`
- Modify: `internal/runtime/kubernetes/runtime.go:2235-2264`

- [ ] **Step 1: Add exact healthy-Pod validation for the readiness loop**

Replace `waitReadyPod` with a helper that uses the existing total `readyTimeout`, calls `getExactPod`,
requires `validateExactFUSEPodIdentity`, the `sandbox.pool.state=prepared` label, matching PoolKey, and
`validatePreparedContainerState` before every health command.

```go
func (r *Runtime) exactHealthyFUSEPod(ctx context.Context, ref runtime.RuntimeRef, poolKey string) (*corev1.Pod, preparedMounterBootstrap, error) {
	pod, err := r.getExactPod(ctx, ref)
	if err != nil {
		return nil, preparedMounterBootstrap{}, err
	}
	bootstrap, err := exactFUSEPodBootstrap(pod, ref)
	if err != nil || pod.Labels["sandbox.pool.state"] != "prepared" || bootstrap.PoolKey != poolKey {
		return nil, preparedMounterBootstrap{}, fmt.Errorf("FUSE Pod identity labels changed")
	}
	if err := validatePreparedContainerState(pod); err != nil {
		return nil, preparedMounterBootstrap{}, err
	}
	return pod, bootstrap, nil
}
```

- [ ] **Step 2: Implement the bounded mounter-ready loop**

Add `waitMounterReady` with one timeout context for all GET, exec, and poll operations. Retry transient
health-command failures while the exact Pod remains healthy. Return `runtime.ErrInvalidRuntimeRef`
immediately. After a successful health command, re-read and revalidate the exact Pod before accepting
the status; reject non-canonical status fields with `kubernetesReadyStatusMismatches`.

```go
func (r *Runtime) waitMounterReady(ctx context.Context, ref runtime.RuntimeRef, poolKey string, generation int64) (*corev1.Pod, error) {
	timeout := r.readyTimeout
	if timeout <= 0 {
		timeout = defaultKubernetesControlTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		if _, _, err := r.exactHealthyFUSEPod(waitCtx, ref, poolKey); err != nil {
			return nil, err
		}
		health, err := r.readMounterStatus(waitCtx, ref, "ready")
		if err == nil {
			pod, bootstrap, podErr := r.exactHealthyFUSEPod(waitCtx, ref, poolKey)
			if podErr != nil {
				return nil, podErr
			}
			mismatches := kubernetesReadyStatusMismatches(health, ref.UID, poolKey, generation, bootstrap.CacheLimitBytes)
			if len(mismatches) == 0 {
				return pod, nil
			}
			return nil, fmt.Errorf("workspace ready status mismatch: %s", strings.Join(mismatches, ","))
		}
		if errors.Is(err, runtime.ErrInvalidRuntimeRef) {
			return nil, err
		}
		lastErr = err
		if pollErr := waitPoll(waitCtx, r.pollInterval); pollErr != nil {
			return nil, fmt.Errorf("wait for workspace mounter ready: %w", errors.Join(lastErr, pollErr))
		}
	}
}
```

Add `kubernetesMounterStatusPending` and retry only the precise pending shape: `state=mounting`, exact
RuntimeUID/PoolKey/Generation, empty mount type, no restart/cache overflow, non-negative cache bytes
below the exact bootstrap cache limit, and an equal cache limit. All other returned status mismatches
fail immediately. The current CLI normally represents mounting as a retryable exec error, while this
predicate keeps the wire-level state machine explicit and safe.

- [ ] **Step 3: Switch `WaitSandboxReady` to the new proof**

Replace the serial `waitReadyPod` and one-shot `readMounterStatus` blocks with one call to
`waitMounterReady`. Record it as stage `mounter_ready_wait`; retain the existing propagation probe and
return construction unchanged.

```go
started := time.Now()
pod, err := r.waitMounterReady(ctx, ref, poolKey, generation)
result := "success"
if err != nil {
	result = "error"
}
metrics.RecordWorkspaceStage(ctx, "kubernetes", "", "mounter_ready_wait", result, time.Since(started).Seconds())
if err != nil {
	return nil, err
}
```

- [ ] **Step 4: Run the focused tests and verify GREEN**

Run:

```bash
go test ./internal/runtime/kubernetes -run 'TestWaitReady' -count=1
```

Expected: PASS.

- [ ] **Step 5: Run the Kubernetes runtime package repeatedly and under race**

Run:

```bash
go test ./internal/runtime/kubernetes -count=5
go test -race ./internal/runtime/kubernetes -run 'TestWaitReady' -count=10
```

Expected: PASS with no race report.

- [ ] **Step 6: Commit the implementation**

```bash
git add internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
git commit -m "perf: remove pod readiness delay from fuse creation"
```

### Task 3: Verify scope and document the deploy-time acceptance

**Files:**
- Create: `docs/testing/2026-09-16-kubernetes-fuse-readiness-latency-remediation.md`

- [ ] **Step 1: Run formatting and static verification**

Run:

```bash
gofmt -w internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
go test ./internal/runtime/kubernetes ./internal/sandbox ./internal/mounter ./cmd/workspace-mounter
go vet ./internal/runtime/kubernetes ./internal/sandbox ./internal/mounter ./cmd/workspace-mounter
git diff --check
```

Expected: every command exits 0 and `git diff --check` prints nothing.

- [ ] **Step 2: Run the full Go regression suite**

Run:

```bash
go test ./...
```

Expected: PASS. Any environment-gated integration tests must report their explicit skip rather than be
described as executed.

- [ ] **Step 3: Write the remediation and rollout record**

Record the measured root cause, RED/GREEN evidence, retained security checks, exact commands and
results, the fact that only `sandbox-api` requires rebuilding, and the still-pending 30-request cluster
benchmark after the user deploys the image. Do not claim the online p95 target before that benchmark.

- [ ] **Step 4: Self-review the final diff**

Verify no readinessProbe template/period, Helm values, Redis schema, API contract, workspace-mounter
protocol or network policy changed. Verify errors and logs do not expose RuntimeUID, PoolKey, prefix,
credentials or raw control output.

- [ ] **Step 5: Commit verification documentation**

```bash
git add docs/testing/2026-09-16-kubernetes-fuse-readiness-latency-remediation.md
git commit -m "docs: record fuse readiness latency remediation"
```
