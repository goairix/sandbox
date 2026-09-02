# Workspace 容器内 FUSE 直接挂载设计

**日期：** 2026-09-01

**复审修订：** 2026-09-02

**状态：** 已完成可实施性复审修订，待最终确认
**目标分支：** `feat/workspace-fuse-mount`

## 1. 决策摘要

Workspace 从“sandbox-api 中转 tar 并定期同步”改为“在 sandbox 所在 Pod 或容器内使用 FUSE 直接挂载对象存储”。`/workspace` 不使用业务 hostPath、节点共享目录或节点级常驻 FUSE DaemonSet。Kubernetes 的 mount propagation 仍会在宿主机 mount namespace 中产生位于 kubelet 管理的 Pod `emptyDir` 路径下的临时子挂载；它不是可被其他 sandbox 复用的宿主机 workspace 目录，并随 Pod 清理。

两个 runtime 使用不同的容器编排方式，但保持相同的数据和 API 语义：

- Kubernetes：每个 sandbox Pod 注入一个可信 FUSE 原生 sidecar；sidecar 与非特权 sandbox 容器通过 memory-backed `emptyDir` 和 mount propagation 共享 `/workspace`。
- Docker：每个 sandbox 使用一个自带 FUSE 的特殊容器；可信 root supervisor 管理 FUSE，所有用户代码和文件命令强制以 UID/GID 1000 执行。
- MinIO 和普通华为 OBS 对象桶一期统一选择 s3fs 客户端族，但使用分别验证和固定的 provider profile；不预设两者必须使用完全相同的二进制或参数。华为 OBS 并行文件系统后续使用 obsfs，不在一期范围内。
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
- sandbox-api 不再为 sandbox 创建、周期同步和销毁中转整棵 FUSE workspace。显式文件上传/下载 API 仍经过 sandbox-api，但必须改为有界内存的流式传输。
- FUSE 凭证不暴露给 Kubernetes sandbox 主容器中的用户进程。
- 保持对象原生布局，使现有 `goairix/fs` 驱动仍能读写同一批对象。
- 挂载、flush、故障、重建、销毁和持久 sandbox 恢复都有明确状态和可观测性。

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

### 4.3 Provider profile 前置验证

实现公共 runtime 接口前，先制作最小 mounter 镜像并完成两个 provider 的挂载 spike。验证产物包括镜像 digest、完整参数数组、服务端签名版本、目录 marker 行为、TLS 校验和读写测试结果。

- MinIO profile：上游 s3fs、显式 `url`、`use_path_request_style`、SigV4 和完整 TLS 校验。
- 华为 OBS profile：必须实测上游 s3fs 与目标 OBS 区域；重点验证 `sigv2`、region、`compat_dir`/`support_compat_dir`、`big_writes` 和 multipart 参数。不得直接照搬 Everest 默认的 `no_check_certificate` 或 `ssl_verify_hostname=0`。
- 如果上游 s3fs 无法在开启证书校验时通过 OBS 四象限测试，则 OBS 使用单独固定 digest 的华为兼容构建；不能为了统一镜像而关闭 TLS 校验。
- spike 未通过时，`provider=obs` 的 `mode=fuse` 配置校验必须失败，不能静默使用未经验证的参数。

### 4.4 版本策略

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

Sidecar 在挂载前把底层 `workspace` anchor 设置为 `root:root`、mode `0555`，且不配置 `fsGroup` 赋予 sandbox 写权限。只有覆盖其上的 FUSE mount 对 UID/GID 1000 可写；如果 mount 消失，sandbox 对暴露出的 emptyDir 只能读、不能创建或修改文件，从内核权限层面 fail closed。

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

`Bidirectional` 仅用于可信 privileged sidecar；sandbox 主容器保持非特权。Pod 不使用 hostPath，也不在宿主机创建可复用的 workspace 路径。该模式不是“宿主机 mount namespace 完全不可见”：FUSE 子挂载会先传播回 kubelet 管理的 Pod volume 路径，再传播给 sandbox 容器。如果安全要求禁止任何回传宿主机 mount namespace，则 sidecar 方案不可用，需要重新选择 CSI 或单容器模型。

### 5.3 启动顺序

