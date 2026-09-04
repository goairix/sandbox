# Workspace 容器内 FUSE 直接挂载设计

**日期：** 2026-09-01

**复审修订：** 2026-09-03

**状态：** 设计已确认，待实现

**目标分支：** `feat/workspace-fuse-mount`

**配套部署手册：** [Workspace FUSE 部署与运维手册](../../deployment/workspace-fuse.md)。该手册给出 Kubernetes sidecar 与 Docker 特殊容器的目标部署契约、MinIO/华为 OBS 配置模板、Secret 管理、验证、灰度及回滚步骤；在实现合入前，模板中的新增配置项不可直接用于当前版本。

## 1. 决策摘要

Workspace 从“sandbox-api 中转 tar 并定期同步”改为“在 sandbox 所在 Pod 或容器内使用 FUSE 直接挂载对象存储”。`/workspace` 不使用业务 hostPath、节点共享目录或节点级常驻 FUSE DaemonSet。Kubernetes 的 mount propagation 仍会在宿主机 mount namespace 中产生位于 kubelet 管理的 Pod `emptyDir` 路径下的临时子挂载；它不是可被其他 sandbox 复用的宿主机 workspace 目录，并随 Pod 清理。

两个 runtime 使用不同的容器编排方式，但保持相同的数据和 API 语义：

- Kubernetes：每个 sandbox Pod 注入一个可信 FUSE 原生 sidecar；sidecar 与非特权 sandbox 容器通过 memory-backed `emptyDir` 和 mount propagation 共享 `/workspace`。
- Docker：每个 sandbox 使用一个自带 FUSE 的特殊容器；可信 root supervisor 管理 FUSE，所有用户代码和文件命令强制以 UID/GID 1000 执行。
- 两个 runtime 都预热“未绑定 workspace”的 locked 空壳实例。provider、endpoint、bucket、profile、镜像、Secret 与 system egress 在预热时固定；只有 `workspace_path/prefix` 在 Pool Acquire 时确定并触发 s3fs 挂载。空壳一旦消费挂载授权，无论成功失败都必须销毁，不得卸载后回池复用。
- FUSE Pool 与当前通用 Pool 一样由 `sandbox-api` 内的 `sandbox.Manager` 维护目标数量和生命周期，不部署独立 Pool Controller、Operator 或 CronJob。Redis 只为多 API 副本保存库存状态并提供原子保留/refill 互斥，不会自行创建或删除 Pod/容器。
- MinIO 和普通华为 OBS 对象桶一期统一选择 s3fs 客户端族，但使用分别验证和固定的 provider profile；不预设两者必须使用完全相同的二进制或参数。华为 OBS 并行文件系统后续使用 obsfs，不在一期范围内。
- 每个 sandbox 只挂载其自己的对象前缀，不暴露 bucket 中的其他 workspace。
- 一期使用 provider 级静态长期 AK/SK 和 root-only s3fs `passwd_file`；不支持 STS/session token。挂载路径限制用户可见的 prefix，但共享凭证本身不构成单 workspace 的 IAM 隔离边界。
- FUSE cache 默认使用节点磁盘和软容量阈值；超限时 sandbox 进入 error/eviction 并重建，不承诺文件写立即返回 ENOSPC。
- Kubernetes 保持 `DNSPolicy=None` 和当前公共 nameserver；对象存储必须使用公共 DNS 可解析的稳定 endpoint，或由运维把证书匹配的 FQDN 静态映射到已批准的稳定 IP，不能直接配置 `cluster.local` Service。只有证书包含 IP SAN 时才允许直接使用 IP endpoint。对象存储 endpoint 必须进入平台审批的精确 system egress 白名单，配置了 provider endpoint 不等于自动获准访问任意内网地址。
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

当前代码只有 local provider 在 sandbox 创建时使用 bind mount；MinIO、OBS 等远端 provider 都使用同步路径。FUSE 方案将远端 provider 改为：先由 Pool 预热未挂载的容器/Pod，收到带 `workspace_path` 的创建请求时 Acquire 一个空壳并直接挂载，再交付给用户；Pool miss 时才按需创建空壳。同时保留显式的 legacy sync 模式用于兼容和回滚。

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
- 容器调度、镜像拉取和基础进程启动可由 Pool 提前完成；请求路径只承担租约、prefix 初始化、s3fs 启动和挂载探测。

### 3.2 非目标

- 不提供完整 POSIX 文件系统语义。
- 不支持 hardlink、原子目录 rename、跨客户端文件锁或可靠的远端 inotify。
- 不支持 SQLite、数据库文件、mmap 密集写或依赖强文件锁的负载。
- 一期不支持同一 workspace 多 sandbox 并发读写。
- 一期只支持对 Pool 中尚未交付、Exec/file gate 关闭的 locked 空壳做一次 Acquire-time 动态挂载；不支持对已经交付或执行过用户代码的 sandbox 再挂载、卸载或切换 workspace。
- 一期不移除 legacy sync 代码；待灰度稳定后另行清理。
- 一期不实现 prefix 级硬容量配额，因为 s3fs、goofys 和普通对象桶均不提供该能力。
- 一期不支持 STS、session token、临时凭证过期管理或运行中凭证热更新；静态 AK/SK 轮换通过停止新建、排空并重建该 provider 的 FUSE sandbox 完成。

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
- 华为云公有云 CCE 文档明确使用 s3fs 挂载普通 OBS 对象桶、使用 obsfs 挂载并行文件系统。本项目实际使用的双华云私有云 CCE 文档也明确采用相同分工，并显示普通对象桶自动使用 `sigv2`、s3fs 1.92 自动添加 `compat_dir`。公有云与私有云证据共同支持一期的 `obs = 普通对象桶 + s3fs` 选择。
- 上述 CCE 文档描述的是 Everest 集成挂载；私有云文档还说明每个对象存储卷会产生一个常驻进程。它们可以证明客户端分工和资源模型，但不能证明任意 s3fs 镜像、参数组合或自建 sidecar 已获得厂商认证。
- s3fs 保持文件对象的原生数据格式，现有对象 API 和 `goairix/fs` 可继续访问挂载产生的文件。

goofys 仅保留为 MinIO A 类负载的性能对照项。如果 MinIO 的 Kubernetes、Docker 两套 runtime 基准显示 s3fs 无法达到性能目标，再单独评审 goofys 或 geesefs，不在一期同时维护两套客户端。

### 4.3 Provider profile 前置验证

实现公共 runtime 接口前，先制作最小 mounter 镜像并完成两个 provider 的挂载 spike。验证产物包括镜像 digest、完整参数数组、服务端签名版本、目录 marker 行为、TLS 校验和读写测试结果。

