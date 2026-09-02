# Workspace FUSE 部署与运维手册

**适用设计：** [Workspace 容器内 FUSE 直接挂载设计](../superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md)

**适用范围：** Kubernetes sidecar、Docker 特殊容器、MinIO、华为 OBS 普通对象桶

**文档状态：** 目标部署契约。当前代码尚未实现本文新增的 FUSE 配置和运行时编排；实现合入后方可按本文启用。现有 `workspace.mode=sync` 部署不受影响。

## 1. 部署结论

- Kubernetes：每个 sandbox Pod 注入一个 FUSE 原生 sidecar，业务 `/workspace` 使用 Pod 私有 `emptyDir`，不挂载宿主机业务目录。sidecar 负责挂载，sandbox 主容器仅消费传播后的 mount。
- Docker：每个 sandbox 使用自带 s3fs 和 root supervisor 的特殊镜像，API 通过 Docker Device 映射 `/dev/fuse`。不在宿主机挂载 `/workspace`，也不运行宿主机常驻 s3fs 进程。
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
| sandbox-api | Helm Deployment | Compose service | 创建/销毁 sandbox、签发运行时规格、维护租约与状态 |
| FUSE 进程 | 每个 sandbox Pod 的 sidecar | 每个特殊 sandbox 容器内的 root supervisor 子进程 | 将单一 workspace prefix 挂载到 `/workspace` |
| 用户进程 | sandbox 主容器，UID/GID 1000 | 同一容器内由 supervisor/exec 强制 UID/GID 1000 | 访问 `/workspace`，不能控制 FUSE |
| Secret | Pod Secret volume，仅 sidecar 可见 | 宿主机 root-only 暂存文件，仅挂载到特殊容器 | 提供 provider 级静态长期 AK/SK 和自定义 CA |
| Redis | Helm 内置或外部 Redis | Compose Redis 或外部 Redis | TTL lease、持久 owner、generation、runtime identity |
| system egress | 每 sandbox NetworkPolicy/Cilium 策略 | 受控 gateway 网络 | 只允许 DNS 和目标对象存储 endpoint |

Helm Chart 只部署控制面。sandbox Pod 由 Kubernetes runtime 动态生成，sidecar、volume、probe 和 sandbox 级 NetworkPolicy 必须在 Go runtime 中实现，不能静态加入 sandbox-api Deployment。

Compose 同样只部署控制面和依赖。特殊 sandbox 容器由 Docker runtime 动态创建，不作为长期 Compose service。

## 3. 交付物和版本固定

发布前应生成并固定以下镜像：

| 变量 | 内容 |
|---|---|
| `SANDBOX_API_IMAGE` | sandbox-api 镜像，使用 digest |
| `SANDBOX_BASE_IMAGE` | 普通 sync 模式 sandbox 镜像，使用 digest |
| `MINIO_MOUNTER_IMAGE` | Kubernetes MinIO s3fs sidecar 镜像，使用 digest |
| `OBS_MOUNTER_IMAGE` | Kubernetes OBS 兼容 s3fs sidecar 镜像，使用 digest |
| `MINIO_SANDBOX_FUSE_IMAGE` | Docker MinIO 特殊 sandbox 镜像，使用 digest |
| `OBS_SANDBOX_FUSE_IMAGE` | Docker OBS 特殊 sandbox 镜像，使用 digest |
| `GATEWAY_IMAGE` | Docker system egress gateway 镜像，使用 digest |

生产环境不得使用 `latest` 或可变 tag。镜像中应记录：

- s3fs 版本、commit 和构建参数；
- provider profile 版本；
- 基础镜像 digest；
- SBOM 和漏洞扫描结果；
- 支持的架构；
- CA bundle 版本。

MinIO 与 OBS 可共用源码仓库和构建流水线，但必须能独立升级和回滚。OBS provider spike 如果证明上游 s3fs 不满足签名或目录语义要求，`OBS_MOUNTER_IMAGE` 和 `OBS_SANDBOX_FUSE_IMAGE` 应切换为固定的厂商兼容构建，不能悄悄复用 MinIO 镜像。

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
- 生产必须启用 TLS 和主机名校验。
- 私有 CA 通过 Secret 文件注入 mounter，不修改宿主机全局 CA。
- endpoint 使用 DNS 名时，必须能由配置的公共 nameserver 解析，system egress 必须允许 DNS 和 endpoint 连接。
- Kubernetes 不接入 CoreDNS；`*.svc.cluster.local` 不能作为 MinIO endpoint。需要使用专用稳定 FQDN、egress proxy 或显式 IP。
- MinIO 明确使用 path-style；OBS 的签名版本、region 和 path-style 由 provider spike 的固定结果决定。
- bucket 必须已存在。挂载器不得以高权限凭证自动创建 bucket。

