# Local runtime termination and safe owner release

本单元细化用户已批准的 etcd 迁移安全回收路径；继续既定小步提交和完整功能单元审查，不重复申请 master 授权。它消费已完成的本地完整同步、node observer 和 enrolled publication，交付真实终止、精确基础设施清理及原子释放的组合，而不是另一个 metadata-only release。

## Scope and public boundary

1. 支持 ordinary Kubernetes、local-provider sync、enrolled plain/no-mount 的精确 runtime ID/UID/BootID 与 node UID/boot。此入口拒绝 legacy、Docker、FUSE、cloud、none 和未知挂载/资源合同；这些仍是 master 后续工作。不接 main/manifests，不上线，不移除 Redis。
2. `Backend.TerminateTaskRuntimeLocal(ctx context.Context, c *TaskClaim, destination *controltransport.TerminationDestination) (TaskRuntimeTerminationResult,error)` 完成至 durable cleanup-complete；`Backend.ReleaseTaskWorkspaceOwnerLocal(ctx context.Context,c *TaskClaim) (TaskOwnerReleaseResult,error)` 执行最终原子释放；`LoadTaskRuntimeTermination(ctx context.Context,task TaskReference) (*TaskRuntimeTerminationEntry,error)` 和 `LoadTaskOwnerRelease(ctx context.Context,task TaskReference) (*TaskOwnerReleaseEntry,error)` 是只读冷诊断。结果仅包含 copied reference/revision/digest/phase/Outcome；不接受 public completion/bool/physical attestation。
3. 新 `Options.LocalRuntimeRelease *LocalRuntimeReleaseOptions` 显式启用，字段 `MaxActive uint32`（1..32）、`Timeout time.Duration`（正值≤30s）、`Provider *kubernetes.TaskRuntimeCleanupClient`。Provider 为借用具体依赖，调用方在 Backend.Close 实际 join 后关闭它；Backend 不销毁其他使用者的借用 client。启用还要求现有 enrolled-publication trust/contract、controlled clock、本地 recovery 配置和下面固定 issuer。nil profile 的新入口在 IO 前拒绝，旧入口/validator 保持。
4. 新 `TaskRuntimeTerminationIssuer` 只有 `Certificate(context.Context)([]byte,error)` 和 `SignRuntimeTermination(context.Context,string,controlprotocol.TaskRuntimeTerminationTicketClaims)([]byte,error)`，作为 Options 的独立可选字段。构造不调用依赖；实际 owner 内复制≤4096 certificate，必须匹配该 Task 原 archive command 已 pin 的 issuer，不添加隐藏比较或接受轮换后的不同 issuer。签名后独立验证固定 ticket，≤8192，窗口≤30s并受当前 claim/issuer/clock 不确定性限制。
5. 不重复 producer：复用 shared16KiB `TaskRuntimeTerminationIntent` codec、既有 termination ticket/proof verifier、`TerminationDestination.Terminate/Query` 和真实 observer。不要导出任意签名/比较器/成功回调；原 `InfrastructureFencer` bool 不能作为来源。

为真正通过公开入口建立首个空目录同步，允许新增窄编排 `BindTaskWorkspaceArchiveLocal(ctx context.Context,received *ReceivedTaskWorkspaceArchive,plan TaskWorkspaceSyncPlan,worker *launcher.KernelBoundary,root string)(uint32,error)`。它只消费原始received/source，复用现有PrepareTaskWorkspaceSyncSource、readonly本地target opener、bindLocalFinalizationTarget/sealOriginalFinalizationBinding，实际Close/join原source与target后才回slot诊断。原claim/plan/profile/物理root仍经既有guard验证；不修改旧validator/预算、不授予文件效果，不以copiedreceived或slot授权，不在unknown后重开已consumed来源。放在新的task_runtime_termination_local_binding.go，有限wrapper拒绝/关闭测试和真实空目录fullpositive覆盖，不接受privatefixture setup替代公开链路。

