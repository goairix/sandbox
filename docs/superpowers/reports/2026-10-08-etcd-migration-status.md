# etcd 迁移整体状态

本次目标仍是主方案阶段一至五完整替代；多 cell 是后续扩展。当前主程序仍连接 Redis，不能宣称生产切换完成或线上性能已经提升。

最新已验收源码 `7fd87c1`/`a483a7b`/`e398f41`：v1兼容/v2严格持久checkpoint、进度与generation GC原子64节点提交、unknown同点只读和Close等待。成功覆盖39不同native叶、最终host race14叶，65文件属于准确记录的提交前snapshot；较大批次14PASS/23FAIL和SIGKILL原因UNKNOWN仍保留，不称整轮可靠性通过。唯一review Missing0/Extra0/C0/I0/M1 Approved；Root354 artifacts、12源码绑定/9binary/目标包231编译输入、8环境24原始child join及独立清理通过。见 `2026-10-09-workspace-sync-checkpoint-verification.md`。正常恢复研究已形成，下一步补有预算归档持久manifest、重新打开与新权限接线；该研究不是恢复验收。主程序仍Redis，安全释放、生产接线、分区/FUSE/collector、大N容量、部署恢复与Redis移除均未完成。

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

新增已验收 `a6d340c`/独立审查修复`9e49f60`：独立 recycling profile、严格free/occupied/terminal slot编码、只读operator配置pin与新旧入口隔离。最终scopedhostrace149叶/真实etcd metadata13叶PASS、vet/cross/format通过；限定复审C0/I0/M1（server环境告警按限制披露），Root167artifact/302source/3336inputs/10workingGitcompiled绑定与两轮exactcleanup核对。详见`2026-10-09-workspace-sync-recycling-profile-verification.md`。这还没有实现slot复用：当前接续generation reserve/local gate，再持久terminal与GC，旧永久收费记录仍保留。主程序仍Redis；安全释放、生产接线、分区/FUSE/collector、大N容量、部署恢复和Redis移除均未完成。

新增已验收源码`7391a5e`与修复`27feaf6`：原始未读body的持久put意图、固定分区slot跨Backend共享背压、未知响应不重发与出生pin诊断。基线native38实际叶子零跳过/警告；独立review发现空body已完成读取的缺口，针对性RED→GREEN修复及复审通过，剩余C0/I0/M0。详见`2026-10-08-workspace-sync-intent-verification.md`。这是native元数据组件，实际可信worker/provider派发和结算未完成，不能把归属记录当OS UID或安全释放证据；S1..64的逻辑预算上界不等于实际MVCC容量测试。

新增已验收源码`cdb5612`/脚本修复`b75fbeb`：实际非特权worker的原始kernel/process身份，非零UID/GID、零cap、NNP、固定boot/start/ns/exe及不可复用incarnation。host34实际模型叶、五个实际native容器和原始子进程身份通过；独立review发现attach超时句柄丢失，独立修复并以三真实child生命周期场景/单nativepositive和限定复审验证，剩余C0/I0/M0。详见`2026-10-08-workspace-sync-worker-verification.md`。它不能代替Deployment/凭证隔离、物理provider target、native派发与终态证明；下一步继续这些实际IO接线，主程序仍Redis。

新增已验收源码`fac3953`/`7bc4ff6`与review修复`6b411a0`：原始非特权worker、只读local root FD与v2永久意图绑定，只有原始明确ACK/postguard可进入一次性native写前门；保留50历史pins，实际59nodes/11517requestbytes/2629replybytes。原native21叶、修复15实际local叶、真实超时保留→释放→原attach Wait→独立reap通过，限定复审C0/I0/M0；失败试验和server日志完整保留，启动错误归类已纠正。见`2026-10-09-workspace-sync-local-gate-verification.md`。当前开始实际本地文件写入/hash/fsync/Close与进程内witness；持久终态/退款、安全释放、生产接线、分区/FUSE/collector、大N容量与Redis移除仍未完成。

新增已验收源码`96c21cc`/`2f59c93`：实际local regular-file有界写入/hash/fsync/Close、原始进程内witness、截断前身份拒绝、mode0000与8路物理关闭等待。24不同实际Linux叶同源分批PASS/零SKIP（23+1），完整轮旧setupdeadline失败如实保留，不能称整轮全绿；host限定race14PASS/24SKIP及vet/cross/format通过。独立reviewC0/I0/M0，Root426artifact/16source/两个Go编译快照/42exactinventory核对通过。见`2026-10-09-workspace-sync-local-put-verification.md`。永久intent/slot仍charged，下一步原生持久terminal与有预算回收/恢复；完整sync、安全释放、生产与大N性能验收仍未完成，main仍Redis。
