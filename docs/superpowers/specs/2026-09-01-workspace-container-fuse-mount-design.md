# Workspace 容器内 FUSE 直接挂载设计

**日期：** 2026-09-01

**状态：** 方案已口头确认，待文档复核
**目标分支：** `feat/workspace-fuse-mount`

## 1. 决策摘要

Workspace 从“sandbox-api 中转 tar 并定期同步”改为“在 sandbox 所在 Pod 或容器内使用 FUSE 直接挂载对象存储”。`/workspace` 不在宿主机建立共享挂载点，也不使用节点级常驻 FUSE DaemonSet。

两个 runtime 使用不同的容器编排方式，但保持相同的数据和 API 语义：

- Kubernetes：每个 sandbox Pod 注入一个可信 FUSE 原生 sidecar；sidecar 与非特权 sandbox 容器通过 memory-backed `emptyDir` 和 mount propagation 共享 `/workspace`。
- Docker：每个 sandbox 使用一个自带 FUSE 的特殊容器；可信 root supervisor 管理 FUSE，所有用户代码和文件命令强制以 UID/GID 1000 执行。
- MinIO 和普通华为 OBS 对象桶一期统一使用 s3fs。华为 OBS 并行文件系统后续使用 obsfs，不在一期范围内。
- 每个 sandbox 只挂载其自己的对象前缀，不暴露 bucket 中的其他 workspace。
- 同一个 workspace 只允许一个读写 sandbox，通过 Redis 独占租约防止多 FUSE 客户端并发写。
- FUSE 模式不再执行 `syncToContainer`、`syncFromContainer` 或 auto-sync；挂载失败必须 fail closed，不自动退回 emptyDir 或同步模式。

本设计取代以下未实施草案：

- `2026-06-28-workspace-storage-node-fuse-hostpath.md`
- `2026-06-28-workspace-storage-sidecar-fuse.md`
- `2026-06-28-workspace-storage-rootless-fuse.md`
- `2026-06-28-workspace-storage-csi-ephemeral.md`

## 2. 背景与现状

远端对象存储 workspace 当前使用以下数据路径：

```text
Object Storage
      │
      ▼
sandbox-api / ScopedFS
      │  List/Open/Create + tar stream
      ▼
Docker container 或 Kubernetes emptyDir /workspace
```

该路径存在以下问题：

1. sandbox-api 需要遍历目录、构造或解析 tar，并中转全部文件数据。
2. 容器内写入只有在手动或自动同步完成后才持久化，存在数据丢失窗口。
3. Kubernetes 的 workspace 数据在同步前驻留于节点 emptyDir，大量 workspace 会占用节点磁盘。
4. mtime 增量判断、删除检测、异常回退和多副本 auto-sync 使同步状态复杂，容易产生遗漏或重复写入。

当前代码只有 local provider 在 sandbox 创建时使用 bind mount；MinIO、OBS 等远端 provider 都使用同步路径。FUSE 方案将远端 provider 改成创建时直接挂载，同时保留显式的 legacy sync 模式用于兼容和回滚。

## 3. 目标与非目标

### 3.1 目标

- 同时支持 Kubernetes 和 Linux Docker Engine runtime。
- 一期支持 MinIO 与普通华为 OBS 对象桶。
- A 类负载作为正式支持范围：普通文件创建、顺序读写、覆盖、删除、目录操作、代码执行和结果生成。
- B 类负载作为 best-effort：`git checkout`、`pip install`、`npm install` 和大量小文件。
- sandbox-api 不再中转 FUSE workspace 的文件数据。
- FUSE 凭证不暴露给 Kubernetes sandbox 主容器中的用户进程。
- 保持对象原生布局，使现有 `goairix/fs` 驱动仍能读写同一批对象。
- 挂载、故障、重挂载、销毁和持久 sandbox 恢复都有明确状态和可观测性。

### 3.2 非目标

- 不提供完整 POSIX 文件系统语义。
- 不支持 hardlink、原子目录 rename、跨客户端文件锁或可靠的远端 inotify。
- 不支持 SQLite、数据库文件、mmap 密集写或依赖强文件锁的负载。
- 一期不支持同一 workspace 多 sandbox 并发读写。
- 一期不支持运行中为既有 Pod/容器动态增加 FUSE workspace。
- 一期不移除 legacy sync 代码；待灰度稳定后另行清理。
- 一期不实现 prefix 级硬容量配额，因为 s3fs、goofys 和普通对象桶均不提供该能力。

