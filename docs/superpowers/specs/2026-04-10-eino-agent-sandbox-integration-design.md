# Eino Agent 与 Sandbox 集成设计

## 概述

让 AI 中台中基于 Eino 框架实现的 Agent，通过 HTTP API 将 Sandbox 当作"虚拟机"使用：workspace 目录作为 Agent 的工作目录，Agent 可自由读写文件、执行代码、加载 Skills。

## 背景与约束

- Eino Agent 运行在 AI 中台服务中，不能直接访问 Sandbox 容器文件系统，只能通过 HTTP API 交互
- 1 个 Agent 会话 = 1 个 Persistent Sandbox 实例
- Agent 身份文件（IDENTITY.md、SOUL.md）来源于中台数据库，InitSession 时转为文件推送到 Sandbox
- Skills 文件来源于独立目录，推送到 Sandbox 后只读，不支持在沙盒内修改
- Agent 产生的文件写入 `/workspace`，会话结束时同步回对象存储
- Agent 可修改身份定义文件，修改后需回写中台数据库
- Sandbox 服务端尽量少改动
- **中台 Agent 是 HTTP 请求驱动的**：用户发消息 → 中台收到 HTTP 请求 → 启动 ReAct 循环（可能调用多轮 Tool）→ 返回响应 → 进程结束。无长驻进程、无 WebSocket
- **用户可能随时消失**：浏览器关闭、网络断开、任务完成后不触发 CloseSession，均属正常场景。不能依赖用户侧的主动关闭

## 架构

### 三层结构

```
┌───────────────────────────────────────────────────────┐
│ Eino Agent (ReAct 循环)                                │
│  Tool: read_file / write_file / list_directory /      │
│        search_files / delete_file /                    │
│        run_code / run_code_stream / install_packages / │
│        load_skill / list_skills / run_skill            │
├───────────────────────────────────────────────────────┤
│ Sandbox Go SDK (中台 pkg 公共仓库)                      │
│  纯 HTTP Client 封装，零外部依赖，类型定义自包含         │
├───────────────────────────────────────────────────────┤
│ Sandbox Service (现有服务，仅 SyncWorkspace 加 exclude) │
│  容器生命周期 / 代码执行 / 文件操作 / 工作空间同步       │
└───────────────────────────────────────────────────────┘
```

### workspace 目录结构

```
/workspace/
├── .agent/                  ← 排除在 workspace 同步之外
│   ├── IDENTITY.md          ← 来自数据库，Agent 可修改，修改后回写数据库
│   ├── SOUL.md              ← 同上
│   └── skills/              ← 来自独立目录，只读，不同步
│       └── ...
├── ...                      ← 项目文件，正常 workspace 同步
```

## 模块设计

### 1. Sandbox Go SDK

放置在中台 pkg 公共仓库中，封装 Sandbox 全部 HTTP API 为类型安全的 Go 方法调用。

```go
type Client struct {
    baseURL string
    apiKey  string
    http    *http.Client
}

// 生命周期
func (c *Client) Create(ctx context.Context, req CreateSandboxRequest) (*SandboxResponse, error)
func (c *Client) Get(ctx context.Context, id string) (*SandboxResponse, error)
func (c *Client) Destroy(ctx context.Context, id string) error

// 执行
func (c *Client) Exec(ctx context.Context, id string, req ExecRequest) (*ExecResponse, error)
func (c *Client) ExecStream(ctx context.Context, id string, req ExecRequest) (<-chan SSEEvent, error)

// 文件
func (c *Client) UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error
func (c *Client) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error)
func (c *Client) ListFiles(ctx context.Context, id string, dirPath string) (*FileListResponse, error)

// 工作空间
func (c *Client) MountWorkspace(ctx context.Context, id string, rootPath string) (*MountWorkspaceResponse, error)
func (c *Client) UnmountWorkspace(ctx context.Context, id string) error
func (c *Client) SyncWorkspace(ctx context.Context, id string, req SyncWorkspaceRequest) error

// 网络
func (c *Client) UpdateNetwork(ctx context.Context, id string, req UpdateNetworkRequest) error
```

