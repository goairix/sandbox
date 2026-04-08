# Sandbox 架构设计文档

## 1. 系统概述

Sandbox 是一个安全的、API 驱动的代码沙箱执行服务，基于 Go 构建。为运行不受信任的 Python、Node.js/TypeScript 和 Bash 代码提供隔离环境，支持细粒度的资源控制与网络隔离。

### 核心设计原则

- **语言无关容器** — 单一容器镜像同时支持 Python、Node.js、Bash，执行时指定语言
- **多运行时抽象** — 统一接口适配 Docker 和 Kubernetes，针对不同后端采用不同的网络隔离策略
- **安全优先** — 多层隔离（容器、资源、网络、文件系统），防御纵深设计
- **性能导向** — 预热容器池、增量工作空间同步、会话持久化

---

## 2. 系统架构

```
                           ┌──────────────────────────────┐
                           │           客户端              │
                           └──────────────┬───────────────┘
                                          │ HTTP/SSE
                           ┌──────────────▼───────────────┐
                           │         API 层 (Gin)          │
                           │  ┌─────────┐  ┌───────────┐  │
                           │  │ Auth    │  │ RateLimit │  │
                           │  │Middleware│  │Middleware │  │
                           │  └─────────┘  └───────────┘  │
                           │  ┌─────────────────────────┐  │
                           │  │       Handlers          │  │
                           │  │ Sandbox│Exec│File│WS   │  │
                           │  └────────┬────────────────┘  │
                           └───────────┼──────────────────┘
                                       │
                           ┌───────────▼──────────────────┐
                           │       Manager (核心)          │
                           │  ┌──────┐ ┌────────────────┐ │
                           │  │ Pool │ │   Workspace    │ │
                           │  └──┬───┘ │  (ScopedFS)    │ │
                           │     │     └───────┬────────┘ │
                           │  ┌──▼─────────────▼────────┐ │
                           │  │    SessionStore         │ │
                           │  │    (Redis)              │ │
                           │  └─────────────────────────┘ │
                           └───────────┬──────────────────┘
                                       │
                     ┌─────────────────┼─────────────────┐
                     │                 │                  │
          ┌──────────▼──────┐  ┌───────▼────────┐  ┌─────▼──────┐
          │  Runtime        │  │  Runtime        │  │  Storage   │
          │  (Docker)       │  │  (Kubernetes)   │  │  Backend   │
          │                 │  │                 │  │            │
          │ ┌─────────────┐ │  │ ┌────────────┐  │  │ Local/S3/  │
          │ │Gateway      │ │  │ │NetworkPol. │  │  │ COS/OSS/  │
          │ │Sidecar      │ │  │ │            │  │  │ OBS/MinIO │
          │ │(iptables)   │ │  │ └────────────┘  │  └────────────┘
          │ └─────────────┘ │  │                 │
          └─────────────────┘  └─────────────────┘
```

---

## 3. 项目结构