## Retained authority and lifecycle

6. 独立读取、严格验证 sealed local progress、immutable complete、原 manifest/plan/target/checkpoint/evidence、quiescence reservation/receipt/final-sync/archive/issuer 和全部当前 Task 域。重建既有50比较：base4、八域32、当前 claim+guard8、六个历史 MOD6，另保留 whole operations-prefix Version=0。public references 只定位，不构造 capability。
7. complete 要求 Version1/Create=Mod/Lease0、与 sealed progress 同提交、progress digest/claim/checkpoint/root/binding 全匹配。生产者已封闭效果准入、实际 join/Close；本入口不得重新打开本地 writer。只在此前提下省去三条 live sync-budget pins，不能删50基线、complete或 operations 屏障。
8. 初始读取真实两条 immutable enrollment 与 publication 历史，验证同提交、两记录 canonical/signatures、root-approved contract、creation/runtime/publickey/node/boot 全相等。将两条 independently validated enrollment MOD 加入 reserve。任何 put（包括同值）、lease reassignment、delete/recreate 均改变 MOD；base restore/cluster pin 防止跨恢复复用 revision。不能盲目给两记录各加四比较而超过64。
9. 所有网络/clock/IO/Close/Wait 都在 claim/global mutex 外。只在短锁中登记、复制 immutable snapshot、修改 attempt 状态；每次效果前重新核查原 owner、当前 claim/deadline 和 native guards。MaxActive 限流与既有 archiveLife Close seal 使用同一 mutex；Close 取消并等待原 owner、原连接/响应、provider 操作和 signer 真实返回，再关闭 client。超时不等于 join，不提前退款。
10. 每个 active Task 至多一个 owner；注册前不执行外部依赖。不新增按已有 N 的常驻 map/actor/timer、List/selector inventory、prefix discovery 或 unbounded waiter。实际 in-flight/unknown 继续收费直到 join。Timeout 后不开始新效果，未返回的原 IO 仍需等待。

## Fixed records and actions

11. 固定 Task keys：`runtime-termination` 使用既有 codec、≤16384、Lease0、固定 birth，phase `reserved→terminated→cleanup-complete`；`runtime-provider-cleanup` 为新 canonical v1 fixed record、≤16384；`owner-release` 为 immutable v1 fixed record、≤8192。新增 payload 合计≤40KiB/retained Task，另计键/MVCC/WAL/既有历史；不是整体容量结论。无按 attempt 扩张的 key family。
12. termination 的原 Current/command/claim、creation/enrollment/node identity 和十五个 writer-derived stamps 不可改写。reserved 禁 proof；terminated 必须持有独立验证的≤4096外部 signed proof；cleanup-complete 另含 exact cleanup digest。unknown 保留原归属，不能以新的 ticket/claim 重发原未知 signal。
13. provider record 使用固定 effect slots：安装 retained finalizer、原 Pod DELETE、最多四个 policy DELETE、移除 retained finalizer；字段只允许固定 kind/name/UID/resourceVersion/原始 effectID/claim/状态，绑定完整 Task/runtime/node/enrollment 和原始 descriptor。每槽至多一次 mutating request；unknown 不覆盖或自动重试。可保存 bounded read reconciliation，不把 absent 变成 no-send receipt。
14. 只处理精确 Pod 和固定 policy 名：logicalID/runtimeID 对应的普通 NetworkPolicy 与 Cilium private-deny，名称相同时去重，最多四个。使用既有 ownership validator 的严格非 legacy 合同。额外无法归属的 mount/CSI/资源或替换 UID 保持 pending；不能调用旧 bundled Remove/Cleanup，也不能删除共享 operator 资源或同步目标数据。
15. 必须在 durable effect intent 与新鲜 native guard 后才执行实际 mutation。顺序：确认精确 Pod/node/profile/creation → 安装并确认 `goairix.github.io/sandbox-runtime-termination` → reserve termination/记录 DELETE intent → 一次 UID+RV-preconditioned Pod DELETE → observer 对 exact deletingUID/Never 的真实终止 → 验证 proof、持久 terminated → 精确 policy 清理 → 仅移除自己的 finalizer → 验证旧 Pod 和目标 artifacts 的 terminal 状态 → 持久 cleanup-complete → release。不能以 Pod404、leader消失、EOF、ACK、finalizer 或 runtime自签证明代替物理死亡。
16. Pod 删除前需完成所有仍依赖旧运行时的认证查询。恢复已经物理退出的 runtime 必须只验证 retained history/原 observer query，不能调用 `PrepareAfterDualDrain` 要求它再次响应。新 claimant 可以消费合法历史 proof，但必须保留旧 Current 并使用自己的50 native guards；历史 claim 不变成当前 sender。

