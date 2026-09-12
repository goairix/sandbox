# Kubernetes Ordinary Runtime Network Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make ordinary Kubernetes sandbox network policy lifecycle fail-closed across direct creation, pool acquisition, dynamic updates, deletion, restart recovery, and failed file-upload cleanup.

**Architecture:** Introduce one validated ordinary-runtime identity shared by Pod and policy operations. Build and create policies before creating a Pod, bind policies to the returned Pod UID, migrate pool policy identity before relabelling a claimed Pod, resolve later operations from the exact Pod, and reconcile only provably orphaned managed policies at startup. Keep FUSE policy code unchanged.

**Tech Stack:** Go, Kubernetes `core/v1` and `networking/v1`, Cilium `cilium.io/v2` unstructured resources, client-go fake clients/reactors, Testify, `errors.Join`.

---

## File Structure

- Add `internal/runtime/kubernetes/ordinary_network.go`: ordinary identity validation, policy builders, ownership checks, pool migration, exact deletion, and orphan reconciliation.
- Modify `internal/runtime/kubernetes/network.go`: route ordinary policy entry points through validated builders while leaving FUSE helpers unchanged.
- Modify `internal/runtime/kubernetes/pod.go`: split pure ordinary Pod construction from API creation and add exact Pod deletion support.
- Modify `internal/runtime/kubernetes/runtime.go`: policy-before-Pod creation, logical-ID update/remove, and startup reconciliation.
- Modify `internal/runtime/kubernetes/file.go`: return and join partial-upload cleanup failures.
- Modify `internal/runtime/runtime.go`: define an ordinary network-state uncertainty sentinel.
- Modify `internal/sandbox/manager.go`: propagate pool relabel failures and count only successful orphan removals.
- Extend Kubernetes and manager tests in their existing `*_test.go` files.

### Task 1: Establish a Validated Ordinary Pod/Policy Identity

**Files:**
- Create: `internal/runtime/kubernetes/ordinary_network.go`
- Modify: `internal/runtime/kubernetes/pod.go`
- Test: `internal/runtime/kubernetes/network_test.go`
- Test: `internal/runtime/kubernetes/pod_test.go`

- [ ] **Step 1: Add failing identity and builder tests**

Add these tests:

```go
func TestOrdinaryLogicalIDUsesExplicitSandboxLabel(t *testing.T)
func TestOrdinaryLogicalIDFallsBackToRuntimeID(t *testing.T)
func TestOrdinaryLogicalIDRejectsInvalidOrFUSEIdentity(t *testing.T)
func TestBuildOrdinaryNetworkPolicyCarriesExactIdentity(t *testing.T)
func TestBuildOrdinaryCiliumPrivateDenyCarriesExactIdentity(t *testing.T)
func TestBuildOrdinaryPodIsPureAndUsesLogicalID(t *testing.T)
```

Verify that `spec.Labels["sandbox.id"]` wins over `spec.ID`, empty labels fall
back to `spec.ID`, invalid DNS-1123 values fail, and
`sandbox.workspace.mode=fuse` is rejected by ordinary helpers. Assert standard
and Cilium policies carry `sandbox.managed=true`, the logical ID, an ordinary
role, `sandbox.runtime.id=<pod-name>`, and an attempt annotation. Their selectors
must be exactly `sandbox.id=<logical-id>` and the UID annotation must initially
be absent.

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
go test ./internal/runtime/kubernetes -run 'Test(OrdinaryLogicalID|BuildOrdinary(NetworkPolicy|CiliumPrivateDeny|Pod))' -count=1
```

Expected: FAIL because the identity, builders, and pure Pod constructor do not exist.

- [ ] **Step 3: Implement the identity and pure builders**

Add to `ordinary_network.go`:

```go
const (
	ordinaryPolicyRole              = "ordinary"
	ordinaryPrivateDenyPolicyRole   = "ordinary-private-deny"
	ordinaryPolicyRoleLabel         = "sandbox.policy.role"
	ordinaryRuntimeIDLabel          = "sandbox.runtime.id"
	ordinaryRuntimeUIDAnnotation    = "sandbox.runtime.uid"
	ordinaryPolicyAttemptAnnotation = "sandbox.network.attempt"
)

