# Workspace FUSE 部署与运维手册

**适用设计：** [Workspace 容器内 FUSE 直接挂载设计](../superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md)

**适用范围：** Kubernetes sidecar、Docker 特殊容器、MinIO、华为 OBS 普通对象桶

**文档状态：** 控制面装配、启动恢复、FUSE Pool、Kubernetes sidecar、Docker 特殊容器、Helm/Compose 配置和 preflight 入口均已实现。真实 provider 六组合兼容性矩阵与 durable-flush 证据仍是发布门禁；当前三个 profile 在证据提升前仍不能用于生产启用 `workspace.mode=fuse`。现有 `workspace.mode=sync` 部署不受影响。

## 0. 当前部署入口

- Helm：`deploy/helm/sandbox`。`workspace.mode=sync` 保持默认；FUSE 渲染示例是 `testdata/values-fuse-minio.yaml`，其中 digest、IP 和 Secret 名仅为测试占位，部署前必须替换为 Task 18 的实测产物。
- Kubernetes：sandbox-api 位于 control namespace，动态 sandbox Pod 位于 runtime namespace；Chart 分别创建 runtime Role/RoleBinding 和 runtime default-deny。运行时 Secret 必须预先存在于 runtime namespace，控制面读取的同内容 Secret则位于 control namespace，因为 Kubernetes 不允许跨 namespace 投射 Secret。
- Docker Compose：只启动控制面、Redis 和镜像构建辅助服务。特殊 FUSE 容器由 sandbox-api 动态创建；Compose 不创建长期 mounter 容器，也不挂载宿主机业务 `/workspace`。
- Docker Secret 暂存：`${WORKSPACE_SECRET_STAGING_ROOT}` 必须是 Docker 宿主机上的绝对目录，并以相同绝对路径挂入 sandbox-api；目录必须为 `root:root`、mode `0700`。`${WORKSPACE_CREDENTIAL_DIR}` 只读挂到 `/run/secrets/workspace`。
- 预检：先执行 `bash -n scripts/workspace-fuse-preflight.sh`；有真实 profile、digest 和 Secret 后执行 `scripts/workspace-fuse-preflight.sh kubernetes --profile <report.yaml>` 或 `docker --profile <report.yaml>`。设置 `WORKSPACE_FUSE_RUN_INTEGRATION=1` 才会进入完整 Acquire/读写/销毁集成用例。

FUSE 模式下控制面不构造旧 `goairix/fs` MinIO/OBS driver：它只读取一次文件型 AK/SK 创建原生 marker/probe object client，随后立即清零持有的字节缓冲。`Manager.Start` 负责恢复、orphan reconcile 和 Pool WarmUp；任一步失败都会发生在 HTTP listener 启动前。

## 1. 部署结论

- Kubernetes：每个 sandbox Pod 注入一个 FUSE 原生 sidecar，业务 `/workspace` 使用 Pod 私有 `emptyDir`，不挂载宿主机业务目录。sidecar 负责挂载，sandbox 主容器仅消费传播后的 mount。
- Docker：每个 sandbox 使用自带 s3fs 和 root supervisor 的特殊镜像，API 通过 Docker Device 映射 `/dev/fuse`。不在宿主机挂载 `/workspace`，也不运行宿主机常驻 s3fs 进程。
- Pool 预先启动没有 workspace mount 的 locked 空壳；provider、endpoint、bucket、profile、镜像、Secret 和 system egress 固定，收到带 `workspace_path` 的创建请求后才启动 s3fs 挂载对应 prefix。授权后的实例使用完必须销毁，不能卸载后回池。
- Pool 数量由 `sandbox-api` 自身维护：启动时 WarmUp，Acquire/移除后异步补池，周期对账，停止或禁用时 Drain；不部署独立 Pool Controller、Operator、CronJob 或 DaemonSet。Redis 只保存跨副本库存状态与协调锁。
- MinIO 和华为 OBS 使用独立 provider profile、镜像和 digest。不得让租户或 API 调用方传入任意 s3fs 参数。
- 一期使用 provider 级静态长期 AK/SK 和 root-only s3fs `passwd_file`，不支持 STS/session token。挂载路径只向用户暴露当前 prefix，但静态凭证本身不提供单 workspace IAM 隔离。
- FUSE cache 默认使用节点磁盘和软容量阈值；达到阈值或被 kubelet eviction 时终止并重建 sandbox，不承诺写入立即返回 ENOSPC。
- Kubernetes 保持 `DNSPolicy=None` 和公共 nameserver。MinIO 必须使用公共 DNS 可解析的稳定 endpoint 或显式 IP，不支持直接使用 `cluster.local` Service。
- 对象存储 endpoint 属于系统必需网络。即使用户网络关闭，也必须通过受控 system egress 或 gateway 可达。
- 凭证不写入 Helm values、Compose environment、容器命令行、镜像或日志。
- `mode=fuse` 必须与 `quota_mode=soft` 同时显式开启；对象存储不能提供与当前本地目录相同的严格磁盘配额语义。

## 2. 部署组件与职责

| 组件 | Kubernetes | Docker | 职责 |
|---|---|---|---|
| sandbox-api | Helm Deployment | Compose service | 创建/销毁 sandbox、签发运行时规格、维护租约与状态，并直接维护 FUSE Pool 数量、补池和排空 |
| FUSE 进程 | 每个预热 Pod 的 sidecar，Acquire 后启动 s3fs | 每个预热特殊容器内的 root supervisor，Acquire 后启动 s3fs 子进程 | 空壳阶段保持 locked；使用时将单一 workspace prefix 挂载到 `/workspace` |
| 用户进程 | sandbox 主容器，UID/GID 1000 | 同一容器内由 supervisor/exec 强制 UID/GID 1000 | 访问 `/workspace`，不能控制 FUSE |
| Secret | Pod Secret volume，仅 sidecar 可见 | 宿主机 root-only 暂存文件，仅挂载到特殊容器 | 提供 provider 级静态长期 AK/SK 和自定义 CA |
| Redis | Helm 内置或外部 Redis | Compose Redis 或外部 Redis | TTL lease、持久 owner、generation、runtime identity，以及跨 API 副本的 FUSE Pool inventory/reservation |
| system egress | 每个 Pool 空壳预建 NetworkPolicy/Cilium 策略 | Pool WarmUp 时创建受控 gateway 网络 | 只允许 DNS 和固定对象存储 endpoint；Acquire 后再叠加用户网络规则 |

Helm Chart 只部署控制面。sandbox Pod 由 Kubernetes runtime 动态生成，sidecar、volume、probe 和 sandbox 级 NetworkPolicy 必须在 Go runtime 中实现，不能静态加入 sandbox-api Deployment。

Compose 同样只部署控制面和依赖。特殊 sandbox 容器由 Docker runtime 动态创建，不作为长期 Compose service。

## 3. 交付物和版本固定

发布前应生成并固定以下镜像：

| 变量 | 内容 |
|---|---|
| `SANDBOX_API_IMAGE` | sandbox-api 镜像，使用 digest |
| `SANDBOX_BASE_IMAGE` | 普通 sync/Kubernetes sandbox 镜像，仅包含 `workspace-probe`，使用 digest |
| `MINIO_MOUNTER_IMAGE` | Kubernetes MinIO s3fs sidecar 镜像，使用 digest |
| `OBS_PUBLIC_MOUNTER_IMAGE` | Kubernetes 华为公有云 OBS s3fs sidecar 镜像，独立 digest |
| `OBS_PRIVATE_2023_MOUNTER_IMAGE` | Kubernetes 2023 私有云 OBS s3fs sidecar 镜像，独立 digest |
| `MINIO_SANDBOX_FUSE_IMAGE` | Docker MinIO 特殊 sandbox 镜像，使用 digest |
| `OBS_PUBLIC_SANDBOX_FUSE_IMAGE` | Docker 华为公有云 OBS 特殊 sandbox 镜像，独立 digest |
| `OBS_PRIVATE_2023_SANDBOX_FUSE_IMAGE` | Docker 2023 私有云 OBS 特殊 sandbox 镜像，独立 digest |
| `GATEWAY_IMAGE` | Docker system egress gateway 镜像，使用 digest |

生产环境不得使用 `latest` 或可变 tag。镜像中应记录：

- s3fs 版本、commit 和构建参数；
- provider profile 版本；
- 基础镜像 digest；
- SBOM 和漏洞扫描结果；
- 支持的架构；
- CA bundle 版本。

MinIO 与 OBS 可共用源码仓库和构建流水线，但必须能独立升级和回滚。华为公有云与 2023 私有云也必须使用独立 profile、构建 job、镜像 digest 和验证报告。对应 OBS provider spike 如果证明上游 s3fs 不满足签名或目录语义要求，只能让该 profile 切换为固定的厂商兼容构建，不能悄悄复用 MinIO 或另一套 OBS 镜像。

### 3.1 Task 12 镜像边界

- Kubernetes：`docker/images/workspace-mounter/Dockerfile` 构建专用 sidecar，包含 s3fs、CA roots、严格 profile manifest 与 `workspace-mounter`，不包含语言运行时。
- Docker：`docker/images/sandbox-fuse/Dockerfile` 以普通 sandbox runtime 为基础构建特殊镜像，保留 Python/Node 等工具，同时加入 s3fs、`workspace-mounter` 与 `workspace-probe`。
- 普通 sandbox：`docker/images/sandbox/Dockerfile` 只安装 root-owned mode `0755` 的 `workspace-probe`，不得出现 s3fs、`workspace-mounter` 或 profile manifest。

### 3.2 不可变构建输入与 profile 绑定

每个 profile 的 CI job 必须先构建专属 mounter 二进制。`PROFILE_ID` Docker build argument 只用于交叉校验，不能改变已经编译的二进制：

```bash
PROFILE_ID=minio-sigv4-path-style-v1
: "${BUILD_ARTIFACT_DIR:?set BUILD_ARTIFACT_DIR}"
GOOS=linux GOARCH=amd64 go build \
  -ldflags "-X=main.imageProfileID=${PROFILE_ID}" \
  -o "$BUILD_ARTIFACT_DIR/workspace-mounter" ./cmd/workspace-mounter

GOOS=linux GOARCH=amd64 go build \
  -o "$BUILD_ARTIFACT_DIR/workspace-probe" ./cmd/workspace-probe
```

构建上下文组装步骤把同一个 profile-bound mounter artifact 分别放入 Kubernetes mounter 与 Docker 特殊镜像上下文，把 `workspace-probe` 放入 Docker 特殊镜像和普通 sandbox 上下文；不得重新执行一个没有 `-ldflags` 绑定的普通 mounter build 覆盖它。

普通 sandbox 不提交生成 binary。`docker/images/sandbox/Dockerfile` 的 `workspace-probe-builder` 阶段从 repository-root context 复制 `go.mod`/`go.sum`、`cmd/workspace-probe`、`internal/fuseprotocol` 和 `internal/workspaceprobe`，并以 `CGO_ENABLED=0` 编译静态 probe。`docker/docker-compose.yml` 将仓库根目录只读挂载到 `/repo`，使用：

```bash
docker build -f /repo/docker/images/sandbox/Dockerfile -t sandbox:latest /repo
```

因此旧的 `/images/sandbox` context 已废弃。开发默认 `WORKSPACE_PROBE_BUILDER=golang:1.25-alpine`、`SANDBOX_BASE_IMAGE=python:3.13-slim`；生产 CI 必须分别传入 digest-pinned builder 和 sandbox base，尤其不能沿用可变的默认 builder tag。

CI 必须向两个 FUSE Dockerfile 传入：

- 带 `@sha256:<64 lowercase hex>` 的不可变 `BASE_IMAGE`；
- 不含 URL credential/query/fragment 的 HTTPS `S3FS_PACKAGE_URL`；
- 与下载内容一致且非全零的 `S3FS_PACKAGE_SHA256`；
- 精确 `PROFILE_ID` 和对应的只读 manifest。

