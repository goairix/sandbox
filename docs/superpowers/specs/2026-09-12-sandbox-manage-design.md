# SANDBOX MANAGE 管理后台设计

日期：2026-09-12
状态：已批准

## 1. 背景

Sandbox 当前是基于 Go 和 Gin 的代码执行服务，支持 Docker、Kubernetes、普通预热池、FUSE 预热池、Redis 会话与协调状态、工作空间和 OpenTelemetry。现有 API 面向业务调用方，只提供单个业务 Sandbox 的创建、查询、执行和销毁等能力，缺少以下内部运维能力：

- 全局查看物理运行实例和业务 Sandbox；
- 区分预热实例、申请中实例和已申请实例；
- 查看普通池、FUSE 池和各 Manager 副本的健康状态；
- 保存沙箱生命周期历史；
- 以受控方式执行 TTL 调整和销毁；
- 管理后台账号、角色、登录会话和审计记录。

本设计在现有 Sandbox 服务中增加模块化的管理后端，并建设独立部署的 React 管理前端。产品名称统一为 **SANDBOX MANAGE**。

## 2. 目标

首期目标如下：

1. 为内部运维人员提供全局总览、实例查询、资源池与运行时健康检查。
2. 统一展示预热实例和业务 Sandbox，并准确表达是否已被申请。
3. 支持调整已申请 Sandbox 的 TTL 和销毁已申请 Sandbox。
4. 建立完整的本地 Admin 账号、认证、内置 RBAC、登录会话和安全控制。
5. 使用 PostgreSQL、GORM 和 Repository 模式持久化管理域数据。
6. 使用 `github.com/go-gormigrate/gormigrate/v2` 管理全部 DDL 和 Seed。
7. 支持多 Manager 副本下的全局查询和跨副本安全操作。
8. 保存运行实例和 Sandbox 生命周期历史，并提供管理操作审计。
9. 前端使用 React、TypeScript 和 Ant Design，独立构建和部署。

## 3. 非目标

首期不包含：

- 自定义角色和自定义权限组合；
- 在线代码执行终端；
- 沙箱文件浏览器；
- 在线修改普通池或 FUSE 池容量；
- 修改沙箱网络策略；
- 手工触发工作空间同步或卸载；
- 在管理后台内嵌日志、Trace 或完整监控查询；
- SQLite 或 MySQL Adapter；
- 管理员自助注册、邮件找回密码和外部 SSO。

日志、Trace 和深度指标通过配置的 Grafana 或其他观测平台链接打开。

## 4. 总体架构

采用“模块化单体管理后端 + 独立前端”方案。

```text
React + Ant Design
        |
        | Bearer Token / JSON
        v
/admin/api/v1
        |
        +-- Auth / RBAC
        +-- Admin Application Services
        +-- Inventory Facade
        +-- Command Dispatcher / Owner Worker
        +-- Audit Service
        |
        +-- Repository Interfaces
        |       |
        |       v
        |   GORM PostgreSQL Adapter
        |
        +-- Manager / Pool / Runtime Facade
        +-- Redis State / FUSE Repository
        +-- OpenTelemetry / External Grafana
```

管理 API 加入现有 `sandbox` 进程，使用独立路由前缀 `/admin/api/v1`。它可以通过明确的 Facade 读取 Manager、Pool、Redis 和 Runtime 信息，但不得绕过 Manager 直接执行资源变更。

现有 `/api/v1` 业务 API 的认证语义保持独立。Admin Token 不能调用业务 API，业务 API Key 也不能调用管理 API。

生产前端独立部署。网关可以把前端和管理 API 暴露在同一站点，也可以使用不同 Origin；不同 Origin 时必须使用精确 CORS Allowlist。

## 5. 后端模块边界

建议新增以下模块：

```text
internal/admin/
  domain/          管理员、角色、会话、实例投影、命令、审计实体
  repository/      数据访问接口和事务边界
  service/         认证、账号、总览、实例、命令、审计用例
  inventory/       Manager、Pool、Redis、Runtime 的只读聚合
  command/         命令派发、Owner Worker、超时和幂等处理
  auth/            密码、JWT、Refresh Token、RBAC
  migration/       gormigrate 迁移和 Seed
  adapter/gorm/    PostgreSQL Repository 实现
  handler/         Gin Handler 和 DTO
```

边界规则：

