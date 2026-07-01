# Workspace 直接挂载：节点级 FUSE + hostPath 设计

**日期：** 2026-06-28  
**状态：** 草案

## 背景

当前 workspace 同步通过 sandbox-api 进程中转所有文件数据，存在 OOM 风险、K8s 宿主机磁盘打满、实时性差等问题。

本方案在每个 K8s 节点上通过 DaemonSet 预挂载远端对象存储（JuiceFS/s3fs），sandbox pod 通过 `hostPath` 将节点上的 FUSE 挂载点子目录直接映射到容器 `/workspace`，数据不经过 sandbox-api。

## 约束

- 仅适用于 Kubernetes 运行时
- 节点扩容时 DaemonSet 自动调度，新节点就绪前无法调度 workspace sandbox（约 30-60s 窗口期）
- 需要 workspace 的 sandbox 绕过 pool（与现有 network-enabled sandbox 逻辑一致）

## 架构

```
                         K8s Node
    DaemonSet Pod        /mnt/sandbox-ws/           Object Storage
    (privileged，        └── workspaces/       ←───  OSS/COS/S3
     挂载 FUSE)               ├── user1/proj-a/
                              └── user2/proj-b/
    Sandbox Pod
    /workspace  ←── hostPath: /mnt/sandbox-ws/workspaces/{path}
    (无特殊权限)
```

## 部署组件

**FUSE 挂载 DaemonSet（集群级，一次性部署）：**

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: sandbox-fuse-mounter
spec:
  template:
    spec:
      containers:
      - name: mounter
        image: juicedata/juicefs-csi-driver:latest
        securityContext:
          privileged: true
        volumeMounts:
        - name: host-mount
          mountPath: /mnt/sandbox-ws
          mountPropagation: Bidirectional
        - name: fuse-device
          mountPath: /dev/fuse
      volumes:
      - name: host-mount
        hostPath: {path: /mnt/sandbox-ws, type: DirectoryOrCreate}
      - name: fuse-device
        hostPath: {path: /dev/fuse}
```

## 配置变更

**`internal/config/config.go`：**

```go
type WorkspaceConfig struct {
    AutoSyncIntervalSeconds int    `mapstructure:"auto_sync_interval_seconds"`
    NodeFUSEMountBase       string `mapstructure:"node_fuse_mount_base"` // 已有，本方案启用
}
```

**`configs/config.yaml`：**

```yaml
workspace:
  node_fuse_mount_base: "/mnt/sandbox-ws"
```

## 关键实现

`manager.go` 中 `useBindMount` 判断扩展——当配置了 `NodeFUSEMountBase` 时，非本地存储也走 bind mount 路径：

```go
nodeMount := m.config.Workspace.NodeFUSEMountBase != ""
useBindMount := cfg.WorkspacePath != "" && m.fsMeta != nil &&
    (m.fsMeta.Provider == storage.ProviderLocal || nodeMount)
```

`resolveLocalWorkspacePath` 扩展为通用的 host path 解析：

```go
func (m *Manager) resolveHostWorkspacePath(workspacePath string) string {
    if m.config.Workspace.NodeFUSEMountBase != "" {
        return filepath.Join(m.config.Workspace.NodeFUSEMountBase, "workspaces", workspacePath)
    }
    return m.resolveLocalWorkspacePath(workspacePath)
}
```

`buildSpec` 中将原来调用 `resolveLocalWorkspacePath` 替换为 `resolveHostWorkspacePath`。

`pod.go` 无需改动（已支持 `spec.Mounts` 中的 hostPath）。

## 变更范围

| 文件 | 变更 |
|------|------|
| `internal/config/config.go` | `WorkspaceConfig.NodeFUSEMountBase` 已有，确认文档一致 |
| `internal/sandbox/manager.go` | `useBindMount` 扩展判断；新增 `resolveHostWorkspacePath` |
| `configs/config.yaml` | 新增 `workspace.node_fuse_mount_base` 示例 |
| DaemonSet YAML | 新增部署文件 `deploy/fuse-mounter-daemonset.yaml` |

## 错误处理

| 场景 | 处理 |
|------|------|
| 新节点 DaemonSet 未就绪 | pod 调度失败或 hostPath 不存在，sandbox 创建返回 500 |
| FUSE 挂载断开 | 容器内 /workspace 返回 IO 错误，sandbox 不受影响 |
| 节点磁盘满 | FUSE 客户端写失败，用户代码收到文件系统错误 |

## 不在范围内

- Docker 运行时（Docker 已有本地 bind mount 方案）
- 节点自动安装 FUSE 工具（由节点镜像或初始化脚本保证）
- 多节点 FUSE 实现方式选型（JuiceFS/s3fs/rclone 均可，部署层决定）