manifest 是严格审计证据，不是运行时配置扩展点。它记录 profile/status/参数元数据和 s3fs SHA-256，并与编译进 Go 二进制的 typed catalog 逐项匹配；只有 compiled catalog 可以构造 s3fs argv。未知字段、重复字段、ID/状态不匹配、s3fs hash 不匹配或未通过 ldflags 绑定都会使镜像构建自检失败。package self-check 还会真实执行固定 `s3fs --version`，以提前发现 artifact 架构错误、loader 缺失或动态依赖缺失。任何租户、API 参数或 manifest 都不能注入额外 `-o` 参数。

当前仓库没有真实 `BASE_IMAGE` digest、s3fs artifact URL/hash 或已发布镜像 digest；这些输入及实际 image build、漏洞扫描、CycloneDX SBOM、签名 attestation 均由 CI/Task 18 生成，不能以本地 contract test 或 Compose config 校验代替。

### 3.3 package-check 与 release-check

`package-check` 只证明镜像内容、权限、manifest/binary/artifact 绑定及 s3fs 可执行性正确；`release-check` 必须调用镜像内固定命令 `workspace-mounter health prepared --release-check-image`，由 Go `CheckProductionProfile` 进一步要求 profile 的 mount parameters 与 durable flush 都为 `verified`。不得在外层脚本中仅 grep manifest 来决定发布资格。当前状态为：

| Profile ID | Mount parameters | Durable flush | 当前部署资格 |
|---|---|---|---|
| `minio-sigv4-path-style-v1` | `verified` | `blocked-pending-flush-spike` | 禁止启用 FUSE |
| `huawei-obs-public-v1` | `candidate` | `blocked-pending-flush-spike` | 禁止启用 FUSE |
| `huawei-obs-private-2023-v1` | `unverified` | `blocked-pending-flush-spike` | 禁止启用 FUSE |

因此三者当前都必须在 `release-check` 和 `workspace.mode=fuse` 配置校验处 fail closed。MinIO 的 mount 参数已冻结不代表 durable flush 已获证明；公有 OBS 只是从官方资料得到的候选参数；2023 私有云不能继承公有云结论。不得使用 `no_check_certificate` 或 `ssl_verify_hostname=0` 绕过任何门禁。

静态 contract test：

```bash
bash -n scripts/verify-fuse-image.sh scripts/test-fuse-images.sh
bash scripts/test-fuse-images.sh
```

CI 取得真实 digest 后，分别运行两个 runtime 与两种 gate：

```bash
FUSE_IMAGE="$MOUNTER_DIGEST" SANDBOX_IMAGE="$ORDINARY_SANDBOX_DIGEST" \
  scripts/verify-fuse-image.sh kubernetes package-check "$PROFILE_ID"
FUSE_IMAGE="$MOUNTER_DIGEST" SANDBOX_IMAGE="$ORDINARY_SANDBOX_DIGEST" \
  scripts/verify-fuse-image.sh kubernetes release-check "$PROFILE_ID"

SANDBOX_IMAGE="$SPECIAL_SANDBOX_DIGEST" \
  scripts/verify-fuse-image.sh docker package-check "$PROFILE_ID"
SANDBOX_IMAGE="$SPECIAL_SANDBOX_DIGEST" \
  scripts/verify-fuse-image.sh docker release-check "$PROFILE_ID"
```

现阶段正确结果是结构正确的候选镜像可通过 `package-check`，但三个 profile 的 `release-check` 都返回非零。只有 Task 18 对某个 runtime/profile 完成真实环境证据、显式提升 compiled catalog 和 manifest 状态并重新构建后，该组合才允许通过 release gate。

## 4. 通用前置检查

### 4.1 节点能力

Kubernetes 节点和 Docker 宿主机均执行：

```bash
test -c /dev/fuse
stat -c '%F %a %U:%G' /dev/fuse
grep -w fuse /proc/filesystems
```

预期：`/dev/fuse` 是字符设备，内核支持 FUSE。若 `/proc/filesystems` 未列出 FUSE，应先由节点管理员加载内核模块；不在 sandbox-api 内执行节点级模块加载。

### 4.2 对象存储与 TLS

- endpoint 必须是运维配置，不能来自 CreateSandbox 请求。
- `storage.filesystem.endpoint` 保持现有 driver 的 provider-native 格式：MinIO 必须是 `host[:port]` 且由 `use_ssl` 决定协议，OBS 按华为 SDK 要求使用完整 endpoint。runtime 必须从这些字段派生并校验 s3fs 的完整 `url=https://...` 参数，不能把同一字符串未经转换同时传给控制面 driver 和 s3fs。
- 生产必须启用 TLS 和主机名校验。
- 私有 CA 通过同一外部 Secret 源分别投射给 sandbox-api 和 mounter；控制面存储客户端与 s3fs 都必须显式使用该 CA，不修改宿主机全局 CA。CA 轮换需要排空并重建相关 FUSE sandbox。
- endpoint 使用 DNS 名时，优先由配置的公共 nameserver 解析。私有云 FQDN 无法公共解析时，使用运维维护的 Pod `hostAliases`/Docker `extra_hosts`，把证书匹配的 FQDN 映射到平台批准的稳定 IP；映射不得来自用户请求，IP 变更必须走受控发布。只有证书包含 IP SAN 时才允许直接使用 IP endpoint。一期不支持 egress proxy。
- Kubernetes 不接入 CoreDNS；`*.svc.cluster.local` 不能作为 MinIO endpoint。需要使用专用稳定 FQDN 或显式 IP。
- MinIO 明确使用 path-style；OBS 的签名版本、region 和 path-style 由 provider spike 的固定结果决定。
- bucket 必须已存在。挂载器不得以高权限凭证自动创建 bucket。

### 4.3 Redis

FUSE 模式依赖 Redis 独占租约。生产必须使用持久化且具备故障恢复能力的 Redis；内置单实例 Redis 仅适合开发或验证环境。部署前确认：

- sandbox-api 所有副本连接同一 Redis；
- Redis 数据持久化和备份已启用；
- FUSE Pool record、preparation/reservation/cleanup token 与 refill lock 使用同一 Redis；它们只供 `sandbox-api` 多副本保存状态和协调，不能以各副本的进程内队列作为 prepared inventory 状态源，也不代表 Redis 负责补池；
- Pool 的 prepare/reservation/cleanup 到期时间统一由 Redis `TIME` 计算；API 主机时钟偏移不得参与过期判定；
- 监控覆盖连接错误、租约续期失败、owner 冲突、preparation/reservation/cleanup 超时和 refill lock 续租/fencing 异常；
- API 重启后可以按 runtime identity 恢复 owner 状态。

## 5. Secret 管理

### 5.1 Kubernetes

FUSE sidecar 使用的 Secret 创建在 sandbox Pod 所在 namespace，而不是只创建在 sandbox-api namespace。sandbox-api 的显式文件 API 也需要访问同一存储，因此控制面 namespace 还需要一份由同一外部 Secret 源同步的 Secret；该 Secret 只投射给 sandbox-api，不能投射给 sandbox 主容器。推荐由外部 Secret 管理器同步；手工验证环境可使用 root-only 临时文件：

```bash
install -m 600 /dev/null workspace-storage.env
printf 'accessKey=%s\nsecretKey=%s\n' \
  "$STORAGE_ACCESS_KEY" "$STORAGE_SECRET_KEY" > workspace-storage.env
kubectl -n "$SANDBOX_NAMESPACE" create secret generic sandbox-workspace-minio \
  --from-env-file=workspace-storage.env \
  --dry-run=client -o yaml | kubectl apply -f -
rm workspace-storage.env
```

当控制面与 runtime namespace 分离时，使用同一外部 Secret 源在 `CONTROL_NAMESPACE` 生成 `sandbox-storage-minio`，供 sandbox-api 的存储 driver 使用；上面的 `sandbox-workspace-minio` 只供 sidecar 使用。若两个组件在同一 namespace，可以让两处配置引用同一个 Secret 名称，但 volume 仍只投射给各自受信任的容器。

使用私有 CA 时，runtime namespace 的 sidecar Secret 和 control namespace 的 sandbox-api Secret 都增加同源 `ca.crt`：

```bash
kubectl -n "$SANDBOX_NAMESPACE" create secret generic sandbox-workspace-minio \
  --from-literal=accessKey="$STORAGE_ACCESS_KEY" \
  --from-literal=secretKey="$STORAGE_SECRET_KEY" \
  --from-file=ca.crt="$MINIO_CA_FILE" \
  --dry-run=client -o yaml | kubectl apply -f -
```

第二种写法适合受控终端，但 literal 可能进入本机进程参数审计；生产优先使用 External Secrets、Sealed Secrets 或等价设施。

Pod 直接 `secretKeyRef`/Secret volume 引用已存在的 provider Secret 时，sandbox-api ServiceAccount 不需要 `get/list/watch secrets`。一期不创建 per-sandbox Secret，也不接受 `sessionToken`；Secret 中出现该字段时配置校验失败。现有 Chart 把 `config.storage.filesystem.accessKey/secretKey` 直接渲染为普通环境变量；FUSE 实现合入时必须改成 `secretKeyRef` 或文件型 credential provider，并删除 values 中的明文凭证入口。

sandbox-api 的配置同时把控制面 Secret 中的 `ca.crt` 映射为 `storage.filesystem.ca_file`。FUSE 控制路径使用本仓库基于 MinIO/华为 OBS 原生 SDK 的 `WorkspaceObjectClient` 注入该 CA；不支持自定义 transport 的 `goairix/fs` v0.3.11 只用于 sync 模式。FUSE 公共上传/下载通过容器内已挂载的 `/workspace`，不能旁路到旧 driver。

### 5.2 Docker

Docker sandbox 不能把凭证放入 `Env` 或 `Cmd`，否则可通过 `docker inspect` 读取。宿主机准备专用暂存根目录，并以相同绝对路径读写挂载给受信任的 sandbox-api；API 为动态容器生成子目录后，再把该子目录只读挂载到特殊 sandbox：

```bash
sudo install -d -m 0700 -o root -g root /var/lib/sandbox/fuse-secrets
```

运行时为每个 sandbox 创建独立子目录和 mode `0600` 文件；特殊容器只读挂载该子目录。销毁容器后立即删除文件，reconciler 负责清理失联容器留下的过期目录。文件名和目录名只使用经过校验的内部 sandbox ID，不使用用户输入路径。

## 6. Kubernetes 部署

### 6.1 Namespace 与安全策略

推荐把 sandbox Pod 放在专用 runtime namespace，和 sandbox-api 控制面分离。集群必须支持 Kubernetes 原生 sidecar container（init container 中的 `restartPolicy: Always`）；Kubernetes 1.29 及以上默认启用该能力，部署仍需通过服务端 dry-run 检查字段和准入，并通过真实预检 Pod 验证运行行为。FUSE sidecar 需要挂载能力，第一期使用可信 `privileged` sidecar；因此 runtime namespace 必须通过 Pod Security Admission 或等价准入策略允许该 sidecar。sandbox 主容器仍保持：

- `runAsNonRoot: true`；
- `runAsUser: 1000`、`runAsGroup: 1000`；
- `allowPrivilegeEscalation: false`；
- `readOnlyRootFilesystem: true`；
- `capabilities.drop: ["ALL"]`；
- 不挂载 Secret 和 `/dev/fuse`。

Pod 级强制设置 `automountServiceAccountToken: false`、`enableServiceLinks: false`、`shareProcessNamespace: false` 和 RuntimeDefault seccomp。Acquire 后的固定 workspace 读写探测在 sandbox 主容器内以 UID/GID 1000 通过可信 runtime 控制路径执行，不增加 privileged init container。

不要为了 sidecar 把 sandbox 主容器改成 privileged，也不要对整个业务 namespace 放宽安全基线。