### 4.3 Redis

FUSE 模式依赖 Redis 独占租约。生产必须使用持久化且具备故障恢复能力的 Redis；内置单实例 Redis 仅适合开发或验证环境。部署前确认：

- sandbox-api 所有副本连接同一 Redis；
- Redis 数据持久化和备份已启用；
- 时钟同步正常；
- 监控覆盖连接错误、租约续期失败和 owner 冲突；
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

使用私有 CA 时增加 `ca.crt`：

```bash
kubectl -n "$SANDBOX_NAMESPACE" create secret generic sandbox-workspace-minio \
  --from-literal=accessKey="$STORAGE_ACCESS_KEY" \
  --from-literal=secretKey="$STORAGE_SECRET_KEY" \
  --from-file=ca.crt="$MINIO_CA_FILE" \
  --dry-run=client -o yaml | kubectl apply -f -
```

第二种写法适合受控终端，但 literal 可能进入本机进程参数审计；生产优先使用 External Secrets、Sealed Secrets 或等价设施。

Pod 直接 `secretKeyRef`/Secret volume 引用已存在的 provider Secret 时，sandbox-api ServiceAccount 不需要 `get/list/watch secrets`。一期不创建 per-sandbox Secret，也不接受 `sessionToken`；Secret 中出现该字段时配置校验失败。现有 Chart 把 `config.storage.filesystem.accessKey/secretKey` 直接渲染为普通环境变量；FUSE 实现合入时必须改成 `secretKeyRef` 或文件型 credential provider，并删除 values 中的明文凭证入口。

### 5.2 Docker

Docker sandbox 不能把凭证放入 `Env` 或 `Cmd`，否则可通过 `docker inspect` 读取。宿主机准备专用暂存根目录，并以相同绝对路径读写挂载给受信任的 sandbox-api；API 为动态容器生成子目录后，再把该子目录只读挂载到特殊 sandbox：

```bash
sudo install -d -m 0700 -o root -g root /var/lib/sandbox/fuse-secrets
```

运行时为每个 sandbox 创建独立子目录和 mode `0600` 文件；特殊容器只读挂载该子目录。销毁容器后立即删除文件，reconciler 负责清理失联容器留下的过期目录。文件名和目录名只使用经过校验的内部 sandbox ID，不使用用户输入路径。

## 6. Kubernetes 部署

### 6.1 Namespace 与安全策略

推荐把 sandbox Pod 放在专用 runtime namespace，和 sandbox-api 控制面分离。集群必须支持 Kubernetes 原生 sidecar container（init container 中的 `restartPolicy: Always`）；Kubernetes 1.29 及以上默认启用该能力，部署仍需通过服务端 dry-run 验证目标集群。FUSE sidecar 需要挂载能力，第一期使用可信 `privileged` sidecar；因此 runtime namespace 必须通过 Pod Security Admission 或等价准入策略允许该 sidecar。sandbox 主容器仍保持：

- `runAsNonRoot: true`；
- `runAsUser: 1000`、`runAsGroup: 1000`；
- `allowPrivilegeEscalation: false`；
- `readOnlyRootFilesystem: true`；
- `capabilities.drop: ["ALL"]`；
- 不挂载 Secret 和 `/dev/fuse`。

Pod 级强制设置 `automountServiceAccountToken: false`、`enableServiceLinks: false`、`shareProcessNamespace: false` 和 RuntimeDefault seccomp。`workspace-ready` init container 同样设置非 root、`allowPrivilegeEscalation: false`、只读 rootfs 和 drop ALL capabilities。

不要为了 sidecar 把 sandbox 主容器改成 privileged，也不要对整个业务 namespace 放宽安全基线。

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

`fuse-cache.sizeLimit` 不是文件系统硬 quota。必须同时为 mounter 配置 `ephemeral-storage` request/limit，并监控 cache；达到软阈值或 kubelet 发出 eviction 时阻止新 Exec、取消活动 Exec 并终止重建，不保证用户写调用先收到 ENOSPC。

### 6.3 目标 Helm values

