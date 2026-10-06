# etcd runtime dispatch：whole-branch 最终独立审查

- 审查日期：2026-10-06。
- 基线：`b99b823d69de6baa860eb95d47bd269599b03f42`（用户指定的 `feat/workspace-fuse-mount` 起点）。
- HEAD：`e3579dc671c1885de7cd80a3a201e18ca6b563f9`。
- 完整差异：58 文件，9,939 insertions / 12 deletions；`review-b99b823..e3579dc.diff` 的 10,581 行已分段、有序读完，未以截断输出代替剩余审查。
- 依据：已批准主设计 `docs/superpowers/specs/2026-10-06-etcd-state-management-design.md`、本轮 `docs/superpowers/plans/2026-10-06-etcd-runtime-dispatch.md`、前序分批计划和 reviewer 模板。
- 本报告来自独立代码、测试及交错审查。前序 review/self-review 仅用于定位已知边界与延期事项，不作为本次通过依据。
- 未派子代理，未运行测试，未更改源代码、index、HEAD 或分支。仅按明确授权写本报告。用户已有 Sentinel 计划修改及控制器新增的 runtime-dispatch verification 文档均未触碰。

## Strengths

### 跨模块的持久证据链完整

`stage.go:62` 在任何 Lease Grant 前生成内部 attempt locator、执行一次 builder，再由 `prepareMutation` 私有复制并计算 digest（`stage.go:74`、`mutation.go:39`）。locator 不包含 digest，故不存在声明内嵌自身 digest 的循环；dispatch builder 在 `runtime_dispatch.go:109` 使用同一个闭合编码提前保存 exact reference，Grant/guard 回复丢失时仍可定位该次仲裁。

`runtime_dispatch.go:95` 将声明和输入放进同一个 Stage；`stage.go:139` 把全部业务比较和两个业务写入与 committed receipt 一起提交。恢复先以两个固定点查找 locator，再以三个固定点重新取得声明、输入和 exact receipt（`runtime_dispatch_read.go:26`）。第二次读独立验证，不把第一次快照拼接成最终证据。三个记录要求永久、首次 revision 相同、CreateRevision=ModRevision；输入重算 compact payload digest，receipt 要求 committed、完整 locator 一致及合法 digest（`:64`、`:102`、`:139`）。结果采用 receipt 内实际持久化的 Stage digest，而非尝试从不包含原比较条件的 payload 重新猜测 Stage digest。

### creation claim 与 Stage 的权限边界正确

`creation_claim.go:83` 保存六个永久记录的 ModRevision、Value、Lease=0，共 18 条比较；`:158` 为原 claim 和原 guard 各追加 Value、Lease、CreateRevision，共 24 条。dispatch 原样复制这组比较，并追加两个目标键不存在比较（`runtime_dispatch.go:46`、`:105`）。同值删除重建、租约替换、永久控制状态改变都不能仅靠相同 JSON 恢复旧写权限。

本地 deadline/lost 与服务器上的原始 CAS 分工明确（`creation_claim.go:179`）。本地失效阻止后续能力构造，不能撤销已复制给 Stage 的事务；后者仍由原服务器比较和 receipt 线性仲裁。Renew 只校验并续原 Lease，失败永久标记本地 lost，迟到正回复不能越过原本地 deadline 复活能力（`:209`）。Release 只撤销该 backend 所有的原 Lease，并保留永久 owner/domain（`:273`）。

声明路径在读取后和 BeginStage 后再次检查调用 context 与本地 claim（`runtime_dispatch.go:72`、`:120`）。重放校验原 request、configuration、workspace、sandbox、generation、restore、gate、snapshot 和 expiry（`:136`），保留原 operation/attempt/creator，不续租、不产生新写入。新 claim 可以恢复旧 creator 的声明，但返回类型明确仅为元数据证据，当前没有调用 runtime 的入口；因此 replay 不会自动变成旧 creator 的执行权限。

### foundation、domain 与资源边界保持一致

`client.go:46`/`:123`/`:203` 检查 endpoint、TLS、预期 identity、cluster 与 restore，TLS 可变容器被复制；正常业务事务使用固定 identity/restore 比较（`:279`）。domain 点读检查响应身份、键和数量（`domain_read.go:27`），WorkspaceIdentity 保留真实 provider/storage/bucket/prefix 绑定，dispatch 没有用 hash 伪造 WorkspaceIdentity。

`workspace_acquire.go:46` 的 request replay、永久 owner/fence、七记录原子获取和 generation 溢出保护，与 creation claim 的多记录校验相接。codec 对记录类型、wire 大小、未知字段、尾随内容、永久 Lease 和业务关联做封闭校验；RawMessage 未经浮点数解码破坏。仅为核实 canonical prefix 的具体调用风险读取了 diff 外的 `internal/storage/prefix.go`，没有进行无目标的全仓爬行。