- MinIO profile：上游 s3fs、显式 `url`、`use_path_request_style`、SigV4 和完整 TLS 校验。
- 华为 OBS profile：必须实测上游 s3fs 与目标 OBS 区域；重点验证 `sigv2`、region、`compat_dir`/`support_compat_dir`、`big_writes` 和 multipart 参数。不得直接照搬 Everest 默认的 `no_check_certificate` 或 `ssl_verify_hostname=0`。
- 目标私有云在 2023 年部署，而在线帮助中心内容仍在更新；文档页面不能替代环境版本证据。spike 前必须记录实际 OBS 服务版本/补丁、CCE Everest 插件版本，以及现网集成路径使用的 s3fs/obsfs 版本（能获取时）。本项目自建 sidecar 和 Docker 镜像仍以目标 endpoint 的实测结果为准。
- 华为公有云与私有云必须分别完成 spike，使用不同的 profile ID、测试证据和镜像 digest；不得因为两者都选择 s3fs 就复用未经验证的签名、目录或 TLS 参数。
- 如果上游 s3fs 无法在开启证书校验时通过对应公有云或私有云 OBS profile 的 Kubernetes、Docker 测试，则该 profile 使用单独固定 digest 的华为兼容构建；不能为了统一镜像而关闭 TLS 校验。
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
│  │    ├─ Pool 中保持 prepared + locked              │
│  │    └─ Acquire 后 s3fs bucket:/prefix → /workspace│
│                                                     │
│  containers                                        │
│  └─ sandbox             已启动，Exec/file gate 关闭 │
│                                                     │
│  memory-backed emptyDir + mount propagation         │
│                     /workspace                      │
└─────────────────────────────────────────────────────┘
```

使用 Kubernetes 原生 sidecar：`workspace-mounter` 位于 `initContainers`，设置 `restartPolicy: Always`。最低支持 Kubernetes 1.29，生产推荐 1.33 或更高版本。startup probe 在 supervisor 进入 prepared/locked 后成功，使 sandbox 主容器能在 Pool 预热阶段启动；mounter readiness 在尚未挂载时保持失败，因此 Pod NotReady 是空壳的预期状态，Pool 不能以 Pod Ready 判断空壳健康。

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

`workspace` 只作为 FUSE mount 的传播锚点，不保存文件数据。`fuse-cache` 为 s3fs 临时写入和缓存提供节点磁盘空间；`sizeLimit` 与 Pod `ephemeral-storage` request/limit 用于调度、监控和超限驱逐，不视为文件系统硬 quota。

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

`Bidirectional` 仅用于可信 privileged sidecar；sandbox 主容器保持非特权。`workspace` volume 不使用 hostPath，也不在宿主机创建可复用的业务 workspace 路径。为了向 sidecar 暴露 FUSE 字符设备，部署可以单独使用指向 `/dev/fuse` 的 `hostPath.type=CharDevice`，或使用集群提供的设备插件；该设备映射不承载 workspace 数据。该模式不是“宿主机 mount namespace 完全不可见”：FUSE 子挂载会先传播回 kubelet 管理的 Pod volume 路径，再传播给 sandbox 容器。如果安全要求禁止任何回传宿主机 mount namespace，则 sidecar 方案不可用，需要重新选择 CSI 或单容器模型。

### 5.3 预热与 Acquire 顺序

预热阶段：

1. Pool 按固定配置指纹创建 system egress policy，再创建 FUSE Pod；此时 spec 不含 workspace prefix 或 lease generation。
2. Kubelet 启动 `workspace-mounter`。Sidecar 从 mounter-only 环境读取固定、非敏感的 versioned bootstrap JSON，从 Downward API 单独读取 Pod UID；校验后把合并结果原子写入 `/run/s3fs/bootstrap.json`（mode `0600`）。bootstrap 明确区分只读 Secret 中的 `access_key_file`/`secret_key_file` 与私有 tmpfs 中的 `passwd_file=/run/s3fs/passwd-s3fs`；supervisor 校验单行非空 AK/SK、原子生成 mode `0600` 的 `AK:SK` 密码文件并清零临时 buffer，随后只进入 prepared/locked，不调用 s3fs。bootstrap 还包含 provider、bucket、endpoint、profile、CA/cache 路径、PoolKey 和超时，不包含 prefix、workspace identity 或 lease generation。
3. `startupProbe` 只检查 supervisor、`/dev/fuse`、cache/Secret 与底层 `/workspace` mode，prepared 后返回成功；Kubelet 随即启动 sandbox 主容器。
4. mounter `readinessProbe` 因尚未挂载而保持失败，Pod 保持 NotReady。Pool 通过独立 `PreparedSandbox` probe 确认 supervisor locked、sandbox 主进程存活、无 FUSE mount、无 mount generation 且 Exec/file gate 关闭，之后才把空壳加入 available 队列。

Acquire 阶段：

1. manager 从匹配配置指纹的 Pool 原子保留一个空壳；Pool miss 时同步 prepare 新空壳。
2. 使用请求中的 `workspace_path` 构造 prefix 并获取独占租约，再创建/验证空 prefix 根目录标记。租约冲突或 prefix 准备失败发生在挂载授权前，空壳复检仍为 pristine 时可以归还原 Pool。
3. manager 把 sandbox identity 绑定到 runtime UID，确认 sidecar `restartCount=0`、持久 owner 与 lease 后，以 CAS 把 owner 的 `mount_attempt` 从 0 改为 1，再通过固定控制命令给 sidecar 一次性授权。
4. Sidecar 校验 runtime UID、Pool 配置指纹、workspace identity、prefix 和 lease generation，写入 mount generation 后启动唯一 s3fs。
5. runtime 应用并确认用户请求对应的网络策略；随后 mounter readiness 确认 FUSE 类型与有界远端只读探测，再在已启动的 sandbox 容器内以 UID/GID 1000 执行固定、不可由用户传参的创建/读取/删除探测，以验证 mount propagation 和实际写权限。
6. 用户网络策略、Pod Ready、mounter generation 和 sandbox 内读写探测全部通过后，manager 才打开 Exec/file gate 并返回创建成功。

Sidecar 不配置 `livenessProbe`。startup probe 只表示“空壳可绑定”，readiness probe 才表示“workspace 已挂载且当前可访问”。不能再使用阻塞主容器启动的 `workspace-ready` init container；Acquire 后的可信 sandbox 内探测取代它。无论授权后挂载成功或失败，该实例都进入 single-use terminal lifecycle，不能退回 Pool。

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
- 必须配置 CPU、内存和 `ephemeral-storage` request/limit；limit 必须覆盖 `cache_size`、容器日志和少量安全余量。
- 配置固定 argv 的 `preStop`：prepared 空壳没有 mount 时立即成功；已授权实例在 termination grace period 内完成尽力 flush 和 unmount。主容器不设置长时间 preStop。
- 一期只读取 provider 级静态长期 AK/SK，并生成 s3fs `passwd_file`；Secret 中出现 session token 时配置校验必须失败。静态凭证轮换不做热加载，按 provider 排空并重建 FUSE sandbox。

`sandbox`：

- `runAsUser: 1000`、`runAsGroup: 1000`。
- `allowPrivilegeEscalation: false`。
- 不挂载 `/dev/fuse`、Secret 或 sidecar 私有目录。
- 不增加 `SYS_ADMIN`。
- Pod 的 `shareProcessNamespace` 保持 `false`。
- 继续禁用 ServiceAccount token 和 service links。
- Pod 使用 RuntimeDefault seccomp；sandbox 显式 drop ALL capabilities，并设置 `allowPrivilegeEscalation=false`。Acquire 后的固定读写探测通过可信 runtime exec 以 UID/GID 1000 在 sandbox 容器内执行，不新增 privileged init container。

### 5.5 Sidecar 健康、故障与销毁

- prepared 空壳没有 workspace 租约、owner、prefix、mount generation 或 s3fs 进程；Pool health 发现任一残留时必须销毁，不能作为可用空壳。
- `readinessProbe` 检测 mount 存在和有界超时的远端只读探测；失败时 Pod NotReady，workspace 状态为 `recovering`，新的 Exec 返回 503。
- mounter 镜像由可信 supervisor 作为 PID 1，启动并监控唯一的 s3fs 子进程。s3fs 退出或 FUSE mount 消失后，supervisor 必须保持运行、写入 unhealthy 状态且禁止自行 remount；普通对象存储网络中断同样不能触发自动重启或 lazy unmount。
- `/run/s3fs` 使用随 Pod 存活的独立 `emptyDir`。supervisor 获得一次性授权后以原子 create-if-absent 写入包含 Pod UID、workspace identity 和 lease generation 的 `mount-generation`；若容器重启且 marker 仍在，则写入 `restart-detected`、不得再次调用 s3fs，并保持存活但 readiness 失败。节点重启可能丢失 memory-backed `emptyDir`，因此 marker 只是本地第二道防线，不能授予挂载权；owner 中持久的 `mount_attempt=1` 禁止 runtime 对同一 Pod UID/lease generation 再次授权。marker 丢失时 supervisor 仍保持 locked，runtime 必须按第 8.3 节确认旧 Pod API 对象消失且进程已退出或节点已 fencing，再以新 Pod UID 和新 lease generation 重建。startup probe 只确认“授权后的初始化判定已完成”，runtime 观察到 `restartCount`、`restart-detected`、授权超时或 generation 异常后停止整个 Pod。
- s3fs 进程死亡会使已有 fd、cwd 和 mmap 指向失效的 FUSE 实例。仅取消 Kubernetes exec/Docker attach 流不保证远端进程及其脱离的子进程已经退出，因此安全恢复路径必须把 sandbox 置为 unavailable 并停止整个 Pod/容器。一期不做原地 remount；只有 runtime 确认旧实例完全退出并获取新的 lease generation 后，才能以新 Pod/容器重新挂载。
- mount state 以 runtime 的实时 probe/status 为准，Redis session 中的状态只用于恢复提示，不能单独作为 Exec 放行依据。
- 对外发布后，sandbox-api 为每个 FUSE sandbox 运行 lifecycle watcher，持续核对 lease、exact runtime UID、generation、restartCount/restart marker 和 FUSE health；任一异常只执行一次 gate close 与 single-use teardown。初始 `WaitSandboxReady` 不能替代该持续监督。
- 正常销毁由 sandbox-api 先拒绝新 Exec 并调用 `QuiesceWorkspace`。quiesce 成功时再通过可信 runtime 控制路径执行经过 profile 验证的 flush，然后删除 Pod；quiesce 失败时直接进入整个 Pod 停止流程，由主容器退出后的 sidecar 做有界、尽力 flush，不能返回强持久化承诺。主容器应快速退出；sidecar 在主容器退出后完成 unmount。
- sidecar 的 SIGTERM/preStop 是 unmount 的正常收尾，也是 API 异常时的 flush 兜底，但不能作为唯一 flush 机制，因为主容器可能消耗大部分 Pod termination grace period。Pod 必须配置足够的 termination grace period，主容器不得设置长时间 preStop。
- 尚未消费挂载授权的 prepared 空壳在缩容、过期或配置轮换时直接删除；一旦 `mount_attempt=1`，无论 workspace 是否进入 ready，销毁都必须走 single-use 清理并确认 runtime 退出后再释放租约。

## 6. Docker 架构

### 6.1 特殊容器模型

Docker 不使用两个独立容器模拟 Kubernetes sidecar。若不经过宿主机 bind mount、volume plugin 或 mount namespace/nsenter，独立 mounter 容器创建的 FUSE mount 无法透明出现在现有 sandbox 容器中。

因此每个 Docker sandbox 使用一个特殊镜像和单个容器：可信 root supervisor、FUSE 进程与 UID 1000 用户环境位于同一 mount namespace。

```text
┌────────────── Docker FUSE Sandbox ──────────────┐
│ PID 1: trusted root supervisor                  │
│   ├─ 读取 root-only Secret                      │
│   ├─ Pool 中 prepared/locked，Acquire 后启动 s3fs│
│   ├─ 管理 /workspace mount                      │
│   └─ SIGTERM 时 flush + unmount                 │
│                                                 │
│ 用户命令、文件命令：Docker Exec User=1000:1000 │
└─────────────────────────────────────────────────┘
```

### 6.2 预热与 Acquire

1. Pool 使用固定 provider 配置指纹创建特殊容器，以 root supervisor 作为 PID 1 启动；Swarm 使用 Docker Secret，普通 Docker Engine 使用 sandbox-api 创建的 root-only 临时凭证目录。该 bind mount 只承载 provider 级凭证/CA，不承载 `/workspace`。
2. supervisor 先保持 locked；sandbox-api 从 `ContainerCreate` 返回值取得不可变 container ID，通过 root-only 控制通道提交一次相同 schema 的 versioned bootstrap JSON。supervisor 校验 RuntimeUID 与固定配置、拒绝重放后，从只读 Secret 的 AK/SK 源生成私有 `/run/s3fs/passwd-s3fs`（mode `0600`）并进入 prepared/locked；用户环境基础进程已经启动，但 API 不允许任何用户 Exec/file 操作，且此时不存在 s3fs 进程或 workspace mount。
3. Pool 用私有 `execControl` 检查容器进程、底层 `/workspace` mode、Secret/cache 和“无 mount、无 generation”状态，将合格空壳加入对应配置指纹队列。Docker engine health 只能表示 supervisor 存活，不能代表 workspace ready。
4. Acquire 时 manager 原子保留空壳、获取 prefix 租约并准备目录标记，再绑定 sandbox identity并 CAS 消费一次 `mount_attempt`。
5. runtime 通过固定 `execControl` 传递 prefix、runtime ID 和 lease generation；supervisor 校验后启动唯一前台 s3fs，runtime 同时在 gateway 中应用用户网络策略。
6. FUSE health 和用户网络策略确认后，runtime 再以 `execUser` 的 UID/GID 1000 执行固定读写探测。所有条件成功才打开 Exec/file gate。
7. 使用完成或授权后任一步失败都删除整个容器和临时 Secret；该容器不能卸载后回到 Pool。Pool 异步补充新的 locked 空壳。

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
2. supervisor 终止并回收全部用户进程及其脱离的后代，验证不存在指向 `/workspace` 的打开写句柄；不能把 Docker attach 关闭当作进程退出。
3. quiesce 成功时执行 provider profile 验证过的 flush，再执行 `fusermount3 -u`；quiesce 失败时只记录有界、尽力 flush 结果，不宣称强持久化。
4. 优雅卸载失败时执行 lazy unmount，并记录错误指标。
5. 删除容器和临时 Secret，并确认 runtime 不再存在。
6. 最后释放 Redis workspace 租约。

## 7. 网络模型

### 7.1 Kubernetes 约束

同一 Pod 内 sidecar 和 sandbox 共享网络 namespace。NetworkPolicy 是 Pod 级的，无法只允许 sidecar 访问对象存储。

因此 prepared 空壳在 Pool WarmUp 阶段就需要为整个 Pod 放行不可被用户配置删除的固定 system egress；它属于 PoolKey，不依赖动态 workspace prefix 或用户网络配置：

- 目标 MinIO 或 OBS endpoint 的受控网络路径；
- 精确端口，默认 HTTPS 443 或 MinIO 配置端口；
- 解析该 endpoint 所需的 DNS 服务。

system egress 的实现按集群能力固定：

- provider endpoint 必须先进入平台维护的精确白名单；仅在配置文件中填写 endpoint 不产生放行规则。内网 endpoint 还必须经过显式安全审批，拒绝用户请求动态增加或覆盖。
- Pod 保持 `DNSPolicy=None` 和当前公共 nameserver，不接入 CoreDNS，也不配置集群 search domain。
- MinIO/OBS 必须使用公共 nameserver 可解析的稳定专用 endpoint；私有云 FQDN 无法公共解析时，可由运维使用 Pod `hostAliases`/等价 CNI 机制把该 FQDN 静态映射到平台批准的稳定 IP，且证书必须匹配原 FQDN。只有证书包含 IP SAN 时才允许直接配置 IP endpoint；`cluster.local` Service 不属于一期支持形式。
- DNS egress 只允许配置的公共 nameserver。Cilium 可按 endpoint FQDN 放行；标准 NetworkPolicy 不支持稳定 FQDN 策略，必须使用运维配置的稳定 CIDR 或显式 endpoint IP，不能把一次 DNS 解析结果当作长期规则。一期 `ProxyURL` 必须为空，egress proxy 仅作为后续安全增强方向。
- endpoint 解析和连通性必须在挂载前检查。仅加入网络白名单不能让公共 nameserver 解析 `cluster.local`。
- runtime 为每个空壳生成不可变的 instance selector，先创建对应 system egress policy，再创建 Pod；Acquire 时保留这条 policy，并在开放 Exec 前另外应用用户请求对应的网络策略。多个 NetworkPolicy 的 allow 语义是并集，用户策略不能删除 system egress，也不能额外获得未批准的内网目的地。

Sandbox 主容器也能连接这些地址，但没有存储凭证。必须使用最小网络范围并防止凭证泄露：

- 禁止放开整个 VPC、集群或对象存储网段。
- MinIO 优先使用独立 endpoint 或专用负载均衡地址，避免向 Pod 开放整个集群服务网段。
- 禁止匿名 bucket 访问。
- provider 级静态凭证只授予所需 bucket/sub path 权限，不授予管理权限；一期明确不提供单 workspace prefix 的凭证级隔离。
- 凭证不得出现在 Pod 共享环境变量、主容器文件系统或 API 响应中。

这是同 Pod 共享 network namespace 的明确边界：平台白名单一旦允许对象存储 endpoint，sandbox 用户进程也具备到该地址的 TCP 可达性。当前规则接受“只允许访问平台审批白名单地址”，不承诺“只有 sidecar 能访问”。如果后续要求即使已白名单也禁止用户进程直连，必须引入认证 egress proxy、CNI 容器级网络身份或 CSI/独立挂载模型，不能依赖标准 NetworkPolicy。

### 7.2 Docker 约束

Docker 特殊容器同样需要访问对象存储 endpoint。FUSE Pool 在 WarmUp 时创建 gateway pair，并只把已经过平台审批的对象存储 endpoint 精确白名单作为不可被用户配置删除的 system egress；Acquire 后、开放 Exec 前再追加用户网络规则。用户进程可连接该已批准 endpoint，但不能获得凭证。若要求按进程隔离 endpoint，单容器模型同样需要认证 egress proxy，不能仅靠 Docker network。

## 8. Workspace 隔离与租约

### 8.1 路径规范化

`workspace_path` 在用于对象前缀、日志、Redis key 或 mount 参数前必须统一规范化：

- 必须为非空相对路径。
- 拒绝 `.`、`..`、绝对路径、NUL 和路径逃逸。
- 清理重复 `/`，规范化后必须仍位于存储 driver 根目录下。
- 拒绝系统保留前缀，例如 `.sandbox-system`。
- mount 命令通过参数数组执行，不拼接 shell 字符串。

配置加载时必须计算 `storage.filesystem.sub_path` 的 canonical candidate：允许为空；非空时必须是 UTF-8 相对对象 key 前缀，拒绝前导 `/`、`.`/`..` segment、NUL 与控制字符，candidate 会折叠重复 `/` 并移除尾部 `/`。比较和构造只处理 ASCII `/` 分隔符，不做 URL decode 或 Unicode 兼容等价转换，原始 Unicode UTF-8 字节必须原样保留。`mode=sync` 继续按现有 driver 语义使用原值；`mode=fuse` 要求原值与 candidate 按字节完全相等，否则启动校验失败，绝不能静默改写并指向另一组对象。

最终对象前缀必须规范化为带且仅带一个尾部 `/` 的目录前缀：

```text
<storage.filesystem.sub_path>/<workspace_path>/
```

实现必须提供唯一共享的 `BuildWorkspacePrefix(canonicalSubPath, canonicalWorkspacePath)`；该函数只接受已验证输入，按字节拼接非空 segment、只插入一个 `/`，并输出带且仅带一个尾部 `/` 的 prefix，不在内部再次清理。FUSE mount source、目录标记、FUSE 模式下的显式 file API 以及 Redis lease/owner key 全部调用这一实现，禁止各自 `Trim`、`Clean` 或重复追加 `sub_path`。现有 MinIO/OBS driver 的 FUSE 路径入口必须扩展为接收该结果；legacy sync 模式保留原 driver 行为。`WorkspaceInfo.RootPath` 继续保存规范化后的 `workspace_path`，不新增 `workspace.object_prefix`。

已有部署若使用 `workspaces/`、`workspaces//team` 或含 dot segment 的旧 `sub_path`，只能继续使用 sync，或先按对象存储迁移方案复制/校验全部 key、排空 sandbox、切换为 canonical 配置后再启用 FUSE。迁移必须有清单、容量/数量校验和回滚点，不属于运行时自动修复。

