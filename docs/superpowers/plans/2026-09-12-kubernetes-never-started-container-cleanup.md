# Kubernetes Never-Started Container Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow FUSE cleanup to confirm a terminal Pod whose declared container provably never started, while retaining fail-closed behavior for missing, running, restarted, or otherwise ambiguous container status.

**Architecture:** Keep `preparedPodContainersTerminated` as the single termination-evidence predicate. Pass a Pod-level `allowNeverStarted` flag into status validation only when the exact Pod has a deletion timestamp and terminal phase; accept a Waiting status only when every field proves there was no prior execution.

**Tech Stack:** Go 1.25, Kubernetes core/v1 Pod and ContainerStatus types, testify.

---

### Task 1: Model strict never-started evidence

**Files:**
- Modify: `internal/runtime/kubernetes/runtime_test.go`
- Modify: `internal/runtime/kubernetes/runtime.go`

- [ ] **Step 1: Add a failing reproduction test**

Add `TestPreparedPodContainersTerminatedAcceptsProvablyNeverStartedContainer` beside the existing status-coverage test. Build a Pod with:

```go
now := metav1.Now()
pod := &corev1.Pod{
	ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
	Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: workspaceMounterContainer}},
		Containers:     []corev1.Container{{Name: sandboxContainer}},
	},
	Status: corev1.PodStatus{
		Phase: corev1.PodFailed,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: workspaceMounterContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  sandboxContainer,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
		}},
	},
}
require.True(t, preparedPodContainersTerminated(pod))
```

Add table cases that each mutate one proof condition and require false: no deletion timestamp, non-terminal Pod phase, non-empty ContainerID, RestartCount > 0, Started=true, non-empty LastTerminationState, Running state, and missing status.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test ./internal/runtime/kubernetes -run TestPreparedPodContainersTerminatedAcceptsProvablyNeverStartedContainer -count=1
```

Expected: FAIL because the current predicate rejects every non-Terminated status.

- [ ] **Step 3: Implement strict quiescence validation**

In `preparedPodContainersTerminated`, calculate:

```go
allowNeverStarted := pod.DeletionTimestamp != nil &&
	(pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded)
```

Pass this flag through declared container and ephemeral-container validation. A status is accepted when it is Terminated, or when `allowNeverStarted` is true and this helper returns true:

```go
func containerProvablyNeverStarted(status corev1.ContainerStatus) bool {
	return status.State.Waiting != nil &&
		status.ContainerID == "" &&
		status.RestartCount == 0 &&
		(status.Started == nil || !*status.Started) &&
		status.LastTerminationState.Running == nil &&
		status.LastTerminationState.Terminated == nil &&
		status.LastTerminationState.Waiting == nil
}
```

Keep exact status-count, declared-name, and duplicate-name checks unchanged.

- [ ] **Step 4: Run focused and package tests**

Run:

```bash
gofmt -w internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
go test ./internal/runtime/kubernetes -run 'TestPreparedPodContainersTerminated' -count=1
go test ./internal/runtime/kubernetes -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the runtime fix**

```bash
git add internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
git commit -m "fix: recognize never-started pod containers during cleanup"
```

### Task 2: Verify repository and prepare cluster recovery

**Files:**
- Modify: `docs/superpowers/plans/2026-09-12-kubernetes-never-started-container-cleanup.md`

- [ ] **Step 1: Run full repository verification**

Run:

```bash
git diff --check
go vet ./...
go test ./... -count=1
helm lint deploy/helm/sandbox
bash scripts/test-helm-chart.sh
bash scripts/test-helm-backend-switch.sh
```

Expected: all commands exit 0; Helm lint reports `0 chart(s) failed`; both scripts print `PASS`.

- [ ] **Step 2: Confirm only sandbox-api needs rebuilding**

Run:

```bash
git diff --name-only HEAD~1
```

Expected: only Go API runtime/test files changed by the implementation commit; no sandbox runtime, gateway, or mounter source changed.

- [ ] **Step 3: Commit the completed checklist**

```bash
git add docs/superpowers/plans/2026-09-12-kubernetes-never-started-container-cleanup.md
git commit -m "docs: complete never-started container cleanup plan"
```

After a uniquely tagged sandbox-api image is deployed, verify that `sandbox-pool-qqdh0cfziw` loses its finalizer and disappears, the stale FUSE policy and cleanup record are removed, all API replicas become Ready, and the end-to-end API suite can proceed.
