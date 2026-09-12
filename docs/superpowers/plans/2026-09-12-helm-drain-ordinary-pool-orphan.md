# Helm Drain Ordinary Pool Orphan Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make release-wide drain remove ordinary warm-pool runtimes that are not present in the drain Job's in-memory Pool before Kubernetes zero-state audit runs.

**Architecture:** Stop, cancel, and wait for ordinary Pool refill before taking the final drain inventory. Then, only for Kubernetes, use a fail-closed Manager helper that lists `sandbox.managed=true,sandbox.pool=true` runtimes, excludes both Kubernetes- and Docker-shaped FUSE identities, validates returned ownership labels, and aggregates exact removal errors; Docker keeps its existing in-memory drain because historical labels do not provide the same safe orphan-scan contract.

**Tech Stack:** Go 1.25, `errors.Join`, sandbox runtime abstraction, testify unit tests

---

### Task 1: Reproduce the release-drain orphan gap

**Files:**
- Modify: `internal/sandbox/pool_test.go`
- Modify: `internal/sandbox/drain_audit_test.go`

- [ ] **Step 1: Add an injectable ListSandboxes error to the test runtime**

Add `listSandboxesErr error` beside `listedSandboxes`, and make the existing test implementation return it before copying results:

```go
func (m *mockRuntime) ListSandboxes(_ context.Context, labels map[string]string) ([]runtime.SandboxInfo, error) {
	 m.mu.Lock()
	 defer m.mu.Unlock()
	 m.listLabels = maps.Clone(labels)
	 if m.listSandboxesErr != nil {
	 	return nil, m.listSandboxesErr
	 }
	 return append([]runtime.SandboxInfo(nil), m.listedSandboxes...), nil
}
```

Import `maps` in `pool_test.go`.

- [ ] **Step 2: Add release-drain regression tests**

Add tests to `drain_audit_test.go` that construct a Manager without calling `Start`:

```go
func TestManagerDrainReleaseRemovesOrphanedOrdinaryPoolRuntime(t *testing.T) {
	rt := newMockRuntime()
	rt.listedSandboxes = []runtime.SandboxInfo{{
		RuntimeID: "sandbox-pool-orphan",
		Labels: map[string]string{"sandbox.managed": "true", "sandbox.pool": "true"},
	}}
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	require.NoError(t, mgr.DrainRelease(context.Background()))
	assert.True(t, rt.wasRemoved("sandbox-pool-orphan"))
	assert.Equal(t, map[string]string{"sandbox.managed": "true", "sandbox.pool": "true"}, rt.listLabels)
}

func TestManagerDrainReleaseDoesNotTreatFUSEPoolRuntimeAsOrdinary(t *testing.T) {
	rt := newMockRuntime()
	rt.listedSandboxes = []runtime.SandboxInfo{{
		RuntimeID: "sandbox-pool-fuse",
		Labels: map[string]string{
			"sandbox.managed": "true", "sandbox.pool": "true",
			"sandbox.workspace.mode": string(WorkspaceMountFUSE),
		},
	}}
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	require.NoError(t, mgr.DrainRelease(context.Background()))
	assert.False(t, rt.wasRemoved("sandbox-pool-fuse"))
}

func TestManagerDrainReleaseReportsOrdinaryPoolDiscoveryFailure(t *testing.T) {
	rt := newMockRuntime()
	rt.listSandboxesErr = errors.New("list failed")
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	err := mgr.DrainRelease(context.Background())
	require.ErrorContains(t, err, "list ordinary pool runtimes during release drain")
}

func TestManagerDrainReleaseReportsOrdinaryPoolRemovalFailure(t *testing.T) {
	rt := newMockRuntime()
	rt.listedSandboxes = []runtime.SandboxInfo{{
		RuntimeID: "sandbox-pool-stuck",
		Labels: map[string]string{"sandbox.managed": "true", "sandbox.pool": "true"},
	}}
	rt.failRemove("sandbox-pool-stuck", errors.New("delete failed"))
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	err := mgr.DrainRelease(context.Background())
	require.ErrorContains(t, err, "remove ordinary pool runtime sandbox-pool-stuck during release drain")
}
```

Add imports for `errors` and `github.com/goairix/sandbox/internal/runtime` to `drain_audit_test.go`.

- [ ] **Step 3: Run the focused tests and verify RED**

Run:

```bash
go test ./internal/sandbox -run 'TestManagerDrainRelease(RemovesOrphanedOrdinaryPoolRuntime|DoesNotTreatFUSEPoolRuntimeAsOrdinary|ReportsOrdinaryPool)' -count=1
```

Expected: FAIL because `DrainRelease` does not list or remove orphaned ordinary Pool runtimes and does not propagate discovery/removal failures.

### Task 2: Add fail-closed ordinary Pool cleanup to release drain