```
sandbox/
├── cmd/sandbox/main.go              # 启动入口，依赖注入
├── internal/
│   ├── api/                         # HTTP API 层
│   │   ├── server.go                # HTTP 服务器 + 优雅关闭
│   │   ├── router.go                # 路由注册
│   │   ├── handler/                 # 请求处理器
│   │   │   ├── sandbox.go           # 沙箱 CRUD
│   │   │   ├── exec.go              # 已有沙箱中执行代码
│   │   │   ├── execute.go           # 一次性执行（自动创建/销毁）
│   │   │   ├── file.go              # 文件上传/下载/列表
│   │   │   └── workspace.go         # 工作空间挂载/卸载/同步
│   │   └── middleware/
│   │       ├── auth.go              # API Key 认证（恒定时间比较）
│   │       └── ratelimit.go         # 令牌桶限流（按客户端 IP）
│   ├── config/config.go             # 配置加载（YAML + 环境变量）
│   ├── sandbox/                     # 核心编排层
│   │   ├── types.go                 # 领域类型定义
│   │   ├── manager.go               # 沙箱生命周期管理
│   │   ├── pool.go                  # 预热容器池
│   │   ├── session.go               # Redis 会话持久化
│   │   └── workspace.go             # 工作空间同步逻辑
│   ├── runtime/                     # 容器编排抽象
│   │   ├── runtime.go               # Runtime 接口定义
│   │   ├── types.go                 # 运行时类型
│   │   ├── docker/                  # Docker 实现
│   │   │   ├── runtime.go           # 生命周期管理
│   │   │   ├── network.go           # 网络隔离（Gateway Sidecar）
│   │   │   ├── exec.go              # 命令执行
│   │   │   ├── file.go              # 文件操作
│   │   │   └── container.go         # 容器创建
│   │   └── kubernetes/              # Kubernetes 实现
│   │       ├── runtime.go           # Pod 生命周期
│   │       ├── network.go           # NetworkPolicy
│   │       ├── exec.go              # Pod exec
│   │       └── file.go              # 文件传输
│   └── storage/                     # 存储抽象
│       ├── filesystem.go            # 多后端文件系统工厂
│       ├── scoped_fs.go             # 路径隔离（防目录逃逸）
│       └── state/
│           ├── state.go             # 状态存储接口
│           └── redis/store.go       # Redis 实现
├── pkg/types/                       # 公开 API 类型
├── docker/
│   ├── images/sandbox/Dockerfile    # 统一沙箱镜像
│   ├── images/gateway/Dockerfile    # 网络网关镜像
│   └── docker-compose.yml           # 本地开发环境
└── deploy/helm/                     # Kubernetes Helm Chart
```

---

## 4. 核心组件

### 4.1 Manager — 沙箱生命周期管理

Manager 是系统的中枢，协调沙箱的创建、执行、销毁全流程。

```go
type Manager struct {
    runtime    runtime.Runtime      // Docker 或 Kubernetes
    filesystem fs.FileSystem        // 文件存储后端
    sessions   *SessionStore        // Redis 会话（可选）
    pool       *Pool                // 预热容器池
    sandboxes  map[string]*Sandbox  // 内存中的沙箱注册表
    workspaces map[string]ScopedFS  // 已挂载的工作空间
}
```

**职责：**
- 从容器池分配/回收容器
- 安装依赖（pip/npm）
- 管理沙箱状态转换
- 工作空间挂载/卸载/同步
- 后台过期清理
- 启动时清理孤儿容器

#### 沙箱状态机

```
                    ┌──────────┐
                    │ Creating │
                    └────┬─────┘
                         │ 容器就绪
                    ┌────▼─────┐
              ┌────▶│  Ready   │
              │     └────┬─────┘
              │          │ 执行代码
              │     ┌────▼─────┐
              │     │ Running  │◀────┐
              │     └──┬───┬───┘     │
              │  成功 ─┘   └─ 失败   │
         ┌────▼─────┐   ┌────▼─────┐ │
         │   Idle   ├──▶│  Error   │ │
         └────┬─────┘   └────┬─────┘ │
              │ 再次执行      │ 重试   │
              └──────────────┴────────┘
                    │ 销毁
              ┌─────▼──────┐
              │ Destroying │
              └─────┬──────┘
              ┌─────▼──────┐
              │ Destroyed  │
              └────────────┘
```

- **Ready** → **Running**：收到 exec 请求
- **Running** → **Idle**：执行成功
- **Running** → **Error**：执行失败
- **Idle/Error** → **Running**：再次执行（Error 状态的容器仍可用，可重试）
- **Idle/Ready/Error** → **Destroying**：收到 destroy 请求或超时过期
- 任何状态 → **Error**：容器健康检查发现容器消失

#### 运行模式

| 模式 | 持久化 | 超时行为 | 适用场景 |
|------|--------|----------|----------|
| **ephemeral** | 不存 Redis | 超时自动销毁 | 一次性执行、短期调试 |
| **persistent** | 存 Redis，API 重启后可恢复 | 超时自动销毁（Redis TTL 同步过期） | 长期开发、工作空间协作 |

Ephemeral 沙箱销毁即消失；Persistent 沙箱的元数据（ID、RuntimeID、Config、Workspace 信息）序列化到 Redis，API 重启后 `Manager.Get()` 可从 Redis 恢复。两种模式共享同一个容器池。

#### 并发控制