调用者 payload 在首个 RPC 前私有化（`runtime_dispatch.go:54`、`workspace_acquire.go:67`）；Cmp 的 key、range、oneof value 深拷贝（`mutation.go:126`），不存在本次审查能构造出的调用者后续修改影响持久 digest/写值路径。dispatch 预算为 24+2 条比较、2 个业务写、14 个协议保留操作，共 42，低于 64；编码字节预算未放宽（`mutation.go:15`、`:43`、`:81`）。当前 domain/claim/dispatch 调用均为固定键集合，没有按 sandbox 数量扫描、每实例 controller 或每实例常驻 goroutine。

## 可证交错检查

以下是逐项审过的失败触发及实际阻断位置；未发现能在本轮支持路径中穿透这些条件的失败交错。

| 交错/异常 | 代码裁决与审查结论 |
|---|---|
| 完整业务事务被延迟，resolver 先写 aborted，旧事务随后抵达服务器 | receipt CreateRevision=0 CAS 互斥；旧事务不能写声明/输入。不是在客户端入口提前返回错误来模拟安全性。`stage.go:133`、`receipt.go:76`。 |
| 声明和 receipt 已提交，但提交回复丢失 | 返回 unknown + exact reference；Resolve 或全新 Backend 的 loader 恢复 committed，原 operation 不变化。`runtime_dispatch.go:113`、`stage.go:142`、`runtime_dispatch_read.go:26`。 |
| claim Lease 失效、同值 claim/guard 被重建、control gate/restore 改变 | 原 24 条服务器比较及 base identity/restore 比较阻止提交。`creation_claim.go:83`、`:158`，`stage.go:139`。 |
| claim 本地 lost 发生在 Stage 已复制原比较之后 | 不声称能撤销已在途事务；只要原服务器条件仍成立，可能合法 committed，否则被 CAS 拒绝。receipt 是最终证据。`creation_claim.go:179`。 |
| KeepAlive 正回复在撤销或本地 deadline 后到达 | deadline/lost 防本地复活；即使 Lease 续租的瞬间与永久记录变更交错，业务提交仍须原比较。`creation_claim.go:241`、`:260`。 |
| 两个声明并发争同一 intent | 两个固定记录的不存在比较只允许一个业务声明成功；失败者保留自己的 unknown attempt，不冒充胜者；以后可重放胜者。`runtime_dispatch.go:105`。 |
| 声明、输入或 receipt 缺失/改写/加 Lease/跨 revision；第一次读后 locator 更换 | 半套记录、非首次 revision、错 locator、错 digest/输入关联均失败关闭；不借用第一次找到的别人的 receipt。`runtime_dispatch_read.go:64`、`:118`、`:123`、`:130`。 |
| 新 claim 重放内容自洽但属于不同 request/gate/snapshot/expiry 的旧声明 | 私有 claim context 关联检查拒绝；同 kind/target/payload 才能返回原证据，不 Grant。`runtime_dispatch.go:78`、`:136`。 |
| 业务 committed 后 Stage 清理失败 | GuardCleanupError 独立报告；不降级 committed，不撤销 creation claim，不删除 owner。`runtime_dispatch.go:119`。 |
| NOSPACE 下读取不到 receipt，或当前 leader 失败 | 缺失 receipt 不被推断为 aborted；只有成功写 aborted 的 CAS 能作此判断。实际 fixture 日志包含对应通过结果。`receipt.go:76`、`stage_fault_test.go:237`、`:268`。 |

## Issues

### Critical / P0–P1（必须修复）

无。本次独立审查未发现可证实的数据丢失、越过原 claim 的持久写入、错误重放授权或 receipt 仲裁失效路径。

### Important / P2（应修复后再通过）

无。未发现本轮明确验收范围的缺失功能或阻断性测试缺口。本结论不覆盖后续生产执行协议。

### Minor / P3（明确 triage）

1. **Task 1：Grant 边界上的通用 mutation 深拷贝测试仍可加强。**
   - 位置：`internal/storage/state/etcd/stage_attempt_test.go:185`；生产对应 `stage.go:74`、`:93` 和 `mutation.go:126`。
   - 现状：已有测试在 Begin 返回后修改源 mutation 并校验隔离；dispatch 另有 Grant hook 修改 payload 的测试（`runtime_dispatch_fault_test.go:122`），但通用 builder 的 Cmp/Write 源容器没有全部在 Grant 边界修改。
   - 影响：这是更精确的回归测试建议。代码顺序清楚显示复制与 digest 在 Grant 前完成，未发现现实 alias 缺陷，也没有可证失败交错可据此升级为实现 bug。
   - 决定：**接受延期，非阻断**。后续可同步在 Grant hook 修改 builder 原 comparison key/range/value 与 write bytes，断言持久内容和 digest 不变；无需为本次通过重跑整套测试。

