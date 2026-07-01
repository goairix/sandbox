# Workspace 直接挂载：Pod 内 Sidecar FUSE 设计

**日期：** 2026-06-28  
**状态：** 草案

> 本方案由 `2026-06-28-workspace-storage-rootless-fuse.md` 改造而来，针对两个硬性约束：
> **(1) 不在 K8s 宿主节点上放置任何常驻/共享组件；(2) 不使用 PVC。**
> 与 rootless 版相比，它把 FUSE 挂载从"untrusted 主容器内"搬到"同 Pod 的独立特权 sidecar 容器",
> 从而堵住 rootless 版的**凭证泄露**，并且不再需要放开主容器的 userns/mount 攻击面。
>
> **FUSE 客户端锁定为 1:1 明文类（geesefs / s3fs）**：容器写入的文件在对象存储桶中即为
> 同名明文对象，与现有文件 API（直接读写桶、对象==文件）天然兼容，无需引入元数据引擎。

## 背景

当前 workspace 通过 `SyncToContainer`/`SyncFromContainer` 在 sandbox-api 进程内中转所有文件，存在三个问题：

1. 文件量大时内存峰值约为文件总大小的 2 倍，可能触发 OOM
2. K8s 使用 emptyDir，workspace 数据驻留宿主机，多用户时宿主机磁盘快速打满
3. 写入文件需等下次 sync 才持久化，存在数据丢失窗口

本方案在 sandbox Pod 内增加一个 **mount sidecar 容器**，由它挂载远端对象存储，并通过
**mount propagation** 把 `/workspace` 透传给 untrusted 主容器。数据完全不经过 sandbox-api，
也不经过任何节点级组件。

### 约束

- **仅适用于 Kubernetes 运行时**（Docker 无 Pod 概念，需单独方案）
- 不使用 PVC / StorageClass；不部署 CSI Driver、DaemonSet 或节点 hostPath
- 需要 workspace 的 sandbox **绕过 pool**（与现有 network-enabled sandbox 逻辑一致）
- 需要 K8s **1.29+**（原生 sidecar：`initContainer.restartPolicy=Always`，GA 于 1.29）
- 存储凭证以 **K8s Secret** 形式一次性下发到 sandbox 命名空间

### 核心原理

同一 Pod 内的两个容器共享一个 `emptyDir` 卷作为 `/workspace` 挂载点：

- **mounter sidecar**（privileged，仅跑我们信任的 geesefs/s3fs 挂载镜像）以 `mountPropagation: Bidirectional`
  挂载该卷，并在其上执行 FUSE mount。挂载事件经宿主 mount 传播回 Pod。
- **主容器 sandbox**（untrusted，保持 `readonly rootfs + 非 root + seccomp`）以
  `mountPropagation: HostToContainer` 挂载同一卷，因而**看到** sidecar 挂上来的 `/workspace`。

关键收益：

- **无需 nsenter**。exec 一直落在主容器（`exec.go` 固定 `Container: "sandbox"`），主容器原生就能看到
  `/workspace`，`ExecRequest` 无需任何改动。
- **凭证隔离**。存储 key 只注入 sidecar 容器的环境变量。主容器是独立容器（默认独立 PID namespace，
  **`shareProcessNamespace` 必须保持 false**），untrusted 代码读不到 sidecar 的 `/proc/*/environ`。
- **特权收敛在 sidecar**。Bidirectional 传播要求 `privileged: true`，但这只作用于我们信任的
  mounter 容器，主容器攻击面不变。特权是 **Pod 级、随 Pod 销毁**，不是节点级常驻组件。

## 数据流

```
sandbox 创建（带 workspace_path）
    │
    ├─ buildSpec 填充 WorkspaceMounter{Image, SecretName, SubPath, ClientType}
    │  并绕过 pool（useDirectCreate）
    │
    ├─ createPod:
    │    initContainers:
    │      - workspace-mounter (restartPolicy=Always, privileged, seccomp=Unconfined)
    │          envFrom: Secret(access-key/secret-key/endpoint/bucket)
    │          volumeMount: workspace @ /workspace (Bidirectional)
    │          cmd: 前台挂载 → geesefs/s3fs ... /workspace -o allow_other
    │          readinessProbe: mountpoint -q /workspace
    │    containers:
    │      - sandbox (untrusted, readonly rootfs, uid 1000)
    │          volumeMount: workspace @ /workspace (HostToContainer)
    │  → 原生 sidecar 保证主容器在挂载 ready 后才启动
    │
    └─ Pod Running ⇒ registerWorkspace（注册 ScopedFS，不 copy 文件），FUSEMounted=true

后续 Exec/ExecStream（Container: "sandbox"）
    └─ 直接 /bin/sh -c "{command}"，/workspace 已就绪，无 nsenter

sandbox 销毁 → Pod 销毁 → sidecar 随之退出，FUSE 自动 umount，卷回收
```

