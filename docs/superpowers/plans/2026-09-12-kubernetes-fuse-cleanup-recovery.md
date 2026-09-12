# Kubernetes FUSE Cleanup Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Kubernetes FUSE runtime cleanup recover safely across sandbox-api crashes without treating a missing Pod as proof of termination.

**Architecture:** Add a monotonic termination checkpoint to each Redis cleanup tombstone and a two-stage exact-runtime cleanup interface. Kubernetes keeps the exact Pod object through a managed finalizer until kubelet proves every container exited, the Pool persists that evidence with CAS, and only then removes the finalizer and policies. Helm drains once whenever the cleanup protocol changes so incompatible controllers never co-manage the Pool.

**Tech Stack:** Go, client-go/Kubernetes 1.33 APIs, Redis Lua CAS scripts, Helm templates, OpenTelemetry metrics, testify.

---

## File map

- `internal/storage/state/pool.go`: persistent cleanup phase/evidence domain types and repository CAS contract.
- `internal/storage/state/redis/pool.go`: Lua validation and atomic termination-checkpoint mutation.
- `internal/storage/state/redis/pool_test.go`: real Redis state-machine coverage.
- `internal/runtime/runtime.go`: optional two-stage prepared-runtime cleanup interface.
- `internal/runtime/kubernetes/pod.go`: finalizer on newly prepared FUSE Pods.
- `internal/runtime/kubernetes/runtime.go`: finalizer adoption, kubelet termination proof, and finalization.
- `internal/runtime/kubernetes/runtime_test.go`: exact-UID and crash-boundary runtime tests.
- `internal/sandbox/fuse_pool.go`: shared two-stage cleanup orchestration and updated-record propagation.
- `internal/sandbox/fuse_pool_test.go`: memory repository and Pool recovery/concurrency tests.
- `internal/sandbox/manager.go`: consume persisted evidence during owner/lifecycle cleanup.
- `internal/sandbox/manager_test.go`: restart-safe Manager teardown tests.
- `internal/telemetry/metrics/metrics.go`: cleanup-stage counters and duration.
- `internal/telemetry/metrics/metrics_test.go`: metric initialization/reset coverage.
- `cmd/sandbox/main.go`, `cmd/sandbox/drain.go`, `cmd/sandbox/drain_test.go`: cleanup-protocol comparison in upgrade mode.
- `deploy/helm/sandbox/templates/deployment.yaml`: advertise cleanup protocol v2.
- `deploy/helm/sandbox/templates/pre-backend-change-drain.yaml`: pass desired cleanup protocol to the hook.
- `scripts/test-helm-backend-switch.sh`: render and cluster-upgrade assertions.
- `docs/deployment/workspace-fuse.md`, `docs/deployment/helm-deployment-upgrade.md`: operator-facing recovery and upgrade contract.

### Task 1: Persistent cleanup phase and evidence

**Files:**
- Modify: `internal/storage/state/pool.go`
- Modify: `internal/sandbox/fuse_pool_test.go`

- [x] **Step 1: Write failing domain/repository fake tests**

Extend the memory repository tests so a cleanup record starts in `terminating`, rejects evidence for a different UID, advances once to `terminated`, preserves evidence during same-token retry/takeover, and increments revision. Use these exact domain shapes:

```go
evidence := state.FUSEPoolTerminationEvidence{
	RuntimeUID:      claimed.RuntimeUID,
	GracefulUnmount: true,
	ProcessExited:   true,
}
terminated, err := repo.ConfirmCleanupTermination(
	context.Background(), claimed.PreparationID, claimed.CleanupToken,
	claimed.Revision, claimed.RuntimeID, claimed.RuntimeUID, evidence,
)
require.NoError(t, err)
require.Equal(t, state.FUSEPoolCleanupTerminated, terminated.CleanupPhase)
require.NotNil(t, terminated.TerminationEvidence)
require.Equal(t, evidence, *terminated.TerminationEvidence)
require.Equal(t, claimed.Revision+1, terminated.Revision)
```

