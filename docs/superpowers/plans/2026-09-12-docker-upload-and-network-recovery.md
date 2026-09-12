# Docker Upload and Network Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 Docker multipart chunk 写入 tmpfs 失败，并让 sandbox-api 安全回收自身遗留的空 pair network。

**Architecture:** 所有 Docker 文件上传统一通过容器内 tar 管道进入容器 mount namespace，保留现有定长校验与原子发布。网络层增加一个带时间门槛的空 managed-network 回收器，在 runtime 启动时运行，并仅在 Docker 明确报告默认地址池耗尽时回收后重试一次；Sandbox 销毁不再吞掉网络清理错误。

**Tech Stack:** Go、Docker Engine API、testify、现有 Docker runtime fake。

---

### Task 1: Docker 上传统一进入容器 mount namespace

**Files:**
- Modify: `internal/runtime/docker/file.go:29-80`
- Modify: `internal/runtime/docker/file_test.go:100-135`
- Modify: `internal/runtime/docker/runtime_test.go:730-760`

- [ ] **Step 1: 写出普通 sandbox 不使用 CopyToContainer 的失败测试**

将现有 `TestLegacyUploadTemporaryFileBelongsToSandboxUser` 改为断言上传命令通过
`ExecPipe` 消费 tar，且 `fake.copyToCalls == 0`。在 fake 中记录 exec attach 收到的
stdin tar header，并断言 header UID/GID 为 1000。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `go test ./internal/runtime/docker -run TestLegacyUploadTemporaryFileBelongsToSandboxUser -count=1`

Expected: FAIL，显示普通 sandbox 仍调用了一次 `CopyToContainer`。

- [ ] **Step 3: 实现最小上传修复**

把 `UploadFile` 的分支：

```go
if fuseContainer {
    consumeErr = r.ExecPipe(ctx, id, []string{"tar", "xf", "-", "-C", dir}, pr)
} else {
    consumeErr = r.cli.CopyToContainer(ctx, id, dir, pr, container.CopyToContainerOptions{CopyUIDGID: true})
}
```

替换为所有 Docker sandbox 共用：

```go
consumeErr = r.ExecPipe(ctx, id, []string{"tar", "xf", "-", "-C", dir}, pr)
```

删除不再需要的 `container.CopyToContainerOptions` import 使用；保留
`isFUSEContainer` 校验、定长 tar、失败清理和原子 publish。

- [ ] **Step 4: 运行 Docker 文件测试并确认 GREEN**

Run: `go test ./internal/runtime/docker -run 'Test(LegacyUpload|WriteSizedTar|UploadSize|UploadPublish)' -count=1`

Expected: PASS。

- [ ] **Step 5: 提交上传修复**

```bash
git add internal/runtime/docker/file.go internal/runtime/docker/file_test.go internal/runtime/docker/runtime_test.go
git commit -m "fix: stream docker uploads through container namespace"
```

### Task 2: 安全回收陈旧空 pair network 并重试地址池耗尽

**Files:**
- Modify: `internal/runtime/docker/network.go:300-410,762-805`
- Modify: `internal/runtime/docker/network_test.go`
- Modify: `internal/runtime/docker/runtime.go:102-125`
- Modify: `internal/runtime/docker/runtime_test.go:730-1160`

- [ ] **Step 1: 写出回收边界与重试的失败测试**

新增表驱动测试，构造 Docker network summary 并断言：

```go
func TestCleanupStaleEmptySandboxNetworksOnlyRemovesOwnedEmptyNetworks(t *testing.T)
func TestCreateManagedPairNetworkRetriesOnceAfterAddressPoolExhaustion(t *testing.T)
func TestCreateManagedPairNetworkDoesNotRetryOtherErrors(t *testing.T)
```

测试数据必须同时覆盖：超过五分钟的空 managed pair、未满五分钟的空 pair、
仍有 endpoint 的 pair、无 managed 标签的同名网络、非 sandbox 网络。fake 增加
`networkCreateErrors []error`、`networkCreateCalls int` 和 `networkRemoveErr error`。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `go test ./internal/runtime/docker -run 'Test(CleanupStaleEmptySandboxNetworks|CreateManagedPairNetwork)' -count=1`

Expected: FAIL，显示回收与重试 helper 尚不存在。

- [ ] **Step 3: 实现带安全时间门槛的回收器**

在 `network.go` 增加：

```go
const staleSandboxNetworkAge = 5 * time.Minute

func cleanupStaleEmptySandboxNetworks(ctx context.Context, cli dockerAPI, now time.Time) (int, error)
func createManagedPairNetwork(ctx context.Context, cli dockerAPI, name string, options dnetwork.CreateOptions, now time.Time) (dnetwork.CreateResponse, error)
func isDockerAddressPoolExhausted(err error) bool
```

