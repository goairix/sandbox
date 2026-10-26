# Sandbox Files v2 Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 发布与 v1 并行的通用文件 v2 核心接口，覆盖元数据、列表、glob/grep、流式读写、按行读取和精确文本编辑。

**Architecture:** 新 HTTP/SDK 只暴露通用文件能力。`FileService` 复用现有 sandbox operation gate；Docker 和 Kubernetes 通过现有执行通道启动同一个短生命周期 `sandbox-file-op`，在沙盒内完成路径解析、搜索及文本操作。Eino 仅作为独立适配契约，不进入服务端依赖。

**Tech Stack:** Go 1.25、Gin、Docker/Kubernetes exec、Linux `openat2`、固定版本 ripgrep、现有 Go SDK。

**Spec:** `docs/superpowers/specs/2026-10-26-sandbox-files-v2-design.md`

## Global Constraints

- v1 路由、SDK 方法和行为不变；只新增 `/api/v2/sandboxes/:id/files` 和 v2 SDK。
- 默认根 `/workspace`，只允许 `/workspace` 或 `/tmp`；沙盒内实际打开时拒绝越界符号链接，统一隐藏 FUSE 保留路径。
- 列表默认 200、最多 1000；文本读取响应最多 1 MiB、文本编辑文件最多 8 MiB；流式上传沿用 `maxUploadBytes`。
- glob/grep 单次最多扫描 10000 个文件或 64 MiB、返回 1000 项，执行最多 5 秒；上限可降低，不能由请求放大。
- `ReadLines.start_line` 从 1 开始；`line_count=0` 到文件末尾但仍受 1 MiB 上限；非 `replace_all` 编辑只在恰好匹配一次时写入。
- `Write` 成功表示同沙盒读回可见；远端持久化仍以现有 workspace sync 为准。超时或取消后不自动重试非幂等编辑。
- 不增加常驻 sidecar、文件元数据库或新写入队列；不把 Eino 依赖加入主服务或 SDK 模块。

## Review Focus

1. `/workspace/link -> /etc` 及扫描中切换的符号链接必须拒绝越界；Task 1/5 用 Linux 测试固定。
2. HTTP Range 起点超过 EOF、空文件和取消中的流必须有确定结果并释放 operation gate；Task 3 固定。
3. 文本末尾换行、CRLF、Unicode、重复 `old_string` 和编辑后读回不能被重排或误改；Task 4 固定。
4. 搜索超过文件/字节/结果/时间上限必须报告不完整，Eino 适配器不能返回部分结果为成功；Task 5/7 固定。
5. FUSE 保留路径在 stat、list、glob、grep 和直接读取中必须一致不可见；Task 2/5/8 固定。

---

## File Map

| 责任 | 文件 |
| --- | --- |
| 沙盒内安全文件操作与协议 | `internal/filev2/path_linux.go`, `internal/filev2/path_unsupported.go`, `internal/filev2/ops.go`, `internal/filev2/text.go`, `internal/filev2/search.go`, `cmd/sandbox-file-op/main.go` |
| Docker/Kubernetes 一次执行与流 | `internal/runtime/docker/file_v2.go`, `internal/runtime/kubernetes/file_v2.go`, `internal/runtime/file_v2.go` |
| sandbox operation gate 和可读流生命周期 | `internal/sandbox/files_v2.go` |
| HTTP 类型、处理和路由 | `pkg/types/file_v2.go`, `internal/api/handler/file_v2.go`, `internal/api/router.go` |
| Go SDK | `sdk/go/files_v2.go`, `sdk/go/files_v2_test.go` |
| 独立 Eino 适配示例 | `examples/eino-files-backend/go.mod`, `examples/eino-files-backend/backend.go`, `examples/eino-files-backend/backend_test.go` |
| 沙盒镜像 | `docker/images/sandbox/Dockerfile`, `docker/images/sandbox-fuse/Dockerfile` |

### Task 1: 沙盒内路径边界与一次执行协议

**Files:** Create `internal/filev2/path_linux.go`, `internal/filev2/path_linux_test.go`, `internal/filev2/path_unsupported.go`, `internal/filev2/ops.go`, `cmd/sandbox-file-op/main.go`; modify both sandbox Dockerfiles.