- [x] **Step 2: Run the focused test and verify it fails**

Run: `go test ./internal/sandbox -run 'TestMemoryFUSEPoolRepository.*CleanupTermination' -count=1`

Expected: compile failure because the cleanup phase, evidence, and repository method do not exist.

- [x] **Step 3: Add the domain types and repository method**

Add to `internal/storage/state/pool.go`:

```go
type FUSEPoolCleanupPhase string

const (
	FUSEPoolCleanupTerminating FUSEPoolCleanupPhase = "terminating"
	FUSEPoolCleanupTerminated  FUSEPoolCleanupPhase = "terminated"
)

type FUSEPoolTerminationEvidence struct {
	RuntimeUID           string `json:"runtime_uid"`
	NodeName             string `json:"node_name,omitempty"`
	GracefulUnmount      bool   `json:"graceful_unmount,omitempty"`
	ProcessExited        bool   `json:"process_exited,omitempty"`
	InfrastructureFenced bool   `json:"infrastructure_fenced,omitempty"`
}
```

Add `CleanupPhase FUSEPoolCleanupPhase` and
`TerminationEvidence *FUSEPoolTerminationEvidence` (with `json:",omitempty"`) to
`FUSEPoolRecord`, and add this repository operation:

```go
ConfirmCleanupTermination(
	ctx context.Context,
	preparationID, cleanupToken string,
	expectedRevision uint64,
	runtimeID, runtimeUID string,
	evidence FUSEPoolTerminationEvidence,
) (*FUSEPoolRecord, error)
```

The memory fake must enforce cleanup state/token/revision/exact identity, accept only evidence where `RuntimeUID == runtimeUID` and `ProcessExited || (InfrastructureFenced && NodeName != "")`, preserve a previous `terminated` checkpoint, and never allow a phase rollback.

- [x] **Step 4: Run the focused tests**

Run: `go test ./internal/sandbox -run 'TestMemoryFUSEPoolRepository.*CleanupTermination' -count=1`

Expected: PASS.

- [x] **Step 5: Commit**

```bash
git add internal/storage/state/pool.go internal/sandbox/fuse_pool_test.go
git commit -m "feat: model durable fuse cleanup evidence"
```

### Task 2: Redis CAS termination checkpoint

**Files:**
- Modify: `internal/storage/state/redis/pool.go`
- Modify: `internal/storage/state/redis/pool_test.go`

- [x] **Step 1: Write failing Redis state-machine tests**

Add tests for successful transition, idempotent replay, stale revision, wrong token, wrong runtime UID, invalid evidence, lease takeover preservation, and `DeleteCleanup` using the returned revision. Assert that pool counts, state membership, deadline, RuntimeUID owner, and membership generation do not drift.

```go
terminated, err := repo.ConfirmCleanupTermination(ctx, claimed.PreparationID,
	claimed.CleanupToken, claimed.Revision, claimed.RuntimeID, claimed.RuntimeUID,
	state.FUSEPoolTerminationEvidence{RuntimeUID: claimed.RuntimeUID, ProcessExited: true})
require.NoError(t, err)
require.Equal(t, state.FUSEPoolCleanupTerminated, terminated.CleanupPhase)
require.Equal(t, claimed.Revision+1, terminated.Revision)
deleted, err := repo.DeleteCleanup(ctx, terminated.PreparationID,
	terminated.CleanupToken, terminated.Revision)
require.NoError(t, err)
require.True(t, deleted)
```

- [x] **Step 2: Run the focused Redis tests and verify they fail**

Run: `go test ./internal/storage/state/redis -run 'TestFUSEPool.*CleanupTermination' -count=1`

Expected: compile failure or missing-method failure.

- [x] **Step 3: Extend Lua record validation and ClaimCleanup**