依赖：仅标准库 `net/http`、`encoding/json`、`io`、`context`。请求/响应类型在 SDK 包内自行定义（从 Sandbox 的 `pkg/types` 复制，避免引入 sandbox module）。

### 2. Eino Tool 层

基于 SDK 封装为 Eino Tool，供 Agent 在 ReAct 循环中调用。

#### 文件操作类

| Tool | 功能 | SDK 方法 |
|------|------|----------|
| `read_file` | 读取 workspace 中的文件内容 | `DownloadFile` → 读内容返回 string |
| `write_file` | 写入/创建文件到 workspace | `UploadFile` |
| `list_directory` | 列出目录内容 | `ListFiles` |
| `search_files` | 搜索文件内容 | `Exec` 执行 `grep -rn` |
| `delete_file` | 删除文件或目录 | `Exec` 执行 `rm` |

#### 代码执行类

| Tool | 功能 | SDK 方法 |
|------|------|----------|
| `run_code` | 执行 Python/Node.js/Bash 代码 | `Exec` |
| `run_code_stream` | 流式执行代码 | `ExecStream` |
| `install_packages` | 安装 pip/npm 依赖 | `Exec` 执行 pip/npm install |

#### Skill 管理类

| Tool | 功能 | 实现方式 |
|------|------|----------|
| `list_skills` | 列出可用 Skills | `ListFiles` 扫描 `/workspace/.agent/skills/` |
| `load_skill` | 读取 Skill 定义内容 | `DownloadFile` 读取 skill 文件 → 返回内容给 Agent |
| `run_skill` | 执行代码类 Skill | 读取 skill → 提取代码部分 → `Exec` 执行（见 Skill 文件格式） |

Skills 只读由 Tool 层保证：不提供写入 `.agent/skills/` 目录的 Tool。

#### Tool 层路径安全

所有文件操作类 Tool 在调用 SDK 前，统一做路径校验：

```go
func validatePath(targetPath string, allowRead bool) error {
    // 1. 解析为绝对路径，消除 ../ 和符号链接
    abs := filepath.Clean("/workspace/" + targetPath)

    // 2. 必须在 /workspace/ 下
    if !strings.HasPrefix(abs, "/workspace/") {
        return ErrPathOutOfBounds
    }

    // 3. .agent/ 目录保护
    if strings.HasPrefix(abs, "/workspace/.agent/") {
        if allowRead {
            return nil  // read_file / search_files 允许读
        }
        return ErrAgentDirProtected  // write/delete 禁止
    }

    return nil
}
```

各 Tool 的访问权限：

| Tool | .agent/ 读 | .agent/ 写/删 | /workspace 外 |
|------|-----------|--------------|--------------|
| `read_file` | 允许 | — | 禁止 |
| `search_files` | 允许 | — | 禁止 |
| `write_file` | — | 禁止 | 禁止 |
| `delete_file` | — | 禁止 | 禁止 |
| `run_code` | 不校验（代码在容器内执行，靠容器沙箱隔离） | — | — |

### 3. Skill 文件格式

Skill 文件为 Markdown 格式，包含 frontmatter 元数据和一个可执行代码块：

```markdown
---
name: data-analysis
description: 对 workspace 中的 CSV 数据进行分析
language: python
---

# 数据分析 Skill

对指定 CSV 文件进行基础统计分析。

```python
import pandas as pd
import sys

file_path = sys.argv[1]
df = pd.read_csv(file_path)
print(df.describe())
```
```

**解析规则**：

1. 读取 frontmatter 中的 `language` 字段，确定执行语言（`python` / `nodejs` / `bash`）
2. 提取文件中 **第一个** 与 `language` 匹配的 fenced code block
3. 如果没有 `language` 字段或没有匹配的代码块 → 返回错误，不执行
4. 多个代码块只取第一个，忽略其余

**run_skill 执行流程**：

