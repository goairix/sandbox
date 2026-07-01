# Workspace Sync 性能优化

**日期：** 2026-07-01  
**状态：** 草案

## 背景

当前 workspace 同步（`syncToContainer` / `syncFromContainer`）存在三个明确的性能与可靠性问题：

1. **OOM 风险**：`writeTarStream` 对每个文件调用 `io.ReadAll`，8个并发 goroutine 同时把文件整体读入内存。10 个 100MB 文件即造成 ~800MB 峰值占用，而 `fileEntry.size` 在 `collectFiles` 阶段已有，根本不需要先读内容。
2. **全量下载浪费**：`syncFromContainer` 不管 changedSet 大小，始终调用 `DownloadDir` 下载整个 `/workspace` tar——改了1个文件也要下载整个目录。
3. **存储写回串行**：`downloadChangedFiles` 对 storage 的写操作(`Create → io.Copy → Close`)完全串行，未利用对象存储的并发能力。

本方案分三层递进优化，每层独立可交付，不互相依赖。

## 约束

- 改动范围限于 `internal/sandbox/workspace.go` 及相关文件，不改公共接口
- 保持现有 sync 语义（mtime 增量、排除列表、bind-mount 短路）不变
- 第三层（init container）需要扩展 `runtime.SandboxSpec`，是较大架构变更

## 优化一：writeTarStream 流式化（消灭 OOM）

### 问题代码

```go
// workspace.go:338 — 整个文件内容进内存
data, err := io.ReadAll(reader)
ch <- readResult{content: data}
```

### 方案：并发预取 reader，顺序流式写 tar

信号量由"限制同时在内存的文件内容"改为"限制同时打开的存储连接数"。goroutine 只负责打开 reader（建立 HTTP 连接，快速），主循环负责 `io.Copy`（消费），消费完再通知 goroutine 释放信号量槽位。

```go
type readResult struct {
    reader io.ReadCloser
    size   int64        // 直接用 entry.size，不再 io.ReadAll
    done   chan struct{} // 主循环 io.Copy 完成后 close，goroutine 才释放 sem
    err    error
}

go func(entry fileEntry, ch chan<- readResult) {
    sem <- struct{}{}
    reader, err := scoped.Open(ctx, entry.relPath)
    if err != nil {
        <-sem
        ch <- readResult{err: ...}
        return
    }
    done := make(chan struct{})
    ch <- readResult{reader: reader, size: entry.size, done: done}
    <-done  // 等主循环消费完
    <-sem   // 消费完才释放；连接在 Close() 时已归还
}(e, ch)

// 主循环
tw.WriteHeader(&tar.Header{Size: res.size, ...}) // 用预知 size
io.Copy(tw, res.reader)                          // 流式，不缓冲
res.reader.Close()
close(res.done)
```

**注意事项：**

1. **`entry.size` 与实际内容不符（极罕见）**：需区分两种情况：
   - 实际字节数 < size：`io.Copy` 提前结束，`tw.Close()` 或下一个 `WriteHeader` 时报错 → 可检测，调用方正常处理
   - 实际字节数 > size：`io.Copy` 继续写超出 header 声明的长度，tar 格式**静默损坏**且不立即报错 → 后续 entry 会被污染
   
   S3/OSS 的 Content-Length 是强一致的，实际中不会发生。此处记录是为了说明为何不加额外校验（如加校验则需在 `io.Copy` 外层计数写入字节数，与 size 对比）。

2. **ctx 取消时 `io.Copy` 的行为**：取决于底层 reader 是否感知 context cancellation。S3 SDK（aws-sdk-go-v2 / aliyun-oss-go-sdk 等）的 object reader 均感知 ctx，取消时 `io.Copy` 会立即返回错误，进入正常错误路径。**实现时必须确保 `scoped.Open` 返回的 reader 透传 ctx 给底层 HTTP 请求**，否则取消信号无法传递，`io.Copy` 会阻塞到文件传完。

3. **cleanup 的链式解锁行为**：cleanup 顺序处理各 channel：`<-resultChs[j]` 可能阻塞，等待还在排队 `sem <- struct{}{}` 的 goroutine 发送结果。但每次 `close(res.done)` 都会释放一个 sem 槽位，从而解锁下一个等待的 goroutine，形成链式传导。cleanup 只在**错误路径**执行，正常路径不受影响。

### 内存收益