## 数据模型变更

**`internal/sandbox/types.go` — WorkspaceInfo 新增字段：**

```go
type WorkspaceInfo struct {
    RootPath     string    `json:"root_path"`
    MountedAt    time.Time `json:"mounted_at"`
    LastSyncedAt time.Time `json:"last_synced_at,omitempty"`
    BindMounted  bool      `json:"bind_mounted,omitempty"`
    SyncExclude  []string  `json:"sync_exclude,omitempty"`
    FUSEMounted  bool      `json:"fuse_mounted,omitempty"` // sidecar FUSE 挂载
}
```

> 与 rootless 版不同，这里**不需要 `FUSEPid`**：挂载生命周期与 sidecar 容器绑定，
> API 无需持有 daemon PID。持久化 sandbox restore 时，只要 Pod 仍在运行、mount 就仍然存活，
> restore 只需重新 `registerWorkspace` 并置 `FUSEMounted=true`；Pod 已消失则 sandbox 本就已失效。

**`internal/runtime/types.go` — SandboxSpec 新增字段：**

```go
type SandboxSpec struct {
    // ...现有字段...
    WorkspaceMounter *WorkspaceMounterSpec // 非 nil 时注入 mount sidecar
}

// WorkspaceMounterSpec 描述 workspace 挂载 sidecar 的构造参数。
type WorkspaceMounterSpec struct {
    Image      string // mounter 镜像（含 geesefs + s3fs + fuse3）
    SecretName string // 存储凭证 Secret 名称（不含明文）
    SubPath    string // 每 workspace 的隔离子路径
    ClientType string // "geesefs"（默认，MinIO/S3）| "s3fs"（OSS/COS/OBS），均为 1:1 明文
}
```

> `ExecRequest` **无需新增 `MountNSPid`**——这是相对 rootless 版最大的简化。

## 存储布局兼容性

现有文件 API（上传/下载/列举）走 `m.workspaces` 里的 `ScopedFS`，直接读写 `m.filesystem` 指向的对象存储，
**对象 == 文件（1:1）**。本方案两个客户端都是 1:1 明文，桶内布局与文件 API 完全一致，无需任何转换层。

| 后端 | 推荐客户端 | 依据 |
|------|-----------|------|
| MinIO / AWS S3 / 其他标准 S3 端点 | **geesefs**（默认） | README 明确列出；活跃维护（v0.43.8, 2026-06-22）；POSIX 比 goofys 强 |
| 阿里云 OSS | **s3fs** | 阿里官方维护 ossfs 即 s3fs 定制版；社区验证充分 |
| 腾讯云 COS | **s3fs** | 腾讯官方 cosfs 基于 s3fs；S3 兼容协议完整 |
| 华为云 OBS | **s3fs** | 华为开发者博客官方推荐；社区验证充分 |

两个客户端均为 **1:1 明文对象**，文件 API（`ScopedFS`）零改动。

**已知限制（两个客户端共有）**：不支持文件中间随机写、原地 append、`flock`。
对"读取/执行/输出整文件"类负载（代码沙箱主场景）足够；sqlite、git 频繁增量写等强随机写负载不在本方案范围内。

## 配置变更

**`internal/config/config.go`：**

```go
type WorkspaceConfig struct {
    AutoSyncIntervalSeconds int    `mapstructure:"auto_sync_interval_seconds"`
    NodeFUSEMountBase       string `mapstructure:"node_fuse_mount_base"`
    SidecarMount            bool   `mapstructure:"sidecar_mount"`          // 开启 sidecar FUSE
    FUSEClientType          string `mapstructure:"fuse_client_type"`       // "geesefs"（默认，MinIO/S3）| "s3fs"（OSS/COS/OBS）
    MounterImage            string `mapstructure:"mounter_image"`          // mounter sidecar 镜像
    MounterSecretName       string `mapstructure:"mounter_secret_name"`    // 存储凭证 Secret 名称
}
```

> 凭证**不进 config 文件、不进 sandbox-api 进程、不进主容器**——只存在于 K8s Secret，
> 由 kubelet 直接注入 sidecar 容器环境变量。