2. **Task 3：故障 fixture 的 5 条预期 warn 应保留分类说明。**
   - 位置：`.superpowers/sdd/2026-10-06-etcd-runtime-dispatch/final-fixture.log:271`、`:341`、`:1156`、`:1810`、`:1811`。
   - 前三条为原 Lease 已不存在时的 Revoke NotFound，分别出现在 `creation_claim_fault_test.go:226`、`creation_claim_test.go:81`、`runtime_dispatch_fault_test.go:46` 的撤销/故障路径；后两条来自 `stage_fault_test.go:237` 的预期 NOSPACE 注入。
   - 影响：日志不等于完全无告警，但没有新的未解释服务错误或测试失败。这五条不能被宣传成“零 warning”。
   - 决定：**按预期故障输出接受，非阻断**。未来如整理 CI 输出，可在测试日志层分类/allowlist，保留未知告警；不建议为美化测试而关闭生产 etcd client 警告。

## 验证证据与限制

本 reviewer 遵从“不重跑已跑测试”，只读取已有日志和报告，并独立审代码与测试行为。

- 控制器最终真实三成员隔离 fixture：`final-fixture.log` 顶层 PASS **140**、SKIP **0**，无 `WARNING: DATA RACE`；包 `-race` 结果 **21.732s**（第 2030 行）。NOSPACE 与 leader-failure 分别于第 1812/1817 行 PASS。5 条 warn 按上文明确 triage。
- Task 3 报告记录 `go test ./...`、`go vet ./...`、`go build ./...`、涉及文件 gofmt 与 `git diff --check` 均成功。本 reviewer 不把这些报告表述为自己重新执行；也不把已有 lint 基线问题表述为全仓 lint 已清零。
- fixture 使用独立 Compose project、动态 loopback 端口、三个成员和独立 foreign cluster；危险故障操作先检查实际 project/container/端口归属，退出清理仅针对自有 fixture。单机多容器能证明成员故障场景，不能证明跨机器或跨可用区高可用。
- 容量测试第 1602/1607/1612 行读取实际三个 value：19 B 样例总 1,935 B；合成 4 KiB 输入总 6,012 B；合成 8 KiB 输入总 10,108 B。100k 条保留记录对应 193.5 / 601.2 / 1,010.8 MB 的 value 字节线性外推。后两者是合成 JSON，不是真实 Pod 分布；未含 key、MVCC、WAL、索引、碎片、副本或其他 domain，不构成 100k 规模性能/容量验收。

## Recommendations

- 以当前 metadata declaration/recovery 合入边界继续后续阶段；每个未来 runtime 执行入口必须重新验证当前有效权限与适用的 restore/gate/业务 deadline，不能仅凭 `DispatchReplay`、`OutcomeCommitted` 或旧 creator 字段执行外部副作用。
- 后续 GC 必须先证明旧 actor/claim/attempt 永久不能重新提交，再回收声明、输入与 receipt；现在的全缺失返回 nil 不表达“外部 runtime 从未存在”。
- 为生产保留真实 workload 大小分布、保留期、配额与故障演练工作项。当前仅靠固定点访问和 wire budget，不能替代实际容量治理。

## Declined to judge（逐项保留给执行者裁定）

- Runtime helper 的真正 create/prepare 调用、目标侧去重及执行时授权：详细本轮计划明确只交付声明与恢复，尚未出现执行入口。
- Publication、runtime UID/BootID/mount 证据、active owner、request completed 与 runtime index 的最终发布事务：属于主设计后续阶段，本次没有声称已实现。
- Admission/operation/pool quota、调度器、owner release、GC 与 receipt retention：属于后续阶段；已审查当前代码没有暗中执行这些动作，未把缺少这些阶段当本轮功能缺陷。
- Redis 生产替换、production wiring、上线切换/回滚：当前仅新增 etcd package 与测试/文档，现有生产入口未切换；本轮不验收整套 Redis 迁移。
- 真实生产 mTLS 握手、部署 RBAC、restore 时旧集群和旧凭据隔离、跨主机/可用区灾备：本轮检查的是配置约束、逻辑 identity/restore fencing 与本地 fixture；上线环境演练仍需单独完成。
- 授权运维直接伪造一套自洽的永久业务记录/receipt，以及非 etcd 契约的成功 RPC（例如 nil Grant response）：当前受信 backend/etcd API 假设之外。Stage 的 Grant 响应防御性校验可后续向 creation claim 对齐，但仅靠构造违约 mock，不能作为本次可证正常路径缺陷；身份误配置和正常 foreign-cluster 场景已在本轮审查之内。
- 100k 实际 workload 的吞吐、峰值内存、磁盘占用与保留期：只有 encoded-value 样例和固定点代码证据，缺少真实分布/负载实验，不给生产容量通过结论。

## Assessment

**Ready to merge? Yes — 仅针对本轮分批交付范围。**

**结论：通过。** 已有 identity/TLS、Stage/receipt、永久 domain/acquisition、original-Lease creation claim 与新增 runtime dispatch 声明/恢复链，在本轮范围内衔接完整；未发现需修复后再通过的 Critical/Important 问题。上述两个 Minor 已逐项接受延期/分类，未默默丢弃。主设计后续 runtime 执行、发布、admission、GC 和生产 wiring 尚未实现，整套 Redis 迁移与生产上线验收仍未完成。
