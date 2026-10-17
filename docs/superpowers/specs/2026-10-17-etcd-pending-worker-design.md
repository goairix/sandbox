# Pending cleanup 自动发现与共享 worker

这是已批准 `2026-10-06-etcd-state-management-design.md` 的实施细化。
用户要求继续完整迁移、加快执行、小步提交，且大量已有沙盒不能引入逐沙盒控制器。
前一单元 `2026-10-17-etcd-cleanup-executor-acceptance.md` 已验收明确引用执行器；本单元
只把永久待办发现接入它，不重做物理协议，不把默认入口或 Redis 移除算入本单元。

## 目标与约束

worker 自动发现自己分区中的真实 cleanup Task，复用原 DueWorker 的 session、partition、
Range/Claim/Relist 份额和 32 registry jobs，把候选交给既有严格执行管线。
Watch、索引和诊断引用都不构成执行权限；原 Task/intent/receipt/owner/slot/Run/Claim
仍在实际执行时重验。未知效果不因取消、失租、超时或 404 归还永久槽。

保持原 256 分区、全 cell 4 Relist、20MiB/s Range、100 claim/s 和持久 32 任务限制。
单事务至多 64 个节点，请求 128KiB、响应 256KiB。缓存/队列纳入原 worker 的
64MiB/10000 keys 范围，不另建按 N 增长的 goroutine、Lease、Watch 或全量镜像。
Stop、Backend.Close 先 seal，实际 join actor、原 native 调用和物理 jobs，最后释放依赖。

## 待办证据与恢复

新任务的永久发现证据必须随原 Task/cleanup-intent 创建事务原子写入；不接受松散后写
索引或只在本进程 admission 成功后投递。创建 ACK 丢失、启动前积压和 destroy 入口均
必须可发现。新 schema/键、精确事务预算和兼容边界须在只读 preflight 冻结后实施。

已完成记录不能无限留在热 pending 集合。移除发现证据须绑定确切原任务终态与占用，
保持已有满 64 节点的 owner-release 原事务完整；不得为了索引删除削弱其比较。
未知的物理效果继续保留永久证据。旧任务缺索引时必须明确兼容/回填策略及有界成本，
不能静默跳过，也不能把每轮历史全表扫描描述为 O(实际待办)。

发现采用固定 revision 分页及可信续接；断流、compaction、缓存超限、旧 session 或
分区失权时停止从不可信视图调度并按原预算恢复。一个坏/未知/繁忙任务不能永久阻塞
后续可执行任务，也不能通过热循环反复消耗预算。重试位置、退避与状态必须有界。

本单元先交付有限分页轮询，不新增第二套 Watch；pending Watch 的 R+1 衔接仍属于
后续生产调度交付，不能记为主方案这一要求已完成。周期扫描仅针对未结算 pending，
下一轮不早于上一轮结束后 30 秒；不承诺固定任务开始时延或扫描成本与 pending 数无关。
旧任务仅在停止旧写入后，做固定 R、持久游标的一次回填；完成后不周期扫描历史。
不支持旧版无索引写入者继续运行的混合版本发布，回填保证限于该固定视图和新原子写入。

历史回填细化为不可变 legacy 截止修订、可替换读取视图及已确认 CAS 的单调 frontier。
压缩使读取游标失效，但不清除已确认进度：重新认证读取视图后，从原 frontier 继续。
这一兼容证明要求旧 Task/intent/link/初始回执在回填完成前不被 GC、删除或改写；恢复
epoch 变化不能采用旧检查点。截止点之后的合规任务已有原子 pending，不属于旧集合。
单次进度事务只消费从该检查点frontier开始的连续前缀，最多一个旧任务；没有有效终态
证明时用原子pending覆盖。非空EOF只有在同一事务覆盖全部返回行时才可写complete；
包含两条旧任务的页处理第一条后仍未完成，必须从新确认的frontier重新读取。
读取改用平坦 cleanup-intents 家族，避免任务执行后代；每次最多2条 keys-only，并按
底层额外一条、每条旧通用64KiB上限计入读取预算。不能用 revision 过滤器代替有界读取。
这避免每次压缩从头回填，不保证无限新任务涌入或始终无法完成一页时的固定完成时间。

## 生产组合边界

沿用 mounted cleanup 配置、真实 KernelBoundary、已有 issuer 和 transport/provider。
本单元应交付可调用的自动 worker 启动/关闭组合；独立进程角色的启动位置、严格配置和
实际 Kernel 生命周期由只读 preflight 从现有入口确定，不接受假 Kernel 或公开任意
callback。如果该入口可独立验收，应拆成下一任务，不把未实现角色算作发现完成。
无默认后端切换、共享分支 push、集群部署或安全身份扩权。

## 验收

真实 etcd 验证启动前积压、进程替换、原事务 ACK 丢失、分页/compaction、重复候选、
同 session 的续租及 due 公平性、同 cell 持久 32 槽、坏任务不阻塞以及实际 Close join。
至少一场真实 Linux 自动发现到物理释放完整链，不能显式调用 Execute 代替发现。
每个连贯切片测试后提交；唯一完整 Task spec/quality 审查，修复轮仅复核对应差异。
原始失败、输入与工具、退出及清理证据保留。小规模组合不等同大 N 性能或生产验收。