```
1. DownloadFile 读取 skill 文件内容
2. 解析 frontmatter → 取 language
3. 正则提取第一个匹配 language 的 code block
4. Exec(language, code, args) → 返回 stdout/stderr
```

**约束**：
- Skill 文件只读，Tool 层不提供写入 `.agent/skills/` 的能力
- `run_skill` 标记 `Dirty=true`（代码可能写文件）
- Skill 代码在容器内执行，安全性由容器沙箱保证

### 4. 会话管理（中台内部，不暴露给 Agent）

#### InitSession

```
1. client.Create(mode=persistent, workspace_path="user/project-x")
   → 创建沙箱 + 挂载 workspace（项目文件从对象存储同步到容器）
2. 记录 session 到 Redis/DB：
   session_id, sandbox_id, workspace_path, last_active_at, last_synced_version, status=active
3. client.UploadFile → /workspace/.agent/IDENTITY.md  (从数据库读取，转为文件)
4. client.UploadFile → /workspace/.agent/SOUL.md      (同上)
5. client.UploadFile → /workspace/.agent/skills/*      (从独立目录读取，逐个推送)
6. 注入 Eino Tool，Agent 开始运行
```

#### CloseSession（best-effort 容错）

各步骤独立执行，某步失败不阻塞后续步骤，错误聚合返回并记录日志。Destroy 必须执行，防止资源泄漏。

```go
func CloseSession(sessionID string) error {
    var errs []error

    // 步骤 1: 回写身份文件（独立）
    err1 := syncIdentityFiles(sessionID)
    if err1 != nil { errs = append(errs, err1) }

    // 步骤 2: 同步 workspace（独立）
    err2 := syncWorkspace(sessionID)
    if err2 != nil { errs = append(errs, err2) }

    // 步骤 3: 销毁容器（必须执行，防止资源泄漏）
    err3 := destroySandbox(sessionID)
    if err3 != nil { errs = append(errs, err3) }

    // 步骤 4: 标记 session closed
    markSessionClosed(sessionID)

    return errors.Join(errs...)
}
```

#### 会话清理（应对用户消失）

中台 Agent 是 HTTP 驱动的，无长驻进程，不能依赖心跳或 lease 续约。用户可能随时关闭浏览器，不触发 CloseSession。

**两层清理机制**：

| 层 | 触发方 | 时机 | 行为 |
|---|---|---|---|
| 主动清理 | 中台 cron worker | 定期扫描（如每 60s）session 表，找到 `last_active_at` 超过阈值（如 30min）的 session | 执行 best-effort CloseSession |
| 兜底清理 | Sandbox reaper | 容器 idle timeout | 销毁容器 |

**session 记录结构**：

```
session_id          — 会话唯一标识
sandbox_id          — 对应的 Sandbox 容器 ID
workspace_path      — 挂载的 workspace 路径
last_active_at      — 最后一次 HTTP 请求时间（每次请求更新）
last_synced_version — 上次同步时的对象存储 version
status              — active / closing / closed
```

**cron worker 清理流程**：

```
1. 扫描 status=active 且 last_active_at > 30min 的 session
2. 设置 status=closing（防止并发清理）
3. best-effort CloseSession
4. 设置 status=closed
```

#### Workspace 并发访问

允许多 session 并发挂载同一 workspace_path，sync 时 last-write-wins。

- `InitSession` 不加互斥锁，各 session 独立创建 sandbox 容器
- 中台 session 记录中维护 `last_synced_version` 字段
- 对象存储端维护 `storage_version`（递增整数或时间戳）
- `SyncFromContainer` 时：无条件写入，更新 `storage_version`，若检测到 version 跳变则记录告警日志（含两个 session_id、workspace_path、时间）
- `SyncToContainer`（InitSession）时：拉取最新快照，记录当前 `storage_version` 到 session

**不做的事**：不做 merge、不做 conflict error、不做文件级锁。

## Sandbox 服务端改动

唯一改动：`SyncWorkspace` API 新增 `exclude` 参数。