type ordinaryNetworkIdentity struct {
	runtimeID  string
	runtimeUID types.UID
	logicalID  string
}
```

Implement:

```go
func ordinaryLogicalID(spec runtime.SandboxSpec) (string, error)
func ordinaryIdentityFromPod(pod *corev1.Pod, expectedRuntimeID string) (ordinaryNetworkIdentity, error)
func buildOrdinaryNetworkPolicy(namespace string, identity ordinaryNetworkIdentity, attempt string, enabled bool, whitelist []string, blockPrivate bool) (*networkingv1.NetworkPolicy, error)
func buildOrdinaryCiliumPrivateDeny(namespace string, identity ordinaryNetworkIdentity, attempt string) (*unstructured.Unstructured, error)
func buildOrdinaryPod(namespace string, spec runtime.SandboxSpec) (*corev1.Pod, error)
```

Use `kvalidation.IsDNS1123Subdomain` for runtime and logical IDs. Require a live
Pod to be managed and non-FUSE. Move the existing non-FUSE construction body
from `createPod` into `buildOrdinaryPod`; keep `createPod` as a wrapper that
builds then calls `Pods.Create`. Extract existing ordinary egress and Cilium
deny rule construction without changing network semantics. Builders perform no
API writes.

- [ ] **Step 4: Run focused tests and verify GREEN**

```bash
gofmt -w internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/network_test.go internal/runtime/kubernetes/pod.go internal/runtime/kubernetes/pod_test.go
go test ./internal/runtime/kubernetes -run 'Test(OrdinaryLogicalID|BuildOrdinary(NetworkPolicy|CiliumPrivateDeny|Pod))' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git diff --check
git add internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/network_test.go internal/runtime/kubernetes/pod.go internal/runtime/kubernetes/pod_test.go
git commit -m "refactor: define kubernetes ordinary network identity"
```

### Task 2: Create Policies Before the Ordinary Pod

**Files:**
- Modify: `internal/runtime/kubernetes/ordinary_network.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Test: `internal/runtime/kubernetes/runtime_test.go`

- [ ] **Step 1: Add failing lifecycle-order and collision tests**

Add:

```go
func TestCreateSandboxCreatesStandardPolicyBeforePod(t *testing.T)
func TestCreateSandboxCreatesCiliumDenyBeforePod(t *testing.T)
func TestCreateSandboxDoesNotAdoptExistingLogicalPolicy(t *testing.T)
func TestCreateSandboxRejectsExistingManagedPodWithLogicalID(t *testing.T)
func TestCreateSandboxCleansOnlyAttemptOwnedResources(t *testing.T)
func TestCreateSandboxJoinsPrimaryAndCleanupErrors(t *testing.T)
```