每个 FUSE 进程直接挂载该前缀，sandbox 看不到同 bucket 的父目录或兄弟 workspace。

### 8.2 空前缀准备

当前 `goairix/fs` v0.3.11 的 MinIO 与华为 OBS `MakeDir` 都是 no-op，不能作为 s3fs 子目录挂载的前置条件。获得 workspace 独占租约后、启动 mounter 前，控制面必须执行新增的 `PrepareWorkspacePrefix`：使用 provider SDK 对 `BuildWorkspacePrefix` 返回的精确 key 写入零字节目录标记对象。标记的 metadata/content-type 由固定 provider profile 定义，默认候选为 `application/x-directory`，只有通过对应 s3fs `compat_dir` 行为测试后才能启用；写入后还必须以直接 API head/list 验证 exact key，而不是把 `MakeDir` 返回 nil 当成成功。

目录标记就是 workspace 根 prefix 本身，不创建用户可见的额外子文件。它在 sandbox 销毁时保留，以支持持久 workspace 后续重挂载；只有显式删除 workspace 且确认 prefix 下所有业务对象已按既有语义清理后才删除。file API 的 list/stat 必须忽略“exact root marker”这一实现对象，但不能忽略 prefix 下的普通用户对象。若 profile 无法证明该标记能挂载全新空 prefix，则该 provider/profile 不允许启用 FUSE。

### 8.3 Redis 独占租约

s3fs 不协调多个客户端对同一对象的并发修改，因此创建 FUSE workspace 前必须获取独占租约：

```text
sandbox:workspace:lease:<provider>:<storage-identity-hash>:<bucket>:<normalized-prefix>
```

租约 value 包含 sandbox ID、runtime、runtime ID、创建时间和 fencing generation。除 TTL lease 外，另保存不自动过期的 workspace owner 记录，并把 workspace hash 与 generation 写入 Pod/container label，供 Redis 状态异常时从 runtime 反查。规则：

- 同一 workspace 同时只允许一个 `rw` sandbox。
- sandbox-api 在 Acquire 获得 lease 后立即启动续租，不等 marker、mount 或对外发布；以不大于 TTL 三分之一的间隔续租。
- 续租失败立即关闭引用计数 operation gate，把 workspace 置为 unavailable 并拒绝新 Exec；lifecycle watcher 随即停止整个 sandbox，确认 runtime 已退出后才允许释放租约或接管。停止前的 flush 只能尽力执行，不能延迟 fail closed。挂载事务尚未发布时，续租失败取消同一 Acquire context 并进入 single-use cleanup。
- 正常销毁时 compare-and-delete 释放，不能删除其他 generation 的租约。
- TTL lease 消失不能自动删除 owner 记录。
- generation 由独立、永不随 owner 删除的 `sandbox:workspace:generation:` Redis 计数器原子递增；接管前必须同时检查 owner 记录、持久 session，以及带相同 workspace label 的 Pod/container；只有确认旧 runtime 已停止或删除，才允许清理旧 owner并分配新 generation。
- Kubernetes 的“旧 runtime 已删除”必须同时满足旧 Pod UID 对应的 API 对象已为 NotFound，以及 kubelet/CRI 已确认进程退出或节点已经完成基础设施级 fencing；节点 NotReady、容器暂时 stopped、空的 mount probe 或仅 force-delete Pod API 对象都不够。Docker 必须确认旧 container ID 已不存在且 daemon/宿主机可确认进程退出，宿主机失联时同样需要 fencing。无法确认时 workspace 保持 blocked，不得接管。新实例总是获得新 lease generation，owner 的 `mount_attempt` 从 0 开始且只允许 CAS 成功一次。
- API 多副本共享同一 Redis 状态，不能只使用进程内锁。

