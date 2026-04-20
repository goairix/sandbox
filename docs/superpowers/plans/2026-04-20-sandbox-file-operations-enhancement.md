# Sandbox 文件操作增强实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 Sandbox 平台新增四个文件操作功能：递归文件列表、按行读取、字符串替换编辑、按行范围编辑

**Architecture:** 采用分层设计，从底层 Runtime 接口开始，逐层向上实现到 SDK。每个功能在 Docker runtime 中通过 shell 命令实现，Manager 层负责参数验证和错误处理，API 层提供 HTTP 接口，SDK 层提供便捷的 Go 客户端。

**Tech Stack:** Go 1.21+, Docker API, Gin Web Framework, shell commands (find, sed)

---

## 实现阶段

本计划分为 5 个阶段，每个阶段产出可测试的功能：

1. **阶段 1**: Runtime 层 - 实现底层接口和 Docker runtime
2. **阶段 2**: Manager 层 - 添加业务逻辑和参数验证
3. **阶段 3**: API 层 - 实现 HTTP 接口
4. **阶段 4**: SDK 层 - 实现 Go 客户端
5. **阶段 5**: 集成测试和文档

---

## 阶段 1: Runtime 层实现

### Task 1.1: 修改 FileInfo.ModTime 类型

**Files:**
- Modify: `internal/runtime/runtime.go:72-78`
- Modify: `internal/runtime/docker/file.go:100-138`

- [ ] **修改 FileInfo 定义，将 ModTime 从 int64 改为 time.Time**
- [ ] **更新 Docker runtime 的 ListFiles 实现以使用 time.Time**
- [ ] **运行测试**: `go test ./internal/runtime/docker/... -v -run TestListFiles`
- [ ] **Commit**: `git commit -m "refactor: change FileInfo.ModTime from int64 to time.Time"`

### Task 1.2: 扩展 Runtime 接口

**Files:**
- Modify: `internal/runtime/runtime.go:69`

- [ ] **在 Runtime 接口中添加四个新方法签名**
- [ ] **Commit**: `git commit -m "feat(runtime): add four new file operation methods to Runtime interface"`

### Task 1.3: 实现 ListFilesRecursive

**Files:**
- Modify: `internal/runtime/docker/file.go`
- Create: `internal/runtime/docker/file_recursive_test.go`

实现要点：
- 使用 `find` 命令递归列出文件
- 使用 `wc -l` 获取总数
- 使用 `tail` 和 `head` 实现分页

- [ ] **编写测试用例**: BasicRecursion, MaxDepth, Pagination
- [ ] **实现 ListFilesRecursive 方法**
- [ ] **运行测试**: `go test ./internal/runtime/docker/... -v -run TestListFilesRecursive`
- [ ] **Commit**: `git commit -m "feat(runtime): implement ListFilesRecursive for Docker runtime"`

### Task 1.4: 实现 ReadFileLines

**Files:**
- Modify: `internal/runtime/docker/file.go`
- Modify: `internal/runtime/docker/file_recursive_test.go`

实现要点：
- 使用 `sed -n 'start,endp'` 按行读取
- 支持读取到文件末尾（endLine="$"）

- [ ] **编写测试用例**: FullFile, PartialLines, OutOfRange
- [ ] **实现 ReadFileLines 方法**
- [ ] **运行测试**: `go test ./internal/runtime/docker/... -v -run TestReadFileLines`
- [ ] **Commit**: `git commit -m "feat(runtime): implement ReadFileLines for Docker runtime"`

### Task 1.5: 实现 EditFile

**Files:**
- Modify: `internal/runtime/docker/file.go`
- Modify: `internal/runtime/docker/file_recursive_test.go`

实现要点：
- 使用 `sed 's/old/new/g'` 替换字符串
- 使用临时文件 + `mv` 保证原子性
- 实现 `shellEscapeSed()` 函数防止注入

