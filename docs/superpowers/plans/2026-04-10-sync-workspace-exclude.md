# SyncWorkspace Exclude 参数实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 `SyncWorkspace` API 新增 `exclude` 参数，同步时跳过匹配的路径前缀（如 `.agent`），支持 Eino Agent 场景下 workspace 同步排除身份文件目录。

**Architecture:** 在 `SyncWorkspaceRequest` 中新增 `Exclude []string` 字段，handler 透传到 `Manager.SyncWorkspace()`，最终在 `syncFromContainer()` 的三个环节（manifest 过滤、changed set 计算、deleted 计算）以及 `fullSyncFromContainer()` 回退路径中应用 exclude 过滤。

**Tech Stack:** Go, testify, Gin

---

### Task 1: 新增 `isExcluded` 辅助函数并测试

**Files:**
- Create: `internal/sandbox/workspace_test.go`

- [ ] **Step 1: 写失败测试**

```go
package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsExcluded(t *testing.T) {
	exclude := []string{".agent", ".cache"}

	// Exact match
	assert.True(t, isExcluded(".agent", exclude))
	assert.True(t, isExcluded(".cache", exclude))

	// Prefix match (subdirectories / files)
	assert.True(t, isExcluded(".agent/IDENTITY.md", exclude))
	assert.True(t, isExcluded(".agent/skills/code.yaml", exclude))
	assert.True(t, isExcluded(".cache/tmp.dat", exclude))

	// Directory entries (trailing slash)
	assert.True(t, isExcluded(".agent/", exclude))
	assert.True(t, isExcluded(".agent/skills/", exclude))

	// Non-excluded paths
	assert.False(t, isExcluded("src/main.py", exclude))
	assert.False(t, isExcluded("data/input.csv", exclude))
	assert.False(t, isExcluded(".agentx/other", exclude))
	assert.False(t, isExcluded("my.agent/file", exclude))

	// Empty exclude list
	assert.False(t, isExcluded(".agent", nil))
	assert.False(t, isExcluded(".agent", []string{}))
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/sandbox/ -run TestIsExcluded -v`
Expected: FAIL — `isExcluded` 未定义

- [ ] **Step 3: 在 workspace.go 中实现 `isExcluded`**

在 `internal/sandbox/workspace.go` 文件顶部（`import` 块之后，第一个方法之前）添加：

```go
// isExcluded reports whether path should be skipped during workspace sync.
// A path is excluded if it equals any exclude entry or starts with an exclude
// entry followed by "/". For example, exclude entry ".agent" matches ".agent",
// ".agent/", ".agent/skills/code.yaml", etc.
func isExcluded(path string, exclude []string) bool {
	for _, e := range exclude {
		if path == e || strings.HasPrefix(path, e+"/") {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/sandbox/ -run TestIsExcluded -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox
git add internal/sandbox/workspace.go internal/sandbox/workspace_test.go
git commit -m "feat(workspace): add isExcluded helper for sync path filtering"
```

---

### Task 2: 扩展 API 类型和 Manager 签名

**Files:**
- Modify: `pkg/types/workspace.go:18-25` (`SyncWorkspaceRequest` struct)
- Modify: `internal/sandbox/workspace.go:99-122` (`SyncWorkspace` method signature)
- Modify: `internal/api/handler/workspace.go:56-82` (`SyncWorkspace` handler)

- [ ] **Step 1: 给 `SyncWorkspaceRequest` 添加 `Exclude` 字段**

In `pkg/types/workspace.go`, change `SyncWorkspaceRequest`:

```go
type SyncWorkspaceRequest struct {
	Direction string   `json:"direction" binding:"required,oneof=to_container from_container"`
	Exclude   []string `json:"exclude,omitempty"`
}
```

- [ ] **Step 2: 修改 `Manager.SyncWorkspace` 签名接受 `exclude` 参数**

In `internal/sandbox/workspace.go`, change the `SyncWorkspace` method：