## 4. 客户端选型

### 4.1 结论

一期统一使用 s3fs：

| Provider | FUSE 客户端 | 后端类型 |
|---|---|---|
| `minio` | s3fs | S3-compatible 普通对象桶 |
| `obs` | s3fs | 华为 OBS 普通对象桶 |
| 华为 OBS 并行文件系统 | obsfs，后续支持 | Parallel File System |

### 4.2 选择依据

- goofys 明确以性能优先、POSIX 兼容次之为设计目标，只支持顺序写，不保存逐文件 mode/owner/group，不支持 symlink/hardlink，并将 MinIO 标记为有限兼容。其最新正式版 v0.24.0 发布于 2020 年。
- s3fs 支持更大的 POSIX 子集，包括随机写、append、symlink、mode 和 uid/gid；它支持自定义 S3 endpoint 与 path-style 请求，且仍持续发布。
- s3fs 的随机写、append 和 rename 仍受对象存储限制：随机写或 append 会重写整个对象，rename 是 copy + delete，不是原子操作。
- 华为云 CCE 当前明确使用 s3fs 挂载普通 OBS 对象桶，使用 obsfs 挂载并行文件系统。
- s3fs 保持文件对象的原生数据格式，现有对象 API 和 `goairix/fs` 可继续访问挂载产生的文件。

goofys 仅保留为 MinIO A 类负载的性能对照项。如果四象限基准显示 s3fs 无法达到性能目标，再单独评审 goofys 或 geesefs，不在一期同时维护两套客户端。

### 4.3 版本策略

不直接追随 `latest` 标签。发布前针对 MinIO 和 OBS 验证 `bucket:/prefix` 挂载，选择通过测试的 s3fs 版本并固定镜像 digest。升级 s3fs 必须重新执行兼容性和故障测试。

## 5. Kubernetes 架构

### 5.1 Pod 结构

```text
┌──────────────────── Sandbox Pod ────────────────────┐
│                                                     │
│  initContainers                                    │
│  ├─ workspace-mounter  restartPolicy: Always       │
│  │    └─ s3fs bucket:/prefix → /workspace          │
│  └─ workspace-ready    一次性最终读写探测           │
│                                                     │
│  containers                                        │
│  └─ sandbox             UID/GID 1000               │
│                                                     │
│  memory-backed emptyDir + mount propagation         │
│                     /workspace                      │
└─────────────────────────────────────────────────────┘
```

使用 Kubernetes 原生 sidecar：`workspace-mounter` 位于 `initContainers`，设置 `restartPolicy: Always`。最低支持 Kubernetes 1.29，生产推荐 1.33 或更高版本。

### 5.2 共享卷与 mount propagation

Pod 定义 memory-backed `emptyDir`：

```yaml
volumes:
  - name: workspace
    emptyDir:
      medium: Memory
  - name: fuse-cache
    emptyDir:
      sizeLimit: 2Gi
```

`workspace` 只作为 FUSE mount 的传播锚点，不保存文件数据。`fuse-cache` 为 s3fs 临时写入和缓存提供有界空间，默认上限由配置决定。

Sidecar volume mount：

```yaml
volumeMounts:
  - name: workspace
    mountPath: /workspace
    mountPropagation: Bidirectional
```

Sandbox volume mount：

```yaml
volumeMounts:
  - name: workspace
    mountPath: /workspace
    mountPropagation: HostToContainer
```

`Bidirectional` 仅用于可信 privileged sidecar；sandbox 主容器保持非特权。Pod 不使用 hostPath，也不在宿主机创建可复用的 workspace 路径。

### 5.3 启动顺序

1. Kubelet 启动 `workspace-mounter`。
2. Sidecar 从只挂载给自身的 Secret volume 读取凭证，生成 mode `0600` 的临时密码文件。
3. Sidecar 前台启动 s3fs，将单个 workspace prefix 挂载到 `/workspace`。
4. `startupProbe` 检查 mount 类型、远端健康对象和 workspace prefix 可访问性。
5. startup probe 成功后，Kubelet 启动普通 init container `workspace-ready`。
6. `workspace-ready` 使用 UID 1000 创建、读取并删除一个保留探测文件。
7. 探测通过后启动 `sandbox` 主容器。
8. sandbox-api 只有在 Pod Running、主容器 Ready 且 mounter Ready 时才返回创建成功。