| 场景 | 优化前 | 优化后 |
|------|--------|--------|
| 8 并发，每文件 50 MB | ~400 MB | ~256 KB（8 × 32 KB HTTP 缓冲）|
| 10000 个小文件 | 不变（由最大文件决定）| 不变，稳定 |

---

## 优化二：syncFromContainer 差异化路径 + 并发写回

### 阈值常量

```go
// 变更文件数 ≤ 此值时走逐文件下载，否则走 tar
const singleFileDownloadThreshold = 5
// 并发写回 storage 的槽位数
const maxConcurrentStorageWrites = 4
```

### 入口分发

```go
func (m *Manager) downloadChangedFiles(
    ctx context.Context,
    scoped storage.ScopedFS,
    runtimeID string,
    changedSet map[string]struct{},
    exclude []string,
) error {
    if len(changedSet) <= singleFileDownloadThreshold {
        return m.downloadFilesDirect(ctx, scoped, runtimeID, changedSet)
    }
    return m.downloadFilesViaTar(ctx, scoped, runtimeID, changedSet, exclude)
}
```

### 小变更路径：downloadFilesDirect

逐文件调用 `runtime.ReadFileContent`（已有接口），并发 4 路写回 storage：

```go
func (m *Manager) downloadFilesDirect(
    ctx context.Context,
    scoped storage.ScopedFS,
    runtimeID string,
    changedSet map[string]struct{},
) error {
    type job struct {
        path   string
        reader io.ReadCloser
    }

    // 并发打开所有文件（数量 ≤ threshold，无需额外信号量）
    jobs := make(chan job, len(changedSet))
    var wg sync.WaitGroup
    errCh := make(chan error, len(changedSet))

    for path := range changedSet {
        path := path
        wg.Add(1)
        go func() {
            defer wg.Done()
            rc, err := m.runtime.ReadFileContent(ctx, runtimeID, "/workspace/"+path)
            if err != nil {
                errCh <- fmt.Errorf("read %q: %w", path, err)
                return
            }
            jobs <- job{path: path, reader: rc}
        }()
    }
    wg.Wait()
    close(jobs)
    close(errCh)

    for err := range errCh {
        return err
    }

    // 并发写回 storage
    sem := make(chan struct{}, maxConcurrentStorageWrites)
    var writeWg sync.WaitGroup
    writeErrs := make(chan error, len(changedSet))

    for j := range jobs {
        j := j
        sem <- struct{}{}
        writeWg.Add(1)
        go func() {
            defer writeWg.Done()
            defer func() { <-sem }()
            defer j.reader.Close()
            w, err := scoped.Create(ctx, j.path, contentTypeOpt(j.path))
            if err != nil {
                writeErrs <- fmt.Errorf("create %q: %w", j.path, err)
                return
            }
            if _, err := io.Copy(w, j.reader); err != nil {
                w.Close()
                writeErrs <- fmt.Errorf("write %q: %w", j.path, err)
                return
            }
            if err := w.Close(); err != nil {
                writeErrs <- fmt.Errorf("flush %q: %w", j.path, err)
            }
        }()
    }
    writeWg.Wait()
    close(writeErrs)

    for err := range writeErrs {
        return err
    }
    return nil
}
```

### 大变更路径：downloadFilesViaTar（并发写回）

tar 格式必须顺序读，所以对不在 changedSet 里的条目必须 `io.Discard`。对在 changedSet 里的条目，一次只缓冲**单个文件**，然后并发写回：

```go
func (m *Manager) downloadFilesViaTar(
    ctx context.Context,
    scoped storage.ScopedFS,
    runtimeID string,
    changedSet map[string]struct{},
    exclude []string,
) error {
    tarReader, err := m.runtime.DownloadDir(ctx, runtimeID, "/workspace")
    if err != nil {
        return fmt.Errorf("download workspace: %w", err)
    }
    defer tarReader.Close()

    sem := make(chan struct{}, maxConcurrentStorageWrites)
    var wg sync.WaitGroup
    writeErrs := make(chan error, len(changedSet))

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
        if name == "" || isExcluded(name, exclude) || hdr.Typeflag == tar.TypeDir {
            if hdr.Typeflag == tar.TypeDir && name != "" {
                _ = scoped.MakeDir(ctx, strings.TrimRight(name, "/"), 0755)
            }
            continue
        }

        if _, changed := changedSet[name]; !changed {
            // 未变更：必须排空当前 entry，否则 tar reader 偏移出错
            if _, err := io.Copy(io.Discard, tr); err != nil {
                return fmt.Errorf("discard tar entry %q: %w", name, err)
            }
            continue
        }

        // 只缓冲单个文件，不是整个 workspace
        buf, err := io.ReadAll(tr)
        if err != nil {
            return fmt.Errorf("read tar content %q: %w", name, err)
        }

        sem <- struct{}{}
        wg.Add(1)
        name, buf := name, buf // capture
        go func() {
            defer wg.Done()
            defer func() { <-sem }()
            w, err := scoped.Create(ctx, name, contentTypeOpt(name))
            if err != nil {
                writeErrs <- fmt.Errorf("create %q: %w", name, err)
                return
            }
            if _, err := w.Write(buf); err != nil {
                w.Close()
                writeErrs <- fmt.Errorf("write %q: %w", name, err)
                return
            }
            if err := w.Close(); err != nil {
                writeErrs <- fmt.Errorf("flush %q: %w", name, err)
            }
        }()
    }

    wg.Wait()
    close(writeErrs)
    for err := range writeErrs {
        return err
    }
    return nil
}
```

