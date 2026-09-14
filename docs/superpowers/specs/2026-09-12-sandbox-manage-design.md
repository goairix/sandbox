# SANDBOX MANAGE 管理后台设计

日期：2026-09-12
修订日期：2026-09-14
状态：原设计已批准；无状态多副本与旁路隔离修订待书面复核

## 1. 背景

Sandbox 当前是基于 Go 和 Gin 的代码执行服务，支持 Docker、Kubernetes、普通预热池、FUSE 预热池、Redis 会话与协调状态、工作空间和 OpenTelemetry。现有 API 面向业务调用方，只提供单个业务 Sandbox 的创建、查询、执行和销毁等能力，缺少以下内部运维能力：

- 全局查看物理运行实例和业务 Sandbox；
- 区分预热实例、申请中实例和已申请实例；
- 查看普通池、FUSE 池和各 Manager 副本的健康状态；
- 保存沙箱生命周期历史；
- 以受控方式执行 TTL 调整和销毁；
- 管理后台账号、角色、登录会话和审计记录。

本设计在现有 Sandbox 服务中增加模块化的管理后端，并建设独立部署的 React 管理前端。产品名称统一为 **SANDBOX MANAGE**。

本次修订基于当前 Kubernetes 后端：Redis 是活跃 Sandbox 生命周期的权威状态，API 副本无状态；普通池与 FUSE 池使用共享库存，操作门禁、控制器租约和清理恢复由核心后端负责。创建请求所在的 API 副本不是 Sandbox 的固定 Owner。Docker 保持现有进程内管理能力，不据此承诺跨进程操作。

最高优先级约束：**Manage 的配置、数据库、认证、迁移、Seed、审计、投影和后台任务故障均不得改变 Sandbox 核心服务的启动、健康、业务响应或生命周期执行。** 只有经过授权的 TTL 调整和销毁允许通过现有 Manager 主动改变目标 Sandbox；这不代表 Manage 可以成为核心生命周期的依赖。

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
10. Manage 为可降级的旁路模块；开启、关闭或故障均不引入 Sandbox 对 PostgreSQL 或管理域的依赖。

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
        +-- Operation Service / Result Reconciler
        +-- Audit Service
        |
        +-- Repository Interfaces
        |       |
        |       v
        |   GORM PostgreSQL Adapter
        |
        +-- Manager Public Operations (TTL / ScheduleDestroy)
        +-- Redis Active / Shared Pool Read Facades
        +-- Runtime Identity / Health Read Facade
        +-- OpenTelemetry / External Grafana