Use fake-client actions to prove all required policies precede Pod creation.
Seed a same-name policy with a foreign attempt and prove it is neither updated
nor deleted. Inject a Pod-create failure plus policy-delete failure and assert
both causes remain visible.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/runtime/kubernetes -run 'TestCreateSandbox(Creates|DoesNotAdopt|RejectsExisting|CleansOnly|Joins)' -count=1
```

Expected: FAIL because current code creates and readies the Pod first and uses
best-effort cleanup.

- [ ] **Step 3: Implement create-only preparation and UID binding**

Implement:

```go
func ensureOrdinaryIdentityAvailable(ctx context.Context, client kubernetes.Interface, namespace string, identity ordinaryNetworkIdentity) error
func createOrdinaryNetworkPolicy(ctx context.Context, client kubernetes.Interface, policy *networkingv1.NetworkPolicy) (*networkingv1.NetworkPolicy, error)
func createOrdinaryCiliumPrivateDeny(ctx context.Context, client dynamic.Interface, policy *unstructured.Unstructured) (*unstructured.Unstructured, error)
func bindOrdinaryNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, name, attempt string, podUID types.UID) error
func bindOrdinaryCiliumPrivateDeny(ctx context.Context, client dynamic.Interface, namespace, name, attempt string, podUID types.UID) error
func deleteAttemptOrdinaryNetworkPolicy(..., name, attempt string) error
func deleteAttemptOrdinaryCiliumPrivateDeny(..., name, attempt string) error
```

`ensureOrdinaryIdentityAvailable` must GET the runtime Pod and LIST
`sandbox.managed=true,sandbox.id=<logicalID>`. Create helpers use Create-only
semantics. Recover from an uncertain Create response only after exact attempt
and shape verification; never adopt another object. Bind helpers verify attempt,
shape, resourceVersion, set `sandbox.runtime.uid`, and verify uncertain writes.

Rewrite `Runtime.CreateSandbox` in this exact order:

1. Purely build Pod and policies and validate identity.
2. Verify runtime/logical identity is unused.
3. Create standard policy.
4. Create optional Cilium deny.
5. Revalidate identity is unused.
6. Create Pod.
7. Bind policy objects to the returned Pod UID.
8. Wait for Ready.

On failure, use a bounded context derived from `context.Background()`. Delete an
attempt-created Pod first, then only attempt-owned policies. Return
`errors.Join(primaryErr, cleanupErrs...)`.

- [ ] **Step 4: Run focused and package tests**

```bash
gofmt -w internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
go test ./internal/runtime/kubernetes -run 'TestCreateSandbox(Creates|DoesNotAdopt|RejectsExisting|CleansOnly|Joins)' -count=1
go test ./internal/runtime/kubernetes -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git diff --check
git add internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
git commit -m "fix: isolate kubernetes pod before creation"
```

### Task 3: Migrate Network Identity During Ordinary Pool Acquisition

**Files:**
- Modify: `internal/runtime/kubernetes/ordinary_network.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Modify: `internal/sandbox/manager.go`
- Test: `internal/runtime/kubernetes/runtime_test.go`
- Test: `internal/sandbox/manager_test.go`
- Test: `internal/sandbox/pool_test.go`

- [ ] **Step 1: Add failing migration and compensation tests**

Add:

```go
func TestUpdateLabelsMigratesPoolPolicyBeforePodIdentity(t *testing.T)
func TestUpdateLabelsRejectsLogicalIDAlreadyInUse(t *testing.T)
func TestUpdateLabelsMigrationPatchFailureRemovesOnlyNewAttemptPolicy(t *testing.T)
func TestUpdateLabelsReturnsOldPolicyDeleteFailure(t *testing.T)
func TestManagerPoolAcquireRelabelFailureRemovesRuntimeAndNotifiesPool(t *testing.T)
func TestManagerPoolAcquireRelabelFailureJoinsRemovalFailure(t *testing.T)
```

