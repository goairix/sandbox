# Workspace 直接挂载：容器内 Rootless FUSE 设计

**日期：** 2026-06-28  
**状态：** 草案

## 背景

当前 workspace 通过 `SyncToContainer`/`SyncFromContainer` 在 sandbox-api 进程内中转所有文件，存在三个问题：

1. 文件量大时内存峰值约为文件总大小的 2 倍，可能触发 OOM
2. K8s 使用 emptyDir，workspace 数据驻留宿主机，多用户时宿主机磁盘快速打满
3. 写入文件需等下次 sync 才持久化，存在数据丢失窗口

本方案在**运行中的容器内**通过 Linux User Namespace + Mount Namespace（Rootless FUSE）挂载远端对象存储，数据完全不经过 sandbox-api。

### 约束

- Pool 容器必须可复用，workspace 挂载在 `Acquire` 后按需进行
- 不需要 workspace 的 sandbox 行为不变
- Docker 和 Kubernetes 运行时均需支持
- 节点无需预配置（K8s 需部署 fuse-device-plugin，见下文）

### 核心原理

`unshare --user --mount --map-root-user` 无需任何 capability 即可创建新的 user+mount namespace，在其中执行 FUSE mount。后续 exec 通过 `nsenter --mount=/proc/{fuse_pid}/ns/mnt` 进入该 namespace 访问 `/workspace`。容器仅需 `/dev/fuse` 设备访问权限（非 capability）。

`nsenter` 要求调用方与目标进程为同一 UID（或持有 `CAP_SYS_PTRACE`）。容器内所有 exec 均以相同非特权用户运行，满足此条件。

## 数据流

```
sandbox 创建（带 workspace_path）
    │
    ├─ pool.Acquire()（容器有 /dev/fuse，无 CAP_SYS_ADMIN）
    │
    ├─ Exec（Env: FUSE_META_URL, ACCESS_KEY, SECRET_KEY）:
    │        unshare --user --mount --map-root-user -- sh -c
    │        'juicefs mount "$FUSE_META_URL" /workspace --subdir={path} --background
    │         --pidfile=/tmp/.fuse.pid
    │         && timeout 5 sh -c "until [ -s /tmp/.fuse.pid ]; do sleep 0.1; done"
    │         && cat /tmp/.fuse.pid'
    │         → stdout: FUSE daemon PID = N
    │         凭证仅存在于进程环境变量，不写入任何文件
    │
    └─ Workspace.FUSEMounted=true, FUSEPid=N

后续 Exec/ExecStream（MountNSPid=N）
    └─ nsenter --mount=/proc/N/ns/mnt -- /bin/sh -c "{command}"

sandbox 销毁 → 容器销毁 → FUSE daemon 随进程树消失，自动 umount
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
    FUSEMounted  bool      `json:"fuse_mounted,omitempty"`
    FUSEPid      int       `json:"fuse_pid,omitempty"`
}
```

持久化模式下，`FUSEMounted`/`FUSEPid` 会随 sandbox state 写入 Redis。API 重启后 PID 已失效，restore 逻辑须将 `FUSEMounted` 清零并触发重挂载（见"错误处理"一节）。

**`internal/runtime/types.go` — ExecRequest 新增字段：**

```go
type ExecRequest struct {
    Command         string
    Stdin           string
    Timeout         int
    Env             map[string]string
    WorkDir         string
    LineBuffered    bool
    RequiresNetwork bool
    MountNSPid      int // 非零时前置 nsenter 进入该 PID 的 mount namespace
}
```

**`internal/runtime/types.go` — SandboxSpec 新增字段：**

```go
type SandboxSpec struct {
    // ...现有字段...
    FUSEDevice bool // 请求 /dev/fuse 设备访问权限
}
```

## 配置变更

**`internal/config/config.go`：**

```go
type WorkspaceConfig struct {
    AutoSyncIntervalSeconds int    `mapstructure:"auto_sync_interval_seconds"`
    NodeFUSEMountBase       string `mapstructure:"node_fuse_mount_base"`
    RootlessFUSE            bool   `mapstructure:"rootless_fuse"`
    FUSEClientType          string `mapstructure:"fuse_client_type"` // "juicefs"（目前仅支持此值）
    FUSEMetaURL             string `mapstructure:"fuse_meta_url"`    // juicefs 元数据 URL，密码通过环境变量注入
    FUSEAccessKey           string `mapstructure:"fuse_access_key"`  // S3/OSS 后端用，Redis 后端留空
    FUSESecretKey           string `mapstructure:"fuse_secret_key"`
}

type PoolConfig struct {
    // ...现有字段...
    FUSEDevice bool `mapstructure:"fuse_device"`
}
```

**`configs/config.yaml` 新增：**