Teach `validateRecord` that non-cleanup records cannot contain cleanup phase/evidence. For cleanup records, normalize a missing phase as legacy `terminating`; require `terminated` to contain matching, valid evidence. Update `claimCleanupScript` so a new cleanup claim writes `cleanup_phase='terminating'`, while same-token retry and expired-token takeover preserve an existing phase/evidence.

- [x] **Step 4: Add the atomic CAS script and Go wrapper**

Create `confirmCleanupTerminationScript` using the existing Lua helpers. It must validate the record, pool indexes, token, expected revision, exact runtime ID/UID and evidence; write only `cleanup_phase='terminated'` plus `termination_evidence`; increment revision; refresh `updated_at`; and return the encoded record. Implement the repository method with bounded UTF-8 validation before Lua.

- [x] **Step 5: Run Redis tests**

Run: `go test ./internal/storage/state/redis -run 'TestFUSEPool.*(Cleanup|Termination)' -count=1`

Expected: PASS.

- [x] **Step 6: Commit**

```bash
git add internal/storage/state/redis/pool.go internal/storage/state/redis/pool_test.go
git commit -m "feat: persist fuse runtime termination evidence"
```

### Task 3: Kubernetes finalizer and two-stage runtime cleanup

**Files:**
- Modify: `internal/runtime/runtime.go`
- Modify: `internal/runtime/kubernetes/pod.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Modify: `internal/runtime/kubernetes/runtime_test.go`

- [x] **Step 1: Write failing Pod construction and termination tests**

Add tests proving prepared FUSE Pods include only the managed finalizer, ordinary Pods do not, a legacy live Pod adopts the finalizer before deletion, and a deleting legacy Pod without it fails closed. Add status fixtures covering the native sidecar in `InitContainerStatuses`, sandbox in `ContainerStatuses`, and ephemeral containers; every declared container must have a terminated state.

```go
require.Contains(t, created.Finalizers, fuseRuntimeCleanupFinalizer)
evidence, err := rt.ConfirmPreparedSandboxTermination(ctx, info.RuntimeID, info.RuntimeUID)
require.NoError(t, err)
require.True(t, evidence.ProcessExited)
current, err := client.CoreV1().Pods(namespace).Get(ctx, info.RuntimeID, metav1.GetOptions{})
require.NoError(t, err)
require.NotNil(t, current.DeletionTimestamp)
```

- [x] **Step 2: Run focused Kubernetes tests and verify failure**

Run: `go test ./internal/runtime/kubernetes -run 'Test.*(Finalizer|PreparedSandboxTermination|FinalizePrepared)' -count=1`

Expected: compile failure because the interface/methods/constants do not exist.

- [x] **Step 3: Add the optional two-stage interface**

Add to `internal/runtime/runtime.go`:

```go
type PreparedSandboxCleanup interface {
	ConfirmPreparedSandboxTermination(ctx context.Context, runtimeID, runtimeUID string) (TerminationEvidence, error)
	FinalizePreparedSandboxRemoval(ctx context.Context, runtimeID, runtimeUID string, evidence TerminationEvidence) error
}
```

- [x] **Step 4: Put the finalizer on new FUSE Pods**

Define `fuseRuntimeCleanupFinalizer = "goairix.github.io/sandbox-fuse-runtime-cleanup"` and add it in `buildPreparedFUSEPod`. Include finalizers in prepared Pod intent equality so admission mutation cannot silently remove or add a cleanup owner.

- [x] **Step 5: Implement finalizer adoption and complete-status validation**

Use GET/update conflict retry with exact UID and resourceVersion checks. Never add the finalizer after `DeletionTimestamp` is set. Add a pure helper that maps declared init, regular, and ephemeral container names to statuses and returns true only when every declared container has `State.Terminated != nil` and no unknown status entry creates ambiguity.

- [x] **Step 6: Implement confirm and finalize methods**

`ConfirmPreparedSandboxTermination` must serialize through `beginWorkspaceRemoval`, reconstruct proof from a terminal finalizer-held Pod when possible, otherwise validate shutdown ack and delete with UID precondition, then wait for terminal status without waiting for NotFound. On API NotFound without prior durable evidence, return `ErrTerminationUnconfirmed`. `FinalizePreparedSandboxRemoval` must validate the evidence UID and exit/fence predicate, remove only the managed finalizer from the exact Pod, wait for the exact UID to disappear, then delete exact-UID policies. A same-name replacement is never patched or deleted.

- [x] **Step 7: Retain `RemovePreparedSandbox` as the orphan-safe wrapper**

Implement it as confirm followed immediately by finalize for callers that have no Redis owner. Keep `ConfirmTerminated` compatible with the in-process wrapper, but do not use it as the Pool's persistent recovery source.

- [x] **Step 8: Run all Kubernetes runtime tests**

Run: `go test ./internal/runtime/kubernetes -count=1`

Expected: PASS, including the existing NotFound-without-proof fail-closed test.

- [x] **Step 9: Commit**

```bash
git add internal/runtime/runtime.go internal/runtime/kubernetes/pod.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
git commit -m "feat: make kubernetes fuse termination resumable"
```

### Task 4: Route every Pool cleanup through the durable checkpoint

**Files:**
- Modify: `internal/sandbox/fuse_pool.go`
- Modify: `internal/sandbox/fuse_pool_test.go`

- [x] **Step 1: Write failing Pool recovery tests**

Use a two-stage fake runtime that can fail after confirm, after repository checkpoint, during finalize, and before delete. Verify retries skip confirm after `terminated`, use the new revision, and reject stale token/revision. Verify a legacy fake implementing only `PreparedSandboxRemover` retains old behavior.

```go
updated, err := pool.RemoveClaimedRuntime(ctx, *claimed)
require.Error(t, err) // injected finalize failure
require.Equal(t, state.FUSEPoolCleanupTerminated, updated.CleanupPhase)