Manager 使用 `sync.RWMutex` 保护 `sandboxes` 和 `workspaces` 两个内存 map：

```
读锁 (RLock):  Get、查找沙箱/工作空间、状态检查
写锁 (Lock):   Create 注册、Destroy 移除、状态变更

关键策略: 锁内只做 map 操作，锁外执行 Runtime API 调用
```

这确保了 Runtime 调用（可能耗时数秒）不会阻塞其他沙箱的操作。例如 `Exec()` 的加锁模式：

```go
m.mu.Lock()
sb.State = StateRunning       // 锁内：修改状态
runtimeID := sb.RuntimeID     // 锁内：拷贝需要的值
m.mu.Unlock()

result := m.runtime.Exec(runtimeID, req)  // 锁外：耗时操作

m.mu.Lock()
sb.State = StateIdle           // 锁内：更新状态
m.mu.Unlock()
```

### 4.2 Pool — 预热容器池

通过提前创建容器消除镜像拉取和容器启动的延迟。

```
┌────────────────────────────────────────┐
│                 Pool                    │
│  ┌──────┐ ┌──────┐ ┌──────┐           │
│  │warm-1│ │warm-2│ │warm-3│  ...       │
│  └──────┘ └──────┘ └──────┘           │
│                                        │
│  Acquire() → 取出 + 验证存活           │
│  NotifyRemoved() → 触发异步补充        │
│  refillIfNeeded() → 指数退避重试       │
└────────────────────────────────────────┘
```

**关键机制：**
- **存活验证**：`Acquire()` 取出容器后调用 `GetSandbox()` 确认容器未被 Docker 回收，stale 容器自动丢弃
- **异步补充**：容器被取走或销毁后，后台协程自动补充到 `MinSize`
- **指数退避**：补充失败时等待 1s → 2s → 4s → ... → 30s（上限），连续 10 次失败后放弃
- **孤儿清理**：Manager 启动时通过 `sandbox.pool=true` label 查找并移除上次遗留的池容器

### 4.3 SessionStore — 会话持久化

将 persistent 模式沙箱的元数据序列化为 JSON 存入 Redis，带 TTL 自动过期。

```
Redis Key: sandbox:<sandbox-id>
Value:     JSON(Sandbox{ID, Config, State, RuntimeID, Workspace, ...})
TTL:       sandbox_timeout_seconds
```

**恢复流程：**
1. `Manager.Get(id)` 先查内存 map
2. 未找到 → 回退到 `SessionStore.Load(id)`
3. 从 Redis 反序列化 → 注册回内存 map
4. 如有 Workspace 信息 → 重建 ScopedFS 实例

### 4.4 Workspace — 工作空间同步

工作空间在沙箱与后端存储之间同步文件，实现跨沙箱的文件持久化。

#### 同步方向

| 方向 | 场景 | 策略 |
|------|------|------|
| Storage → Container | 挂载时 | 全量打包为 tar，一次 API 上传 |
| Container → Storage | 同步/卸载/销毁时 | 增量同步，仅写入变更文件 |

#### 增量同步流程

增量同步的优化目标是**减少存储后端写入**（MinIO/S3 每次 PUT 都是一次 API 调用），而非减少容器端下载量。完整 tar 仍然从容器下载（Docker socket 本地传输，延迟极低），但只将变更文件写入存储。

```
1. Exec: find /workspace -printf '%P\t%Y\t%T@\n'
   → 获取容器内所有文件的路径、类型、修改时间
   → 开销: 1 次 Exec 调用

2. ScopedFS.List() 递归
   → 获取存储中所有文件路径
   → 开销: 每个目录层级 1 次 List 调用

3. 计算差异：
   - 变更文件 = 容器中 modtime > LastSyncedAt 的文件
   - 删除文件 = 存储中有但容器中没有的文件
   - 无变化 → 直接返回，跳过 tar 下载

4. DownloadDir(/workspace) → 完整 tar 流 (Docker socket, 本地传输)
   → 遍历 tar entries，仅解压 changedSet 中的文件到存储
   → 未变更文件: 跳过（不调用 ScopedFS.Create）

5. 删除存储中已移除的文件

6. 更新 LastSyncedAt 时间戳
```

**性能对比：**

