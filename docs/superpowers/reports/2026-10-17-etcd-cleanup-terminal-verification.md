# Cleanup 释放后重启结算验证

本片支持已持久记录 Joined 的任务在 worker 重启后独立结算。先用原私有读取器认证真实 owner-release、终止/provider/enrollment 历史，再以独立收费轮次读取永久 Task、intent、link、初始 receipt 四点；不依赖已删除的 owner/control/runtime-index，不申请 TaskClaim、不打开物理目标、不创建或冒认原 Run。

四点必须具有相同 TaskBirth、Lease=0、Version=1、Create=Mod，canonical bytes、任务/初始 attempt 引用与 receipt committed 状态一致，并重新校验三份摘要和 incarnation。原运行任务和重启任务共用私有结算 CAS，保留原任务自身的 runID/effectsJoined 条件、真实 registry、W/T/I/R/E/S/release 全部凭据。结算为 44 个比较、两写、两 failure points，共 48 个节点；原 owner-release 满 64 节点事务不变。

范围为四个 etcd 产品文件和两个测试文件。历史认证集中在独立文件，actor 只接固定阶段，lifetime 保留结算所有权；共享内部事务与生命周期，保持同包以避免导出权限 DTO。已检查关键注释、逻辑分段、包职责及 gofmt/diff-check。

## 验证结果

- 修复前真实 Linux `recovery-release-red-01` 已证明释放事务成功但 ACK 丢失后，新 worker 仍不能结算。入口 host RED `terminal-history-red-01` 进一步复现畸形、未认证和错误任务 proof 被静默忽略，日志 SHA256 `ff2fc87ce46a6566cdfdd3ae72be4ec943146ce3bb3c6c707c328eb6f807bebd`。
- `terminal-history-host-01`：47 个测试组、138 个子场景通过，79.362s；9015 个输入。日志 SHA256 `29e18f7ba02bfe800a848046f2f410e2941a8bbf5e97e55b00e3a9833c34b2b3`。包含真实四点元数据正向及 40 个字段变异负向，不能将这些元数据用例描述为物理证明。
- `terminal-history-race-01`：相同 47/138 通过，86.845s，无数据竞争；9020 个输入。日志 SHA256 `8dfea5a312b68de1ba17f5f0ad7664ed7f16187ae4ff6c9e7147c39b2efec37a`。host 有 6 WARN（5 cancel、1 LeaseNotFound），race 有 11 WARN（10 cancel、1 LeaseNotFound），原始日志保留。
- 新 `terminal-history` 投影纳入实际 pre-Claim inventory，实测 10 nodes、request1049B、success17261B、failure564B；原闭集与全部 protobuf Size/节点断言保留，未扩大 128KiB/256KiB/64-node 限制。
- Linux arm64/CGO0 vet 通过（9106 个输入）；新 runner 构建通过（9271 个输入），ELF64 AArch64 SHA256 `3857b1ddfeb41ea438fc7e1baec90f13cfd994ed10c4392755a82b7454a28691`。每个 Go epoch 全部 17 类输入、源码/工具清单、前后/当前 SHA 和每个归档成员，均在后续 Go/源码修改前独立核对。monitor 没有 etcd 依赖、native 测试未再变化，其全部 2041 个当前输入与 ELF/buildinfo 重新核对后复用。
- **真实释放重启测试通过**：`terminal-release-native-01/release-settlement-recovery`，8.80s。真实释放事务 rev119 成功后扣留响应直至原 deadline；首 worker 返回未知、实际 Wait=nil。三条 ownership key 已删除，原 genuine proof 存在。独立新 worker 使用新 Backend/session，返回 settled/committed，原 Run 不变，重复结果一致，S 为空；第二 worker 只有自己的 session Grant，TaskClaim put=0、archive-command put=0，provider/bridge 计数不增加。两 worker 与 UID1000 目录清理子进程均实际 Wait=nil。日志 11064B，SHA256 `b4b2b9ecaa09964474fc0697c43aec5881f1e450e8403b273bb8e73674e01c3c`。
- `terminal-complete-native-01/complete` 正常成功链通过，5.67s：两次真实 Claim、归档内容/目录文件模式、运行时终止、安全释放、重复结果、due 推进65条及 provider1PATCH/1DELETE。原 Stop 返回 Unknown+Canceled，但实际 Done/Status、Backend.Close 和 worker Wait 均通过，保留原未知结果语义。日志 8917B，SHA256 `cf278ef97cb1ff2ffdf8b0c4d88ef432a32647bb07c62712c5e2c4764c12fb81`。
- 两轮 native 固定运行参数、工具归档、二进制/etcd 输入、父容器真实 attach0/Wait0、无 OOM/timeout、确切删除及空清单独立核对通过。release 有8WARN/3ERROR，complete 有8WARN/4ERROR及1条 gRPC warning；ERROR 为取消的 Range/fixture 关闭监听，未静默或描述成零告警。恢复 native 三个测试文件仍留待归档续接功能完成后一起提交，当前完整源码凭据在验证归档中。
- terminal-01 三节点 host 实际 Wait=[0,0,0]、无 OOM、容器及专用网络确切删除；实际 Compose/工具+生产者归档、各测试环境、最终清单独立核对通过。成员 WARN33/20/19、ERROR各4条关闭监听，日志保留。

## 尚未完成

归档已绑定但缺释放 proof 的物理续接仍保留占用；冷 unjoined、首次 Run 写未知也不会被冒认。observer ACK 恢复、跨实例 32/33 条真实 Task、close-join、部署 loader 与完整 Task 审查仍待完成；此片不是容量验收、生产默认切换或 Redis 移除验收。
