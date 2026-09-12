# Helm Drain Ordinary Pool Orphan Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make release-wide drain remove ordinary warm-pool runtimes that are not present in the drain Job's in-memory Pool before Kubernetes zero-state audit runs.

**Architecture:** Add a drain-only, fail-closed Manager helper that lists `sandbox.managed=true,sandbox.pool=true` runtimes, excludes FUSE runtimes, validates returned ownership labels, and aggregates exact removal errors. Invoke it after the in-memory ordinary Pool has drained so failed or orphaned removals are retried, while the existing FUSE and final audit paths remain unchanged.

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