**`configs/config.yaml` 新增：**

```yaml
workspace:
  sidecar_mount: true
  fuse_client_type: "geesefs"   # MinIO/S3 用 geesefs；阿里云 OSS / 腾讯云 COS / 华为云 OBS 用 "s3fs"
  mounter_image: "registry.example.com/sandbox-mounter:1.0"
  mounter_secret_name: "sandbox-storage-secret"
```

**存储凭证 Secret（集群级，一次性）：**

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: sandbox-storage-secret
  namespace: sandbox
stringData:
  # geesefs / s3fs 通用（1:1 明文对象）
  AWS_ACCESS_KEY_ID: "YOUR_ACCESS_KEY"
  AWS_SECRET_ACCESS_KEY: "YOUR_SECRET_KEY"
  S3_ENDPOINT: "https://oss-cn-hangzhou.aliyuncs.com"  # OSS/COS/OBS/MinIO 对应端点
  BUCKET: "your-bucket"
```

## 关键实现

### manager.go — buildSpec 填充 WorkspaceMounter

```go
func (m *Manager) buildSpec(id string, cfg SandboxConfig) runtime.SandboxSpec {
    spec := runtime.SandboxSpec{
        // ...现有字段...
    }
    if cfg.WorkspacePath != "" && m.config.Workspace.SidecarMount {
        spec.WorkspaceMounter = &runtime.WorkspaceMounterSpec{
            Image:      m.config.Workspace.MounterImage,
            SecretName: m.config.Workspace.MounterSecretName,
            SubPath:    cfg.WorkspacePath,
            ClientType: m.config.Workspace.FUSEClientType,
        }
    }
    return spec
}
```

### manager.go — Create：绕过 pool 并注册 workspace

`useBindMount` 旁边新增 `useSidecarFUSE`；两者都进 `direct` 分支（绕过 pool）：

```go
useBindMount := cfg.WorkspacePath != "" && m.fsMeta != nil && m.fsMeta.Provider == storage.ProviderLocal
useSidecarFUSE := cfg.WorkspacePath != "" && m.config.Workspace.SidecarMount && !useBindMount