### API 变更

```json
POST /api/v1/sandboxes/:id/workspace/sync
{
  "direction": "from_container",
  "exclude": [".agent"]
}
```

`exclude` 为可选字符串数组，匹配的路径前缀在同步时被跳过。

### 影响范围

- `pkg/types/workspace.go`：`SyncWorkspaceRequest` 新增 `Exclude []string` 字段
- `internal/sandbox/workspace.go`：`syncFromContainer()` 在差异计算和 tar 解压时跳过 exclude 匹配的路径
- `internal/api/handler/workspace.go`：透传 exclude 参数

不影响现有 `MountWorkspace`、`UnmountWorkspace`、`GetWorkspaceInfo` 接口。无 exclude 参数时行为与当前完全一致（向后兼容）。

## Workspace 同步时机策略

Agent 会话可能持续数分钟到数小时，仅依赖 InitSession / CloseSession 两个端点同步存在数据丢失风险。需要在**数据安全性**与**同步开销**之间取得平衡。

### 同步时机总览

| 时机 | 方向 | 触发方 | exclude | 说明 |
|------|------|--------|---------|------|
| InitSession | to_container | 中台 HTTP 请求 | — | 对象存储 → 容器，全量 |
| **ReAct 轮次结束** | from_container | 中台 HTTP 请求 | `.agent` | 本轮有 dirty Tool → 请求内同步，再返回响应 |
| **cron worker 清理** | from_container | 中台后台 cron | `.agent` | 扫描过期 session，best-effort 同步后销毁 |
| **显式 CloseSession** | from_container | 中台 HTTP 请求 | `.agent` | 用户主动结束会话（如果触发的话） |
| Sandbox reaper | — | Sandbox 服务 | — | 最终兜底，容器 idle timeout 销毁 |

### 方案选型

**主策略：ReAct 轮次结束同步**。在单次 HTTP 请求内，ReAct 循环结束后、响应返回前执行同步。用户看到回复时 workspace 已持久化。

**兜底策略：cron worker 扫描 + Sandbox reaper**。无长驻进程，不使用空闲心跳。

#### 主策略：ReAct 轮次结束同步

在 Eino Agent 的 ReAct 循环中，每完成一轮 Tool 调用 → LLM 推理后，判断本轮是否有写操作（write_file / run_code / delete_file / install_packages / run_skill），如果有则触发一次增量同步。

```
Tool Call(write_file) → LLM 推理 → 检测到写操作 → SyncWorkspace(from_container, exclude=[".agent"])
                                                      ↓
                                              更新 last_synced_version + last_active_at
                                                      ↓
                                              返回响应给用户
```

- 优点：同步时机精准，仅在有实际变更时触发；与 Agent 执行节奏自然对齐；用户看到响应时数据已持久化
- 缺点：需要 Tool 层标记"是否产生写操作"
- 实现：Tool 返回值中附加 `dirty bool` 标记，ReAct 循环聚合后决策

#### 不推荐：每次写操作后立即同步

每次 write_file / run_code 之后立即触发同步。

- 优点：数据零丢失
- 缺点：同步过于频繁，增量同步需要 `find` 扫描 + tar 传输，对高频写入场景影响性能
- 不推荐：Agent 一轮可能连续调用多次 write_file，逐次同步浪费严重

#### 不适用：空闲心跳

中台 Agent 是 HTTP 请求驱动的，无长驻进程，无法维护定时器。此方案不适用。

### 实现要点

#### 1. Tool 层 dirty 标记

```go
// ToolResult 在 Eino Tool 返回时附加元数据
type ToolMeta struct {
    Dirty bool // 本次调用是否产生了文件系统写操作
}
```

以下 Tool 固定标记 `Dirty=true`：
- `write_file`、`delete_file`、`install_packages`
- `run_code`、`run_skill`（代码可能写文件，保守标记为 dirty）

以下 Tool 固定标记 `Dirty=false`：
- `read_file`、`list_directory`、`search_files`
- `list_skills`、`load_skill`