updated, err = pool.RemoveClaimedRuntime(ctx, *updated)
require.NoError(t, err)
require.Equal(t, 1, runtime.confirmCalls)
require.Equal(t, 2, runtime.finalizeCalls)
require.NoError(t, pool.CompleteClaimedCleanup(ctx, *updated))
```

- [x] **Step 2: Run focused Pool tests and verify failure**

Run: `go test ./internal/sandbox -run 'TestFUSEPool.*(DurableCleanup|TerminationCheckpoint|CleanupTakeover)' -count=1`

Expected: compile/signature failure.

- [x] **Step 3: Add conversion and the shared helper**

Add exact conversions between `runtime.TerminationEvidence` and `state.FUSEPoolTerminationEvidence`. Change `RemoveClaimedRuntime` to return `(*state.FUSEPoolRecord, error)`. If runtime implements `PreparedSandboxCleanup`, confirm when needed, CAS the evidence, finalize with the persisted evidence, and return the newest record even when finalize fails. For legacy runtimes, call `RemovePreparedSandbox` and return an unchanged copy.

- [x] **Step 4: Replace all direct remover calls**

Update `ReleaseConsumed`, `destroyClaimedCleanup`, publication compensation, `claimAndDestroyWithRuntimeEvidence`, normal reconcile, and release drain to use the shared helper and pass the returned record to `DeleteCleanup`. Do not swallow `ErrTerminationUnconfirmed` or treat `ErrNotFound` as success on the two-stage path.

- [x] **Step 5: Run Pool tests**

Run: `go test ./internal/sandbox -run 'TestFUSEPool' -count=1`

Expected: PASS.

- [x] **Step 6: Commit**

```bash
git add internal/sandbox/fuse_pool.go internal/sandbox/fuse_pool_test.go
git commit -m "feat: checkpoint fuse cleanup before finalization"
```

### Task 5: Recover Manager teardown from persisted evidence

**Files:**
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/manager_test.go`