```

管理 API 加入现有 `sandbox` 进程，使用独立路由前缀 `/admin/api/v1`。它可以通过明确的 Facade 读取 Manager、Pool、Redis 和 Runtime 信息，但不得绕过 Manager 直接执行资源变更。

整个管理模块受 `manage.enabled` 开关控制，默认值为 `false`。关闭时只解析该布尔值，不校验其他 Manage 配置，不创建数据库连接，不执行 Migration 或 Seed，不注册任何 `/admin/api/v1` 路由，不装配管理 Repository 和历史观察器，也不启动管理投影、操作结果对账或历史清理任务。关闭管理模块的服务不要求配置或访问 PostgreSQL，现有 `/api/v1` 和 Sandbox 生命周期行为保持不变。核心 Redis 控制器、共享池维护、工作空间协调和 readiness 不属于 Manage，不能被此开关关闭。

只有 `manage.enabled=true` 时才在独立的 Manage 初始化流程中校验 DSN、JWT、首管 Seed 等管理配置。失败只标记 Manage 为不可用，不向核心启动流程返回致命错误，不退出进程，不阻断核心 Ready。Manage 使用独立状态 `initializing`、`ready`、`unavailable`、`stopping`；启用但尚未就绪时管理路由返回脱敏的 `503 MANAGE_UNAVAILABLE`，不得绕过认证放行。

### 4.1 故障与资源隔离

- 核心 Manager、Pool、Runtime 和 Redis Repository 不导入管理 Repository，也不等待管理数据库事务、迁移、审计或历史消费。
- Manage 独立拥有数据库连接池、任务 Context、并发额度、请求限流和熔断；只允许管理 Context 派生自进程 Context，不得由 Manage 取消核心 Context。
- 管理初始化在核心服务可正常启动的前提下异步进行。管理配置解析错误在管理域内报告；不能因为管理字段无效导致整体配置加载失败。
- 管理查询、对账和历史清理使用有界分页、超时、退避和速率限制；不得执行无界 `KEYS`、高频全量 Pod 扫描、长持锁操作或无界重试。管理读连接独立预算，不能修改核心 Redis 的 HA、持久性 ACK 或租约配置。
- 管理请求的认证、限流、CORS 和健康中间件只挂载到管理路由组，不改变业务 API 的中间件和认证语义。
- 管理任务故障只停止或降级相应管理功能；禁止调用进程退出、核心 Stop，或为了补审计重新执行已经受理的核心操作。
- `/health` 和 `/ready` 继续描述核心 Sandbox；管理可用性在管理健康接口单独表达，不得影响 Kubernetes 核心 readiness。
- 同进程方案只能提供上述依赖和有界资源隔离，无法承诺 OOM、进程崩溃或所有 CPU 竞争的物理隔离；若要求这类故障也完全不波及核心，必须另行批准 Manage 独立进程部署，不能声称同进程方案具有硬隔离。

现有 `/api/v1` 业务 API 的认证语义保持独立。Admin Token 不能调用业务 API，业务 API Key 也不能调用管理 API。

生产前端独立部署。网关可以把前端和管理 API 暴露在同一站点，也可以使用不同 Origin；不同 Origin 时必须使用精确 CORS Allowlist。

## 5. 后端模块边界

建议新增以下模块：

```text
internal/admin/
  domain/          管理员、角色、会话、实例投影、操作、审计实体
  repository/      数据访问接口和事务边界
  service/         认证、账号、总览、实例、命令、审计用例
  inventory/       Manager、Pool、Redis、Runtime 的只读聚合
  operation/       管理操作、幂等记录和只读结果对账
  projection/      有界历史观察、实例投影和保留期清理
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
- Operation Service 只能通过 Manager 的公开管理能力修改 Sandbox；不建立固定 Owner 路由或第二套生命周期控制器。
- 管理结果对账只读取核心状态和证据，不接管工作空间、不修改控制器租约、不驱动核心清理。
- 管理模块不得读取或持久化工作空间访问密钥、代码内容和 Token 明文。
- Handler 使用显式字段白名单 DTO，不直接序列化内部 Snapshot、池记录、租约对象或 reservation token。

## 6. PostgreSQL、GORM 与迁移

### 6.1 数据库选择

首期支持 PostgreSQL 17 或更高版本。数据库必须通过当前已安装的插件提供无参数、返回 PostgreSQL 原生 `uuid` 类型的 `uuid_generate_v7()` 函数。应用只做能力检测，不负责执行 `CREATE EXTENSION`、安装或升级数据库插件。使用：

- `gorm.io/gorm`；
- `gorm.io/driver/postgres`；
- `github.com/go-gormigrate/gormigrate/v2`。

Repository 和领域模型保持数据库无关，以便未来增加 MySQL Adapter。当前不承诺无需迁移工作即可切换数据库。

所有管理域自有表都使用名为 `id` 的原生 UUID 主键，DDL 统一定义为 `id uuid PRIMARY KEY DEFAULT uuid_generate_v7()`。关联表也保留独立 UUIDv7 主键，并为业务唯一关系增加唯一约束。Gormigrate 自身的迁移记录表沿用库默认结构，不纳入这一主键约束。