以下配置块是实现完成后 Chart 应支持的目标 schema。它补充现有 `config.workspace`，不应把 AK/SK 写入 values：

```yaml
config:
  storage:
    filesystem:
      provider: minio
      bucket: sandbox-storage
      region: us-east-1
      endpoint: https://minio-fuse.example.com:9000
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
        mounterImage: registry.example.com/sandbox-s3fs-minio@sha256:<64-hex-digest>
        dockerImage: registry.example.com/sandbox-fuse-minio@sha256:<64-hex-digest>
        systemEgressFQDNs:
          - minio-fuse.example.com
        systemEgressCIDRs: []
      obs:
        driver: s3fs
        profile: huawei-obs-verified-v1
        mounterImage: registry.example.com/sandbox-s3fs-obs@sha256:<64-hex-digest>
        dockerImage: registry.example.com/sandbox-fuse-obs@sha256:<64-hex-digest>
        systemEgressFQDNs:
          - obs.cn-north-4.myhuaweicloud.com
        systemEgressCIDRs: []

storageCredentials:
  existingSecret: sandbox-storage-minio
  accessKeyKey: accessKey
  secretKeyKey: secretKey
```

`<64-hex-digest>` 必须替换为真实镜像 digest。运行时只选择与 `storage.filesystem.provider` 同名的 profile；没有验证结果或 profile 不匹配时启动失败。不同 provider 推荐使用独立 release values，避免 endpoint、bucket、Secret 与 profile 交叉配置。

`storageCredentials` 是 Chart 层配置：实现后应把该 Secret 作为只读文件投射给 sandbox-api，并将文件路径映射到 `storage.filesystem.credential_files`。它和 `config.workspace.secretName` 职责不同：前者供控制面存储 driver 使用，后者是动态 sandbox sidecar 的 Secret 引用。

部署前先渲染检查：

```bash
helm lint deploy/helm/sandbox
helm template sandbox deploy/helm/sandbox \
  --namespace "$CONTROL_NAMESPACE" \
  -f values-fuse.yaml > rendered-sandbox.yaml
kubectl apply --dry-run=server -f rendered-sandbox.yaml
helm upgrade --install sandbox deploy/helm/sandbox \
  --namespace "$CONTROL_NAMESPACE" \
  --create-namespace \
  -f values-fuse.yaml
```

### 6.4 动态 sandbox Pod 目标模板

以下片段描述 runtime 必须生成的关键字段。`workspace-mounter` 是 Kubernetes 原生 sidecar init container；它在普通 init container 和主容器之前启动并持续运行：

