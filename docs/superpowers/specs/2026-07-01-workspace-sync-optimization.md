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
- init container 模式仅适用于 K8s + 远端对象存储；Docker 运行时和手动挂载已有 sandbox 仍走 API sync 路径

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

releaseResult := func(res readResult) {
    if res.reader != nil {
        _ = res.reader.Close()
    }
    if res.done != nil {
        close(res.done)
    }
}

// cleanup 函数：仅在错误路径调用，负责排空并释放尚未被主循环处理的 entry。
// 注意：本函数处理的是主循环还没 range 到的 entry（下标 ≥ start）。
// 当前 entry 的 reader/done 必须由调用点先 releaseResult，避免 WriteHeader/io.Copy
// 失败时泄漏 reader 或卡住等待 done 的 goroutine。
cleanup := func(start int) {
    for j := start; j < len(entries); j++ {
        if resultChs[j] == nil { // 目录 entry 没有 goroutine
            continue
        }
        res := <-resultChs[j] // 可能阻塞，等排队中的 goroutine 发送
        releaseResult(res)    // 释放 reader 和 sem 槽位，链式解锁下一个
    }
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

// 主循环：正常路径，每个 entry 只处理一次
res := <-resultChs[i]
if res.err != nil {
    cleanup(i + 1) // 从下一个未处理的 entry 开始清理
    return res.err
}
if err := tw.WriteHeader(&tar.Header{Size: res.size, ...}); err != nil {
    releaseResult(res)
    cleanup(i + 1)
    return err
}
if _, err := io.Copy(tw, res.reader); err != nil { // 流式，不缓冲
    releaseResult(res)
    cleanup(i + 1)
    return err
}
releaseResult(res)
```

**注意事项：**

1. **`entry.size` 与实际内容不符（极罕见）**：需区分两种情况：
   - 实际字节数 < size：`io.Copy` 提前结束，`tw.Close()` 或下一个 `WriteHeader` 时报错 → 可检测，调用方正常处理
   - 实际字节数 > size：Go 的 `archive/tar.Writer` 会在写入超过 header 声明字节数时返回 `ErrWriteTooLong`，`io.Copy` 拿到错误后中止并向上传递 → **不是静默损坏**，可正常检测和处理（P2 修正）

   S3/OSS 的 Content-Length 是强一致的，实际中不会发生。此处记录是为了说明为何不加额外校验。

2. **ctx 取消时 `io.Copy` 的行为**：取决于底层 reader 是否感知 context cancellation。S3 SDK（aws-sdk-go-v2 / aliyun-oss-go-sdk 等）的 object reader 均感知 ctx，取消时 `io.Copy` 会立即返回错误，进入正常错误路径。**实现时必须确保 `scoped.Open` 返回的 reader 透传 ctx 给底层 HTTP 请求**，否则取消信号无法传递，`io.Copy` 会阻塞到文件传完。

3. **cleanup 的链式解锁行为**：cleanup 顺序处理各 channel：`<-resultChs[j]` 可能阻塞，等待还在排队 `sem <- struct{}{}` 的 goroutine 发送结果。但每次 `releaseResult(res)` 都会释放一个 sem 槽位，从而解锁下一个等待的 goroutine，形成链式传导。cleanup 只处理“尚未被主循环接收”的后续 entry；当前 entry 在 `WriteHeader` / `io.Copy` 失败时必须先 `releaseResult(res)` 再 cleanup。

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

**关键约束**：必须保证任何错误路径都关闭已打开的 reader。`ReadFileContent` 在 K8s 下是 exec/cat 流，且当前 `pipeReadCloser.Close()` 会同步 drain 到 EOF；如果 `Create` 失败后直接 `Close`，大文件会被读完才返回。实现时必须在错误路径先取消该文件的 context，再关闭 reader。

实现上用 `errgroup.WithContext`（`golang.org/x/sync/errgroup`）管控整个 open → create → copy → close 流程。不要拆成“先打开所有 reader，再写回”的两阶段 channel 设计；一段式处理更简单，且任意错误都会触发 `egCtx` 取消。每个文件再派生一个 `fileCtx`，用于在当前文件出错时立即中断 `ReadFileContent` 的底层 exec/cat 流。

```go
func (m *Manager) downloadFilesDirect(
    ctx context.Context,
    scoped storage.ScopedFS,
    runtimeID string,
    changedSet map[string]struct{},
) error {
    eg, egCtx := errgroup.WithContext(ctx)
    eg.SetLimit(maxConcurrentStorageWrites)

    for path := range changedSet {
        path := path
        eg.Go(func() error {
            fileCtx, cancelFile := context.WithCancel(egCtx)
            defer cancelFile()

            rc, err := m.runtime.ReadFileContent(fileCtx, runtimeID, "/workspace/"+path)
            if err != nil {
                return fmt.Errorf("read %q: %w", path, err)
            }
            closeReader := func() {
                cancelFile()
                _ = rc.Close()
            }
            defer closeReader()

            w, err := scoped.Create(egCtx, path, contentTypeOpt(path))
            if err != nil {
                cancelFile() // 避免 rc.Close() drain 完整个文件
                return fmt.Errorf("create %q: %w", path, err)
            }
            closed := false
            defer func() {
                if !closed {
                    _ = w.Close()
                }
            }()

            if _, err := io.Copy(w, rc); err != nil {
                cancelFile()
                return fmt.Errorf("write %q: %w", path, err)
            }
            if err := w.Close(); err != nil {
                closed = true
                return fmt.Errorf("flush %q: %w", path, err)
            }
            closed = true
            return nil
        })
    }

    return eg.Wait()
}
```

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
| 100 个文件变更 | 下载全量 tar，串行写回 | 下载全量 tar，顺序流式写回（保持内存稳定） |

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
    Image       string   // sync init container 镜像
    RootPath    string   // 对象存储路径前缀，必须包含 storage.filesystem.sub_path
    Provider    string   // "s3" | "oss" | "cos" | "obs" | "minio"
    Endpoint    string
    Region      string
    Bucket      string
    UseSSL      bool
    SecretRef   string   // K8s Secret name，含 access_key / secret_key
    EgressFQDNs []string // Cilium toFQDNs 使用的对象存储域名
    EgressCIDRs []string // 标准 NetworkPolicy ipBlock 使用的稳定 CIDR
    SyncExclude []string
}
```

**`internal/sandbox/types.go` — WorkspaceInfo 新增 sync mode：**

```go
const (
    WorkspaceSyncModeAPI           = "api"
    WorkspaceSyncModeInitContainer = "init_container"
    WorkspaceSyncModeBindMount     = "bind_mount"
)

type WorkspaceInfo struct {
    // ...现有字段...
    SyncMode string `json:"sync_mode,omitempty"`
}
```

`SyncMode` 记录“这个 sandbox 的 workspace 实际如何完成初始同步”，不能用全局配置推断。原因：`MountWorkspace` 也可用于已有 sandbox 的手动挂载；这类挂载没有 init container 跑过，即使全局配置了 `sync_secret_ref`，也必须继续执行 `syncToContainer`。

**`internal/storage/filesystem.go` — FileSystemMeta 补充远端定位字段：**

```go
type FileSystemMeta struct {
    Provider  StorageProvider
    LocalPath string // non-empty only when Provider == ProviderLocal
    Endpoint  string
    Region    string
    Bucket    string
    SubPath   string
    UseSSL    bool
}
```

`RootPath` 不能只用 `cfg.WorkspacePath`。对象存储 driver 当前会在内部叠加 `storage.filesystem.sub_path`，init container 直连对象存储时也必须使用同一个前缀：

```go
func objectStorageRootPath(meta *storage.FileSystemMeta, workspacePath string) string {
    return path.Join(meta.SubPath, workspacePath)
}
```

**`internal/sandbox/manager.go` — ManagerConfig 新增内部配置：**

```go
type ManagerConfig struct {
    // ...现有字段...
    WorkspaceSyncEnabled             bool     // 仅 runtime.type == "kubernetes" 且 secret/image 配齐时为 true
    WorkspaceSyncSecretRef           string
    WorkspaceSyncImage               string
    WorkspaceSyncEgressFQDNs         []string
    WorkspaceSyncEgressCIDRs         []string
    SingleFileDownloadThreshold      int
}
```

上层配置装配时负责校验：

- `workspace.sync_secret_ref` 与 `workspace.sync_image` 必须同时配置，否则启动时报配置错误
- Docker 运行时忽略/拒绝 init container 配置，不能让 Docker runtime 静默忽略 `WorkspaceSync`
- Cilium 环境优先使用 `WorkspaceSyncEgressFQDNs`；为空时可从对象存储 endpoint 推导域名
- 标准 NetworkPolicy 环境必须配置 `WorkspaceSyncEgressCIDRs`，不能把单次 DNS 解析结果当成稳定策略

### pod.go — 注入 sync init container

```go
if ws := spec.WorkspaceSync; ws != nil {
    pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
        Name:  "workspace-sync",
        Image: ws.Image, // 精简镜像，内置 rclone 或自研 sync 二进制
        Command: []string{
            "/bin/workspace-sync",
            "--provider=" + ws.Provider,
            "--endpoint=" + ws.Endpoint,
            "--region=" + ws.Region,
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

### kubernetes/runtime.go — init 阶段网络策略

K8s `NetworkPolicy` 是 Pod 级别，不是 container 级别；不能只给 init container 开网、同时让 main container 完全无网。因此 init container 模式必须显式接受这个安全取舍：启动阶段临时允许 Pod 访问对象存储 endpoint，Pod Ready 后立即切换为 sandbox 的正常网络策略，并删除任何额外的 bootstrap allow policy。

当前 `CreateSandbox` 是“创建 Pod → 等 Ready → 应用 NetworkPolicy”。启用 init container 时需要改为“应用 bootstrap policy → 创建 Pod → 等 Ready → 应用正常 policy → 清理临时 bootstrap policy”。注意：`spec.ID` 是 pod/runtime 名称，restore 重建时可能等于旧 `RuntimeID`；NetworkPolicy selector 必须使用 pod 上最终的 `sandbox.id` label，也就是逻辑 sandbox ID。否则 restore 重建时 policy 选不中 pod。

```go
func policySandboxID(spec runtime.SandboxSpec) string {
    if spec.Labels != nil && spec.Labels["sandbox.id"] != "" {
        return spec.Labels["sandbox.id"]
    }
    return spec.ID
}

type workspaceSyncBootstrapCleanup struct {
    // OnFailure 删除 bootstrap 阶段创建的临时策略。
    // 标准 NetworkPolicy 模式删除 sandbox-<id>；Cilium FQDN 模式删除独立 Cilium allow policy。
    OnFailure func(context.Context)
    // AfterReady 只删除 Ready 后仍会额外放行的策略。
    // 标准 NetworkPolicy 模式由 updateNetworkPolicy 覆盖同名 policy，因此这里是 no-op；
    // Cilium FQDN 模式必须删除独立 Cilium allow policy，避免 bootstrap allow 遗留。
    AfterReady func(context.Context)
}

func (r *Runtime) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
    policyID := policySandboxID(spec)
    var bootstrapCleanup workspaceSyncBootstrapCleanup

    if ws := spec.WorkspaceSync; ws != nil {
        // Cilium egressDeny 是独立策略，allow policy 不能覆盖 deny。
        // restore 重建同一 logical sandbox 时，旧 deny 可能仍存在；init 阶段必须先删除，
        // Ready 后再按正常网络配置重建。
        if r.hasCilium {
            _ = deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, policyID)
        }
        cleanup, err := applyWorkspaceSyncBootstrapPolicy(ctx, r.client, r.dynClient, r.namespace, policyID, r.hasCilium, ws)
        if err != nil {
            return nil, fmt.Errorf("apply workspace sync bootstrap policy: %w", err)
        }
        bootstrapCleanup = cleanup
    }

    cleanupCreateFailure := func() {
        if bootstrapCleanup.OnFailure != nil {
            bootstrapCleanup.OnFailure(ctx)
        }
    }

    pod, err := createPod(ctx, r.client, r.namespace, spec)
    if err != nil {
        cleanupCreateFailure()
        return nil, err
    }

    if err := waitForPodReady(ctx, r.client, r.namespace, pod.Name, 60*time.Second); err != nil {
        _ = deletePod(ctx, r.client, r.namespace, pod.Name)
        cleanupCreateFailure()
        return nil, fmt.Errorf("wait for pod: %w", err)
    }

    // Ready 后把标准 bootstrap policy 更新为正常 sandbox 网络策略；
    // 若 bootstrap 额外创建了 Cilium FQDN allow policy，update 后必须删除。
    if err := updateNetworkPolicy(ctx, r.client, r.namespace, policyID, spec.NetworkEnabled, spec.NetworkWhitelist, spec.NetworkBlockPrivate); err != nil {
        _ = deletePod(ctx, r.client, r.namespace, pod.Name)
        cleanupCreateFailure()
        return nil, fmt.Errorf("apply network policy: %w", err)
    }
    if bootstrapCleanup.AfterReady != nil {
        bootstrapCleanup.AfterReady(ctx)
    }
    if r.hasCilium {
        if spec.NetworkEnabled && len(spec.NetworkWhitelist) == 0 {
            if err := applyCiliumPrivateDeny(ctx, r.dynClient, r.namespace, policyID); err != nil {
                _ = deleteNetworkPolicy(ctx, r.client, r.namespace, policyID)
                cleanupCreateFailure()
                _ = deletePod(ctx, r.client, r.namespace, pod.Name)
                return nil, fmt.Errorf("apply cilium private deny: %w", err)
            }
        } else {
            _ = deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, policyID)
        }
    }
    ...
}
```

`applyWorkspaceSyncBootstrapPolicy` 的规则：

- 允许 DNS
- 仅允许对象存储 endpoint 的 80/443 egress
- 标准 NetworkPolicy 模式使用现有 `sandbox-<logical sandbox id>` policy name，Ready 后由 `updateNetworkPolicy` 覆盖
- Cilium FQDN 模式创建独立的 `sandbox-workspace-sync-<logical sandbox id>` `CiliumNetworkPolicy`，Ready 后必须删除，避免 bootstrap allow 与正常策略叠加
- Pod selector 必须匹配 `createPod` 最终写入的 `sandbox.id` label；restore 重建时不能用 runtime/pod name 当 selector
- Cilium 环境下 bootstrap 前删除同 logical sandbox 的旧 `sandbox-private-deny-*`，Ready 后再按正常网络配置恢复
- Cilium 模式缺少可用 FQDN、标准 NetworkPolicy 模式缺少稳定 CIDR、或 policy 创建失败时，sandbox 创建失败且不创建 Pod

**P4 —— endpoint 的 IP 漂移问题（必须在实现前决策）**：标准 K8s `NetworkPolicy` 的 egress 只能写 `ipBlock`（CIDR），不支持 FQDN。而阿里云 OSS、腾讯 COS 等公网 endpoint 通常是 **CDN / 多 IP 轮换**，DNS A 记录会变。如果在创建 sandbox 时把 endpoint 解析成一组固定 CIDR 写进 policy，下一次 endpoint IP 漂移后，bootstrap policy 就会漏掉新 IP，init container 拉取间歇性失败——且因为是间歇性的，极难排查。两种可行策略，按环境二选一：

- **Cilium 环境**：用 `CiliumNetworkPolicy` 的 `toFQDNs`，直接按 `sync_egress_fqdns` 放行，天然跟随 IP 变化。这是首选，也和本方案已有的 Cilium 分支一致。该 bootstrap Cilium policy 必须在 Pod Ready 且正常 policy 更新完成后删除。
- **标准 NetworkPolicy 环境**：`ipBlock` 必须放宽到 endpoint 所属对象存储服务的**已知网段**（各云厂商公布了 OSS/COS/OBS 的公网 IP 段），而不是解析单次 A 记录。文档需明确要求运维填 `sync_egress_cidrs`。若既非 Cilium、endpoint 又是不可枚举的动态 IP，则该 endpoint 不适合用 init container 模式。

`RemoveSandbox` 也必须使用同一套 logical sandbox id 清理策略。当前调用方传入的是 runtime/pod id；在 restore 重建场景下它可能不等于 logical sandbox id。K8s runtime 删除前应尽量读取 pod label：

```go
func (r *Runtime) cleanupSandboxPolicies(ctx context.Context, runtimeID string) {
    policyID := runtimeID
    if pod, err := getPod(ctx, r.client, r.namespace, runtimeID); err == nil {
        if v := pod.Labels["sandbox.id"]; v != "" {
            policyID = v
        }
    }
    if r.hasCilium {
        _ = deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, policyID)
        _ = deleteWorkspaceSyncBootstrapCiliumPolicy(ctx, r.dynClient, r.namespace, policyID)
        if policyID != runtimeID {
            _ = deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, runtimeID) // 兼容旧策略名
            _ = deleteWorkspaceSyncBootstrapCiliumPolicy(ctx, r.dynClient, r.namespace, runtimeID)
        }
    }
    _ = deleteNetworkPolicy(ctx, r.client, r.namespace, policyID)
    if policyID != runtimeID {
        _ = deleteNetworkPolicy(ctx, r.client, r.namespace, runtimeID) // 兼容旧策略名
    }
}
```

`UpdateNetwork` 只接收 runtime id，也需要同样先从 pod label 解析 logical sandbox id，再更新对应 policy。

**安全边界（P2，必须显式接受）**：Pod 进入 Running 到正常 policy 更新完成之间存在一个窗口，在此窗口内 **main container 的网络命名空间也处于 bootstrap policy 下，可直连对象存储 endpoint 的 80/443**。但按当前 sandbox 启动模型，main container 默认命令是 `sleep infinity`，且 `CreateSandbox` 在正常 policy 更新完成前不会把 sandbox 返回给用户；因此正常用户代码并不会在这个窗口内经由 API 执行。风险应表述为“主容器网络短暂可达对象存储 endpoint”，而不是“用户代码必然可达 endpoint”：

- 用户代码**没有** access key（key 只存在于 init container 的 Secret，不注入 main container），无法直接读写 bucket；
- 如果未来镜像 entrypoint 会自动运行用户代码、引入 sidecar、或存在能在 Pod Ready 前 exec 进容器的外部权限，endpoint 可达性会扩大攻击面——探测 bucket 是否存在、尝试匿名/公共读、对 endpoint 做指纹识别等；
- 当前实现下该窗口是平台残余风险，不是常规用户代码路径；实现和运维文档需把这个前提写清楚，避免后续改启动命令时误判风险。

这个窗口**无法用纯 K8s 机制消除**：init container 退出后、main container 启动前，K8s 没有 hook 点可以插入 policy 切换。相比"Ready 前完全没有 NetworkPolicy"的现状，本方案是风险收敛（窗口从"整个创建期"缩短到"Running→policy 更新"），但不是零风险。

严格零窗口方案见"不在范围内"：让 main container entrypoint 阻塞等待 API 写入的解除文件/annotation，API 在正常 policy 更新完成后才放行。本阶段不纳入。

### manager.go — buildSpec 与创建路径联动

**P1-c 修正**：init container 必须在 Pod 创建时注入，pool 里的 pod 是预先建好的，事后无法补注入。因此，**启用 init container 时，workspace sandbox 必须强制绕过 pool，走 direct 创建路径**。这与现有 network-enabled 和 bind-mount 的处理方式一致。

`Create` 里的 direct 创建判断扩展：

```go
useBindMount := cfg.WorkspacePath != "" && m.fsMeta != nil && m.fsMeta.Provider == storage.ProviderLocal
useSyncInitCtr := m.shouldUseWorkspaceInitContainer(cfg.WorkspacePath)

