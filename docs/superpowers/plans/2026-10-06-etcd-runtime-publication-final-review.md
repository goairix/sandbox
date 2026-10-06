# Whole-branch final review — etcd runtime publication

日期：2026-10-06。审查基点 `b99b823d69de6baa860eb95d47bd269599b03f42`，冻结 source HEAD `6ed468dbf2f616cee3bca6f774b658969cdcfea5`，分支 `codex/etcd-state-management`。

范围是完整 28 个已提交 commit、86 个文件，覆盖 identity/client、Stage/receipt、永久领域模型与 AcquireIntent、original-Lease creation claim、dispatch、binding/index/mount intent、签名协议及 Publish/recovery。不是仅审 Task 4。主证据为 `review-b99b823..6ed468d.diff`，分轮独立读取实现、测试、配置及文档；被工具截断的相关段落另外补读。没有派生 reviewer 或子代理。

依据完整设计、foundation/domain acquisition/creation claim/dispatch/publication 计划、progress ledger、各 Task 报告和 review、先前 whole-branch review，以及 controller 的修复后验证日志。未提交的 publication verification 文档只作为解释和运行证据，不计入冻结 source。命名的未改代码集成检查包括 `internal/storage/prefix.go:17` 的 canonical prefix 契约；没有扩大到下一 operation 草稿。除本报告外没有修改源码、索引、HEAD 或分支，没有运行测试、故障注入、runtime 调用或读取凭证。原 Sentinel 文档修改和下一操作计划草稿均未触碰。

## Strengths

- **身份与 authority 边界贯穿整个链。** `internal/storage/state/etcd/client.go:132` 不初始化空 namespace，验证各 endpoint 的 cluster 及 operator 预置 metadata；`client.go:298` 的原始 identity/restore value 与 Lease0 比较进入授权事务。workspace hash 对 provider/storage identity/bucket/canonical prefix 做带长度边界的摘要，claim 保留真实 private WorkspaceIdentity，不能用公开 hash 重新构造更弱的身份。
- **永久事实与临时管理权分离。** Stage 的不可复用 guard、永久 committed/aborted receipt 和同 key 仲裁阻止迟到事务绕过 abort；Acquire 的七个永久领域写入与 receipt 原子提交。`creation_claim.go:181` 保留六个永久 key 和 claim/guard 的原 24 条比较，续租始终针对原 Lease，单调截止及 lost 状态不因迟到回复复活。owner 不随 claim/Stage Lease 消失。
- **跨阶段输入具备连贯的不可变证据。** dispatch 固定 operation/input 与 creator；binding/certificate/UID index/receipt 同首次 revision，UID 索引不含 BootID，阻止同 UID 换 boot 后被另一个 intent 占用。mount intent 在外部执行之前永久消费，plain 明确为 0、FUSE 为 1；重放不生成新的 mount operation。
- **签名验证不是 caller 断言布尔值。** `internal/runtime/controlprotocol/publication_verify.go:16` 固定并复制 roots；根证书与 delegate ready 使用独立签名域。严格静态 schema、非 float 整数处理、长度边界、canonical certificate digest，以及 claim/epoch/UID/BootID/mount operation 的上下文匹配，在 `runtime_publication.go:147` 与 private preparation context 结合。当前验证与历史验证的区别明确，读取历史不延长证据有效期。
- **Publish 没有牺牲原 fencing 来满足预算。** `runtime_publication.go:31` 使用原 claim 24、dispatch 6、binding/index/receipt 8、mount/receipt 4、publication absence 2，共 44 条业务比较；六个业务写入加 Stage 预留 14，正好 64。index 只比较、不重写。所有准备数据来自最终同一次固定点读取，并以 ModRevision/Lease0 进入 CAS。`runtime_publication_validation_test.go:216` 在真实请求边界检查这一清单与超一项拒绝行为。
- **测试覆盖真实的竞争边界。** commit 回复丢失是在实际服务端成功后丢弃回复；迟到事务是在完整 Txn 发出前阻塞，由 resolver 先写 aborted，再验证全部业务值不变。并发 UID 争用、同值 claim/guard 重建、restore/control 改变、证据改写、post-Begin 时钟/取消/本地失效，以及永久 receipt 恢复都有行为断言。没有用单纯 mock success 代替 etcd 仲裁。
- **历史恢复的结构与权限边界得到实际检验。** publication loader 在两点发现后进行三点一致读取，要求 journal/proof/receipt 永久、不可变、同首次 revision，并绑定确切 locator。same-first-revision 的坏 receipt 测试真正到达内容解码器；coherent forged metadata 的 preparation 测试则证明结构读取结果不能越过 mutating API 的认证。

## Issues

### Critical — Must Fix