- Handler 只处理 HTTP 绑定、验证、状态码和响应 DTO。
- Service 编排业务规则，不依赖 Gin 或 `*gorm.DB`。
- Repository 接口不暴露 GORM、SQL 语句或 PostgreSQL 专有类型。
- GORM Adapter 负责查询、事务、锁、分页和数据库错误翻译。
- Inventory Facade 只负责读取并合并实时信息，不执行资源变更。
- Owner Worker 只能通过 Manager 的公开管理能力修改 Sandbox。
- 管理模块不得读取或持久化工作空间访问密钥、代码内容和 Token 明文。

## 6. PostgreSQL、GORM 与迁移

### 6.1 数据库选择

首期只支持 PostgreSQL。使用：

- `gorm.io/gorm`；
- `gorm.io/driver/postgres`；
- `github.com/go-gormigrate/gormigrate/v2`。

Repository 和领域模型保持数据库无关，以便未来增加 MySQL Adapter。当前不承诺无需迁移工作即可切换数据库。

为降低未来适配成本：

- ID 由应用生成，使用 UUID 字符串，不依赖数据库序列；
- 需要保存的结构化元数据编码为 JSON 字符串，不在领域层依赖 `jsonb` 查询；
- PostgreSQL 的 Advisory Lock、`FOR UPDATE SKIP LOCKED` 和错误码解析只存在于 Adapter；
- Service 不拼接 SQL，也不判断数据库 Dialect。

### 6.2 迁移规则

- 禁止调用 GORM `AutoMigrate`。
- 所有表、列、索引、唯一约束、外键和数据修复都必须写在有序 Migration 中。
- 每个 Migration 使用不可复用、不可修改的稳定 ID。
- 已进入主分支且可能在线上执行过的 Migration 禁止改写；变更必须追加新 Migration。
- 每项迁移提供显式 `Migrate`；可安全回滚的迁移同时提供 `Rollback`。
- Migration 在每次服务启动时由主进程自动检查并执行，不新增迁移 CLI、Seed CLI、初始化 CLI、独立 Job 或 Init Container。
- 多副本启动时，以 PostgreSQL Advisory Lock 串行执行迁移。未取得锁的副本在限定时间内等待，迁移完成前不得进入 Ready。
- Migration 失败时服务启动失败，不对外提供业务或管理流量。

### 6.3 Seed

Seed 作为具名 Migration 注册到同一 `gormigrate` 迁移序列，并在服务启动的迁移阶段自动执行。已经成功记录的 Seed Migration 后续启动不会重复执行，不存在独立 Seed 命令。首期 Seed 包括：

1. 稳定权限码；
2. `super_admin`、`operator`、`auditor` 三种内置角色；
3. 角色与权限的固定关系；
4. 首个启用的超级管理员。

首个管理员用户名、展示名和密码来自配置文件，环境变量按现有 Viper 规则覆盖配置文件。只有首个管理员 Seed 尚未执行时才要求这些配置存在。Seed 必须幂等，且不得覆盖已有账号的密码、状态或人工调整后的资料。

## 7. 数据模型

### 7.1 身份与权限

`admin_users`：

- `id`、`username`、`display_name`、可选 `email`；
- `password_hash`；
- `status`：`active` 或 `disabled`；
- `failed_login_count`、`locked_until`；
- `password_changed_at`、`created_at`、`updated_at`。

用户名使用规范化后的唯一索引。管理员不物理删除。

`roles`、`permissions`、`admin_user_roles`、`role_permissions` 保存内置 RBAC。角色和权限使用稳定 `code` 作为业务标识，数据库 ID 仅用于关联。

`admin_sessions` 保存 Refresh Session：

- Session ID、Admin User ID；
- Refresh Token SHA-256 摘要；
- 创建、最后使用、过期、撤销时间；
- 轮换后的 Session ID；
- 来源 IP 和 User-Agent 摘要。

数据库不得保存 Access Token、Refresh Token 明文或密码明文。

### 7.2 运行实例与分配历史

`runtime_instances` 表示一个不可变物理 Runtime 实例：

- `runtime_id`、不可变 `runtime_uid`；
- Runtime 类型：Docker 或 Kubernetes；
- 池类型：`ordinary`、`fuse`、`direct`；
- Manager Owner ID；
- 原始池状态、归一化申请状态、Runtime 状态；
- 创建、首次发现、最近观察、终止时间。

`sandbox_allocations` 表示业务 Sandbox 对物理实例的单次申请：

- 业务 Sandbox ID；
- Runtime Instance ID；
- 模式、TTL、过期时间；
- 工作空间挂载模式和经过安全处理的配置快照；
- 申请、完成绑定、结束时间和结束原因。