if cfg.Network.Enabled || useBindMount || useSidecarFUSE {
    source = "direct"
    spec := m.buildSpec(id, cfg)
    if useBindMount {
        // ...现有 hostPath 逻辑...
    }
    // useSidecarFUSE 无需在此追加 Mounts：sidecar 由 spec.WorkspaceMounter 驱动
    info, err = m.runtime.CreateSandbox(spanCtx, spec) // 内部 waitForPodReady 已确保 sidecar ready
    ...
}
```

workspace 注册段（现有 `bindMounted` 分支旁）：sidecar FUSE 与 bind mount 一样**无需 copy 文件**，
复用 `registerWorkspace` 语义，仅标记 `FUSEMounted`：

```go
if cfg.WorkspacePath != "" {
    if bindMounted || useSidecarFUSE {
        if err := m.registerWorkspace(spanCtx, id, cfg.WorkspacePath); err != nil { /* cleanup */ }
        if useSidecarFUSE {
            m.mu.Lock()
            sb.Workspace.BindMounted = false
            sb.Workspace.FUSEMounted = true
            m.mu.Unlock()
        }
    } else {
        // ...现有 MountWorkspace（sync 模式）...
    }
}
```

### manager.go — Exec / ExecStream

**无改动。** exec 落在主容器，`/workspace` 已由 sidecar 透传就绪。
（这是相对 rootless 版删掉的一整块 `MountNSPid`/nsenter 逻辑。）

### manager.go — syncFromContainer

sidecar FUSE 与 bind mount 一样是"实时直写对象存储"，无需反向 sync。在现有 bind-mount 短路旁加一个条件：

```go
if sb.Workspace != nil && (sb.Workspace.BindMounted || sb.Workspace.FUSEMounted) {
    return nil // 数据已实时落对象存储，无需 sync
}
```

### pod.go — 注入 mount sidecar + mount propagation

在 `createPod` 组装 `pod` 之后、`Create` 之前插入：

```go
if spec.WorkspaceMounter != nil {
    always := corev1.ContainerRestartPolicyAlways
    bidirectional := corev1.MountPropagationBidirectional
    hostToContainer := corev1.MountPropagationHostToContainer
    priv := true
    unconfined := corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}

    // 主容器 /workspace 改为接收传播（VolumeMounts[0] 即 workspace）
    pod.Spec.Containers[0].VolumeMounts[0].MountPropagation = &hostToContainer

    pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
        Name:          "workspace-mounter",
        Image:         spec.WorkspaceMounter.Image,
        RestartPolicy: &always, // 原生 sidecar：先于主容器启动并常驻
        SecurityContext: &corev1.SecurityContext{
            Privileged:     &priv,        // Bidirectional 传播的硬性要求
            SeccompProfile: &unconfined,  // 放行 mount/umount2 等（仅此容器）
        },
        Env: []corev1.EnvVar{{Name: "WORKSPACE_SUBPATH", Value: spec.WorkspaceMounter.SubPath}},
        EnvFrom: []corev1.EnvFromSource{{
            SecretRef: &corev1.SecretEnvSource{
                LocalObjectReference: corev1.LocalObjectReference{Name: spec.WorkspaceMounter.SecretName},
            },
        }},
        Command: mounterCommand(spec.WorkspaceMounter.ClientType), // 见下
        VolumeMounts: []corev1.VolumeMount{{
            Name:             "workspace",
            MountPath:        "/workspace",
            MountPropagation: &bidirectional,
        }},
        ReadinessProbe: &corev1.Probe{
            ProbeHandler: corev1.ProbeHandler{
                Exec: &corev1.ExecAction{Command: []string{"mountpoint", "-q", "/workspace"}},
            },
            InitialDelaySeconds: 1, PeriodSeconds: 1, FailureThreshold: 30,
        },
    })
}
```

前台挂载命令（`-o allow_other` 使 uid 1000 的主容器可访问 root 挂载的 FUSE）。
`fuse_client_type` 决定客户端；两个分支都是 1:1 明文，文件 API 均兼容：

```go
func mounterCommand(clientType string) []string {
    switch clientType {
    case "s3fs":
        // 阿里云 OSS / 腾讯云 COS / 华为云 OBS 及其他兼容端点
        return []string{"/bin/sh", "-c",
            `echo "$AWS_ACCESS_KEY_ID:$AWS_SECRET_ACCESS_KEY" > /tmp/.passwd-s3fs \
             && chmod 600 /tmp/.passwd-s3fs \
             && exec s3fs "$BUCKET:/$WORKSPACE_SUBPATH" /workspace -f \
                -o allow_other \
                -o passwd_file=/tmp/.passwd-s3fs \
                -o url="$S3_ENDPOINT" \
                -o use_path_request_style`}
    default:
        // geesefs：MinIO / AWS S3 / 标准 S3 兼容端点（默认）
        return []string{"/bin/sh", "-c",
            `exec geesefs -f -o allow_other \
                --endpoint "$S3_ENDPOINT" --list-type 2 \
                "$BUCKET:$WORKSPACE_SUBPATH" /workspace`}
    }
}
```

> geesefs 无本地持久 cache，数据直写对象存储，不在 sidecar/节点留存。
> s3fs 默认同样无持久 cache；如开启 VFS cache，须通过独占 emptyDir 挂载到 sidecar，
> 勿与主容器共享，避免 untrusted 代码读到缓存内容。

### pod.go — 就绪保证

原生 sidecar（`restartPolicy=Always` 的 initContainer）会在其 `ReadinessProbe` 通过后才启动主容器，
因此 `waitForPodReady`（等 `PodRunning`）天然等价于"挂载已就绪"，无需额外轮询。

## 安全模型（对比 rootless 版）

| 维度 | rootless-fuse | **本方案（sidecar）** |
|------|---------------|----------------------|
| 凭证暴露给 untrusted 代码 | ❌ 经 `/proc/N/environ` 泄露 | ✅ 仅在独立 sidecar 容器，主容器读不到 |
| 主容器攻击面 | ❌ 放开 userns/mount/pivot_root | ✅ 不变（readonly rootfs / 非 root / RuntimeDefault seccomp） |
| 特权范围 | 主容器 | sidecar 容器（Pod 级，随 Pod 销毁） |
| 节点级常驻组件 | 无 | 无（满足约束 1） |
| PVC | 无 | 无（满足约束 2） |

**必须坚持的两条不变式：**

1. `shareProcessNamespace` 保持 false（默认）——否则 untrusted 主容器能看到 sidecar PID 并读取其环境变量，凭证隔离失效。
2. 存储 Secret 只 `envFrom` 到 sidecar 容器，**绝不**注入主容器；主容器保持现有 `KUBERNETES_*` 清空策略。

> 残余风险：mounter sidecar 是 privileged，若其镜像/FUSE 客户端本身被攻破可逃逸到节点。
> 缓解：sidecar 只运行我们构建的可信镜像 + 固定版本客户端，不含任何 untrusted 输入；
> 这与 CSI 的 mount pod 信任模型一致，区别是把它收敛进了单个 Pod、不做节点级常驻。

## 镜像要求

mounter sidecar 镜像（与主 sandbox 镜像可分离，保持主镜像精简）：

```dockerfile
FROM debian:bookworm-slim

