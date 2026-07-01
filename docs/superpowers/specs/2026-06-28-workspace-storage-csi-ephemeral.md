# Workspace 直接挂载：CSI Ephemeral Volume 设计

**日期：** 2026-06-28  
**状态：** 草案

## 背景

当前 workspace 同步通过 sandbox-api 进程中转所有文件数据，存在 OOM 风险、K8s 宿主机磁盘打满、实时性差等问题。

本方案使用 Kubernetes CSI Ephemeral Volume（内联卷，非 PVC）在 Pod 创建时声明存储，Volume 随 Pod 生命周期自动创建/销毁，数据不经过 sandbox-api，对节点扩容完全透明。

## 约束

- 仅适用于 Kubernetes 运行时
- 需要 workspace 的 sandbox 绕过 pool（与现有 network-enabled sandbox 逻辑一致）
- 需要在集群安装对应的 CSI Driver（一次性，由运维完成）

## CSI Ephemeral Volume vs PVC

| | PVC | CSI Ephemeral（本方案） |
|---|---|---|
| 创建时机 | 需预先申请 | Pod 创建时自动创建 |
| 删除时机 | 手动删除 | Pod 删除时自动清理 |
| 数据存储位置 | 取决于 StorageClass | Object Storage（不丢失） |
| 生命周期独立于 Pod | 是 | 否（但数据在对象存储中持久） |
| 适合 Pool | 否 | 绕过 pool 创建即可 |

## 架构

```
K8s Control Plane
└── CSI Controller Plugin（Deployment）
        ↓ gRPC
K8s Node
└── CSI Node Plugin（DaemonSet，K8s 自动部署）
        ↓ FUSE
    Sandbox Pod
    /workspace ←── CSI Ephemeral Volume → Object Storage
```

## 推荐 CSI Driver

| 存储后端 | CSI Driver |
|----------|-----------|
| 任意 S3/OSS/COS（推荐） | JuiceFS CSI（`csi.juicefs.com`） |
| AWS S3 | Mountpoint for Amazon S3 CSI |
| 阿里云 OSS | ACK OSSFS CSI（托管 K8s 自带） |
| 腾讯云 COS | TencentCloud CSI |

## 配置变更

**`internal/config/config.go`：**

```go
type WorkspaceConfig struct {
    AutoSyncIntervalSeconds int               `mapstructure:"auto_sync_interval_seconds"`
    NodeFUSEMountBase       string            `mapstructure:"node_fuse_mount_base"`
    RootlessFUSE            bool              `mapstructure:"rootless_fuse"`
    FUSEClientType          string            `mapstructure:"fuse_client_type"`
    FUSEMetaURL             string            `mapstructure:"fuse_meta_url"`
    CSIDriver               string            `mapstructure:"csi_driver"`       // e.g. "csi.juicefs.com"
    CSISecretName           string            `mapstructure:"csi_secret_name"`  // K8s Secret 名称
    CSIAttributes           map[string]string `mapstructure:"csi_attributes"`   // 额外 VolumeAttributes
}
```

**`configs/config.yaml` 新增：**

```yaml
workspace:
  csi_driver: "csi.juicefs.com"
  csi_secret_name: "sandbox-storage-secret"
  csi_attributes:
    juicefs/mount-cache-size: "100"
```

**存储凭证 Secret（集群级，一次性）：**

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: sandbox-storage-secret
  namespace: sandbox
stringData:
  metaurl: "redis://:password@redis:6379/1"
  access-key: "YOUR_ACCESS_KEY"
  secret-key: "YOUR_SECRET_KEY"
  bucket: "https://your-bucket.oss-cn-hangzhou.aliyuncs.com"
```

## 数据模型变更

**`internal/runtime/types.go` — SandboxSpec 新增字段：**

```go
type SandboxSpec struct {
    // ...现有字段...
    WorkspaceCSI *CSIVolumeSpec
}

type CSIVolumeSpec struct {
    Driver     string
    SecretName string
    SubPath    string            // workspace 隔离路径
    Attributes map[string]string
}
```

## 关键实现

**`internal/runtime/kubernetes/pod.go`** — workspaceVolume 新增 CSI 分支：

```go
if spec.WorkspaceCSI != nil {
    attrs := make(map[string]string, len(spec.WorkspaceCSI.Attributes)+1)
    for k, v := range spec.WorkspaceCSI.Attributes {
        attrs[k] = v
    }
    attrs["subPath"] = spec.WorkspaceCSI.SubPath
    workspaceVolume = corev1.VolumeSource{
        CSI: &corev1.CSIVolumeSource{
            Driver: spec.WorkspaceCSI.Driver,
            NodePublishSecretRef: &corev1.LocalObjectReference{
                Name: spec.WorkspaceCSI.SecretName,
            },
            VolumeAttributes: attrs,
        },
    }
}
```

**`internal/sandbox/manager.go`** — `buildSpec` 填充 CSI 信息：

```go
if cfg.WorkspacePath != "" && m.config.Workspace.CSIDriver != "" {
    spec.WorkspaceCSI = &runtime.CSIVolumeSpec{
        Driver:     m.config.Workspace.CSIDriver,
        SecretName: m.config.Workspace.CSISecretName,
        SubPath:    cfg.WorkspacePath,
        Attributes: m.config.Workspace.CSIAttributes,
    }
}
```

`useDirectCreate` 判断扩展（有 CSI 时也绕过 pool）：

```go
useDirectCreate := cfg.Network.Enabled || useBindMount ||
    (cfg.WorkspacePath != "" && m.config.Workspace.CSIDriver != "")
```

## 变更范围

| 文件 | 变更 |
|------|------|
| `internal/runtime/types.go` | `SandboxSpec` 新增 `WorkspaceCSI *CSIVolumeSpec`；新增 `CSIVolumeSpec` 类型 |
| `internal/config/config.go` | `WorkspaceConfig` 新增 `CSIDriver`、`CSISecretName`、`CSIAttributes` |
| `internal/runtime/kubernetes/pod.go` | `workspaceVolume` 新增 CSI 分支 |
| `internal/sandbox/manager.go` | `buildSpec` 填充 `WorkspaceCSI`；`useDirectCreate` 扩展 |
| `configs/config.yaml` | 新增 `workspace.csi_*` 配置项 |
| `deploy/storage-secret.yaml` | 新增 Secret 部署示例 |

## 错误处理

| 场景 | 处理 |
|------|------|
| CSI Driver 未安装 | Pod 创建失败，sandbox 返回 500 |
| Secret 不存在 | Pod 创建失败，sandbox 返回 500 |
| CSI mount 超时 | Pod 启动超时，sandbox 创建超时返回 |
| 对象存储网络中断 | FUSE 层返回 IO 错误，用户代码收到文件系统错误 |

## 不在范围内

- Docker 运行时（CSI 是 K8s 原生概念）
- CSI Driver 的安装和维护（由运维负责）
- StorageClass 和 PVC 方案