在启用 FUSE 前，必须在实际 `SANDBOX_NAMESPACE` 预先部署 namespace 级 default-deny ingress/egress policy。控制面 Helm release 位于其他 namespace 时，其静态 NetworkPolicy 不会保护 runtime namespace。Pool WarmUp 必须先创建带唯一 instance selector 的 system egress allow policy，确认 API Server 已保存后再创建匹配的空壳 Pod；selector 必须使用生命周期内不可变的 `sandbox.pool.instance`，不能使用会从 `preparing` 变化为 `prepared/reserved/binding/consumed` 的 state label。Acquire 后、开放 Exec 前再应用用户网络策略。销毁时先删除 Pod 并确认退出，再删除策略。这样 Pod 从预热开始就只具备固定 system egress，不会经历默认全放行窗口。

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: sandbox-runtime-default-deny
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
```

该基线 policy 必须由 runtime namespace 的基础设施部署长期持有，不随单个 sandbox 删除。

### 6.2 `/dev/fuse` 与 workspace volume

推荐优先使用集群认可的 FUSE 设备插件。没有设备插件时，可只为 `/dev/fuse` 字符设备配置 hostPath：

```yaml
volumes:
  - name: dev-fuse
    hostPath:
      path: /dev/fuse
      type: CharDevice
  - name: workspace
    emptyDir:
      medium: Memory
  - name: fuse-cache
    emptyDir:
      sizeLimit: 2Gi
```

这里的 hostPath 仅暴露字符设备，不承载数据。业务 `/workspace` 仍是 Pod 私有 `emptyDir`，不会映射到用户指定的宿主机目录。因为 `mountPropagation: Bidirectional`，mount 会在 kubelet 管理的 Pod volume 路径下短暂进入宿主机 mount namespace；Pod 删除和异常回收必须清理该 mount。

`fuse-cache.sizeLimit` 不是文件系统硬 quota。必须同时为 mounter 配置 `ephemeral-storage` request/limit，并监控 cache；达到软阈值或 kubelet 发出 eviction 时阻止新 Exec 并停止整个 Pod，执行有界、尽力 flush 后重建，不保证用户写调用先收到 ENOSPC，也不能把取消 exec 流当作进程退出。

### 6.3 目标 Helm values

以下配置块是实现完成后 Chart 应支持的目标 schema。它补充现有 `config.workspace`，不应把 AK/SK 写入 values。当前即使填入真实镜像 digest，MinIO 和 OBS 也都会因 durable-flush/profile release gate 未通过而拒绝启动；示例只用于开发和渲染检查：

```yaml
config:
  storage:
    filesystem:
      provider: minio
      bucket: sandbox-storage
      region: us-east-1
      endpoint: minio-fuse.example.com:9000
      subPath: workspaces
      useSSL: true
  workspace:
    mode: fuse
    quotaMode: soft
    autoSyncIntervalSeconds: 0
    secretName: sandbox-workspace-minio
    cacheMedium: disk
    cacheSize: 2Gi
    mountTimeoutSeconds: 60
    flushTimeoutSeconds: 30
    unmountTimeoutSeconds: 20
    recreateMaxAttempts: 1
    leaseTTLSeconds: 90
    leaseRenewIntervalSeconds: 30
    fusePool:
      minSize: 3
      maxSize: 20
      refillIntervalSeconds: 10
      prepareTimeoutSeconds: 120
    mounterResources:
      cpuRequest: 50m
      cpuLimit: "1"
      memoryRequest: 64Mi
      memoryLimit: 512Mi
      ephemeralStorageRequest: 512Mi
      ephemeralStorageLimit: 3Gi
    providers:
      minio:
        driver: s3fs
        profile: minio-sigv4-path-style-v1
        storageIdentity: minio-prod-primary
        caSecretKey: ca.crt
        credentialGeneration: "2026-09-03-01"
        endpointHostIPs: []
        mounterImage: registry.example.com/sandbox-s3fs-minio@sha256:<64-hex-digest>
        dockerImage: registry.example.com/sandbox-fuse-minio@sha256:<64-hex-digest>
        lsmProfile: sandbox-fuse
        systemEgressMode: cidr
        dnsCIDRs: ["8.8.8.8/32", "1.1.1.1/32"]
        endpointPorts: [9000]
        proxyURL: ""
        systemEgressFQDNs:
          - minio-fuse.example.com
        # CIDR mode uses only this approved destination in standard NetworkPolicy;
        # the FQDN above remains the TLS/DNS name and is not an NP selector.
        systemEgressCIDRs: ["192.0.2.10/32"]
      obs:
        driver: s3fs
        # 当前仅为 candidate，以下配置会被 fail-closed 校验拒绝；
        # Task 18 完成公有云实测并显式提升状态后方可启用。
        profile: huawei-obs-public-v1
        storageIdentity: huawei-obs-public-cn-north-4
        caSecretKey: ca.crt
        credentialGeneration: "2026-09-03-01"
        endpointHostIPs: []
        mounterImage: registry.example.com/sandbox-s3fs-obs@sha256:<64-hex-digest>
        dockerImage: registry.example.com/sandbox-fuse-obs@sha256:<64-hex-digest>
        lsmProfile: sandbox-fuse
        systemEgressMode: cidr
        dnsCIDRs: ["8.8.8.8/32", "1.1.1.1/32"]
        endpointPorts: [443]
        proxyURL: ""
        systemEgressFQDNs:
          - obs.cn-north-4.myhuaweicloud.com
        systemEgressCIDRs: ["192.0.2.20/32"]

storageCredentials:
  existingSecret: sandbox-storage-minio
  accessKeyKey: accessKey
  secretKeyKey: secretKey
  caKey: ca.crt
```

`<64-hex-digest>` 必须替换为真实镜像 digest。运行时只选择与 `storage.filesystem.provider` 同名且已通过 release gate 的 profile；没有验证结果、durable flush 未验证或 profile 不匹配时启动失败。上面的公有云 OBS 段只是展示配置形状，当前不能启用。不同 provider 推荐使用独立 release values，避免 endpoint、bucket、Secret 与 profile 交叉配置；公有云与 2023 私有云也分别使用独立 values、digest 和报告。

`workspace.fusePool.minSize/maxSize` 表示当前活动 provider 配置下的 prepared 空壳数量，而不是已挂载 workspace 的复用容器；它与现有通用 `pool` 分开，后者继续服务 sync/无 workspace sandbox。和当前 Pool 一样，FUSE Pool 由 `sandbox-api` 的 Manager 启动并维护：启动时 WarmUp，Acquire/异常移除后调用 `refillIfNeeded`，并按 `refillIntervalSeconds` 周期对账。Redis 让多个 sandbox-api 副本原子领取空壳、续租本轮 refill 权并计算全局水位；进程内 slice 只能做非权威缓存。创建 runtime 前必须先以不可复用的 `PreparationID/spec.ID` 原子占用容量槽，`maxSize` 只限制 preparing + prepared；RuntimeUID 返回后原子绑定且不可变。reservation 超时不能自动回池，必须经 Manager owner/session/gate guard 得到明确 disposition。Protected、Unknown 或 guard 检查错误的记录保持不可领取且不得删除；只有明确 Abandoned（或无引用的 Pristine 过期空壳）才能先 claim cleanup、再精确删除 `(RuntimeID, RuntimeUID)`。guard 已明确证明 Pristine 后若 prepared runtime health 明确失败，也必须 cleanup 而不能重新发布。`reserved → prepared` 在同一 Redis 事务中重新检查 `preparing + prepared < maxSize`；若异步 refill 已占满容量，则销毁旧 reservation。删除失败保留 cleanup tombstone 重试。`minSize=0` 只启用 cold prepare；生产要获得预热收益必须配置 `minSize>=1`，并按实测突发并发量定容。

PoolKey 必须覆盖 runtime、sandbox 镜像/资源/安全配置、provider、storage identity、bucket、endpoint、profile、mounter 镜像 digest、Secret 名、`credentialGeneration`、CA、cache 和 system egress；明确排除 Acquire 时才知道的 `workspace_path/prefix`、workspace identity、lease generation、reservation token 和请求级用户网络规则。任一固定字段变化都创建新 key 并排空旧 key 空壳；请求固定字段与现有 key 不匹配时只能 cold prepare。Secret 或 CA 每次轮换都必须递增 `credentialGeneration`，因为 sandbox-api 不读取 runtime namespace Secret 的 resourceVersion。

`storageCredentials` 是 Chart 层配置：实现后应把该 Secret 作为只读文件投射给 sandbox-api，并将文件路径映射到 `storage.filesystem.credential_files`。它和 `config.workspace.secretName` 职责不同：前者供控制面存储 driver 使用，后者是动态 sandbox sidecar 的 Secret 引用。

MinIO 的 `storage.filesystem.endpoint` 必须写为 `host[:port]`，不能带 `https://`；`useSSL: true` 由现有控制面 driver 使用，runtime 再派生 s3fs 的完整 `url=https://host[:port]`。OBS endpoint 继续遵循华为 SDK 的完整 URL 格式。`storageCredentials.caKey` 在使用公共 CA 时可留空；非空时必须同时接入控制面 transport 和 sidecar。

部署前先渲染并检查 schema/准入。以下脚本假定 sandbox-api ServiceAccount/RBAC 已存在；首次安装必须先以 `workspace.mode=sync` 安装控制面和 RBAC，完成预检后才用 FUSE values 升级，不能让尚未验收的 FUSE 创建入口先对外生效：

```bash
#!/usr/bin/env bash
set -euo pipefail

helm lint deploy/helm/sandbox
helm template sandbox deploy/helm/sandbox \
  --namespace "$CONTROL_NAMESPACE" \
  -f values-fuse.yaml > rendered-sandbox.yaml
kubectl apply --dry-run=server -f rendered-sandbox.yaml
kubectl -n "$SANDBOX_NAMESPACE" apply --dry-run=server -f rendered-sandbox-preflight-policy.yaml
kubectl -n "$SANDBOX_NAMESPACE" apply --dry-run=server -f rendered-sandbox-preflight-pod.yaml

SERVICE_ACCOUNT_SUBJECT="system:serviceaccount:$CONTROL_NAMESPACE:$SERVICE_ACCOUNT"
for verb in create get list watch delete; do
  kubectl auth can-i "$verb" pods -n "$SANDBOX_NAMESPACE" \
    --as="$SERVICE_ACCOUNT_SUBJECT" --quiet
done
kubectl auth can-i create pods/exec -n "$SANDBOX_NAMESPACE" \
  --as="$SERVICE_ACCOUNT_SUBJECT" --quiet
for verb in create get update delete; do
  kubectl auth can-i "$verb" networkpolicies.networking.k8s.io \
    -n "$SANDBOX_NAMESPACE" --as="$SERVICE_ACCOUNT_SUBJECT" --quiet
done
if [[ "${NETWORK_POLICY_PROVIDER:-standard}" == "cilium" ]]; then
  for verb in create get update delete; do
    kubectl auth can-i "$verb" ciliumnetworkpolicies.cilium.io \
      -n "$SANDBOX_NAMESPACE" --as="$SERVICE_ACCOUNT_SUBJECT" --quiet
  done
fi

kubectl -n "$SANDBOX_NAMESPACE" get secret "$WORKSPACE_SECRET"
sandbox-runtime-preflight kubernetes run \
  --namespace "$SANDBOX_NAMESPACE" \
  --service-account-subject "$SERVICE_ACCOUNT_SUBJECT" \
  --workspace-secret "$WORKSPACE_SECRET" \
  --policy-manifest rendered-sandbox-preflight-policy.yaml \
  --pod-manifest rendered-sandbox-preflight-pod.yaml

helm upgrade --install sandbox deploy/helm/sandbox \
  --namespace "$CONTROL_NAMESPACE" \
  --create-namespace \
  -f values-fuse.yaml
```

服务端 dry-run 只验证资源 schema、原生 sidecar 字段和准入/PSA 是否接受，不能证明 Secret 存在、ServiceAccount RBAC 可用或 FUSE mount propagation 正常。`kubectl auth can-i` 与 Secret 检查必须全部成功；`false` 或 not found 都是发布阻断项。

`scripts/workspace-fuse-preflight.sh` 已提供环境、RBAC、Secret、镜像和设备的外层门禁；完整 runtime 生命周期由 `WORKSPACE_FUSE_RUN_INTEGRATION=1` 调用 Task 18 集成测试执行。任何用于预检的 manifest 字段必须与真实 sandbox 完全一致。runtime namespace 的 default-deny 是长期基础设施，由 Helm 单独渲染，不放进临时 workload 文件。