`endpoint-hash` 不能直接使用 endpoint 字符串。相同物理存储可能同时存在公网、私网或新旧域名，滚动变更 endpoint 时也可能有旧 sandbox 存活；若按 URL 生成不同租约 key，会允许同一对象前缀被重复写挂载。配置必须提供平台维护、对同一物理对象命名空间保持稳定的 `storage_identity`，租约 key 使用其哈希；endpoint 轮换不得改变该 identity，变更 identity 前必须排空相关 sandbox。

普通对象存储无法强制 fencing token，因此 generation 只用于检测和审计，不是强一致分布式锁。实现不能依赖“TTL 到期即安全”；旧 sandbox 是否仍在运行始终是接管前置条件。如果 runtime 状态无法确认，workspace 保持 blocked 并要求人工处理。

## 9. 配置设计

建议扩展 `WorkspaceConfig`，并将 provider 差异收敛到经过验证的 profile：

```go
type WorkspaceFUSEProviderConfig struct {
    Driver               string   `mapstructure:"driver"`
    Profile              string   `mapstructure:"profile"`
    StorageIdentity      string   `mapstructure:"storage_identity"`
    MounterImage         string   `mapstructure:"mounter_image"`
    DockerImage          string   `mapstructure:"docker_image"`
    CASecretKey          string   `mapstructure:"ca_secret_key"`
    CredentialGeneration string   `mapstructure:"credential_generation"`
    LSMProfile           string   `mapstructure:"lsm_profile"`
    EndpointHostIPs      []string `mapstructure:"endpoint_host_ips"`
    SystemEgressMode     string   `mapstructure:"system_egress_mode"`
    DNSCIDRs             []string `mapstructure:"dns_cidrs"`
    SystemEgressFQDNs    []string `mapstructure:"system_egress_fqdns"`
    SystemEgressCIDRs    []string `mapstructure:"system_egress_cidrs"`
    EndpointPorts        []int32  `mapstructure:"endpoint_ports"`
    ProxyURL             string   `mapstructure:"proxy_url"`
}

type FileSystemCredentialFileConfig struct {
    AccessKeyFile string `mapstructure:"access_key_file"`
    SecretKeyFile string `mapstructure:"secret_key_file"`
}

type FileSystemConfig struct {
    // 省略现有 provider、bucket、endpoint 等字段。
    CredentialFiles FileSystemCredentialFileConfig `mapstructure:"credential_files"`
    CAFile          string                           `mapstructure:"ca_file"`
}

type WorkspaceFUSEResourceConfig struct {
    CPURequest              string `mapstructure:"cpu_request"`
    CPULimit                string `mapstructure:"cpu_limit"`
    MemoryRequest           string `mapstructure:"memory_request"`
    MemoryLimit             string `mapstructure:"memory_limit"`
    EphemeralStorageRequest string `mapstructure:"ephemeral_storage_request"`
    EphemeralStorageLimit   string `mapstructure:"ephemeral_storage_limit"`
}

type WorkspaceFUSEPoolConfig struct {
    MinSize               int `mapstructure:"min_size"`
    MaxSize               int `mapstructure:"max_size"`
    RefillIntervalSeconds int `mapstructure:"refill_interval_seconds"`
    PrepareTimeoutSeconds int `mapstructure:"prepare_timeout_seconds"`
}

type WorkspaceConfig struct {
    AutoSyncIntervalSeconds   int                                    `mapstructure:"auto_sync_interval_seconds"`
    Mode                      string                                 `mapstructure:"mode"` // "sync" | "fuse"
    SecretName                string                                 `mapstructure:"secret_name"`
    CacheSize                 string                                 `mapstructure:"cache_size"`
    CacheMedium               string                                 `mapstructure:"cache_medium"` // 一期固定 "disk"
    MountTimeoutSeconds       int                                    `mapstructure:"mount_timeout_seconds"`
    FlushTimeoutSeconds       int                                    `mapstructure:"flush_timeout_seconds"`
    UnmountTimeoutSeconds     int                                    `mapstructure:"unmount_timeout_seconds"`
    RecreateMaxAttempts       int                                    `mapstructure:"recreate_max_attempts"`
    LeaseTTLSeconds           int                                    `mapstructure:"lease_ttl_seconds"`
    LeaseRenewIntervalSeconds int                                    `mapstructure:"lease_renew_interval_seconds"`
    QuotaMode                 string                                 `mapstructure:"quota_mode"` // FUSE 一期固定 "soft"
    MounterResources          WorkspaceFUSEResourceConfig            `mapstructure:"mounter_resources"`
    FUSEPool                  WorkspaceFUSEPoolConfig                `mapstructure:"fuse_pool"`
    Providers                 map[string]WorkspaceFUSEProviderConfig `mapstructure:"providers"`
}
```

`security.max_upload_bytes` 默认 2 GiB 且必须为正数；它只放宽精确的 direct file-upload 路由，其他 request body 继续限制为 64 MiB。生产 FUSE provider 的 `lsm_profile` 必填并拒绝 `unconfined`/`label=disable`。

配置约束：

- `mode=fuse` 时 runtime 必须为 Kubernetes 或 Linux Docker Engine。
- `provider=minio|obs` 时一期只允许验证通过的 s3fs profile；profile 没有对应测试证据时启动失败。
- 每个启用的 FUSE provider 必须配置不可由请求覆盖的 `storage_identity`；同一物理对象命名空间的不同 endpoint 必须使用相同 identity。
- `credential_generation` 是不含敏感信息的运维版本号，Secret/CA 内容每次轮换都必须递增；它参与 PoolKey，版本变化时只排空旧 key 的 prepared 空壳，已绑定实例按凭证轮换流程排空。
- `storage.filesystem.endpoint` 保持现有 driver 的 provider-native 格式：MinIO 使用 `host[:port]` 并由 `use_ssl` 决定协议，OBS 使用华为 SDK 要求的完整 endpoint；runtime 按 provider 派生 s3fs 的完整 `url`，不能把同一字符串未经转换传给两类客户端。
- 可选 `endpoint_host_ips` 只能由运维配置为字面量 IP，并且必须落入已批准的 system egress CIDR；runtime 用它为 endpoint FQDN 生成 Pod `hostAliases`/Docker `extra_hosts`，不得改变 TLS 使用的主机名。
- Kubernetes 必须为当前 provider 配置固定 digest 的 mounter image 和 Secret。
- Docker 必须为当前 provider 配置固定 digest 的特殊 sandbox image 和 Secret 来源。
- `lease_renew_interval_seconds` 必须不大于 `lease_ttl_seconds / 3`。
- `quota_mode=soft` 必须显式配置，避免把现有 `max_disk` 误认为 FUSE workspace 硬限制。
- 一期 `cache_medium` 固定为 `disk`。Kubernetes 同时配置 `emptyDir.sizeLimit` 和 Pod/container `ephemeral-storage` request/limit；Docker 使用独立 cache volume/目录、软阈值监控和销毁清理。`cache_size` 是软阈值，不承诺硬 quota 或 ENOSPC。
- Secret、AK、SK 不得通过日志输出。
- 生产凭证必须通过 `FileSystemCredentialFileConfig` 指向只读 Secret 文件；文件凭证与现有明文 `access_key`/`secret_key` 互斥。私有 CA 通过 `ca_file` 注入控制面存储客户端，并通过 provider Secret 的 `ca_secret_key` 注入 mounter；两者由同一外部 Secret 源同步，不能由 API 读取 runtime Secret 内容后再转发。FUSE 模式的 prefix marker、head/list 探测使用本仓库中基于 MinIO/华为 OBS 原生 SDK 的独立 `WorkspaceObjectClient`，由它显式注入自定义 CA；当前不支持自定义 transport 的 `goairix/fs` v0.3.11 只保留给 legacy sync 模式，不能在 FUSE 启动路径中悄悄回退使用，更不能关闭 TLS 校验绕过。FUSE 模式的公共文件 API 一律通过已挂载的 runtime `/workspace` 操作，不通过该对象客户端旁路写业务文件。
- 一期只接受长期 AK/SK；任何 session token 或凭证过期字段都必须拒绝，不能隐式退回环境变量认证。
- `mount_timeout_seconds`、`flush_timeout_seconds`、`lease_ttl_seconds` 和 cache size 必须为正值并设置安全默认值。
- `fuse_pool.min_size/max_size` 独立于现有通用 `pool`；必须满足 `0 <= min_size <= max_size`，`prepare_timeout_seconds` 覆盖 Pod/容器基础启动但不包含 Acquire 后的 mount timeout。
- `min_size=0` 只保留 cold prepare 能力，不满足“容器先启动、请求时只绑定 prefix”的低延迟目标；生产启用预热时必须配置 `min_size >= 1`，并按突发并发量压测定容。

示例：

```yaml
storage:
  filesystem:
    credential_files:
      access_key_file: "/run/secrets/storage_access_key"
      secret_key_file: "/run/secrets/storage_secret_key"
    ca_file: "/run/secrets/storage_ca"

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
  fuse_pool:
    min_size: 3
    max_size: 20
    refill_interval_seconds: 10
    prepare_timeout_seconds: 120
  mounter_resources:
    cpu_request: "50m"
    cpu_limit: "1"
    memory_request: "64Mi"
    memory_limit: "512Mi"
    ephemeral_storage_request: "512Mi"
    ephemeral_storage_limit: "3Gi"
  providers:
    minio:
      driver: "s3fs"
      profile: "minio-sigv4-path-style-v1"
      storage_identity: "minio-prod-primary"
      ca_secret_key: "ca.crt"
      credential_generation: "2026-09-03-01"
      mounter_image: "${SANDBOX_MINIO_MOUNTER_IMAGE}"
      docker_image: "${SANDBOX_MINIO_FUSE_RUNTIME_IMAGE}"
      lsm_profile: "sandbox-fuse"
      system_egress_mode: "cidr"
      dns_cidrs: ["8.8.8.8/32", "1.1.1.1/32"]
      endpoint_ports: [9000]
      proxy_url: ""
      system_egress_fqdns: ["${SANDBOX_MINIO_ENDPOINT_HOST}"]
      system_egress_cidrs: ["${SANDBOX_MINIO_ENDPOINT_CIDR}"]
    obs:
      driver: "s3fs"
      profile: "huawei-obs-private-2023-verified-v1"
      storage_identity: "huawei-obs-private-primary"
      ca_secret_key: "ca.crt"
      credential_generation: "2026-09-03-01"
      mounter_image: "${SANDBOX_OBS_MOUNTER_IMAGE}"
      docker_image: "${SANDBOX_OBS_FUSE_RUNTIME_IMAGE}"
      lsm_profile: "sandbox-fuse"
      system_egress_mode: "cidr"
      dns_cidrs: ["8.8.8.8/32", "1.1.1.1/32"]
      endpoint_ports: [443]
      proxy_url: ""
      system_egress_fqdns: ["${SANDBOX_OBS_ENDPOINT_HOST}"]
      system_egress_cidrs: ["${SANDBOX_OBS_ENDPOINT_CIDR}"]
```