**Interfaces:** `filev2.Run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error` 作为 CLI 入口；内部 `openBeneath(rootFD int, relative string, flags int, mode uint32) (int, error)` 使用 Linux `openat2` 的 beneath/no-magic-link 约束。路径和操作由分离的 argv 字段传入；`write` 的 stdin、`read` 的 stdout 是原始字节，其他操作使用 JSON 输入/输出。对 `stat/list/read/write` 使用同一根目录句柄；JSON 控制消息不与原始文件字节混流。

- [ ] 写 `TestOpenBeneathRejectsTraversalSymlinkAndSwap`、`TestOpenBeneathAllowsWorkspaceAndTmp`：断言越界/悬空链接拒绝，受控根内正常文件可打开。
- [ ] 在 Linux 容器运行 `go test ./internal/filev2 -run 'TestOpenBeneath'`；预期因 `openBeneath` 未实现失败。
- [ ] 实现独立的路径解析与 CLI 输入协议，非法操作和非 JSON 控制消息在打开文件前失败；非 Linux 文件用 `path_unsupported.go` 明确返回不支持，以保持宿主机测试可编译；在两种沙盒镜像构建静态二进制，不改变启动命令。
- [ ] 在 Linux 容器内运行 `go test ./internal/filev2 ./cmd/sandbox-file-op`，并交叉编译 amd64/arm64 helper；确认越界拒绝、正常路径通过。
- [ ] 仅提交本任务文件：`feat(files): add bounded in-sandbox file operations helper`。

### Task 2: Docker/Kubernetes transport 与 v2 元数据

**Files:** Create `internal/runtime/file_v2.go`, `internal/runtime/docker/file_v2.go`, `internal/runtime/kubernetes/file_v2.go`, `internal/sandbox/files_v2.go` 及对应 `_test.go`。

**Interfaces:** `runtime.FileV2Transport.RunFileOp(ctx context.Context, id string, argv []string, stdin io.Reader) (io.ReadCloser, error)`，Docker/Kubernetes 编译时断言实现；Manager 在一个入口检查该接口，不支持时返回明确错误。`Manager.StatFileV2(ctx,id,path) (runtime.FileInfo,error)` 与 `Manager.ListFilesV2(ctx,id,path,cursor string,limit int) (runtime.FileListPage,error)`；`FileListPage` 含 `Files []FileInfo`、`NextCursor string`，游标按路径字典序推进而非承诺目录快照。流的 `Close` 必须等候 transport 终态并释放原 operation gate。

- [ ] 写 `TestFileV2TransportCloseJoins` 和 `TestFileV2StatList`：断言默认 200、最大 1000、稳定游标、保留路径不可见、关闭后 gate 释放。
- [ ] 运行 `go test ./internal/runtime/docker ./internal/runtime/kubernetes ./internal/sandbox -run 'TestFileV2'`；预期因 transport 或 Manager 方法缺失失败。
- [ ] 在原 Docker/Kubernetes exec 通道接一次 helper 调用；实现 Manager 的权限门、状态和保留路径过滤，不复制 v1 handler 逻辑。
- [ ] 跑 `go test ./internal/runtime/docker ./internal/runtime/kubernetes ./internal/sandbox` 中新增测试和 `go test -race` 对应包，确认通过。
- [ ] 仅提交本任务文件：`feat(files): add v2 stat and directory listing`。

### Task 3: 流式原始字节读写

**Files:** Modify `internal/filev2/ops.go`, `internal/runtime/{docker,kubernetes}/file_v2.go`, `internal/sandbox/files_v2.go`; create `internal/api/handler/file_v2_stream.go`, `internal/api/handler/file_v2_stream_test.go`; modify `internal/api/router.go` 的上传体限额识别。

**Interfaces:** `Manager.ReadFileRangeV2(ctx,id,path string,start,length int64) (io.ReadCloser,error)`、`Manager.WriteFileV2(ctx,id,path string,size int64,createParents bool,src io.Reader) error`；HTTP `GET/PUT /content?path=`，`GET` 支持单个标准 Range（206 与 `Content-Range`，越 EOF 416），`PUT` 要求 `Content-Length` 并验证精确 EOF。

