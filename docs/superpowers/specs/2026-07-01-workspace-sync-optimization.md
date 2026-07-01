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
   - 实际字节数 > size：Go 的 `archive/tar.Writer` 会在写入超过 header 声明字节数时返回 `ErrWriteTooLong`，`io.Copy` 拿到错误后中止并向上传递 → **不是静默损坏**，可正常检测和处理（P2 修正）

   S3/OSS 的 Content-Length 是强一致的，实际中不会发生。此处记录是为了说明为何不加额外校验。

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
// 可通过配置覆盖，见 WorkspaceConfig.SingleFileDownloadThreshold
const defaultSingleFileDownloadThreshold = 5
// 并发写回 storage 的槽位数（仅小变更路径使用）
const maxConcurrentStorageWrites = 4
```

**P3：阈值依据说明**

`singleFileDownloadThreshold = 5` 是初始保守值，依据如下权衡：

- **K8s `ReadFileContent` 是 exec/cat 流**：每次调用对应一次 SPDY exec 会话建立，固定开销约 50-200ms（取决于 API server 负载）。5 个文件 = 最多 ~1s 额外开销，可接受。
- **tar 下载的固定成本**：`DownloadDir` 需要在容器内执行 tar，有启动 exec 的固定开销，对于极少量文件（1-2个）比逐文件 exec 更重。
- **Docker runtime**：Docker exec 开销更低，阈值可以更高；K8s 开销更高，阈值应更低。

因此阈值设为**可配置**：

```go
type WorkspaceConfig struct {
    // ...现有字段...
    // 小于等于此值时用逐文件下载，0 表示使用默认值（5）
    // Docker 环境可适当调高（10-20）；K8s 建议保持默认
    SingleFileDownloadThreshold int `mapstructure:"single_file_download_threshold"`
}
```

后续建议在真实环境对 K8s 和 Docker 两种 runtime 做 benchmark，测量不同文件数量下两条路径的延迟曲线，再用数据驱动调整默认值。

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

逐文件调用 `runtime.ReadFileContent`（已有接口），并发 4 路写回 storage。

**关键约束**：必须保证任何错误路径都关闭已打开的 reader。`ReadFileContent` 在 K8s 下是 exec/cat 流，未 Close 的流会在后台 drain 完整个文件，泄漏连接和资源。

实现上用 `errgroup.WithContext`（`golang.org/x/sync/errgroup`）管控全流程——第一个错误自动 cancel ctx，后续 Open 和 io.Copy 均感知取消：

```go
func (m *Manager) downloadFilesDirect(
    ctx context.Context,
    scoped storage.ScopedFS,
    runtimeID string,
    changedSet map[string]struct{},
) error {
    // 所有 goroutine 共享可取消 ctx；第一个错误触发取消
    eg, egCtx := errgroup.WithContext(ctx)

    type job struct {
        path   string
        reader io.ReadCloser
    }
    jobs := make(chan job, len(changedSet))

    // 阶段一：并发打开 reader（数量 ≤ threshold，无需额外信号量）
    for path := range changedSet {
        path := path
        eg.Go(func() error {
            rc, err := m.runtime.ReadFileContent(egCtx, runtimeID, "/workspace/"+path)
            if err != nil {
                return fmt.Errorf("read %q: %w", path, err)
            }
            select {
            case jobs <- job{path: path, reader: rc}:
            case <-egCtx.Done():
                rc.Close() // ctx 已取消，立刻关闭，不泄漏
                return egCtx.Err()
            }
            return nil
        })
    }

    // 等阶段一全部完成（含错误），再关闭 jobs channel
    go func() {
        eg.Wait() //nolint:errcheck // 错误通过 eg.Wait() 返回值收集
        close(jobs)
    }()

    // 阶段二：并发写回 storage
    sem := make(chan struct{}, maxConcurrentStorageWrites)
    for j := range jobs {
        j := j
        eg.Go(func() error {
            sem <- struct{}{}
            defer func() { <-sem }()
            defer j.reader.Close()

            w, err := scoped.Create(egCtx, j.path, contentTypeOpt(j.path))
            if err != nil {
                return fmt.Errorf("create %q: %w", j.path, err)
            }
            if _, err := io.Copy(w, j.reader); err != nil {
                w.Close()
                return fmt.Errorf("write %q: %w", j.path, err)
            }
            return w.Close()
        })
    }

    return eg.Wait()
}
```

> **注意**：上面的 `go func() { eg.Wait(); close(jobs) }()` 启动了一个后台 goroutine 来关闭 channel，而主流程通过 `range jobs` 消费然后再调 `eg.Wait()`。这里有一个重入问题——`eg.Wait()` 被调用了两次。实现时应改用两个独立的 errgroup，或用 `sync.WaitGroup` 控制阶段一、`errgroup` 控制阶段二。伪代码仅示意流程，实现时需仔细拆分两个阶段的并发控制。

### 大变更路径：downloadFilesViaTar

tar 格式必须顺序读，对不在 changedSet 里的条目必须 `io.Discard`。

**内存模型修正（P1-b）**：原设计承诺"只缓冲单个文件"，但 `io.ReadAll(tr)` 后立即 `sem <-` 等待槽位，当4个写回 goroutine 都在运行时，第5个文件已完整读入内存。实际峰值为 `(maxConcurrentStorageWrites + 1) × 最大单文件大小`，而非"一个文件"。

因此需明确目标：

- **目标是减少 storage 写回延迟（并发上传）**：接受上述内存代价，峰值有界，对大多数场景可接受。用 `io.ReadAll` 缓冲后并发写回。
- **目标是零额外内存**：改用顺序 `io.Copy` 直接写回，无并发，但无额外内存开销。

**本方案选顺序写回**——原始问题（全量 tar 下载）的核心瓶颈是网络传输，而非 storage 写回的并发度。顺序写回更简单、内存可预期：

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
        if name == "" || isExcluded(name, exclude) {
            continue
        }
        if hdr.Typeflag == tar.TypeDir {
            _ = scoped.MakeDir(ctx, strings.TrimRight(name, "/"), 0755)
            continue
        }

        if _, changed := changedSet[name]; !changed {
            // 未变更：必须排空当前 entry，否则 tar reader 偏移出错
            if _, err := io.Copy(io.Discard, tr); err != nil {
                return fmt.Errorf("discard tar entry %q: %w", name, err)
            }
            continue
        }

        // 顺序流式写回 storage，零额外内存
        w, err := scoped.Create(ctx, name, contentTypeOpt(name))
        if err != nil {
            return fmt.Errorf("create %q: %w", name, err)
        }
        if _, err := io.Copy(w, tr); err != nil {
            w.Close()
            return fmt.Errorf("write %q: %w", name, err)
        }
        if err := w.Close(); err != nil {
            return fmt.Errorf("flush %q: %w", name, err)
        }
    }
    return nil
}
```