```yaml
pool:
  fuse_device: true  # 与 workspace.rootless_fuse 同步开启

workspace:
  rootless_fuse: true
  fuse_client_type: "juicefs"
  # 密码通过环境变量注入：SANDBOX_WORKSPACE_FUSE_META_URL=redis://:${REDIS_PASSWORD}@redis:6379/1
  fuse_meta_url: "redis://redis:6379/1"
```

生产环境通过 Secret 注入 `SANDBOX_WORKSPACE_FUSE_META_URL` 环境变量以包含密码，不在配置文件中明文存储。

## 关键实现

### workspace.go — mountRootlessFUSE

凭证通过 `ExecRequest.Env` 注入进程环境变量，不写入任何文件：

```go
func (m *Manager) mountRootlessFUSE(ctx context.Context, sandboxID, workspacePath string) (int, error) {
    result, err := m.runtime.Exec(ctx, sandboxID, runtime.ExecRequest{
        Command: fmt.Sprintf(
            `unshare --user --mount --map-root-user -- sh -c `+
                `'juicefs mount "$FUSE_META_URL" /workspace --subdir=%s --background --pidfile=/tmp/.fuse.pid `+
                `&& timeout 5 sh -c "until [ -s /tmp/.fuse.pid ]; do sleep 0.1; done" `+
                `&& cat /tmp/.fuse.pid'`,
            workspacePath,
        ),
        Env:     m.buildFUSEEnv(),
        Timeout: 30,
    })
    if err != nil || result.ExitCode != 0 {
        return 0, fmt.Errorf("fuse mount failed (exit %d): %s", result.ExitCode, result.Stderr)
    }
    pid, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
    if err != nil || pid <= 0 {
        return 0, fmt.Errorf("invalid fuse pid %q: %w", result.Stdout, err)
    }
    return pid, nil
}

// buildFUSEEnv 将凭证组装为环境变量，不落盘
// JuiceFS 支持 FUSE_META_URL（Redis/TiKV）以及 ACCESS_KEY/SECRET_KEY（S3/OSS 对象存储后端）
func (m *Manager) buildFUSEEnv() map[string]string {
    env := map[string]string{
        "FUSE_META_URL": m.config.Workspace.FUSEMetaURL,
    }
    if m.config.Workspace.FUSEAccessKey != "" {
        env["ACCESS_KEY"] = m.config.Workspace.FUSEAccessKey
        env["SECRET_KEY"] = m.config.Workspace.FUSESecretKey
    }
    return env
}
```

### exec.go（kubernetes/docker 共用）— nsenter 注入

```go
func buildExecShellCmd(req runtime.ExecRequest) []string {
    shell := []string{"/bin/sh", "-c", req.Command}
    if req.MountNSPid > 0 {
        return append([]string{
            "nsenter", fmt.Sprintf("--mount=/proc/%d/ns/mnt", req.MountNSPid), "--",
        }, shell...)
    }
    return shell
}
```

### manager.go — Create 新增 FUSE 分支

```go
// workspace 处理：在现有 bindMounted / MountWorkspace 分支前插入
case m.config.Workspace.RootlessFUSE:
    pid, err := m.mountRootlessFUSE(spanCtx, id, cfg.WorkspacePath)
    if err != nil {
        _ = m.runtime.RemoveSandbox(spanCtx, info.RuntimeID)
        m.mu.Lock(); delete(m.sandboxes, id); m.mu.Unlock()
        return nil, fmt.Errorf("rootless fuse mount workspace: %w", err)
    }
    now := time.Now()
    m.mu.Lock()
    sb.Workspace = &WorkspaceInfo{RootPath: cfg.WorkspacePath, MountedAt: now,
        FUSEMounted: true, FUSEPid: pid}
    m.mu.Unlock()