Assert successful action order: new policy Create, Pod Patch, old policy Delete.
The new policy must preserve old `Spec.Egress` and `PolicyTypes`. On patch
failure, old policy and old Pod label remain while only the attempt-owned new
policy is removed. Extend `mockRuntime` with an injectable UpdateLabels error
and captured calls.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/runtime/kubernetes -run 'TestUpdateLabels(Migrates|RejectsLogical|MigrationPatch|ReturnsOld)' -count=1
go test ./internal/sandbox -run 'TestManagerPoolAcquireRelabelFailure' -count=1
```

Expected: FAIL because generic label patching does not migrate policies and
Manager discards the error.

- [ ] **Step 3: Implement exact pool migration**

Add:

```go
func (r *Runtime) migrateOrdinaryPoolIdentity(ctx context.Context, pod *corev1.Pod, newLogicalID string) error
func validateOrdinaryNetworkPolicy(policy *networkingv1.NetworkPolicy, identity ordinaryNetworkIdentity, allowLegacyRole bool) error
```

Allow migration only for a managed, non-FUSE Pod still carrying
`sandbox.pool=true` when one patch both removes that label and changes
`sandbox.id`. GET the old policy and validate its name, managed label, selector,
and old identity. Clone its egress semantics into an identity-correct new
policy with a fresh attempt. Create it before applying the UID/resourceVersion
Pod patch. Delete the old policy only after patch success. If patch fails,
delete only the new policy whose UID and attempt match the create result.

In `Manager.Create`, replace the ignored UpdateLabels call with checked error
handling. On error, remove the acquired runtime under a bounded background
context, call `m.pool.NotifyRemoved()`, and return
`errors.Join(fmt.Errorf("claim pooled sandbox: %w", err), removeErr)`.

- [ ] **Step 4: Run focused and package tests**

```bash
gofmt -w internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go internal/sandbox/manager.go internal/sandbox/manager_test.go internal/sandbox/pool_test.go
go test ./internal/runtime/kubernetes -run 'TestUpdateLabels(Migrates|RejectsLogical|MigrationPatch|ReturnsOld)' -count=1
go test ./internal/sandbox -run 'TestManagerPoolAcquireRelabelFailure' -count=1
go test ./internal/runtime/kubernetes ./internal/sandbox -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git diff --check
git add internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go internal/sandbox/manager.go internal/sandbox/manager_test.go internal/sandbox/pool_test.go
git commit -m "fix: migrate kubernetes pool network identity"
```

### Task 4: Resolve Dynamic Network Updates From the Exact Pod

**Files:**
- Modify: `internal/runtime/runtime.go`
- Modify: `internal/runtime/kubernetes/ordinary_network.go`
- Modify: `internal/runtime/kubernetes/network.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Test: `internal/runtime/kubernetes/network_test.go`
- Test: `internal/runtime/kubernetes/runtime_test.go`

- [ ] **Step 1: Add failing update tests**

Add:

```go
func TestUpdateNetworkUsesCurrentPodLogicalID(t *testing.T)
func TestUpdateNetworkRejectsForeignSameNamePolicy(t *testing.T)
func TestUpdateNetworkUpgradesExactLegacyOrdinaryPolicy(t *testing.T)
func TestUpdateNetworkReportsPartialCiliumStateAsUncertain(t *testing.T)
```

Create a Pod named `sandbox-pool-abc` with `sandbox.id=user-sandbox` and verify
only `sandbox-user-sandbox` and `sandbox-private-deny-user-sandbox` are touched.
A same-name policy with a different selector must remain unchanged. An exact
historical managed policy without the new role may be upgraded. A Cilium failure
after standard policy success must match `runtime.ErrNetworkStateUncertain`.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/runtime/kubernetes -run 'TestUpdateNetwork(UsesCurrent|RejectsForeign|UpgradesExact|ReportsPartial)' -count=1
```

Expected: FAIL because runtime ID is currently treated as logical ID and
ordinary upserts do not validate ownership.

- [ ] **Step 3: Implement validated updates**

Add to `internal/runtime/runtime.go`:

```go
var ErrNetworkStateUncertain = errors.New("network policy state is uncertain")
```

Implement update helpers that GET and validate the current policy before Update.
Accept a missing role only for a historical object whose name, namespace,
managed/logical labels, selector, and runtime identity are otherwise exact.
Never mutate a mismatching object.

Rewrite `Runtime.UpdateNetwork` to GET the exact Pod by runtime ID, derive the
ordinary identity, update the standard policy using its logical ID, then
create/update/delete the corresponding Cilium deny. If the standard mutation
succeeds and Cilium fails, return
`errors.Join(runtime.ErrNetworkStateUncertain, err)`. Existing free functions
may remain as compatibility wrappers, but Runtime paths must use ownership
validation.

- [ ] **Step 4: Run focused and package tests**

```bash
gofmt -w internal/runtime/runtime.go internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/network.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/network_test.go internal/runtime/kubernetes/runtime_test.go
go test ./internal/runtime/kubernetes -run 'TestUpdateNetwork(UsesCurrent|RejectsForeign|UpgradesExact|ReportsPartial)' -count=1
go test ./internal/runtime/kubernetes ./internal/sandbox -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git diff --check
git add internal/runtime/runtime.go internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/network.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/network_test.go internal/runtime/kubernetes/runtime_test.go
git commit -m "fix: update kubernetes network by pod identity"
```

### Task 5: Delete the Exact Pod Before Policies and Propagate Errors

**Files:**
- Modify: `internal/runtime/kubernetes/ordinary_network.go`
- Modify: `internal/runtime/kubernetes/pod.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Modify: `internal/sandbox/manager.go`
- Test: `internal/runtime/kubernetes/runtime_test.go`
- Test: `internal/sandbox/manager_test.go`