```yaml
apiVersion: v1
kind: Pod
metadata:
  labels:
    sandbox.managed: "true"
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
      lifecycle:
        preStop:
          exec:
            command:
              - /usr/local/bin/mounter-control
              - shutdown
              - --mount=/workspace
              - --flush-timeout=30s
              - --unmount-timeout=20s
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
          command: ["/usr/local/bin/mounter-health", "startup", "/workspace"]
        periodSeconds: 2
        failureThreshold: 30
      readinessProbe:
        exec:
          command: ["/usr/local/bin/mounter-health", "ready", "/workspace"]
        periodSeconds: 10
        failureThreshold: 3
      livenessProbe:
        exec:
          command: ["/usr/local/bin/mounter-health", "live", "/workspace"]
        periodSeconds: 10
        failureThreshold: 3
    - name: workspace-ready
      image: registry.example.com/sandbox-s3fs-minio@sha256:<64-hex-digest>
      command: ["/usr/local/bin/mounter-health", "write-probe", "/workspace"]
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

实际 runtime 还必须根据 workspace 生成唯一 prefix、租约标签和 runtime identity。对象 key 根路径严格为：

```text
<storage.filesystem.sub_path>/<workspace_path>
```

不得再次追加独立 `object_prefix`，也不得把 workspace path 作为 shell 字符串拼接进 s3fs 命令。

主容器启动后，runtime 必须持续观察 sidecar 的 `restartCount`、Pod Ready 和 mount generation。liveness 导致 sidecar 重启时，立即阻止新 Exec 并取消活动 Exec；默认删除并重建整个 Pod，只有确认没有活动 Exec 时才允许原地重挂载。单纯 endpoint 网络中断只令 readiness 失败，不能触发 liveness 重启风暴。

### 6.5 RBAC

控制面需要在 runtime namespace 内管理：

- Pod 的 create/get/list/watch/delete；
- `pods/exec` 的 create；
- sandbox 级 NetworkPolicy 的 create/get/update/delete；
- 使用 Cilium 时，对 CiliumNetworkPolicy 的 create/get/update/delete。

Secret 通过已知名称挂载时不授予读取权限。若 control namespace 与 runtime namespace 不同，在 runtime namespace 创建 Role 和 RoleBinding，RoleBinding 的 subject 指向 control namespace 中的 sandbox-api ServiceAccount。

### 6.6 网络策略

现有 Chart 的静态 NetworkPolicy 只提供默认拒绝和 DNS 基线。FUSE endpoint 的 system egress 必须由 runtime 为每个 sandbox 动态生成：

- Pod 保持 `DNSPolicy=None`，DNS egress 只允许运维配置的公共 nameserver；不允许 CoreDNS，也不配置集群 search domain；
- 无论 `network_enabled` 是否为 false，都允许 sidecar 访问该 workspace 固定 provider endpoint；
- sandbox 主容器不因此获得任意公网访问；
- endpoint 必须是公共 nameserver 可解析的稳定专用 FQDN，或显式稳定 IP；不支持 `cluster.local` Service。仅加入网络白名单不能解决集群域名解析；
- Cilium 环境可使用 FQDN policy；仅使用标准 NetworkPolicy 时，使用运维维护的稳定 CIDR、固定 egress proxy 或显式 endpoint IP，不把短期 DNS 解析结果永久写死；
- 禁止访问云元数据地址和不必要的 RFC1918 网段。

NetworkPolicy 不能按容器区分流量；若必须严格保证主容器无法直连对象存储，应使用 sidecar 独立网络身份、CNI 扩展或 egress proxy，而不是仅依赖同 Pod 的标准 NetworkPolicy。

### 6.7 Kubernetes 验证

创建测试 sandbox 后执行：

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

Compose 需要把 Secret 暂存目录以同一绝对路径挂载给 API，以便 API 创建动态容器时使用宿主机可解析的 source path：

```yaml
services:
  sandbox-api:
    secrets:
      - storage_access_key
      - storage_secret_key
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /var/lib/sandbox/fuse-secrets:/var/lib/sandbox/fuse-secrets:rw
      - ./config-fuse.yaml:/etc/sandbox/config.yaml:ro

secrets:
  storage_access_key:
    file: ${STORAGE_ACCESS_KEY_FILE}
  storage_secret_key:
    file: ${STORAGE_SECRET_KEY_FILE}
```

嵌套 provider profile 推荐通过只读配置文件传入，不依赖 Compose 环境变量展开 map key。目标配置示例：

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
    endpoint: https://minio.example.internal:9000
    sub_path: workspaces
    use_ssl: true
    credential_files:
      access_key_file: /run/secrets/storage_access_key
      secret_key_file: /run/secrets/storage_secret_key

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
      mounter_image: registry.example.com/sandbox-s3fs-minio@sha256:<64-hex-digest>
      docker_image: registry.example.com/sandbox-fuse-minio@sha256:<64-hex-digest>
      system_egress_fqdns: [minio.example.internal]
      system_egress_cidrs: []
    obs:
      driver: s3fs
      profile: huawei-obs-verified-v1
      mounter_image: registry.example.com/sandbox-s3fs-obs@sha256:<64-hex-digest>
      docker_image: registry.example.com/sandbox-fuse-obs@sha256:<64-hex-digest>
      system_egress_fqdns: [obs.cn-north-4.myhuaweicloud.com]
      system_egress_cidrs: []
```

对象存储的 provider、bucket、endpoint、region、sub path 和 TLS 开关继续使用 `storage.filesystem` 配置。AK/SK 不出现在该文件；Compose Secret 默认出现在 `/run/secrets/storage_access_key` 和 `/run/secrets/storage_secret_key`，sandbox-api 读取后在 `/var/lib/sandbox/fuse-secrets` 中为动态容器生成 root-only 文件。实现需要新增文件型 credential provider，不能继续使用当前 Compose 中的 `SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY/SECRET_KEY` 明文环境变量。

### 7.3 特殊镜像要求

Docker FUSE 镜像不是简单把 s3fs 安装进现有 sandbox 镜像。它必须包含经审计的 root supervisor，并满足：