使用 startup probe 而不是只依赖 readiness probe，确保主容器不会在 FUSE 尚未完成挂载时开始执行用户命令。

一期采用固定的挂载参数基线，禁止由 API 调用方传入任意 s3fs 参数：

- 通用参数包含 `allow_other`、`uid=1000`、`gid=1000`、`umask=0022`、`mp_umask=0022`、前台运行和有界本地缓存；mounter 镜像中的 `/etc/fuse.conf` 仅启用 `user_allow_other`。
- MinIO 使用显式 endpoint、`use_path_request_style` 和配置的 TLS 校验，不依赖 AWS 域名推导。
- 华为 OBS 普通对象桶使用显式 OBS endpoint 和 region；是否启用 path-style 由四象限兼容测试固定为 provider profile，不能由租户覆盖。
- 生产环境禁止 `-o passwd_file` 之外的命令行明文凭证，也禁止 `-o ssl_verify_hostname=0`、跳过证书校验或任意 `url` 覆盖。
- 挂载参数通过参数数组生成，并对 endpoint、bucket 和 prefix 分别校验，不能拼接为 shell 命令。

### 5.4 Sidecar 权限与隔离

`workspace-mounter`：

- 使用固定 digest 的可信镜像。
- `privileged: true`，挂载 `/dev/fuse`。
- Secret 只挂载到 sidecar，不使用会被主容器读取的共享环境变量。
- s3fs 密码文件位于 sidecar 私有 tmpfs，权限 `0600`。
- 不暴露监听端口。

`sandbox`：

- `runAsUser: 1000`、`runAsGroup: 1000`。
- `allowPrivilegeEscalation: false`。
- 不挂载 `/dev/fuse`、Secret 或 sidecar 私有目录。
- 不增加 `SYS_ADMIN`。
- Pod 的 `shareProcessNamespace` 保持 `false`。
- 继续禁用 ServiceAccount token 和 service links。

### 5.5 Sidecar 故障恢复

- Sidecar 使用 liveness probe 检测 `Transport endpoint is not connected`、mount 消失或远端探测失败。
- 重启入口先执行 `fusermount3 -uz /workspace` 清理 stale mount，再重新挂载。
- 重挂载期间 workspace 状态为 `recovering`，新的 Exec 请求返回 workspace unavailable。
- 恢复成功后状态回到 `ready`。
- 连续重挂载超过配置次数后状态变为 `error`，不再自动重试；Pod 保留用于诊断或由上层销毁。
- Pod 终止时，原生 sidecar 在主容器之后收到 SIGTERM；sidecar 负责 flush 和 unmount，超时后允许 kubelet SIGKILL。

## 6. Docker 架构

### 6.1 特殊容器模型

Docker 不使用两个独立容器模拟 Kubernetes sidecar。若不经过宿主机 bind mount、volume plugin 或 mount namespace/nsenter，独立 mounter 容器创建的 FUSE mount 无法透明出现在现有 sandbox 容器中。

因此每个 Docker sandbox 使用一个特殊镜像和单个容器：可信 root supervisor、FUSE 进程与 UID 1000 用户环境位于同一 mount namespace。

```text
┌────────────── Docker FUSE Sandbox ──────────────┐
│ PID 1: trusted root supervisor                  │
│   ├─ 读取 root-only Secret                      │
│   ├─ 启动和监控 s3fs                            │
│   ├─ 管理 /workspace mount                      │
│   └─ SIGTERM 时 flush + unmount                 │
│                                                 │
│ 用户命令、文件命令：Docker Exec User=1000:1000 │
└─────────────────────────────────────────────────┘
```

### 6.2 容器启动

1. 容器以 root supervisor 作为 PID 1 启动。
2. supervisor 读取 AK/SK：Swarm 环境可使用 Docker Secret；普通 Docker Engine 使用 sandbox-api 创建的 root-only 临时凭证文件，以只读 bind mount 挂入 `/run/secrets`。该 bind mount 只承载凭证，不承载 `/workspace`，容器销毁后立即删除临时文件。
3. supervisor 创建 mode `0600` 的 s3fs 密码文件。
4. supervisor 前台启动 s3fs 并挂载单个 workspace prefix。
5. supervisor 完成远端读写探测，写入 root-only readiness 状态。
6. Docker health check 成功后，sandbox-api 将 sandbox 标记为 ready。
7. 用户命令始终通过 Docker Exec 的 `User: "1000:1000"` 执行。