- [ ] **Step 1: Add failing deletion and accounting tests**

Add:

```go
func TestRemoveSandboxDeletesExactPodBeforeLogicalPolicies(t *testing.T)
func TestRemoveSandboxDoesNotDeletePoliciesWhilePodDeletionUnconfirmed(t *testing.T)
func TestRemoveSandboxJoinsStandardAndCiliumDeleteFailures(t *testing.T)
func TestRemoveSandboxMissingPodFindsPoliciesByRuntimeBinding(t *testing.T)
func TestRemoveSandboxDoesNotGuessHistoricalLogicalPolicyFromRuntimeID(t *testing.T)
func TestCleanupOrphanedPoolContainersCountsOnlySuccessfulRemovals(t *testing.T)
```

Require a Pod UID precondition. A same-name replacement Pod proves the old UID
is gone; an old UID that remains blocks policy deletion. For a missing Pod,
new-format runtime-bound policies may be deleted, but unbound historical
policies remain.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/runtime/kubernetes -run 'TestRemoveSandbox(DeletesExact|DoesNotDelete|Joins|MissingPod|DoesNotGuess)' -count=1
go test ./internal/sandbox -run 'TestCleanupOrphanedPoolContainersCountsOnlySuccessfulRemovals' -count=1
```

Expected: FAIL because current code drops policy errors, deletes policies first,
and has no immutable Pod precondition.

- [ ] **Step 3: Implement exact ordered removal**

Implement:

```go
func deleteExactOrdinaryPod(ctx context.Context, client kubernetes.Interface, namespace string, pod *corev1.Pod, pollInterval, timeout time.Duration) error
func deleteOwnedOrdinaryNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace string, identity ordinaryNetworkIdentity, allowLegacy bool) error
func deleteOwnedOrdinaryCiliumPrivateDeny(ctx context.Context, client dynamic.Interface, namespace string, identity ordinaryNetworkIdentity, allowLegacy bool) error
func findBoundOrdinaryPolicies(ctx context.Context, ..., runtimeID string) ([]ordinaryPolicyReference, error)
```

Delete the Pod with `metav1.Preconditions{UID: &pod.UID}` and poll until GET is
NotFound or has a different UID. Only then delete policies. Revalidate policy
ownership immediately before deletion and use policy UID preconditions when
supported. For Cilium, verify NotFound or changed UID after Delete.

If the Pod is missing, list new-format policies by
`sandbox.runtime.id=<runtimeID>`, validate each candidate, recheck Pod absence,
then delete. Never derive an unbound historical logical ID from the runtime ID.
Use `errors.Join` for independent deletion failures. Preserve
`runtime.ErrNotFound` when neither Pod nor safely bound policy exists.

Change `cleanupOrphanedPoolContainers` so `removed` increments only after
successful removal and failures log runtime ID plus error.

- [ ] **Step 4: Run focused and race tests**

```bash
gofmt -w internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/pod.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go internal/sandbox/manager.go internal/sandbox/manager_test.go
go test ./internal/runtime/kubernetes -run 'TestRemoveSandbox(DeletesExact|DoesNotDelete|Joins|MissingPod|DoesNotGuess)' -count=1
go test ./internal/sandbox -run 'TestCleanupOrphanedPoolContainers' -count=1
go test -race ./internal/runtime/kubernetes ./internal/sandbox -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git diff --check
git add internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/pod.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go internal/sandbox/manager.go internal/sandbox/manager_test.go
git commit -m "fix: tear down kubernetes sandbox exactly"
```

### Task 6: Reconcile Provable Ordinary Policy Orphans at Startup

**Files:**
- Modify: `internal/runtime/kubernetes/ordinary_network.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Test: `internal/runtime/kubernetes/network_test.go`
- Test: `internal/runtime/kubernetes/runtime_test.go`

