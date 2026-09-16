# Kubernetes Ordinary Immediate Destroy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the useless 30-second Kubernetes grace wait with the minimum non-forced one-second termination for API-managed ordinary sandboxes without weakening exact identity, distributed fencing, workspace finalization, kubelet termination confirmation, or network-policy cleanup.

**Architecture:** Keep the ordinary Pod template's existing grace period so external/manual deletion semantics do not change. Only `deleteExactOrdinaryPod`, which runs after Manager fencing and final sync, sends `gracePeriodSeconds: 1` together with the existing immutable UID precondition, then continues waiting for kubelet-confirmed removal of the original Pod identity before cleaning policies. Zero is forbidden because Kubernetes force deletion removes the API object without waiting for node termination confirmation; FUSE remains on its separate flush/unmount/finalizer path.

**Tech Stack:** Go, Kubernetes client-go fake client, `testify`, Go race detector, Git

---

## File structure

- Modify `internal/runtime/kubernetes/ordinary_cleanup_test.go`: add the focused RED/GREEN contract for the minimum non-forced delete that remains bound to the exact Pod UID.
- Modify `internal/runtime/kubernetes/ordinary_network.go`: add only the zero grace period to the existing exact ordinary Pod DELETE options.
- Create `docs/testing/2026-09-16-kubernetes-ordinary-destroy-remediation.md`: record the live root-cause evidence, safety reasoning, TDD evidence, deployment scope, and the still-pending post-deployment acceptance.

No Pod template, FUSE runtime, API protocol, Redis model, Helm values, or runtime image file changes are planned.

### Task 1: Capture the minimum graceful exact-delete contract

**Files:**
- Modify: `internal/runtime/kubernetes/ordinary_cleanup_test.go:198-206`

- [ ] **Step 1: Add the focused failing regression**

Add this test before the existing timeout tests:

```go
func TestOrdinaryDeletionRequestsMinimumGracefulExactUID(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", UID: "old-uid"}}
	client := fake.NewSimpleClientset(pod)

	require.NoError(t, deleteExactOrdinaryPod(context.Background(), client, "", pod, time.Millisecond, time.Second))

	var deletion ktesting.DeleteAction
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "pods" {
			deletion = action.(ktesting.DeleteAction)
			break
		}
	}
	require.NotNil(t, deletion)
	options := deletion.GetDeleteOptions()
	require.NotNil(t, options.Preconditions)
	require.NotNil(t, options.Preconditions.UID)
	require.Equal(t, pod.UID, *options.Preconditions.UID)
	require.NotNil(t, options.GracePeriodSeconds)
	require.Equal(t, int64(1), *options.GracePeriodSeconds, "zero would force-delete the API object before kubelet termination confirmation")
}
```

- [ ] **Step 2: Run the test and verify RED**

Run:

```bash
go test ./internal/runtime/kubernetes -run '^TestOrdinaryDeletionRequestsMinimumGracefulExactUID$' -count=1
```

Expected: FAIL at `require.NotNil(t, options.GracePeriodSeconds)` because the current DELETE options contain only the UID precondition.

- [ ] **Step 3: Commit the proven regression**

```bash
git add internal/runtime/kubernetes/ordinary_cleanup_test.go
git commit -m "test: expose ordinary pod grace delay"
```

### Task 2: Apply the minimum non-forced ordinary deletion grace

**Files:**
- Modify: `internal/runtime/kubernetes/ordinary_network.go:274-284`

- [ ] **Step 1: Add the minimal zero-grace DELETE option**

Replace the current Pod delete call with:

```go
	uid := pod.UID
	minimumGracePeriod := int64(1)
	err := client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &minimumGracePeriod,
		Preconditions:      &metav1.Preconditions{UID: &uid},
	})
```

Do not change the subsequent polling loop, replacement-UID success condition, termination-unconfirmed classification, or policy cleanup order.

- [ ] **Step 2: Run the focused test and verify GREEN**

Run:

```bash
go test ./internal/runtime/kubernetes -run '^TestOrdinaryDeletionRequestsMinimumGracefulExactUID$' -count=1
```

Expected: PASS.

- [ ] **Step 3: Verify ordinary identity and failure safety**

Run:

```bash
go test ./internal/runtime/kubernetes \
  -run 'Test(OrdinaryDeletion|RemoveSandbox|ExactOrdinaryRemoval|RemoveOrdinarySandbox)' \
  -count=20
```

Expected: PASS. This includes UID replacement rejection, termination-unconfirmed classification, Pod-before-policy ordering, malformed/foreign policy retention, and API permission failures.

- [ ] **Step 4: Verify the race-sensitive cleanup paths**

Run:

```bash
go test -race ./internal/runtime/kubernetes \
  -run 'Test(OrdinaryDeletion|RemoveSandbox|ExactOrdinaryRemoval|RemoveOrdinarySandbox)' \
  -count=10
```

Expected: PASS with no race report.

- [ ] **Step 5: Commit the minimal implementation**

```bash
git add internal/runtime/kubernetes/ordinary_network.go
git commit -m "perf: eliminate ordinary pod grace wait"
```

### Task 3: Verify subsystem boundaries and document deployment

**Files:**
- Create: `docs/testing/2026-09-16-kubernetes-ordinary-destroy-remediation.md`

- [ ] **Step 1: Run Kubernetes runtime and Manager integration tests**

Run:

```bash
go test ./internal/runtime/kubernetes ./internal/sandbox
go test -race ./internal/runtime/kubernetes ./internal/sandbox
go vet ./internal/runtime/kubernetes ./internal/sandbox
```

Expected: every command exits 0. The sandbox package proves fencing/live-operation/final-sync orchestration still composes with the runtime; Kubernetes tests prove network policy cleanup remains after exact Pod termination.

- [ ] **Step 2: Run FUSE termination isolation tests**

Run:

```bash
go test ./internal/runtime/kubernetes \
  -run 'Test(ConfirmPreparedSandboxTermination|FinalizePreparedSandboxRemoval|PreparedSandboxTermination|RemovePreparedSandbox)' \
  -count=10
```

Expected: PASS. These tests cover the separate FUSE flush/unmount/finalizer termination path and demonstrate that the ordinary DeleteOptions change did not alter it.

- [ ] **Step 3: Run the complete repository gates**

Run:

```bash
go vet ./...
go test ./...
git diff --check
```

Expected: every command exits 0 and the test output contains no failure.

- [ ] **Step 4: Write the remediation record**

Create `docs/testing/2026-09-16-kubernetes-ordinary-destroy-remediation.md` with these sections and facts:

```markdown
# Kubernetes 普通沙盒销毁延迟整改记录

## 根因

记录现场的 30 秒 Pod grace、`sleep infinity` PID 1、Terminating 期间仍 Running、32.561 秒 NotFound 和 API 随后约 7ms 返回。

## 实现与安全边界

说明只有 exact ordinary DELETE 增加 `gracePeriodSeconds: 1`；零值因 force deletion 语义被禁止。UID precondition、fencing、live operations、sync finalization、NotFound/replacement UID 确认和 Pod 后策略清理均保留。明确 FUSE 终止路径未修改。

## 本地验证

记录 RED 失败点、GREEN 定向测试、race、vet 和全仓命令的实际结果。

## 部署范围

只需构建和部署 sandbox-api；无 values 变更，不重建 runtime、mounter、AppArmor、Redis/Sentinel 或 Chat。

## 线上验收

上线前标记为待执行。部署后记录旧 30 秒 Pool Pod 的销毁样本、10 次销毁 p50/p95/p99、三副本并发 DELETE、最终 Pool/Pod/策略/Redis 清理和 FUSE 冒烟结果。
```

- [ ] **Step 5: Self-review against the design**

Re-read `docs/superpowers/specs/2026-09-16-kubernetes-ordinary-immediate-destroy-design.md` and verify:

- only the exact ordinary helper sets the minimum non-zero grace of one second;
- no Pod delete path added `gracePeriodSeconds: 0`;
- UID precondition is present in the same DELETE options object;
- the helper still waits for NotFound or a different UID;
- network policy deletion still happens only after the helper succeeds;
- no `buildPod`, FUSE termination, Helm, Redis, or API files changed;
- the deployment record does not claim live latency success before the user deploys the new API image.

Run:

```bash
git diff --check
git status --short
```

Expected: only the intended documentation file remains uncommitted at this point, and diff check exits 0.

- [ ] **Step 6: Commit the verified remediation record**

```bash
git add docs/testing/2026-09-16-kubernetes-ordinary-destroy-remediation.md
git commit -m "docs: record ordinary destroy remediation"
```

### Task 4: Post-deployment acceptance on `ds-ai-research`

**Files:**
- Modify: `docs/testing/2026-09-16-kubernetes-ordinary-destroy-remediation.md`

- [ ] **Step 1: Verify the deployed API image and pre-existing Pool inventory**

Use explicit `--context ds-ai-research` and namespace `aiadp-sandbox-fuse`. Confirm three API replicas are Ready, record their image digest without exposing Secret values, and identify ordinary prepared inventory through the UID-bound `sandbox.pool.state=prepared` annotation. At least one selected Pod must still have `spec.terminationGracePeriodSeconds: 30` to prove backward compatibility.

- [ ] **Step 2: Run ten sequential ordinary hot-pool samples**

For every sample: wait for three annotation-prepared ordinary Pool Pods, create through a direct per-replica port-forward, execute from a different replica, destroy from the third replica, and record only duration/status/runtime identity. Require ten successes and destruction p95 no higher than five seconds.

- [ ] **Step 3: Re-run three-replica concurrent destroy**

Run:

```bash
SANDBOX_API_TEST_URLS=http://127.0.0.1:18081,http://127.0.0.1:18082,http://127.0.0.1:18083 \
go test ./test/integration/api -run '^TestDeployedOrdinaryConcurrentDestroy$' -v -count=3 -timeout=5m
```

Expected: all ephemeral and persistent concurrent DELETE requests return explicit success and all replicas subsequently return NotFound.

- [ ] **Step 4: Run FUSE isolation smoke and final cleanup audit**

Create one FUSE sandbox, perform real write/read/flush, and destroy it. Then require ordinary Pool size 3, FUSE Pool size 3, no active non-pool or deleting managed Pod, no test-owned policy, and no test active/session/controller/lease record. Stop only the three port-forwards started by this acceptance.

- [ ] **Step 5: Record live evidence and commit**

Replace the pending online section with the measured p50/p95/p99, old-Pod compatibility evidence, concurrent result, FUSE smoke result, and final cleanup audit. Run `git diff --check`, then commit:

```bash
git add docs/testing/2026-09-16-kubernetes-ordinary-destroy-remediation.md
git commit -m "docs: record ordinary destroy live validation"
```
