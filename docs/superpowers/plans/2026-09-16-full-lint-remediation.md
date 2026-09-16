# 全仓 lint 整改实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: 使用 executing-plans 内联执行，不使用子代理、worktree 或分支合并。

**Goal:** 无上限宿主与 Linux/arm64 全仓 lint 零项，并保留现有安全和性能契约。

**Architecture:** 先修复解析与 SSE 错误传播，再修正无效测试，最后做等价 lint 整理。资源释放的忽略只用于纯释放或已完成协议后的幂等关闭，不吞写入/同步错误。

**Tech Stack:** Go、testify、Docker SDK、client-go、golangci-lint、Helm。

---

## Task 1：文件解析 RED / GREEN

Files: 新增 `internal/runtime/file_metadata.go` 与 `_test.go`；修改
`internal/runtime/docker/file.go`、`internal/runtime/kubernetes/file.go`。

- [x] 新增表驱动测试：count 拒绝空串、负数、溢出、`1 suffix`；size 拒绝负数/尾部垃圾；mtime 拒绝 NaN/Inf/溢出。合法 size=42、mtime=1700000000.25 保留纳秒。

```go
func TestParseFileCountRejectsMalformed(t *testing.T) {
    for _, input := range []string{"", "-1", "1 suffix", "999999999999999999999"} {
        _, err := ParseFileCount(input)
        require.Error(t, err)
        require.NotContains(t, err.Error(), input+" suffix")
    }
}
```

- [x] `go test ./internal/runtime -run 'TestParseFile' -count=1`：观察 RED，再实现纯 parser。count 使用 `strconv.Atoi(strings.TrimSpace(text))` 且非负；size 使用 `ParseInt(text,10,64)` 且非负；mtime 使用 `ParseFloat(text,64)` 且有限、秒可表示为 int64。错误用固定 `invalid file count/metadata`，不回显不可信输入。
- [x] 两个 runtime 所有忽略 Sscanf 的 size/mtime/count 调用改用 parser，失败立即返回。CountReservedFiles 也复用严格 count。保留现有 command、分页和 exact UID 路径，不扩大到管线重构。

```go
totalCount, err := runtime.ParseFileCount(countResult.Stdout)
if err != nil { return nil, err }
size, modTime, err := runtime.ParseFileMetadata(parts[1], parts[3])
if err != nil { return nil, err }
```

- [x] `go test ./internal/runtime/... -count=1` GREEN，检查 reserved count、上传清理和文件权限回归。
- [x] 提交本组 parser 与调用修复。

## Task 2：SSE 写失败 RED / GREEN

Files: 修改 `internal/api/handler/execute.go`、`oneshot_cleanup_test.go`。

- [x] 使用现有 observedDestroyRepository 和 disconnectStreamRuntime；失败 ResponseWriter 的 `Write` 返回 `io.ErrClosedPipe`，不取消请求。执行 handler 后要求它在 500ms 内结束、BeginDestroy 收到 destroying 记录且 context 未取消。测试结束主动取消请求以释放测试 stream。

```go
type failedSSEWriter struct { *httptest.ResponseRecorder }
func (w *failedSSEWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
```

- [x] 启用专用测试 Redis 后运行 `go test ./internal/api/handler -run 'TestOneShot.*Write' -count=1`，确认旧 handler 等待输出造成 RED。
- [x] 两处 Fprintf 都检查错误并 return；沿用 deferred durable cleanup，不写第二条错误事件、不等待 runtime termination。

```go
if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, jsonData); err != nil {
    return
}
```

- [x] 运行 `go test ./internal/api/handler -count=1`，要求新用例及 disconnect/done/cleanup_pending GREEN，提交。

## Task 3：安全测试与 reactor

Files: 修改 `internal/runtime/docker/runtime_test.go`、`internal/runtime/kubernetes/runtime_test.go`。

- [x] Docker prepared JSON 加入实际 Config 和 HostConfig，新增非空 image/labels 断言；先保留旧 marshal 观察断言失败，再用真实配置修复。

```go
preparedJSON, err := json.Marshal(struct {
    Config *container.Config
    HostConfig *container.HostConfig
}{fake.containers[info.RuntimeID].config, fake.containers[info.RuntimeID].host})
```

- [x] Kubernetes 两个 concurrent-policy 测试的 fake store 按 `action.GetVerb()` 分派，分支内读取 `action.(ktesting.GetAction/CreateAction/DeleteAction)`。使用生产删除路径新增回归，旧版必须暴露 delete 被误分派，修复后 GREEN。
- [x] `go test ./internal/runtime/docker ./internal/runtime/kubernetes -count=1`，保留 ownership、replacement、exact UID 并发负例，提交。

## Task 4：等价整理与关闭结果

Files: 无上限 lint 提示的 runtime、sandbox、mounter、storage、workspaceprobe 和单测文件。

- [x] 逐处确认 Close 是否纯资源释放。只读 fd、socket、pipe 和已显式 Close 的 deferred cleanup 采用 `defer func(){ _ = file.Close() }()` 或 `_ = conn.Close()`；原先 Write/Sync/Close 确认错误仍传播。Start/Stop/WarmUp 用例采用真实场景断言，故障注入清理错误不一律 NoError。
- [x] 核对 9 个 unused private helpers 全仓没有引用后删除；清理因此失效的 imports，不改变 exact UID 主路径。
- [x] QF/S1016/SA1019 改等价表达式和现行 container ExecOptions/ExecAttachOptions；ST1005 仅错误首字母小写，同步固定错误断言。删除两个无效赋值与两个只含注释的空分支。
- [x] nil-context 故意拒绝测试使用该行 `//nolint:staticcheck // SA1012: exercises intentional nil context rejection.`；exclusiveUse 保留等待屏障，用 defer Unlock 表达锁持有到 return，执行并发 teardown 回归。
- [x] `golangci-lint run --timeout=3m --max-issues-per-linter=0 --max-same-issues=0`，逐批回归至零；不增加全局排除或规则关闭。

## Task 5：最终验收与提交

- [x] `go test ./... -count=1`、`go build ./...`、`go vet ./...`。
- [x] `go test -race ./internal/runtime/... ./internal/sandbox ./internal/mounter ./internal/workspaceprobe -count=1`。
- [x] `GOOS=linux GOARCH=arm64 golangci-lint run --timeout=3m --max-issues-per-linter=0 --max-same-issues=0` 与 `GOOS=linux GOARCH=arm64 go build ./...`。
- [x] `go test ./internal/helmtest ./test/integration/helm -count=1` 与 `scripts/test-helm-apparmor-loader.sh`；记录外部依赖跳过，不计真实通过。
- [x] 更新 `docs/testing/2026-09-16-kubernetes-follow-ups.md`，附验证证据；`git diff --check`、逐文件复核后提交，交付用户自行构建所需镜像范围。