## Concrete Kubernetes client

17. 新 `kubernetes.NewTaskRuntimeCleanupClient(TaskRuntimeCleanupOptions)(*TaskRuntimeCleanupClient,error)`；options 只含 `Config *rest.Config`、`Namespace string`、`IncludeCilium bool`、`RequestTimeout time.Duration`（正值≤5s）、`MaxActive uint32`（1..32）。`Close()error` seals/取消/实际 join 自己的操作。配置明确 artifact set；实际 Pod 合同不符合该 set 则拒绝，不猜测 Cilium/历史资源已不存在。
18. 具体 client 生成 opaque `PreparedTaskRuntimeCleanup`，固定 API 为 `Prepare(context.Context,TaskRuntimeCleanupBinding,[]byte)(*PreparedTaskRuntimeCleanup,error)`、`Descriptor()[]byte`、`Observe(context.Context)(TaskRuntimeCleanupObservation,error)`、`Apply(context.Context,TaskRuntimeCleanupStep)(TaskRuntimeCleanupObservation,error)`。saved bytes 只能来自 Backend 已认证的 fixed provider record；client 仍独立重查精确对象。diagnostic descriptor 不证明 kernel/claim，也不能选择任意 API resource。`TaskRuntimeCleanupBinding` 字段为 `Authority controlprotocol.TrustBinding`、`Runtime controlprotocol.RuntimeReference`、`Generation,DataGateEpoch int64`，以及 string 字段 `TaskID,LogicalID,WorkspaceHash,CreationIntentID,NodeUID,NodeBootID,EnrollmentDigest,ContractDigest`。API namespace 独立来自 client 配置，不把 KV namespace 当 API namespace。封闭 step enum 恰为第13条七个动作（四个 policy 占四槽），不接受任意 effects。
19. 每个 Apply 只能产生一条已归属的 mutation，UID/resourceVersion preconditions 必须使用实际对象值。每个 response/body 独立≤262144、请求 descriptor≤16384；只做固定 point GET，无无限 watch/poll。原 Pod/Node profile、Never、deletion/finalizer 以及 policy ownership 不符均保留 pending。
20. 禁止自动写重试与 redirect。显式 client-go `MaxRetries(0)`，owned HTTP client 禁止 redirect，专用 HTTP/1.1 transport 禁 keepalive/HTTP2 replay；检查 Go transport 的实际 resend 条件，不以 wrapper 调用次数证明一次真实请求。配置不接受任意 Transport/WrapTransport/Dial/Proxy/exec-auth 成功替身。SDK 可见 TLS/config 容器发布后保持 immutable；不要把 cancellation/CloseIdleConnections 当所有内部握手 join。

## Native transaction budgets and unknown

21. 所有计数为 literal comparisons+success+failure 节点；请求和 EACH branch response 独立≤262144。真实 canonical 最大值/512 namespace/1024 key 在任何效果前预检，不提高 etcd 配置上限，不复用隐藏预留14节点的 generic Stage。