preflight 工具必须使用与 sandbox-api 相同的 Kubernetes 身份和 Redis owner/lease 状态机，完整执行 Pool WarmUp、Acquire、authorize、ready 流程：先创建精确 system egress allow policy 和 supervisor locked 的一次性空壳 Pod，确认 sandbox 主容器已启动且 Pod 因未挂载保持 NotReady，再 CAS 消费一次 mount authorization 并等待完整 Ready。预检 Pod 必须真实投射 Secret，并由 sandbox 内的固定 helper 验证 mount 类型、传播及远端创建/读取/删除。成功或失败后都必须在 finally/defer 中按“先删除 Pod 并确认退出、后删除 policy”清理；任一步失败或清理不完整均返回非零。外层脚本使用 `set -euo pipefail` 和 `kubectl auth can-i --quiet`，因此不会继续 Helm 发布。只做 dry-run、跳过 prepared 状态、直接 apply 一个无法授权的 locked Pod，或只检查 Pod Ready，都不构成 FUSE runtime 验证。

### 6.4 FUSE Pool 空壳 Pod 目标模板

以下片段描述 Pool WarmUp 必须生成的关键字段。`workspace-mounter` 是 Kubernetes 原生 sidecar init container；它先进入 prepared/locked，startup probe 成功后 sandbox 主容器随即启动。模板不含 workspace prefix、workspace identity 或 lease generation：

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    # Kubernetes 1.29 使用兼容 annotation；值由已校验的 provider LSM profile 生成。
    container.apparmor.security.beta.kubernetes.io/workspace-mounter: localhost/sandbox-fuse
  labels:
    sandbox.managed: "true"
    sandbox.pool: "true"
    sandbox.pool.state: preparing
    # 完整 PoolKey 对应 32-byte SHA-256 的 lowercase base32（无 padding，52 字符）；仅作选择器。
    sandbox.pool.key: "<pool-key-label>"
    sandbox.pool.instance: "<random-instance-id>"
    sandbox.workspace.mode: fuse
    sandbox.workspace.provider: minio
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  enableServiceLinks: false
  shareProcessNamespace: false
  dnsPolicy: None
  dnsConfig:
    nameservers: ["8.8.8.8", "1.1.1.1"]
  securityContext:
    seccompProfile:
      type: RuntimeDefault
  terminationGracePeriodSeconds: 90
  initContainers:
    - name: workspace-mounter
      image: registry.example.com/sandbox-s3fs-minio@sha256:<64-hex-digest>
      restartPolicy: Always
      securityContext:
        privileged: true
        runAsUser: 0
        readOnlyRootFilesystem: true
      resources:
        requests:
          cpu: 50m
          memory: 64Mi
          ephemeral-storage: 512Mi
        limits:
          cpu: "1"
          memory: 512Mi
          ephemeral-storage: 3Gi
      env:
        - name: SANDBOX_RUNTIME_UID
          valueFrom:
            fieldRef:
              fieldPath: metadata.uid
        - name: SANDBOX_MOUNTER_BOOTSTRAP
          value: '{"version":1,"provider":"minio","bucket":"sandbox","endpoint":"https://minio.example.com:9000","profile":"minio-sigv4-path-style-v1","access_key_file":"/run/secrets/workspace/accessKey","secret_key_file":"/run/secrets/workspace/secretKey","passwd_file":"/run/s3fs/passwd-s3fs","ca_file":"/run/secrets/workspace/ca.crt","cache_dir":"/var/cache/s3fs","cache_limit_bytes":2147483648,"mount_path":"/workspace","pool_key":"<pool-key-sha256>","mount_timeout_seconds":60,"flush_timeout_seconds":120,"unmount_timeout_seconds":30}'
      lifecycle:
        preStop:
          exec:
            command:
              - /usr/local/bin/workspace-mounter
              - shutdown
      volumeMounts:
        - name: workspace
          mountPath: /workspace
          mountPropagation: Bidirectional
        - name: fuse-cache
          mountPath: /var/cache/s3fs
        - name: dev-fuse
          mountPath: /dev/fuse
        - name: workspace-credentials
          mountPath: /run/secrets/workspace
          readOnly: true
        - name: mounter-run
          mountPath: /run/s3fs
      startupProbe:
        exec:
          command: ["/usr/local/bin/workspace-mounter", "health", "prepared"]
        periodSeconds: 2
        failureThreshold: 30
      readinessProbe:
        exec:
          command: ["/usr/local/bin/workspace-mounter", "health", "ready"]
        periodSeconds: 10
        failureThreshold: 3
  containers:
    - name: sandbox
      image: registry.example.com/sandbox@sha256:<64-hex-digest>
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: workspace
          mountPath: /workspace
          mountPropagation: HostToContainer
  volumes:
    - name: workspace
      emptyDir:
        medium: Memory
    - name: fuse-cache
      emptyDir:
        sizeLimit: 2Gi
    - name: dev-fuse
      hostPath:
        path: /dev/fuse
        type: CharDevice
    - name: workspace-credentials
      secret:
        secretName: sandbox-workspace-minio
        defaultMode: 0400
    - name: mounter-run
      emptyDir:
        medium: Memory
        sizeLimit: 16Mi
```

`SANDBOX_MOUNTER_BOOTSTRAP` 只包含 PoolKey 已覆盖的固定、非敏感配置，不包含 prefix、workspace identity 或 lease generation。bootstrap 携带完整的 64 位十六进制 PoolKey；`sandbox.pool.key` label 则把这 32 字节摘要编码为 lowercase base32（无 padding）的 52 字符值，以满足 Kubernetes label 长度约束。该 label 仅供 NetworkPolicy 和资源选择器使用，不得当作授权值。bootstrap 同时包含 `cache_limit_bytes` 以及 mount、flush、unmount 三个超时；Pod 的 `terminationGracePeriodSeconds` 取 `max(90, ceil(flush_timeout + unmount_timeout) + 15)`。`access_key_file`/`secret_key_file` 位于只读 Secret volume，`passwd_file` 必须位于 mounter 私有 `/run/s3fs`；sidecar 校验 AK/SK 为单行非空值后原子生成 mode `0600` 的 `AK:SK` 文件，不能假设 Secret 已提供 `passwd-s3fs`。sidecar 把 Downward API 提供的 Pod UID 与该 JSON 分别校验，并在 `/run/s3fs` 原子落成 mode `0600` 的 bootstrap 文件后才进入 prepared。Docker 特殊容器没有 Downward API：它先以 locked supervisor 启动，sandbox-api 从 `ContainerCreate` 返回值取得不可变 container ID，再通过一次性 `workspace-mounter bootstrap` 控制命令写入同一 schema；bootstrap 重放或 ID 不一致必须失败。

prepared health 必须报告 `cache_bytes=0`、`cache_limit_bytes` 与 PoolKey 固定配置一致且 `cache_exceeded=false`；这会阻止带残留缓存的空壳入池。ready health 每次统计 cache volume 中普通文件的逻辑字节数，达到或超过软阈值时把 supervisor 置为 unhealthy 并向 s3fs 发送终止信号。`sandbox-api` 的健康监督随后关闭 admission、销毁该一次性实例并补池。该机制是健康门禁，不是文件系统硬 quota，也不保证导致超限的写调用收到 ENOSPC。

目标最低版本包含 Kubernetes 1.29，因此 runtime 不使用 1.30 才稳定可用的结构化 `securityContext.appArmorProfile` 字段；mounter 的已校验、非 `unconfined` profile 通过兼容的 container AppArmor annotation 注入。升级最低版本前不得同时渲染两种形式，避免不同 API Server/准入插件产生不一致结果。

Pod 渲染前会把批准列表排序去重。DNS 端口集合必须精确为 `{53}`；对象存储端口、FQDN 与 CIDR 集合可以包含额外的运维批准项，但必须覆盖当前 endpoint。`cilium-fqdn` 的每个元素都必须是 canonical FQDN，不接受 IP literal 或通配符；重复、乱序输入按集合归一化，不能扩大 system egress。

空壳创建时只携带固定 provider 配置和 PoolKey，初始 label state 必须是 `preparing`，不能在 health 验证前标成 `prepared`。Pool availability 不能等待 Pod Ready：私有 `PreparedSandboxHealth` 验证 sidecar locked、sandbox 主容器 running、无 s3fs 和无 mount/generation；Pool/manager 另外从 Redis owner/session 反查确认该 runtime UID 未绑定 workspace，且公共 Exec/file gate 关闭。prepared 空壳不创建用户 session，不出现在公共 sandbox list/get 响应中，runtime ID/UID 也不能返回给调用方；公共 Exec/file API 必须校验已提交的用户 session 和 gate，不能仅凭 runtime ID 访问。全部通过后 Kubernetes 才以 resourceVersion 冲突保护把 state patch 为 `prepared` 并入队；Acquire 后 runtime 才根据请求生成唯一 prefix、租约标签和 workspace identity。对象 key 根路径严格为：

```text
<storage.filesystem.sub_path>/<workspace_path>/
```

启用 FUSE 时，`storage.filesystem.sub_path` 必须已经是空值或不带首尾 `/` 的 UTF-8 canonical 相对前缀：拒绝前导 `/`、`.`/`..` segment、NUL 和控制字符，且不能含重复 `/`；只保留原始 Unicode UTF-8 字节。校验器可以计算 candidate 做比较，但不得静默改写旧值。已有 sync 配置若为 `workspaces/`、`workspaces//team` 等非规范形式，必须保持 sync，或完成对象 key 显式迁移与校验后再改配置启用 FUSE。`workspace_path` 按设计文档的更严格规则规范化。

目录前缀必须由唯一共享的 `BuildWorkspacePrefix` 构造，带且仅带一个尾部 `/`；FUSE mount source、根目录标记、FUSE 模式 file API 和 Redis lease key 都复用结果，不得分别清理、再次追加 `sub_path` 或引入独立 `object_prefix`，也不得把路径作为 shell 字符串拼接进 s3fs 命令。legacy sync 模式继续使用现有 driver 路径语义，不受该校验静默重定向。

全新空 prefix 挂载前，控制面必须在获得独占租约后调用 `PrepareWorkspacePrefix`，对上述精确 prefix 写入 provider profile 已验证的零字节目录标记并用直接对象 API 验证。现有 `goairix/fs` v0.3.11 的 MinIO/OBS `MakeDir` 是 no-op，不能作为成功依据。根标记在 sandbox 销毁后保留，只随显式 workspace 删除流程清理；对应 profile 没有通过“全新空 prefix”测试时禁止启用 FUSE。

Redis FUSE Pool record 带不可复用 `PreparationID`、单调 `revision`、Redis 服务时间生成的 `PrepareUntil/ReservedUntil/CleanupUntil`；transition、cleanup claim 和 delete 都必须同时匹配预期 state、token 与 revision。cold prepare 先登记无 reservation 的 `preparing` 容量意图，runtime health 通过后才在受 refill lock fencing 的 Lua 中从当前 Redis 时间开始 reservation TTL 并直接 CAS 到 `reserved`，绝不能短暂发布成可被其他副本领取的 `prepared`。final publication 回复发生网络错误，或成功后立即观察到 Stop/cancellation 时，控制器先 claim 本次唯一可能的 exact after 版本，再 claim exact before 版本做补偿；若另一个 Acquire 已推进 revision/token，补偿 CAS 失败且绝不能删它的 runtime。cold reservation 完成该 post-check 前不能返回调用方。任何 runtime 删除前必须先取得 `cleanup` claim；旧 prepared 快照与 Acquire 并发时只能 CAS 失败，不能先删除新 reserved runtime。runtime identity 在 intent-only cleanup claim 之后才返回时，相同 cleanup token 在一个 Lua 中补全 record、record UID 映射和全局 RuntimeUID owner 索引；失败的 runtime 删除留下可读取、可接管的 cleanup tombstone。生产接口不提供从 live state 直接物理删 record 的捷径。SessionStore 使用独立 `sandbox:session:v2:` 前缀；lease、owner、generation 和 pool key 均使用各自命名空间，恢复扫描不能把它们当作 session JSON。