PostgreSQL Adapter 插入新实体时默认让数据库生成 ID，并通过 `RETURNING` 取回 UUIDv7。领域层使用 UUID 值，但不感知 `uuid_generate_v7()` 函数。业务 Sandbox ID、Runtime ID、Runtime UID 和 Manager Instance ID 是外部标识，继续使用字符串字段，不冒充管理表主键。

为降低未来适配成本：

- Repository 接口不规定 UUID 的生成位置；未来 MySQL Adapter 可在插入前由应用生成 UUIDv7；
- 需要保存的结构化元数据编码为 JSON 字符串，不在领域层依赖 `jsonb` 查询；
- PostgreSQL 的 Advisory Lock、`FOR UPDATE SKIP LOCKED` 和错误码解析只存在于 Adapter；
- Service 不拼接 SQL，也不判断数据库 Dialect。

### 6.2 迁移规则

- 禁止调用 GORM `AutoMigrate`。
- 所有表、列、索引、唯一约束、外键和数据修复都必须写在有序 Migration 中。
- 每个 Migration 使用不可复用、不可修改的稳定 ID。
- 已进入主分支且可能在线上执行过的 Migration 禁止改写；变更必须追加新 Migration。
- 每项迁移提供显式 `Migrate`；可安全回滚的迁移同时提供 `Rollback`。
- 所有管理域建表迁移必须显式声明 `id uuid PRIMARY KEY DEFAULT uuid_generate_v7()`，不得退化为 UUIDv4、自增整数或无序字符串主键。
- 仅当 `manage.enabled=true` 时，Migration 才在每次服务启动时由主进程自动检查并执行；不新增迁移 CLI、Seed CLI、初始化 CLI、独立 Job 或 Init Container。
- 多副本启动时，以 PostgreSQL Advisory Lock 串行执行迁移。未取得锁的副本只在 Manage 初始化任务中限定时间等待；迁移完成前 Manage 不进入 Ready，但核心服务照常 Ready。
- Migration、Seed 或 UUIDv7 能力检测失败只使 Manage 初始化失败，业务流量与核心生命周期不受影响。关闭管理模块时不得探测数据库或迁移状态。

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
- State Scope、Pool Key / Contract Fingerprint；
- 原始池状态、归一化申请状态、生命周期阶段、Runtime 状态、清理阶段；
- 创建、首次发现、最近观察、终止时间。

`sandbox_allocations` 表示业务 Sandbox 对物理实例的单次申请：

- 业务 Sandbox ID；
- Runtime Instance ID；
- 模式、TTL、过期时间；
- 工作空间挂载模式和经过安全处理的配置快照；
- 申请、完成绑定、结束时间和结束原因。

普通池和 FUSE 池均为单次使用，因此一个物理实例最多关联一个业务 Allocation。直接创建的实例在创建成功后立即视为已申请。

`sandbox_events` 是追加式生命周期观察记录，包含物理实例、可选业务 Sandbox、事件类型、前后状态、原因、请求 ID、发生时间、观察时间和完成时间。事件不保存执行代码、标准输出、环境变量或凭据。对账推断的事件必须标明来源，不能冒充精确发生时间。历史采集允许缺口，规则见第 15 节。

原始创建时间独立保存，不能取每次最新 Snapshot 的 `CreatedAt` 作为原始创建时间：当前 `UpdateTTL` 会重置该字段作为过期基准。

### 7.3 多副本诊断与管理操作

`manager_nodes` 保存：

- 当前进程生命周期内稳定、跨副本唯一的 Manager Instance ID；
- 节点/Pod 标识和 Runtime 类型；
- 应用版本和能力信息；
- 最近观察、启动时间和健康状态及数据来源。

优先复用核心已经使用的 Instance ID。该表仅为可选诊断投影，不是 Sandbox 所属关系、请求路由表或租约权威。数据缺失或过期表示诊断未知，不能据此拒绝核心操作或改变控制器归属。