- [ ] **Step 1: Add failing reconciliation tests**

Add:

```go
func TestReconcileOrdinaryPoliciesDeletesOnlyProvableOrphans(t *testing.T)
func TestReconcileOrdinaryPoliciesKeepsLiveFUSEForeignAndMalformedPolicies(t *testing.T)
func TestReconcileOrdinaryPoliciesFailsClosedOnListGetOrDeleteError(t *testing.T)
func TestInitializeOrdinaryPolicyRecoveryUsesBoundedContext(t *testing.T)
```

Seed standard and Cilium resources for an exact orphan, a live matching Pod, a
FUSE role, a third-party policy, a malformed selector, a runtime binding that
points at a live exact Pod, and an exact historical ordinary policy. Only exact
ordinary orphans may be removed. Inject List, Pod List, and Delete failures and
require returned errors.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/runtime/kubernetes -run 'Test(ReconcileOrdinaryPolicies|InitializeOrdinaryPolicyRecovery)' -count=1
```

Expected: FAIL because ordinary startup reconciliation does not exist.

- [ ] **Step 3: Implement conservative reconciliation**

Implement:

```go
func reconcileOrphanedOrdinaryPolicies(ctx context.Context, client kubernetes.Interface, dynClient dynamic.Interface, namespace string, hasCilium bool) error
func classifyOrdinaryNetworkPolicy(policy *networkingv1.NetworkPolicy) (ordinaryNetworkIdentity, bool)
func classifyOrdinaryCiliumPolicy(policy *unstructured.Unstructured) (ordinaryNetworkIdentity, bool)
func hasManagedOrdinaryPodForLogicalID(ctx context.Context, client kubernetes.Interface, namespace, logicalID string) (bool, error)
```

List `sandbox.managed=true` resources. Skip every FUSE role and any object whose
name, selector, role, runtime binding, or UID annotation shape is not recognized.
Retain a recognized policy if any managed Pod has its logical ID. Delete only
after a successful empty Pod query. Return and aggregate control-plane errors.

After applying all options in `New`, use a bounded context based on
`runtimeImpl.prepareTimeout` and run reconciliation before returning. Reconcile
Cilium only when detected.

- [ ] **Step 4: Run focused and package tests**

```bash
gofmt -w internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/network_test.go internal/runtime/kubernetes/runtime_test.go
go test ./internal/runtime/kubernetes -run 'Test(ReconcileOrdinaryPolicies|InitializeOrdinaryPolicyRecovery)' -count=1
go test ./internal/runtime/kubernetes -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git diff --check
git add internal/runtime/kubernetes/ordinary_network.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/network_test.go internal/runtime/kubernetes/runtime_test.go
git commit -m "fix: reconcile kubernetes ordinary policies"
```

### Task 7: Surface Partial Upload Cleanup Failures

**Files:**
- Modify: `internal/runtime/kubernetes/file.go`
- Test: `internal/runtime/kubernetes/file_test.go`

- [ ] **Step 1: Add failing cleanup-result tests**

Add a package-level executor seam whose production default calls `execInPod`,
then add:

```go
func TestRemovePartialPodUploadValidatesExecResult(t *testing.T)
func TestUploadFileErrorJoinsPartialCleanupFailure(t *testing.T)
func TestUploadFileErrorPreservesInvalidSizeWhenCleanupSucceeds(t *testing.T)
```

Cover transport error, nil result, non-zero exit code with stderr, and success.
Inject the upload stream/executor seam so a short body produces
`runtime.ErrInvalidUploadSize` while cleanup returns another sentinel; require
both errors in the result.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/runtime/kubernetes -run 'Test(RemovePartialPodUpload|UploadFileError)' -count=1
```