if cfg.Network.Enabled || useBindMount || useSyncInitCtr {
    source = "direct"
    spec := m.buildSpec(id, cfg) // buildSpec 按条件填充 WorkspaceSync
    ...
}
```

```go
func (m *Manager) shouldUseWorkspaceInitContainer(workspacePath string) bool {
    return workspacePath != "" &&
        m.config.WorkspaceSyncEnabled &&
        m.fsMeta != nil &&
        m.fsMeta.Provider != storage.ProviderLocal
}
```

`buildSpec` 按 provider 填充 `WorkspaceSync`：

```go
if m.shouldUseWorkspaceInitContainer(cfg.WorkspacePath) {
    spec.WorkspaceSync = &runtime.WorkspaceSyncSpec{
        Image:       m.config.WorkspaceSyncImage,
        RootPath:    objectStorageRootPath(m.fsMeta, cfg.WorkspacePath),
        Provider:    string(m.fsMeta.Provider),
        Endpoint:    m.fsMeta.Endpoint,
        Region:      m.fsMeta.Region,
        Bucket:      m.fsMeta.Bucket,
        UseSSL:      m.fsMeta.UseSSL,
        SecretRef:   m.config.WorkspaceSyncSecretRef,
        EgressFQDNs: m.workspaceSyncEgressFQDNs(),
        EgressCIDRs: m.workspaceSyncEgressCIDRs(),
        SyncExclude: cfg.WorkspaceSyncExclude,
    }
}
```

自动挂载 workspace 时按创建模式分流：

```go
if cfg.WorkspacePath != "" {
    switch {
    case bindMounted:
        scoped, fsErr := storage.NewScopedFS(m.filesystem, cfg.WorkspacePath)
        if fsErr != nil {
            err = fmt.Errorf("create scoped filesystem: %w", fsErr)
        } else {
            err = m.registerWorkspaceWithMode(ctx, id, scoped, cfg.WorkspacePath, cfg.WorkspaceSyncExclude, true, WorkspaceSyncModeBindMount)
        }
    case useSyncInitCtr:
        // init container 已在 waitForPodReady 前完成 storage -> /workspace。
        // 这里只注册 ScopedFS 和 WorkspaceInfo，不再调用 syncToContainer。
        scoped, fsErr := storage.NewScopedFS(m.filesystem, cfg.WorkspacePath)
        if fsErr != nil {
            err = fmt.Errorf("create scoped filesystem: %w", fsErr)
        } else {
            err = m.registerWorkspaceWithMode(ctx, id, scoped, cfg.WorkspacePath, cfg.WorkspaceSyncExclude, false, WorkspaceSyncModeInitContainer)
        }
    default:
        err = m.MountWorkspace(ctx, id, cfg.WorkspacePath, cfg.WorkspaceSyncExclude)
    }
    if err != nil {
        // 保持现有失败清理逻辑：remove sandbox + 删除内存 map
        return nil, fmt.Errorf("mount workspace: %w", err)
    }
}
```

`MountWorkspace` 是 public API handler 使用的路径，必须始终执行 API sync。不要在这里用全局配置推断 init 是否完成：

```go
func (m *Manager) MountWorkspace(ctx context.Context, sandboxID, rootPath string, exclude []string) error {
    runtimeID := m.sandboxes[sandboxID].RuntimeID
    scoped, err := storage.NewScopedFS(m.filesystem, rootPath)
    if err != nil {
        return fmt.Errorf("create scoped filesystem: %w", err)
    }
    if err := m.syncToContainer(ctx, scoped, runtimeID); err != nil {
        return fmt.Errorf("sync to container: %w", err)
    }
    return m.registerWorkspaceWithMode(ctx, sandboxID, scoped, rootPath, exclude, false, WorkspaceSyncModeAPI)
}
```

`registerWorkspaceWithMode` 只负责登记已经创建好的 `ScopedFS`，避免 `MountWorkspace` 在完成 `syncToContainer` 后再次 `NewScopedFS`，造成“容器已同步但注册失败”的半成功状态：

```go
func (m *Manager) registerWorkspaceWithMode(
    ctx context.Context,
    sandboxID string,
    scoped storage.ScopedFS,
    rootPath string,
    exclude []string,
    bindMounted bool,
    syncMode string,
) error {
    if scoped == nil {
        return fmt.Errorf("scoped filesystem is nil")
    }
    now := time.Now()
    m.mu.Lock()
    sb := m.sandboxes[sandboxID]
    m.workspaces[sandboxID] = scoped
    sb.Workspace = &WorkspaceInfo{
        RootPath:     rootPath,
        MountedAt:    now,
        LastSyncedAt: now,
        BindMounted:  bindMounted,
        SyncExclude:  exclude,
        SyncMode:     syncMode,
    }
    sb.UpdatedAt = now
    m.mu.Unlock()
    if m.sessions != nil {
        _ = m.sessions.Save(ctx, sb)
    }
    return nil
}
```

> **Open Question 回答**：是否接受"有 workspace sync 的 sandbox 不走 pool"？**是，必须接受**。init container 在 pod spec 里声明，pool pod 无法事后改造。这是设计约束，不是可选项。需在配置文档里明确说明：启用 `sync_secret_ref` 后，所有带 `workspace_path` 的 sandbox 都走 direct 路径，pool 仅服务无 workspace 的 sandbox。

### restorePersistentSandboxes — 按 SyncMode 恢复

第三层必须同步修改 restore 流程，否则 pod 重建后仍会走 API `syncToContainer`，收益表里的“API 进程文件流量 = 0”不成立。

**`recreatedThisRun` 语义定义**：这是一个 **per-sandbox** 布尔量，作用域仅限当前 sandbox 的 restore 处理，含义是「本次进程启动是否重建了这个 pod」——`restorePersistentSandboxes` 发现 pod 已不存在、调用 `recreateSandbox` 并成功后置为 `true`；若原 pod 仍在 running（无需重建）则为 `false`。它不是整个 restore loop 的全局标志，每个 sandbox 独立计算。

它只影响 `WorkspaceSyncModeInitContainer` 分支的降级判断：

- `recreatedThisRun == true`（pod 是本次新建）：init container 已在新 pod 里跑过一次；只有当**当前配置已不再支持 init container**时，新 pod 的 `/workspace` 才是空的，必须降级 API sync 补数据。
- `recreatedThisRun == false`（pod 原本仍 running）：`/workspace` 数据仍在老 pod 的 emptyDir 里，**无论配置是否变化都不能重新 sync**（会覆盖运行中的数据）。所以这一步直接跳过。

因此下方 `if recreatedThisRun && !shouldUseWorkspaceInitContainer(...)` 的两个条件缺一不可。

```go
if sbPtr.Workspace != nil && sbPtr.Workspace.RootPath != "" {
    scoped, fsErr := storage.NewScopedFS(m.filesystem, sbPtr.Workspace.RootPath)
    if fsErr == nil {
        m.workspaces[id] = scoped

        switch sbPtr.Workspace.SyncMode {
        case WorkspaceSyncModeBindMount:
            // 直接共享宿主机目录，无需同步。
        case WorkspaceSyncModeInitContainer:
            // 如果 pod 刚被 recreateSandbox 重建，init container 已经完成拉取；
            // 如果 pod 原本仍 running，/workspace 仍在该 pod 的 emptyDir 中。
            // 两种情况都不能再调用 syncToContainer。
            //
            // 例外：如果这是本次启动刚重建的 pod，但当前配置已不再支持 init container
            //（例如 sync_secret_ref 被移除），则必须降级到 API sync，并把 SyncMode 更新为 api。
            if recreatedThisRun && !m.shouldUseWorkspaceInitContainer(sbPtr.Config.WorkspacePath) {
                logger.Warn(ctx, "workspace init-container config unavailable, falling back to API sync", ...)
                if syncErr := m.syncToContainer(ctx, scoped, sbPtr.RuntimeID); syncErr != nil {
                    logger.Error(ctx, "workspace re-sync failed", ...)
                } else {
                    sbPtr.Workspace.SyncMode = WorkspaceSyncModeAPI
                }
            }
        default:
            // 兼容旧 session 和手动挂载：继续用 API sync 恢复。
            if syncErr := m.syncToContainer(ctx, scoped, sbPtr.RuntimeID); syncErr != nil {
                logger.Error(ctx, "workspace re-sync failed", ...)
            }
        }
    }
}
```

`recreateSandbox` 使用 `m.buildSpec(sb.RuntimeID, sb.Config)` 时，只有 `sb.Config.WorkspacePath` 非空且当前配置仍启用 init container 的 sandbox 才会注入 init container。手动挂载的 workspace 通常只存在于 `sb.Workspace.RootPath`，不在 `sb.Config.WorkspacePath`，因此仍走 API restore，符合预期。

### 新增配置项

```go
type WorkspaceConfig struct {
    // ...现有字段...
    SyncSecretRef                  string   `mapstructure:"sync_secret_ref"`   // 与 sync_image 同时填写才启用
    SyncImage                      string   `mapstructure:"sync_image"`        // sync init container 镜像
    SyncEgressFQDNs                []string `mapstructure:"sync_egress_fqdns"` // Cilium toFQDNs；可从 endpoint 推导
    SyncEgressCIDRs                []string `mapstructure:"sync_egress_cidrs"` // 标准 NetworkPolicy ipBlock；非 Cilium 必填
    SingleFileDownloadThreshold int `mapstructure:"single_file_download_threshold"`
}
```

```yaml
workspace:
  single_file_download_threshold: 5
  sync_secret_ref: "sandbox-storage-secret"
  sync_image: "registry.example.com/workspace-sync:1.0"
  # Cilium 环境推荐按域名放行。
  sync_egress_fqdns:
    - "oss-cn-hangzhou.aliyuncs.com"
  # 标准 NetworkPolicy 环境必须配置对象存储服务的稳定 CIDR，不能填单次 DNS 解析结果。
  sync_egress_cidrs:
    - "203.0.113.0/24"