`admin_commands` 保存：

- 命令 ID、幂等键、命令类型；
- 发起请求的 API Instance ID（仅诊断，不是目标 Owner）；
- 业务 Sandbox ID、Runtime ID 和不可变 Runtime UID；
- 脱敏后的命令参数；
- `pending`、`running`、`accepted`、`succeeded`、`failed`、`unknown`、`expired` 状态；
- 请求 Deadline、执行阶段及结果对账时间；
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

管理端“沙箱”页面实际是统一运行实例总表。Kubernetes 模式下合并：

- `Runtime.ListSandboxes` 返回的全局物理实例；
- Redis 活跃 Sandbox 权威记录；
- 普通共享 Pool 当前库存，按 Scope 和 Contract Fingerprint 区分；
- Redis 中的 FUSE Pool Repository 记录；
- PostgreSQL 中的历史投影。

申请状态和生命周期以 Redis 控制记录为依据，物理运行状态以 Runtime 为依据，两者以不可变 Runtime UID 关联并分别展示。SQL 只补充历史，不能覆盖实时事实。不能累加各 API 副本缓存的共享池大小作为全局库存，也不能只凭 Pod 标签判断是否已申请。Docker 使用现有进程内只读 Facade，并明确显示查询范围。

归一化申请状态：

| 申请状态 | 普通池 | FUSE 池 | 直接创建 |
|---|---|---|---|
| `unallocated` 未申请 | `preparing`、`prepared` 且无已领取证据 | `preparing`、`prepared` | 不适用 |
| `allocating` 申请中 | `claimed` 或业务 `publishing` | `reserved`、`binding` | `publishing` |
| `allocated` 已申请 | 活跃记录已发布并绑定业务 Sandbox | 活跃记录已发布并绑定业务 Sandbox；`consumed` 补充池领取证据 | 活跃记录已发布 |
| `unknown` 待核实 | 记录缺失、读取失败或证据冲突 | 记录缺失、读取失败或证据冲突 | 控制状态无法确认 |

`cleanup` 是清理维度，不自动代表未申请。已申请实例进入 `destroying` 或 `cleanup_pending` 后仍保留已申请事实；确认不足则展示 `unknown`。状态冲突、Runtime 存在但控制状态缺失等情况出现在总览的“需要处理”区域。

关联记录仍为 `publishing` 时优先显示申请中；不能仅凭 FUSE 池 `consumed` 就推断业务发布成功。已领取但发布证据缺失时显示待核实，不能退回未申请。

列表返回采集时间、各数据源可用性和投影滞后信息；不能把数据不可读解释为零库存。补充 Active Lifecycle、Pool State 和 Cleanup Stage 独立列。

列表至少支持按以下条件筛选：

- Sandbox ID、Runtime ID、Workspace Path；
- 申请状态；
- Pool 类型和原始 Pool 状态；
- Runtime 状态；
- State Scope、Pool Contract Fingerprint；
- 创建时间范围。

## 9. 多副本管理操作

PostgreSQL 只保存管理操作与审计，不负责 Sandbox 请求路由、核心操作门禁或生命周期接管。Kubernetes 的任意 API 副本均通过现有 Manager 和 Redis fencing 执行操作，无需 HTTP 转发、固定 Owner 或 PostgreSQL Owner Worker。

管理操作流程：

1. Admin API 完成认证、权限校验和请求验证。
2. 服务写入审计意图；写入失败则拒绝执行。
3. 服务使用客户端幂等键创建 `admin_command`，与审计意图在同一管理事务中提交。
4. 服务读取实时目标身份，绑定 Sandbox ID 和不可变 Runtime UID；SQL 投影不能作为执行依据。
5. 当前副本调用 Manager 的 `UpdateTTL` 或 `ScheduleDestroy`，目标身份校验必须与核心操作门禁组合，避免先查后改竞态。
6. Manager 使用现有操作租约、版本检查和控制器 fencing；管理层不另行改变其持久性和清理语义。
7. 服务保存受理或执行结果；数据库结果补写失败不回滚核心状态、不停止清理、不盲目重放。
8. 管理结果对账只读取权威状态和清理证据，补全操作与审计结果。
9. 前端使用 Operation ID 查询状态。