1. Kubelet 启动 `workspace-mounter`。
2. Sidecar 从只挂载给自身的 Secret volume 读取凭证，生成 mode `0600` 的临时密码文件。
3. Sidecar 前台启动 s3fs，将单个 workspace prefix 挂载到 `/workspace`。
4. `startupProbe` 检查 mount 类型和一次远端读写探测；通过后才允许后续 init container 启动。
5. startup probe 成功后，Kubelet 启动普通 init container `workspace-ready`。
6. `workspace-ready` 以 `HostToContainer` 挂载同一个 workspace volume，使用 UID/GID 1000 创建、读取并删除一个随机命名的保留探测文件。
7. 探测通过后启动 `sandbox` 主容器。
8. sandbox-api 只有在 Pod Running、主容器 Ready 且 mounter Ready 时才返回创建成功。

Sidecar 同时配置 `readinessProbe`。startup probe 用于启动顺序，readiness probe 持续反映 mount 和远端访问状态，并参与 Pod Ready；runtime 等待逻辑必须检查 Pod Ready condition 和 sidecar init container status，不能只检查 Pod Phase=Running。

一期采用固定的挂载参数基线，禁止由 API 调用方传入任意 s3fs 参数：

- 通用参数包含 `allow_other`、`uid=1000`、`gid=1000`、`umask=0022`、`mp_umask=0022`、前台运行和有界本地缓存；mounter 镜像中的 `/etc/fuse.conf` 仅启用 `user_allow_other`。
- MinIO 使用显式 endpoint、`use_path_request_style` 和配置的 TLS 校验，不依赖 AWS 域名推导。
- 华为 OBS 普通对象桶使用第 4.3 节验证产出的固定 profile；签名版本、兼容目录参数和 path-style 行为均不能由租户覆盖。
- 生产环境禁止 `-o passwd_file` 之外的命令行明文凭证，也禁止 `-o ssl_verify_hostname=0`、跳过证书校验或任意 `url` 覆盖。
- 挂载参数通过参数数组生成，并对 endpoint、bucket 和 prefix 分别校验，不能拼接为 shell 命令。

### 5.4 Sidecar 权限与隔离

`workspace-mounter`：

- 使用固定 digest 的可信镜像。
- `privileged: true`，挂载 `/dev/fuse`。
- Secret 只挂载到 sidecar，不使用会被主容器读取的共享环境变量。
- s3fs 密码文件位于 sidecar 私有 tmpfs，权限 `0600`。
- 不暴露监听端口。
- 一期使用的凭证有效期必须覆盖 sandbox 最大存活时间和卸载宽限期。若使用会过期的临时凭证，`timeout=-1` 的 FUSE sandbox 必须拒绝创建；凭证轮换通过受控重建 sandbox 完成，不假设运行中的 s3fs 自动重载 Secret。

`sandbox`：

- `runAsUser: 1000`、`runAsGroup: 1000`。
- `allowPrivilegeEscalation: false`。
- 不挂载 `/dev/fuse`、Secret 或 sidecar 私有目录。
- 不增加 `SYS_ADMIN`。
- Pod 的 `shareProcessNamespace` 保持 `false`。
- 继续禁用 ServiceAccount token 和 service links。

### 5.5 Sidecar 健康、故障与销毁

- `readinessProbe` 检测 mount 存在和有界超时的远端只读探测；失败时 Pod NotReady，workspace 状态为 `recovering`，新的 Exec 返回 503。
- `livenessProbe` 只检测 s3fs 进程死亡、FUSE mount 消失或本地控制循环失去响应。普通对象存储网络中断不能触发反复重启和 lazy unmount。
- s3fs 进程死亡会使已有 fd、cwd 和 mmap 指向失效的 FUSE 实例。manager 必须取消活动 Exec；默认恢复方式是销毁并重建整个 Pod。只有 runtime 能证明没有活动 Exec 时，才允许有界的原地 unmount/remount。
- mount state 以 runtime 的实时 probe/status 为准，Redis session 中的状态只用于恢复提示，不能单独作为 Exec 放行依据。
- 正常销毁由 sandbox-api 先拒绝新 Exec、等待或取消活动 Exec，再通过可信 runtime 控制路径执行 `syncfs` 并确认成功，然后删除 Pod。主容器应快速退出；sidecar 在主容器退出后完成 unmount。
- sidecar 的 SIGTERM/preStop 是 unmount 的正常收尾，也是 API 异常时的 flush 兜底，但不能作为唯一 flush 机制，因为主容器可能消耗大部分 Pod termination grace period。Pod 必须配置足够的 termination grace period，主容器不得设置长时间 preStop。

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