| 场景 | 全量同步 (存储写入) | 增量同步 (存储写入) |
|------|---------------------|---------------------|
| 1000 文件，无变化 | 1000 次 PUT | 0 次 PUT |
| 1000 文件，改了 5 个 | 1000 次 PUT | 5 次 PUT |
| 首次同步 | N/A | 挂载即首次同步，后续全增量 |

**容错：** 如果 `find` 命令失败（容器异常），自动回退到全量同步。

---

## 5. 运行时抽象

### 5.1 Runtime 接口

```go
type Runtime interface {
    // 生命周期
    CreateSandbox(ctx, spec) (*SandboxInfo, error)
    RemoveSandbox(ctx, id) error
    GetSandbox(ctx, id) (*SandboxInfo, error)

    // 执行
    Exec(ctx, id, req) (*ExecResult, error)
    ExecStream(ctx, id, req) (<-chan StreamEvent, error)

    // 文件
    UploadFile / DownloadFile / UploadArchive / DownloadDir / ListFiles

    // 网络
    UpdateNetwork(ctx, id, enabled, whitelist) error

    // 管理
    RenameSandbox(ctx, id, name) error
    ListSandboxes(ctx, labels) ([]SandboxInfo, error)
}
```

### 5.2 Docker 实现 — Gateway Sidecar 网络隔离

Docker 环境下，沙箱容器以非 root 用户（uid 1000）运行，无法直接配置 iptables。通过 Gateway Sidecar 模式实现网络隔离：

```
                    ┌─────────────────────────────────────┐
                    │       sandbox-pair-<id> 网络         │
                    │           (internal)                 │
                    │                                     │
                    │  ┌──────────┐    ┌──────────────┐   │
                    │  │ Sandbox  │───▶│   Gateway    │   │
                    │  │ Container│    │  (Sidecar)   │   │
                    │  │ uid=1000 │    │  NET_ADMIN   │   │
                    │  └──────────┘    │  iptables    │   │
                    │                  └──────┬───────┘   │
                    └─────────────────────────┼───────────┘
                                              │
                    ┌─────────────────────────▼───────────┐
                    │       sandbox-open 网络              │
                    │        (external)                    │
                    │         → Internet                   │
                    └─────────────────────────────────────┘
```

**三种网络：**

| 网络 | 类型 | 用途 |
|------|------|------|
| `sandbox-isolated` | internal | 无网络沙箱共享，无外部连接 |
| `sandbox-open` | external | Gateway 出站桥接，可达外部 |
| `sandbox-pair-<id>` | internal | 每个网络沙箱独占，连接沙箱和 Gateway |

**Gateway 配置流程：**
1. 创建 pair 网络（internal, attachable）
2. 创建 Gateway 容器，赋予 `NET_ADMIN` + `NET_RAW` 能力
3. Gateway 同时连接 pair 网络和 open 网络
4. 在 Gateway 内执行 iptables 规则：
   - 启用 IP 转发
   - NAT 出站流量
   - 放行已建立连接
   - 按白名单放行目标地址
5. 设置沙箱默认路由指向 Gateway IP

**动态网络更新：**
- 启用网络：创建 Gateway pair，连接沙箱
- 更新白名单：flush + 重建 Gateway 内的 iptables 规则
- 禁用网络：断开沙箱与 pair 网络的连接，删除 Gateway 和 pair 网络

### 5.3 Kubernetes 实现 — NetworkPolicy 网络隔离

Kubernetes 环境下，直接利用原生 NetworkPolicy 实现出站流量控制：

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: sandbox-<id>
spec:
  podSelector:
    matchLabels:
      sandbox.id: <id>
  policyTypes: ["Egress"]
  egress:
    - to:
        - ipBlock:
            cidr: <resolved-whitelist-ip>/32