### 收益

| 场景 | 优化前 | 优化后 |
|------|--------|--------|
| 1 个文件变更 | 下载全量 workspace tar（如 500 MB）| 只下载该文件 |
| 5 个文件变更 | 同上 | 5 次 ReadFileContent，并发写回 |
| 100 个文件变更 | 下载全量 tar，串行写回 | 下载全量 tar，并发 4 路写回 |

---

## 优化三：Init Container 方案（移除 API 进程数据路径）

> **范围说明**：此层改动较大（新增镜像、K8s Secret、扩展 SandboxSpec），建议作为独立阶段实施。优化一、二完成后再推进。

### 核心思路

把 `syncToContainer`（storage → container）搬出 API 进程，用一个 **sync init container** 直连对象存储拉文件到 emptyDir，API 进程不经手任何文件字节。

```
Pod 启动顺序：
  init container: workspace-sync
      直连对象存储 → 把 workspace 文件拉到 emptyDir
      完成（exit 0）→ 主容器才启动
      API 进程文件字节流量 = 0

  main container: sandbox
      /workspace 已就绪，直接用
```

### 数据模型变更

**`internal/runtime/types.go` — SandboxSpec 新增字段：**

```go
type SandboxSpec struct {
    // ...现有字段...
    WorkspaceSync *WorkspaceSyncSpec // 非 nil 时创建 sync init container
}

// WorkspaceSyncSpec 描述 init container 如何从对象存储拉取 workspace。
type WorkspaceSyncSpec struct {
    RootPath    string   // 对象存储路径前缀
    Provider    string   // "s3" | "oss" | "cos" | "obs" | "minio"
    Endpoint    string
    Bucket      string
    SecretRef   string   // K8s Secret name，含 access_key / secret_key
    SyncExclude []string
}
```

**`internal/sandbox/types.go` — WorkspaceInfo 新增字段：**

```go
type WorkspaceInfo struct {
    // ...现有字段...
    SyncMode string `json:"sync_mode,omitempty"` // "api" | "init_container"
}
```

### pod.go — 注入 sync init container

```go
if ws := spec.WorkspaceSync; ws != nil {
    pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
        Name:  "workspace-sync",
        Image: syncContainerImage, // 精简镜像，内置 rclone 或自研 sync 二进制
        Command: []string{
            "/bin/workspace-sync",
            "--provider=" + ws.Provider,
            "--endpoint=" + ws.Endpoint,
            "--bucket=" + ws.Bucket,
            "--src=" + ws.RootPath,
            "--dst=/workspace",
            "--exclude=" + strings.Join(ws.SyncExclude, ","),
        },
        EnvFrom: []corev1.EnvFromSource{{
            SecretRef: &corev1.SecretEnvSource{
                LocalObjectReference: corev1.LocalObjectReference{Name: ws.SecretRef},
            },
        }},
        VolumeMounts: []corev1.VolumeMount{
            {Name: "workspace", MountPath: "/workspace"},
        },
        SecurityContext: &corev1.SecurityContext{
            AllowPrivilegeEscalation: &falseVal,
            RunAsUser:                &spec.RunAsUser,
        },
    })
}
```

### manager.go — buildSpec 与 MountWorkspace 联动

`buildSpec` 按 provider 填充 `WorkspaceSync`：