**Files:**
- Modify: `internal/sandbox/manager.go`
- Test: `internal/sandbox/drain_audit_test.go`

- [ ] **Step 1: Implement the drain-only helper**

Add this helper near `cleanupOrphanedPoolContainers`:

```go
func (m *Manager) drainOrphanedOrdinaryPoolContainers(ctx context.Context) error {
	containers, err := m.runtime.ListSandboxes(ctx, map[string]string{"sandbox.managed": "true", "sandbox.pool": "true"})
	if err != nil {
		return fmt.Errorf("list ordinary pool runtimes during release drain: %w", err)
	}
	var drainErr error
	for _, container := range containers {
		if container.Labels["sandbox.workspace.mode"] == string(WorkspaceMountFUSE) {
			continue
		}
		if container.Labels["sandbox.managed"] != "true" || container.Labels["sandbox.pool"] != "true" {
			drainErr = errors.Join(drainErr, fmt.Errorf("ordinary pool runtime %s has invalid pool identity", container.RuntimeID))
			continue
		}
		if err := m.runtime.RemoveSandbox(ctx, container.RuntimeID); err != nil {
			drainErr = errors.Join(drainErr, fmt.Errorf("remove ordinary pool runtime %s during release drain: %w", container.RuntimeID, err))
		}
	}
	return drainErr
}
```

- [ ] **Step 2: Invoke it from DrainRelease**

Immediately after `m.pool.Drain(ctx)`, join the helper result into `drainErr`:

```go
	m.pool.Drain(ctx)
	drainErr = errors.Join(drainErr, m.drainOrphanedOrdinaryPoolContainers(ctx))
```

Keep Redis audit after this call.

- [ ] **Step 3: Run the focused tests and verify GREEN**

Run:

```bash
go test ./internal/sandbox -run 'TestManagerDrainRelease(RemovesOrphanedOrdinaryPoolRuntime|DoesNotTreatFUSEPoolRuntimeAsOrdinary|ReportsOrdinaryPool)' -count=1
```

Expected: PASS.

- [ ] **Step 4: Run sandbox and Kubernetes runtime tests**

Run:

```bash
go test ./internal/sandbox ./internal/runtime/kubernetes -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the implementation**

```bash
git add internal/sandbox/manager.go internal/sandbox/pool_test.go internal/sandbox/drain_audit_test.go
git commit -m "fix: drain orphaned kubernetes pool runtimes"
```

### Task 3: Verify the complete fix

**Files:**
- Verify only

- [ ] **Step 1: Run race-enabled affected-package tests**

Run:

```bash
go test -race ./internal/sandbox ./internal/runtime/kubernetes -count=1 -timeout=300s
```

Expected: PASS.

- [ ] **Step 2: Run the full repository test suite**

Run:

```bash
go test ./... -count=1 -timeout=600s
```

Expected: PASS.

- [ ] **Step 3: Check patch hygiene and workspace state**

Run:

```bash
git diff --check HEAD~1..HEAD
git status --short
```

Expected: no whitespace errors; only unrelated pre-existing workspace changes, if any, remain.

### Task 4: Address review findings for FUSE identity and refill races

**Files:**
- Modify: `internal/sandbox/pool.go`
- Modify: `internal/sandbox/pool_test.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/drain_audit_test.go`

- [ ] **Step 1: Add failing tests for backend identity boundaries**

Cover a `sandbox.role=fuse-runtime` object without `sandbox.workspace.mode`, and verify a Docker
Manager does not run the Kubernetes orphan scan. Run the focused Manager tests and require both to
fail against the first implementation.

- [ ] **Step 2: Limit scanning and recognize both FUSE identities**

Guard the helper call with `m.config.RuntimeType == "kubernetes"`. Skip a listed object when either
`sandbox.workspace.mode=fuse` or `sandbox.role=fuse-runtime`. Run the focused Manager tests and
require PASS.

- [ ] **Step 3: Add a failing in-flight refill race test**

Block `CreateSandbox` inside a scheduled refill, start `Pool.Drain`, and assert drain cannot return
until the refill exits and its successful result is removed. Run the focused Pool test and require
it to fail because the old drain only snapshots `available`.

- [ ] **Step 4: Add the refill stop barrier**

Give Pool a pool-owned cancellable context, stopping flag, and refill wait group. Route every async
refill through one scheduler that rejects work after stopping; have Drain cancel, wait, then snapshot
and remove the final inventory. If creation succeeds after cancellation, append that result to the
final drain inventory. Run the focused Pool and Manager tests and require PASS.

- [ ] **Step 5: Verify all affected implementations**

Run:

```bash
go test ./internal/sandbox ./internal/runtime/kubernetes ./internal/runtime/docker -count=1
```

Expected: PASS.