TTL 在限定时间内完成可直接返回结果；销毁受理后返回 `202 Accepted` 与 Operation ID。`accepted` 只表示核心持久化了销毁请求，既有核心控制器负责后续清理和跨副本恢复。不能仅凭 Pod 消失或活跃记录缺失宣称全部清理成功；缺少足够终态证据时结果为 `unknown`，不阻碍核心删除其记录。

操作响应丢失、Redis 持久性未确认或执行副本崩溃后，不能因 Deadline 到期就自动重新执行 TTL 或判定执行失败。TTL 的重放可能再次延长过期时间。相同幂等键返回原操作；只有确认未进入核心执行的请求可标为 `expired`，不确定结果进入只读对账。Docker 操作限于现有本地 Manager 能力，不能套用 Kubernetes 跨副本承诺。

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

TTL 调整只允许核心 Manager 判定可变更的已申请 Sandbox；销毁允许提交或查询已进入清理的目标。两者都必须绑定不可变 Runtime UID，并遵循核心状态机。未申请的预热实例首期只读。

`GET /system/health` 单独返回 Manage 初始化、数据库和投影状态，以及经过脱敏的核心依赖诊断：Redis 模式及持久性状态、生命周期与清理阶段、共享池合同版本、Runtime、网络提供方和 AppArmor 门禁。只展示已取得的证据，未知状态不能伪装成健康。Manage 不可用时此接口只返回不含敏感信息的 `503`，不开放其他未认证诊断；核心健康接口不依赖它。

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
- 未申请、申请中、已申请、待核实数量及采集时间；
- 普通池和 FUSE 池水位；
- Manager 节点健康；
- 创建和销毁趋势；
- 需要处理的异常与外部观测平台链接。

沙箱页面同时展示业务 ID、Runtime ID、Runtime UID 摘要、申请状态、Pool 类型、原始 Pool 状态、生命周期阶段、清理进度、Runtime 状态、Scope / Contract Fingerprint 和时间信息。控制器归属如果展示，只能作为瞬时诊断，不称为固定 Sandbox Owner。

前端请求层负责：

- 自动附加 Access Token；
- 收到 401 后只发起一个并发 Refresh，其余请求等待；
- 刷新成功后重放只读请求；写请求必须保留原幂等键，结果不确定时先查询 Operation，不能盲目重放；
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
- `409`：幂等键冲突、状态变化或 Runtime UID 不匹配；
- `423`：账号临时锁定；
- `429`：登录或管理 API 限流；
- `503`：Manage 尚未就绪，或管理请求依赖的 PostgreSQL、Redis、Runtime 暂不可用。仅影响管理接口，不能由此改变核心健康状态。

内部错误记录结构化日志和 Trace，响应不得泄露 SQL、DSN、密码哈希、Token、Runtime 凭据或内部堆栈。

## 15. 一致性、对账与保留期

### 15.1 权威状态与故障边界

Kubernetes 核心生命周期由现有 Redis 记录、操作门禁、控制器租约和 Runtime UID 证据决定。PostgreSQL 不是第二份生命周期权威，管理投影不能驱动核心资源创建、销毁或恢复。

核心业务创建、申请、执行、文件操作、TTL 更新、销毁、到期清理、共享池补充和工作空间恢复均不写入或等待 Manage PostgreSQL。管理数据库不可写、连接失败、迁移失败或管理任务崩溃不能阻断这些路径。