Expected: FAIL because partial cleanup returns no error and is discarded.

- [ ] **Step 3: Implement checked cleanup and joined errors**

Change the helper to return an error:

```go
result, err := podExec(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
	Command: "rm -f -- " + shellEscape(tempPath),
})
if err != nil {
	return fmt.Errorf("exec cleanup: %w", err)
}
if result == nil {
	return errors.New("exec cleanup returned no result")
}
if result.ExitCode != 0 {
	return fmt.Errorf("exec cleanup exited %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
}
return nil
```

Add `joinUploadCleanupError(primary, cleanup error)`. It returns the primary
error unchanged when cleanup succeeds; otherwise it returns
`errors.Join(primary, fmt.Errorf("cleanup partial upload: %w", cleanup))`.
Use it on body-size, tar-write, and container-consumption failure paths. Restore
all injectable package variables with `t.Cleanup`.

- [ ] **Step 4: Run focused and package tests**

```bash
gofmt -w internal/runtime/kubernetes/file.go internal/runtime/kubernetes/file_test.go
go test ./internal/runtime/kubernetes -run 'Test(RemovePartialPodUpload|UploadFileError|WriteSizedTar|UploadFileCommand)' -count=1
go test ./internal/runtime/kubernetes -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git diff --check
git add internal/runtime/kubernetes/file.go internal/runtime/kubernetes/file_test.go
git commit -m "fix: report kubernetes upload cleanup failures"
```

### Task 8: Integrated Verification and Review

**Files:**
- Verify all files changed above.
- Update `docs/superpowers/specs/2026-09-12-kubernetes-ordinary-runtime-network-recovery-design.md` only if implementation reveals a real contract correction.

- [ ] **Step 1: Run the ordinary-runtime regression matrix**

```bash
go test ./internal/runtime/kubernetes -run 'Test(CreateSandbox|UpdateLabels|UpdateNetwork|RemoveSandbox|ReconcileOrdinary|RemovePartialPodUpload|UploadFileError)' -count=1
go test ./internal/sandbox -run 'Test(ManagerPoolAcquireRelabelFailure|CleanupOrphanedPoolContainers)' -count=1
```

Expected: PASS.

- [ ] **Step 2: Run affected packages with the race detector**

```bash
go test -race ./internal/runtime/kubernetes ./internal/sandbox -count=1
```

Expected: PASS with no race reports.

- [ ] **Step 3: Run the full repository suite**

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 4: Inspect final state**

```bash
git diff --check
git status --short
git log --oneline -12
```

Confirm that no ordinary Pod precedes its policies; no foreign policy can be
mutated; pool migration failures are compensated; Pod UID disappearance is
proven before policy deletion; FUSE roles are skipped by ordinary recovery;
upload errors preserve both causes; and Helm default-deny remains unchanged.

- [ ] **Step 5: Request code review**

Use `superpowers:requesting-code-review` for the full implementation diff from
`e48a796` through `HEAD`. If review finds an issue, use
`superpowers:receiving-code-review`, reproduce it with a failing test, apply the
smallest fix, and rerun focused plus full tests.

- [ ] **Step 6: Commit review-only corrections if any**

```bash
git add internal/runtime internal/sandbox docs/superpowers
git commit -m "fix: address kubernetes lifecycle review"
```

Do not create an empty commit when review requires no changes.

- [ ] **Step 7: Finish without a worktree**

Invoke `superpowers:finishing-a-development-branch` on the current normal
checkout `feat/workspace-fuse-mount`. Do not create or switch to a git worktree.
Present the skill's four integration choices only after verification is green.