镜像中的底层 `/workspace` 目录固定为 `root:root`、mode `0555`。supervisor 以 root 将 FUSE 覆盖挂载到该目录；如果 FUSE 消失，UID 1000 用户不能把数据写入 container writable layer。

该模型的安全边界弱于 Kubernetes sidecar，因为容器本身持有 `SYS_ADMIN`。所有用户可触发路径都必须强制 UID/GID 1000，且不得提供可切换到 root 的命令或文件能力。

### 6.4 Docker Exec 用户约束

Docker runtime 必须把 exec 分成两个互不复用的入口：

- `execUser`：所有 API 用户可触发的命令，固定 `User: "1000:1000"`。
- `execControl`：runtime 内部私有的 root 控制入口，只接受代码内固定 argv allowlist，用于 route、health、flush 和 unmount；不接受请求参数中的 shell、命令或容器名。

以下用户路径必须走 `execUser`，不能依赖容器默认用户：

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

因此 FUSE workspace Pod 即使 `network.enabled=false`，仍需要为整个 Pod 放行不可被用户配置删除的 system egress：

- 目标 MinIO 或 OBS endpoint 的受控网络路径；
- 精确端口，默认 HTTPS 443 或 MinIO 配置端口；
- 解析该 endpoint 所需的 DNS 服务。

system egress 的实现按集群能力固定：

- Cilium 集群优先使用 FQDN/service aware policy，并单独允许 CoreDNS。
- 标准 NetworkPolicy 不支持稳定的 FQDN 策略，不允许把启动时的一次 DNS 解析结果当成长期规则。必须使用运维配置的稳定 CIDR、专用 egress proxy，或具有稳定地址的对象存储 endpoint。
- 当前 Pod 使用 `DNSPolicy=None` 和公共 nameserver。若 MinIO 是 `cluster.local` Service，必须显式改为允许 CoreDNS 的受控 DNS 配置，或者通过专用稳定 endpoint 访问；不能假设公共 DNS 能解析集群 Service。

Sandbox 主容器也能连接这些地址，但没有存储凭证。必须使用最小网络范围和最小权限凭证降低风险：

- 禁止放开整个 VPC、集群或对象存储网段。
- MinIO 优先使用独立 endpoint 或专用负载均衡地址，避免向 Pod 开放整个集群服务网段。
- 禁止匿名 bucket 访问。
- 存储策略只授权当前 workspace prefix。
- 凭证不得出现在 Pod 共享环境变量、主容器文件系统或 API 响应中。

### 7.2 Docker 约束

Docker 特殊容器同样需要访问对象存储 endpoint。FUSE workspace 无论 `network.enabled` 是否开启，都创建 gateway pair，并将对象存储 endpoint 作为不可被用户配置删除的 system egress；用户网络规则在其上追加。用户进程可连接 endpoint，但不能获得凭证。

## 8. Workspace 隔离与租约

### 8.1 路径规范化

`workspace_path` 在用于对象前缀、日志、Redis key 或 mount 参数前必须统一规范化：

- 必须为非空相对路径。
- 拒绝 `.`、`..`、绝对路径、NUL 和路径逃逸。
- 清理重复 `/`，规范化后必须仍位于存储 driver 根目录下。
- 拒绝系统保留前缀，例如 `.sandbox-system`。
- mount 命令通过参数数组执行，不拼接 shell 字符串。

最终对象前缀：

```text
<storage.filesystem.sub_path>/<workspace_path>/
```

`storage.filesystem.sub_path` 已由现有 `goairix/fs` driver 在内部叠加；FUSE 直接连接 bucket 时必须显式补上且只能补一次。`WorkspaceInfo.RootPath` 继续保存 API 传入并规范化后的 `workspace_path`，保证 legacy sync、现有对象和 FUSE 指向同一批 key，不新增 `workspace.object_prefix`，也不要求迁移对象。

每个 FUSE 进程直接挂载该前缀，sandbox 看不到同 bucket 的父目录或兄弟 workspace。

### 8.2 Redis 独占租约

s3fs 不协调多个客户端对同一对象的并发修改，因此创建 FUSE workspace 前必须获取独占租约：