仅由管理端主动发起的写操作要求先写管理审计意图；失败时拒绝该管理请求，不执行目标变更。操作一旦进入核心执行，后续 SQL 故障不得撤销其核心持久化意图、延迟销毁或要求核心保留记录等待审计。结果补写由只读管理对账完成，证据不足则保留 `unknown`。

### 15.2 历史观察与允许缺口

历史属于旁路观察数据，而非核心事务承诺。首期以限速游标扫描、Runtime 身份对账和有界非阻塞观察通知建立投影。通知消费、入库或缓存失败只计数并标记管理历史缺口；队列满时允许丢弃，不反压核心，不无限积压内存，不向核心调用方返回错误。

不得为 Manage 在核心 Redis Lua 中增加强制事件写入，不得将 Manage 消费 ACK、历史落库成功或管理保留期作为核心状态推进和删除的前提。关闭 Manage 时不启动这些观察器。

扫描不能保证捕获短生命周期实例或精确终态，进程重启、PG 故障和队列溢出均可能造成历史缺失。列表、趋势和事件详情必须表达观察来源、最后采集时间、已知中断窗口和缺口计数；不能承诺所有缺失事件都可识别或恢复，也不能把缺口解释为没有业务发生。以实体身份、版本和事件来源构建唯一键去重，允许重复观察。

若未来必须保证完整生命周期历史，应独立设计核心自身的可靠事件能力并评估成本，或使用外部可靠观察源；不能以管理域的可用性换取完整历史。当前优先级是 Sandbox 不受 Manage 影响，因此取消旧设计中的全生命周期 SQL 写前意图和无损历史承诺。

### 15.3 保留期

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
- Manage 初始化状态、重试与熔断次数；
- 节点诊断观察滞后和未知节点数；
- 实例投影对账差异和修复次数；
- 管理数据库操作延迟和错误率；
- 历史观察丢弃、缺口和投影延迟；
- 管理查询限流、后台任务并发和执行超时。

所有日志带 Request ID、Admin User ID、Command ID、Sandbox ID、Runtime ID 和 Trace Context 中适用的非敏感字段。

## 17. 配置

建议增加：

```yaml
manage:
  enabled: false
  initialization:
    timeout_seconds: 150
    retry_interval_seconds: 30
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
  inventory:
    refresh_interval_seconds: 30
    request_timeout_seconds: 3
    scan_page_size: 100
    max_concurrent_queries: 2
  operation:
    request_timeout_seconds: 10
    reconcile_interval_seconds: 30
    max_concurrent_requests: 2
  history:
    queue_capacity: 1024
    batch_size: 100
    reconcile_interval_seconds: 60
  retention:
    sandbox_history_days: 30
    audit_log_days: 180
    completed_command_days: 30
  observability:
    grafana_url: ""
```

环境变量继续使用 `SANDBOX_` 前缀和下划线展开嵌套配置，例如 `SANDBOX_MANAGE_ENABLED=true` 和 `SANDBOX_MANAGE_DATABASE_DSN`。敏感配置优先通过部署 Secret 注入环境变量。

## 18. 部署与启动顺序

核心服务启动顺序不由 Manage 改变：

1. 加载并校验核心配置；仅解析 `manage.enabled`，其他管理配置错误留在管理域处理；
2. 初始化日志和 Telemetry；
3. 按现有路径初始化 Runtime、Redis、Storage 和 Manager，以及核心恢复和共享池维护；
4. 按核心已有条件对外进入 Ready，不等待 PostgreSQL、迁移、Seed 或实例投影。

`manage.enabled=true` 时在同一主进程的独立有界初始化任务中自动执行：

1. 设置 Manage 为 `initializing`，管理路由通过状态门禁返回 `503`；
2. 校验管理配置，连接独立 PostgreSQL 连接池，并调用 `uuid_generate_v7()` 校验能力；
3. 获取迁移锁，执行全部待执行 Migration 与 Seed；
4. 初始化管理 Repository、认证服务、只读库存和操作服务；
5. 设置 Manage 为 `ready`，启动有界历史观察、操作结果对账和保留期清理。初次全量投影不是核心或管理认证 Ready 的前置条件，未采集数据明确展示未知。