## 10. Runtime 与状态模型

### 10.1 Runtime spec

`runtime.SandboxSpec` 增加可选 FUSE 描述：

```go
type WorkspaceFUSESpec struct {
    Provider        string
    Driver          string
    Profile         string
    StorageIdentity string
    MounterImage    string
    SecretName      string
    CASecretKey     string
    EndpointHostIPs []string
    Bucket          string
    Endpoint        string
    Region          string
    UseSSL          bool
    CacheSize       string
    MountTimeout    time.Duration
    FlushTimeout    time.Duration
    LSMProfile      string
    SystemEgress    SystemEgressSpec
    PoolKey         string
}

type SystemEgressMode string
const (
    SystemEgressCIDR       SystemEgressMode = "cidr"
    SystemEgressCiliumFQDN SystemEgressMode = "cilium-fqdn"
)
type SystemEgressSpec struct {
    Mode          SystemEgressMode
    DNSCIDRs      []string
    DNSPorts      []int32
    EndpointCIDRs []string
    EndpointFQDNs []string
    EndpointPorts []int32
    ProxyURL      string // 一期必须为空，代理模式不在支持范围
}

type WorkspaceMountAuthorization struct {
    RuntimeUID      string
    PoolKey         string
    WorkspaceHash   string
    Prefix          string
    LeaseGeneration int64
    MountAttempt    uint8 // 一期固定为 1
}
```

`WorkspaceFUSESpec` 只包含预热时已经确定的固定配置，不包含 workspace prefix 或 lease generation。`PoolKey` 是以下字段规范化序列化后的哈希：runtime、sandbox 镜像/资源/安全配置、provider、`storage_identity`、bucket、endpoint、region、profile、mounter 镜像 digest、Secret 名与运维维护的 credential generation、CA、cache、LSM profile 和 system egress。它明确不包含 `workspace_path/prefix`、workspace identity、lease generation、reservation token 或请求级用户网络规则；这些字段只能在 Acquire 时绑定或追加。不同 PoolKey 的空壳绝不能混用；请求的固定字段不匹配现有 PoolKey 时只能按新 key cold prepare，不能借用其他 key 的空壳。Spec 只携带 Secret 引用，不携带明文 AK/SK；Docker 如无法通过名称引用 Secret，则使用 root-only secret path，由 runtime 管理生命周期，不能写入持久 session。标准 NetworkPolicy 只接受 CIDR mode；Cilium FQDN mode 只接受精确、无通配符的 endpoint FQDN。两种模式都固定 DNS TCP/UDP 53 与对象端口，并拒绝任何未审批私网目的地或非空 `ProxyURL`。

FUSE 创建必须拆为 prepare/authorize/ready 三阶段，不能沿用“创建后直接等 Ready”的单调用流程：

```go
PrepareSandbox(ctx context.Context, spec SandboxSpec) (*SandboxInfo, error)
AuthorizeWorkspaceMount(ctx context.Context, id string, auth WorkspaceMountAuthorization) error
WaitSandboxReady(ctx context.Context, id string) (*SandboxInfo, error)
```

FUSE runtime 还必须使用 `RuntimeFencer` 产生与 exact RuntimeUID 匹配的退出证据。Kubernetes 正常路径要求 supervisor 已确认 unmount，再对 exact Pod UID graceful delete 并观察到 NotFound；节点/控制通道异常时必须由基础设施 fencer 证明节点已隔离，否则保持 owner blocked。Docker 要求 daemon 可达且对 exact container ID 的 inspect 返回 NotFound。force-delete/remove 请求本身不是退出证据。runtime 构造函数不得提前清理资源；Manager 完成 session/owner/Pool 对账后，才把受保护 RuntimeUID 集合交给可选 `OrphanReconciler`。

`PrepareSandbox` 先创建固定 system egress policy，再创建 supervisor locked、无 prefix 的 Pod/容器，等待 sandbox 基础进程启动并返回不可复用的 runtime UID；它不得启动 s3fs 或开放用户 Exec。Pool 使用新增的 `PreparedSandboxHealth` 验证 supervisor locked、sandbox 进程存活、无 mount/generation 后才入队。

Acquire 时 manager 确认 PoolKey 相等、实例仍 pristine 且 Kubernetes sidecar `restartCount=0`，再以 runtime UID 和 lease generation 对持久 owner 执行 `mount_attempt: 0→1` CAS；只有 CAS 成功才调用 `AuthorizeWorkspaceMount`。授权命令固定且只能通过可信控制通道调用，失败、超时或进程在 CAS 前后重启时必须删除该实例并用新 generation 重建，不能回滚 `mount_attempt` 或重放授权。`WaitSandboxReady` 先等 FUSE health，再从 sandbox 容器内部以 UID/GID 1000 执行固定读写探测；全部通过后才返回可用。现有 `CreateSandbox` 继续服务 sync 模式；FUSE 创建由 Pool 和上述三阶段方法编排，禁止 runtime 自己绕过 owner CAS。

runtime 接口同时增加以下可信控制能力：

```go
WorkspaceHealth(ctx context.Context, id string) (*WorkspaceHealth, error)
PreparedSandboxHealth(ctx context.Context, id string, poolKey string) error
QuiesceWorkspace(ctx context.Context, id string) (WorkspaceQuiesceToken, error)
ResumeWorkspace(ctx context.Context, id string, token WorkspaceQuiesceToken) error
FlushWorkspace(ctx context.Context, id string) error
```

`QuiesceWorkspace` 在 Manager 已关闭 admission、等待 API 引用归零后验证没有脱离的用户进程或仍指向 `/workspace` 的打开写句柄；它不能把“客户端断开”当作进程退出。成功返回绑定 exact RuntimeUID/generation 的一次性 `WorkspaceQuiesceToken`；非销毁 flush 后必须用 `ResumeWorkspace` 消费同一 token 恢复进程，跨 runtime、跨 generation 或重放都失败。验证失败时调用方只能保持 unavailable 或停止整个 sandbox，不能继续 flush、重挂载或释放租约。Kubernetes 实现只能对固定名称的容器执行固定控制命令；Docker 实现只能走私有 `execControl`。挂载授权与这些能力都不能从公共 Exec 请求中选择容器、用户或 argv。

上传接口需要把调用方声明并由流式读取校验的文件大小传入 runtime，使 tar header 可以先写出并通过 pipe 直接流向 Docker/Kubernetes，而不是 `io.ReadAll`：

```go
UploadFile(ctx context.Context, id, destPath string, size int64, reader io.Reader) error
```

非上传请求继续使用 64 MiB body 上限，只有 exact direct-upload 路由使用 `security.max_upload_bytes`（默认 2 GiB）。大于 64 MiB 的上传要求 `X-Sandbox-File-Size`，handler 使用 `Request.MultipartReader` 直接读取 file part，禁止 `FormFile`/`ParseMultipartForm` 产生整文件临时缓存；实现必须校验实际读取字节数与声明大小一致，并拒绝负数、超限、短读和多余字节。小文件继续兼容现有 multipart 格式。

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

Pool 空壳不创建 `WorkspaceInfo`，使用 Redis 持久 FUSE pool record 保存 `preparing → prepared → reserved → binding → consumed` 以及终止态 `cleanup`。`PreparationID` 是在调用 runtime 前生成的不可复用 opaque UUID，同时作为 runtime `spec.ID`；Redis 只用其 SHA-256 digest 构造 key。`preparing` 先原子占用 `preparing + prepared < max_size` 的容量槽，此时 runtime ID/UID 允许为空；runtime 返回后再以 refill lock token 和 revision 原子绑定不可变且全局唯一的 runtime UID。record 还保存 PoolKey、创建它的 API instance ownership token、reservation/cleanup token、由 Redis `TIME` 计算的 `PrepareUntil`、`ReservedUntil`、`CleanupUntil` 和单调 revision。所有 transition/claim/delete 必须同时匹配预期 state、token 与 revision，过期 reconciler 不能凭旧快照先删 runtime。只有先原子 claim 为 `cleanup` 的副本才可执行物理删除；删除失败保留 tombstone 供相同 token 重试，其他 token 仅在 `CleanupUntil` 后接管。若 runtime identity 在 intent-only cleanup claim 之后才返回，相同 cleanup token 必须在同一事务中补全 record、record UID 映射与全局 RuntimeUID owner 索引；映射冲突不得覆盖。只有 health 验证完成后的 `prepared` 可被领取；Kubernetes 初始 label 必须为 `preparing`，通过校验后再以 resourceVersion 保护 patch 为 `prepared`。Docker label 创建后不可更新，只保存 PoolKey/instance identity，Redis record 是跨副本状态源。`reserved` 在授权前通过 pristine 复检后可退回 `prepared`，但该 transition 必须重新原子检查 `preparing + prepared < max_size`；容量已被 refill 占满时销毁旧 reservation。`binding` 表示 owner 的 `mount_attempt` 已消费，必须最终销毁。

### 10.3 FUSE Pool

现有 Pool 已采用 single-use Release（使用后删除容器），FUSE 扩展沿用它由 `sandbox-api` 负责 WarmUp、Acquire 后补池、异常移除补池和 Stop/Drain 的控制模式。FUSE 空壳的 prepared health、一次性挂载授权和恢复规则与普通空壳不同，因此使用独立 `FUSEPool` 队列，不能直接混入当前 `available` slice。多副本场景下由各 `sandbox-api` 副本运行相同控制循环，并按 PoolKey 使用 Redis inventory/互斥共同维护全局目标水位；Redis 是状态与协调介质，不是主动控制器：