#### 2. ReAct 循环集成

```
for each ReAct round:
    tools = agent.planAndCallTools()
    llmResponse = agent.reason(toolResults)

    if any(tool.meta.Dirty for tool in tools):
        sdk.SyncWorkspace(ctx, sandboxID, SyncWorkspaceRequest{
            Direction: "from_container",
            Exclude:   []string{".agent"},
        })
        updateLastActiveAt(sessionID)
        updateLastSyncedVersion(sessionID)
```

### 同步开销评估

当前增量同步流程：
1. 容器内执行 `find /workspace -printf` 收集文件清单 — 轻量，毫秒级
2. 比较 `LastSyncedAt` 时间戳筛选变更文件 — 内存操作
3. 仅下载变更文件的 tar — 开销与变更量成正比

典型 Agent 会话：每轮写入 1-5 个文件，增量同步耗时约 100-500ms，对 Agent 体验无明显影响。

### 会话期间 to_container 同步约束

**明确不支持**会话期间自动从对象存储拉取新文件到容器。

原因：
- Agent 在容器内有自己的工作状态，外部覆盖可能破坏进行中的工作
- 多 session 场景下，自动 pull 会和正在进行的写操作冲突
- 保持模型简单：InitSession 拉一次，之后容器内是 Agent 的独占工作空间

如果业务需要刷新，由中台显式调用 `SyncWorkspace(to_container)` 实现，不在本设计范围内。

## 数据流总览

```
                InitSession (HTTP)                     CloseSession (HTTP)
                    │                                       │
    ┌───────────────▼───────────────┐      ┌────────────────▼────────────────┐
    │ 1. Create persistent sandbox  │      │ 1. best-effort 回写身份文件      │
    │ 2. Mount workspace            │      │ 2. best-effort SyncWorkspace    │
    │    (对象存储 → 容器)           │      │    (容器 → 对象存储)             │
    │ 3. Upload .agent/ 文件        │      │    exclude=[".agent"]           │
    │ 4. 记录 session + version     │      │ 3. Destroy sandbox              │
    │ 5. 注入 Eino Tool             │      │ 4. 标记 session closed          │
    └───────────────┬───────────────┘      └─────────────────────────────────┘
                    │
                    ▼
    ┌───────────────────────────────────┐
    │   用户 HTTP 请求 → ReAct 循环     │
    │                                   │
    │  read_file ──┐                    │
    │  write_file ─┤                    │
    │  run_code ───┤→ SDK → HTTP → Sandbox
    │  run_skill ──┤                    │
    │  ...         ┘                    │
    │                                   │
    │  轮次结束 + dirty=true            │
    │    → SyncWorkspace(from_container)│
    │    → 更新 last_active_at          │
    │    → 返回响应给用户               │
    └───────────────────────────────────┘

    ┌───────────────────────────────────┐
    │   中台 cron worker (后台)         │
    │                                   │
    │  定期扫描 last_active_at 过期      │
    │    → best-effort CloseSession     │
    └───────────────────────────────────┘

    ┌───────────────────────────────────┐
    │   Sandbox reaper (最终兜底)       │
    │                                   │
    │  容器 idle timeout → 销毁容器     │
    └───────────────────────────────────┘
```

## 后续迭代事项（P2）

以下事项当前有意不做，留待后续优化：

### 1. run_code dirty 标记优化

当前 `run_code` 保守标记 `Dirty=true`。后续可优化：
- 增量同步时 `find` 发现无变更文件则 early return（当前实现已支持，实际开销仅一次 find，可接受）
- 或执行后用 `find /workspace -newer /tmp/marker` 快速判断是否有新文件，动态决定 dirty

### 2. SDK 类型维护

当前 SDK 手动复制 Sandbox `pkg/types` 中的类型定义。长期维护策略：
- 考虑从 OpenAPI spec 自动生成 SDK 类型
- 或在 SDK 每个类型上标注对应的 Sandbox 版本号，便于人工比对