### 6.3 最小权限

特殊容器不使用全量 `--privileged`，优先采用：

- `/dev/fuse` device mapping；
- `CAP_SYS_ADMIN`；
- mount/umount 所需 seccomp syscall；
- 专用 AppArmor profile；
- `no-new-privileges`；
- 只读 rootfs；
- root-only Secret 和 FUSE 管理目录；
- 删除镜像中的 setuid/setgid 二进制；
- 仅 `/workspace`、`/tmp`、FUSE cache 和 supervisor 状态目录可写。

该模型的安全边界弱于 Kubernetes sidecar，因为容器本身持有 `SYS_ADMIN`。所有用户可触发路径都必须强制 UID/GID 1000，且不得提供可切换到 root 的命令或文件能力。

### 6.4 Docker Exec 用户约束

以下所有路径必须显式设置 `User: "1000:1000"`，不能依赖容器默认用户：

- `Exec`
- `ExecStream`
- `ExecPipe`
- `ReadFileContent`
- 文件存在性、目录遍历、glob、行编辑等间接 exec
- 依赖安装命令

Docker `CopyToContainer` 写入的 tar header 必须保持 UID/GID 1000。安全测试需要证明 API 用户无法通过任一执行或文件接口创建 root-owned 可执行文件、读取 Secret 或向 supervisor 发送控制命令。

### 6.5 Docker 销毁

1. sandbox 状态改为 `destroying` 并拒绝新 Exec。
2. 等待正在执行的请求结束，超过销毁宽限期后取消。
3. supervisor 执行 `syncfs` 或等价刷新，再执行 `fusermount3 -u`。
4. 优雅卸载失败时执行 lazy unmount，并记录错误指标。
5. 删除容器和临时 Secret。
6. 释放 Redis workspace 租约。

## 7. 网络模型

### 7.1 Kubernetes 约束

同一 Pod 内 sidecar 和 sandbox 共享网络 namespace。NetworkPolicy 是 Pod 级的，无法只允许 sidecar 访问对象存储。

因此 FUSE workspace Pod 即使 `network.enabled=false`，仍需要为整个 Pod放行：

- 目标 MinIO 或 OBS endpoint 的精确 CIDR；
- 精确端口，默认 HTTPS 443 或 MinIO 配置端口；
- 解析该 endpoint 所需的 DNS 服务。

Sandbox 主容器也能连接这些地址，但没有存储凭证。必须使用最小网络范围和最小权限凭证降低风险：

- 禁止放开整个 VPC、集群或对象存储网段。
- MinIO 优先使用独立 endpoint 或专用负载均衡地址。
- 禁止匿名 bucket 访问。
- 存储策略只授权当前 workspace prefix。
- 凭证不得出现在 Pod 共享环境变量、主容器文件系统或 API 响应中。

### 7.2 Docker 约束

Docker 特殊容器同样需要访问对象存储 endpoint。现有 gateway/网络过滤必须将对应 endpoint 加入系统级 allowlist；用户配置的网络关闭不能覆盖该系统 allowlist。用户进程可连接 endpoint，但不能获得凭证。

## 8. Workspace 隔离与租约

### 8.1 路径规范化

`workspace_path` 在用于对象前缀、日志、Redis key 或 mount 参数前必须统一规范化：

- 必须为非空相对路径。
- 拒绝 `.`、`..`、绝对路径、NUL 和路径逃逸。
- 清理重复 `/`，规范化后必须仍位于配置的 `object_prefix` 下。
- 拒绝系统保留前缀，例如 `.sandbox-system`。
- mount 命令通过参数数组执行，不拼接 shell 字符串。

最终对象前缀：

```text
<storage.filesystem.sub_path>/<workspace.object_prefix>/<workspace_path>/
```

每个 FUSE 进程直接挂载该前缀，sandbox 看不到同 bucket 的父目录或兄弟 workspace。

### 8.2 Redis 独占租约

s3fs 不协调多个客户端对同一对象的并发修改，因此创建 FUSE workspace 前必须获取独占租约：

```text
sandbox:workspace-lease:<provider>:<bucket>:<normalized-prefix>
```