```text
sandbox:workspace-lease:<provider>:<endpoint-hash>:<bucket>:<normalized-prefix>
```

租约 value 包含 sandbox ID、runtime、runtime ID、创建时间和 fencing generation。除 TTL lease 外，另保存不自动过期的 workspace owner 记录，并把 workspace hash 与 generation 写入 Pod/container label，供 Redis 状态异常时从 runtime 反查。规则：

- 同一 workspace 同时只允许一个 `rw` sandbox。
- sandbox-api 以不大于 TTL 三分之一的间隔续租。
- 续租失败立即把 workspace 置为 unavailable 并拒绝新 Exec；持续失败时取消活动 Exec、flush 并停止 sandbox。
- 正常销毁时 compare-and-delete 释放，不能删除其他 generation 的租约。
- TTL lease 消失不能自动删除 owner 记录。
- 接管前必须同时检查 owner 记录、持久 session，以及带相同 workspace label 的 Pod/container；只有确认旧 runtime 已停止或删除，才允许清理旧 owner 并创建新 generation。
- API 多副本共享同一 Redis 状态，不能只使用进程内锁。

普通对象存储无法强制 fencing token，因此 generation 只用于检测和审计，不是强一致分布式锁。实现不能依赖“TTL 到期即安全”；旧 sandbox 是否仍在运行始终是接管前置条件。如果 runtime 状态无法确认，workspace 保持 blocked 并要求人工处理。

## 9. 配置设计

建议扩展 `WorkspaceConfig`，并将 provider 差异收敛到经过验证的 profile：

```go
type WorkspaceFUSEProviderConfig struct {
    Driver            string   `mapstructure:"driver"`
    Profile           string   `mapstructure:"profile"`
    MounterImage      string   `mapstructure:"mounter_image"`
    DockerImage       string   `mapstructure:"docker_image"`
    SystemEgressFQDNs []string `mapstructure:"system_egress_fqdns"`
    SystemEgressCIDRs []string `mapstructure:"system_egress_cidrs"`
}

type WorkspaceConfig struct {
    AutoSyncIntervalSeconds   int                                    `mapstructure:"auto_sync_interval_seconds"`
    Mode                      string                                 `mapstructure:"mode"` // "sync" | "fuse"
    SecretName                string                                 `mapstructure:"secret_name"`
    CacheSize                 string                                 `mapstructure:"cache_size"`
    CacheMedium               string                                 `mapstructure:"cache_medium"` // "disk" | "memory"
    MountTimeoutSeconds       int                                    `mapstructure:"mount_timeout_seconds"`
    FlushTimeoutSeconds       int                                    `mapstructure:"flush_timeout_seconds"`
    UnmountTimeoutSeconds     int                                    `mapstructure:"unmount_timeout_seconds"`
    RecreateMaxAttempts       int                                    `mapstructure:"recreate_max_attempts"`
    LeaseTTLSeconds           int                                    `mapstructure:"lease_ttl_seconds"`
    LeaseRenewIntervalSeconds int                                    `mapstructure:"lease_renew_interval_seconds"`
    QuotaMode                 string                                 `mapstructure:"quota_mode"` // FUSE 一期固定 "soft"
    Providers                 map[string]WorkspaceFUSEProviderConfig `mapstructure:"providers"`
}
```

配置约束：

- `mode=fuse` 时 runtime 必须为 Kubernetes 或 Linux Docker Engine。
- `provider=minio|obs` 时一期只允许验证通过的 s3fs profile；profile 没有对应测试证据时启动失败。
- Kubernetes 必须为当前 provider 配置固定 digest 的 mounter image 和 Secret。
- Docker 必须为当前 provider 配置固定 digest 的特殊 sandbox image 和 Secret 来源。
- `lease_renew_interval_seconds` 必须不大于 `lease_ttl_seconds / 3`。
- `quota_mode=soft` 必须显式配置，避免把现有 `max_disk` 误认为 FUSE workspace 硬限制。
- `cache_medium=disk` 会在节点临时盘保存有界的明文缓存块；禁止节点落盘时必须显式使用 `memory`，并把 cache 纳入 Pod/container 内存限制。
- Secret、AK、SK 不得通过日志输出。
- `mount_timeout_seconds`、`flush_timeout_seconds`、`lease_ttl_seconds` 和 cache size 必须为正值并设置安全默认值。

示例：