- `Manager.Start(ctx) error` 在 persistent sandbox 恢复与 FUSE inventory 对账后调用 `FUSEPool.WarmUp`。FUSE 模式下 Redis、恢复、对账或 WarmUp 任一失败都必须返回错误并阻止 HTTP 服务监听。获得分布式 refill lock 的 `sandbox-api` 根据 Redis 记录计算缺口，创建不含 prefix 的 `WorkspaceFUSESpec` 空壳，直到 `prepared` 达到 `min_size`；不能调用等待 workspace Ready 的旧 `CreateSandbox` 路径。
- 健康验证通过后，以 `PreparationID` 定位并在同一 Lua 中校验 refill lock token，把已绑定 runtime UID 的 `preparing` record CAS 为 `prepared`；Pod label 只用于观测和 runtime 反查，不能代替 Redis 状态。
- prepared 空壳不创建用户 sandbox/session，不出现在公共 list/get 响应中，其 runtime ID/UID 也不能返回给调用方；所有公共 Exec/file API 必须先验证已提交的用户 session 和 gate，不能仅凭可猜测或泄露的 runtime ID 直达空壳。
- `AcquirePrepared(poolKey, reservationToken)` 通过 Lua/事务原子选择同 key 的一个 `prepared` record 并改为 `reserved`，避免多个 API 副本领取同一实例；随后执行 runtime pristine probe，并由 Manager guard 返回明确的 `Pristine / Protected / Abandoned` disposition：只有 `Pristine` 可交付，`Protected` 或检查失败必须 fail-closed 且保留 record，只有明确 `Abandoned` 才允许进入 cleanup。领取后当前 `sandbox-api` 立即异步调用 `refillIfNeeded`；没有可用实例时，请求先以 `PreparationID` 登记无 reservation 的 `preparing` 容量意图，再创建 runtime，健康通过后在受 refill lock fencing 的 Lua 中从 Redis 当前时间开始写 `ReservedUntil` 并直接 CAS 为 `reserved`，绝不能短暂发布为可被另一副本领取的 `prepared`。
- manager 先以 CAS 把 workspace owner 的 runtime UID/lease generation 和 `mount_attempt: 0→1` 绑定，再以相同 reservation token 把 pool record 从 `reserved` CAS 为 `binding`；两步都成功后才能向 supervisor 发授权。任一步失败或两步之间崩溃都按不确定实例销毁，reconciler 只要发现 reserved record 已被 owner 引用就禁止回池，因此不要求两个不同 key 具备跨槽事务。
- `ReturnPrepared` 仅允许持有相同 reservation token、尚未进入 binding、没有 mount generation 和 owner/session 引用的实例；返回前重新执行完整 pristine probe，再以 `preparing + prepared < max_size` 容量 admission 原子改回 `prepared`。若异步 refill 已占满容量则先 claim cleanup 并销毁该 reservation，绝不突破全局上限；Return 本身纳入 Stop 的在途操作计数，Stop 开始后不得晚发布 `prepared`。
- final publication 的 Redis 回复按不确定结果处理：`reserved → prepared`、warm `preparing → prepared` 和 cold `preparing → reserved` 返回错误，或成功回复后发现 Stop/cancellation 时，先按本次唯一可能提交的 after state/token/revision 尝试 cleanup claim，再按 exact before 版本尝试；只有成功 claimant 可删除 runtime。若另一个 Acquire 已推进 revision/token，两次 claim 都失败且不得删除新 owner 的 runtime。cold Acquire 在该 post-check 完成前不得向调用方返回 reservation。
- `ReleaseConsumed` 先以完整 record 原子 claim `cleanup`，再通过 FUSE 专用 exact remove 契约同时携带 runtime ID 和不可变 runtime UID 删除 Pod/容器、system egress policy、cache 和临时 Secret；未绑定意图只允许用不可复用的 `PreparationID/spec.ID` 清理。确认 runtime 退出后才删除 cleanup tombstone、释放 owner/lease并调用 `NotifyRemoved/refillIfNeeded`；删除失败保留 record 重试，绝不把实例放回 available。
- 每个 `sandbox-api` 还按 `refill_interval_seconds` 运行有抖动的 reconciliation；只有取得对应 PoolKey refill lock 的副本执行本轮增删。lock 必须按 token 续租，失锁立即取消本轮；容量 admission、runtime UID bind 和 publish 的 Lua 仍再次校验 lock token，形成最终 fencing。`min_size` 是期望的 `prepared` 可用数，`max_size` 限制 `preparing + prepared`，`reserved/binding/consumed/cleanup` 已离开可用 Pool、不计入容量。补池创建先登记 `preparing`，避免并发副本超配。
- `Drain(poolKey)` 用于 profile、镜像、credential generation、CA、endpoint、bucket、网络或安全配置变化，只删除该 key 的 prepared 空壳；已绑定实例按正常排空策略结束。
- `Manager.Stop` 只考虑本副本持有 ownership token 的 `preparing/prepared` 空壳，并仍须经过 guard；仅 `Pristine`/`Abandoned` 可 cleanup，`Protected`/`Unknown`/检查错误保留并报告未完全 drain。其他 maintainer 的记录和所有 reserved/binding/consumed 记录不由该副本 Stop 删除；滚动发布时其他副本继续维护全局水位。最后一个副本退出或明确禁用 FUSE 时，运维 drain 流程负责删除剩余未绑定空壳。
- Pool 大小指标增加 `runtime/provider/pool_key/state` 标签，但日志和指标只记录 PoolKey/ workspace hash，不记录 prefix、AK/SK 或 Secret 内容。
- API 副本崩溃后，`prepared` record 由新副本先原子改为 inspection reservation，再调用 Manager guard 和 pristine health；只有明确 `Pristine` 才退回 `prepared`，`Protected`、`Unknown` 或 guard 错误保持不可领取、不得删除且初次启动 fail-closed，明确 `Abandoned` 才 cleanup。guard 已明确返回 `Pristine` 后，若 runtime health 明确失败，则该空壳已被证明不可安全复用，可以 cleanup。超时的 `preparing/reserved` 也必须先查询 guard；有效 owner/session/open gate 是 `Protected`，不确定状态禁止物理删除，只有 `Pristine` 或 `Abandoned` 才可 cleanup。`binding` 同样只在明确 `Abandoned` 时清理，`consumed` 由持久 sandbox session 接管。所有到期判断使用 Redis 服务端时间，reconciler 不能把其他副本仍持有的实例当孤儿删除。

FUSE Pool 复用现有 `Pool` 的 `WarmUp → Acquire → refill → single-use Release/Drain` 生命周期，但使用独立的 `workspace.fuse_pool` 配置与 Redis registry，不能占用或改变现有通用 `pool` 的 sync/无 workspace 空壳队列。独立配置是因为两类空壳的镜像、权限和 ready 判定不同，不表示由其他组件维护。当前部署只有一个活动 filesystem provider，因此一期只维护一个活动 FUSE PoolKey；未来同时服务多个 provider 时可自然增加多个 key，不需要把进程内队列作为跨副本协调源。

Redis key 必须分域：session 使用 `sandbox:session:v2:`，Pool、workspace lease、owner 与 generation 使用各自独立前缀。恢复只扫描 v2 session 前缀；旧 `sandbox:<id>` 只有在 exact key 读取且 JSON 验证为合法 Sandbox 后才能迁移，不能用 `sandbox:*` 把 Pool/lease/owner 当 session。Docker 容器会跨 sandbox-api 进程重启存活，runtime 构造时禁止清理 managed 资源；Manager 先恢复 session/owner/Pool、构造受保护 RuntimeUID 集合，再调用 runtime orphan reconciliation。

## 11. API 语义

### 11.1 创建

创建请求带 `workspace_path` 且 `workspace.mode=fuse` 时：

- 根据固定配置计算 PoolKey，从 FUSE Pool 保留一个 `prepared` 空壳；Pool miss 时按需执行 `PrepareSandbox`；
- 使用规范化 prefix 获取独占租约并执行 `PrepareWorkspacePrefix`；
- 在持久 owner 中绑定 runtime UID，CAS 消费一次挂载授权，再启动 s3fs；
- 等待 FUSE health 和 sandbox 容器内固定读写探测；
- 注册 workspace 状态，但不执行文件复制。

租约冲突、prefix 准备失败或请求取消若发生在 `mount_attempt` CAS 之前，只有通过 `PreparedSandboxHealth` 证明无 mount、无 generation、无 owner 引用且 gate 关闭后才能把空壳归还同一 PoolKey；无法证明时直接销毁。CAS 之后任何结果都必须销毁实例并异步补池。无 workspace 的 sandbox 和 legacy sync 模式继续使用现有通用 Pool，不从 privileged FUSE Pool 取空壳。

### 11.2 SyncWorkspace

FUSE workspace 不再复制整棵目录，但保留 `SyncWorkspace` 的持久化语义：

- 每个已发布 sandbox 使用可关闭、引用计数的 operation gate；所有 Exec/stream/file/workspace 操作从开始到流结束都持有引用。`from_container` 进入 `flushing` 状态并取得 exclusive token：先关闭新 admission，等待已有引用归零，再调用 runtime `QuiesceWorkspace` 和 `FlushWorkspace`；只有目标 s3fs 版本与 provider profile 的故障注入测试证明该 flush/close 路径已把全部已关闭文件上传到远端时，才返回 `files_synced=0`、`flushed=true` 和 `last_flushed_at`。
- Kubernetes 保持 `shareProcessNamespace=false`，因此不能让 sidecar 检查主容器 `/proc`。`QuiesceWorkspace` 必须在 sandbox 容器内运行固定的非特权 `workspace-probe`：停止同 UID 的其余进程、枚举 `/proc/*/fd` 并确认没有指向 `/workspace` 的可写 fd，返回可恢复 token。无法枚举/停止、发现后台/脱离进程或写句柄、或 generation 在 flush 后变化时，`SyncWorkspace` 返回 409/503 与 `flushed=false`，不能调用 mounter flush 或宣称持久化完成；健康复检成功后才恢复进程并重开 gate。
- flush 超时、quiesce 失败、s3fs 返回错误或 provider spike 无法证明持久化边界时，API 返回错误或明确的 `flushed=false`/unsupported，不能把健康检查、Linux `syncfs` 返回成功或 Exec 流结束单独当成数据已持久化。
- `to_container` 不执行复制并返回明确 no-op；其前提是持有租约期间禁止其他客户端或管理工具修改同一 prefix。s3fs 不提供可靠的跨客户端缓存失效，因此外部修改不属于一期支持语义。
- 响应包含 `mount_type=fuse`，区分“未复制但已 flush”和 legacy sync 的文件复制计数。