任一步失败只设置 Manage 为 `unavailable`，关闭本次失败初始化产生的管理资源，记录脱敏错误并按限定间隔重试。初始化不得无限等待或累积多个重试任务，核心服务不重启。运行时 PG 故障使管理请求 fail closed，核心服务继续；数据库恢复后管理初始化或查询在有界重试下恢复。

`manage.enabled=false` 时不注册管理路由，不启动上述管理初始化、投影、观察和清理任务。PostgreSQL DSN 可以为空，数据库可以不存在或不可达；核心 Redis 与 Kubernetes 所需依赖仍按原有条件初始化。

关闭进程时取消管理任务，并独立、有界地释放其资源；核心停止和清理不等待管理数据库关闭或历史队列清空。管理资源回收不得串行阻塞核心停止流程。

管理前端独立部署，通过环境构建配置或运行时配置获得管理 API Base URL。发布时前后端版本需满足明确的 API 兼容窗口。

## 19. 测试策略

### 19.1 后端单元测试

- `manage.enabled=false` 时不校验 DSN、JWT 和 Seed 配置，不创建任何 Manage 依赖；
- `manage.enabled=true` 时缺少必要配置只返回明确的管理初始化错误，不退出核心进程；
- bcrypt 密码生成、正确/错误密码验证及超长密码错误处理；
- JWT Claim、Issuer、Audience、过期和签名校验；
- Refresh Token 轮换、重放和 Session 链撤销；
- 登录失败计数、锁定、解锁和账号禁用；
- 三种内置角色的完整权限矩阵；
- 最后一个启用超级管理员保护；
- 申请状态归一化；
- 命令状态机、幂等键和 Runtime UID fencing；
- Service 不依赖 GORM 的 Repository Mock 测试；
- 管理任务取消、错误和通知队列溢出不会取消核心 Context、返回核心错误或阻塞核心执行；
- 观察历史使用独立原始创建时间，不被 TTL 重置覆盖。

### 19.2 PostgreSQL 集成测试

- 空数据库迁移到最新版本；
- 重复执行迁移无副作用；
- 多副本并发迁移只有一个执行者；
- 所有管理域自有表的主键类型为 PostgreSQL `uuid`，数据库默认值生成 UUIDv7；
- 新增记录由 Go UUID 解析器验证版本为 7；
- `uuid_generate_v7()` 缺失、无执行权限或返回类型错误时 Manage 不可用，核心照常启动；
- Seed 幂等且不覆盖已有密码；
- Repository 查询、分页、事务和唯一约束；
- 并发管理请求的幂等约束，结果未知时不自动重复执行；
- 管理结果只读对账、SQL 结果补写失败和历史去重；
- 迁移锁超时、Seed 错误、PG 断连和慢查询均不影响核心 Ready 或生命周期。

集成测试使用真实 PostgreSQL，不使用 SQLite 模拟 PostgreSQL 行为。

### 19.3 API 测试

- `manage.enabled=false` 时所有 `/admin/api/v1` 路由均未注册，现有 `/api/v1` 行为不变；
- `manage.enabled=true` 时管理路由注册，但初始化或依赖不可用时返回管理 `503`；
- 登录、刷新、退出和个人密码修改；
- 401、403、409、423、429 和 503 映射；
- 所有管理接口的 RBAC；
- CORS Allowlist；
- TTL 同步结果、销毁 202、轮询及幂等行为；
- 审计意图写入失败时拒绝操作；
- 响应和日志不泄露敏感字段；
- 启用 Manage 但 PG 不可用时，核心 `/health`、`/ready` 和 `/api/v1` 行为与未启用时一致；
- 库存数据源失败返回未知或部分数据标记，不展示零库存或默认未申请；
- 任意 Kubernetes API 副本均可执行管理操作；销毁与活跃流式操作并发时遵循现有门禁；
- 管理查询、轮询、历史队列溢出和后台任务并发压力下，核心服务仍满足既有延迟与吞吐预算；
- 结果未确认时不宣称已销毁，不基于瞬时 Controller Owner 拒绝请求。

