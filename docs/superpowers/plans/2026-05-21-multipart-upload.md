# 分片上传实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增五个分片上传接口（init/chunk/status/complete/cancel），分片暂存于容器 `/tmp/.uploads/`，状态持久化到 Redis，不影响现有上传逻辑。

**Architecture:** Handler 层负责参数校验和 HTTP 响应；Manager 层持有 Redis state.Store，负责分片状态读写和调用 runtime.Exec 操作容器内文件；分片目录在 `/tmp/.uploads/`，sync 机制只扫描 `/workspace`，天然隔离。

**Tech Stack:** Go, Gin, Redis (github.com/redis/go-redis/v9 via state.Store), github.com/google/uuid

---

## 文件变更清单

| 文件 | 操作 |
|------|------|
| `pkg/types/file.go` | 新增 5 组请求/响应类型 |
| `internal/sandbox/manager.go` | 新增 `multipartStore` 字段 + 5 个 Manager 方法 |
| `internal/api/handler/file.go` | 新增 5 个 Handler 方法 |
| `internal/api/router.go` | 注册 5 条新路由 |

---

## Task 1: 新增 pkg/types 类型

**Files:**
- Modify: `pkg/types/file.go`

- [ ] **Step 1: 在 `pkg/types/file.go` 末尾追加以下类型**

```go
// MultipartInitRequest is the request body for POST .../files/upload/init.
type MultipartInitRequest struct {
	Path        string `json:"path" binding:"required"`
	TotalChunks int    `json:"total_chunks" binding:"required,min=1"`
}

// MultipartInitResponse is returned by the init endpoint.
type MultipartInitResponse struct {
	UploadID string `json:"upload_id"`
}

// MultipartChunkResponse is returned after each chunk upload.
type MultipartChunkResponse struct {
	Received int `json:"received"`
	Total    int `json:"total"`
}

// MultipartStatusResponse is returned by the status endpoint.
type MultipartStatusResponse struct {
	UploadID       string    `json:"upload_id"`
	DestPath       string    `json:"dest_path"`
	TotalChunks    int       `json:"total_chunks"`
	ReceivedChunks int       `json:"received_chunks"`
	CreatedAt      time.Time `json:"created_at"`
}

// MultipartCompleteRequest is the request body for POST .../files/upload/complete.
type MultipartCompleteRequest struct {
	UploadID string `json:"upload_id" binding:"required"`
}

// MultipartCompleteResponse is returned after a successful merge.
type MultipartCompleteResponse struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// MultipartCancelRequest carries the upload_id for DELETE .../files/upload/cancel.
type MultipartCancelRequest struct {
	UploadID string `form:"upload_id" binding:"required"`
}
```

- [ ] **Step 2: 确认编译通过**

```bash
go build ./pkg/types/...
```
Expected: 无输出，exit 0

- [ ] **Step 3: Commit**

```bash
git add pkg/types/file.go
git commit -m "feat(types): add multipart upload request/response types"
```

---

## Task 2: Manager 新增 multipartStore 字段和状态操作

**Files:**
- Modify: `internal/sandbox/manager.go`

分片上传状态存储在 Redis，key 格式：`sandbox:multipart:{sandbox_id}:{upload_id}`，TTL 24h。
Manager 复用已有的 `state.Store` 接口（`m.sessions.store` 不可直接访问），需要在 Manager 上单独注入一个 `state.Store`。

- [ ] **Step 1: 在 `Manager` 结构体中新增字段**

在 `internal/sandbox/manager.go` 的 `Manager` struct 中，在 `sessions` 字段后面加一行：

```go
multipartStore state.Store // optional, for multipart upload state
```

同时在 import 中确认已有 `"github.com/goairix/sandbox/internal/storage/state"`（已存在，无需新增）。

- [ ] **Step 2: 新增 SetMultipartStore 方法**

在 `SetSessionStore` 方法后面添加：

```go
// SetMultipartStore sets the state.Store used for multipart upload state.
func (m *Manager) SetMultipartStore(s state.Store) {
	m.multipartStore = s
}
```

- [ ] **Step 3: 新增 multipartUploadState 内部类型**

在 `manager.go` 文件顶部常量区附近添加：

```go
const multipartKeyPrefix = "sandbox:multipart:"
const multipartTTL = 24 * time.Hour

type multipartUploadState struct {
	UploadID       string    `json:"upload_id"`
	SandboxID      string    `json:"sandbox_id"`
	DestPath       string    `json:"dest_path"`
	TotalChunks    int       `json:"total_chunks"`
	ReceivedChunks int       `json:"received_chunks"`
	CreatedAt      time.Time `json:"created_at"`
}
```