mounter 必须预创建 `/var/cache/s3fs/tmp`（以及启用文件 cache 时的 `/var/cache/s3fs/cache`），固定 argv 至少包含 `tmpdir=/var/cache/s3fs/tmp`；不得使用默认 `/tmp`。启用 `use_cache` 时必须指向 `/var/cache/s3fs/cache`，确保 `emptyDir.sizeLimit` 和 ephemeral-storage 监控覆盖 s3fs 的全部本地数据。

workspace-mounter 不配置 liveness probe。可信 supervisor 作为 PID 1 在 Pool 中保持 locked。Acquire 原子保留同 PoolKey 空壳并取得 Pod UID，确认 sidecar `restartCount=0`、无 mount/generation，再核对持久 owner 和有效 lease，将 owner 的 `mount_attempt` 从 0 CAS 为 1；只有 CAS 成功才能通过固定控制命令授权它启动唯一 s3fs 子进程。授权前后失败或重启都必须删除实例并用新 generation 重建，不能回滚 `mount_attempt` 或重放授权。子进程退出或 mount 消失后，supervisor 保持运行、置 unhealthy，禁止自行 remount。租约从 Acquire 成功起立即续期，不等到对外发布；发布后由 sandbox-api 的 lifecycle watcher 持续核对 lease、Pod UID、generation、restartCount 和 FUSE health，任一异常都原子关闭引用计数 gate、拒绝新操作并销毁该 single-use runtime。

授权后 mounter readiness 成功还不够；runtime 必须在 sandbox 主容器中以 UID/GID 1000 执行固定、不可由用户传入 argv 的读写探测。探测对象 basename 由共享 `fuseprotocol.DeriveProbeObjectName(RuntimeUID, generation)` 以版本化、域分隔 SHA-256 稳定推导，不能包含原始 UID，也不能由请求指定；探测确认传播后的 `/workspace` 可创建、读取和删除该 exact 对象。随后更新 pool state 为 consumed、持久化 sandbox session 并打开 Exec/file gate。只有授权 CAS 前失败且完整 pristine probe 通过的 reserved 空壳可以回到同一 PoolKey；授权后的实例永不回池。

后续清理契约：公共 FUSE file API 必须隐藏或拒绝 `IsReservedProbeObjectName` 匹配的单一保留模式。Manager 在取得 Pod/container 的 exact termination evidence 后，使用注入的 `WorkspaceObjectClient` 和 canonical workspace prefix 删除并 HEAD/Stat 验证 exact derived probe key；禁止使用通配、前缀扫描或批量删除任何“看起来像 probe”的对象。失败时保留 cleanup tombstone 并由恢复流程重试，不能当作销毁完成。

`mounter-run` 是 memory-backed emptyDir，其中的 `mount-generation` 只是本地第二道防线：获得授权后以 create-if-absent 原子写入；容器重启若 marker 仍在，写入 `restart-detected`、不再调用 s3fs，并让 readiness 持续失败。节点重启可能丢失该卷，因此 runtime 永不对同一 Pod UID/lease generation 做第二次授权；marker 丢失时 supervisor 仍保持 locked。正常销毁必须先收到 supervisor 的成功 flush/unmount 证明，再对精确 Pod UID 执行非 force 的 graceful delete 并观察到 NotFound。若节点或控制通道不可用，只能由配置的基础设施 fencer 返回匹配 Pod UID/节点的证明；没有证明时保留 owner/lease 与策略并标记 blocked。仅 force-delete API 对象、节点 NotReady 或状态无法确认时禁止接管。runtime 的 termination evidence 缓存只用于同一 sandbox-api 进程内完成“两阶段删除策略后再确认”；进程重启后不能把缺失缓存当成已退出，必须重新通过精确 graceful termination 取证或配置的 infrastructure fencer 恢复，否则继续 fail closed 并保留 owner/lease 与策略。

### 6.5 RBAC

控制面需要在 runtime namespace 内管理：

- Pod 的 create/get/list/watch/delete/patch；
- `pods/exec` 的 create；
- sandbox 级 NetworkPolicy 的 create/get/update/delete；
- 使用 Cilium 时，对 CiliumNetworkPolicy 的 create/get/update/delete。

Secret 通过已知名称挂载时不授予读取权限。若 control namespace 与 runtime namespace 不同，在 runtime namespace 创建 Role 和 RoleBinding，RoleBinding 的 subject 指向 control namespace 中的 sandbox-api ServiceAccount。

预检必须包含 `kubectl auth can-i patch pods -n "$SANDBOX_NAMESPACE" --as system:serviceaccount:<control-ns>:<sandbox-api-sa>`，否则不能启用 FUSE。

### 6.6 网络策略

实现中的 `SystemEgressSpec` 必须显式选择 `cidr` 或 `cilium-fqdn`：前者只允许运维提供的 DNS/endpoint CIDR 与精确端口，后者只允许无通配符的 endpoint FQDN。DNS 固定 TCP/UDP 53，对象端口固定为配置值；一期不支持 `ProxyURL`，出现代理字段时配置校验失败，不能静默扩大出口。

现有 Chart 的静态 NetworkPolicy 只提供 default-deny 基线。FUSE endpoint 的 system egress 必须由 runtime 为每个 prepared 空壳生成：

- runtime namespace 的 default-deny 必须作为独立部署前置项存在，不能依赖 control namespace 中的 Chart policy；
- provider endpoint 必须先进入平台维护的精确白名单，配置文件或创建请求不能自动批准新的内网目的地；
- Pool WarmUp 先创建并确认 instance 专属 system allow policy，再创建带相同唯一 selector 的 Pod；selector 只使用不可变 `sandbox.pool.instance`，不能依赖可变 state label。Acquire 后、开放 Exec 前再创建用户网络 policy，销毁顺序相反；
- system policy 在 Pod 创建后必须绑定不可变 Pod UID，用户 policy 也必须携带相同 UID；更新与删除同时核对 instance、role 和 UID。同名 Pod 被替换时，旧 runtime 的退出证明和策略清理不得作用于新 Pod 或新策略；
- Pod 保持 `DNSPolicy=None`，DNS egress 只允许运维配置的公共 nameserver；不允许 CoreDNS，也不配置集群 search domain；
- 用户网络启用时，独立 user policy 额外只向该 Pod `DNSConfig` 中经校验的公共 nameserver 精确主机地址（IPv4 `/32`、IPv6 `/128`）开放 TCP/UDP 53；禁用时不加入用户 DNS 规则。用户域名白名单仍由控制面当次解析为已审批 CIDR，不使用 user `toFQDNs` 动态放行；DNS 重解析得到新地址后必须重新调用网络更新并重新审批策略；
- Cilium 集群的 `block_private` 不能只依赖标准 NetworkPolicy 的 `0/0 + Except`；runtime 还为 exact instance/Pod UID 创建独立 user-deny CiliumNetworkPolicy。deny 必须先于 user allow 生效，收紧/禁用时先确认 allow，再删除不再需要的 deny。私网 deny 的 exception 仅来自平台 system policy 中持久的已审批 endpoint CIDR（Cilium FQDN 模式只作为 deny exception，不新增 CIDR allow）和用户显式批准的私网白名单；元数据、link-local、loopback、multicast 与 unspecified 范围永久禁止，不能作为 exception；
- 无论 `network_enabled` 是否为 false，都允许 sidecar 访问该 workspace 固定 provider endpoint；
- sandbox 主容器不因此获得任意公网访问；
- endpoint 必须是公共 nameserver 可解析的稳定专用 FQDN，或由运维通过 `hostAliases` 映射的证书匹配 FQDN；只有证书包含 IP SAN 时才允许直接使用稳定 IP。不支持 `cluster.local` Service，仅加入网络白名单不能解决集群域名解析；
- Cilium 环境可使用 FQDN policy；仅使用标准 NetworkPolicy 时，使用运维维护的稳定 CIDR或显式 endpoint IP，不把短期 DNS 解析结果永久写死；
- 禁止访问云元数据地址和不必要的 RFC1918 网段。

NetworkPolicy 不能按容器区分流量；若必须严格保证主容器无法直连对象存储，应使用 sidecar 独立网络身份、CNI 扩展或 egress proxy，而不是仅依赖同 Pod 的标准 NetworkPolicy。

当前确认的边界是“sandbox 只能访问平台审批的白名单地址”，因此对象存储 endpoint 加入 system egress 后，用户进程也能连接该地址，但没有凭证。如果安全目标升级为“用户进程不能连接，即使地址已白名单”，必须使用认证 egress proxy 或容器级网络身份后才能启用 FUSE。

### 6.7 Kubernetes 验证

Pool WarmUp 后、Acquire 前先验证空壳：

```bash
kubectl -n "$SANDBOX_NAMESPACE" get pod "$PREPARED_POD" \
  -o jsonpath='{.status.containerStatuses[?(@.name=="sandbox")].state.running}{"\n"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
kubectl -n "$SANDBOX_NAMESPACE" exec "$PREPARED_POD" -c workspace-mounter -- \
  /usr/local/bin/workspace-mounter health prepared
kubectl -n "$SANDBOX_NAMESPACE" exec "$PREPARED_POD" -c sandbox -- \
  sh -c 'test ! -w /workspace'
```

预期 sandbox 主容器已经 running、Pod Ready 为 False、supervisor 为 prepared，且 UID 1000 不能写底层 `/workspace`。此阶段通过公共 API 发起的 Exec/file 操作必须被 gate 拒绝。

使用该空壳 Acquire 测试 workspace 后执行：

```bash
kubectl -n "$SANDBOX_NAMESPACE" get pod "$SANDBOX_POD" -o wide
kubectl -n "$SANDBOX_NAMESPACE" get pod "$SANDBOX_POD" \
  -o jsonpath='{.status.initContainerStatuses[*].name}{"\n"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
kubectl -n "$SANDBOX_NAMESPACE" exec "$SANDBOX_POD" -c sandbox -- \
  findmnt -T /workspace
kubectl -n "$SANDBOX_NAMESPACE" exec "$SANDBOX_POD" -c sandbox -- \
  getent ahosts minio-fuse.example.com
kubectl -n "$SANDBOX_NAMESPACE" exec "$SANDBOX_POD" -c sandbox -- \
  sh -c 'id && p=/workspace/.deploy-probe-$(date +%s); printf ok > "$p"; test "$(cat "$p")" = ok; rm "$p"'
```

验收时还要验证：

- sandbox 容器内不存在 `/dev/fuse` 和 Secret mount；
- Pool hit 的 runtime UID/Pod UID 在 Acquire 前后保持不变，证明没有为 workspace 新建 Pod；
- prepared Pod NotReady 是预期状态，不能被通用 Ready watcher 当作 Pool 创建失败；
- Pod 的 nameserver 为配置的公共 DNS，`cluster.local` 不可解析，目标 endpoint 可以解析且只有 system egress 允许连接；
- Pod 未 Ready 前 API 不返回创建成功；
- 删除 s3fs 进程后 sandbox 被标记 error 并按策略重建，不写到底层 emptyDir；
- 删除 Pod 后节点没有遗留对应 kubelet volume 的 FUSE mount；
- `kubectl get pod -o yaml` 和日志不包含 AK/SK；
- MinIO/OBS 上仅出现该 workspace prefix 下的对象。

## 7. Docker 部署

### 7.1 宿主机与 Docker daemon

除第 4.1 节外，确认 Docker daemon 可映射字符设备：

```bash
docker info
docker run --rm --device /dev/fuse:/dev/fuse alpine:3.22 \
  sh -c 'test -c /dev/fuse'
```

生产启用 AppArmor、SELinux 或等价 LSM 时，为特殊镜像提供经过审计的 profile。不要全局关闭 LSM；只允许受信任 supervisor 完成 mount/umount，用户进程仍不可获得 `SYS_ADMIN`。

### 7.2 Compose 控制面配置

Compose wiring 已落地，但当前所有 FUSE profile 仍会被 release gate fail closed，不能作为可上线配置直接使用。

Compose 需要把 Secret 暂存目录以同一绝对路径挂载给 API，以便 API 创建动态容器时使用宿主机可解析的 source path：

