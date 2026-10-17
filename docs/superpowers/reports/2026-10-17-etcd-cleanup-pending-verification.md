# Cleanup：业务已完成、进度未提交后的恢复验证

本切片基于 `0cb580e5d0bc5d795ee5cc74dfd47feb2ac0dbfe`。完整 cleanup Task
和生产迁移仍未完成；此处只验收关闭、排空已有真实回执而进度请求未提交的恢复。

## 行为与职责

关闭完成后，原执行记录可能仍是 `claimed`，并保留完整 `pending.close`。
现在该阶段可以进入原有早期恢复候选路径，但必须认证原关闭历史，并逐字段匹配
Pending 的 purpose、Stage、Request、Attempt、digest、Claim 和 lease。
随后仍由原 Load/Query、固定 registry job、新 Run CAS 和原 managed Claim 接续。
缺失历史、未知结果、不完整或不匹配的 Pending 都不能取得恢复权限。

产品仅修改 `task_cleanup_local_early.go`，相应测试保留旧断言并增加 14 个子例。
存储内部历史匹配仍在原包；没有新增跨包权限接口，也没有复制物理执行链。
原事务形状、容量限制、运行时权限和进程退出要求保持原协议。

## 真实故障与结果

TLS proxy 在关闭或排空业务返回后，拦住一次 proposed progress Txn，**不转发到
etcd**，等待原请求 deadline，再返回实际 gRPC deadline error 并等待 handler 退出。
这与运行时 ACK 丢失、已提交进度 ACK 丢失是不同的故障。

第一 worker 的 fault matched/not-forwarded/joined 均为 1，committed/revision 均为 0。
实际执行记录除 `Run.Joined` 外与原 CAS value 完全相同，原 Pending 保持；父进程在
实际 OS Wait 成功后才启动第二 worker，使用原 Load/Query 验证真实回执。

| 实测 epoch | 结果 | 原始日志 SHA-256 |
| --- | --- | --- |
| pending-close-red-01 | 修复前真实失败，6.31s；新 worker missing-release-proof、0 Claim | `3c344fdadf2cadaf852afdc8b41448e55232371dd71e0445881cd57408ee1184` |
| pending-quiescence-initial-01 | 既有实现已通过，9.27s | `832374a80c2d3e826ac57a6ceef82c0853500049d4171ac10f3974f47d77c64c` |
| pending-close-native-01 | 修复后通过，9.18s | `179fe81648123596ce9f50033c3320a3a91515ef88a363c71f1a0dedbc1adc1b` |
| pending-quiescence-native-01 | 最新代码回归通过，9.43s | `2ed6d101bab46c7aa5f33112c8379ffd7dd2724cab2b7c2c95642768c47d48dd` |

最终两场均验证新 Run、两个真实 managed Claims、原命令/Stage 回执/Task 的 bytes
及 revision 不变、关闭不重发、已有排空不重发、fresh dual-drain、归档内容和 mode、
终止/安全释放/重复调用、settled E 和空 S、实际 provider/cgroup、两个 worker Wait=nil。
关闭场景首次准备排空时的额外 Stage guard Grant，仍逐项归属到实际 Claim/Attempt；
排空场景没有额外 guard。bridge upgrade 数量不作为 Query 次数。

## 定向验证与证据

- host/race：`TestCleanupLocalEarlyRecoveryHistory`、`TestCleanupLocalBoundManifest`、
  `TestCleanupLocalRecoveryPhase`，均为 3 组、83 子例；分别 1.528s、2.798s，无 DATA RACE。
  原始日志 SHA 分别为 `456aea9b9b3ca1769cbf59b99903ffa1b8e998e308df4d3e3a74a1963cbaa7ff`、
  `cbbbebc58af1148f4432803b2c1c9a22247e4419fc57656a460d867028d0e6c7`。
- Linux etcd、controlrunner vet 均通过。gofmt-d 和精确 diff-check 无输出；人工检查注释、
  逻辑分段与包职责。未重跑此前已验收的 12 组/98 子例及未变化的 wire 矩阵。
- 最新 runner 为 Go1.25.6/linux/arm64/CGO0/non-race ELF，SHA
  `5a6497cfe48670eae848670915da9cc23fa44f8e7ce9eb33779a5a4ea34d68e6`；monitor SHA
  `b38943838984e0b1634156664aec0eae93d0884c2638144e2cfbd7a15ecc0423`。
- 每个 Go epoch 均核对全部 17 类输入、源码/工具前后值及每份 archive；每次 native
  后重新核对当前二进制输入。每场独立审计真实容器配置、输入、工具归档、实际退出和清理。
  RED 的审计通过仅表示证据与清理成立，其 native_pass=false，未被记作功能通过。
- 所有实际容器无 OOM/超时，精确 CID 已删除，label inventory 为空；原失败证据保留。
  最终两场各有 8 条 warn（含实际注入 deadline、启动提示和清理 lease-not-found），
  3 条 error 均为实际 etcd shutdown 日志；无 gRPC WARNING。

仍需完成 `cleanup-complete` 但 owner 尚未释放的独立恢复，以及完整 Task 审查。
自动 pending 发现、生产入口接线、FUSE/collector、大规模性能容量、部署恢复和 Redis
移除属于后续迁移工作；本报告不宣称这些已完成。