租约 value 包含 sandbox ID、runtime、runtime ID、创建时间和 fencing generation。规则：

- 同一 workspace 同时只允许一个 `rw` sandbox。
- sandbox-api 定期续租。
- 正常销毁时 compare-and-delete 释放，不能删除其他 generation 的租约。
- 租约过期后不能立即接管；必须确认旧 Pod/容器不存在或不在运行，再创建新 generation。
- API 多副本共享同一 Redis 状态，不能只使用进程内锁。

普通对象存储无法强制 fencing token，因此租约是防误用机制，不是强一致分布式锁。旧 sandbox 是否仍在运行必须作为接管前置条件。

## 9. 配置设计

建议扩展 `WorkspaceConfig`：

```go
type WorkspaceConfig struct {
    AutoSyncIntervalSeconds int    `mapstructure:"auto_sync_interval_seconds"`
    Mode                    string `mapstructure:"mode"` // "sync" | "fuse"
    FUSEDriver              string `mapstructure:"fuse_driver"` // 一期固定 s3fs
    MounterImage            string `mapstructure:"mounter_image"`
    DockerImage             string `mapstructure:"docker_image"`
    SecretName              string `mapstructure:"secret_name"`
    ObjectPrefix            string `mapstructure:"object_prefix"`
    CacheSize               string `mapstructure:"cache_size"`
    MountTimeoutSeconds     int    `mapstructure:"mount_timeout_seconds"`
    UnmountTimeoutSeconds   int    `mapstructure:"unmount_timeout_seconds"`
    RecoveryMaxAttempts     int    `mapstructure:"recovery_max_attempts"`
    LeaseTTLSeconds         int    `mapstructure:"lease_ttl_seconds"`
}
```

配置约束：

- `mode=fuse` 时 runtime 必须为 Kubernetes 或 Linux Docker Engine。
- `provider=minio|obs` 时一期只允许 `fuse_driver=s3fs`。
- Kubernetes 必须配置 mounter image 和 Secret。
- Docker 必须配置特殊 sandbox image 和 Secret 来源。
- Secret、AK、SK 不得通过日志输出。
- `mount_timeout_seconds`、`lease_ttl_seconds` 和 cache size 必须为正值并设置安全默认值。

示例：

```yaml
workspace:
  mode: "fuse"
  fuse_driver: "s3fs"
  # 由部署系统注入已经过四象限验证并固定 digest 的镜像引用。
  mounter_image: ${SANDBOX_WORKSPACE_MOUNTER_IMAGE}
  docker_image: ${SANDBOX_FUSE_RUNTIME_IMAGE}
  secret_name: "sandbox-storage-secret"
  object_prefix: "workspaces"
  cache_size: "2Gi"
  mount_timeout_seconds: 30
  unmount_timeout_seconds: 15
  recovery_max_attempts: 3
  lease_ttl_seconds: 120
```

## 10. Runtime 与状态模型

### 10.1 Runtime spec

`runtime.SandboxSpec` 增加可选 FUSE 描述：

```go
type WorkspaceFUSESpec struct {
    Driver       string
    Image        string
    SecretName   string
    Bucket       string
    Prefix       string
    Endpoint     string
    Region       string
    UseSSL       bool
    CacheSize    string
    MountTimeout time.Duration
}
```

Spec 只携带 Secret 引用，不携带明文 AK/SK。Docker 如无法通过名称引用 Secret，则传递 root-only secret file descriptor/path，由 runtime 管理生命周期，不能写入持久 session。

### 10.2 Workspace 状态

替换仅能表达 local bind mount 的布尔状态，增加：

```go
type WorkspaceMountType string

const (
    WorkspaceMountSync WorkspaceMountType = "sync"
    WorkspaceMountFUSE WorkspaceMountType = "fuse"
)

type WorkspaceMountState string

const (
    WorkspaceMountPending    WorkspaceMountState = "pending"
    WorkspaceMountReady      WorkspaceMountState = "ready"
    WorkspaceMountRecovering WorkspaceMountState = "recovering"
    WorkspaceMountError      WorkspaceMountState = "error"
    WorkspaceMountUnmounting WorkspaceMountState = "unmounting"
)
```

`WorkspaceInfo` 保存 mount type、mount state、driver、最后健康时间和租约 generation。旧 session 中 `BindMounted=true` 迁移为 local bind 兼容状态；不存在新字段的远端 workspace 继续解释为 sync，不自动升级为 FUSE。

