# Sandbox etcd 状态管理最后一次完整方案 review

日期：2026-10-06。只读审查；未修改仓库、部署或运行环境，未运行性能或故障注入测试。

对象：`docs/superpowers/specs/2026-10-06-etcd-state-management-design.md`（519行）、`docs/superpowers/reports/2026-10-06-etcd-state-management-review.md`及关键现有源码。源码基线：`feat/workspace-fuse-mount` HEAD `b99b823d69de6baa860eb95d47bd269599b03f42`。审查不以既有报告的“关闭”结论替代独立检查。

## 结论

**没有发现新增或仍未关闭的 P1 阻断项；可从当前分支切新功能分支，进入分阶段实施。没有需要为本轮再扩展架构的 P2 发现。** 第一版单cell、停机全源drain后切换、删除Redis运行依赖的取舍可以成立。无需为尚未启用的多cell增加数据库或全局路由服务。

这是方案可实施的结论，不能作为“后端已替换”“launcher已安全”或“大N容量已通过”的证明。一至五阶段共同构成完整替代；阶段一只能独立交付可测试的基础包，不可据此开放新版allocator或移除旧运行保护。

## 独立核对与证据

| 核对点 | 证据与判断 |
| --- | --- |
| 领域Txn是否能实现 | 方案141-153、167、183-187、203-223、295-307、415-433要求同cell组合事务、客户端计算generation后CAS、固定guard/claim和stage receipt；不假设etcd可做JSON计算、服务器业务时间或跨集群事务。Acquire、Publish、CloseAdmission、Cleanup、slot消费均可表达为有限key集合的Compare加原子Put/Delete。operations排空是在准入关闭后Range，未要求把全部token塞入事务。403行把事务预算控制到64，具体方法需编译检查预算。 |
| 旧接口不能直接套驱动 | `internal/storage/state/state.go:12`旧Store包含SetNX/Keys，AtomicStore靠单keyCAS；`internal/storage/state/active_sandbox.go`的旧Validate要求runtime UID与整块snapshot，旧Repository依赖业务Revision、server-time lease和controller Scan。新版provisional publishing记录、ModRevision与分离snapshot需要原生类型/领域事务，不能套旧Validate或满足旧Store后宣布完成。方案415-433已明确。 |
| 大量已有sandbox的稳态 | `internal/sandbox/lifecycle_coordinator.go:227`分页循环仍遍历全部record，253-266还有expired exclusive经BeginOperation重开；当前每controller续租与workspace续租都真实存在。方案259-273、313-356改为有界cache、近期due窗口、共享预算、低频O(N)审计；没有把全库镜像搬到每API。FUSE F/5本地probe、F/Treport tuple、F/10 kubelet探针、auto-sync S/T、历史U及request churn均单列，未把空闲或聚合误算为零成本。 |
| launcher/gate执行安全 | `internal/runtime/kubernetes/exact_exec.go:20`现有入口只检查Pod UID，不能拒绝同UID迟到命令或同Pod容器重启；`internal/runtime/docker/container.go:66`仍是sleep PID1；exec/file代码还有多种raw transport路径。方案177-185、235-249显式新增真实sandbox容器PID1 launcher、两层排空、container incarnation、UID/FD隔离、全后代与file/stream入口覆盖。现有代码不满足新协议，但方案没有假称已满足，也没有先删除保护的实施空窗。 |
| owner释放 | `internal/sandbox/workspace_lease.go:1055`旧safeToReleaseOwner允许sync空证据、InfrastructureFenced或ProcessExited等简化判断。方案187、251-255明确要求发送端隔离与远端settled/fenced证据，unknown不释放。初版接受未知外部写可能长期pending，这是一项保守可交付边界，而不是缺少自动恢复就必须增设网关。 |
| restore边界 | 方案95-99、247、409-411把authority/cluster/runtime/storage identity、不可复用restore ID、可信target安装与旧credentials隔离、snapshot后runtime盘点及prefix quarantine连成开放前条件；无法映射prefix冻结整个scope。未将revision bump或递增计数当作对象写fence。 |
| drain与删Redis | `deploy/helm/sandbox/templates/pre-backend-change-drain.yaml:41`真实使用新chart镜像，59-70读取旧配置与卷；方案456-463已要求installed旧digest独立Job，全部旧API归零后全源审计，pending阻止切换。`internal/sandbox/drain_audit.go:11`旧审计prefix仅覆盖部分family，不能将当前AuditDrainedState成功当作新迁移全源报告；方案457与483已要求扩展检查。回滚先新版drain，不允许两allocator共存。 |

