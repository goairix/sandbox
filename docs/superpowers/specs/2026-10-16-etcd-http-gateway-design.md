# etcd plain HTTP 业务入口

已批准总迁移的下一完整业务接入单元，基线 `4cb308b6629b6621367d8e1b312241c5bbd228c6`。冷创建/业务幂等、Lookup、实际 Exec 与 mounted loader 已分别验收；不访问旧 OWN/pair，不重做旧单元。本任务提供实际可启动的受限 plain HTTP 模式，不冒称全功能替代或容量/生产部署完成。

## 启动与范围

新增显式 `sandbox --serve-etcd /etc/sandbox/deployment.json`，在 legacy config/Redis/旧 Manager 初始化之前选择此路径。与其他显式 startup flags/位置参数互斥。沿 mounted Deployment.Open、同一个 Service/Backend/signer、固定 principal/profile 启动 HTTP；失败不能回退 Redis。旧默认启动路径保留到后续完整切换，不在本任务部署或修改运行中的业务。

manifest 可选新增严格有界 HTTP 配置；旧只读 startup check 的 manifest 兼容，serve 模式要求完整 HTTP 配置。明确 host/port、mounted API key、固定执行环境/默认和最大执行期限；私钥继续由原 owner 管理，不新增私钥来源。API key 非空、从有界 mounted 文件读取，错误不泄露内容；沿已有单 API key/固定服务 principal 语义，不引入多租户或用户可选 authority。具体字段在只读源码预检冻结。

四条实际业务路由：POST `/api/v1/sandboxes`、GET `/api/v1/sandboxes/:id`、POST `/api/v1/sandboxes/:id/exec`、POST `/api/v1/sandboxes/:id/exec/stream`，以及 health/readiness。重用现有 v1 请求/响应结构、language command builder 和认证/限流语义的可重用部分；不能复制整份旧 Handler 或塞 nil legacy Manager。其余 workspace/file/upload/network/TTL/destroy/oneshot 等尚未迁移路由在此模式明确返回 typed unsupported（501），不能走 legacy Manager、raw Pod exec、Redis 或假成功。文档列出限制，后续继续实现，不能将此模式当作总需求完成。

## 创建、查询与错误

- 创建真正调用原 CreateBusiness，Idempotency-Key 可省略（原 service 生成），响应始终回显合法实际 key。固定 principal 来自部署。相同 key 重试沿原 stable IDs/配置恢复，不重造 snapshot/default；冲突409，不确定/pending 返回202/原 key、有限 Retry-After 和可定位 ID，客户端以同 key/原请求继续 POST 恢复。不得发明未实现的 request-ID 索引或 N 扫描。
- HTTP JSON 解析有总字节/结构限制，拒绝未知/重复字段、非法参数和不支持的 profile。不得静默忽略 workspace/dependency/permanent/network/resource 等旧字段；当前支持范围与 service 严格一致。
- GET 调用原 Lookup，返回其 owned 业务 metadata、原 CreatedAt/原配置及当前 expiry。成功的 state 映射须说明其仅代表 active/健康运行时，不能伪造全局用户进程 busy/idle 观察；具体兼容映射由预检冻结。历史创建完成不等当前健康，不能用旧 receipt 代替 Lookup。
- 错误统一公开 code/status/Retry-After：非法400、认证401、not found404、幂等冲突409、容量/忙429或503、unknown 明确非自动重试；不输出 key、环境内容、签名或完整内部请求。原 operation/effect reference 只作 authenticated unknown 的诊断，不变权限。

## 执行业务映射

沿现有语言/code→command 语义，明确 `/bin/sh -c`、workdir 默认 `/workspace`、固定 launcher UID/GID，完整环境为 operator 基础环境加请求 env，不继承 API 管理进程环境。timeout0使用部署默认、负值或超过配置/原 descriptor 最大3600秒拒绝；TTY/LineBuffered 当前无对应 PTY 实现时明确拒绝，不静默降级。RequiresNetwork 遵循 fixed profile。同步和 SSE 都只能调用原 service.Exec，不能 Lookup DTO 授权或再次创建新 operation 重试。

每次 Exec 请求产生新 operation，X-Request-ID 只追踪；不把 Idempotency-Key 当执行去重。如果带该 header 必须在下发前明确拒绝，避免 Go HTTP transport 将其视为可自动重试的 POST。unknown/断连只保留原 refs 和明确错误，绝不返回默认 exit0 或自动重发命令。只有 verified local_terminal 才按原 wait status 生成 exit code；非零退出仍正常业务结果。timeout/失权原因需按原真实 receipt/error 区分，不能凭本地取消推断远端已结束。

同步输出有明确有限合计内存额度，超界停止本地消费并保留 unknown/ref，不伪造成功。SSE 保持现有 stdout/stderr/done/error/ping 形状，只有原 verified terminal 可以 done；用有界通道/背压连接原同步 writer 和 HTTP 发送，客户端断连、写失败或超时取消并等待原 Exec/worker/reader owner。真实 ResponseController 写期限必须可作用于底层连接，不能忽略不支持的 deadline 后声称慢客户端已可取消。心跳不能导致无限排队，所有辅助 goroutine/ticker 在 handler 结束前 join/停止。

## 总预算和关闭

HTTP 业务请求有固定总并发预算（至多32）、有界 body、有限 read/header/write/idle 期限，不按 N 建常驻对象。service 原总 MaxActive 保留，不另建每 workspace owner/map/后台轮询。health/readiness 不采样全部 sandbox；readiness 保持有限 etcd identity/服务生命周期检查，无用户 exec。

信号/失败关闭流程：先停止接受新 HTTP 请求并取消原请求 context，使 SSE/Exec 真实退出；等待 HTTP handler/原 stream owner，再 Close 原 Deployment。Shutdown 的本地超时不能伪装 handler 或任意 writer 已 join；实际 socket 关闭与原 Wait 必须被消费。只读启动检查仍不启动 HTTP/不注册 issuer。

## 验证

新增窄 parser/业务映射/CLI 分支 RED 后产品实现，handler unit 可用最小业务边界假件验证 wire/错误和背压，成功运行时行为必须由实际 native 验证。唯一新 native selector 在真实 TLS etcd/正式 launcher/原 bridge 上启动实际 HTTP server：认证拒绝、幂等创建/重复/冲突、GET真实健康与原时间、同步非零退出、SSE真实输出/终态与断连取消、unsupported 路由无副作用、server取消/关闭及全部原 Wait/IO join。无需重复32秒执行测试或旧矩阵。

Root 固定 Go1.25.6、apply_patch 唯一编辑、精确小步提交；Sentinel禁止。新 OWN/pair、外部完整 module cache 与已验证外部 etcd3.6.14 fixture可复用，禁止访问旧 OWN。每 action 实际 GoList17/platform/race、源码/脚本前后与逐 archive 成员；Docker实际literal预检、有限隔离、真实Wait/精确清理。一个 fresh reviewer，修复同pair；验收后继续未实现 API/调度/FUSE/容量/部署/Redis移除。