```

### 收益

| 场景 | 优化前 | 优化后 |
|------|--------|--------|
| K8s，workspace 1000 个文件 | API 进程全量 tar 传输 | API 进程文件流量 = 0 |
| pod crash 后恢复 | `restorePersistentSandboxes` 全量 syncToContainer | 当前 init 配置仍有效时，pod 重建由 init container 自动重拉 |
| API OOM 风险 | 存在（受文件总大小影响）| 消除（数据不过 API 进程）|

---

## 变更范围

| 文件 | 变更 | 所属层 |
|------|------|--------|
| `internal/sandbox/workspace.go` | `writeTarStream` 改为流式；新增 `downloadFilesDirect`、`downloadFilesViaTar`；常量 `singleFileDownloadThreshold`、`maxConcurrentStorageWrites` | 优化一、二 |
| `internal/runtime/types.go` | `SandboxSpec` 新增 `WorkspaceSync *WorkspaceSyncSpec`；`WorkspaceSyncSpec` 包含 image、对象存储定位、SecretRef、bootstrap egress FQDN/CIDR | 优化三 |
| `internal/storage/filesystem.go` | `FileSystemMeta` 补充 Endpoint、Region、Bucket、SubPath、UseSSL | 优化三 |
| `internal/sandbox/types.go` | `WorkspaceInfo` 新增 `SyncMode`，并定义 `api` / `init_container` / `bind_mount` 常量 | 优化三 |
| `internal/config/config.go` | `WorkspaceConfig` 新增 `SyncSecretRef`、`SyncImage`、`SyncEgressFQDNs`、`SyncEgressCIDRs`、`SingleFileDownloadThreshold` | 优化二、三 |
| `internal/sandbox/manager.go` | direct 创建判断、`buildSpec` 填充 `WorkspaceSync`、自动挂载注册 `SyncMode`、public `MountWorkspace` 固定 API sync、注册 helper 接收已创建的 `ScopedFS`、restore 按 `SyncMode` 恢复 | 优化三 |
| `internal/runtime/kubernetes/runtime.go` | init container 模式下按 logical sandbox id 应用 bootstrap NetworkPolicy/CiliumNetworkPolicy，Pod Ready 后更新为正常策略并删除临时 Cilium allow；Cilium 环境下 init 前移除旧 deny，Ready 后恢复正常 deny；`RemoveSandbox`/`UpdateNetwork` 从 pod label 解析 logical sandbox id 清理或更新策略 | 优化三 |
| `internal/runtime/kubernetes/pod.go` | 按 `WorkspaceSync` 注入 sync init container | 优化三 |
| `internal/runtime/kubernetes/exec.go` | **无改动** | — |
| `configs/config.yaml` | 新增 `workspace.single_file_download_threshold`、`workspace.sync_secret_ref`、`workspace.sync_image`、`workspace.sync_egress_fqdns`、`workspace.sync_egress_cidrs` | 优化二、三 |
| sync 镜像 Dockerfile | 新增（内置 rclone 或自研二进制，精简） | 优化三 |

## 错误处理

| 场景 | 处理 |
|------|------|
| `entry.size` 与实际内容不符（极罕见的 S3 不一致）| `tar.Writer` 返回错误，`writeTarStream` 向上透传，sandbox 创建失败 |
| `ReadFileContent` 超时（小变更路径）| 单文件 error 返回，整个 `downloadFilesDirect` 中止 |
| 大变更路径下 `io.Discard` 失败 | 返回错误，中止整个 syncFromContainer |
| `sync_secret_ref` 与 `sync_image` 只配置其一 | 启动配置校验失败，避免半启用状态 |
| Docker runtime 配置了 init container sync | 启动配置校验失败或禁用该模式，不能让 Docker 静默忽略 `WorkspaceSync` |
| Cilium 模式缺少 bootstrap FQDN / 标准 NetworkPolicy 模式缺少 bootstrap CIDR / policy 创建失败 | sandbox 创建失败，不创建 Pod 或清理已创建资源 |
| restore 重建时 runtime id 与 logical sandbox id 不一致 | NetworkPolicy / CiliumNetworkPolicy 使用 logical sandbox id，selector 匹配 pod 的最终 `sandbox.id` label |
| runtime id 与 logical sandbox id 不一致导致策略清理遗漏 | `RemoveSandbox` 从 pod label 解析 logical id，并兼容删除 runtime id 命名的旧策略 |
| Cilium 旧 private deny 阻断 init container 访问私网对象存储 | bootstrap 前删除同 logical sandbox 的旧 deny，Ready 后按正常网络配置重建或保持删除 |
| Cilium FQDN bootstrap allow policy 遗留 | Ready 后显式删除 `sandbox-workspace-sync-<logical id>`；失败路径和 `RemoveSandbox` 也幂等清理 |
| init container 拉取失败（凭证错/网络断）| pod init container 失败 → `waitForPodReady` 超时 → sandbox 创建返回 500 |
| init container 拉取超时 | 同上 |
| init 模式 session 在重启后配置失效且 pod 需要重建 | 降级为 API `syncToContainer`，成功后将 `SyncMode` 更新为 `api` |

## 不在范围内

- `collectFiles` / `containerFileManifest` 的性能优化（当前 `find` 命令在大仓库下也较慢，可作为后续方向）
- `autoSync` 定时任务的频率/并发控制
- sync init container 镜像的构建与发布流程
- Docker 运行时（init container 是 K8s 原生概念）
- 严格消除 Pod Running 到正常 NetworkPolicy 更新之间的短暂 bootstrap egress 窗口

## 实施顺序建议

1. **优化一**（`writeTarStream` 流式化）——改动集中一个函数，风险最低，直接消灭 OOM，**优先实施**
2. **优化二**（差异化下载 + 并发写回）——新增两个函数，替换原 `downloadChangedFiles`，逻辑清晰，**紧跟实施**
3. **优化三**（init container）——架构变动最大，建议在优化一、二稳定后独立排期