```go
// SyncWorkspace manually syncs files in the given direction.
// exclude is an optional list of path prefixes to skip during from_container sync.
func (m *Manager) SyncWorkspace(ctx context.Context, sandboxID, direction string, exclude []string) error {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	runtimeID := sb.RuntimeID
	scoped, hasWS := m.workspaces[sandboxID]
	m.mu.RUnlock()

	if !hasWS {
		return fmt.Errorf("no workspace mounted for sandbox %s", sandboxID)
	}

	switch direction {
	case "to_container":
		return m.syncToContainer(ctx, scoped, runtimeID)
	case "from_container":
		return m.syncFromContainer(ctx, sandboxID, runtimeID, exclude)
	default:
		return fmt.Errorf("invalid sync direction: %s", direction)
	}
}
```

- [ ] **Step 3: 修改 handler 透传 `exclude` 参数**

In `internal/api/handler/workspace.go`, change the `SyncWorkspace` handler 的调用行：

```go
	if err := h.manager.SyncWorkspace(c.Request.Context(), id, req.Direction, req.Exclude); err != nil {
```

- [ ] **Step 4: 修改 `syncFromContainer` 签名接受 `exclude` 参数（先不实现过滤逻辑）**

In `internal/sandbox/workspace.go`, change `syncFromContainer` signature and the existing internal callers:

`syncFromContainer` 方法签名改为：

```go
func (m *Manager) syncFromContainer(ctx context.Context, sandboxID, runtimeID string, exclude []string) error {
```

同时更新所有内部调用点，传入 `nil`：

`UnmountWorkspace` 方法中（约第 80 行）：
```go
	if err := m.syncFromContainer(ctx, sandboxID, runtimeID, nil); err != nil {
```

`Manager.Destroy` 方法中（`internal/sandbox/manager.go` 约第 225 行）：
```go
		_ = m.syncFromContainer(ctx, id, sb.RuntimeID, nil)
```

- [ ] **Step 5: 确认编译通过**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./...`
Expected: 编译成功，无错误

- [ ] **Step 6: 运行已有测试确认无回归**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./... -v`
Expected: 所有测试通过

- [ ] **Step 7: 提交**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox
git add pkg/types/workspace.go internal/sandbox/workspace.go internal/sandbox/manager.go internal/api/handler/workspace.go
git commit -m "feat(workspace): add exclude parameter to SyncWorkspace API"
```

---

### Task 3: 在 `syncFromContainer` 中实现 exclude 过滤逻辑

**Files:**
- Modify: `internal/sandbox/workspace.go:229-318` (`syncFromContainer` method)

这是核心改动。exclude 需要在三个环节生效：
1. 构建 `changedSet` 时跳过 exclude 路径
2. 构建 `deletedFiles` 时跳过 exclude 路径
3. `fullSyncFromContainer` 回退时也跳过 exclude 路径

- [ ] **Step 1: 写测试覆盖 exclude 过滤行为**

在 `internal/sandbox/workspace_test.go` 中追加：

```go
func TestSyncFromContainer_ExcludeFiltering(t *testing.T) {
	// Test that excluded paths are filtered from manifest
	manifest := map[string]int64{
		"src/main.py":                100,
		".agent/IDENTITY.md":         200,
		".agent/skills/code.yaml":    200,
		".agent/":                    200,
		"data/input.csv":             100,
	}

	exclude := []string{".agent"}

	// Simulate changed set building with exclude
	changedSet := make(map[string]struct{})
	var cutoff int64 = 0
	for path, modtime := range manifest {
		if strings.HasSuffix(path, "/") {
			continue
		}
		if isExcluded(path, exclude) {
			continue
		}
		if cutoff == 0 || modtime > cutoff {
			changedSet[path] = struct{}{}
		}
	}

	assert.Contains(t, changedSet, "src/main.py")
	assert.Contains(t, changedSet, "data/input.csv")
	assert.NotContains(t, changedSet, ".agent/IDENTITY.md")
	assert.NotContains(t, changedSet, ".agent/skills/code.yaml")

	// Simulate deleted files building with exclude
	storageFiles := map[string]struct{}{
		"src/main.py":             {},
		"old_file.txt":            {},
		".agent/SOUL.md":          {},
	}

	var deletedFiles []string
	for path := range storageFiles {
		if isExcluded(path, exclude) {
			continue
		}
		if _, exists := manifest[path]; !exists {
			deletedFiles = append(deletedFiles, path)
		}
	}

	assert.Contains(t, deletedFiles, "old_file.txt")
	assert.NotContains(t, deletedFiles, ".agent/SOUL.md")
}
```

- [ ] **Step 2: 在文件顶部添加 `"strings"` import（如果 workspace_test.go 还没有）**

确保 `internal/sandbox/workspace_test.go` 的 import 包含 `"strings"`：

```go
import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)
```

- [ ] **Step 3: 运行测试确认通过**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/sandbox/ -run TestSyncFromContainer_ExcludeFiltering -v`
Expected: PASS（测试只调用已实现的 `isExcluded`，验证过滤逻辑的正确性）