- [ ] **Step 4: 新增 multipartKey 辅助函数**

```go
func multipartKey(sandboxID, uploadID string) string {
	return multipartKeyPrefix + sandboxID + ":" + uploadID
}
```

- [ ] **Step 5: 确认编译通过**

```bash
go build ./internal/sandbox/...
```
Expected: 无输出，exit 0

- [ ] **Step 6: Commit**

```bash
git add internal/sandbox/manager.go
git commit -m "feat(sandbox): add multipartStore field and state types to Manager"
```

---

## Task 3: Manager.InitMultipartUpload

**Files:**
- Modify: `internal/sandbox/manager.go`

- [ ] **Step 1: 添加 InitMultipartUpload 方法**

```go
// InitMultipartUpload initialises a multipart upload session.
// It creates the staging directory in the container and persists state to Redis.
func (m *Manager) InitMultipartUpload(ctx context.Context, sandboxID, destPath string, totalChunks int) (string, error) {
	sb, err := m.resolve(ctx, sandboxID)
	if err != nil {
		return "", err
	}

	uploadID := uuid.New().String()
	stagingDir := "/tmp/.uploads/" + uploadID

	if _, err := m.runtime.Exec(ctx, sb.RuntimeID, runtime.ExecRequest{
		Command: "mkdir -p " + stagingDir,
		Timeout: 10,
	}); err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}

	st := multipartUploadState{
		UploadID:    uploadID,
		SandboxID:   sandboxID,
		DestPath:    destPath,
		TotalChunks: totalChunks,
		CreatedAt:   time.Now(),
	}
	data, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("marshal multipart state: %w", err)
	}
	if err := m.multipartStore.Set(ctx, multipartKey(sandboxID, uploadID), data, multipartTTL); err != nil {
		return "", fmt.Errorf("save multipart state: %w", err)
	}
	return uploadID, nil
}
```

确认 import 中有 `"encoding/json"`（已有）和 `"github.com/google/uuid"`（需要新增到 import）。

- [ ] **Step 2: 确认编译通过**

```bash
go build ./internal/sandbox/...
```
Expected: 无输出，exit 0

- [ ] **Step 3: Commit**

```bash
git add internal/sandbox/manager.go
git commit -m "feat(sandbox): add Manager.InitMultipartUpload"
```

---

## Task 4: Manager.UploadChunk

**Files:**
- Modify: `internal/sandbox/manager.go`

- [ ] **Step 1: 添加 UploadChunk 方法**

```go
// UploadChunk writes a single chunk to the container staging directory.
// Chunks must be uploaded in order: chunk_index must equal ReceivedChunks.
func (m *Manager) UploadChunk(ctx context.Context, sandboxID, uploadID string, chunkIndex int, reader io.Reader) (received int, total int, err error) {
	st, err := m.loadMultipartState(ctx, sandboxID, uploadID)
	if err != nil {
		return 0, 0, err
	}
	if chunkIndex != st.ReceivedChunks {
		return 0, 0, fmt.Errorf("expected chunk_index %d, got %d", st.ReceivedChunks, chunkIndex)
	}

	sb, err := m.resolve(ctx, sandboxID)
	if err != nil {
		return 0, 0, err
	}

	chunkPath := fmt.Sprintf("/tmp/.uploads/%s/%d", uploadID, chunkIndex)
	if err := m.runtime.UploadFile(ctx, sb.RuntimeID, chunkPath, reader); err != nil {
		return 0, 0, fmt.Errorf("upload chunk: %w", err)
	}

	st.ReceivedChunks++
	if err := m.saveMultipartState(ctx, sandboxID, uploadID, st); err != nil {
		return 0, 0, err
	}
	return st.ReceivedChunks, st.TotalChunks, nil
}
```

- [ ] **Step 2: 添加 loadMultipartState / saveMultipartState 辅助方法**

```go
func (m *Manager) loadMultipartState(ctx context.Context, sandboxID, uploadID string) (*multipartUploadState, error) {
	data, err := m.multipartStore.Get(ctx, multipartKey(sandboxID, uploadID))
	if err != nil {
		return nil, fmt.Errorf("get multipart state: %w", err)
	}
	if data == nil {
		return nil, fmt.Errorf("upload not found: %s", uploadID)
	}
	var st multipartUploadState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("unmarshal multipart state: %w", err)
	}
	return &st, nil
}

func (m *Manager) saveMultipartState(ctx context.Context, sandboxID, uploadID string, st *multipartUploadState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal multipart state: %w", err)
	}
	return m.multipartStore.Set(ctx, multipartKey(sandboxID, uploadID), data, multipartTTL)
}
```

