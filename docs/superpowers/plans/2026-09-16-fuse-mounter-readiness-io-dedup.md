# FUSE Mounter Readiness I/O Deduplication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the redundant remote directory read from mounter readiness while preserving exact mount health and the mandatory sandbox propagation probe.

**Architecture:** `workspace-mounter` readiness becomes a local component-health proof: supervised process, real `fuse.s3fs` mount, exact mount identity, and cache safety. Kubernetes and Docker runtimes retain their existing `workspace-probe write-read-delete` call as the end-to-end delivery gate, so no API, control protocol, Helm schema, or Redis contract changes.

**Tech Stack:** Go, `testify`, Kubernetes and Docker runtimes, Go race detector, Git

---

## File structure

- Modify `internal/mounter/supervisor_test.go`: encode the new readiness contract with a regression that fails if `ReadyStatus` invokes a remote-read command.
- Modify `internal/mounter/supervisor.go`: remove only the `/bin/ls` execution and its derived deadline context; retain process, mount identity, deadline, cache and fail-closed checks.
- Modify `docs/testing/2026-09-16-kubernetes-fuse-readiness-latency-remediation.md`: record phase-two root cause, code scope, verification evidence and deployment boundary.

No runtime implementation file changes are planned because both runtimes already execute the exact `write-read-delete` propagation probe after mounter readiness. Their tests are verification gates for this optimization.

### Task 1: Capture the non-remote mounter readiness contract

**Files:**
- Modify: `internal/mounter/supervisor_test.go:651-670`

- [ ] **Step 1: Replace the old remote-read assertion with a failing regression**

Rename the test and replace its final assertions with the following complete behavior check:

```go
func TestReadyPollsStartupMountWithoutRemoteReadAndRecordsExactMountID(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
	calls := 0
	s.config.MountPollInterval = time.Millisecond
	s.config.MountInfo = func() (Mount, error) {
		calls++
		if calls < 3 {
			return Mount{ID: 10, MountPoint: bootstrap.MountPath, FilesystemType: "tmpfs"}, nil
		}
		return Mount{ID: 42, MountPoint: bootstrap.MountPath, FilesystemType: "fuse.s3fs"}, nil
	}
	status, err := s.ReadyStatus(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StateReady, s.State())
	assert.Equal(t, int64(1), status.Generation)
	assert.Equal(t, int64(42), s.mountID)
	assert.Empty(t, runner.runs, "mounter readiness must not duplicate sandbox propagation I/O")
}
```

- [ ] **Step 2: Run the test and verify RED**

Run:

```bash
go test ./internal/mounter -run '^TestReadyPollsStartupMountWithoutRemoteReadAndRecordsExactMountID$' -count=1
```

Expected: FAIL because the old implementation records `[/bin/ls -U -- <mount>]` in `runner.runs`.

- [ ] **Step 3: Commit the regression test**

```bash
git add internal/mounter/supervisor_test.go
git commit -m "test: expose duplicate mounter readiness io"
```

### Task 2: Remove the redundant mounter remote read

**Files:**
- Modify: `internal/mounter/supervisor.go:374-437`

- [ ] **Step 1: Delete only the remote-read block**

Keep mount acquisition and exact verification unchanged. Remove `wasMounting` and the entire `readCtx`/`cancel`/`runner.Run` block so the middle of `ReadyStatus` becomes:

```go
	var mount Mount
	if s.state == StateReady {
		if err := s.verifyActiveMountLocked(s.mountID); err != nil {
			s.state = StateUnhealthy
			return s.statusLocked(), err
		}
		mount = Mount{ID: s.mountID}
	} else {
		var err error
		mount, err = s.waitForMountLocked(ctx)
		if err != nil {
			if ctx.Err() == nil || !time.Now().Before(s.mountDeadline) {
				s.state = StateUnhealthy
			}
			return s.statusLocked(), err
		}
	}
	if err := s.verifyActiveMountLocked(mount.ID); err != nil {
		s.state = StateUnhealthy
		return s.statusLocked(), err
	}
	s.mountID = mount.ID
	s.state = StateReady
```

Do not change `waitForMountLocked`, `verifyActiveMountLocked`, process-exit handling, cache accounting, cache soft-limit termination or error sanitization.