普通池和 FUSE 池均为单次使用，因此一个物理实例最多关联一个业务 Allocation。直接创建的实例在创建成功后立即视为已申请。

`sandbox_events` 是追加式生命周期记录，包含物理实例、可选业务 Sandbox、事件类型、前后状态、原因、请求 ID、发生时间和完成时间。事件不保存执行代码、标准输出、环境变量或凭据。

### 7.3 多副本控制

`manager_nodes` 保存：

- 当前进程生命周期内稳定、跨副本唯一的 Manager Instance ID；
- 节点/Pod 标识和 Runtime 类型；
- 应用版本和能力信息；
- 最近心跳、启动时间和健康状态。

Kubernetes 部署优先通过 Downward API 注入不可变 Pod UID 作为 Instance ID；其他部署在进程启动时生成 Boot UUID。禁止多个活跃副本配置相同的 Instance ID。每个副本周期更新心跳，超过配置阈值未更新的节点视为离线。进程重启产生的新 Instance ID 必须先完成状态恢复和 Runtime UID 对账，之后才能接管旧 Owner 的 Sandbox。

`admin_commands` 保存：

- 命令 ID、幂等键、命令类型；
- 目标 Manager Owner ID；
- 业务 Sandbox ID、Runtime ID 和不可变 Runtime UID；
- 脱敏后的命令参数；
- `pending`、`running`、`succeeded`、`failed`、`expired` 状态；
- Lease Owner、Lease Deadline、尝试次数；
- 结果码、结果摘要和各阶段时间。

`admin_audit_logs` 保存：

- 操作者 ID 和用户名快照；
- 动作、资源类型和资源 ID；
- 请求 ID、来源 IP、User-Agent 摘要；
- 操作结果和错误码；
- 脱敏后的前值、后值；
- 创建和完成时间。

审计记录禁止更新业务内容，只允许补全同一操作的最终结果和保留期清理。

## 8. 实例库存与申请状态

管理端“沙箱”页面实际是统一运行实例总表，合并：

- `Runtime.ListSandboxes` 返回的全局物理实例；
- 普通 Pool 当前库存；
- Redis 中的 FUSE Pool Repository 记录；
- Manager 持有的业务 Sandbox；
- PostgreSQL 中的历史投影。

实时 Runtime 状态优先于 SQL 投影。SQL 用于补充业务配置、Owner、事件和已终止历史，不得把陈旧 SQL 状态展示为实时事实。

归一化申请状态：

| 申请状态 | 普通池 | FUSE 池 | 直接创建 |
|---|---|---|---|
| `unallocated` 未申请 | `available`，保留 `sandbox.pool=true` | `preparing`、`prepared` | 不适用 |
| `allocating` 申请中 | 从 Pool 取出但业务发布尚未完成 | `reserved`、`binding` | 创建中 |
| `allocated` 已申请 | 已移除 Pool 标记并绑定业务 Sandbox ID | `consumed` 并绑定业务 Sandbox | 创建完成 |

`cleanup`、状态冲突、Runtime 存在但控制状态缺失等情况展示为异常状态，并出现在总览的“需要处理”区域。

列表至少支持按以下条件筛选：

- Sandbox ID、Runtime ID、Workspace Path；
- 申请状态；
- Pool 类型和原始 Pool 状态；
- Runtime 状态；
- Manager Owner；
- 创建时间范围。

## 9. 多副本命令路由

多副本生产环境必须使用 PostgreSQL。所有 Manager 副本均运行 Owner Worker。

管理操作流程：

1. Admin API 完成认证、权限校验和请求验证。
2. 服务写入审计意图；写入失败则拒绝执行。
3. 服务根据实例投影和实时库存解析 Manager Owner，并绑定当前 Runtime UID。
4. 服务使用客户端幂等键创建 `admin_command`。
5. 目标 Owner Worker 使用数据库 Lease 原子领取命令。
6. Worker 再次校验 Sandbox ID、Owner 和 Runtime UID，防止名称复用或实例替换。
7. Worker 调用 Manager 执行 `UpdateTTL` 或 `Destroy`。
8. Worker 保存命令结果、生命周期投影和审计结果。
9. 前端使用 Operation ID 查询状态。

命令默认异步返回 `202 Accepted`。Worker 崩溃后，Lease 到期的命令可以被同一 Owner 的新实例安全重领。命令处理必须幂等；已完成命令不得重复执行。