## 11. API 语义

### 11.1 创建

创建请求带 `workspace_path` 且 `workspace.mode=fuse` 时：

- 获取独占租约；
- 绕过 pool，直接创建 Pod/容器；
- 等待 FUSE ready；
- 注册 workspace 状态，但不执行文件复制。

无 workspace 的 sandbox 继续使用 pool。legacy sync 模式保持现有行为。

### 11.2 SyncWorkspace

FUSE workspace 已实时写入后端：

- `to_container` 和 `from_container` 都返回成功；
- `files_synced=0`；
- 响应明确返回 `mount_type=fuse` 和 `message=direct-mounted workspace does not require sync`；
- 不更新为表示发生复制的时间，只更新健康检查时间。

auto-sync 完全跳过 FUSE workspace。

### 11.3 运行中 MountWorkspace/UnmountWorkspace

Kubernetes Pod spec 和 Docker mount namespace 不能通过现有 runtime 抽象安全地动态增加或移除该挂载：

- FUSE 模式下，对运行中无 workspace sandbox 调用 `MountWorkspace` 返回 HTTP 409，要求创建时提供 `workspace_path`。
- FUSE 模式下，`UnmountWorkspace` 返回 HTTP 409，workspace 随 sandbox 销毁卸载。
- sync 模式继续支持现有动态 mount/unmount API。

不得静默切换到 legacy sync，也不得重建容器而不告知调用方。

### 11.4 Exec

- mount state 为 `ready` 时允许 Exec。
- `pending`、`recovering`、`error`、`unmounting` 时返回 workspace unavailable，HTTP 503。
- Docker 所有 Exec 强制 UID/GID 1000。
- Kubernetes Exec 固定目标容器 `sandbox`，不能允许调用方选择 sidecar。

## 12. 对象元数据与兼容性

s3fs 写入的文件数据保持原生对象格式，但现有同步代码会按扩展名设置 `Content-Type`，并为 HTML 设置 `Content-Disposition: inline`；直接 FUSE 写入不保证复制这套对象元数据。

一期语义：

- Sandbox 文件下载 API 继续按扩展名设置 HTTP 响应头，不依赖对象元数据。
- 对象存储直链的 Content-Type/Content-Disposition 不作为 FUSE 一期保证。
- 如果后续必须保证对象直链元数据，增加异步元数据修复器或对象存储事件处理器，不能重新引入文件内容同步。

FUSE 上线前必须验证双向可见性：

- `goairix/fs` 创建的对象能被 s3fs 正确列举、读取、覆盖和删除。
- s3fs 创建的对象能被 MinIO/OBS driver 正确列举、读取、签名和删除。
- 隐式目录、空目录 marker、零字节文件和 Unicode key 行为一致。

## 13. 容量、缓存与性能边界

### 13.1 容量限制

FUSE workspace 不再使用 Kubernetes workspace emptyDir，因此当前 `max_disk` 不构成 workspace 硬配额。普通对象桶和 s3fs 不提供 prefix 级硬配额。

一期采用软配额：

- 周期统计 workspace prefix 对象总量和容量。
- 超过阈值后拒绝新的 Exec 和文件写 API，并触发 sandbox 终止策略。
- 指标与告警必须在接近阈值时提前触发。
- 文档和 API 明确软配额存在采样延迟，不能作为强安全边界。

若业务要求强配额，应使用每租户/每 workspace 独立 bucket 配额，或改用提供元数据与 quota 的文件系统；不在本设计内模拟硬配额。

### 13.2 s3fs 临时空间

s3fs 写入可能使用本地临时文件：

- Kubernetes 使用独立、带 sizeLimit 的 `fuse-cache` emptyDir。
- Docker 使用受限 scratch/cache volume，禁止无限使用 container writable layer。
- cache 满时写操作应返回 ENOSPC，不应触发无界内存缓存。
- cache 使用率、清理失败和 ENOSPC 进入指标和告警。

### 13.3 B 类负载建议

- npm/pip/build cache 放到 `/tmp`，不放入 `/workspace`。
- 同一 workspace 禁止并发 Git 操作。
- Git lock/rename 在异常退出时可能残留或处于非原子状态，调用方需要允许清理重试。
- 对大量小文件的安装和遍历不承诺与本地文件系统相同的性能。

