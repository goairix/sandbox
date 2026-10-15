# Enrolled launcher and publication transport

本单元落实已批准 master 的真实 launcher 生产接点，接续本地安全释放，不重新讨论 Redis/etcd 取舍。当前 cmd/sandbox-launcher/serve.go 只构造 NewSupervisor，管理 transport 只支持 exec/close/quiesce/archive；测试直接调用的 Enrollment/EnrolledPublicationProof 无法由真正 launcher 的调用者获得。本单元交付正式二进制的 enrolled 启动与只读 publication 查询，不将其称为 Pod 创建、main 切换或部署验收。

## Contract

1. 正式 `sandbox-launcher serve` 使用版本2 canonical protected config，新增必填 enrollment 对象，含规范非零 UUID node_uid/node_boot_id。enrollment socket 固定在 ControlDirectory 下的新常量路径，不接受环境/请求选择路径。拒绝 v1、缺失/null/duplicate/unknown/超界/错节点格式；不保留静默 legacy fallback。当前单环境尚未用此命令启动生产 runtime，旧镜像排空仍由最终切换负责。文件上限保持16384，UID/GID/MaxActive/clock/trust/contract 保留原严格约束，增加缺失 clock audience/contract 的前置合法性检查。
2. `serve` 在实际 BootstrapPID1 后只构造 `NewEnrolledSupervisor`，传原 protected trust/clock/UID/GID/kernel/executable/options及固定 node 身份。节点 UDS 仍须原 constructor/实际 SO_PEERCRED/nonce/certificate 验证；不调用测试入口或添加绕过 kernel 的 factory。拒绝不存在/不受保护的节点 socket；旧 control socket 不删除/接管。Serve/Close 的现有实际 borrower/handler/journal/signing join 保留。
3. 新管理 TLS request purpose `runtime_enrolled_publication`，只读、无 stdin、单连接单请求，字段固定 version/purpose/publication_certificate/claim。certificate ≤4096，保持原 publication codec 上限；claim 复用原 canonical ClaimReference。不得接受 activation/节点声明/成功 bool/任意执行动作。请求头≤32768，独立 length-framed 响应≤65536（enrollment 原请求+observer certificate+receipt+ready 的 base64 膨胀，不能受旧32768 Event cap 截断）；严格 canonical JSON、完整 EOF、unknown/duplicate/null/超界拒绝；不可改变旧 request 的严格语义。
4. response 固定 version/purpose/enrollment/ready：enrollment 为原 `EnrolledRuntimeEvidence` 的复制值，ready 为原 `EnrolledPublicationProof` wire。Supervisor 在当前真实管理 TLS 连接内处理，调用原公开方法/验证器，不自行签新格式、补种 journal 或将 diagnostics 作为权限。gate 已关闭/failed/closed、未完成 enrollment、错误 certificate/runtime/issuer 必须拒绝；响应不重开 gate、不刷新任何 etcd claim、不做 Pod/API/etcd 写入。报告若两只读快照间发生 Close，只允许失败或可由原 verifier 判定的已签历史结果；结果不是后续数据操作许可。
5. `(*controltransport.Destination).ReadEnrolledPublication(ctx, certificate, claim)` 返回 copied enrollment 与 ready bytes（最终精确 Go 签名在 plan 固定）。复用 origin-sealed Destination、原 pinned immutable TLS config、有限 Open、原 Session 一次 send/halfclose、独立 publication response mode 的上限与 EOF；原连接实际 Close/join，错误不重试。所有分配前做 raw cap，各嵌套证据仍服从原 codec 上限，不能仅检查外层总长。caller 仍须原 Backend PublishEnrolledRuntime 验证真实证据与当前 native guards，此方法不能成为 copied authority。
6. 无新增 idle goroutine/ticker/每已有sandbox map或轮询。沿用 Supervisor16连接、Destination5s有限 IO，Close/取消不能留下新借用者或提前退款。既有 signer原生 join 和 clock范围不放宽。不导出任意 transport/成功回调/receipt signer。

## Evidence and boundaries

一个 fresh author、一次 fresh 完整 Task reviewer，同 pair 修复；小步源码提交，不将整项压成一个提交。新增 bounded frame/transport/config RED→GREEN；实际管理 TLS 正例及 wrong identity/cert/closed gate/oversize/trailing/取消拒绝，计数原请求与实际 join。必须新增有限 Linux fixture：真正构建的 cmd/sandbox-launcher 二进制作为隔离 PID1（不是测试函数代替 serve），真实 protected config、签名 clock、独立 observer 的实际 UDS enrollment/activation，经过真实新 TLS request 获得原 verifier 可验证的 enrolled publication；关闭与错 node 至少一个有限拒绝案例。可复用既有 native helpers 的源代码，不能重放已验收矩阵/借用 CLOSED OWN 产物。遇真实内核隔离限制如实报告，不换 fake source。

仅已有批准的私有 Linux fixture：network none/private cgroup+PID+mount namespaces/read-only root/no-new-privileges，512MiB/2CPU/256PID，原白名单 caps，无权限扩张、hostroot/controller 修改、secret/liveRedis/liveKubernetes。每作者≤1/总≤2，保留原 Cmd.Wait/attach/containerWait/exactcleanup、所有失败日志、全17字段 GoList 输入 pre/post filename/SHA 与实际 race/GOOS/CGO一致，binary/sourcearchive绑定。源码/测试/文档/格式只 apply_patch；raw logs/JSON/binary/diff/brief例外；不读 CLOSED OWN，不碰 Sentinel，不升级/push/deploy。

不修改普通 Pod 创建或已经验收的 cleanup profile 以允许未知 control mounts：正式 Kubernetes 如何安全提供 protected config/clock/enrollment UDS、节点 agent/cgroup 委派、RBAC/证书维护与 operator 部署是后续具体 driver 单元，必须整体满足同一真实 profile。不得将独立 CLI 隔离 fixture 称为真实 kubelet或main迁移完成；容量与长期维护仍属 master。