### 19.4 前端测试

- 登录、Refresh 并发合并和退出；
- 权限路由与按钮可见性；
- 实例搜索、筛选、分页、四种申请状态及数据滞后提示；
- TTL 修改和销毁确认；
- 异步命令状态展示与失败恢复；
- 管理员禁用、解锁、密码重置和角色分配；
- 审计筛选与详情；
- 关键流程端到端测试。

## 20. 验收标准

实现完成需满足：

1. PostgreSQL 空库可由 `gormigrate/v2` 完整迁移并创建首个超级管理员。
2. 代码中不存在 `AutoMigrate` 调用，所有 DDL 均可定位到 Migration。
3. 启用管理模块后，普通服务启动会自动执行所有待执行 Migration 与 Seed，系统不依赖任何 CLI、独立 Job 或 Init Container。
4. `manage.enabled=false` 时不连接数据库、不执行 Migration/Seed、不注册管理路由、不启动管理后台任务，且 PostgreSQL 不可用不影响现有服务。
5. `manage.enabled=true` 时独立初始化管理模块；配置、PG、UUIDv7 能力、Migration 或 Seed 失败均只使 Manage 不可用，核心服务正常启动和 Ready。
6. 所有管理域自有表都使用 PostgreSQL 原生 `uuid` 主键，并由数据库 `uuid_generate_v7()` 默认生成 UUIDv7。
7. 三种角色只能访问其允许的页面与 API。
8. Access Token、Refresh 轮换、退出、禁用和密码重置符合本设计。
9. 多副本下可以全局查看普通池、FUSE 池、直接创建实例和历史实例。
10. 每个实例展示有证据支持的未申请、申请中、已申请或待核实状态；保留原始池状态、生命周期及清理阶段，不累加各副本的共享库存缓存。
11. 任意 Kubernetes 副本收到 TTL 或销毁请求都直接通过现有 Manager 门禁执行；Runtime UID 不匹配安全拒绝，不建立固定 Owner 路由。
12. 所有管理写操作先保存脱敏审计意图；结果可检索或明确标记未知，补写失败不阻碍已受理的核心操作。
13. 管理依赖不可用只影响管理请求；核心创建、申请、TTL、销毁、到期清理、共享池与工作空间恢复不依赖管理 SQL、审计或历史。
14. Go 测试、PostgreSQL Migration 集成测试、前端测试和生产构建全部通过。
15. 核心 Redis 状态和 Runtime 身份为实时依据，SQL 只作旁路投影；历史缺口、来源与观察时间如实展示，不承诺无损历史。
16. Manage 使用独立资源预算和有界任务；关闭、初始化失败、慢查询、通知丢弃与重试均不引入核心等待、错误传播或健康降级。

## 21. 后续演进

Repository 边界允许未来新增 MySQL Adapter。届时必须：

- 为 MySQL 提供独立连接和数据库错误翻译；
- 在 MySQL Adapter 中于插入前生成 UUIDv7，并映射到适合索引的原生或二进制 UUID 存储；
- 验证所有 Migration 的 Dialect 兼容性，必要时为 Migration 增加受控的 Dialect 分支；
- 替换 PostgreSQL Advisory Lock 和管理幂等事务实现；
- 使用真实 MySQL 增加完整 Repository、迁移和并发测试；
- 保持 Handler、Service、领域模型和前端 API 不变。

未来还可以独立评估自定义 RBAC、SSO、在线 Pool 调整、内嵌日志与 Trace、管理后端拆分为独立服务等能力；这些不属于本次实现计划。