## 14. 故障处理

| 场景 | 行为 |
|---|---|
| Secret 缺失或无权限 | sandbox 创建失败，释放租约 |
| bucket/prefix 不存在 | 尝试通过存储驱动创建 prefix；仍失败则创建失败 |
| mount 超时 | 创建失败，清理 Pod/容器和租约 |
| startup probe 失败 | 主容器不启动，最终创建超时 |
| 运行中 FUSE 断开 | mount state=`recovering`，拒绝新 Exec，执行有界重挂载 |
| 重挂载失败 | mount state=`error`，返回 503，等待销毁或人工诊断 |
| 对象存储网络中断 | FUSE 返回 I/O 错误；记录错误，不切换到本地目录 |
| cache 满 | 写返回 ENOSPC，记录 workspace/cache 指标 |
| Redis 租约冲突 | 返回 HTTP 409，包含占用 sandbox ID |
| API 重启 | 从 session 恢复状态并检查 runtime/mounter 健康；不重复挂载已有健康实例 |
| Pod/容器丢失 | 清理 session 和租约；按持久 sandbox 规则重新创建 |
| 优雅卸载超时 | lazy unmount 后强制删除，记录告警 |

任何故障路径都不能把空 emptyDir、容器目录或 writable layer 当作 workspace 继续运行。

## 15. 可观测性

新增结构化日志字段：

- `workspace_mount_type`
- `workspace_mount_state`
- `workspace_driver`
- `storage_provider`
- `workspace_hash`，不记录完整敏感路径
- `mount_attempt`
- `lease_generation`

新增指标：

- `sandbox_workspace_mount_duration_seconds`
- `sandbox_workspace_mount_total{runtime,provider,result}`
- `sandbox_workspace_unmount_total{runtime,result}`
- `sandbox_workspace_recovery_total{runtime,provider,result}`
- `sandbox_workspace_unavailable_total{reason}`
- `sandbox_workspace_lease_conflict_total`
- `sandbox_workspace_fuse_cache_bytes`
- `sandbox_workspace_fuse_errors_total{operation}`

Pod 和 Docker health 状态必须区分 container running 与 FUSE ready，不能仅凭主进程存活判定 workspace 健康。

## 16. 代码变更边界

| 文件/组件 | 设计变更 |
|---|---|
| `internal/config/config.go` | Workspace FUSE 配置、默认值与校验 |
| `internal/runtime/types.go` | `WorkspaceFUSESpec`、mount 状态相关类型 |
| `internal/sandbox/types.go` | Workspace mount type/state/driver/health/lease 字段 |
| `internal/sandbox/manager.go` | FUSE direct create、绕过 pool、租约、restore、健康门控 |
| `internal/sandbox/workspace.go` | FUSE 模式跳过 sync；动态 mount/unmount 冲突语义 |
| `internal/runtime/kubernetes/pod.go` | 原生 sidecar、memory emptyDir、propagation、probe、Secret |
| `internal/runtime/kubernetes/exec.go` | 保持 exec 固定到 `sandbox`；错误映射 |
| `internal/runtime/docker/container.go` | 特殊镜像、FUSE device/capability/security/health |
| `internal/runtime/docker/exec.go` | 所有 ExecOptions 强制 `User=1000:1000` |
| `internal/runtime/docker/file.go` | 直接 Exec 强制用户；CopyToContainer ownership 检查 |
| `internal/storage/state/redis` | workspace 独占租约与 generation |
| `deploy/helm/sandbox` | mounter image、Secret、network policy、sidecar 资源配置 |
| `docker` | 特殊 sandbox-fuse image、entrypoint、healthcheck、Secret 示例 |

不在一期修改公共文件 API 的主要请求结构；workspace 状态响应允许增加向后兼容字段。

## 17. 验证与验收

### 17.1 测试矩阵

必须覆盖：

| Runtime | Provider |
|---|---|
| Kubernetes | MinIO |
| Kubernetes | 华为 OBS 普通对象桶 |
| Docker | MinIO |
| Docker | 华为 OBS 普通对象桶 |

### 17.2 功能测试

