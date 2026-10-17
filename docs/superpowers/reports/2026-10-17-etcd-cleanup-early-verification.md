# 已关闭与已排空 Task 的 joined 重启恢复

本片修复原执行进度已确认 closed/quiescent、原 Run 已实际 joined，但尚未归档
时的新进程恢复。此前消费者只支持 archive-bound/local-complete，遇到这两种
状态会返回 retained/missing-release-proof，不申请 TaskClaim，也无法继续清理。

现在同一个有界 registry job 先调用原历史 Load/Query，核对完整 Task、birth、
摘要、原 Stage、reservation、pending 与 E/S 原生 fences；取得真实 receipt 后，
回到原 actor，用一次新 Run CAS 和原预算 managed Claim 接续。取得新 Claim 后
再次原 Load/Query，拒绝历史变化；旧 Close 不重发，已有 quiescence 不重发。
仅原 quiescence 历史和 reservation 都不存在时，才首次 Prepare/DeliverQuiescence。
后续使用原 fresh dual-drain、归档、实际 Received.Close、第二 Claim rollover、
本地同步、终止、安全释放与独立结算。没有省略 whole-operations-prefix 新观察。

历史加载/Query 位于同一最多32个 job 的 goroutine，没有 actor native owner
跨 IO。原 LoadQuiescence 的4次RPC保持完整，不放宽为 actor 2次RPC回合。新 Run
仍只有一个候选，unknown不重试新身份、不补写旧 Run joined；原 pending 在真实
历史确认前保留。原 unjoined、未知且无认证 bound source、reservation-only 等
边界仍拒绝/保留。状态诊断不代替 receipt 或新 Claim 的实际权限。

包仍保持原事务、Claim、registry所有权私有。新 early 文件负责历史匹配和接续；
normal Close/Quiescence 与共用归档链按完整职责拆函数，没有导出权限 DTO。
原 bound/observer caller 保留各自 gate，仅共用同一原43节点 Run事务构造。Root
人工检查注释、不确定结果、join顺序、逻辑分段和包依赖，gofmt/diff-check通过。

## 定向 host/race 与 wire

固定 Go1.25.6，host/race darwin/arm64 CGO1，Linux vet arm64 CGO0。精确12组为
EarlyRecoveryHistory、SlotsWire、BoundManifest、RecoveryPhase、TerminationReservation、
RecoveryRetains、ReleaseSettlement、ProgressUnknownJoin，以及原 DueWorkerCleanup
SharedBudget/Fairness/StopJoin/ClaimRollover。未重放不受影响的52组旧矩阵。

| Epoch | 结果 | 输入数 | 日志字节 / SHA256 |
|---|---|---:|---|
| early-host-01 | 12组98子例通过，包36.997s | 9022 | 23134 / e3403c92187413cdd97fc529bcf3baf3591701c73bc6432e061f0ace45de2182 |
| early-race-01 | 12组98子例通过，包39.682s，无数据竞争 | 9027 | 24391 / aa765755a1c37c6008cca2ddf31dcd0c94197116a99e5e39081584f9ec729e37 |
| early-vet-01 | etcd Linux vet通过，无诊断 | 9113 | 0 / e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 |
| early-native-vet-01 | controlrunner Linux vet通过，无诊断 | 9287 | 同上空日志SHA |

新增实际 wire：recovery-early-run为43节点，请求11905、成功96、失败564字节；
early-history为12节点，请求1200、成功/失败最大各74723字节。后者包含已存在、
必须拒绝的最大原记录，未只测空响应。原64节点/128KiB请求/256KiB响应上限不变。
pure host匹配测试只验证归属/拒绝与ABA，不伪造 Kernel、receipt或物理执行权限。

使用 fresh early-01 真DockerCompose三节点，原镜像/资源/私有cgroup/只读root/
dropALL/NNP/loopback端口合同；实际Compose及工具归档已核对。结束时原工具执行
SIGTERM和三个实际Wait均0，无OOM，确切CID和网络已删除，标签清单为空；独立
literal/producer/Compose工具归档/两次host环境/实际退出清理审计通过。每个成员
17WARN，ERROR4/4/5为服务关闭及一项取消Range；保留原记录。host无WARN，race
有3条取消WARN，无FAIL/SKIP/DATA RACE。

## 真实双进程与直接影响回归