Owner 离线时命令保持 Pending，达到 Deadline 后转为 Expired。首期不允许其他 Manager 接管仍可能拥有活跃工作空间的 Sandbox。启动恢复流程完成并确认新 Owner 后，Reconciler 才能更新 Owner。

## 10. 认证与 Token

### 10.1 密码

密码使用 `golang.org/x/crypto/bcrypt`。创建首个管理员、创建管理员和重置密码时统一调用 `bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)`；登录和修改密码前校验统一调用 `bcrypt.CompareHashAndPassword`。数据库只保存 bcrypt 哈希。密码、初始密码和密码重置值不得写入日志或审计元数据。

### 10.2 Access Token

- Access Token 为签名 JWT，默认有效期 15 分钟；
- 使用 `Authorization: Bearer <token>` 调用管理 API；
- Claim 至少包含 Subject、Session ID、Token ID、签发时间、过期时间和角色/权限版本；
- JWT 签名密钥由配置或环境变量提供，启动时校验最小强度；
- API 只接受管理域的 Issuer 和 Audience。

### 10.3 Refresh Token

- Refresh Token 为 256-bit 加密安全随机值，默认有效期 7 天；
- Refresh 请求同样使用 `Authorization: Bearer <refresh-token>`；
- 每次刷新都轮换 Token；
- 旧 Token 再次使用视为重放，撤销该 Session 链并要求重新登录；
- 退出、修改密码、重置密码和禁用账号均撤销相关 Refresh Session。

前端仅在内存中保存 Access Token，在 `sessionStorage` 中保存 Refresh Token。关闭浏览器会结束本地登录状态。前端不得把 Token 放入 URL、日志、错误上报或持久化分析事件。

### 10.4 登录保护

- 连续失败达到配置阈值后临时锁定账号；
- 成功登录后清零失败计数；
- 锁定、解锁、成功登录和失败登录均记录审计；
- 所有登录失败对外返回统一错误，避免枚举用户名。

## 11. RBAC

权限码至少包括：

- `dashboard:read`；
- `sandbox:read`；
- `sandbox:ttl:update`；
- `sandbox:destroy`；
- `pool:read`；
- `manager:read`；
- `admin:read`、`admin:manage`；
- `audit:read`；
- `profile:update`。

内置角色：

| 角色 | 权限 |
|---|---|
| `super_admin` | 全部权限 |
| `operator` | 总览、实例、历史、Pool 和 Manager 只读；TTL 调整和 Sandbox 销毁 |
| `auditor` | 总览、实例、历史、Pool、Manager 和审计只读 |

API 校验权限码，不在 Handler 中硬编码角色名。前端按权限码隐藏路由和操作按钮，但后端权限检查始终是最终安全边界。

系统必须始终保留至少一个启用的 `super_admin`。超级管理员不能禁用自己，不能移除自己的最后一个超级管理员角色。

## 12. 管理 API

统一前缀为 `/admin/api/v1`。

### 12.1 认证与个人资料

- `POST /auth/login`
- `POST /auth/refresh`
- `POST /auth/logout`
- `GET /auth/me`
- `PUT /auth/password`

### 12.2 总览与系统状态

- `GET /dashboard/summary`
- `GET /dashboard/trends`
- `GET /system/health`
- `GET /manager-nodes`
- `GET /pools`

### 12.3 运行实例

- `GET /instances`
- `GET /instances/:runtime_uid`
- `GET /instances/:runtime_uid/events`
- `POST /sandboxes/:id/actions/update-ttl`
- `POST /sandboxes/:id/actions/destroy`
- `GET /operations/:id`

TTL 调整和销毁只允许已申请且仍由有效 Owner 管理的业务 Sandbox。未申请的预热实例首期只读。

### 12.4 管理员

- `GET /admins`
- `POST /admins`
- `GET /admins/:id`
- `PUT /admins/:id/profile`
- `PUT /admins/:id/roles`
- `POST /admins/:id/disable`
- `POST /admins/:id/enable`
- `POST /admins/:id/unlock`
- `POST /admins/:id/reset-password`

### 12.5 审计

- `GET /audit-logs`
- `GET /audit-logs/:id`

列表接口使用服务端分页、稳定排序和有上限的 Page Size。写请求接受 `Idempotency-Key` Header。

## 13. 前端设计

前端使用 React、TypeScript 和 Ant Design，独立构建、发布和回滚。页面包括：

1. 登录；
2. 总览；
3. 沙箱；
4. 资源池与运行时；
5. 管理员；
6. 审计日志；
7. 个人设置。