- [ ] **Step 2: Run focused mounter tests and verify GREEN**

Run:

```bash
go test ./internal/mounter -run 'Test(Ready|SupervisorReady|FlushRejectsMountIdentity|Cache)' -count=1
go test -race ./internal/mounter -run 'Test(Ready|SupervisorReady|FlushRejectsMountIdentity|Cache)' -count=10
```

Expected: both commands PASS. The regression records zero `Runner.Run` calls while cancellation, early exit, mount identity and cache protections remain green.

- [ ] **Step 3: Verify runtime delivery gates remain mandatory**

Run:

```bash
go test ./internal/runtime/kubernetes -run 'TestWaitReady' -count=5
go test ./internal/runtime/docker -run 'Test(DockerPoolHitAuthorizesSameContainer|WaitReady|DockerFUSEFixedControlExecs)' -count=5
```

Expected: PASS. Kubernetes `TestWaitReadyChecksGenerationAndRunsFixedExactUIDProbe` and
`TestWaitReadyUsesMounterProofWithoutBlockingOnPodReady` still observe the final sandbox
`write-read-delete` command. Docker pool-hit readiness and its failure tests remain green.

- [ ] **Step 4: Commit the implementation**

```bash
git add internal/mounter/supervisor.go
git commit -m "perf: deduplicate mounter readiness io"
```

### Task 3: Run repository verification and record the result

**Files:**
- Modify: `docs/testing/2026-09-16-kubernetes-fuse-readiness-latency-remediation.md`

- [ ] **Step 1: Run package and repository verification**

Run:

```bash
go test ./internal/mounter ./cmd/workspace-mounter ./internal/runtime/kubernetes ./internal/runtime/docker
go test -race ./internal/mounter ./internal/runtime/kubernetes ./internal/runtime/docker
go vet ./internal/mounter ./cmd/workspace-mounter ./internal/runtime/kubernetes ./internal/runtime/docker
go test ./...
go vet ./...
git diff --check
```

Expected: every command exits with code 0. Environment-dependent integration tests may report their established explicit skips; a skip must not be described as live-cluster validation.

- [ ] **Step 2: Append the phase-two remediation record**

Add sections that state:

```markdown
## 第二阶段：mounter 远端 I/O 去重

部署第一阶段 sandbox-api 后，30 次轻量跨副本 FUSE 创建全部成功，p50 为 0.854 秒、
p95 为 3.695 秒、p99 为 4.066 秒；完整 API 套件的 30 次 FUSE 创建 p50 为 3.879 秒、
p95 为 5.652 秒、p99 为 6.685 秒。旧的 10 秒 PodReady 周期长尾已经消失，但 p95 目标
仍未达到。

分段观测和代码检查确认 `workspace-mounter health ready` 的远端 `/bin/ls` 与紧随其后的
sandbox `workspace-probe write-read-delete` 重复访问对象存储。实现已移除前者；s3fs 进程、
真实 `fuse.s3fs` mount、exact mount ID、cache 扫描和 soft-limit 保护全部保留。Kubernetes
与 Docker runtime 的强传播探针保持 mandatory，失败时仍不交付 sandbox。

本阶段只需重新构建 `sandbox-fuse-mounter` 镜像并在 Helm values 更新它的 tag。新镜像部署
前不得宣称线上 p95 已达标；部署后重复不少于 30 次轻量跨副本创建和完整 API 套件。
```

Under the existing verification section, list the exact commands from Step 1 and their actual outcomes. Include the RED test failure and the two new commit hashes; do not invent live-cluster results.

- [ ] **Step 3: Self-review the final diff**

Run:

```bash
git diff --check
git diff --stat
git status --short
```

Expected: only the remediation document is uncommitted at this point; no generated binaries, credentials, cluster dumps or unrelated files are present.

- [ ] **Step 4: Commit the verification record**

```bash
git add docs/testing/2026-09-16-kubernetes-fuse-readiness-latency-remediation.md
git commit -m "docs: record mounter readiness io remediation"
```

- [ ] **Step 5: Confirm clean handoff state**

Run:

```bash
git status --short
git log -4 --oneline
```

Expected: clean status and separate recent commits for the design, plan, regression test, implementation and remediation record.