回收器只接受 `Labels["sandbox.managed"] == "true"`、名称为
`sandbox-pair-*`/`sandbox-net-*`/`sandbox-network`、创建至少五分钟且 inspect
确认 `len(Containers) == 0` 的网络。创建 helper 只对包含
`could not find an available, non-overlapping IPv4 address pool` 的错误执行一次
回收与一次重试。

- [ ] **Step 4: 接入两类 pair network 和 runtime 启动**

普通 `createSandboxPair` 与 `createFUSESandboxPair` 都调用
`createManagedPairNetwork(..., time.Now())`。`New` 在 `ensureNetworks` 前调用
`cleanupStaleEmptySandboxNetworks(ctx, cli, time.Now())`，失败时关闭 client 并返回。

- [ ] **Step 5: 运行网络测试并确认 GREEN**

Run: `go test ./internal/runtime/docker -run 'Test(FUSEPairNetwork|CleanupStaleEmptySandboxNetworks|CreateManagedPairNetwork)' -count=1`

Expected: PASS，且 existing network policy 测试保持通过。

- [ ] **Step 6: 提交网络恢复修复**

```bash
git add internal/runtime/docker/network.go internal/runtime/docker/network_test.go internal/runtime/docker/runtime.go internal/runtime/docker/runtime_test.go
git commit -m "fix: reclaim stale docker sandbox networks"
```

### Task 3: Sandbox 销毁返回网络清理错误

**Files:**
- Modify: `internal/runtime/docker/runtime.go:770-800`
- Modify: `internal/runtime/docker/runtime_test.go`

- [ ] **Step 1: 写出销毁不能吞掉 pair network 错误的失败测试**

新增：

```go
func TestDockerRemoveSandboxReportsPairNetworkCleanupFailure(t *testing.T)
```

fake 返回 `networkRemoveErr = errors.New("network remove failed")`，断言
`RemoveSandbox` 的错误包含该文本，同时确认 container remove 已被调用。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `go test ./internal/runtime/docker -run TestDockerRemoveSandboxReportsPairNetworkCleanupFailure -count=1`

Expected: FAIL，因为当前实现忽略 `removeSandboxPair` 错误。

- [ ] **Step 3: 合并容器与网络清理结果**

在 `RemoveSandbox` 中保留始终尝试两项清理的顺序，使用 `errors.Join` 合并：

```go
var pairErr error
if sandboxID != "" {
    pairErr = removeSandboxPair(ctx, r.cli, sandboxID)
}
if removeErr != nil {
    removeErr = fmt.Errorf("remove container: %w", removeErr)
}
return errors.Join(removeErr, pairErr)
```

- [ ] **Step 4: 运行销毁测试并确认 GREEN**

Run: `go test ./internal/runtime/docker -run 'TestDocker(RemoveSandbox|PreparedRemoval)' -count=1`

Expected: PASS。

- [ ] **Step 5: 提交销毁修复**

```bash
git add internal/runtime/docker/runtime.go internal/runtime/docker/runtime_test.go
git commit -m "fix: report docker network cleanup failures"
```

### Task 4: 全量验证与部署说明

**Files:**
- Modify: `docs/deployment/helm-deployment-upgrade.md`

- [ ] **Step 1: 运行全量静态与单元验证**

Run: `gofmt -w internal/runtime/docker/file.go internal/runtime/docker/file_test.go internal/runtime/docker/network.go internal/runtime/docker/network_test.go internal/runtime/docker/runtime.go internal/runtime/docker/runtime_test.go`

Run: `go test ./... -count=1`

Run: `go vet ./...`

Run: `go build ./cmd/sandbox`

Expected: 全部成功。

- [ ] **Step 2: 更新升级说明**

在部署文档中说明该修复只需要重新构建并更新 `sandbox-api` 镜像；不需要更新
`sandbox-runtime`、`sandbox-fuse-docker`、`sandbox-fuse-mounter`、gateway，且不需要
重启 Docker daemon。API 重启会自动回收符合安全条件的空 managed pair network。

- [ ] **Step 3: 校验 Compose 渲染**

Run: `docker compose -f docker/docker-compose.yml config >/dev/null`

Expected: exit code 0。

- [ ] **Step 4: 提交文档并检查工作树**

```bash
git add docs/deployment/helm-deployment-upgrade.md
git commit -m "docs: document docker runtime recovery upgrade"
git status --short
```

Expected: 工作树为空。

- [ ] **Step 5: 线上 MinIO 回归**

更新 sandbox-api 镜像后，对 Docker 环境复测 multipart init/chunk/status/complete/
cancel、普通 sandbox 网络启用/禁用、FUSE 创建/读写/flush/销毁，并删除所有唯一
测试路径。未更新线上镜像前，不把本地测试结果表述为线上通过。