无。本次没有发现破坏永久所有权、允许重复 runtime 绑定/重复 mount intent、绕过原 claim fencing 或伪造签名授权的已交付路径。

### Important — Should Fix

**I1：AcquireIntent 与 DeclareRuntimeDispatch 的 Begin 失败路径仍未分离清理错误。**

- 位置：`internal/storage/state/etcd/workspace_acquire.go:207`–`:216`；`internal/storage/state/etcd/runtime_dispatch.go:116`–`:119`。共同上游为 `stage.go:60`–`:72` 的 `stageBeginFailure`；已修好的同类消费路径是 `runtime_preparation.go:221`–`:227`。
- 触发：guard-init Txn 在真实 etcd 成功，但回复丢失；Begin 的独立 bounded Revoke 又失败。Begin 返回 nil Stage 与携带 primary/cleanup 两个 cause 的 `stageBeginFailure`。Acquire/Dispatch 此时直接 `return result, err`，没有到达正常 Release 路径，故 `GuardCleanupError == nil`，cleanup 仍混在主错误链里。
- 影响：调用方依专用字段记录/处置未清理 guard 时，会得到错误的“没有 cleanup failure”诊断；其 primary error 还混入另一类操作失败。同样的故障在 Bind/Consume/Publish 已正确分离，在同一条创建流水线的前两步却违反既有结果契约。acquisition 计划 `:116`、`:132` 和 dispatch 计划 `:100`、`:107`、`:111` 均规定独立清理诊断。此项属于错误处理契约遗漏，不是 etcd 原子性破坏；已知业务结果和所有权安全没有被降级。
- 修复：两个入口在 Begin error 分支提取既有私有 aggregate，将 cleanup 写入各自 `GuardCleanupError`，只返回原 primary cause；保留当前 Outcome、Dispatch 的 exact Reference、nil Entry/Owner、原 Lease 清理范围和公开 BeginStage 的多 cause `errors.Is` 契约。无需重构交易协议，也不要求为此扩展 Acquire 的既有 Begin-reference API。
- 验证：增加两个真实 guard-init reply-loss + revoke-failure 的组合回归，确认只撤销本 Stage Lease、context 独立有界、无业务写入/能力返回、结果 cleanup 与 primary 分开；Dispatch 保留 exact reference 并可仲裁。`runtime_publication_fault_test.go:275` 的四个现有案例只覆盖 publish/bind/mount/stage，未覆盖这两个 sibling。代码控制流已足以确认，无需 reviewer 重跑已报告套件。
- 与已关闭 Task 4 I1 的关系：`6ed468d` 确实修好了 Task 4 指定的 publication/preparation 路径，并保持 Stage 错误兼容；本项是本次 whole-branch 审查发现的另外两个受同一契约约束的调用方。此前局部通过不豁免本项。

### Minor — Nice to Have

**M1：保留先前已接受延期的通用 builder Grant 边界深拷贝测试。**

- 位置：`internal/storage/state/etcd/stage_attempt_test.go:185`。
- 当前测试在 Begin 返回后变更源 mutation；dispatch 的 Grant hook 覆盖了 payload，但没有在 Grant 边界同时变更通用 builder 的 comparison key/range/value 与 write byte 容器。
- 独立检查实现顺序仍是 builder 一次调用、prepareMutation 复制与 digest、然后 Grant；没有发现实际 alias 漏洞。因此这是更精确的回归强度建议，继续 **接受延期、非阻断**，不要求把它混入 I1 修复。

## 先前裁决与跨模块核对

| 项目 | 本轮独立判断 |
| --- | --- |
| 先前 dispatch M1 | 仍属上述非阻断测试建议；没有默默删除 |
| 先前 dispatch M2：预期 client WARN | 本批最终日志 8 条已分类为 4 LeaseNotFound、2 Canceled、2 NOSPACE；文档如实保留，视为解释完成，不要求压掉生产日志 |
| Task 2 M1：错误 receipt 重写先撞 envelope | Task 3 的 `runtime_publication_read_test.go:92`、`:179` 同首次 revision 案例覆盖 aborted/schema/context/duplicate/missing/null，且共用 receipt decoder；对应缺口已补齐 |
| Task 4 I1 修复 | publication/preparation/public Stage 的诊断传递正确；组合真实故障测试通过记录可核对。另发现本报告 I1 的 sibling 遗漏 |
| immutable CAS 只用 ModRevision + Lease0 | 最终 coherent point snapshot 已验证值、首 revision 与关联；同 cluster/restore 下重写/删除重建改变 ModRevision。保留原 24 比较，未放宽授权 |
| public preparation/publication loaders 只校验结构 | 是明确裁决，公开注释和测试相符。Bind/Consume/Publish 在所有 mutation 前重新用 pinned roots/context/clock 认证；当前没有把公开结构 Entry 当执行能力 |
| preparation replay server fence / Publish replay | Bind/Consume 的 replay 增加原 24 的只读 server check。Publish 成功已改变四个前置永久值，exact proof replay只能返回历史元数据，因此不再使用已失效的原 publishing CAS；仍验证 local claim 与 fresh proof/clock，不 Grant/KeepAlive/执行外部动作 |
| 64 operation 与 byte 上限 | Bind 50、Consume 54、Dispatch 42、Publish 64，原 Stage 预算未提高；超额在 Grant 前失败。publication 达到上限，后续改动必须继续显式预算 |
| 完整方案与增量边界 | phases 1–5 未全部完成；未改生产 Manager/allocator/wiring，不能因本次库层通过而宣称 Redis 替代或 physical execution fencing 完成 |