- [ ] **编写测试用例**: ReplaceFirst, ReplaceAll, NotFound, SpecialChars
- [ ] **实现 shellEscapeSed 函数**
- [ ] **实现 EditFile 方法**
- [ ] **运行测试**: `go test ./internal/runtime/docker/... -v -run TestEditFile`
- [ ] **Commit**: `git commit -m "feat(runtime): implement EditFile for Docker runtime"`

### Task 1.6: 实现 EditFileLines

**Files:**
- Modify: `internal/runtime/docker/file.go`
- Modify: `internal/runtime/docker/file_recursive_test.go`

实现要点：
- 使用 `sed 'd'` 删除行范围
- 使用 `sed 'r'` 插入新内容
- 使用临时文件保证原子性

- [ ] **编写测试用例**: ReplaceRange, ToEnd, SingleLine
- [ ] **实现 EditFileLines 方法**
- [ ] **运行测试**: `go test ./internal/runtime/docker/... -v -run TestEditFileLines`
- [ ] **Commit**: `git commit -m "feat(runtime): implement EditFileLines for Docker runtime"`

---

## 阶段 2: Manager 层实现

### Task 2.1: 添加 Manager 方法

**Files:**
- Modify: `internal/sandbox/manager.go`

- [ ] **实现 ListFilesRecursive 方法（调用 runtime 并处理错误）**
- [ ] **实现 ReadFileLines 方法**
- [ ] **实现 EditFile 方法**
- [ ] **实现 EditFileLines 方法**
- [ ] **Commit**: `git commit -m "feat(manager): add four file operation methods"`

---

## 阶段 3: API 层实现

### Task 3.1: 定义 API 类型

**Files:**
- Modify: `pkg/types/sandbox.go`

- [ ] **添加 ListFilesRecursiveRequest/Response**
- [ ] **添加 ReadFileLinesRequest/Response**
- [ ] **添加 EditFileRequest**
- [ ] **添加 EditFileLinesRequest**
- [ ] **Commit**: `git commit -m "feat(types): add API types for file operations"`

### Task 3.2: 实现 API Handlers

**Files:**
- Modify: `internal/api/handler/file.go`

- [ ] **实现 ListFilesRecursive handler（包含参数验证）**
- [ ] **实现 ReadFileLines handler**
- [ ] **实现 EditFile handler**
- [ ] **实现 EditFileLines handler**
- [ ] **Commit**: `git commit -m "feat(api): implement file operation handlers"`

### Task 3.3: 注册路由

**Files:**
- Modify: `internal/api/router.go`

- [ ] **注册 POST /api/v1/sandboxes/:id/files/list-recursive**
- [ ] **注册 POST /api/v1/sandboxes/:id/files/read-lines**
- [ ] **注册 POST /api/v1/sandboxes/:id/files/edit**
- [ ] **注册 POST /api/v1/sandboxes/:id/files/edit-lines**
- [ ] **Commit**: `git commit -m "feat(api): register file operation routes"`

### Task 3.4: API Handler 测试

**Files:**
- Modify: `internal/api/handler/file_test.go`

- [ ] **编写 handler 测试用例（参数验证、错误处理）**
- [ ] **运行测试**: `go test ./internal/api/handler/... -v`
- [ ] **Commit**: `git commit -m "test(api): add file operation handler tests"`

---

## 阶段 4: SDK 层实现

### Task 4.1: 定义 SDK 类型

**Files:**
- Modify: `sdk/go/types.go`

- [ ] **添加 SDK 请求/响应类型（与 API 类型对应）**
- [ ] **Commit**: `git commit -m "feat(sdk): add SDK types for file operations"`

### Task 4.2: 实现 Client 方法

**Files:**
- Modify: `sdk/go/client.go`

- [ ] **实现 ListFilesRecursive 方法**
- [ ] **实现 ReadFileLines 方法**
- [ ] **实现 EditFile 方法**
- [ ] **实现 EditFileLines 方法**
- [ ] **Commit**: `git commit -m "feat(sdk): implement Client file operation methods"`

### Task 4.3: 实现 Sandbox 便捷方法