1. 容器启动时把 `/workspace` 底层 anchor 设置为 `root:root`、mode `0555`。
2. supervisor 以 root 读取 mode `0400/0600` Secret，生成 s3fs 密码文件并启动前台 FUSE 进程。
3. mount ready 后才允许 API 执行用户命令。
4. 所有用户命令和文件命令强制 UID/GID 1000，不能调用 root supervisor 的控制接口。
5. FUSE 进程异常退出时取消活动 Exec，容器进入 unhealthy/error；默认销毁并重建，而不是把用户流量切到底层目录。
6. 收到 TERM 时停止接收 Exec、flush、卸载，然后退出；超时后 runtime 才执行强制删除。

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
securityOpt:
  - no-new-privileges=true
readOnlyRootfs: true
```

并挂载：

- 每 sandbox root-only Secret 目录到 `/run/secrets/workspace:ro`；
- 独立 cache volume/目录到 `/var/cache/s3fs`，由 supervisor 上报使用量并按 `cache_size` 软阈值触发终止重建；
- 必要的 tmpfs 到 `/run` 和 `/tmp`；
- 不挂载宿主机业务 `/workspace`。

`CAP_SYS_ADMIN` 属于容器内 root supervisor，不得传递给 UID 1000 用户进程。runtime 必须覆盖所有 Exec、文件 API 和间接命令路径，强制 `User=1000:1000`。容器镜像内 `/workspace` 底层目录必须不可由 UID 1000 写入，以便 mount 消失时 fail closed。

普通 Docker named volume 不提供可移植的硬 quota。达到 cache 软阈值时，runtime 必须阻止新 Exec、取消活动 Exec、尽力 flush 后删除并重建容器；不承诺用户写操作先收到 ENOSPC。cache 不得落在无界 container writable layer，销毁后必须清理。

部分 Docker/LSM 组合需要额外 AppArmor FUSE mount 规则。若当前节点只有 `apparmor=unconfined` 才能运行，应停止上线并补充最小 profile，不能把 unconfined 作为生产默认值。

### 7.5 system egress

FUSE 容器始终加入只允许以下目的地的系统网络：

- DNS resolver；
- 当前 provider endpoint/egress proxy；
- 必需的证书状态或内部 PKI 服务。

用户网络关闭时仍保留该系统网络；用户进程的网络请求必须经过 gateway 策略，不能因为与 supervisor 同容器而获得不受限出口。对象存储 endpoint 和用户白名单分别建模，不能把 endpoint 自动加入用户可见白名单。

### 7.6 Docker 验证

创建测试 sandbox 后执行：

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
- 普通用户不能执行 mount/umount、读取 Secret、向 supervisor 发控制命令或取得 root；
- 宿主机没有 sandbox 对应的 `/workspace` mount；
- FUSE 进程退出后 health 失败，用户写入不会落到镜像目录；
- API/daemon 重启后 reconciler 能依据 runtime identity 清理旧容器、Secret 和租约。

## 8. Provider profile

### 8.1 MinIO 固定基线

MinIO profile 至少固定：

- 显式 HTTPS endpoint；
- `use_path_request_style`；
- SigV4；
- region，默认不依赖 AWS endpoint 推导；
- `allow_other`、`uid=1000`、`gid=1000`、`umask=0022`、`mp_umask=0022`；
- 前台运行、有界 cache 和 multipart 参数；
- TLS 主机名与 CA 校验。
- provider 级静态长期 AK/SK，写入 root-only s3fs `passwd_file`；出现 session token 时拒绝启动。

### 8.2 华为 OBS 固定基线

OBS profile 必须由目标区域、目标普通对象桶和最终镜像完成 provider spike 后冻结。至少验证：

- 上游 s3fs 与厂商兼容构建分别测试；
- `sigv2` 是否需要；
- region 和 endpoint 组合；
- `compat_dir`/`support_compat_dir` 目录对象语义；
- path-style 或 virtual-host-style；
- `big_writes`、multipart、零字节对象、rename 和大量小文件；
- 私有 CA、TLS SNI 和主机名校验。
- provider 级静态长期 AK/SK 与目标 s3fs 构建的 `passwd_file` 认证。

未完成 spike 前，不得把 `storage.filesystem.provider` 切换为 `obs`，也不得发布可选中的 OBS profile/image。验证结论要落为版本化 profile，而不是在生产临时追加参数。

## 9. 灰度、回滚与变更顺序

### 9.1 上线顺序

1. 构建并扫描 provider 专用镜像，记录 digest。
2. 完成 MinIO 和 OBS provider spike，冻结 profile。
3. 部署配置和 Secret，但保持 `workspace.mode=sync`。
4. 在测试环境完成 Kubernetes × MinIO、Docker × MinIO、Kubernetes × OBS、Docker × OBS 四象限验证。
5. 先灰度 Kubernetes × MinIO，再 Docker × MinIO，然后 Kubernetes × OBS，最后 Docker × OBS。
6. 每一步至少观察一个完整 sandbox TTL，验证租约接管、API/Redis/节点故障和 cleanup。
7. 达到实测阈值后逐步扩大新建 sandbox 流量。

静态 AK/SK 轮换不走热更新：先停止该 provider 的 FUSE 新建流量，排空或销毁全部存量 FUSE sandbox，再更新控制面和 runtime namespace Secret，完成验证后恢复新建流量。

### 9.2 回滚

- 把 `workspace.mode` 改回 `sync` 只影响新建 sandbox。
- 已运行的 FUSE sandbox 保持原模式直到销毁，不做原地切换。
- 回滚后的首次挂载执行全量同步，不复用 FUSE 写入形成的旧增量基线。
- 保留 provider 镜像 digest 和 profile 版本，便于对运行中实例排障。
- 如果是单个 provider 故障，只禁用该 provider 的 FUSE 新建流量，不影响已验证 provider。

## 10. 生产验收清单

### 10.1 通用

- [ ] 镜像全部使用 digest，没有 `latest`。
- [ ] 凭证未进入 Git、values、environment、命令行、inspect 或日志。
- [ ] `mode=fuse` 与 `quota_mode=soft` 同时显式开启。
- [ ] 对象根路径等于 `sub_path/workspace_path`，没有重复 prefix。
- [ ] Redis owner、lease、generation 和 runtime identity 可恢复。
- [ ] system egress 与用户网络策略相互独立。
- [ ] 1 GiB 文件和 10,000 小文件场景通过，API 内存不随文件大小线性增长。

### 10.2 Kubernetes

- [ ] `/workspace` 使用 Pod 私有 `emptyDir`，没有业务 hostPath。
- [ ] 只有 mounter sidecar 可见 Secret 和 `/dev/fuse`。
- [ ] 只有 mounter sidecar privileged，sandbox 主容器保持非特权 UID/GID 1000。
- [ ] Pod 禁用 ServiceAccount token、service links 和共享进程 namespace；sandbox 与普通 init container 使用 RuntimeDefault seccomp 并 drop ALL capabilities。
- [ ] mounter 配置 CPU、内存和 ephemeral-storage request/limit；cache 超限按 eviction/error 重建，不宣称硬 quota 或 ENOSPC。
- [ ] Pod 使用公共 nameserver，目标 endpoint 可解析且 `cluster.local` 不可解析。
- [ ] startup/readiness/liveness 和 `workspace-ready` init 检查均通过。
- [ ] mounter `preStop` 可以在 termination grace period 内完成 flush/unmount，主容器没有长时间 preStop。
- [ ] Pod 删除、节点重启、sidecar 异常后无遗留 mount。
- [ ] NetworkPolicy/Cilium 策略只开放 DNS 和 provider system egress。

### 10.3 Docker

- [ ] 宿主机只提供 `/dev/fuse` 和 root-only Secret 暂存目录，不挂载业务 `/workspace`。
- [ ] root supervisor 与 UID/GID 1000 用户执行边界经过安全测试。
- [ ] 特殊容器使用最小 capabilities、seccomp/LSM 和只读根文件系统。
- [ ] cache 使用独立 volume/目录并配置软阈值；阈值触发时终止重建，Secret 和 cache 均随容器清理。
- [ ] API/daemon 重启和强制删除后没有孤儿容器、Secret 或租约。

## 11. 日常观测与故障定位

至少采集以下指标并按 provider/runtime 打标签：

- mount 启动耗时、成功率和失败原因；
- readiness/liveness 失败次数；
- FUSE 进程重启、非正常退出和强制卸载次数；
- cache 使用量、软阈值触发、ephemeral-storage eviction、实际 ENOSPC 和 flush 延迟；
- endpoint DNS/TLS/鉴权错误；
- lease 续期失败、owner 冲突和 stale runtime 清理；
- sandbox 创建延迟、首读延迟、顺序吞吐和小文件耗时。

排障时先确认 mount、endpoint、Secret 和 lease 四个状态，不要通过开放网络、关闭 TLS 校验、改为 privileged sandbox 或回退到底层目录来绕过故障。