- 创建、读取、覆盖、append、truncate、删除文件。
- 创建、遍历、rename、删除目录。
- 零字节、Unicode、空格、长文件名和深层目录。
- 单文件至少 1 GiB 的 multipart 写入和读取。
- 至少 10,000 个小文件的创建、list、stat 和删除。
- s3fs 与 `goairix/fs` 的双向对象可见性。
- `git clone/status/checkout`。
- pip/npm install，cache 指向 `/tmp`。
- 动态 MountWorkspace/UnmountWorkspace 在 FUSE 模式返回 409。
- SyncWorkspace 返回 no-op 语义且不产生对象内容传输。

### 17.3 安全测试

- 路径穿越、绝对路径、特殊字符和 shell 参数注入。
- sandbox 主容器无法读取 Kubernetes Secret、sidecar `/proc` 或 `/dev/fuse`。
- Docker 用户命令 UID/GID 始终为 1000，覆盖所有直接和间接 exec 路径。
- Docker 用户无法读取 root-only Secret、控制 supervisor 或执行 mount/umount。
- symlink 无法逃逸到同 bucket 的其他 workspace。
- NetworkPolicy/gateway 只放行目标 endpoint 和 DNS。
- 同 workspace 第二个读写创建请求稳定返回租约冲突。

### 17.4 故障测试

- kill s3fs、kill Kubernetes sidecar、重启 Docker supervisor 子进程。
- 对象存储断网、DNS 失败、证书错误、AK/SK 失效。
- mount 过程中 Pod/容器被删除。
- 写入中 cache 满。
- Pod 驱逐、节点重启、Docker daemon 重启、sandbox-api 重启。
- 优雅卸载失败和 stale FUSE mount 恢复。
- Redis 短暂不可用、租约续期失败和过期接管。

### 17.5 验收标准

- 四象限所有 A 类功能测试通过。
- FUSE workspace 的创建、运行和销毁路径不调用 tar 同步方法。
- 挂载未 ready 时用户代码不会启动。
- 任一 FUSE 故障不会导致数据写入本地空目录。
- 同 workspace 并发 RW sandbox 被阻止。
- Docker 安全测试无法获得 root、Secret、`SYS_ADMIN` 或 mounter 控制能力。
- 1 GiB 文件传输时 sandbox-api 内存不随文件大小线性增长。
- 对象存储恢复后，Kubernetes sidecar 和 Docker supervisor 能按策略恢复或稳定进入 error 状态。

性能基线不在文档中预设绝对数值；灰度前在目标 MinIO、OBS 网络环境采集旧 sync 与新 FUSE 的创建延迟、首读延迟、顺序吞吐和 10,000 小文件耗时，以实测结果确定发布阈值。

## 18. 灰度与回滚

### 18.1 灰度

1. 保留 `workspace.mode=sync` 默认值，完成单元和集成测试。
2. 在测试环境分别完成四象限验证并固定 s3fs 镜像 digest。
3. MinIO Kubernetes 小流量开启 `mode=fuse`。
4. 扩展到 MinIO Docker。
5. 扩展到 OBS Kubernetes。
6. 最后扩展到 OBS Docker。
7. 观察至少一个完整 sandbox TTL 周期后再扩大流量。

### 18.2 回滚

- 回滚只对新建 sandbox 生效；已经运行的 FUSE sandbox 保持原模式直到销毁。
- 将 `workspace.mode` 改回 `sync` 后，新建 sandbox 使用 legacy sync。
- 对象数据保持原生布局，无需迁移文件内容。
- 首次回滚挂载应执行一次全量同步，不依赖 s3fs 写入的 mtime/metadata 与旧增量基线完全一致。
- 不允许在运行中的 FUSE sandbox 上原地切换为 sync。

## 19. 官方参考资料

- goofys README 与限制：https://github.com/kahing/goofys
- goofys releases：https://github.com/kahing/goofys/releases
- s3fs README、兼容性与限制：https://github.com/s3fs-fuse/s3fs-fuse
- s3fs releases：https://github.com/s3fs-fuse/s3fs-fuse/releases
- MinIO S3 compatibility：https://github.com/minio/minio/blob/master/README.md
- 华为云 CCE OBS 对象桶挂载：https://support.huaweicloud.com/usermanual-cce/cce_10_0630.html
- Kubernetes Sidecar Containers：https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/
- Kubernetes Mount Propagation：https://kubernetes.io/docs/concepts/storage/volumes/#mount-propagation
- Docker Exec 用户参数：https://docs.docker.com/reference/cli/docker/container/exec/