- [ ] **Step 4: 在 `syncFromContainer` 中应用 exclude 过滤**

In `internal/sandbox/workspace.go`, modify `syncFromContainer` method. 在构建 `changedSet` 的循环中（约第 276 行）添加 exclude 检查：

```go
	// Compute changed files: container files with modtime > cutoff
	changedSet := make(map[string]struct{})
	for path, modtime := range manifest {
		if strings.HasSuffix(path, "/") {
			continue // skip directories
		}
		if isExcluded(path, exclude) {
			continue
		}
		if cutoff == 0 || modtime > cutoff {
			changedSet[path] = struct{}{}
		}
	}
```

在构建 `deletedFiles` 的循环中（约第 286 行）添加 exclude 检查：

```go
	// Compute deleted files: in storage but not in container
	var deletedFiles []string
	for path := range storageFiles {
		if isExcluded(path, exclude) {
			continue
		}
		if _, exists := manifest[path]; !exists {
			deletedFiles = append(deletedFiles, path)
		}
	}
```

- [ ] **Step 5: 在 `fullSyncFromContainer` 中应用 exclude 过滤**

修改 `fullSyncFromContainer` 签名接受 exclude，并在 tar 解压循环中过滤：

```go
func (m *Manager) fullSyncFromContainer(ctx context.Context, scoped storage.ScopedFS, runtimeID string, exclude []string) error {
	tarReader, err := m.runtime.DownloadDir(ctx, runtimeID, "/workspace")
	if err != nil {
		return fmt.Errorf("download workspace: %w", err)
	}
	defer tarReader.Close()

	tr := tar.NewReader(tarReader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		name := strings.TrimPrefix(hdr.Name, "workspace/")
		if name == "" {
			continue
		}

		if isExcluded(name, exclude) {
			continue
		}

		if hdr.Typeflag == tar.TypeDir {
			_ = scoped.MakeDir(ctx, strings.TrimRight(name, "/"), 0755)
			continue
		}

		writer, err := scoped.Create(ctx, name)
		if err != nil {
			return fmt.Errorf("create %q: %w", name, err)
		}

		_, copyErr := io.Copy(writer, tr)
		closeErr := writer.Close()
		if copyErr != nil {
			return fmt.Errorf("write %q: %w", name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("flush %q to storage: %w", name, closeErr)
		}
	}

	return nil
}
```

同时更新 `syncFromContainer` 中对 `fullSyncFromContainer` 的两处调用（约第 249 行和第 265 行），传入 `exclude`：

```go
		if err := m.fullSyncFromContainer(ctx, scoped, runtimeID, exclude); err != nil {
```

- [ ] **Step 6: 同样修改 `downloadChangedFiles` 加 exclude 过滤（防御性）**

虽然 `changedSet` 已经排除了 exclude 路径，但 tar 中的目录条目也应跳过。修改 `downloadChangedFiles`：

在 `downloadChangedFiles` 方法中，`name` 计算之后、`TypeDir` 判断之前，添加：

```go
		if isExcluded(name, exclude) {
			continue
		}
```

同时更新方法签名：

```go
func (m *Manager) downloadChangedFiles(ctx context.Context, scoped storage.ScopedFS, runtimeID string, changedSet map[string]struct{}, exclude []string) error {
```

以及 `syncFromContainer` 中的调用（约第 303 行）：

```go
		if err := m.downloadChangedFiles(ctx, scoped, runtimeID, changedSet, exclude); err != nil {
```

- [ ] **Step 7: 确认编译通过**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./...`
Expected: 编译成功

- [ ] **Step 8: 运行全部测试确认无回归**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./... -v`
Expected: 所有测试通过

- [ ] **Step 9: 提交**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox
git add internal/sandbox/workspace.go internal/sandbox/workspace_test.go
git commit -m "feat(workspace): implement exclude filtering in syncFromContainer"
```