```go
if cfg.WorkspacePath != "" &&
    m.fsMeta != nil &&
    m.fsMeta.Provider != storage.ProviderLocal &&
    m.config.Workspace.SyncSecretRef != "" {
    spec.WorkspaceSync = &runtime.WorkspaceSyncSpec{
        RootPath:    cfg.WorkspacePath,
        Provider:    string(m.fsMeta.Provider),
        Endpoint:    m.fsMeta.Endpoint,
        Bucket:      m.fsMeta.Bucket,
        SecretRef:   m.config.Workspace.SyncSecretRef,
        SyncExclude: cfg.WorkspaceSyncExclude,
    }
}
```

`MountWorkspace` 检测到 init container 已同步时跳过 `syncToContainer`：

```go
initSynced := spec.WorkspaceSync != nil // pod 已通过 init container 完成同步
if !initSynced {
    if err := m.syncToContainer(ctx, scoped, runtimeID); err != nil {
        return fmt.Errorf("sync to container: %w", err)
    }
}
```

### 新增配置项

```go
type WorkspaceConfig struct {
    // ...现有字段...
    SyncSecretRef string `mapstructure:"sync_secret_ref"` // 填写则启用 init container 模式
    SyncImage     string `mapstructure:"sync_image"`      // sync init container 镜像
}
```

```yaml
workspace:
  sync_secret_ref: "sandbox-storage-secret"
  sync_image: "registry.example.com/workspace-sync:1.0"
```

### 收益

| 场景 | 优化前 | 优化后 |
|------|--------|--------|
| K8s，workspace 1000 个文件 | API 进程全量 tar 传输 | API 进程文件流量 = 0 |
| pod crash 后恢复 | `restorePersistentSandboxes` 全量 syncToContainer | pod 重建时 init container 自动重拉 |
| API OOM 风险 | 存在（受文件总大小影响）| 消除（数据不过 API 进程）|

---

## 变更范围

| 文件 | 变更 | 所属层 |
|------|------|--------|
| `internal/sandbox/workspace.go` | `writeTarStream` 改为流式；新增 `downloadFilesDirect`、`downloadFilesViaTar`；常量 `singleFileDownloadThreshold`、`maxConcurrentStorageWrites` | 优化一、二 |
| `internal/runtime/types.go` | `SandboxSpec` 新增 `WorkspaceSync *WorkspaceSyncSpec`；新增 `WorkspaceSyncSpec` 类型 | 优化三 |
| `internal/sandbox/types.go` | `WorkspaceInfo` 新增 `SyncMode` | 优化三 |
| `internal/config/config.go` | `WorkspaceConfig` 新增 `SyncSecretRef`、`SyncImage` | 优化三 |
| `internal/sandbox/manager.go` | `buildSpec` 填充 `WorkspaceSync`；`MountWorkspace` 检测 init container 已同步时跳过 `syncToContainer` | 优化三 |
| `internal/runtime/kubernetes/pod.go` | 按 `WorkspaceSync` 注入 sync init container | 优化三 |
| `internal/runtime/kubernetes/exec.go` | **无改动** | — |
| `configs/config.yaml` | 新增 `workspace.sync_secret_ref`、`workspace.sync_image` | 优化三 |
| sync 镜像 Dockerfile | 新增（内置 rclone 或自研二进制，精简） | 优化三 |

## 错误处理

| 场景 | 处理 |
|------|------|
| `entry.size` 与实际内容不符（极罕见的 S3 不一致）| `tar.Writer` 返回错误，`writeTarStream` 向上透传，sandbox 创建失败 |
| `ReadFileContent` 超时（小变更路径）| 单文件 error 返回，整个 `downloadFilesDirect` 中止 |
| 大变更路径下 `io.Discard` 失败 | 返回错误，中止整个 syncFromContainer |
| init container 拉取失败（凭证错/网络断）| pod init container 失败 → `waitForPodReady` 超时 → sandbox 创建返回 500 |
| init container 拉取超时 | 同上 |

## 不在范围内

- `collectFiles` / `containerFileManifest` 的性能优化（当前 `find` 命令在大仓库下也较慢，可作为后续方向）
- `autoSync` 定时任务的频率/并发控制
- sync init container 镜像的构建与发布流程
- Docker 运行时（init container 是 K8s 原生概念）

## 实施顺序建议

1. **优化一**（`writeTarStream` 流式化）——改动集中一个函数，风险最低，直接消灭 OOM，**优先实施**
2. **优化二**（差异化下载 + 并发写回）——新增两个函数，替换原 `downloadChangedFiles`，逻辑清晰，**紧跟实施**
3. **优化三**（init container）——架构变动最大，建议在优化一、二稳定后独立排期