```

域名在策略创建时解析为 IP 地址。

---

## 6. 安全模型

### 6.1 隔离层次

```
┌────────────────────────────────────────────────┐
│ Layer 1: API 安全                               │
│   API Key 认证 (恒定时间比较)                    │
│   令牌桶限流 (按 IP)                            │
│   请求体大小限制 (64MB)                          │
├────────────────────────────────────────────────┤
│ Layer 2: 容器隔离                               │
│   非 root 用户 (uid 1000)                       │
│   Drop all capabilities                         │
│   可选只读根文件系统                             │
│   Seccomp 安全配置                              │
├────────────────────────────────────────────────┤
│ Layer 3: 资源限制                               │
│   内存: 可配置 (默认 256Mi)                      │
│   CPU: 可配置                                    │
│   PID: 可配置 (默认 100)                         │
│   磁盘: 可配置 (默认 100Mi)                      │
│   执行超时: 可配置 (默认 30s)                    │
│   沙箱生命周期: 可配置 (默认 3600s)              │
├────────────────────────────────────────────────┤
│ Layer 4: 网络隔离                               │
│   默认无网络访问                                 │
│   Docker: Gateway Sidecar + iptables 白名单      │
│   K8s: NetworkPolicy 出站规则                    │
├────────────────────────────────────────────────┤
│ Layer 5: 文件系统隔离                           │
│   ScopedFS: 保护存储后端不被越界访问             │
│   文件 API: 限制容器内操作路径                   │
│   容器隔离: 容器内代码无法影响宿主机             │
└────────────────────────────────────────────────┘
```

#### Layer 5 详解：三层文件系统保护边界

文件系统的安全由三个独立机制分别守护不同的攻击面：

```
┌─────────────────────────────────────────────────────────────┐
│ 攻击面 1: 存储后端（MinIO/S3/本地磁盘）                      │
│ 保护者: ScopedFS                                            │
│                                                             │
│ 风险: workspace sync 时路径遍历，读写其他用户的存储文件       │
│ 防护: resolvePath() 拒绝绝对路径、.. 遍历、前缀不匹配的路径  │
│ 效果: 每个 workspace 只能访问自己的 rootPath 下的文件         │
│                                                             │
│ 示例:                                                       │
│   rootPath = "user123/project-a"                            │
│   ✓ "src/main.py"        → user123/project-a/src/main.py   │
│   ✗ "../user456/secret"  → ErrPathEscaped                  │
│   ✗ "/etc/passwd"        → ErrPathEscaped                  │
├─────────────────────────────────────────────────────────────┤
│ 攻击面 2: 容器内文件系统（通过文件 API）                      │
│ 保护者: validateSandboxPath()                               │
│                                                             │
│ 风险: 通过文件上传/下载 API 读写容器内 /workspace 以外的文件   │
│ 防护: 路径必须以 /workspace/ 或 /tmp/ 开头，不含 ..          │
│ 效果: API 调用方只能操作 /workspace/ 和 /tmp/ 目录            │
├─────────────────────────────────────────────────────────────┤
│ 攻击面 3: 容器内文件系统（通过代码执行）                      │
│ 保护者: 容器隔离本身                                        │
│                                                             │
│ 容器内执行的代码可以自由访问容器的整个文件系统，这是预期行为。  │
│ 容器内 /workspace 以外的文件只是运行时基础文件（python、node  │
│ 等二进制和库），不包含用户敏感数据。安全由容器隔离保证：       │
│   - 非 root (uid 1000): 无法写系统目录、无法提权             │
│   - Drop all capabilities: 无法挂载、加载内核模块            │
│   - 网络隔离: 无法将容器内文件外传（除非显式开启网络）        │
│   - 容器边界: 代码看到的 / 是容器根，不是宿主机根             │
└─────────────────────────────────────────────────────────────┘
```

**关键区分：** ScopedFS 不保护容器内部——它保护的是 API 服务端的存储后端。容器内的文件系统隔离由 Docker/Kubernetes 的容器机制保证。

### 6.2 依赖安装安全

```go
var validDepRegexp = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
```

- 包名和版本号仅允许字母数字 + 点/横线/下划线
- 防止通过依赖名称注入 shell 命令（如 `; rm -rf /`）
- 无效依赖跳过并记录 WARNING 日志

### 6.3 白名单域名解析

域名在容器创建时解析为 IP 地址并写入 iptables 规则，防止运行时通过 DNS 变更绕过白名单。

---

## 7. 关键数据流

### 7.1 创建沙箱

```
客户端 → POST /api/v1/sandboxes {mode: "persistent"}
  │
  ▼