- [ ] **Step 3: 确认编译通过**

```bash
go build ./internal/sandbox/...
```
Expected: 无输出，exit 0

- [ ] **Step 4: Commit**

```bash
git add internal/sandbox/manager.go
git commit -m "feat(sandbox): add Manager.UploadChunk"
```

---

## Task 5: Manager.GetMultipartStatus / CompleteMultipartUpload / CancelMultipartUpload

**Files:**
- Modify: `internal/sandbox/manager.go`

- [ ] **Step 1: 添加 GetMultipartStatus**

```go
// GetMultipartStatus returns the current state of a multipart upload.
func (m *Manager) GetMultipartStatus(ctx context.Context, sandboxID, uploadID string) (*multipartUploadState, error) {
	return m.loadMultipartState(ctx, sandboxID, uploadID)
}
```

- [ ] **Step 2: 添加 CompleteMultipartUpload**

```go
// CompleteMultipartUpload merges all chunks into the destination path.
// Returns the final file size in bytes.
func (m *Manager) CompleteMultipartUpload(ctx context.Context, sandboxID, uploadID string) (destPath string, size int64, err error) {
	st, err := m.loadMultipartState(ctx, sandboxID, uploadID)
	if err != nil {
		return "", 0, err
	}
	if st.ReceivedChunks != st.TotalChunks {
		return "", 0, fmt.Errorf("incomplete upload: received %d of %d chunks", st.ReceivedChunks, st.TotalChunks)
	}

	sb, err := m.resolve(ctx, sandboxID)
	if err != nil {
		return "", 0, err
	}

	// Build: cat /tmp/.uploads/{id}/0 /tmp/.uploads/{id}/1 ... > destPath
	parts := make([]string, st.TotalChunks)
	for i := 0; i < st.TotalChunks; i++ {
		parts[i] = fmt.Sprintf("/tmp/.uploads/%s/%d", uploadID, i)
	}
	catCmd := "cat " + strings.Join(parts, " ") + " > " + st.DestPath
	if _, err := m.runtime.Exec(ctx, sb.RuntimeID, runtime.ExecRequest{
		Command: catCmd,
		Timeout: 120,
	}); err != nil {
		return "", 0, fmt.Errorf("merge chunks: %w", err)
	}

	// Get file size via stat
	statResult, err := m.runtime.Exec(ctx, sb.RuntimeID, runtime.ExecRequest{
		Command: "stat -c %s " + st.DestPath,
		Timeout: 10,
	})
	if err == nil {
		fmt.Sscanf(strings.TrimSpace(statResult.Stdout), "%d", &size)
	}

	// Cleanup staging dir
	_, _ = m.runtime.Exec(ctx, sb.RuntimeID, runtime.ExecRequest{
		Command: "rm -rf /tmp/.uploads/" + uploadID,
		Timeout: 10,
	})
	_ = m.multipartStore.Delete(ctx, multipartKey(sandboxID, uploadID))

	return st.DestPath, size, nil
}
```

- [ ] **Step 3: 添加 CancelMultipartUpload**

```go
// CancelMultipartUpload removes staging files and Redis state.
func (m *Manager) CancelMultipartUpload(ctx context.Context, sandboxID, uploadID string) error {
	st, err := m.loadMultipartState(ctx, sandboxID, uploadID)
	if err != nil {
		return err
	}

	sb, err := m.resolve(ctx, sandboxID)
	if err != nil {
		return err
	}
	_ = st // validated ownership via loadMultipartState

	_, _ = m.runtime.Exec(ctx, sb.RuntimeID, runtime.ExecRequest{
		Command: "rm -rf /tmp/.uploads/" + uploadID,
		Timeout: 10,
	})
	return m.multipartStore.Delete(ctx, multipartKey(sandboxID, uploadID))
}
```

- [ ] **Step 4: 确认编译通过**