| Transition | Fixed budget |
| --- | --- |
| provider record initial absent+put before termination | 50+operations1+complete4+absent1+put1=57 |
| provider exact-state CAS before termination | 50+1+4+provider4+put1=60 |
| termination reserve with enrollment2 and provider MOD1 | 50+1+4+2+1+absent1+put1=60 |
| exact termination guard/CAS | 50+1+4+termination4+read/put1=60 |
| termination transition pinning validated provider MOD | 50+1+4+4+1+put1=61 |
| provider state CAS after termination, pinning both states | 50+1+4+termination4+provider MOD1+put1=61 |
| final atomic release | 50+1+complete4+termination4+releaseAbsent1+delete3+put1=64 |

22. provider 后续独立 CAS 同时 pin 当前 termination 时用 provider MOD1（读取已验证值，MOD绑定其完整状态），不是再隐藏添加四比较；必须逐条实际列计数和 native protobuf bytes。任何预算不满足先报告 Root 修正设计，不能删原 pins。

reserved 之前还要预检未来 proof 上限4096字节加入后 terminated/cleanup-complete 的实际 codec 和原生 wire 投影，不能等杀死原 runtime 后才发现终态写不下。所有输入先做固定 raw/canonical cap 再复制，无须为非法超界输入先分配任意大 backing。
23. uncertain native ACK、ticket/send/provider response 冻结该原始 attempt，后续同 attempt 只读，不能 Grant/Put/Delete/KeepAlive 重放。absent 不证明 no-send/成功；未知保持 owner/control/永久 intent，不以 TTL 清理。若证据无法恢复，允许明确 pending，不制造终态。
24. release 的一个事务删除 exact owner、destroying control、runtime index，并写 immutable receipt；其他 task/fence/history 不删。receipt 绑定所有被删 key birth/revision/digest、task/runtime/node/complete/termination/cleanup 和当前 release claim。失 ACK 后冷 resolver 用 retained task+receipt/history，原 owner/control/index 已删或有新 owner 时仍能诊断；不得重建 ClaimTask 或以 owner absence 判成功。

## Validation and nonclaims

25. 有限实际 positive 必须组合 genuine enrolled Supervisor/observer、真正完成的本地同步（首个空目录即可）、当前公开 Backend、TLS etcd 和精确 HTTP Kubernetes fixture，观察实际 pidfd死亡/cgroup空、proof/cleanup持久化、64节点释放与新 owner 后冷 receipt。seeded complete、假 physical callback、fake boolean 不能验收组合链路。
26. 新有限负例覆盖 enrollment每条同值写/lease/recreate、错误 runtime/node/claim/restore/complete/provider UID/RV、每个 mutating ACK gap、429/307/308/broken-connection 实际 request 数、blocked original Close join、原 runtime 已死后 fresh claim、release lost ACK+new owner。不重放已验收旧矩阵，只有实际影响源码/选择器增量运行。
27. 保留原始 Cmd.Wait/attach/containerWait、source filename/SHA 和 binary 绑定、精确资源清理及失败/告警；不能用补 Wait 重建证据。组合链路一个 fresh author、一次唯一 fresh完整review，修复限定同 pair，达到提交条件及时提交。2026-10-16 在6160c36固定 provider 接口后，将第17–20条具体 Kubernetes client 抽成独立并行依赖单元，见 `2026-10-16-task-runtime-cleanup-client-design.md`；其独立作者/唯一审查只拥有该客户端文件。原组合作者继续 retained authority/termination/release，不重复实现客户端；依赖验收后才将其计为可用。Root串行Git窗口和实际编译closure的quiet期，不能靠并行读写混成无法绑定的二进制。
28. 本单元不是真实 kubelet/部署恢复、全部内核负例、大 N/长期故障或完整容量验收。Observer slot 回收须有认证 release provenance、旧命令 fenced/过期及真实 join，不能仅按 TTL/terminal 释放；其1024槽复用、长 idle 证书维护、其他 provider/运行时、生产API/调度/FUSE、备份恢复、历史GC及 Redis 移除仍是 master 明确后续工作。