## 验证证据与容量判断

本 reviewer 未重新运行已报告 suite/race/vet/build，也未新建 fixture。读取并统计修复后日志：

- `final-owned-fixture-after-fix.log`：188 个顶层 PASS、0 SKIP、0 FAIL、0 race diagnostic，`ok .../internal/storage/state/etcd 33.617s`；真实 leader pause/failover 与 NOSPACE 案例有 PASS，foreign-cluster 回归包含在同次 suite。8 条 WARN 分类如上。
- `final-repository-tests-after-fix.log`：全仓命令成功，etcd 包 27.039s；其他多个包显示 cached，不能表述为所有包均重新执行。该日志不证明全仓 race 或所有可选外部集成环境均已运行。
- controller verification 记录 source `6ed468d` 下 `go vet ./...`、`go build ./...`、`git diff --check` exit 0，以及 owned script trap 完成项目资源清理；这些是 controller 的运行证据，不声称 reviewer 独立复跑。没有宣称既有全仓 lint 问题已清零。
- fixture 危险操作的 project/container/service/loopback endpoint 归属校验在故障前执行；脚本动态端口、独立 foreign 集群、只清理自己的项目。单主机三成员可证明本次成员故障测试，不能证明主机/AZ 隔离。

`runtime_publication_capacity_test.go:18` 的实际值统计完整分类 20 个永久非 meta value，包含原七领域值与 Acquire receipt、dispatch/binding/mount/publication 及共享 index 一次。最终日志在合成 dispatch input 16 B / 4096 B / 8192 B 时，raw value 合计分别 **12,769 / 16,847 / 20,943 B**。snapshot payload 始终只有 **56 B**；certificate 与 proof 的原文已计入记录，32 B root 在 etcd 之外。

这个结果是 **GC 前保留创建工作集**，不能整体当作长期 B_live，也不能假设历史记录已经回收。当前没有 GC；20 值的 raw subtotal 已超过设计中的 8 KiB live 建模目标，后续必须分别计量真正 live 记录与 `新申请速率 × 保留窗 × 历史工作集`。key/MVCC/WAL/碎片/replica/pending/历史 workspace fence/其他任务均未包含，没有 100k/1M、4 GiB quota、吞吐或 SLO 通过结论。controller verification 正确限定了这些含义，不需要因这组诚实的样本扩展本次代码修复。

从已交付源码可确定正常流程是有界 point reads、固定数量的短 Stage/claim 资源，没有按存量 N 添加常驻 goroutine、Watch 或续租 loop；这支持后续规模设计方向，不能代替完整工作负载压测。

## Recommendations

1. 在继续下一实现单元前完成 I1 的两个调用方及组合故障回归；复用既有诊断 transport，保留 public Stage 行为和业务协议，随后做 scoped fix review。当前通过的 source `6ed468d` 日志不可直接作为修改后源码的最终验证。
2. 继续保持 publication/read-only replay 与执行能力分离。未来 trusted dispatcher/launcher 必须凭当下有效的 claim/gate/restore/expiry 执行，不能只凭历史 `OutcomeCommitted`、`DispatchReplay` 或 public Entry。
3. 后续生产接入必须以既定阶段退出条件完成物理 once-only mount/gate、GC/fenced retention、cell admission/capacity、部署 restore 与 migration；不把这些未交付能力改写成本次已经通过。

## Declined to judge（逐项列出行为及理由）

