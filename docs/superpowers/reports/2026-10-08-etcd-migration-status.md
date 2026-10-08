# etcd 迁移整体状态

本次目标仍是主方案阶段一至五完整替代；多 cell 是后续扩展。当前主程序仍连接 Redis，不能宣称生产切换完成或线上性能已经提升。

已验收组件包括原生 etcd 元数据与永久 owner/事务回执、原始 operation/claim 和命令 fencing、运行时关闭与用户排空，以及实际 workspace 归档生产者。最新两个功能提交：`b5bdca2` 封闭 TLS 归档传输接口；`f1e3086` Supervisor 原始生产者、独立截止时间、历史完成查询和物理关闭等待。对应验收见 `2026-10-08-supervisor-workspace-archive-verification.md`。这些组件验收不等于整条生产链路完成。

新增已验收功能提交 `bd46b2b`：后端私有归档接收器，含原始 spool 的磁盘/索引/并发额度、真实 TLS 数据与签名/TAR/EOF 校验、物理关闭等待和保守退款。限定新测试/race、包 vet、Linux arm64 编译通过；独立 review 无阻断项。验收见 `2026-10-08-backend-workspace-archive-receive-verification.md`。这仍是接收组件，尚未完成当前 KV 所有权约束的发送链路。

新增已验收功能提交 `90d23d0`：原始归档命令签名、不可变 etcd 记录、当前 claim 与历史排空身份绑定、固定有效期和保守响应丢失处理。新 host/native race、vet、Linux arm64 编译通过，独立 review 无阻断项。见 `2026-10-08-backend-workspace-archive-command-verification.md`。长会话接线是紧接着的工作，目前仍未发送归档命令到实际生产运行时。

后续按以下依赖顺序推进：

1. 后端原始 final-sync 发送者：当前 claim/KV 发送屏障、长接收期间续约、已验收接收器的接线及归档消费前的当前所有权检查。签名与不可变归档命令已完成组件验收。
2. 每个真实远端请求（包括 SDK 重试/跳转/分片）的意图、fencing、确定结算或 sticky unknown；结合实际运行时/基础设施终止，完成安全释放链路。
3. API、Pool、Docker/Upload 与主程序生产接线；分区 due/dirty 调度，移除全量扫描和按已有沙盒常驻的控制循环。
4. FUSE/collector/可信同步、长期空闲运行时按需证书维护；验证 N 增大时 idle 成本与活跃工作背压。
5. 大量已有沙盒的完整容量/性能验收、etcd 部署/备份恢复/旧版本排空，最后移除 Redis 并做整分支审查。

当前无外部阻塞，剩余工作仍然较多。容量验收须测量实际 MVCC/WAL/复制、调度/API/FUSE 成本和大 N 空闲行为；现有组件测试与编码大小样本不能替代。每个连贯改动继续独立验证、审查和提交，源码修改使用 apply_patch。