- [ ] 写 `TestFilesV2Content`：断言空文件、`Range: bytes=2-4`、越 EOF、错误大小、取消、零字节以及同沙盒写后读回。
- [ ] 运行 `go test ./internal/api/handler -run TestFilesV2Content`；预期因 v2 content handler 缺失失败。
- [ ] 实现无 base64/JSON 的直接流；保持现有上传配置上限，取消/错误时终结 helper 并释放 gate；仅 v2 `PUT` 使用上传路由上限。
- [ ] 跑 `go test ./internal/api/handler ./internal/runtime/docker ./internal/runtime/kubernetes ./internal/sandbox` 及对应 race 测试。
- [ ] 仅提交本任务文件：`feat(files): stream v2 content with bounded ranges`。

### Task 4: 按行读取和唯一匹配编辑

**Files:** Create `internal/filev2/text.go`, `internal/filev2/text_test.go`, `internal/api/handler/file_v2_text.go`, `internal/api/handler/file_v2_text_test.go`; modify `internal/sandbox/files_v2.go`。

**Interfaces:** `Manager.ReadLinesV2(ctx,id,path string,start,count int) (string,error)`；`Manager.EditTextV2(ctx,id,path,oldText,newText string,replaceAll bool) error`；HTTP `POST /text/read-lines`、`POST /text/edit`。

- [ ] 写 `TestFilesV2Text`：断言 1 起始行号、CRLF/末尾换行、1 MiB 读取上限、Unicode、空/相同字符串、0/1/2 次匹配和 8 MiB 编辑上限。
- [ ] 运行 `go test ./internal/filev2 ./internal/api/handler -run TestFilesV2Text`；预期因严格编辑或 v2 handler 缺失失败。
- [ ] 实现沙盒内一次读/计数/写回和直接错误分类；只修改 v2 方法，v1 `EditFile` 仍按原语义工作。
- [ ] 跑 `go test ./internal/filev2 ./internal/api/handler ./internal/sandbox` 和 race 测试；在写后同沙盒读回校验字节不变或目标替换。
- [ ] 仅提交本任务文件：`feat(files): add line reads and exact text edit`。

### Task 5: glob 与 grep

**Files:** Create `internal/filev2/search.go`, `internal/filev2/search_test.go`, `internal/api/handler/file_v2_search.go`, `internal/api/handler/file_v2_search_test.go`; modify两个沙盒 Dockerfiles以提供 ripgrep 14.1.1、`internal/runtime/file_v2.go`、`internal/sandbox/files_v2.go`。

**Interfaces:** `Manager.GlobFilesV2(ctx,id string,req runtime.FileV2GlobRequest) (runtime.FileV2SearchPage,error)`、`Manager.GrepFilesV2(ctx,id string,req runtime.FileV2GrepRequest) (runtime.FileV2GrepResult,error)`。`FileV2SearchPage` 含 `Files []FileInfo`、`Truncated bool`；`FileV2GrepResult` 含 `Matches []FileV2Match`、`Truncated bool`，每个 match 含路径、1 起始行号和内容。所有 glob、文件类型、大小写、多行和上下文参数使用结构化 argv，不经 shell。

- [ ] 写 `TestFilesV2Search`：断言 `**/*.go`、字符类、非法正则、大小写/多行/上下文、符号链接、保留路径及文件/字节/结果/时间预算；限额结果不能伪称完整。
- [ ] 在 Linux 镜像运行 `go test ./internal/filev2 -run TestFilesV2Search`；预期因 v2 search 缺失失败。
- [ ] 在 helper 中从锚定的根目录调用固定 `rg --json`，按 amd64/arm64 分别固定发布包及校验和；限制扫描与输出并稳定排序，解析器遇无效或不完整结果返回错误。
- [ ] 跑 `go test ./internal/filev2 ./internal/api/handler ./internal/sandbox`，另在 Linux 镜像内运行搜索契约测试。
- [ ] 仅提交本任务文件：`feat(files): add bounded glob and grep`。

### Task 6: HTTP v2 类型、路由和 SDK

**Files:** Create `pkg/types/file_v2.go`, `internal/api/handler/file_v2.go`, `sdk/go/files_v2.go`, `sdk/go/files_v2_test.go`; modify `internal/api/router.go` 和对应 router 测试。