etcd的原子多key事务、KV revision和Lease关联key删除支持上述推导；它们不提供runtime fencing或远端结算证明。依据：[etcd API reference](https://etcd.io/docs/v3.6/dev-guide/api_reference_v3/)。NOSPACE可能返回错误但已实际提交，方案的未知结果处理有官方语义依据：[etcd Maintenance](https://etcd.io/docs/v3.6/op-guide/maintenance/)。

## 阶段一独立交付边界

主线拟定的首批范围合理：namespace/authority identity failclosed、专用Lease immutable attempt guard、业务Txn与stage attempt receipt原子commit/abort仲裁、真实三成员fixture。新增独立包与测试，不接入旧Store或生产allocator，可审查、可验证，也不会把尚未交付的launcher能力伪装为已完成的安全保证。该首批是阶段一的第一块基础交付，尚不等于阶段一全部领域Repository已完成。

实施API必须固定以下现有方案约束：

1. 应用Open只验证operator预置identity；空keyspace、错误cluster ID、scope/prefix/cell/storage/runtime identity或restore ID不能自动初始化。authority中的每项绑定都应能被测试，不只验证endpoint可连。
2. 专用guard创建unknown不得重新创建同ID；提交时比较精确value、Lease、restore epoch。签发授权的入口不能绕过这一原语。
3. 区分两种receipt：短operation admission receipt可附同专用Lease（213行）；会产生永久owner/control/intent的Acquire等stage committed/aborted receipt必须按207、215-217行持久保留至fenced GC。通用runner不能默认把所有receipt挂guard Lease，否则Lease过期会抹去永久业务提交的证明。
4. committed与aborted竞争的是同一txn attempt；logical stage重试换新attempt/guard/receipt，原marker不覆盖。响应应明确known committed、known aborted、unknown，不能把CAS失败或absent强行归类为aborted。
5. NOSPACE时resolver的aborted Put也可能失败；保留unknown、停止危险新建，恢复空间后继续仲裁，不能为写marker先删owner。Lease过期导致guard/短receipt消失，只能证明旧事务不能再提交，不能把已发生的永久effect当作不存在。
6. 原生方法须拒绝跨namespace key、重复写同key、超compare/op/byte预算；永久owner/control不附Lease。业务generation正int64溢出拒绝且无mutation。

版本取舍亦可推进：当前仓库go1.25.0；官方client v3.6.15的go.mod要求go1.26，而v3.6.14要求go1.25.0。固定server镜像3.6.15/client模块3.6.14，在真实三成员上验证本项目使用的3.6协议，不构成本轮设计阻断；必须固定镜像digest和实际依赖版本，并报告测试结果。证据：[client v3.6.15 go.mod](https://github.com/etcd-io/etcd/blob/v3.6.15/client/v3/go.mod)、[client v3.6.14 go.mod](https://github.com/etcd-io/etcd/blob/v3.6.14/client/v3/go.mod)。不要用同minor推断本项目所有故障协议已经被测试。

## 必须真实验证的退出条件

- **首批三成员etcd**：并发首次申请只有一个owner/generation获胜；control/snapshot/index/receipt原子可见；commit丢响应、请求延迟至abort后、guard创建unknown、Lease自然过期与Revokeunknown；stage commit/abort恰有一个结果；永久stage receipt在guard过期后仍在，短receipt消失后旧guard永不重建；restore ID变更与旧claim拒绝；leader切换/单成员故障；NOSPACE可能已提交和resolver不能写marker的unknown路径。故障注入要实际让旧Txn在服务器收到前被延迟，不能只在client返回后伪造timeout。
- **领域接入**：生成具体Txn操作清单，核对正常/失败分支不重复修改同key且符合预算；generation溢出无写；slot/free index及runtime反向索引同事务；永久record无Lease；fixed-revision分页与R+1 Watch、compaction relist不凭缓存授权。旧API主要成功响应与SDKCreate调用兼容，未知申请查状态和Idempotency-Key真实回归。
- **真实Docker/Kubernetes launcher**：所有exec/file/tar/stream进入可信runner；UID0/同管理UID拒绝；管理FD、Secret、proc/ptrace与IPC隔离；setsid/doublefork/并发fork全部排空；同PodUID容器重启、launcher崩溃/state丢失保持closed；迟到旧gate与旧claim不能恢复命令。测launcher在原语言环境及原cgroup内的资源开销。
- **外部效果与恢复**：flush/terminate不同阶段响应丢失、S3 PUT/DELETE/CompleteMultipart未知均保留owner；旧snapshot之外新增仍写runtime必须阻塞同prefix，无法映射时scope冻结；反复恢复同snapshot产生不同restore ID且旧凭证无效。
- **大N与churn**：普通idle N从10k到100k，固定操作/变化率，短周期Txn/runtime请求与每API snapshot内存不随N十倍增长；测低命中snapshot读取、GC跨1h默认幂等窗及24h代表性soak、历史U、pending工作集和journal容量；HPA/rolling/restart不复制cell份额，准备硬并发始终受持久slot约束。
- **FUSE与迁移**：真实probe挂死不释放实际执行slot导致无界累积，readiness无周期fork；采样稠密节点CPU/RAM/report bytes。旧digest migration Job完成全family、multipart/staging、runtime、finalizer和policy-only审计；pending阻止开放；新流量后的rollback先新版drain；Redis运行引用按tracked源码/config/chart/scripts/tests/现行手册检索并审查历史allowlist。

上述全部来自方案既定契约。阶段推进按退出条件提供证据，不需要再进行无止境设计扩展；实测发现新事实时再按证据调整参数或协议。