- **可信 issuer/launcher 是否确实观察了物理 ready、mount 和持久 gate**：本批只有协议签名与元数据消费者，测试使用合成签名断言；真实生产者和物理实现尚未交付。
- **真实 create/prepare 的外部 exactly-once、目标 journal 去重及网络迟到命令拒绝**：dispatch 只固定声明和恢复身份，未调用 target；这些属于后续 trusted dispatcher/helper。
- **真实 mountAttempt 1 是否对应恰好一次实际 FUSE 挂载**：本批证明永久 pre-consumption/不可更换 operation，未执行 mount；不能将元数据消费视为物理执行证明。
- **client 时间检查与服务端 commit/target 执行时刻的原子一致**：etcd 比较不提供业务 UTC 条件；本批执行 bounded controlled-clock 检查，未来 target 必须执行时重验 expiry。任意不遵守 context/immutable/concurrent-safe 契约的注入 clock 不属于支持依赖契约。
- **用户代码与管理 key/IPC/FD/proc/UID 隔离、全部后代排空、raw exec/file/stream 旁路封堵**：真实 launcher/securityContext/镜像尚未实现；协议密码学通过不代替这些安全验收。
- **Operation admission/Close/End、exclusive 生命周期与清理、owner 释放及远端写 settled/fenced**：本批没有相关执行入口或释放 API；未知外部写如何安全完成继续受原设计后续阶段约束。
- **Pool quota/preparation slot、Docker persistent recovery、Upload multipart 状态**：属于未交付领域集成，不能从创建 publication 路径推断完整支持。
- **partition scheduler/due/dirty、Watch compaction/relist、collector health/FUSE probe/auto-sync 的实际负载与恢复**：本次没有相应实现，固定点库代码不等于这些系统完成。
- **安全 GC、默认幂等窗口、历史 fence 和 pending 工作集背压**：当前永久证据不回收；GC 与 admission 未实现。其缺失限制生产接入，但不是本次只交付元数据协议的遗漏功能。
- **生产 HTTP/SDK wiring、etcd-only 配置、Redis removal、旧镜像 drain、上线与回滚**：原生产入口未切换，明确仍属整体 phases 1–5；未审未提交的 operation 草稿，也未给生产切换许可。
- **生产 mTLS 完整握手、prefix RBAC、实际 operator 初始化/authority 注册、密钥保管轮换与 restore 旧凭据隔离**：本次已审配置约束/复制/身份 fence，fixture 是本地 HTTP；部署与运维协议须后续独立验收。
- **跨主机/AZ 容灾、旧快照之外活跃 writer 的 quarantine、真实 restore 演练**：当前测试只有隔离本机多成员故障与 restore value 变化；不能据此证明基础设施隔离或外部 writer 已被 fence。
- **拥有 etcd 写权限的管理者伪造完整自洽业务记录/receipt、恶意替换受信 KV 响应**：处于受信存储/管理边界之外；一般 Stage 对 nil-success Grant/部分违约 response 的防御深度不足以在正常 etcd 契约下构成已证错误授权，延续先前明确裁决，不机械升级 mock-only 异常。
- **100k/1M 存量、实际 manifest/snapshot 分布、QPS/延迟/内存/磁盘/GC 稳态与长期 soak**：只有有限 raw-value 样本和代码访问模式，缺少相应实测；不提供容量通过判断。
- **依赖生态的最新漏洞状态及第三方包所有兼容行为**：本次审到固定 go.mod/go.sum 变化及 controller 的构建/回归证据，未开展独立供应链漏洞扫描或完整外部依赖审计；不声称依赖无漏洞。
- **无关 Sentinel 文档修改、既有 lint 基线及未提交下一阶段草稿**：不属于冻结的 28 commit scope，明确保留且不据此作产品缺陷判断。

## Assessment

**Ready to merge? With fixes — 仅针对本次已交付增量。**

完整 identity → Stage → acquisition → claim → dispatch → preparation → signed publication/recovery 链条的主要并发与认证约束成立，未发现 Critical；存在 **1 项 Important（两个 sibling Begin 路径的独立清理错误契约）**，应修复后再通过本次最终 gate。另有 **1 项已有、明确接受延期的 Minor**。整体 phases 1–5 与生产切换仍未完成，修复本项也不改变这一界限。

## 最终修复与收敛（controller补充）

source `a4412e05e52a15741fd61a238c47c775de6dcdac`补齐AcquireIntent/DeclareRuntimeDispatch两个Begin错误路径。两个真实guard-init成功后回复丢失+独立bounded原Lease Revoke失败回归RED→GREEN；七项covering race PASS2.243s，public Stage和Bind/Consume/Publish原契约通过。独立scoped review判I1 ADDRESSED、无new Critical/Important breakage，M1继续明确延期。没有第二轮whole-branch重新评分或声称整个生产方案完工。

最终changed source fresh owned189topPASS、0SKIP、0FAIL，race33.982s；全仓go test通过（etcd26.634s，其他cached包如实保留），vet/build/diff exit0。WARN8仍为明确故障注入4LeaseNotFound2Canceled2NOSPACE。前文冻结6ed468d的计数与容量是原review所读证据，最终样本与scope限制见同目录verification文档。

最终增量gate通过：无遗留Critical/Important；所有Declined-to-judge下游项仍为整体phases1–5必须另行交付/验收的范围，或明确未声称验证的外部条件，不因本gate而豁免。