- [x] **Step 1: Write failing Manager restart tests**

Cover failed-create cleanup and normal FUSE teardown where the cleanup record is already `terminated` but the new runtime has no in-memory proof. Assert owner release receives the record evidence, the runtime confirm phase is not repeated, lifecycle transitions complete, and the cleanup record is deleted with its current revision.

- [x] **Step 2: Run focused Manager tests and verify failure**

Run: `go test ./internal/sandbox -run 'TestManager.*PersistedTerminationEvidence' -count=1`

Expected: failure because Manager still calls process-local `RuntimeFencer` and retains the old record.

- [x] **Step 3: Propagate updated cleanup records**

At both `RemoveClaimedRuntime` call sites, replace the saved claimed record with the returned record and persist/copy it into the in-flight lifecycle. Populate `lifecycle.evidence` from a valid terminated record before falling back to `RuntimeFencer`. Use the same conversion helper as the Pool so UID and exit/fence validation cannot diverge.

- [x] **Step 4: Run Manager and workspace-owner tests**

Run: `go test ./internal/sandbox -run 'TestManager|TestWorkspaceCoordinator' -count=1`

Expected: PASS.

- [x] **Step 5: Commit**

```bash
git add internal/sandbox/manager.go internal/sandbox/manager_test.go
git commit -m "fix: resume manager teardown from cleanup evidence"
```

### Task 6: Cleanup-stage observability

**Files:**
- Modify: `internal/telemetry/metrics/metrics.go`
- Modify: `internal/telemetry/metrics/metrics_test.go`
- Modify: `internal/sandbox/fuse_pool.go`

- [x] **Step 1: Write failing metric initialization test**

Assert non-nil instruments for `sandbox.workspace.pool.cleanup.total` and `sandbox.workspace.pool.cleanup.duration`, plus reset coverage.

- [x] **Step 2: Run metric tests and verify failure**

Run: `go test ./internal/telemetry/metrics -count=1`

Expected: compile failure for the new instruments.

- [x] **Step 3: Add metrics and structured stage logging**

Register a counter and histogram. Record runtime/provider, stage and result at confirm, checkpoint, finalizer/policy finalize, and tombstone delete boundaries. Extend reconcile failures with `preparation_id`, `runtime_id`, `runtime_uid`, `cleanup_phase`, and `revision`; do not log evidence payloads or credentials.

- [x] **Step 4: Run metric and Pool tests**

Run: `go test ./internal/telemetry/metrics ./internal/sandbox -count=1`

Expected: PASS.

- [x] **Step 5: Commit**

```bash
git add internal/telemetry/metrics/metrics.go internal/telemetry/metrics/metrics_test.go internal/sandbox/fuse_pool.go
git commit -m "feat: expose fuse cleanup recovery stages"
```

### Task 7: Helm cleanup protocol v2 drain gate

**Files:**
- Modify: `cmd/sandbox/main.go`
- Modify: `cmd/sandbox/drain.go`
- Modify: `cmd/sandbox/drain_test.go`
- Modify: `deploy/helm/sandbox/templates/deployment.yaml`
- Modify: `deploy/helm/sandbox/templates/pre-backend-change-drain.yaml`
- Modify: `scripts/test-helm-backend-switch.sh`

- [x] **Step 1: Write failing command and render tests**

Add helper tests for installed cleanup protocol equality. Extend the Helm script to require:

```bash
grep -Fq 'goairix.github.io/sandbox-cleanup-protocol: "v2"' <<<"$rendered"
grep -Fq -- '--kubernetes-cleanup-protocol=v2' <<<"$rendered"
```

Add decision-table unit cases: both fingerprint/protocol equal skips; same backend/different protocol with
installed drain protocol drains; changed backend/matching installed drain protocol drains; any required
drain with missing installed drain protocol fails before scaling.