**Files:**
- Modify: `sdk/go/sandbox.go`

- [ ] **实现 Sandbox.ListFilesRecursive 便捷方法**
- [ ] **实现 Sandbox.ReadFileLines 便捷方法**
- [ ] **实现 Sandbox.EditFile 便捷方法**
- [ ] **实现 Sandbox.EditFileLines 便捷方法**
- [ ] **Commit**: `git commit -m "feat(sdk): add Sandbox convenience methods for file operations"`

---

## 阶段 5: 集成测试和文档

### Task 5.1: SDK 集成测试

**Files:**
- Modify: `sdk/go/client_test.go`

- [ ] **编写端到端测试**: TestClient_ListFilesRecursive
- [ ] **编写端到端测试**: TestClient_ReadFileLines
- [ ] **编写端到端测试**: TestClient_EditFile
- [ ] **编写端到端测试**: TestClient_EditFileLines
- [ ] **运行测试**: `go test ./sdk/go/... -v`
- [ ] **Commit**: `git commit -m "test(sdk): add integration tests for file operations"`

### Task 5.2: 更新文档

**Files:**
- Modify: `README.md` 或相关 API 文档

- [ ] **添加新功能的使用示例**
- [ ] **更新 API 文档**
- [ ] **Commit**: `git commit -m "docs: add file operations documentation"`

### Task 5.3: 更新 CHANGELOG

**Files:**
- Modify: `CHANGELOG.md`

- [ ] **添加新功能说明**
- [ ] **Commit**: `git commit -m "chore: update CHANGELOG for file operations"`

---

## 关键实现细节

### shellEscapeSed 函数

```go
// shellEscapeSed 转义 sed 表达式中的特殊字符
func shellEscapeSed(s string) string {
    s = strings.ReplaceAll(s, "\\", "\\\\")
    s = strings.ReplaceAll(s, "/", "\\/")
    s = strings.ReplaceAll(s, "&", "\\&")
    return s
}
```

### ListFilesRecursive 核心命令

```bash
# 获取总数
find /workspace -maxdepth 2 \( -type f -o -type d \) | wc -l

# 分页查询
find /workspace -maxdepth 2 \( -type f -o -type d \) -printf '%P\t%s\t%Y\t%T@\n' | tail -n +3 | head -n 10
```

### ReadFileLines 核心命令

```bash
# 读取第 10-20 行
sed -n '10,20p' /workspace/file.txt

# 读取第 10 行到文件末尾
sed -n '10,$p' /workspace/file.txt
```

### EditFile 核心命令

```bash
# 替换第一个匹配
sed 's/old/new/' /workspace/file.txt > /tmp/edit-xxx && mv /tmp/edit-xxx /workspace/file.txt

# 替换所有匹配
sed 's/old/new/g' /workspace/file.txt > /tmp/edit-xxx && mv /tmp/edit-xxx /workspace/file.txt
```

### EditFileLines 核心命令

```bash
# 替换第 10-20 行
sed '10,20d' /workspace/file.txt > /tmp/edit-xxx && \
sed -i '9r /tmp/content-xxx' /tmp/edit-xxx && \
mv /tmp/edit-xxx /workspace/file.txt
```

---

## 测试策略

### 单元测试覆盖
- Runtime 层：每个方法至少 3 个测试用例
- API 层：参数验证、错误处理、正常流程

### 集成测试覆盖
- SDK 层：端到端测试，覆盖完整调用链

### 手动测试场景
1. 大文件（10000+ 行）性能测试
2. 特殊字符处理测试
3. 并发编辑测试
4. 分页边界测试

---

## 预期产出

完成后将提供：
1. ✅ 四个新的 Runtime 接口方法
2. ✅ Docker runtime 完整实现
3. ✅ Manager 层业务逻辑
4. ✅ 四个新的 HTTP API 端点
5. ✅ Go SDK 完整支持
6. ✅ 完整的单元测试和集成测试
7. ✅ API 文档和使用示例