```yaml
services:
  sandbox-api:
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ${WORKSPACE_CREDENTIAL_DIR}:/run/secrets/workspace:ro
      - /var/lib/sandbox/workspace-secrets:/var/lib/sandbox/workspace-secrets:rw
```

仓库 Compose 文件已显式映射 MinIO provider 的环境变量；OBS 或生产多 profile 部署推荐改用只读配置文件，避免在 Compose 中复制整套 map key。配置示例：

```yaml
runtime:
  type: docker
  docker:
    host: unix:///var/run/docker.sock

storage:
  filesystem:
    provider: minio
    bucket: sandbox-storage
    region: us-east-1
    endpoint: minio.example.internal:9000
    sub_path: workspaces
    use_ssl: true
    credential_files:
      access_key_file: /run/secrets/workspace/accessKey
      secret_key_file: /run/secrets/workspace/secretKey
    ca_file: /run/secrets/workspace/ca.crt

workspace:
  mode: fuse
  quota_mode: soft
  auto_sync_interval_seconds: 0
  secret_name: sandbox-workspace-minio
  cache_medium: disk
  cache_size: 2Gi
  mount_timeout_seconds: 60
  flush_timeout_seconds: 30
  unmount_timeout_seconds: 20
  recreate_max_attempts: 1
  lease_ttl_seconds: 90
  lease_renew_interval_seconds: 30
  fuse_pool:
    min_size: 3
    max_size: 20
    refill_interval_seconds: 10
    prepare_timeout_seconds: 120
  mounter_resources:
    cpu_request: 50m
    cpu_limit: "1"
    memory_request: 64Mi
    memory_limit: 512Mi
    ephemeral_storage_request: 512Mi
    ephemeral_storage_limit: 3Gi
  providers:
    minio:
      driver: s3fs
      profile: minio-sigv4-path-style-v1
      storage_identity: minio-prod-primary
      credential_generation: "2026-09-03-01"
      endpoint_host_ips: [192.0.2.10]
      mounter_image: registry.example.com/sandbox-s3fs-minio@sha256:<64-hex-digest>
      docker_image: registry.example.com/sandbox-fuse-minio@sha256:<64-hex-digest>
      lsm_profile: sandbox-fuse
      system_egress_mode: cidr
      dns_cidrs: ["8.8.8.8/32", "1.1.1.1/32"]
      endpoint_ports: [9000]
      proxy_url: ""
      system_egress_fqdns: [minio.example.internal]
      system_egress_cidrs: [192.0.2.10/32]
    obs:
      driver: s3fs
      # 当前仍为 unverified，以下配置会被 fail-closed 校验拒绝；
      # Task 18 完成目标私有云实测并显式提升状态后方可启用。
      profile: huawei-obs-private-2023-v1
      storage_identity: huawei-obs-private-primary
      credential_generation: "2026-09-03-01"
      endpoint_host_ips: [192.0.2.20]
      mounter_image: registry.example.com/sandbox-s3fs-obs@sha256:<64-hex-digest>
      docker_image: registry.example.com/sandbox-fuse-obs@sha256:<64-hex-digest>
      lsm_profile: sandbox-fuse
      system_egress_mode: cidr
      dns_cidrs: ["8.8.8.8/32", "1.1.1.1/32"]
      endpoint_ports: [443]
      proxy_url: ""
      system_egress_fqdns: [obs.private.example.com]
      system_egress_cidrs: [192.0.2.20/32]
```

对象存储的 provider、bucket、endpoint、region、sub path 和 TLS 开关继续使用 `storage.filesystem` 配置。MinIO endpoint 使用 `host[:port]`，runtime 根据 `use_ssl` 派生 s3fs URL；OBS endpoint 使用华为 SDK 要求的格式。AK/SK 不出现在该文件或环境变量；Compose 把 `${WORKSPACE_CREDENTIAL_DIR}` 只读挂到 `/run/secrets/workspace`，sandbox-api 读取后在 `/var/lib/sandbox/workspace-secrets` 中为动态容器生成 root-only 文件。私有 CA 同样从该只读目录读取并复制给特殊容器，同时配置到控制面原生存储客户端；CA 或凭证轮换都需要排空并重建相关 sandbox。

上面的 Compose 片段按私有 CA 场景给出；使用系统公共 CA 时删除 `storage_ca` Secret 和 `ca_file` 配置，不能保留指向不存在文件的路径。

示例中的 `192.0.2.0/24` 属于文档保留地址，部署时必须替换为平台批准的真实稳定 IP/CIDR。`endpoint_host_ips` 只负责把当前 provider endpoint 的 FQDN 写入 `extra_hosts`，TLS 仍校验 FQDN；其 IP 必须同时存在于 `system_egress_cidrs` 精确白名单中。

### 7.3 特殊镜像要求

Docker FUSE 镜像不是简单把 s3fs 安装进现有 sandbox 镜像。它必须包含经审计的 root supervisor，并满足：

1. 镜像构建阶段把 `/workspace` 底层 anchor 固定为 `root:root`、mode `0555`；容器启动时只验证，不能在 `readOnlyRootfs` 上执行必然失败的 chmod/chown。
2. Pool WarmUp 时 supervisor 先保持 locked；sandbox-api 取得不可变 container ID 后通过一次性 `workspace-mounter bootstrap` 注入固定 provider 配置和该 ID。supervisor 校验后以 root 读取 mode `0400/0600` provider Secret、生成 s3fs 密码文件，然后进入 prepared/locked；bootstrap 重放或 ID 不一致失败，此时用户环境基础进程已经启动，但不能启动 s3fs。
3. Acquire 后 supervisor 只接受一次包含相同 PoolKey、runtime ID、prefix 和 lease generation 的固定授权，随后启动前台 FUSE 进程；mount health 与 UID 1000 sandbox 内读写探测都通过后才允许 API 执行用户命令。
4. 所有用户命令和文件命令强制 UID/GID 1000，不能调用 root supervisor 的控制接口。
5. FUSE 进程异常退出时容器进入 unhealthy/error 并停止整个 sandbox；不能把取消 attach/exec 流当作用户进程退出，也不能把用户流量切到底层目录。
6. flush/销毁时 sandbox-api 先关闭引用计数 operation gate 并等待所有 API stream 结束，再在 sandbox 容器内运行固定 `workspace-probe quiesce`：停止同 UID 的其余进程并验证没有指向 `/workspace` 的可写 fd。只有得到绑定 exact RuntimeUID/generation 的一次性 quiesce token 才执行经过 profile 验证的 flush；非销毁 flush 后必须用 `workspace-probe resume` 消费同一 token，重放/跨 generation 失败并保持 gate 关闭。无法证明静止时返回 `flushed=false`。删除后还必须由可达 Docker daemon 对精确 container ID 返回 NotFound，host 失联时保留 owner/resources 并阻止接管，不能把 force remove 请求本身当成退出证明。

镜像入口点必须使用 exec 形式，s3fs 参数必须由参数数组构造。禁止在命令行包含明文 AK/SK，也不接受 API 调用方传入任意 `-o` 参数。

### 7.4 动态容器 HostConfig

Docker runtime 创建特殊容器时至少设置：

```yaml
devices:
  - pathOnHost: /dev/fuse
    pathInContainer: /dev/fuse
    cgroupPermissions: rwm
capDrop:
  - ALL
capAdd:
  - SYS_ADMIN
  - NET_ADMIN
securityOpt:
  - no-new-privileges=true
  - apparmor=sandbox-fuse
readOnlyRootfs: true
```

并挂载：

- 每 sandbox root-only Secret 目录到 `/run/secrets/workspace:ro`；
- 独立 cache volume/目录到 `/var/cache/s3fs`，固定 profile 至少设置 `tmpdir=/var/cache/s3fs/tmp`；启用文件 cache 时同时设置 `use_cache=/var/cache/s3fs/cache`，由 supervisor 上报总使用量并按 `cache_size` 软阈值触发终止重建；
- 独立 tmpfs 到 `/run/s3fs`（root:root、mode `0700`、当前固定 16 MiB）和受 `tmp_disk` 限制的 `/tmp`；不得给整个 `/run` 叠加 tmpfs，否则会遮蔽特殊镜像中预置的可信 `/run` 契约。s3fs 不得把临时/缓存数据写入不受 cache 阈值约束的 `/tmp`；
- 不挂载宿主机业务 `/workspace`。

`CAP_SYS_ADMIN` 和 `CAP_NET_ADMIN` 只属于容器内 root supervisor：前者用于 FUSE mount，后者只用于 runtime 通过固定绝对路径 `/usr/sbin/ip route replace default via <gateway-ip>` 设置 sidecar gateway 默认路由。禁止使用 Docker exec `Privileged=true`，也不得将这两个 capability 传递给 UID 1000 用户进程。runtime 必须覆盖所有 Exec、文件 API 和间接命令路径，强制 `User=1000:1000`。容器镜像内 `/workspace` 底层目录必须不可由 UID 1000 写入，以便 mount 消失时 fail closed。

Docker 特殊镜像的 root PID 1 还必须提供版本化的本地 child-reaper 握手：校验 Unix peer PID/UID，并只对已登记的 UID 1000 broker PID/starttime 执行精确 `wait4(pid)`。仅观察 `SIGCHLD` disposition 不足以放行 quiesce；握手缺失或身份不匹配时必须 fail closed。

普通 Docker named volume 不提供可移植的硬 quota。prepared 空壳的 cache 必须为空且无 mount generation。达到 cache 软阈值时，runtime 必须阻止新 Exec 并停止整个容器，执行有界、尽力 flush 后删除并补充新空壳；不承诺用户写操作先收到 ENOSPC。cache 不得落在无界 container writable layer，销毁后必须清理。

部分 Docker/LSM 组合需要额外 AppArmor FUSE mount 规则；SELinux 节点使用等价的专用 type。profile 名进入 PoolKey，Compose/Helm 与 runtime HostConfig 必须显式注入，预检同时做允许 mount/umount 的正向测试和用户进程调用 mount 的负向测试。若当前节点只有 `apparmor=unconfined`/`label=disable` 才能运行，应停止上线并补充最小 profile，不能把 unconfined 作为生产默认值。

### 7.5 system egress

FUSE Pool 空壳从 WarmUp 起就加入只允许以下目的地的系统网络：

- DNS resolver；
- 当前 provider endpoint；
- 必需的证书状态或内部 PKI 服务。

用户网络关闭时仍保留该系统网络；上述地址必须预先进入平台审批的精确 system egress 白名单，配置 provider endpoint 不得自动批准任意内网地址。Acquire 后、开放 Exec 前再把用户网络规则追加到 gateway。用户进程的网络请求必须经过 gateway 策略，不能因为与 supervisor 同容器而获得不受限出口。对象存储 endpoint 和用户白名单分别建模；当前边界允许用户进程连接已批准的 endpoint，但其不能获得凭证。若要求按进程阻止该连接，必须增加认证 egress proxy，单容器 Docker network 本身无法实现。

私有云 endpoint FQDN 无法由公共 DNS 解析时，Docker runtime 可从只读运维配置生成精确 `extra_hosts` 映射，仍以原 FQDN 发起 TLS 请求；不得由 CreateSandbox 参数控制映射，也不得因为解析困难改用跳过证书校验。

Docker 一期网络只支持 IPv4 CIDR，并在创建 sandbox-facing internal bridge 时显式关闭 IPv6。gateway 使用固定 `SBOX_PERMANENT`、`SBOX_SYSTEM`、`SBOX_USER` 三条链：先安装主链跳转和不可变 system/permanent 规则，再启动 runtime；Acquire 只通过单次 `iptables-restore --noflush` 原子替换 `SBOX_USER`，不得 flush `FORWARD` 或重写前两条链。DNS 只接受 1–3 个公共 IPv4 `/32` resolver，并仅开放 TCP/UDP 53；对象 endpoint 仅开放 TCP 且端口集合必须覆盖规范化 endpoint 的实际端口。`0.0.0.0/8`、`127.0.0.0/8`、`169.254.0.0/16`、`224.0.0.0/4` 永久先于 system/user 规则拒绝，用户白名单或 open 模式不能覆盖。所有 CIDR、端口和静态 host 映射在生成规则前排序去重；Docker 收到任何 IPv6 endpoint、DNS、静态映射或用户白名单输入时必须 fail closed。