```yaml
workspace:
  mode: "fuse"
  secret_name: "sandbox-storage-secret"
  cache_size: "2Gi"
  cache_medium: "disk"
  mount_timeout_seconds: 30
  flush_timeout_seconds: 30
  unmount_timeout_seconds: 15
  recreate_max_attempts: 1
  lease_ttl_seconds: 120
  lease_renew_interval_seconds: 30
  quota_mode: "soft"
  providers:
    minio:
      driver: "s3fs"
      profile: "minio-sigv4-path-style-v1"
      mounter_image: "${SANDBOX_MINIO_MOUNTER_IMAGE}"
      docker_image: "${SANDBOX_MINIO_FUSE_RUNTIME_IMAGE}"
      system_egress_fqdns: ["${SANDBOX_MINIO_ENDPOINT_HOST}"]
    obs:
      driver: "s3fs"
      profile: "huawei-obs-verified-v1"
      mounter_image: "${SANDBOX_OBS_MOUNTER_IMAGE}"
      docker_image: "${SANDBOX_OBS_FUSE_RUNTIME_IMAGE}"
      system_egress_fqdns: ["${SANDBOX_OBS_ENDPOINT_HOST}"]
```

## 10. Runtime 与状态模型

### 10.1 Runtime spec

`runtime.SandboxSpec` 增加可选 FUSE 描述：

```go
type WorkspaceFUSESpec struct {
    Provider        string
    Driver          string
    Profile         string
    MounterImage    string
    SecretName      string
    Bucket          string
    Prefix          string
    Endpoint        string
    Region          string
    UseSSL          bool
    CacheSize       string
    LeaseGeneration int64
    MountTimeout    time.Duration
    FlushTimeout    time.Duration
    SystemEgress    SystemEgressSpec
}
```

Spec 只携带 Secret 引用，不携带明文 AK/SK。Docker 如无法通过名称引用 Secret，则传递 root-only secret file descriptor/path，由 runtime 管理生命周期，不能写入持久 session。

runtime 接口增加可信控制能力：

```go
WorkspaceHealth(ctx context.Context, id string) (*WorkspaceHealth, error)
FlushWorkspace(ctx context.Context, id string) error
```

Kubernetes 实现只能对固定名称的 mounter sidecar 执行固定控制命令；Docker 实现只能走私有 `execControl`。这些能力不能从公共 Exec 请求中选择容器、用户或 argv。

现有上传接口还需要把 multipart 已知的文件大小传入 runtime，使 tar header 可以先写出并通过 pipe 直接流向 Docker/Kubernetes，而不是 `io.ReadAll`：

```go
UploadFile(ctx context.Context, id, destPath string, size int64, reader io.Reader) error
```