Handler: 校验请求参数
  │
  ▼
Manager.Create():
  ├─ 生成 ID: sandbox-<10位随机字符>
  ├─ 无网络 → Pool.Acquire() → 取预热容器（验证存活）
  │  有网络 → Runtime.CreateSandbox() → 创建容器 + Gateway Sidecar
  ├─ RenameSandbox() → 容器名改为沙箱 ID
  ├─ 有依赖 → buildInstallCommand() → Exec 安装 (pip/npm)
  ├─ 注册到内存 map
  ├─ persistent 模式 → SessionStore.Save()
  └─ 有 workspace_path → MountWorkspace()
  │
  ▼
返回: {id, mode, state, runtime_id, created_at}
```

### 7.2 一次性执行（One-shot）

```
客户端 → POST /api/v1/execute {language: "python", code: "print(42)"}
  │
  ▼
Handler.ExecuteOneShot():
  ├─ 校验 language
  ├─ Manager.Create(mode=ephemeral) → 从池取容器
  ├─ defer Manager.Destroy(context.WithoutCancel) → 确保清理
  │   （即使客户端断开，Destroy 也会执行）
  ├─ buildCommand(language, code)
  └─ Manager.Exec(id, command)
  │
  ▼
返回: {exit_code, stdout, stderr, duration}
  │
  ▼
defer 触发 → Destroy → 移除容器 → Pool.NotifyRemoved() → 异步补充
```

与持久化沙箱的区别：不注册到 SessionStore，执行完即销毁，全程自动。

### 7.3 执行代码（已有沙箱）

```
客户端 → POST /api/v1/sandboxes/<id>/exec {language: "python", code: "..."}
  │
  ▼
Handler:
  ├─ 校验 language (python|nodejs|bash)
  └─ buildCommand():
       python → python3 <<'SANDBOX_EOF'\n<code>\nSANDBOX_EOF
       nodejs → node <<'SANDBOX_EOF'\n<code>\nSANDBOX_EOF
       bash   → <code> (直接执行)
  │
  ▼
Manager.Exec():
  ├─ 状态: Ready/Idle → Running
  └─ Runtime.Exec(runtimeID, command)
       ├─ 超时控制: context.WithTimeout
       ├─ Docker: ContainerExecCreate → Attach → 读 stdout/stderr → Inspect 获取 exit code
       └─ K8s: Pod exec via SPDY
  │
  ▼
  ├─ 成功 → 状态: Running → Idle
  └─ 失败 → 状态: Running → Error
  │
  ▼
返回: {exit_code, stdout, stderr, duration}
```

### 7.4 工作空间挂载与同步

```
挂载: POST /workspace/mount {root_path: "user123/project-a"}
  │
  ▼
  NewScopedFS(filesystem, rootPath) → 路径隔离
  │
  ▼
  syncToContainer():
    ├─ 递归遍历 ScopedFS
    ├─ 构建 tar (uid=1000, gid=1000)
    └─ UploadArchive → /workspace
  │
  ▼
  设置 LastSyncedAt = now()
  │
  ▼
  SessionStore.Save()

同步: POST /workspace/sync {direction: "from_container"}
  │
  ▼
  syncFromContainer():
    ├─ Exec: find /workspace → 容器文件清单 (path, modtime)
    ├─ ScopedFS.List() → 存储文件清单
    ├─ 差异计算:
    │   变更 = modtime > LastSyncedAt
    │   删除 = 存储有, 容器无
    ├─ DownloadDir → tar → 选择性解压变更文件
    ├─ Remove 已删除文件
    └─ 更新 LastSyncedAt
```

### 7.5 销毁沙箱

```
客户端 → DELETE /api/v1/sandboxes/<id>
  │
  ▼
Manager.Destroy(id):
  ├─ 写锁: 设置状态 Destroying, 从 sandboxes map 移除
  ├─ SessionStore.Remove(id)
  ├─ 如有工作空间:
  │   ├─ syncFromContainer() → best-effort 增量同步回存储
  │   └─ 从 workspaces map 移除
  ├─ Runtime.RemoveSandbox(runtimeID):
  │   ├─ Docker: ContainerRemove(force=true)
  │   └─ 如有网络: removeSandboxPair() → 删除 Gateway + pair 网络
  └─ Pool.NotifyRemoved() → 触发异步补充
  │
  ▼