### 7.6 Docker 验证

Pool WarmUp 后先检查 prepared 容器：

```bash
docker inspect "$PREPARED_CONTAINER" \
  --format '{{.State.Running}} {{.State.Health.Status}} {{json .Config.Labels}}'
docker exec -u 0:0 "$PREPARED_CONTAINER" \
  /usr/local/bin/workspace-mounter health prepared
docker exec -u 1000:1000 "$PREPARED_CONTAINER" sh -c 'test ! -w /workspace'
```

预期容器和 supervisor 已运行、prepared health 通过、没有 s3fs/mount generation，且公共 API 不能对它执行用户命令。Acquire 测试 workspace 后执行：

sandbox-api 重启不等于 Docker 容器退出。Docker runtime 构造函数禁止清理 managed gateway/network/container；Manager 必须先从 Redis session、owner 与 Pool record 恢复受保护的 runtime UID 集合并做健康校验，再调用 orphan reconciliation。只有不在保护集合中的资源才能删除；仍存活的 FUSE container 与 gateway 必须按原 generation 恢复，不能重新 bootstrap 或 authorize。

```bash
docker inspect "$SANDBOX_CONTAINER" \
  --format '{{json .HostConfig.Devices}} {{json .HostConfig.CapAdd}} {{json .Config.Env}}'
docker exec -u 1000:1000 "$SANDBOX_CONTAINER" findmnt -T /workspace
docker exec -u 1000:1000 "$SANDBOX_CONTAINER" \
  sh -c 'id && p=/workspace/.deploy-probe-$(date +%s); printf ok > "$p"; test "$(cat "$p")" = ok; rm "$p"'
docker exec -u 1000:1000 "$SANDBOX_CONTAINER" \
  sh -c 'test ! -r /run/secrets/workspace/credentials'
```

同时检查：

- `docker inspect` 的 Env、Cmd、Entrypoint 和 Labels 不包含 AK/SK；
- Pool hit 的 container ID 在 Acquire 前后保持不变；授权后该 ID 不再进入 available 队列，销毁后由新 container ID 补池；
- 普通用户不能执行 mount/umount、读取 Secret、向 supervisor 发控制命令或取得 root；
- `1000:1000` 强制只作用于 FUSE 特殊容器；普通 sync/legacy Docker 容器继续继承其镜像用户。请求取消会关闭对应 Docker attach 以释放 API 调用，但 attach 关闭本身不作为用户进程或容器已经退出的证明；
- 宿主机没有 sandbox 对应的 `/workspace` mount；
- FUSE 进程退出后 health 失败，用户写入不会落到镜像目录；
- API/daemon 重启后 reconciler 能依据 runtime identity 清理旧容器、Secret 和租约。

### 7.7 API 流式上传

非上传请求继续使用 64 MiB body 上限。只有精确的 direct file-upload 路由使用 `security.max_upload_bytes`（默认 2 GiB）；大于 64 MiB 的请求必须携带 `X-Sandbox-File-Size`，handler 用 `MultipartReader` 直接把 file part 流向 runtime，并校验声明长度、短读、超量与配置上限。Kubernetes/Docker runtime 在已知 size 后先写 tar header 再 `io.CopyN`，禁止 `ParseMultipartForm`、`FormFile` 或 `io.ReadAll` 把 1 GiB 请求落入内存/临时文件。其他 JSON、exec 和 multipart chunk 路由不得继承这个放宽值。

Go SDK 对应使用 `UploadFileSized(ctx, sandboxID, remotePath, size, reader)`；它把精确路径放在 query、大小放在 `X-Sandbox-File-Size`，multipart 中只流式发送单个 file part。普通小文件继续使用 `UploadFile`，保持兼容。

### 7.8 本地 Docker、kind 与本地镜像仓库验证

本地双 runtime 可以共用同一套 release profile 报告，但必须分别执行完整测试。Docker 使用自带 supervisor/s3fs 的特殊 sandbox 容器；kind 使用普通 sandbox 容器加 mounter sidecar。两者都不得把宿主机业务目录挂到 `/workspace`：Docker 的 `/workspace` 位于特殊容器内，Kubernetes 的 `/workspace` 位于 Pod 私有 `emptyDir`；kind 节点内出现的 kubelet Pod mount 不是业务 hostPath。

先确认实际 Docker context、kind 集群名、节点 FUSE 设备和镜像仓库：

```bash
docker context show
docker info
kind get clusters
kubectl config current-context
kind get nodes --name "$KIND_CLUSTER_NAME"
for node in $(kind get nodes --name "$KIND_CLUSTER_NAME"); do
  docker exec "$node" test -c /dev/fuse
done
```

本地仓库只解决镜像分发，不是对象存储 endpoint。mounter、普通 sandbox 和 Docker 特殊 sandbox 镜像必须先推送到该仓库，再以仓库返回的真实 `@sha256:` digest 写入 profile；禁止用本地 image ID 或 tag 代替 registry digest。kind 必须能够从节点 containerd 拉取该 digest，或者在仅供开发的验证中用 `kind load docker-image` 导入同一内容并确认 Pod 实际 image ID。MinIO/OBS endpoint、DNS、CA 和 egress 白名单仍独立配置。

本机执行结构与设备检查的典型环境如下；Secret 名、namespace、digest 和 staging 路径必须换成实际值：

```bash
export KIND_CLUSTER_NAME=desktop
export WORKSPACE_RUNTIME_NAMESPACE=sandbox-runtime
export WORKSPACE_RUNTIME_SECRET=workspace-minio
export FUSE_LSM_PROFILE=sandbox-fuse
export WORKSPACE_SECRET_STAGING_ROOT=/var/lib/sandbox/workspace-secrets

scripts/workspace-fuse-preflight.sh kubernetes \
  --profile testdata/fuse/profiles/minio-sigv4-path-style-v1.yaml
scripts/workspace-fuse-preflight.sh docker \
  --profile testdata/fuse/profiles/minio-sigv4-path-style-v1.yaml
```

preflight 会验证 digest、镜像自检、RBAC、Secret、`/dev/fuse`、非 unconfined LSM、无宿主机 `/workspace` bind 和现有 FUSE workload。完整测试还需提供 sandbox-api URL/API key、真实对象存储凭据和 fault driver。MinIO 本地替身也必须启用证书与主机名校验，因为 release profile 拒绝 HTTP 和跳过 TLS 校验。对象存储若位于内网，只对其精确 FQDN/CIDR、端口以及必要 DNS 开 system egress 白名单，不开放通用内网访问。

## 8. Provider profile

### 8.1 MinIO mount 参数基线（尚未通过 durable-flush gate）

`minio-sigv4-path-style-v1` 的 mount parameters 已标记为 `verified`，至少固定：

- 显式 HTTPS endpoint；
- `use_path_request_style`；
- SigV4；
- region，默认不依赖 AWS endpoint 推导；
- `allow_other`、`uid=1000`、`gid=1000`、`umask=0022`、`mp_umask=0022`；
- 前台运行、有界 cache 和 multipart 参数；
- TLS 主机名与 CA 校验。
- provider 级静态长期 AK/SK，写入 root-only s3fs `passwd_file`；出现 session token 时拒绝启动。

但其 durable flush 当前仍为 `blocked-pending-flush-spike`，所以这不是可上线结论。配置校验和 `release-check` 必须继续拒绝该 profile，直至 Task 18 证明 exact profile 的写入静止、flush、卸载和终止语义并显式提升状态。

### 8.2 华为 OBS 公有云候选与 2023 私有云未验证基线

