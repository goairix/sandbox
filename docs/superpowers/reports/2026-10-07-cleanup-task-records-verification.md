# 清理任务元数据：Task1 验收

原始范围 `edcbf2ff609f8542ccc5a85941bd93085c7a1971..7544388619d9f101c847829bfed5db3762d87b3e`。`db5bdc9` 提交严格永久 task/intent/link/checkpoint 及固定 key；`7544388` 提交只读诊断加载；范围内 `dee2fb2` 为诊断与 live authority 的规格澄清。代码五文件642新增行，独立 review 包含该澄清共六文件644新增行。

Task1 独立 spec/quality review 通过：Critical0、Important0、Minor2、Cannot-verify4。诊断读取只比较当前 backend identity/restore 和精确 reference，不发放能力；task/intent/link 为 Lease0 且 Create==Mod，checkpoint 为 Lease0 且 Mod>=Create。严格递归 JSON/大小/信封验证，失败不改写目标。没有新增每个空闲沙盒 Lease、客户端、goroutine、timer、ticker、Watch 或生产后端切换。

## 实际门禁与失败保留

Darwin arm64 Go1.25.6 CGO1 race 对真实 Linux etcd3.6.15 三成员运行：`go test -race -count=1 -timeout=90s -run '^TestTask(RecordsStrict|Keys|ReadCopies|ReadIdentity)$' -v ./internal/storage/state/etcd`，4顶层+10子测试PASS、0FAIL、0SKIP，exit0。六个独立 namespace 的日志记录同一集群的三个不同 member。657份 worker 当前/副本/Git源码加 Root fixture custody 合计660份，Go/compile/link 三个程序 hash 均核对；源码从冻结到执行及最终轻量校验一致。scoped vet/diff/worktree 检查exit0。保留此前行为 RED、错误UTF8测试输入的 exit1 与 host integration SKIP，不把它们当实际验收。

成功项目 `sandbox-etcd-state-test-12556-1791360153`，含隔离 foreign 成员但本次没有 foreign-cluster拒绝测试。所有21条原始命令exit0；四个成员每个64MiB、0.5CPU、128PID、只读根、dropALL、NNP、固定本地镜像、仅loopback端口、当前项目拥有的命名数据卷；运行前后健康且未OOM。down退出0，项目容器/卷/网络三个精确清单为空。此前第一项目仅因 Root 把 Compose 的命名卷 Binds 误认宿主挂载而在测试前失败，18条raw及三个空清单保留。只修 Root checker、验证实际命名卷与五个拒绝控制、重新冻结后运行；未改产品验证器或放宽资源。

## 每项 finding 与证据边界

- M1：取消上下文用例有两条预期 canceled-Txn client WARN；原始 stdout 与分类完整保留，测试断言错误+nil结果通过。记录为 deferred minor，最终单元 review 仍须核对；不静默丢弃，也不重跑已通过测试清洗输出。
- M2：metadata日志18WARN+1启动ERROR完整保留。错误为 etcd-1 storage schema detection/update 的 missing-term，发生在测试前；后续健康/读取成功不能证明 storageVersion恢复。该问题对本次严格诊断断言非阻塞；记录 deferred minor，任何依赖升级/snapshot/整体健康属性的后续门禁需要直接证据。
- CV1：现有 Stage预算64操作、64KiB/256KiB及14项协议预留沿用 `docs/superpowers/reports/2026-10-06-etcd-stage-verification.md` 的原始实际三成员验证，Task1没有改动该原语。销毁/claim/renew/checkpoint授权尚未实现，必须由Task2/3分别当前源码实际验收，不能以诊断加载替代。已满足Task1范围，跨任务条件保留为强制工作。
- CV2：未取得精确镜像源码对应关系及失败成员storageVersion恢复证据；上游源码仅解释匹配错误路径。保留为证据限制，不声称修复/良性。升级、降级、snapshot验证进入后续部署与恢复门禁，不要求重放本次功能测试。
- CV3：本次只验证真实三成员身份/restore拒绝，没有额外证明foreign拒绝/failover/生产TLS/RBAC。生产安全/故障切换仍为后续要求；存在foreign容器本身不算测试。
- CV4：本次是宿主race+实际Linux etcd，不是Linux应用race/PID1/FUSE/物理清理/End或fleet性能。已封存认证执行单元证据见 `docs/superpowers/plans/2026-10-07-authenticated-target-execution-verification.md`；TaskClaim与持久目标gate关闭/双侧排空/remote settled/安全释放以及大量已有沙盒验收仍必须另行完成。

匹配错误路径参考 [etcd v3.6.15 schema](https://github.com/etcd-io/etcd/blob/v3.6.15/server/storage/schema/schema.go) 与 [version monitor](https://github.com/etcd-io/etcd/blob/v3.6.15/server/etcdserver/version/monitor.go)。镜像日志caller行号与上游tag不同，不能据此认定字节级构建来源；没有错误恢复证明。

## 完整证据保留

本单元 OWN `.superpowers/sdd/2026-10-07-cleanup-task-authority/` 保留完整报告/review/raw/源码，最终单元结束统一校验并封存。以下hash固定Task1接受时的内容，后续补充报告不得覆盖原始raw：

- `task-1-report.md` SHA256 `2979fb322c96c26d1820479aedf8d7a417cb3be3cf31d30596f59b4ab043bf38`。
- `task-1-review.md` SHA256 `ca17f9f8390de7557dec44ba01d24fa8f36442ffb170577906de7a1020515196`。
- `root-task-1-real-etcd-audit.json` SHA256 `9e248fa49d46a574fa6b31b13db9016ff2372defa9317287ac1c9142a3e1db34`。
- `root-task-1-log-classification.json` SHA256 `5fbf1d8ace9260eed939f8896cf06cf218c6e34dcf022fe23039eb70b6316f57`。