返回: {"message": "sandbox destroyed"}
```

销毁时的 workspace 同步是 best-effort：即使同步失败，容器仍会被移除，不会因为同步错误阻塞销毁。

---

## 8. 启动与关闭

### 8.1 启动流程

```
main():
  1. 加载配置 (YAML + 环境变量)
  2. 初始化 Runtime:
     Docker → 确保网络存在, 清理孤儿 Gateway/网络
     K8s → 建立集群连接
  3. 初始化文件系统 (Local/S3/MinIO/...)
  4. 创建 Manager + Pool
  5. 连接 Redis → 创建 SessionStore → 注入 Manager
  6. Manager.Start():
     a. 清理孤儿池容器 (label: sandbox.pool=true)
     b. Pool.WarmUp() → 创建 MinSize 个预热容器
     c. 启动后台过期清理协程 (每 10 秒)
  7. 启动 HTTP 服务
  8. 注册信号处理 (SIGINT/SIGTERM)
```

### 8.2 关闭流程

```
收到 SIGINT/SIGTERM:
  1. 取消全局 context
  2. Manager.Stop():
     a. 关闭 stopCh → 停止过期清理协程
     b. 等待协程退出
     c. Pool.Drain() → 移除所有预热容器
  3. HTTP Server.Stop() (10 秒超时)
```

### 8.3 崩溃恢复

| 场景 | 影响 | 恢复机制 |
|------|------|----------|
| API 优雅关闭 | 池容器被 Drain | 重启后重新 WarmUp |
| API 崩溃/SIGKILL | 池容器成为孤儿 | 重启时 `cleanupOrphanedPoolContainers()` 通过 label 清理 |
| Docker 重启 | 池中容器消失 | `Pool.Acquire()` 检测 stale 容器并丢弃，按需创建 |
| Persistent 沙箱恢复 | 内存 map 清空 | `Manager.Get()` 从 Redis 加载并重建 |

---

## 9. 存储层

### 9.1 文件系统工厂

```go
func NewFileSystem(cfg FileSystemConfig) (fs.FileSystem, error) {
    switch cfg.Provider {
    case "local": return local.New(cfg.LocalPath)
    case "s3":    return s3.New(cfg.Bucket, cfg.Region, cfg.AccessKey, cfg.SecretKey)
    case "minio": return minio.New(cfg.Endpoint, cfg.Bucket, cfg.AccessKey, cfg.SecretKey)
    // cos, oss, obs...
    }
}
```

#### 工作空间与存储后端的关系

工作空间文件存储在存储后端中，通过 tar 归档在存储后端和容器之间复制——**不是 Docker volume mount**。

```
存储后端                                     容器
(Local/S3/MinIO/...)                        /workspace/
└── user123/project-a/      ── tar ──▶      ├── main.py
    ├── main.py             (挂载时上传)     └── data/
    └── data/                                   └── input.csv
        └── input.csv
                            ◀── tar ──      修改后的文件
                            (增量同步回存储)  写回存储后端
```

无论底层是 Local、S3 还是 MinIO，上层 ScopedFS 和 sync 逻辑完全一致，差异仅在 `fs.FileSystem` 的 I/O 实现。

#### 存储后端选型注意事项

| 后端 | 适用场景 | 限制 |
|------|----------|------|
| **local** | 开发调试 | 文件在 API 进程所在主机的磁盘上；Docker Compose 下 API 跑在容器内，需额外挂载宿主机目录才能持久化；不支持多副本共享 |
| **s3/cos/oss/obs** | 生产环境 | 需要配置 Bucket、AccessKey 等；天然支持多副本共享 |
| **minio** | 自建对象存储 | 兼容 S3 协议；适合不依赖云厂商的场景 |

**Local 模式注意：** API 进程重启不影响文件（在磁盘上），但 API 容器重建会丢失数据（容器内临时存储）。生产环境应使用对象存储后端。

ScopedFS 的保护对象是**存储后端**（MinIO/S3/本地磁盘），不是容器内文件系统。它确保 workspace sync 操作不会读写 `rootPath` 以外的存储文件。

```
存储后端根目录
└── sub_path ("workspaces")           ← 配置级
    ├── user123/proj-a/               ← ScopedFS A 的 rootPath
    │   ├── main.py                   ✓ A 可访问
    │   └── data/input.csv            ✓ A 可访问
    └── user456/proj-b/               ← ScopedFS B 的 rootPath
        └── secret.key                ✗ A 不可访问（不同 rootPath）