```bash
go build ./internal/sandbox/...
```
Expected: 无输出，exit 0

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/manager.go
git commit -m "feat(sandbox): add GetMultipartStatus, CompleteMultipartUpload, CancelMultipartUpload"
```

---

## Task 6: Handler 层 — 五个新 handler

**Files:**
- Modify: `internal/api/handler/file.go`

- [ ] **Step 1: 添加 InitMultipartUpload handler**

在 `file.go` 末尾追加：

```go
func (h *Handler) InitMultipartUpload(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.InitMultipartUpload")
	defer span.End()

	id := c.Param("id")

	var req types.MultipartInitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}
	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	uploadID, err := h.manager.InitMultipartUpload(spanCtx, id, req.Path, req.TotalChunks)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, types.MultipartInitResponse{UploadID: uploadID})
}
```

- [ ] **Step 2: 添加 UploadChunk handler**

```go
func (h *Handler) UploadChunk(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.UploadChunk")
	defer span.End()

	id := c.Param("id")
	uploadID := c.PostForm("upload_id")
	if uploadID == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "upload_id is required"})
		return
	}
	chunkIndexStr := c.PostForm("chunk_index")
	var chunkIndex int
	if _, err := fmt.Sscanf(chunkIndexStr, "%d", &chunkIndex); err != nil || chunkIndex < 0 {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "chunk_index must be a non-negative integer"})
		return
	}

	file, _, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "file is required"})
		return
	}
	defer file.Close()

	received, total, err := h.manager.UploadChunk(spanCtx, id, uploadID, chunkIndex, file)
	if err != nil {
		if strings.Contains(err.Error(), "upload not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		if strings.Contains(err.Error(), "expected chunk_index") {
			c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
			return
		}
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, types.MultipartChunkResponse{Received: received, Total: total})
}
```

- [ ] **Step 3: 添加 GetMultipartStatus handler**

```go
func (h *Handler) GetMultipartStatus(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.GetMultipartStatus")
	defer span.End()

	id := c.Param("id")
	uploadID := c.Query("upload_id")
	if uploadID == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "upload_id is required"})
		return
	}

	st, err := h.manager.GetMultipartStatus(spanCtx, id, uploadID)
	if err != nil {
		if strings.Contains(err.Error(), "upload not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, types.MultipartStatusResponse{
		UploadID:       st.UploadID,
		DestPath:       st.DestPath,
		TotalChunks:    st.TotalChunks,
		ReceivedChunks: st.ReceivedChunks,
		CreatedAt:      st.CreatedAt,
	})
}
```

- [ ] **Step 4: 添加 CompleteMultipartUpload handler**

```go
func (h *Handler) CompleteMultipartUpload(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.CompleteMultipartUpload")
	defer span.End()

	id := c.Param("id")

	var req types.MultipartCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	path, size, err := h.manager.CompleteMultipartUpload(spanCtx, id, req.UploadID)
	if err != nil {
		if strings.Contains(err.Error(), "upload not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		if strings.Contains(err.Error(), "incomplete upload") {
			c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
			return
		}
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, types.MultipartCompleteResponse{Path: path, Size: size})
}
```

- [ ] **Step 5: 添加 CancelMultipartUpload handler**

```go
func (h *Handler) CancelMultipartUpload(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.CancelMultipartUpload")
	defer span.End()

	id := c.Param("id")

	var req types.MultipartCancelRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := h.manager.CancelMultipartUpload(spanCtx, id, req.UploadID); err != nil {
		if strings.Contains(err.Error(), "upload not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		internalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
```

- [ ] **Step 6: 确认编译通过**

```bash
go build ./internal/api/...
```
Expected: 无输出，exit 0

- [ ] **Step 7: Commit**

```bash
git add internal/api/handler/file.go
git commit -m "feat(handler): add multipart upload handlers"
```

---

## Task 7: 注册路由 + 注入 multipartStore

**Files:**
- Modify: `internal/api/router.go`
- Modify: `cmd/sandbox/main.go`（注入 multipartStore）

- [ ] **Step 1: 在 `router.go` 的文件操作路由块末尾注册五条新路由**

在 `v1.POST("/sandboxes/:id/files/edit-lines", h.EditFileLines)` 后面添加：

```go
// Multipart upload
v1.POST("/sandboxes/:id/files/upload/init", h.InitMultipartUpload)
v1.POST("/sandboxes/:id/files/upload/chunk", h.UploadChunk)
v1.GET("/sandboxes/:id/files/upload/status", h.GetMultipartStatus)
v1.POST("/sandboxes/:id/files/upload/complete", h.CompleteMultipartUpload)
v1.DELETE("/sandboxes/:id/files/upload/cancel", h.CancelMultipartUpload)
```

- [ ] **Step 2: 在 `cmd/sandbox/main.go` 中找到 Manager 初始化位置，注入 multipartStore**

找到调用 `m.SetSessionStore(...)` 的地方，在其后添加：

```go
if redisStore != nil {
    m.SetMultipartStore(redisStore)
}
```

其中 `redisStore` 是已有的 `state.Store` 实例（与 SessionStore 共用同一个 Redis 连接）。

- [ ] **Step 3: 确认整体编译通过**

```bash
go build ./...
```
Expected: 无输出，exit 0

- [ ] **Step 4: 运行现有测试，确认无回归**

```bash
go test ./...
```
Expected: 全部 PASS，无 FAIL

- [ ] **Step 5: Commit**

```bash
git add internal/api/router.go cmd/sandbox/main.go
git commit -m "feat(router): register multipart upload routes and wire multipartStore"
```

---

## Task 8: 集成测试

**Files:**
- Modify: `internal/sandbox/manager_test.go`（或新建 `internal/sandbox/multipart_test.go`）

- [ ] **Step 1: 写 InitMultipartUpload 测试**

在 `internal/sandbox/` 下新建 `multipart_test.go`：

```go
package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitMultipartUpload(t *testing.T) {
	rt := newMockRuntime()
	m := newTestManager(t, rt)
	m.SetMultipartStore(newInMemoryStore())

	sb := createTestSandbox(t, m)

	uploadID, err := m.InitMultipartUpload(context.Background(), sb.ID, "/workspace/big.bin", 3)
	require.NoError(t, err)
	assert.NotEmpty(t, uploadID)

	// staging dir should have been created
	assert.True(t, rt.execCalled("/tmp/.uploads/"+uploadID))
}
```

注：`newInMemoryStore()` 是一个简单的 `state.Store` 内存实现，用于测试（见 Step 2）。`newTestManager` 和 `createTestSandbox` 复用 `manager_test.go` 中已有的辅助函数。

- [ ] **Step 2: 新增 inMemoryStore 测试辅助**

在 `multipart_test.go` 中添加：

```go
type inMemoryStore struct {
	data map[string][]byte
}

func newInMemoryStore() *inMemoryStore {
	return &inMemoryStore{data: make(map[string][]byte)}
}

func (s *inMemoryStore) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	s.data[key] = value
	return nil
}
func (s *inMemoryStore) Get(_ context.Context, key string) ([]byte, error) {
	return s.data[key], nil
}
func (s *inMemoryStore) Delete(_ context.Context, key string) error {
	delete(s.data, key)
	return nil
}
func (s *inMemoryStore) Exists(_ context.Context, key string) (bool, error) {
	_, ok := s.data[key]
	return ok, nil
}
func (s *inMemoryStore) SetNX(_ context.Context, key string, value []byte, _ time.Duration) (bool, error) {
	if _, ok := s.data[key]; ok {
		return false, nil
	}
	s.data[key] = value
	return true, nil
}
func (s *inMemoryStore) Keys(_ context.Context, pattern string) ([]string, error) {
	return nil, nil
}
```

- [ ] **Step 3: 写 UploadChunk 顺序校验测试**

```go
func TestUploadChunk_OutOfOrder(t *testing.T) {
	rt := newMockRuntime()
	m := newTestManager(t, rt)
	m.SetMultipartStore(newInMemoryStore())

	sb := createTestSandbox(t, m)
	uploadID, _ := m.InitMultipartUpload(context.Background(), sb.ID, "/workspace/big.bin", 3)

	// chunk_index=1 before chunk_index=0 should fail
	_, _, err := m.UploadChunk(context.Background(), sb.ID, uploadID, 1, strings.NewReader("data"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected chunk_index 0")
}
```

- [ ] **Step 4: 写 CompleteMultipartUpload 未完成时报错测试**

```go
func TestCompleteMultipartUpload_Incomplete(t *testing.T) {
	rt := newMockRuntime()
	m := newTestManager(t, rt)
	m.SetMultipartStore(newInMemoryStore())

	sb := createTestSandbox(t, m)
	uploadID, _ := m.InitMultipartUpload(context.Background(), sb.ID, "/workspace/big.bin", 3)

	_, _, err := m.CompleteMultipartUpload(context.Background(), sb.ID, uploadID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete upload")
}
```

- [ ] **Step 5: 运行新测试**

```bash
go test ./internal/sandbox/... -run TestInitMultipartUpload -v
go test ./internal/sandbox/... -run TestUploadChunk_OutOfOrder -v
go test ./internal/sandbox/... -run TestCompleteMultipartUpload_Incomplete -v
```
Expected: 全部 PASS

- [ ] **Step 6: 运行全量测试**

```bash
go test ./...
```
Expected: 全部 PASS

- [ ] **Step 7: Commit**

```bash
git add internal/sandbox/multipart_test.go
git commit -m "test(sandbox): add multipart upload unit tests"
```