总览展示：

- 活动、异常和待清理实例数量；
- 未申请、申请中、已申请数量；
- 普通池和 FUSE 池水位；
- Manager 节点健康；
- 创建和销毁趋势；
- 需要处理的异常与外部观测平台链接。

沙箱页面同时展示业务 ID、Runtime ID、Runtime UID 摘要、申请状态、Pool 类型、原始 Pool 状态、Runtime 状态、Owner 和时间信息。

前端请求层负责：

- 自动附加 Access Token；
- 收到 401 后只发起一个并发 Refresh，其余请求等待；
- 刷新成功后重放原请求；
- Refresh 失败后清理本地 Token 并跳转登录；
- 避免重复提交相同运维命令。

销毁操作要求输入业务 Sandbox ID 进行二次确认。TTL 调整弹窗展示当前 TTL、新 TTL 和预计过期时间。二次确认只提供误操作保护，不代替后端 RBAC、状态和 Runtime UID 校验。

## 14. 错误处理

错误响应格式：

```json
{
  "code": "SANDBOX_STATE_CONFLICT",
  "message": "sandbox state changed",
  "request_id": "...",
  "details": {}
}
```

主要状态码：

- `400`：输入无效；
- `401`：Token 缺失、无效或过期；
- `403`：权限不足或账号禁用；
- `404`：资源不存在；
- `409`：幂等键冲突、状态变化、Owner 变化或 Runtime UID 不匹配；
- `423`：账号临时锁定；
- `429`：登录或管理 API 限流；
- `503`：PostgreSQL、Owner Manager、Redis 或 Runtime 暂不可用。

内部错误记录结构化日志和 Trace，响应不得泄露 SQL、DSN、密码哈希、Token、Runtime 凭据或内部堆栈。

## 15. 一致性、对账与保留期

为了保存完整生命周期历史，所有新生命周期变更采用写意图模式：

1. PostgreSQL 写入事件或命令意图；
2. 执行 Pool、Manager 或 Runtime 操作；
3. 写入最终状态。

若第一步失败，不开始新的生命周期变更。若第二步成功但第三步失败，未完成意图由启动 Reconciler 和周期 Reconciler 根据 Runtime UID、Redis 状态和 Manager 状态完成对账。

数据库不可写时拒绝新的 Sandbox 创建、申请、TTL 更新和销毁，但不主动停止已运行 Sandbox。健康接口明确报告控制面降级。

默认保留期：

- Runtime Instance、Allocation 和生命周期事件：30 天；
- Admin Audit Log：180 天；
- 已完成 Admin Command：30 天；
- 活跃管理员、角色和有效 Session：按业务状态保留。

保留期均可配置。清理任务分批删除，避免长事务和大范围锁。

## 16. 可观测性

新增指标至少包括：

- 管理登录成功、失败和锁定次数；
- Access Refresh 成功、失败和重放检测次数；
- 各状态 Admin Command 数量和执行延迟；
- Manager 心跳延迟和离线节点数；
- 实例投影对账差异和修复次数；
- 管理数据库操作延迟和错误率；
- 生命周期意图未完成数量。

所有日志带 Request ID、Admin User ID、Command ID、Sandbox ID、Runtime ID 和 Trace Context 中适用的非敏感字段。

## 17. 配置

建议增加：

```yaml
admin:
  enabled: true
  database:
    dsn: ""
    max_open_conns: 20
    max_idle_conns: 10
    conn_max_lifetime_seconds: 1800
    operation_timeout_seconds: 10
    migration_lock_timeout_seconds: 120
  auth:
    issuer: "sandbox-manage"
    audience: "sandbox-manage-web"
    jwt_signing_key: ""
    access_token_ttl_seconds: 900
    refresh_token_ttl_seconds: 604800
    max_login_failures: 5
    login_lock_seconds: 900
  seed:
    admin_username: ""
    admin_display_name: ""
    admin_password: ""
  cors:
    allowed_origins: []
  manager:
    instance_id: "" # Kubernetes 注入 Pod UID；留空时生成进程 Boot UUID
    heartbeat_interval_seconds: 10
    offline_after_seconds: 30
    command_lease_seconds: 30
    command_deadline_seconds: 120
  retention:
    sandbox_history_days: 30
    audit_log_days: 180
    completed_command_days: 30
  observability:
    grafana_url: ""
```

环境变量继续使用 `SANDBOX_` 前缀和下划线展开嵌套配置，例如 `SANDBOX_ADMIN_DATABASE_DSN`。敏感配置优先通过部署 Secret 注入环境变量。