**Interfaces:** `Sandbox.Files() *Files`；`Files.Stat/List/Glob/Grep/Read/Write/ReadLines/Edit` 映射 v2。`Read` 返回 `io.ReadCloser`，`Write` 接收 `io.Reader` 和 `int64` 字节数。错误码固定为 `FILE_INVALID`(400)、`FILE_PATH_FORBIDDEN`(403)、`FILE_NOT_FOUND`(404)、`FILE_TYPE_MISMATCH`(409)、`FILE_CONFLICT`(409)、`FILE_LIMIT_EXCEEDED`(413)、`SANDBOX_NOT_READY`(503)、`FILE_OUTCOME_UNKNOWN`(503)。

- [ ] 写 `TestFilesV2Router` 与 `TestFilesV2SDK`：断言 Bearer 认证、v1 路由不变、每个 v2 请求/响应、流关闭、取消和机器可读错误码。
- [ ] 运行 `go test ./internal/api -run TestFilesV2Router` 与 `cd sdk/go && go test ./... -run TestFilesV2SDK`；预期因 v2 路由/客户端缺失失败。
- [ ] 增加独立 v2 路由组，沿用原认证和限流；SDK 保留原 API 且不覆盖已有 `Client`/`Sandbox` 方法。
- [ ] 跑 `go test ./internal/api/... ./pkg/types/...` 与 `cd sdk/go && go test ./...`；确认旧文件 API 测试仍通过。
- [ ] 仅提交本任务文件：`feat(files): expose generic files v2 in HTTP and SDK`。

### Task 7: 独立 Eino 契约适配

**Files:** Create `examples/eino-files-backend/go.mod`, `backend.go`, `backend_test.go`；仅该独立示例模块引入 Eino v0.9.22。

**Interfaces:** `var _ filesystem.Backend = (*Backend)(nil)`；六方法映射 Task 6 SDK，虚拟 `/` 映射 `/workspace`，`GrepRaw` 收到 `truncated=true` 返回错误。

- [ ] 写 `TestBackendContract`：断言六方法、行偏移、唯一匹配、多行 grep、glob 元数据和不完整搜索返回错误。
- [ ] 在 `examples/eino-files-backend` 运行 `go test ./... -run TestBackendContract`；预期适配器缺失时编译失败。
- [ ] 实现薄适配器，不在主模块加入 Eino 依赖；可选 `Shell` 和 `MultiModalRead` 不在本任务实现。
- [ ] 在示例模块运行 `go test ./...`，并检查主模块及 SDK `go.mod` 未新增 Eino。
- [ ] 仅提交本任务文件：`test(files): prove Eino backend compatibility`。

### Task 8: 真实 FUSE 验收与第一阶段发布检查

**Files:** Add focused integration tests under `internal/api/` and a short validation record in `docs/superpowers/reports/`；不修改 v1 产品代码。

**Interfaces:** 使用 Task 6 的公开 v2 HTTP/SDK 与现有 workspace sync 接口；记录沙盒内读回和远端持久化是两个不同检查。

- [ ] 写 `TestFilesV2RuntimeContract` 集成测试：镜像含 helper/固定 rg，覆盖核心路径、取消/限额/保留路径及同沙盒读回。
- [ ] 在隔离 Docker 和 Kubernetes/FUSE sandbox 运行 `go test ./internal/api -run TestFilesV2RuntimeContract`；若失败，保留失败证据并只修对应操作。
- [ ] 调用现有 workspace sync 验证远端持久化，记录环境、镜像 digest、原始请求/响应与限制；不得把同沙盒读回称作远端持久化。
- [ ] 跑 v1 文件接口回归、核心包 Linux `go test`/race/vet 和 SDK/示例契约；出现真实失败时保留失败记录并修复对应小片。
- [ ] 仅提交本任务验证与报告文件：`test(files): validate files v2 alongside v1`。

**Follow-on plan:** v2 文件管理、按行编辑和可恢复上传见 `docs/superpowers/plans/2026-10-26-sandbox-files-v2-management.md`。核心阶段通过后才执行它；完成调用清单和迁移验收后才能讨论 v1 退役。