> 如果未来 benchmark 表明 storage 写回是瓶颈（而非 tar 下载），可将每个 changed entry 改为 `io.ReadAll` + goroutine 写回，并在 spec 里用 `maxConcurrentStorageWrites` 控制并发，同时在注释里承认 `(N+1) × max_file_size` 的内存代价。当前不做这个优化。

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

### manager.go — buildSpec 与创建路径联动

**P1-c 修正**：init container 必须在 Pod 创建时注入，pool 里的 pod 是预先建好的，事后无法补注入。因此，**启用 init container 时，workspace sandbox 必须强制绕过 pool，走 direct 创建路径**。这与现有 network-enabled 和 bind-mount 的处理方式一致。

`Create` 里的 `useDirectCreate` 判断扩展：

```go
useBindMount   := cfg.WorkspacePath != "" && m.fsMeta != nil && m.fsMeta.Provider == storage.ProviderLocal
useSyncInitCtr := cfg.WorkspacePath != "" &&
    m.fsMeta != nil &&
    m.fsMeta.Provider != storage.ProviderLocal &&
    m.config.Workspace.SyncSecretRef != "" // 有 SecretRef = 启用 init container

if cfg.Network.Enabled || useBindMount || useSyncInitCtr {
    source = "direct"
    spec := m.buildSpec(id, cfg) // buildSpec 按条件填充 WorkspaceSync
    ...
}
```

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

`MountWorkspace` 检测到 init container 已完成同步时跳过 `syncToContainer`（init container 的完成由 `waitForPodReady` 保证——K8s 原生 init container 在退出 0 之前主容器不会启动）：

```go
initSynced := m.fsMeta != nil &&
    m.fsMeta.Provider != storage.ProviderLocal &&
    m.config.Workspace.SyncSecretRef != ""
if !initSynced {
    if err := m.syncToContainer(ctx, scoped, runtimeID); err != nil {
        return fmt.Errorf("sync to container: %w", err)
    }
}
```

> **Open Question 回答**：是否接受"有 workspace sync 的 sandbox 不走 pool"？**是，必须接受**。init container 在 pod spec 里声明，pool pod 无法事后改造。这是设计约束，不是可选项。需在配置文档里明确说明：启用 `sync_secret_ref` 后，所有带 `workspace_path` 的 sandbox 都走 direct 路径，pool 仅服务无 workspace 的 sandbox。

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
