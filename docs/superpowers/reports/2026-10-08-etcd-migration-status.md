# etcd 迁移整体状态

本次目标仍是主方案阶段一至五完整替代；多 cell 是后续扩展。当前主程序仍连接 Redis，不能宣称生产切换完成或线上性能已经提升。

已按用户要求合入基分支修复 `52b4d4f`、`d543cf3`，合并提交 `7bca73a`。无冲突；三个受影响包 race/vet/Linuxarm64CGO0编译通过。见 `2026-10-08-base-quiesce-fixes-verification.md`。

新增已验收功能提交 `61fe19a`：原始归档消费owner、当前native权限、有界TAR/PAX私有body和所有关闭路径的物理等待。实际native24项与host5项通过，native零跳过/零警告，vet/Linuxarm64编译通过，独立review无问题。见 `2026-10-08-workspace-sync-source-verification.md`。原始45秒会话实际过期后继续消费、真实续约已测；当前重复整归档SHA存在多文件IO成本，尚不是性能/容量或安全释放验收。

已验收组件包括原生 etcd 元数据与永久 owner/事务回执、原始 operation/claim 和命令 fencing、运行时关闭与用户排空，以及实际 workspace 归档生产者。最新两个功能提交：`b5bdca2` 封闭 TLS 归档传输接口；`f1e3086` Supervisor 原始生产者、独立截止时间、历史完成查询和物理关闭等待。对应验收见 `2026-10-08-supervisor-workspace-archive-verification.md`。这些组件验收不等于整条生产链路完成。

新增已验收功能提交 `bd46b2b`：后端私有归档接收器，含原始 spool 的磁盘/索引/并发额度、真实 TLS 数据与签名/TAR/EOF 校验、物理关闭等待和保守退款。限定新测试/race、包 vet、Linux arm64 编译通过；独立 review 无阻断项。验收见 `2026-10-08-backend-workspace-archive-receive-verification.md`。这仍是接收组件，尚未完成当前 KV 所有权约束的发送链路。

新增已验收功能提交 `90d23d0`：原始归档命令签名、不可变 etcd 记录、当前 claim 与历史排空身份绑定、固定有效期和保守响应丢失处理。新 host/native race、vet、Linux arm64 编译通过，独立 review 无阻断项。见 `2026-10-08-backend-workspace-archive-command-verification.md`。

新增已验收功能提交 `4e3c3c3`、修复 `d97d221`：当前 claim/KV 发送屏障、长接收期间真实续约、接收额度背压、接收后新鲜权限检查及物理关闭等待。最终新 native race15项通过，vet/Linux arm64编译通过，独立审查与针对性复审无未解决项。见 `2026-10-08-backend-workspace-archive-delivery-verification.md`。测试使用真实 etcd 和 TLS 协议 fixture，尚未接到实际生产运行时或 provider，不是安全释放与容量验收。

新增已验收功能提交 `4ca3e45`：不可变规范同步计划绑定实际 workspace identity、排除规则和删除策略；新预留入口在错误目标时、查询/写入前拒绝，且不消耗原始申请。新 native race8项通过，vet/Linuxarm64编译通过，独立审查无问题。见 `2026-10-08-workspace-sync-plan-verification.md`。计划仅为结构归属，当前原生 provider 物理身份和消费接线仍未实现。

后续按以下依赖顺序推进：

1. 原始归档消费者与规范计划已验收；继续实际可信provider worker、物理目标校验与有界body接线。接线研究见 `2026-10-08-trusted-workspace-sync-preparation.md`；重复归档完整性扫描必须在容量验收前优化。
2. 每个真实远端请求（包括 SDK 重试/跳转/分片）的持久意图、fencing、确定结算或 sticky unknown；本地 provider 的可信物理 IO 证据；结合实际运行时/基础设施终止，完成安全释放链路。当前归档能力只能保守拒绝重建，正常 manager 故障恢复仍需实现和实测。
3. API、Pool、Docker/Upload 与主程序生产接线；分区 due/dirty 调度，移除全量扫描和按已有沙盒常驻的控制循环。
4. FUSE/collector/可信同步、长期空闲运行时按需证书维护；验证 N 增大时 idle 成本与活跃工作背压。
5. 大量已有沙盒的完整容量/性能验收、etcd 部署/备份恢复/旧版本排空，最后移除 Redis 并做整分支审查。

当前无外部阻塞，剩余工作仍然较多。容量验收须测量实际 MVCC/WAL/复制、调度/API/FUSE 成本和大 N 空闲行为；现有组件测试与编码大小样本不能替代。每个连贯改动继续独立验证、审查和提交，源码修改使用 apply_patch。

当前实施单元为`2026-10-08-workspace-sync-intent.md`：原始body的持久put意图、固定分区slot共享背压与未知响应仲裁。它仍是native元数据组件，实际可信worker/provider派发和结算未完成；不能把本单元归属记录当作OS UID证明或安全释放证据。