auto-sync 完全跳过 FUSE workspace。

### 11.3 动态绑定与 MountWorkspace/UnmountWorkspace

FUSE 的动态挂载只发生在 Create/Pool Acquire 事务内部：此时 Pod/容器虽然已经预热运行，但尚未注册为用户可用 sandbox，Exec/file gate 从未开放。它不是对已交付 sandbox 的通用热插拔能力。

- 带 `workspace_path` 的 FUSE Create 在 Acquire 阶段内部执行一次 bind；成功响应前必须完成租约、授权、挂载和 sandbox 内探测。
- 对已经返回给调用方的 FUSE sandbox 再调用公共 `MountWorkspace`，无论当前是否已有 workspace，都返回 HTTP 409。
- FUSE `UnmountWorkspace` 返回 HTTP 409，workspace 只随 sandbox 销毁卸载，实例不回 Pool。
- sync 模式继续支持现有动态 mount/unmount API。

不得静默切换到 legacy sync，也不得把使用过、授权失败或状态不明的容器重新标记为 prepared。

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
- 超过阈值后拒绝新的 Exec 和文件写 API，并停止整个 sandbox；不能假设取消 attach/exec 流会终止已有或脱离的进程。停止前只做有界、尽力 flush，确认 runtime 已退出后才进入重建或租约释放。
- 指标与告警必须在接近阈值时提前触发。
- 文档和 API 明确软配额存在采样延迟，不能作为强安全边界。

若业务要求强配额，应使用每租户/每 workspace 独立 bucket 配额，或改用提供元数据与 quota 的文件系统；不在本设计内模拟硬配额。`mode=fuse` 与 `quota_mode=soft` 必须同时显式启用，部署文档需要说明这是相对于现有 `max_disk` 的能力变化。

### 13.2 s3fs 临时空间

s3fs 写入可能使用本地临时文件：

- Kubernetes 使用独立、带 `sizeLimit` 的 `fuse-cache` emptyDir，并为 mounter 和 Pod 配置 `ephemeral-storage` request/limit；固定 profile 必须至少设置 `tmpdir=/var/cache/s3fs/tmp`，启用 `use_cache` 时还必须设置 `use_cache=/var/cache/s3fs/cache`，确保所有 s3fs 临时/缓存数据进入该卷。
- Docker 使用独立 cache volume/目录和软阈值监控，并采用相同的 `tmpdir`/`use_cache` 路由，禁止使用无界 container writable layer 或不受阈值约束的 `/tmp` tmpfs；宿主机不能提供真实 quota 时，达到阈值后终止并重建 sandbox。
- 一期 `cache_medium=disk` 使用节点临时盘，要求节点磁盘加密并在 Pod/容器销毁后清理。`emptyDir.sizeLimit` 和 Docker 软阈值都不是文件系统硬 quota，Kubernetes 可能以 eviction 而不是同步写入错误处理超限。
- 即使不启用 s3fs 持久 `use_cache`，s3fs 写入仍需要临时空间；“不使用宿主机 workspace”不等于“节点上绝不出现临时文件”。
- cache 达到软阈值或 Pod 收到 ephemeral-storage eviction 信号时，runtime 立即阻止新 Exec 并停止整个 sandbox，执行有界、尽力 flush 后重建；不得继续使用无界缓存，也不能把取消 Exec 流当作用户进程已经退出。
- cache 使用率、阈值触发、eviction、清理失败和实际出现的 ENOSPC 进入指标和告警。

### 13.3 B 类负载建议

- npm/pip/build cache 放到 `/tmp`，不放入 `/workspace`。
- 同一 workspace 禁止并发 Git 操作。
- Git lock/rename 在异常退出时可能残留或处于非原子状态，调用方需要允许清理重试。
- 对大量小文件的安装和遍历不承诺与本地文件系统相同的性能。

## 14. 故障处理

| 场景 | 行为 |
|---|---|
| Secret 缺失或无权限 | sandbox 创建失败，释放租约 |
| bucket 不存在 | 创建失败；一期不由 sandbox runtime 自动创建 bucket |
| 新 workspace prefix 为空或不存在 | 获得租约后以 `PrepareWorkspacePrefix` 创建并验证 profile 兼容的根目录标记；失败则创建失败 |
| prepared 空壳 health 失败 | 从 Pool 丢弃并异步补充；请求尝试下一个空壳或 cold prepare |
| 租约冲突或 prefix 准备失败且未消费授权 | pristine probe 通过后归还同一 PoolKey；否则删除空壳 |
| `mount_attempt` CAS 后挂载/探测失败 | 删除整个 Pod/容器并确认退出，释放租约，绝不回池 |
| mount 超时 | 创建失败，清理 Pod/容器和租约 |
| prepared startup probe 失败 | 空壳不入池并删除；Pool miss 时请求按 prepare timeout 失败 |
| 运行中 FUSE 进程死亡 | 拒绝新 Exec 并停止整个 Pod/容器；一期禁止原地重挂载，确认旧实例退出后以新 lease generation 重建 |
| 重建失败 | mount state=`error`，返回 503，等待销毁或人工诊断 |
| 对象存储网络中断 | readiness 失败并返回 503，不自动重启 mounter，不切换到本地目录 |
| cache 达到软阈值或 ephemeral-storage 超限 | 阻止新 Exec，停止整个 sandbox，尽力 flush 后重建；不承诺写调用先收到 ENOSPC |
| Redis 租约冲突 | 返回 HTTP 409，包含占用 sandbox ID |
| Redis 续租失败 | 立即阻止新 Exec；持续失败则停止整个 sandbox 并做有界、尽力 flush，保留 owner 记录直到确认 runtime 已退出 |
| API 重启 | 从 owner/session 恢复已绑定实例；按 Redis pool record + runtime probe 复核 prepared 空壳，销毁超时/不明 reservation 或 binding；不重复挂载已有健康实例 |
| Pod/容器丢失 | 清理 session 和租约；按持久 sandbox 规则重新创建 |
| 静态 AK/SK 计划轮换 | 停止该 provider 的新建流量，排空并重建全部 FUSE sandbox 后切换 Secret；不热加载凭证 |
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
- `sandbox_workspace_pool_size{runtime,provider,pool_key,state}`
- `sandbox_workspace_pool_acquire_total{runtime,provider,result}`
- `sandbox_workspace_pool_prepare_duration_seconds{runtime,provider}`
- `sandbox_workspace_pool_discard_total{runtime,provider,reason}`
- `sandbox_workspace_fuse_cache_bytes`
- `sandbox_workspace_fuse_errors_total{operation}`

Pod 和 Docker health 状态必须区分 container running 与 FUSE ready，不能仅凭主进程存活判定 workspace 健康。
Kubernetes prepared 空壳长期 Pod NotReady 是预期状态，通用 NotReady 告警必须按 `sandbox.pool.state=prepared` 排除，并由 prepared health、prepare timeout 与 Pool 容量告警替代。

## 16. 代码变更边界

| 文件/组件 | 设计变更 |
|---|---|
| `internal/config/config.go` | Workspace FUSE 配置、默认值与校验 |
| `internal/runtime/types.go`、`internal/runtime/runtime.go` | `WorkspaceFUSESpec`、prepare/authorize/ready 创建状态机、system egress、health/quiesce/flush 控制接口、带 size 的流式上传签名 |
| `internal/sandbox/types.go` | Workspace mount type/state/driver/health/lease 字段 |
| `internal/sandbox/pool.go` | 在现有 Pool 生命周期内增加独立 FUSEPool：按 PoolKey WarmUp、pristine probe、保留/安全归还、single-use 销毁、周期对账与补池 |
| `internal/sandbox/manager.go`、`operation_gate.go`、`cmd/sandbox/main.go` | 由 sandbox-api 构造并 fail-closed 启动/停止 FUSE Pool，编排 Acquire、prefix 绑定、租约/owner CAS、restore、引用计数 gate 与 lifecycle watcher |
| `internal/sandbox/workspace.go` | FUSE 模式跳过 sync；动态 mount/unmount 冲突语义 |
| `internal/runtime/kubernetes/pod.go` | 可预热的原生 sidecar、memory emptyDir、propagation、supervisor generation gate、prepared/readiness probe、Secret、Acquire 后 sandbox 内探测 |
| `internal/runtime/kubernetes/network.go` | system egress 与用户网络规则合并、公共 DNS 和稳定 endpoint/CIDR 策略 |
| `internal/runtime/kubernetes/exec.go` | 保持 exec 固定到 `sandbox`；错误映射 |
| `internal/runtime/docker/container.go` | 特殊镜像、FUSE device/capability/security/health、root supervisor 与用户 exec 分离 |
| `internal/runtime/docker/exec.go` | `execUser` 强制 UID/GID 1000；私有固定 argv `execControl` |
| `internal/runtime/docker/network.go` | network disabled 时仍创建仅含 system egress 的 gateway |
| `internal/runtime/docker/file.go` | 直接 Exec 强制用户；CopyToContainer ownership；上传改为流式 tar |
| `internal/runtime/kubernetes/file.go` | 上传改为 pipe 流式 tar，禁止 `io.ReadAll` 整文件缓存 |
| `internal/api/router.go`、`internal/api/handler/file.go`、`internal/sandbox/manager.go` | upload-only body 上限、`MultipartReader` 流式 size 校验、workspace health gate |
| `cmd/workspace-mounter`、`cmd/workspace-probe` | root supervisor 的 versioned bootstrap/authorize/health/flush，以及 sandbox 内 UID-1000 读写和 quiesce 探针 |
| `pkg/types/workspace.go`、`internal/api/handler/workspace.go` | 为 flush/no-op 增加向后兼容的 mount type、flushed、last flushed 等响应字段 |
| `internal/storage/filesystem.go`、`workspace_marker.go` | 共享 prefix builder、`sub_path` 规范化；FUSE 专用原生 MinIO/OBS 对象客户端负责自定义 CA、兼容目录标记创建/验证/过滤，legacy `goairix/fs` driver 不进入 FUSE 控制路径 |
| `internal/storage/state/redis` | workspace TTL lease、持久 owner/generation，以及 FUSE Pool inventory、reservation/refill lock；lease key 复用共享 prefix builder |
| `deploy/helm/sandbox` | mounter image、Secret、network policy、sidecar 资源配置 |
| `docker` | 特殊 sandbox-fuse image、entrypoint、healthcheck、Secret 示例 |