ScopedFS A 尝试 "../../user456/proj-b/secret.key" → ErrPathEscaped
```

所有路径操作经过 `resolvePath()` 校验，确保解析后的路径不超出 rootPath 范围。

### 9.3 State Store

```go
type Store interface {
    Set(ctx, key, value, ttl) error
    Get(ctx, key) ([]byte, error)
    Delete(ctx, key) error
    Exists(ctx, key) (bool, error)
    SetNX(ctx, key, value, ttl) (bool, error)  // 分布式锁
    Keys(ctx, pattern) ([]string, error)
}
```

Redis 实现使用 SCAN 迭代器进行模式匹配，避免 KEYS 命令在大数据集上阻塞。

---

## 10. 部署

### 10.1 Docker Compose（开发环境）

```bash
cd docker
cp .env.example .env
docker-compose up -d
```

服务组成：
- `sandbox-api` — API 服务，挂载 Docker socket
- `sandbox-images` — 构建沙箱 + Gateway 镜像
- `redis` — 会话持久化

### 10.2 Kubernetes + Helm（生产环境）

**基础部署（所有资源在同一 namespace）：**

```bash
helm install sandbox deploy/helm/sandbox/ \
  -n sandbox --create-namespace \
  --set config.security.apiKey=your-key \
  --set config.images.sandbox=your-registry/sandbox:latest
```

**自定义 sandbox pod namespace（API 与 sandbox pods 分离）：**

```bash
helm install sandbox deploy/helm/sandbox/ \
  -n sandbox --create-namespace \
  --set config.runtime.kubernetes.namespace=sandbox-runners \
  --set config.security.apiKey=your-key
```

此配置下：
- API 服务部署在 `sandbox` namespace
- 动态创建的沙箱 pod 运行在 `sandbox-runners` namespace
- RBAC 自动配置跨命名空间权限（Role 在 `sandbox-runners`，ServiceAccount 在 `sandbox`）

**Namespace 规则：**
- `config.runtime.kubernetes.namespace` 为空时（默认），sandbox pods 与 Helm release 在同一 namespace
- 设置后，sandbox pods 限定在指定 namespace，API 可以在不同 namespace

特性：
- 多副本部署（共享 Redis 状态）
- HPA 自动伸缩（基于 CPU）
- NetworkPolicy 出站控制
- RBAC 授权 ServiceAccount 管理 pods、pods/exec、NetworkPolicies
- Pod 安全标准

---

## 11. 关键设计决策

### 为什么用 Gateway Sidecar 而不是直接配置 iptables？

沙箱容器以 uid 1000 运行，无权操作 iptables。Sidecar 以 `NET_ADMIN` 能力运行，独立于沙箱容器，保证沙箱安全性的同时实现网络过滤。

### 为什么从多语言池改为单一池？

统一镜像预装 Python + Node.js + Bash，语言在执行时指定。单一池简化了管理，提高了资源利用率，不需要为每种语言维护独立的预热容器。

### 为什么工作空间同步是增量的？

大型工作空间全量同步代价高。通过比较容器内文件修改时间与 `LastSyncedAt` 时间戳，只写入变更文件。对 MinIO 等对象存储来说，每次 PUT 都是一次 API 调用，减少不必要的写入显著降低延迟和成本。

### 为什么用 `context.WithoutCancel` 做清理？

一次性执行模式下，客户端可能在流式响应中途断开。`context.WithoutCancel` 确保 Destroy 不受父 context 取消影响，避免容器和网络资源泄漏。

### 为什么 Pool.Acquire 要验证容器存活？

Docker 可能重启或容器被外部移除，Pool 中的容器 ID 过时。取出时验证存活状态，stale 容器自动丢弃，避免后续操作失败。