新selector仅 closed-progress-recovery、quiescent-progress-recovery。原固定镜像、
network none、六cap、只读root、内存/CPU/PID、三挂载、七env和100/120/150/180s
期限不变。原真实TLS gRPC代理只扣已成功提交的E.progress ACK，明确不是runtime
TLS业务ACK丢失。首worker实际Query取得close/terminal quiescence receipt、实际
工作join且OS Wait=nil，原E/S仍occupied、Archive=nil；父保存完整Task、原命令、
Stage receipt及reservation字节/revision。第二worker真实新进程执行新Run和两次
managed Claim，不改旧command/receipt/Task；新归档、内容/模式、最终结算/repeat、
provider1PATCH/1DELETE、observer/PID1/cgroup及所有实际join保持原完整断言。

实现前两次真实行为RED：early-closed-red-01失败6.38s，第二worker为
missing-release-proof/retained/0Claim，原始12670字节，SHA
`ee8b342ddd0ca0679b32f5907252ec003bcbf729baa910a879cd5ef2623720ad`；
early-quiescent-red-01失败6.57s，同一缺口，12985字节，SHA
`72277955271aff1c0d2036606eb0c4b4dcfd80e3b53b064bf2edadb0b5717f92`。
两者首worker已有真实receipt/RunJoined/Waitnil；实际失败退出、proxy/清理worker/
supervisor/etcd的Wait及容器清理全部保留并独立审计，未把故障结果记为成功。

首个实现版 early-closed-native-01 已执行到settled/committed，但整个用例仅因新
Grant计数断言3而实际4失败；13361字节，SHA
`c6211f3a2cd8cf580de91568bef0195452dd3c9fe556c7eb8fd77dbeecefde4d`。
源码确认首次创建quiescence还需要原 Stage guard lease。只修新closed/second
断言，其他旧计数不变：严格验证1session+2不同TaskClaim lease+1原排空guard，
guard实际成功PUT、LeaseID/AttemptID/RequestID与首Claim、最终quiescence Attempt
逐一对应。观察各限4条并报告溢出、snapshot深复制；没有产品修复或统一放宽Grant。
该失败也完成独立输入/环境/工具/实际退出清理审计后，才授权测试修改。

最终两程序 fresh build02：runner9287输入，ELF64AArch64 SHA
`2ffd5dcb16d2f5a3de95fed1bb1c8945373abd71664d652c54a604504fdbf086`；
monitor2045输入，SHA `aebff24f86ffdfa717e30f20be9d0a490624b9740a43f8cdfc62c8673a054500`。
确认Go1.25.6/linux/arm64/CGO0/non-race、实际hash和buildinfo。每次后续Go、native
或源码/工具修改前，独立核对全部17类输入、前后SHA、当前源码/工具及每个归档。

| 最终native epoch | 结果 | 日志字节 / SHA256 |
|---|---|---|
| early-closed-native-02 | 通过9.29s；guard归属、新Run/两Claim、旧Close保持、实际完整链 | 14244 / 2c1f2b690f685966390c81efec2903941113810f1c38fadefec0e26ef639ab53 |
| early-quiescent-native-02 | 通过9.33s；旧Close/Quiescence与全部历史保持、完整链 | 14242 / 3d7cc1e770aead7d1a3b242fb9dfc1757c2f66e751787c7c5fc3249424cb5252 |
| early-mounted-native-01 | 通过5.79s；本次Close/Quiescence职责提取影响的普通完整链 | 5891 / 7164dcfe2768129fc40b0feeb5a0ec89daa2cd88df77342af76eb639fdb18d27 |
| early-bound-native-01 | 通过9.15s；本次共用Run CAS提取影响的bound恢复 | 13036 / beebbcfb9409fb1e603937e9dc85b919ee0f1b032abf308d49b1dfeee8bb17c4 |

每例实际attach/Wait均0，无OOM/timeout；proxy handler/pump、原OS worker及清理
实际退出，确切容器已删除且标签清单为空。独立literal runtime、输入、工具归档、
退出和清理审计native_pass=true；每例后两程序的当前17类源码/工具/归档再核对
通过。每例保留8条JSON WARN、3条关闭ERROR，无gRPC WARNING，不宣称零警告。
旧预言修复前quiescent-native-01通过9.18s的证据也保留，最终验收采用上述current版。

没有从TLS密文或bridge upgrade数冒称Query次数；凭原Query真实结果、不可变命令
与代码无旧Deliver/Prepared路径验证此边界。本片不是runtime业务ACK丢失覆盖。

完整Task仍OPEN：claimed+pendingclose、closed+pendingquiescence已有committed
历史但确认进度未落盘的恢复，以及cleanup-complete owner-held恢复仍须实现，
然后唯一完整Task审查。后续生产发现/共享调度/worker角色、业务/FUSE/collector/
远程同步、部署恢复、大N性能容量与Redis移除仍未完成，不能以本片代替生产验收。