不在一期修改公共文件 API 的主要请求结构；workspace 状态响应允许增加向后兼容字段。

## 17. 验证与验收

### 17.1 测试矩阵

必须覆盖：

| Runtime | Provider |
|---|---|
| Kubernetes | MinIO |
| Docker | MinIO |
| Kubernetes | 华为公有云 OBS 普通对象桶 |
| Docker | 华为公有云 OBS 普通对象桶 |
| Kubernetes | 2023 私有云 OBS 普通对象桶 |
| Docker | 2023 私有云 OBS 普通对象桶 |

### 17.2 功能测试

- Kubernetes 与 Docker 都验证：Pool WarmUp 后 sandbox 基础容器已经运行，但没有 s3fs 进程、workspace mount、owner/lease，且用户 Exec/file API 被 gate 拒绝。
- Pool hit 时传入动态 `workspace_path`，不新建 Pod/容器即可完成租约、prefix 绑定、挂载和 sandbox 内读写探测；Pool miss 能 cold prepare 并完成相同流程。
- `sandbox.Manager.Start` 会把每个 PoolKey 补到 `min_size`；并发 Acquire、异常移除和周期 reconciliation 都由 sandbox-api 补池，且多副本并发时 `preparing + prepared` 不超过 `max_size`。
- 授权前租约冲突时，pristine 空壳可以安全归还同一 PoolKey；授权后成功、失败和请求取消三种情况都销毁实例并触发补池。
- 更改 provider profile、镜像 digest、credential generation、CA、endpoint/bucket 或安全/网络配置会生成不同 PoolKey，旧 key 空壳被排空且不会被新请求 Acquire。
- 空 `sub_path`、canonical `sub_path` 和 Unicode workspace path 的 prefix 结果在 file API、FUSE source、目录标记与 lease key 中逐字节一致；非 canonical 旧 `sub_path` 在 FUSE 模式启动失败且对象 key 不被改写。
- 对每个 provider/profile 使用全新空 prefix 创建、挂载、写入、销毁和再次挂载，验证根目录标记兼容且不会作为用户文件列出。
- 创建、读取、覆盖、append、truncate、删除文件。
- 创建、遍历、rename、删除目录。
- 零字节、Unicode、空格、长文件名和深层目录。
- 单文件至少 1 GiB 的 multipart 写入和读取。
- 至少 10,000 个小文件的创建、list、stat 和删除。
- s3fs 与 `goairix/fs` 的双向对象可见性。
- 显式文件上传/下载 API 仍可用，1 GiB 上传过程中 sandbox-api 内存保持有界。
- `git clone/status/checkout`。
- pip/npm install，cache 指向 `/tmp`。
- Create/Pool Acquire 内部动态绑定成功；sandbox 交付后的公共 MountWorkspace/UnmountWorkspace 在 FUSE 模式返回 409。
- `SyncWorkspace(from_container)` 执行 flush barrier；`to_container` 返回受约束的 no-op，二者均不复制整棵目录。

### 17.3 安全测试

- 路径穿越、绝对路径、特殊字符和 shell 参数注入。
- sandbox 主容器无法读取 Kubernetes Secret、sidecar `/proc` 或 `/dev/fuse`。
- Pod 明确设置 `automountServiceAccountToken=false`、`enableServiceLinks=false`、`shareProcessNamespace=false`；sandbox 使用 RuntimeDefault seccomp 并 drop ALL capabilities。
- Docker 用户命令 UID/GID 始终为 1000，覆盖所有直接和间接 exec 路径。
- Docker 用户无法读取 root-only Secret、控制 supervisor 或执行 mount/umount。
- 公共请求无法触发 `execControl`、指定 mounter 容器或传入 root 控制 argv。
- symlink 无法逃逸到同 bucket 的其他 workspace。
- NetworkPolicy/gateway 只放行目标 endpoint 和 DNS。
- 同 workspace 第二个读写创建请求稳定返回租约冲突。

### 17.4 故障测试

- kill s3fs、kill Kubernetes sidecar、重启 Docker supervisor 子进程。
- 在活动 Exec 持有 cwd/fd 时 kill s3fs，验证整个 sandbox 被停止并重建，不把 attach 断开或旧句柄标记为已恢复。
- 强制移除 FUSE mount 后尝试通过 Exec 和文件 API 写入，验证底层 emptyDir/容器目录返回权限错误且没有本地文件残留。
- Exec 启动后台/双重 fork 进程并关闭客户端连接，验证 quiesce 不会误判为空闲；安全故障会停止整个 sandbox，`SyncWorkspace` 不返回 `flushed=true`。
- 对象存储断网、DNS 失败、证书错误、AK/SK 失效。
- mount 过程中 Pod/容器被删除。
- 写入中 cache 达到软阈值和 Kubernetes ephemeral-storage eviction。
- Pod 驱逐、节点重启、Docker daemon 重启、sandbox-api 重启。
- API 在 preparing、prepared、reserved、binding 各状态崩溃：prepared record 只有重新通过 pristine probe 才可继续使用；超时 preparing/reserved 和 binding 都先经持久 owner/session/gate guard 分类，只有明确 Pristine 或 Abandoned（binding 仅 Abandoned）才销毁并补充，Protected、Unknown 或检查错误必须保留且不可领取，因此不会重复挂载或被其他副本误删。
- 节点重启导致 `mounter-run` 丢失时，旧 Pod 保持 locked 且同一 `mount_attempt` 不能再次授权；只有旧 Pod API 对象 NotFound 且进程退出已确认（或节点已 fencing）后才能以新 UID/generation 重建。
- 优雅卸载失败和 stale FUSE mount 恢复。
- Redis 短暂不可用、租约续期失败、API 崩溃超过一个 TTL、owner 记录恢复和安全接管。
- provider 静态 AK/SK 计划轮换时的停止新建、排空与受控重建。

### 17.5 验收标准

- 上述六个 runtime × target profile 组合的 A 类功能测试全部通过；公有云与私有云结果不能互相替代。
- 两个 runtime 的 Pool hit 都能在不创建新 Pod/容器的情况下按请求 prefix 完成挂载；未挂载空壳已启动但无法执行用户代码。
- FUSE workspace 的创建、运行和销毁路径不调用 tar 同步方法。
- 挂载未 ready 时 sandbox 基础进程可以预热运行，但任何用户 Exec/file 操作都不会启动；runtime 不能只以 Pod Running、Docker health 或容器主进程存活判定 workspace ready。
- 任一 FUSE 故障不会导致数据写入本地空目录。
- 同 workspace 并发 RW sandbox 被阻止，包括 Redis lease 已过期但旧 runtime 仍存活的场景。
- 同一 Pod UID/lease generation 的挂载授权只能消费一次，memory emptyDir 丢失不会使旧 Pod 自动重挂载。
- 同一物理 bucket 通过两个 endpoint 别名或滚动切换 endpoint 时仍使用相同 `storage_identity`，第二个 RW sandbox 被阻止。
- Docker 安全测试无法获得 root、Secret、`SYS_ADMIN` 或 mounter 控制能力。
- 1 GiB 显式上传和 FUSE 文件传输时 sandbox-api 内存不随文件大小线性增长。
- 对象存储恢复后，Kubernetes sidecar 和 Docker supervisor 能按策略恢复或稳定进入 error 状态。

性能基线不在文档中预设绝对数值；灰度前在目标 MinIO、OBS 网络环境采集旧 sync 与新 FUSE 的创建延迟、首读延迟、顺序吞吐和 10,000 小文件耗时，以实测结果确定发布阈值。

## 18. 灰度与回滚

### 18.1 灰度

1. 保留 `workspace.mode=sync` 默认值，先完成 MinIO/OBS provider spike，产出固定 profile、镜像 digest 和 TLS/签名验证结果。
2. 完成 runtime、FUSE Pool、租约、flush、system egress、流式文件 API 的单元和集成测试。
3. 先在测试环境只启动 prepared 空壳而不接用户流量，验证无 mount/owner/lease、Exec gate、NotReady、补池和配置换代排空。
4. 分别完成 MinIO、华为公有云 OBS、2023 私有云 OBS 与两套 runtime 的六组合 Pool hit/miss、Acquire 挂载与 single-use 销毁验证。
5. MinIO Kubernetes 小流量开启 `mode=fuse`。
6. 扩展到 MinIO Docker。
7. 分别为已验证的公有云/私有云 OBS profile 扩展 Kubernetes。
8. 最后分别扩展公有云/私有云 OBS Docker。
9. 每个 profile 观察至少一个完整 sandbox TTL 周期，并完成一次 Redis/API 故障接管及 Pool refill 演练后再扩大流量。

### 18.2 回滚

- 回滚只对新建 sandbox 生效；已经运行的 FUSE sandbox 保持原模式直到销毁。
- 停止 FUSE Pool refill 并删除所有未绑定 prepared 空壳；reserved/binding 状态按不确定实例销毁，不能转入 sync Pool。
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
- 华为云公有云 CCE OBS 对象桶挂载：https://support.huaweicloud.com/usermanual-cce/cce_10_0630.html
- 华为云公有云 CCE OBS 挂载参数：https://support.huaweicloud.com/intl/zh-cn/usermanual-cce/cce_10_0631.html
- 双华云私有云 CCE OBS 概述（对象桶/并行文件系统、常驻进程）：https://docs.shuanghuayun.com/zh-cn/usermanual/cce/cce_10_0628.html
- 双华云私有云 CCE OBS 挂载参数（s3fs/obsfs、sigv2、compat_dir）：https://docs.shuanghuayun.com/zh-cn/usermanual/cce/cce_10_0631.html
- 双华云私有云 OBS API 签名验证：https://docs.shuanghuayun.com/zh-cn/api/obs/obs_04_0009.html
- Kubernetes Sidecar Containers：https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/
- Kubernetes Mount Propagation：https://kubernetes.io/docs/concepts/storage/volumes/#mount-propagation
- Docker Exec 用户参数：https://docs.docker.com/reference/cli/docker/container/exec/