[华为云公有云 CCE OBS 挂载参数文档](https://support.huaweicloud.com/intl/zh-cn/usermanual-cce/cce_10_0631.html)与本项目使用的[双华云私有云 CCE OBS 挂载参数文档](https://docs.shuanghuayun.com/zh-cn/usermanual/cce/cce_10_0631.html)均明确规定：普通对象桶使用 s3fs，并行文件系统使用 obsfs。私有云文档还显示普通对象桶自动使用 `sigv2`，s3fs 1.92 会自动添加 `compat_dir`。一期只接入普通对象桶，因此 OBS profile 的客户端固定为 s3fs，不使用 obsfs。

该文档描述的是 Everest 集成路径，不等于任意自建 sidecar/Docker 镜像已兼容。并且目标私有云部署于 2023 年，在线文档的当前内容不能证明现网组件版本。上线前须向平台侧或厂商确认并留档实际 OBS 服务版本/补丁、CCE Everest 插件版本及集成路径客户端版本；无法取得服务端版本时，至少保存 endpoint、桶类型、Everest 版本和全套兼容性测试证据。

当前 `huawei-obs-public-v1` 只把官方公有云资料中的 `url`、`endpoint` 与 SigV2 形状记录为 `candidate`，addressing style 尚未验证；`huawei-obs-private-2023-v1` 的 mount parameters 为 `unverified`。两者 durable flush 都是 `blocked-pending-flush-spike`，不能通过配置或 release gate。

OBS profile 必须由目标区域、目标普通对象桶和最终镜像完成 provider spike 后冻结。至少验证：

- 上游 s3fs 与厂商兼容构建分别测试；
- `sigv2` 是否需要；
- region 和 endpoint 组合；
- `compat_dir`/`support_compat_dir` 目录对象语义；
- path-style 或 virtual-host-style；
- `big_writes`、multipart、零字节对象、rename 和大量小文件；
- 私有 CA、TLS SNI 和主机名校验。
- provider 级静态长期 AK/SK 与目标 s3fs 构建的 `passwd_file` 认证。
- 待挂载资源确认为普通对象桶；如果识别为并行文件系统，配置校验失败并转入后续 obsfs 方案评审。

所有测试必须保留完整 TLS 与主机名校验；Everest 示例中的 `no_check_certificate` 和 `ssl_verify_hostname=0` 不得进入 compiled profile、manifest、Helm/Compose 配置或临时发布参数。

华为公有云与 2023 私有云必须产出不同的 profile ID、镜像 digest 和验证报告，例如 `huawei-obs-public-<region>-v1` 与 `huawei-obs-private-2023-<site>-v1`。即使实测参数暂时相同，也不能把其中一方的兼容性结论直接复用到另一方。

未完成 spike 前，不得把 `storage.filesystem.provider` 切换为 `obs`，也不得发布可选中的 OBS profile/image。验证结论要落为版本化 profile，而不是在生产临时追加参数。

### 8.3 六组合矩阵与证据冻结

`scripts/workspace-fuse-matrix.sh` 固定执行三个 profile × 两个 runtime。每个启用的 profile 都必须同时通过功能测试和 fault matrix；fault driver 缺失、任一 cleanup 未确认、证据摘要不完整、digest 不一致都会失败。当前仓库中的三个报告均为 `enabled: false`，因此默认执行矩阵会以非零退出并阻止发布。仅查看候选阻塞清单时可显式运行：

```bash
ALLOW_BLOCKED_FUSE_PROFILES=1 scripts/workspace-fuse-matrix.sh
```

该变量只用于开发环境盘点，发布流水线不得设置。发布流水线必须让默认命令通过，并观察到 `enabled=3 blocked=0 combinations=6`。

集成测试需要：

```bash
export WORKSPACE_FUSE_API_URL=https://sandbox-api.example.com
export WORKSPACE_FUSE_API_KEY_FILE=/run/secrets/sandbox-api-key
export WORKSPACE_FUSE_CHAOS_DRIVER=/opt/workspace-fuse/fault-driver
```

执行器应从受限文件读取 API key 后仅注入测试进程的 `WORKSPACE_FUSE_API_KEY`，不得把值写入命令行、profile、证据或日志。fault driver 接收 `<runtime> <profile> <scenario>`，成功时只输出 `{"passed":true,"cleanup_confirmed":true}`；endpoint、Secret、AK/SK 和原始 workspace prefix 不得输出。

测试通过后生成不含凭据的 evidence JSON。它必须绑定 profile ID、mounter/sandbox digest、服务/Everest/s3fs 版本、目录标记、TLS、固定 options，以及 Kubernetes/Docker 各自的 `functional`、`faults` 和 `cleanup_confirmed` 布尔值。随后才允许执行 recorder，例如 MinIO：

```bash
scripts/workspace-fuse-preflight.sh record-profile \
  --profile-id minio-sigv4-path-style-v1 \
  --image-digest "$MINIO_MOUNTER_IMAGE_DIGEST" \
  --sandbox-image-digest "$MINIO_SANDBOX_IMAGE_DIGEST" \
  --service-version "$MINIO_SERVICE_VERSION" \
  --s3fs-version "$S3FS_VERSION" \
  --directory-marker trailing-slash-zero-byte \
  --tls-verify true \
  --option use_path_request_style \
  --option sigv4 \
  --evidence /secure-evidence/minio.json \
  --output testdata/fuse/profiles/minio-sigv4-path-style-v1.yaml
```

OBS 两份报告还强制要求各自实测的 `--everest-version`。recorder 校验证据与待写参数完全一致，保存 evidence 文件的 SHA-256，并原子写出 `enabled: true/status: release-verified`；profile 中的任何 digest 都不能用环境变量覆盖。原始 evidence、镜像签名、SBOM 和扫描结果保存在制品库，不提交凭据或内部 endpoint。

## 9. 灰度、回滚与变更顺序

### 9.1 上线顺序

1. 构建并扫描 provider 专用镜像，记录 digest。
2. 完成 MinIO 和 OBS provider spike，冻结 profile。
3. 部署配置和 Secret，但保持 `workspace.mode=sync`。
4. 先启用 FUSE Pool WarmUp 但不路由用户请求，验证 prepared 空壳数量、无 mount/owner/lease、NotReady 语义、配置换代排空和补池。
5. 在测试环境完成 MinIO、华为公有云 OBS、2023 私有云 OBS 与 Kubernetes、Docker 的六组合 Pool hit/miss、Acquire 挂载和 single-use 销毁验证；公有云和私有云报告不能互相替代。
6. 先灰度 Kubernetes × MinIO，再 Docker × MinIO；随后按各自独立 profile 灰度公有云/私有云 OBS 的 Kubernetes，最后灰度各自的 Docker。
7. 每个 profile 至少观察一个完整 sandbox TTL，验证租约接管、API/Redis/节点故障、Pool refill 和 cleanup。
8. 达到实测阈值后逐步扩大新建 sandbox 流量。

静态 AK/SK 轮换不走热更新：先停止该 provider 的 FUSE 新建流量，排空或销毁全部存量 FUSE sandbox，再更新控制面和 runtime namespace Secret，完成验证后恢复新建流量。

### 9.2 回滚

- 把 `workspace.mode` 改回 `sync` 只影响新建 sandbox。
- 切回 sync 时立即停止补充 FUSE Pool，并删除所有未绑定 prepared 空壳；已消费授权的实例按正常排空，不回池。
- 已运行的 FUSE sandbox 保持原模式直到销毁，不做原地切换。
- 回滚后的首次挂载执行全量同步，不复用 FUSE 写入形成的旧增量基线。
- 保留 provider 镜像 digest 和 profile 版本，便于对运行中实例排障。
- 如果是单个 provider 故障，只禁用该 provider 的 FUSE 新建流量，不影响已验证 provider。

## 10. 生产验收清单

### 10.1 通用

- [ ] 镜像全部使用 digest，没有 `latest`。
- [ ] 每个发布 profile 的 mounter 都用精确 `-ldflags '-X=main.imageProfileID=<exact-profile-id>'` 单独构建，且 Kubernetes/Docker 镜像中的二进制与 manifest ID 一致。
- [ ] `BASE_IMAGE` 使用 digest，s3fs artifact 使用 HTTPS 并完成 SHA-256 校验；真实镜像的 scan、SBOM 和签名 attestation 已归档。
- [ ] Kubernetes 与 Docker 镜像先通过 `package-check`，再通过 `release-check`；当前三个 checked-in profile 在状态提升前均不得通过后者。
- [ ] 华为公有云和 2023 私有云拥有不同的 profile、镜像 digest 与实测报告，没有互相复用验证结论。
- [ ] 凭证未进入 Git、values、environment、命令行、inspect 或日志。
- [ ] `mode=fuse` 与 `quota_mode=soft` 同时显式开启。
- [ ] 对象根路径等于 `sub_path/workspace_path`，没有重复 prefix。
- [ ] 对象根路径规范化后带且仅带一个尾部 `/`。
- [ ] FUSE 对非 canonical `sub_path`（重复 `/`、dot segment、首尾 `/`）启动失败且不改写；合法 Unicode 的 byte identity 在 file API、mount source 和 lease key 中一致。
- [ ] 每个 provider/profile 都能通过目录标记挂载全新空 prefix，不能依赖 no-op `MakeDir`。
- [ ] `storage_identity` 对同一物理 bucket 的所有 endpoint 别名保持一致，endpoint 轮换不会绕过独占租约。
- [ ] 私有 CA 同时被 sandbox-api 存储客户端和 mounter 使用，未关闭 TLS 主机名校验。
- [ ] Redis owner、lease、generation 和 runtime identity 可恢复。
- [ ] Session 只扫描 `sandbox:session:v2:`；Pool/lease/owner/generation key 不会被当作 session，旧 key 只做 exact-key 验证迁移。
- [ ] Redis owner 的 `mount_attempt` 对同一 Pod UID/generation 只能 CAS 一次，节点重启丢失 `mounter-run` 时不会重新授权旧 Pod。
- [ ] PoolKey 覆盖所有固定存储、镜像、Secret/credential generation、网络和安全配置；不同 key 的空壳不会混用。
- [ ] PoolKey 不包含动态 `workspace_path/prefix` 或请求级用户网络规则；固定字段不匹配时 cold prepare，不借用其他 key 空壳。
- [ ] 生产 `fusePool.minSize>=1` 且完成突发容量压测；若设置为 0，已明确接受所有请求走 cold prepare。
- [ ] `sandbox-api` 启动时 WarmUp 到 `minSize`，Acquire/异常移除后自动补池，周期对账可修复数量漂移；没有部署其他 Pool 控制组件。
- [ ] 多副本通过 Redis 原子 reserve prepared record；同一 runtime UID 不会被领取两次，prepared inspection 遇 Protected/未知状态会隔离且不会误删。
- [ ] runtime 创建前已登记 PreparationID 容量槽；RuntimeUID 绑定全局唯一且不可变，Pool transition/publish 校验 refill lock token、state 和 revision。
- [ ] cold prepare 的 reservation TTL 从 Redis publish 时开始，直接进入请求独占的 reserved，不暴露 prepared 抢占窗口。
- [ ] cleanup claim 先于任何 runtime 删除；删除使用精确 RuntimeID+RuntimeUID，失败 tombstone 可重试且过期后可安全接管。
- [ ] sandbox-api 在 lease 获取后立即续租，并持续监督 lease/runtime UID/generation/FUSE health；失败时 gate 在销毁前先关闭。
- [ ] FUSE 模式 Redis/reconcile/WarmUp 失败会阻止 HTTP 服务监听。
- [ ] 1 GiB direct upload 端到端流式通过，非上传路由仍在 64 MiB 返回 413，API 内存不随文件大小线性增长。
- [ ] AppArmor/SELinux 使用明确的非 unconfined profile；用户进程 mount 负向测试失败、可信 supervisor 正向测试成功。
- [ ] 多副本同时补池时，全局 `preparing + prepared` 不超过 `maxSize`；单副本停止只考虑带自身 ownership token 的 preparing/prepared 空壳，且 guard 为 Protected/Unknown/错误时保留并报告未完全 Drain，绝不删除其他 maintainer 或已进入 reserved/binding/consumed 的实例。
- [ ] prepared 空壳已启动基础容器，但没有 s3fs、workspace mount、owner/lease 或开放的 Exec/file gate。
- [ ] prepared 空壳不创建用户 session，不出现在公共 list/get 响应中，公共 API 无法凭 runtime ID 绕过 gate。
- [ ] 授权前失败只有 pristine probe 通过才可归还空壳；授权后成功、失败或取消都销毁实例并补池。
- [ ] system egress 与用户网络策略相互独立。
- [ ] 1 GiB 文件和 10,000 小文件场景通过，API 内存不随文件大小线性增长。
- [ ] 后台/双重 fork 用户进程不会在 Exec 结束后绕过 quiesce；无法证明静止时 `SyncWorkspace` 不返回 `flushed=true`。

### 10.2 Kubernetes

- [ ] `/workspace` 使用 Pod 私有 `emptyDir`，没有业务 hostPath。
- [ ] runtime namespace 已预置 default-deny，sandbox 专属 allow policy 在 Pod 创建前完成，且 selector 使用不可变 instance label 而非可变 state label。
- [ ] 只有 mounter sidecar 可见 Secret 和 `/dev/fuse`。
- [ ] 只有 mounter sidecar privileged，sandbox 主容器保持非特权 UID/GID 1000。
- [ ] Pod 禁用 ServiceAccount token、service links 和共享进程 namespace；sandbox 使用 RuntimeDefault seccomp 并 drop ALL capabilities。
- [ ] mounter 配置 CPU、内存和 ephemeral-storage request/limit；cache 超限按 eviction/error 重建，不宣称硬 quota 或 ENOSPC。
- [ ] s3fs `tmpdir` 和可选 `use_cache` 均指向 `/var/cache/s3fs` 下的受限目录。
- [ ] Pod 使用公共 nameserver，目标 endpoint 可解析且 `cluster.local` 不可解析。
- [ ] prepared startup、mounter readiness 和 Acquire 后 sandbox 内固定读写探测均通过；sidecar 或 s3fs 异常不会原地自动 remount。
- [ ] prepared Pod 的 NotReady 不会被 Pool 当作失败；Pool hit 挂载前后 Pod UID 不变。
- [ ] mounter `preStop` 可以在 termination grace period 内完成 flush/unmount，主容器没有长时间 preStop。
- [ ] prepared 空壳删除时 `preStop` 能识别无 mount 并立即成功，不会消耗完整 termination grace period。
- [ ] Pod 删除、节点重启、sidecar 异常后无遗留 mount。
- [ ] NetworkPolicy/Cilium 策略只开放 DNS 和 provider system egress。

### 10.3 Docker

- [ ] 宿主机只提供 `/dev/fuse` 和 root-only Secret 暂存目录，不挂载业务 `/workspace`。
- [ ] root supervisor 与 UID/GID 1000 用户执行边界经过安全测试。
- [ ] prepared 容器无 s3fs/mount generation，Pool hit 挂载前后 container ID 不变，授权后的容器不会回池。
- [ ] 特殊容器使用最小 capabilities、seccomp/LSM 和只读根文件系统。
- [ ] cache 使用独立 volume/目录并配置软阈值；阈值触发时终止重建，Secret 和 cache 均随容器清理。
- [ ] s3fs `tmpdir` 和可选 `use_cache` 均指向受阈值监控的 cache volume，而不是 `/tmp`。
- [ ] API/daemon 重启和强制删除后没有孤儿容器、Secret 或租约。

## 11. 日常观测与故障定位

至少采集以下指标并按 provider/runtime 打标签：

- mount 启动耗时、成功率和失败原因；
- Pool preparing/prepared/reserved/binding 数量、hit/miss、补池耗时、pristine return 和 discard 原因；
- Kubernetes 通用 Pod NotReady 告警必须排除 `sandbox.pool.state=prepared`，改为对 prepared health、超出 prepare timeout 和 Pool 数量不足单独告警；
- startup/readiness 失败、generation/restart gate 触发次数；
- FUSE 进程重启、非正常退出和强制卸载次数；
- cache 使用量、软阈值触发、ephemeral-storage eviction、实际 ENOSPC 和 flush 延迟；
- endpoint DNS/TLS/鉴权错误；
- lease 续期失败、owner 冲突和 stale runtime 清理；
- sandbox 创建延迟、首读延迟、顺序吞吐和小文件耗时。

排障时先确认 mount、endpoint、Secret 和 lease 四个状态，不要通过开放网络、关闭 TLS 校验、改为 privileged sandbox 或回退到底层目录来绕过故障。
