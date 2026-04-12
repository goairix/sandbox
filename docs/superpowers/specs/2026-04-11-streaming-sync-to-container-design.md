# syncToContainer 流式管道优化设计

## 背景

`MountWorkspace` 初始化时调用 `syncToContainer` 将 storage 中的所有文件同步到容器。当工作区文件数量多或单文件体积大时，初始化明显变慢。

### 当前实现的瓶颈

当前 `syncToContainer` 分三个串行阶段：

1. `collectFiles()` — 递归遍历目录树，收集元数据（快，无问题）
2. `readFilesConcurrent()` — 8 并发将所有文件内容读入 `fileEntry.content []byte`
3. 遍历 entries 构建 tar 到 `bytes.Buffer`，最后调用 `UploadArchive(&buf)`

问题：
- 峰值内存约为文件总大小的 2 倍（fileEntry.content + bytes.Buffer）
- 上传必须等所有文件读完 + tar 构建完成后才开始
- 读取、构建、上传三个阶段完全串行，无 I/O 重叠

### 目标

- 消除内存双缓冲，峰值内存从 ~2x 文件总大小降至 O(io.Copy buffer × 并发数)，约 256KB 级别
- 读取、tar 构建、上传三者并行，首字节上传延迟从"全部完成后"降至"第一个文件读完即开始"
- 改动范围限制在 `internal/sandbox/workspace.go` 单文件

## 方案选型

| 方案 | 描述 | 优点 | 缺点 |
|------|------|------|------|
| A. io.Pipe + 有序预取 | 预取 goroutine 池 + per-entry channel + 顺序消费写 tar | 读取与上传并行，延迟隐藏好 | 实现稍复杂，需 cleanup 逻辑 |
| B. io.Pipe + 顺序读取 | writer 逐个 Open → Copy → Close | 最简单 | 文件间串行读取，云存储延迟敏感 |
| C. Channel 生产消费 | bounded channel 传递 {header, reader} | 概念清晰 | 无法保证 tar entry 顺序 |

选择方案 A：在性能和可控性之间取得最好平衡。

## 架构设计

### 数据流

```
collectFiles(scoped, ".") → []fileEntry{relPath, isDir, size, modTime}
         │
         ▼
   ┌─ prefetch pool (maxConcurrentReads=8) ─┐
   │  entry[0] → ch[0] → io.ReadCloser      │
   │  entry[1] → ch[1] → io.ReadCloser      │
   │  entry[2] → ch[2] → io.ReadCloser      │
   │  ...                                    │
   └─────────────────────────────────────────┘
         │ (按 entry 顺序消费)
         ▼
   writeTarStream(entries, pipeWriter)
   ├─ dir entry  → tw.WriteHeader(TypeDir)
   └─ file entry → <-ch[i] → tw.WriteHeader + io.Copy(tw, reader)
         │
    io.Pipe()
         │
         ▼
   UploadArchive(ctx, runtimeID, "/workspace", pipeReader)
```

### 组件职责

#### `fileEntry` 结构体（修改）

```go
type fileEntry struct {
    relPath string
    isDir   bool
    size    int64     // 新增：从 fi.Size() 获取，用于 tar header
    modTime time.Time
    // content []byte 已移除
}
```

#### `collectFiles()` （微调）

在 append 时补充 `size: fi.Size()`。其余逻辑不变。

#### `syncToContainer()` （重写）

```
1. collectFiles() 收集元数据
2. 如果 entries 为空，直接返回
3. 创建 io.Pipe()
4. 启动 goroutine 调用 UploadArchive(ctx, runtimeID, "/workspace", pipeReader)
5. 调用 writeTarStream(ctx, scoped, entries, pipeWriter)
6. 根据 writeErr 关闭 pipeWriter（CloseWithError 或 Close）
7. 等待 uploadErr，双向错误收集后返回
```

#### `writeTarStream()` （新增，核心方法）

```
1. 创建 tar.Writer(w)
2. 创建信号量 channel（容量 maxConcurrentReads=8）
3. 为每个非目录 entry 启动 prefetch goroutine（Go goroutine 轻量，~2KB 栈，万级文件可接受）：
   - 获取信号量
   - 检查 ctx.Err()
   - scoped.Open(ctx, entry.relPath) → 发送到 per-entry buffered(1) channel
   - 释放信号量
4. 按 entry 顺序遍历：
   - 目录 → tw.WriteHeader(TypeDir)
   - 文件 → <-ch[i] 获取 reader → tw.WriteHeader + io.Copy(tw, reader) → reader.Close()
5. 任何步骤失败 → cleanup() drain 后续所有 channel 并关闭 reader → 返回 error
```

### 错误处理

| 场景 | 行为 |
|------|------|
| 文件读取失败 | writeTarStream 返回 error → pw.CloseWithError(err) → UploadArchive 读到 error 退出 |
| UploadArchive 失败 | pipe reader 关闭 → writer 的 io.Copy 得到 write error → 双向错误收集，返回 upload error |
| Context 取消 | scoped.Open 和 UploadArchive 都尊重 context；prefetch goroutine 在 Open 前检查 ctx.Err() |
| Goroutine 泄漏防护 | 每个 prefetch goroutine 写入 buffered(1) channel，永不阻塞；cleanup() drain 所有未消费 channel |

### 性能对比

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| 峰值内存 | ~2x 文件总大小 | O(io.Copy buffer × 并发数)，约 256KB + tar/pipe 开销 |
| 首字节上传延迟 | 全部读完 + tar 构建后 | 第一个文件读完即开始 |
| I/O 重叠 | 无（串行阶段） | 读取 / tar 构建 / 上传并行 |

说明：`scoped.Open()` 返回 `io.ReadCloser`（云存储为 HTTP response body，本地为 file handle），
不会将文件内容缓冲到内存。数据通过 `io.Copy` 流式传输，每次仅占用默认 32KB buffer。

## 变更范围

所有改动集中在 `internal/sandbox/workspace.go`：

| 变更 | 类型 |
|------|------|
| `fileEntry` 结构体 | 修改：移除 content，添加 size |
| `collectFiles()` | 微调：append 时补充 size |
| `syncToContainer()` | 重写：io.Pipe 流式管道 |
| `writeTarStream()` | 新增：预取 + 有序 tar 写入 |
| `readFilesConcurrent()` | 删除：不再需要 |
| imports | 移除 `"bytes"` 和 `"sync"`（均不再被 workspace.go 中其他代码使用） |

不涉及 runtime 层、storage 层、config 层或 API 层的任何改动。

## 验证方式

- `go build ./...` 编译通过
- `go test ./internal/sandbox/ -count=1` 现有测试通过
- `go vet ./...` 无警告