## 18. 部署与启动顺序

服务启动顺序：

1. 加载并校验配置；
2. 初始化日志和 Telemetry；
3. 连接 PostgreSQL；
4. 主进程获取迁移锁，自动执行全部待执行 Migration 与 Seed；
5. 初始化 Repository、Admin Service 和命令 Worker；
6. 初始化现有 Runtime、Redis、Storage 和 Manager；
7. 恢复现有 Sandbox 状态；
8. 执行首次实例对账；
9. 启动 Manager 心跳与 Owner Worker；
10. 对外进入 Ready。

管理前端独立部署，通过环境构建配置或运行时配置获得管理 API Base URL。发布时前后端版本需满足明确的 API 兼容窗口。

## 19. 测试策略

### 19.1 后端单元测试

- bcrypt 密码生成、正确/错误密码验证及超长密码错误处理；
- JWT Claim、Issuer、Audience、过期和签名校验；
- Refresh Token 轮换、重放和 Session 链撤销；
- 登录失败计数、锁定、解锁和账号禁用；
- 三种内置角色的完整权限矩阵；
- 最后一个启用超级管理员保护；
- 申请状态归一化；
- 命令状态机、幂等键和 Runtime UID fencing；
- Service 不依赖 GORM 的 Repository Mock 测试。

### 19.2 PostgreSQL 集成测试

- 空数据库迁移到最新版本；
- 重复执行迁移无副作用；
- 多副本并发迁移只有一个执行者；
- Seed 幂等且不覆盖已有密码；
- Repository 查询、分页、事务和唯一约束；
- 多 Worker 竞争同一命令；
- Lease 超时重领、Owner 离线和命令过期；
- 生命周期未完成意图对账。

集成测试使用真实 PostgreSQL，不使用 SQLite 模拟 PostgreSQL 行为。

### 19.3 API 测试

- 登录、刷新、退出和个人密码修改；
- 401、403、409、423、429 和 503 映射；
- 所有管理接口的 RBAC；
- CORS Allowlist；
- TTL 和销毁操作的 202、轮询及幂等行为；
- 审计意图写入失败时拒绝操作；
- 响应和日志不泄露敏感字段。

### 19.4 前端测试

- 登录、Refresh 并发合并和退出；
- 权限路由与按钮可见性；
- 实例搜索、筛选、分页和三种申请状态；
- TTL 修改和销毁确认；
- 异步命令状态展示与失败恢复；
- 管理员禁用、解锁、密码重置和角色分配；
- 审计筛选与详情；
- 关键流程端到端测试。

## 20. 验收标准

实现完成需满足：

1. PostgreSQL 空库可由 `gormigrate/v2` 完整迁移并创建首个超级管理员。
2. 代码中不存在 `AutoMigrate` 调用，所有 DDL 均可定位到 Migration。
3. 普通服务启动会自动执行所有待执行 Migration 与 Seed，系统不依赖任何 CLI、独立 Job 或 Init Container。
4. 三种角色只能访问其允许的页面与 API。
5. Access Token、Refresh 轮换、退出、禁用和密码重置符合本设计。
6. 多副本下可以全局查看普通池、FUSE 池、直接创建实例和历史实例。
7. 每个实例准确展示未申请、申请中或已申请状态，并保留原始 Pool 状态。
8. 任意副本收到的 TTL 或销毁请求都能路由给正确 Owner；Owner 或 Runtime UID 变化时安全拒绝。
9. 所有管理写操作都有完整、脱敏且可检索的审计记录。
10. 数据库或 Owner 不可用时返回明确错误，不绕过 Manager 直接修改 Runtime。
11. Go 测试、PostgreSQL Migration 集成测试、前端测试和生产构建全部通过。

## 21. 后续演进

Repository 边界允许未来新增 MySQL Adapter。届时必须：

- 为 MySQL 提供独立连接和数据库错误翻译；
- 验证所有 Migration 的 Dialect 兼容性，必要时为 Migration 增加受控的 Dialect 分支；
- 替换 PostgreSQL Advisory Lock 和命令领取实现；
- 使用真实 MySQL 增加完整 Repository、迁移和并发测试；
- 保持 Handler、Service、领域模型和前端 API 不变。

未来还可以独立评估自定义 RBAC、SSO、在线 Pool 调整、内嵌日志与 Trace、管理后端拆分为独立服务等能力；这些不属于本次实现计划。