- [x] **Step 2: Run tests and verify failure**

Run: `go test ./cmd/sandbox -count=1 && ./scripts/test-helm-backend-switch.sh`

Expected: missing cleanup-protocol assertions fail.

- [x] **Step 3: Add the cleanup protocol flag and decision helper**

Add `cleanupProtocolAnnotation = "goairix.github.io/sandbox-cleanup-protocol"`, a
`--kubernetes-cleanup-protocol` desired-value flag, and a pure decision helper. Skip only if backend and
cleanup protocol both match. Require the installed `drain-protocol=v1` compatibility guard before every
release drain, including a same-backend cleanup protocol migration, so rollback can resume replicas.

- [x] **Step 4: Render protocol v2 in Helm**

Annotate the Deployment Pod template with cleanup protocol v2 and pass the desired value to the pre-upgrade hook. Keep the existing post-upgrade resume hook so a successful protocol drain restores replicas and a failed drain leaves them at zero.

- [x] **Step 5: Run command and Helm tests**

Run: `go test ./cmd/sandbox -count=1 && ./scripts/test-helm-chart.sh && ./scripts/test-helm-backend-switch.sh`

Expected: PASS.

- [x] **Step 6: Commit**

```bash
git add cmd/sandbox/main.go cmd/sandbox/drain.go cmd/sandbox/drain_test.go deploy/helm/sandbox/templates/deployment.yaml deploy/helm/sandbox/templates/pre-backend-change-drain.yaml scripts/test-helm-backend-switch.sh
git commit -m "feat: drain helm upgrades on cleanup protocol changes"
```

### Task 8: Documentation and full verification

**Files:**
- Modify: `docs/deployment/workspace-fuse.md`
- Modify: `docs/deployment/helm-deployment-upgrade.md`

- [x] **Step 1: Update operator documentation**

Document cleanup phases, expected temporary `Terminating` Pods, protocol-v2 one-time drain, and the rule that Pod NotFound without evidence requires fencer/audit. State explicitly that operators must not remove the finalizer and Redis tombstone as an unverified pair.

- [x] **Step 2: Run formatting and focused verification**

Run:

```bash
gofmt -w internal/storage/state/pool.go internal/storage/state/redis/pool.go internal/storage/state/redis/pool_test.go internal/runtime/runtime.go internal/runtime/kubernetes/pod.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go internal/sandbox/fuse_pool.go internal/sandbox/fuse_pool_test.go internal/sandbox/manager.go internal/sandbox/manager_test.go internal/telemetry/metrics/metrics.go internal/telemetry/metrics/metrics_test.go cmd/sandbox/main.go cmd/sandbox/drain.go cmd/sandbox/drain_test.go
go test ./internal/storage/state/redis ./internal/runtime/kubernetes ./internal/sandbox ./internal/telemetry/metrics ./cmd/sandbox -count=1
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
```

Expected: all commands PASS.

- [x] **Step 3: Run race and full repository tests**

Run:

```bash
go test -race ./internal/runtime/kubernetes ./internal/sandbox ./internal/storage/state/redis -count=1
go test ./... -count=1
```

Expected: PASS with no race reports.

- [x] **Step 4: Inspect the final diff and cluster-facing artifacts**

Run:

```bash
git diff --check
git status --short
helm template sandbox ./deploy/helm/sandbox --namespace aiadp-sandbox-fuse | rg 'cleanup-protocol|kubernetes-cleanup-protocol|image:'
```

Expected: no whitespace errors; only intended files changed; rendered Deployment and pre-upgrade Job advertise protocol v2 and use the configured sandbox-api image.

- [x] **Step 5: Commit**

```bash
git add docs/deployment/workspace-fuse.md docs/deployment/helm-deployment-upgrade.md
git commit -m "docs: explain kubernetes fuse cleanup recovery"
```
