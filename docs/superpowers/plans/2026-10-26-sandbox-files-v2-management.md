# Sandbox Files v2 Management Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 v2 核心接口通过后补齐目录与文件管理、按行编辑和可恢复上传，使 v2 可以完整承接现有文件调用方。

**Architecture:** 扩展已验收的 `sandbox-file-op`、`FileService`、HTTP v2 与 `Files` SDK。分片上传复用现有 manager 的持久上传机制，不另建状态系统。每个新方法仍经过同一 sandbox operation gate 和路径边界。

**Tech Stack:** Go 1.25、Gin、现有 Docker/Kubernetes exec、现有 multipart upload 管理器。

**Spec:** `docs/superpowers/specs/2026-10-26-sandbox-files-v2-design.md`

## Global Constraints

- 本计划以 `docs/superpowers/plans/2026-10-26-sandbox-files-v2-core.md` 全部验收为前提；v1 行为和接口保持不变。
- 仍只允许 `/workspace`、`/tmp`，实际打开时拒绝越界符号链接并隐藏 FUSE 保留路径。
- 管理操作明确 `recursive`、`overwrite`、`create_parents`；默认值均为 `false`。无原子跨文件或 FUSE move 承诺。
- 按行编辑文件最多 8 MiB，流式上传沿用 `maxUploadBytes`；不自动重试结果未知的非幂等操作。
- 删除 v1 是独立后续决策，不属于本计划。

## Review Focus

1. 根目录、FUSE 保留文件和跨根符号链接不能被删除或移动；Task 1/2 固定。
2. `overwrite=false` 时目标存在必须返回冲突，原目标字节不变；Task 2 固定。
3. 按行编辑末尾空行、CRLF、非法范围时不得改变原字节；Task 2 固定。
4. 分片重复、乱序、完成 ACK 丢失与取消后查询需保留原上传状态和限额；Task 3 固定。
5. v2 替代清单必须覆盖实际在用的 v1 文件操作，不能仅凭 Eino 六方法宣布迁移完成；Task 3 固定。

---

## File Map

| 责任 | 文件 |
| --- | --- |
| 沙盒内管理与编辑 | `internal/filev2/manage.go`, `internal/filev2/text.go` |
| operation gate 与上传状态接线 | `internal/sandbox/files_v2.go`, `internal/sandbox/manager.go` |
| v2 HTTP 与类型 | `internal/api/handler/file_v2_manage.go`, `internal/api/handler/file_v2_upload.go`, `pkg/types/file_v2.go`, `internal/api/router.go` |
| v2 SDK | `sdk/go/files_v2.go`, `sdk/go/files_v2_test.go` |
| 迁移覆盖证据 | `docs/superpowers/reports/2026-10-26-sandbox-files-v2-parity.md` |

### Task 1: 创建目录与删除条目

**Files:** Create `internal/filev2/manage.go`, `internal/filev2/manage_test.go`, `internal/api/handler/file_v2_manage.go`; modify `internal/sandbox/files_v2.go`, `pkg/types/file_v2.go`, `internal/api/router.go`, `sdk/go/files_v2.go` 及对应测试。

**Interfaces:** `Files.Mkdir(ctx,path string,parents,existOK bool) error`、`Files.Delete(ctx,path string,recursive bool) error`；HTTP `POST /mkdir`、`DELETE /entry?path=&recursive=`。拒绝删除 `/workspace`、`/tmp` 本身；目录非空且 `recursive=false` 返回冲突。

- [ ] 写 `TestFilesV2MkdirDelete`：断言重复 mkdir、缺父目录、非空目录、保留路径和根目录拒绝，以及成功后 stat/list 与 gate 释放。
- [ ] 运行 `go test ./internal/filev2 ./internal/api/handler -run TestFilesV2MkdirDelete`；预期因 v2 管理接口缺失失败。
- [ ] 扩展 helper、Manager、handler、SDK；沿用核心计划的错误码和精确路径校验。
- [ ] 跑 `go test ./internal/filev2 ./internal/sandbox ./internal/api/...`、相应 race 测试和 `cd sdk/go && go test ./...`。
- [ ] 仅提交本任务文件：`feat(files): add v2 mkdir and delete`。

### Task 2: 复制、移动与按行范围编辑

**Files:** Modify `internal/filev2/manage.go`, `internal/filev2/text.go`, `internal/sandbox/files_v2.go`, `internal/api/handler/file_v2_manage.go`, `pkg/types/file_v2.go`, `internal/api/router.go`, `sdk/go/files_v2.go` 及对应测试。

**Interfaces:** `Files.Copy(ctx,src,dst string,recursive,overwrite,createParents bool) error`、`Files.Move(ctx,src,dst string,recursive,overwrite,createParents bool) error`、`Files.EditLines(ctx,path string,start,end int,newText string) error`；HTTP `POST /copy`、`POST /move`、`POST /text/edit-lines`。目录操作要求显式 `recursive=true`；行范围为 1 起始闭区间，`end=0` 表示文件末尾。

- [ ] 写 `TestFilesV2CopyMoveEditLines`：断言同一路径、目录/文件冲突、禁止覆盖、跨根链接、FUSE 复制读回、CRLF/末尾空行和非法行范围。
- [ ] 运行 `go test ./internal/filev2 ./internal/api/handler -run TestFilesV2CopyMoveEditLines`；预期因 v2 copy/move/edit-lines 缺失失败。
- [ ] 实现单项文件操作；move 若底层复制后删除失败，返回明确未知结果并保留可观察状态，不声称原子成功。
- [ ] 跑 `go test ./internal/filev2 ./internal/sandbox ./internal/api/...`、相应 race 测试及 SDK 测试。
- [ ] 仅提交本任务文件：`feat(files): add v2 copy move and line edit`。

### Task 3: 可恢复上传与 v1 能力覆盖检查

**Files:** Create `internal/api/handler/file_v2_upload.go`, `internal/api/handler/file_v2_upload_test.go`, `docs/superpowers/reports/2026-10-26-sandbox-files-v2-parity.md`; modify `internal/sandbox/files_v2.go`, `pkg/types/file_v2.go`, `internal/api/router.go`, `sdk/go/files_v2.go` 及 SDK 测试。

**Interfaces:** HTTP `POST /uploads`、`PUT /uploads/:upload_id/chunks/:index`、`GET /uploads/:upload_id`、`POST /uploads/:upload_id/complete`、`DELETE /uploads/:upload_id`；`Files.StartUpload/PutChunk/UploadStatus/CompleteUpload/CancelUpload`。请求与返回字段沿用既有 manager 上传语义，v2 仅提供通用命名和错误码。

- [ ] 写 `TestFilesV2ResumableUpload`：断言重复/乱序分片、超限、完成 ACK 丢失后的查询、取消后查询以及 v1 上传仍独立可用。
- [ ] 运行 `go test ./internal/api/handler -run TestFilesV2ResumableUpload`；预期因 v2 上传路由缺失失败。
- [ ] 将 v2 handler/SDK 接到原 manager 上传生命周期；不新增上传状态键、spool 目录或宽松重试规则。
- [ ] 跑 v1/v2 文件 API 与 SDK 回归，完成报告中的路由/SDK 调用方逐项清单；真实 FUSE 上验证上传完成后的同沙盒读回和独立 workspace sync。
- [ ] 仅提交本任务文件与验收报告：`feat(files): expose resumable v2 upload and record parity`。

**Handoff:** 本计划完成表示 v2 具备替代旧文件能力的候选范围；业务是否已迁移、能否删除 v1，需要后续按调用量和真实业务验收单独判断。