# Docker BuildKit 多架构构建时自动注入 TARGETARCH（amd64 / arm64）
ARG GEESEFS_VERSION=0.43.8
ARG TARGETARCH

# s3fs 通过 apt 安装，Debian 仓库原生支持多架构，无需额外处理
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        fuse3 s3fs ca-certificates curl \
    && curl -sSfL \
        "https://github.com/yandex-cloud/geesefs/releases/download/v${GEESEFS_VERSION}/geesefs-linux-${TARGETARCH}" \
        -o /usr/local/bin/geesefs \
    && chmod +x /usr/local/bin/geesefs \
    && rm -rf /var/lib/apt/lists/*
# fuse3 已包含 mountpoint 命令，供 readinessProbe 使用
```

多架构镜像构建（推送到 registry 后 manifest 自动合并）：

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t registry.example.com/sandbox-mounter:1.0 \
  --push \
  -f deploy/mounter.Dockerfile .
```

> **注意**：GeeseFS 的 release binary 命名为 `geesefs-linux-amd64` / `geesefs-linux-arm64`，
> 与 Docker 的 `TARGETARCH` 值（`amd64` / `arm64`）直接对应，无需映射。
> 构建前确认 [releases 页面](https://github.com/yandex-cloud/geesefs/releases) 目标版本两个架构 binary 都存在。

主 sandbox 镜像**无需**任何 FUSE/util-linux 改动（挂载全在 sidecar）。

## 变更范围

| 文件 | 变更 |
|------|------|
| `internal/sandbox/types.go` | `WorkspaceInfo` 新增 `FUSEMounted` |
| `internal/runtime/types.go` | `SandboxSpec` 新增 `WorkspaceMounter *WorkspaceMounterSpec`；新增 `WorkspaceMounterSpec` 类型 |
| `internal/config/config.go` | `WorkspaceConfig` 新增 `SidecarMount`、`FUSEClientType`、`MounterImage`、`MounterSecretName` |
| `internal/sandbox/manager.go` | `buildSpec` 填充 `WorkspaceMounter`；`Create` 新增 `useSidecarFUSE` 走 direct 并 `registerWorkspace`；`syncFromContainer` 对 `FUSEMounted` 短路；restore 时重置 `FUSEMounted` |
| `internal/runtime/kubernetes/pod.go` | 注入 mounter sidecar、mount propagation、readinessProbe |
| `internal/runtime/kubernetes/exec.go` | **无改动** |
| `internal/sandbox/workspace.go` | **无改动**（复用 `registerWorkspace`） |
| `configs/config.yaml` | 新增 `workspace.sidecar_mount` 等配置项 |
| `deploy/storage-secret.yaml` | 新增 Secret 部署示例 |
| mounter 镜像 | 新增独立 Dockerfile |

## 错误处理

| 场景 | 处理 |
|------|------|
| mounter sidecar 挂载失败（凭证错/桶不存在） | readinessProbe 持续失败 → 主容器不启动 → `waitForPodReady` 超时 → sandbox 创建失败，Pod 删除 |
| 对象存储网络中断 | FUSE 层返回 IO 错误，用户代码收到文件系统错误，sandbox 本身不受影响 |
| Secret 不存在 | Pod 创建/启动失败，sandbox 返回 500 |
| sidecar 中途 OOM/退出 | `restartPolicy=Always` 触发重启并重挂；重挂窗口内主容器访问 `/workspace` 报 IO 错误 |
| API 重启，持久化 sandbox restore | Pod 仍在 ⇒ mount 仍存活，只需 `registerWorkspace` + `FUSEMounted=true`；Pod 已消失 ⇒ sandbox 失效 |

## 不在范围内

- Docker 运行时（无 Pod 概念，sidecar 传播模型不适用，需单独设计）
- 强随机写 / POSIX 完整语义负载（sqlite、git 频繁增量写等）——geesefs/s3fs 类客户端不适用，需另行评估
- mounter sidecar 去特权化（Bidirectional 传播目前强制要求 privileged）