```

### manager.go — Exec/ExecStream 注入 namespace

```go
if sb.Workspace != nil && sb.Workspace.FUSEMounted {
    req.MountNSPid = sb.Workspace.FUSEPid
}
```

### manager.go — pool buildSpec 中传递 FUSEDevice

`FUSEDevice` 来自 `PoolConfig`（Pool 预热时无 `SandboxConfig`），不从每次 `Create` 的 cfg 传入：

```go
// pool.go / manager.go buildSpec
func (m *Manager) buildSpec(id string, cfg SandboxConfig) runtime.SandboxSpec {
    spec := runtime.SandboxSpec{
        // ...现有字段...
        FUSEDevice: m.config.Pool.FUSEDevice, // 来自 PoolConfig，非 cfg
    }
    return spec
}
```

### pod.go — K8s /dev/fuse（fuse-device-plugin）

K8s 中 `/dev/fuse` 不能通过普通 HostPath VolumeMount 挂载为字符设备。需在集群中部署 [fuse-device-plugin](https://github.com/kuberenetes-learning-group/fuse-device-plugin)，通过资源请求方式获得设备访问权：

```go
if spec.FUSEDevice {
    pod.Spec.Containers[0].Resources.Limits[corev1.ResourceName("github.com/fuse")] =
        resource.MustParse("1")
}
```

部署 fuse-device-plugin（集群级一次性操作）：

```bash
kubectl apply -f https://raw.githubusercontent.com/kuberenetes-learning-group/fuse-device-plugin/master/fuse-device-plugin-k8s-1.16.yml
```

### docker.go — /dev/fuse 设备

```go
if spec.FUSEDevice {
    hostConfig.Devices = append(hostConfig.Devices, container.DeviceMapping{
        PathOnHost: "/dev/fuse", PathInContainer: "/dev/fuse", CgroupPermissions: "rwm",
    })
}
```

## seccomp profile 变更

Rootless FUSE 需要放行以下 syscall（在现有 seccomp profile 的 `syscalls.names` 中追加）：

```json
{
  "names": ["unshare", "mount", "umount2", "pivot_root"],
  "action": "SCMP_ACT_ALLOW"
}
```

> `clone` 已在默认 Docker/K8s seccomp profile 中放行（用于线程创建）；`CLONE_NEWUSER | CLONE_NEWNS` flags 无需单独放行，由 `unshare` 内部调用。

## 镜像要求

```dockerfile
# 锁定 juicefs 版本以保证 CLI flags 兼容性
ARG JUICEFS_VERSION=1.2.1
RUN apt-get install -y --no-install-recommends fuse3 util-linux \
    && curl -sSL "https://github.com/juicedata/juicefs/releases/download/v${JUICEFS_VERSION}/juicefs-${JUICEFS_VERSION}-linux-amd64.tar.gz" \
       | tar -xz -C /usr/local/bin juicefs \
    && rm -rf /var/lib/apt/lists/*
```

`util-linux` 提供 `unshare`/`nsenter`；`fuse3` 提供用户态 FUSE 支持。

## 变更范围

| 文件 | 变更 |
|------|------|
| `internal/sandbox/types.go` | `WorkspaceInfo` 新增 `FUSEMounted`、`FUSEPid` |
| `internal/runtime/types.go` | `ExecRequest` 新增 `MountNSPid`；`SandboxSpec` 新增 `FUSEDevice` |
| `internal/config/config.go` | `WorkspaceConfig` 新增5字段；`PoolConfig` 新增 `FUSEDevice` |
| `internal/sandbox/workspace.go` | 新增 `mountRootlessFUSE`、`buildFUSEEnv` |
| `internal/sandbox/manager.go` | `Create` 新增 FUSE 分支；`Exec`/`ExecStream` 注入 `MountNSPid`；`buildSpec` 从 `PoolConfig` 传 `FUSEDevice`；restore 时重置 `FUSEMounted` |
| `internal/sandbox/pool.go` | 无需额外改动（`FUSEDevice` 由 manager.buildSpec 统一处理） |
| `internal/runtime/kubernetes/pod.go` | `FUSEDevice=true` 时追加 fuse-device-plugin 资源请求 |
| `internal/runtime/kubernetes/exec.go` | `MountNSPid>0` 时前置 nsenter |
| `internal/runtime/docker/docker.go` | `FUSEDevice=true` 时追加 device mapping |
| `internal/runtime/docker/exec.go` | `MountNSPid>0` 时前置 nsenter |
| `configs/config.yaml` | 新增 pool/workspace FUSE 配置项 |
| seccomp profile | 放行 `unshare`、`mount`、`umount2`、`pivot_root` |

## 错误处理

| 场景 | 处理 |
|------|------|
| `mountRootlessFUSE` 超时（30s） | sandbox 创建失败，容器销毁，pool 补充新容器 |
| juicefs mount 非零退出 | 同上，stderr 作为错误详情返回 |
| pidfile 等待超时（5s） | 视同 mount 失败，同上 |
| FUSE daemon 意外退出 | `nsenter` 失败，Exec 返回 `ErrWorkspaceUnavailable`，API 返回 503 |
| `unshare` 被 seccomp 拦截（EPERM） | mountRootlessFUSE 失败，错误提示需在 seccomp profile 放行相关 syscall |
| 对象存储网络中断 | FUSE 层返回 IO 错误，用户代码收到文件系统错误，sandbox 不受影响 |
| API 重启，持久化 sandbox restore | restore 时检测到 `FUSEMounted=true`，pid 已失效；清零 `FUSEMounted`/`FUSEPid` 并调用 `mountRootlessFUSE` 重挂载；失败则将 sandbox 标记为 `Error` 状态 |

## 不在范围内

- FUSE daemon 退出后自动重挂载（运行中热恢复）
- 一个 sandbox 挂载多个 workspace
- s3fs 的 rootless 适配（其 rootless 支持有限）