实现必须校验实际读取字节数与声明大小一致，并设置上传大小上限；该调整不改变公共 HTTP multipart 请求格式。

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
    WorkspaceMountFlushing   WorkspaceMountState = "flushing"
    WorkspaceMountBlocked    WorkspaceMountState = "blocked"
    WorkspaceMountError      WorkspaceMountState = "error"
    WorkspaceMountUnmounting WorkspaceMountState = "unmounting"
)
```

`WorkspaceInfo` 保存 mount type、mount state、driver、最后健康时间、最后 flush 时间、owner 和租约 generation。旧 session 中 `BindMounted=true` 迁移为 local bind 兼容状态；不存在新字段的远端 workspace 继续解释为 sync，不自动升级为 FUSE。

## 11. API 语义

### 11.1 创建

创建请求带 `workspace_path` 且 `workspace.mode=fuse` 时：

- 获取独占租约；
- 绕过 pool，直接创建 Pod/容器；
- 等待 FUSE ready；
- 注册 workspace 状态，但不执行文件复制。

无 workspace 的 sandbox 继续使用 pool。legacy sync 模式保持现有行为。

### 11.2 SyncWorkspace

FUSE workspace 不再复制整棵目录，但保留 `SyncWorkspace` 的持久化语义：

- `from_container` 调用 runtime `FlushWorkspace`，进入 `flushing` 状态，等待 `syncfs/fsync` 完成；成功后返回 `files_synced=0`、`flushed=true` 和 `last_flushed_at`。
- flush 与用户 Exec 使用同一个互斥门：已有 Exec 未结束时等待到请求超时或返回冲突，flush 期间不允许启动新 Exec，确保成功响应对应一个明确的持久化边界。
- flush 超时或 s3fs 返回错误时，API 返回错误，不能把健康检查成功当成数据已持久化。
- `to_container` 不执行复制并返回明确 no-op；其前提是持有租约期间禁止其他客户端或管理工具修改同一 prefix。s3fs 不提供可靠的跨客户端缓存失效，因此外部修改不属于一期支持语义。
- 响应包含 `mount_type=fuse`，区分“未复制但已 flush”和 legacy sync 的文件复制计数。

auto-sync 完全跳过 FUSE workspace。

### 11.3 运行中 MountWorkspace/UnmountWorkspace

Kubernetes Pod spec 和 Docker mount namespace 不能通过现有 runtime 抽象安全地动态增加或移除该挂载：

- FUSE 模式下，对运行中无 workspace sandbox 调用 `MountWorkspace` 返回 HTTP 409，要求创建时提供 `workspace_path`。
- FUSE 模式下，`UnmountWorkspace` 返回 HTTP 409，workspace 随 sandbox 销毁卸载。
- sync 模式继续支持现有动态 mount/unmount API。

不得静默切换到 legacy sync，也不得重建容器而不告知调用方。

### 11.4 Exec

- mount state 为 `ready` 时允许 Exec。
- `pending`、`recovering`、`flushing`、`blocked`、`error`、`unmounting` 时返回 workspace unavailable，HTTP 503。
- Docker 所有公共 API 和用户可触发的 Exec 强制 UID/GID 1000；只有不可由请求传参的 runtime 私有 `execControl` 可以使用 root。
- Kubernetes Exec 固定目标容器 `sandbox`，不能允许调用方选择 sidecar。
- Upload、Download、Read、List、Glob、Edit 等公共文件操作使用相同的实时 workspace health gate；runtime 在执行 tar/copy 前再次验证 FUSE mount，防止检查后挂载消失时写入底层 emptyDir 或 writable layer。

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

双向可见性不等于允许并发写。FUSE 租约存活期间：

- 公共文件 API 继续通过 runtime 操作容器内 `/workspace`，不能绕过 mount 直接调用 `goairix/fs` 写对象。
- `goairix/fs` 可用于只读下载、签名和管理检查；会修改同一 prefix 的后台任务必须等待 owner 释放。
- 运维人员或外部工具直接写对象后的缓存刷新不属于一期保证，不能通过 `SyncWorkspace(to_container)` 伪装成强制刷新。

## 13. 容量、缓存与性能边界

### 13.1 容量限制

FUSE workspace 不再使用 Kubernetes workspace emptyDir，因此当前 `max_disk` 不构成 workspace 硬配额。普通对象桶和 s3fs 不提供 prefix 级硬配额。

一期只能采用显式接受的软配额：

- 周期统计 workspace prefix 对象总量和容量。
- 超过阈值后拒绝新的 Exec 和文件写 API，取消仍在执行的用户命令，并触发 sandbox flush/终止策略。仅拒绝新请求不能阻止已有进程继续写入。
- 指标与告警必须在接近阈值时提前触发。
- 文档和 API 明确软配额存在采样延迟，不能作为强安全边界。

若业务要求强配额，应使用每租户/每 workspace 独立 bucket 配额，或改用提供元数据与 quota 的文件系统；不在本设计内模拟硬配额。`mode=fuse` 与 `quota_mode=soft` 必须同时显式启用，部署文档需要说明这是相对于现有 `max_disk` 的能力变化。

### 13.2 s3fs 临时空间

s3fs 写入可能使用本地临时文件：

- Kubernetes 使用独立、带 sizeLimit 的 `fuse-cache` emptyDir；它不是完整 workspace，但可能包含文件分片或缓存内容。
- Docker 使用受限 scratch/cache mount，禁止无限使用 container writable layer。
- `cache_medium=disk` 使用节点临时盘，要求节点磁盘加密并在 Pod/容器销毁后清理；`cache_medium=memory` 使用 tmpfs，但会增加内存压力并限制可安全处理的文件大小。
- 即使不启用 s3fs 持久 `use_cache`，s3fs 写入仍需要临时空间；“不使用宿主机 workspace”不等于“节点上绝不出现临时文件”。
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
| 运行中 FUSE 进程死亡 | 拒绝新 Exec，取消活动 Exec；默认重建 Pod/容器，仅在零活动 Exec 时允许原地重挂载 |
| 重建失败 | mount state=`error`，返回 503，等待销毁或人工诊断 |
| 对象存储网络中断 | readiness 失败并返回 503，不触发 liveness 重启，不切换到本地目录 |
| cache 满 | 写返回 ENOSPC，记录 workspace/cache 指标 |
| Redis 租约冲突 | 返回 HTTP 409，包含占用 sandbox ID |
| Redis 续租失败 | 立即阻止新 Exec；持续失败则取消 Exec、flush 并停止 sandbox，保留 owner 记录 |
| API 重启 | 从 owner/session 和 runtime label 恢复状态并检查 mounter 健康；不重复挂载已有健康实例 |
| Pod/容器丢失 | 清理 session 和租约；按持久 sandbox 规则重新创建 |
| 凭证即将过期或轮换 | 阻止新 Exec，flush 后受控重建；不期望 s3fs 热加载 Secret |
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
- `sandbox_workspace_flush_duration_seconds`
- `sandbox_workspace_flush_total{runtime,provider,result}`
- `sandbox_workspace_recovery_total{runtime,provider,result}`
- `sandbox_workspace_unavailable_total{reason}`
- `sandbox_workspace_lease_conflict_total`
- `sandbox_workspace_lease_lost_total{runtime}`
- `sandbox_workspace_owner_blocked_total{reason}`
- `sandbox_workspace_fuse_cache_bytes`
- `sandbox_workspace_fuse_errors_total{operation}`

Pod 和 Docker health 状态必须区分 container running 与 FUSE ready，不能仅凭主进程存活判定 workspace 健康。

## 16. 代码变更边界

| 文件/组件 | 设计变更 |
|---|---|
| `internal/config/config.go` | Workspace FUSE 配置、默认值与校验 |
| `internal/runtime/types.go`、`internal/runtime/runtime.go` | `WorkspaceFUSESpec`、system egress、health/flush 控制接口、带 size 的流式上传签名 |
| `internal/sandbox/types.go` | Workspace mount type/state/driver/health/lease 字段 |
| `internal/sandbox/manager.go` | FUSE direct create、绕过 pool、租约、restore、健康门控 |
| `internal/sandbox/workspace.go` | FUSE 模式跳过 sync；动态 mount/unmount 冲突语义 |
| `internal/runtime/kubernetes/pod.go` | 原生 sidecar、memory emptyDir、propagation、startup/readiness/liveness probe、Secret、完整 Ready 等待 |
| `internal/runtime/kubernetes/network.go` | system egress 与用户网络规则合并、CoreDNS/FQDN 或稳定出口策略 |
| `internal/runtime/kubernetes/exec.go` | 保持 exec 固定到 `sandbox`；错误映射 |
| `internal/runtime/docker/container.go` | 特殊镜像、FUSE device/capability/security/health、root supervisor 与用户 exec 分离 |
| `internal/runtime/docker/exec.go` | `execUser` 强制 UID/GID 1000；私有固定 argv `execControl` |
| `internal/runtime/docker/network.go` | network disabled 时仍创建仅含 system egress 的 gateway |
| `internal/runtime/docker/file.go` | 直接 Exec 强制用户；CopyToContainer ownership；上传改为流式 tar |
| `internal/runtime/kubernetes/file.go` | 上传改为 pipe 流式 tar，禁止 `io.ReadAll` 整文件缓存 |
| `internal/api/handler/file.go`、`internal/sandbox/manager.go` | 从 multipart header 透传 size，执行大小和 workspace health 校验 |
| `pkg/types/workspace.go`、`internal/api/handler/workspace.go` | 为 flush/no-op 增加向后兼容的 mount type、flushed、last flushed 等响应字段 |
| `internal/storage/state/redis` | workspace TTL lease、持久 owner 记录与 generation |
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
- 显式文件上传/下载 API 仍可用，1 GiB 上传过程中 sandbox-api 内存保持有界。
- `git clone/status/checkout`。
- pip/npm install，cache 指向 `/tmp`。
- 动态 MountWorkspace/UnmountWorkspace 在 FUSE 模式返回 409。
- `SyncWorkspace(from_container)` 执行 flush barrier；`to_container` 返回受约束的 no-op，二者均不复制整棵目录。

### 17.3 安全测试

- 路径穿越、绝对路径、特殊字符和 shell 参数注入。
- sandbox 主容器无法读取 Kubernetes Secret、sidecar `/proc` 或 `/dev/fuse`。
- Docker 用户命令 UID/GID 始终为 1000，覆盖所有直接和间接 exec 路径。
- Docker 用户无法读取 root-only Secret、控制 supervisor 或执行 mount/umount。
- 公共请求无法触发 `execControl`、指定 mounter 容器或传入 root 控制 argv。
- symlink 无法逃逸到同 bucket 的其他 workspace。
- NetworkPolicy/gateway 只放行目标 endpoint 和 DNS。
- 同 workspace 第二个读写创建请求稳定返回租约冲突。

### 17.4 故障测试

- kill s3fs、kill Kubernetes sidecar、重启 Docker supervisor 子进程。
- 在活动 Exec 持有 cwd/fd 时 kill s3fs，验证 Exec 被取消且 sandbox 重建，不把旧句柄标记为已恢复。
- 强制移除 FUSE mount 后尝试通过 Exec 和文件 API 写入，验证底层 emptyDir/容器目录返回权限错误且没有本地文件残留。
- 对象存储断网、DNS 失败、证书错误、AK/SK 失效。
- mount 过程中 Pod/容器被删除。
- 写入中 cache 满。
- Pod 驱逐、节点重启、Docker daemon 重启、sandbox-api 重启。
- 优雅卸载失败和 stale FUSE mount 恢复。
- Redis 短暂不可用、租约续期失败、API 崩溃超过一个 TTL、owner 记录恢复和安全接管。
- 长生命周期 sandbox 的凭证过期与受控重建。

### 17.5 验收标准

- 四象限所有 A 类功能测试通过。
- FUSE workspace 的创建、运行和销毁路径不调用 tar 同步方法。
- 挂载未 ready 时用户代码不会启动；runtime 不能只以 Pod Running 或容器主进程存活判定 ready。
- 任一 FUSE 故障不会导致数据写入本地空目录。
- 同 workspace 并发 RW sandbox 被阻止，包括 Redis lease 已过期但旧 runtime 仍存活的场景。
- Docker 安全测试无法获得 root、Secret、`SYS_ADMIN` 或 mounter 控制能力。
- 1 GiB 显式上传和 FUSE 文件传输时 sandbox-api 内存不随文件大小线性增长。
- 对象存储恢复后，Kubernetes sidecar 和 Docker supervisor 能按策略恢复或稳定进入 error 状态。

性能基线不在文档中预设绝对数值；灰度前在目标 MinIO、OBS 网络环境采集旧 sync 与新 FUSE 的创建延迟、首读延迟、顺序吞吐和 10,000 小文件耗时，以实测结果确定发布阈值。

## 18. 灰度与回滚

### 18.1 灰度

1. 保留 `workspace.mode=sync` 默认值，先完成 MinIO/OBS provider spike，产出固定 profile、镜像 digest 和 TLS/签名验证结果。
2. 完成 runtime、租约、flush、system egress、流式文件 API 的单元和集成测试。
3. 在测试环境分别完成四象限验证。
4. MinIO Kubernetes 小流量开启 `mode=fuse`。
5. 扩展到 MinIO Docker。
6. 扩展到 OBS Kubernetes。
7. 最后扩展到 OBS Docker。
8. 观察至少一个完整 sandbox TTL 周期，并完成一次 Redis/API 故障接管演练后再扩大流量。

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
- s3fs 参数、缓存与临时空间：https://github.com/s3fs-fuse/s3fs-fuse/blob/master/doc/man/s3fs.1.in
- s3fs releases：https://github.com/s3fs-fuse/s3fs-fuse/releases
- MinIO S3 compatibility：https://github.com/minio/minio/blob/master/README.md
- 华为云 CCE OBS 对象桶挂载：https://support.huaweicloud.com/usermanual-cce/cce_10_0630.html
- 华为云 CCE OBS 挂载参数：https://support.huaweicloud.com/intl/zh-cn/usermanual-cce/cce_10_0631.html
- Kubernetes Sidecar Containers：https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/
- Kubernetes Mount Propagation：https://kubernetes.io/docs/concepts/storage/volumes/#mount-propagation
- Docker Exec 用户参数：https://docs.docker.com/reference/cli/docker/container/exec/
