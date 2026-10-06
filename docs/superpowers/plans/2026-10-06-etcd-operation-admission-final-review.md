# Operation 整分支审查与最终修复复核

本文件保留初始失败 gate 和全部审查条目；修改后验证以 verification 文档为准。初始 I1/I2/M1 已在 29c3cf6 关闭，M2 延期。

# Whole-branch final review — native etcd foundation and Operation

日期：2026-10-06。冻结 source：`b99b823d69de6baa860eb95d47bd269599b03f42..c3fdc6f6ee8062ceb75fdc1e0c206b51dd7d98f1`，分支 `codex/etcd-state-management`。

本次独立读完所提供完整 `review-b99b823..c3fdc6f.diff`：39 commits、101 files、980,112 bytes、18,595 insertions / 12 deletions。按有界段落审阅全部实现、测试、依赖、fixture、计划、设计与历史报告；最初被工具截断的文档段落已补齐，没有以抽样或历史批准替代全分支审查。依据已批准的整体 phases 1–5 设计、已完成的 Operation 增量计划、前序实现计划、当前 progress/context 和 code-reviewer 契约。当前工作树的计划勾选进度与冻结 diff 的旧勾选状态分开理解。

审查未派子代理，未运行测试、race、vet、build 或故障注入，未修改源码、index、HEAD、分支、生产环境、运行时或凭证；唯一写入为本报告。controller 的新验证与本人的静态审查明确区分。未把 metadata library 当成完整 Redis 替代。

## Strengths

- **永久所有权与临时能力分层连贯。** `client.go:189`、`:298` 固定 operator identity/restore；Open 不初始化空 authority。Acquire 通过 Stage 同时写七个永久领域值与持久 receipt；claim 的原 Lease/guard 与六条永久记录 CAS 相接。owner、fence、control 不因短 Lease 消失。Operation 新增独立 ephemeral codec/key family，没有把短 receipt 混入永久 Stage 仲裁。
- **创建链的证据与不可变身份得到保留。** dispatch 的 operation/input、binding 的 exact ID/UID/BootID 与 UID index、mount 的一次性预消费、publication journal/proof/receipt 形成可恢复链。coherent loaders 最终在同一固定点快照重验参与事务的记录，检查永久 Lease、首次 revision、完整 locator 和归属。恢复 Entry 与公开 Reference 都不是可重建 capability。
- **认证边界具体。** `internal/runtime/controlprotocol` 分开 root-certificate 和 delegate-ready 签名域；typed canonical JSON、严格 nested schema、UTF8/escaped Unicode、签名长度、context、UTC/window 与复制逻辑互相配合。constructor 固定 roots/authority/clock，调用方不能用自己的 root、Now 或布尔完成断言替换。历史验证不延长当前授权，公开 metadata loader 的结构结果不绕过 mutating API 的认证。
- **Operation 主写事务保护原上下文。** `operation_read.go` 最终取得 placement/control/owner/fence/runtime index 五条 coherent chain；`operation_admission.go` 在 Grant 前固定输入、digest、五条 fence 与预算，内部 UUID 绑定已知原 Lease。guard 初始化未知不发送 admit；admit 原子写 token/committed receipt/可选 mutation lock，失败或未知不返回 capability。已知成功 header 后不能交付 capability 时保留历史 committed 并独立回收原 Lease，这一裁决正确。
- **续期与取消的本地约束清楚。** `operation_lifecycle.go:16` 的原 parent context、origin、mutex、旧单调 deadline、不可逆 lost 与 original-ID KeepAlive/Revoke 设计合理；本报告 I1 是服务器读一致性遗漏，不是否定这些本地保护。已入场 data 可在后来 mutation lock 出现后继续排空，mutation 则必须保有自己的 lock；Cancel 不叫 End，不宣称物理终态。
- **资源与复制约束可审计。** Stage builder 在 Grant 前一次构建、深拷贝 Cmp key/range/oneof 和 Write；签名 wire/root 与 API 输出也复制。所有已实现正常路径使用固定点集合，没有按空闲 N 新增 Lease、goroutine、Watch 或 hot operation counter。Dispatch 42、Bind 50、Consume 54、Publish 64 的预算保持；Publish 原 claim 24 比较未被删除来凑上限。Operation admit 20 comparisons、2/3 writes、最多 5 failure reads，renew 至多 30 comparisons，均有余量且固定字节上界。
- **测试多数命中真实协议边界。** 完整事务在发送前延迟、真实服务端提交后丢回复、原 Lease 撤销/自然到期、同值重写/重建、restore/control 改变、同 UID 竞争、签名和 record corruption 等均有行为断言。owned fixture 在全局故障前校验 project/container/service/loopback endpoint，NOSPACE 与 leader pause 不施加于任意外部 endpoints。

以上文件简称均位于 `internal/storage/state/etcd/`，签名协议除外。

## Issues

### Critical — Must Fix

无。本次未发现已交付入口可直接删除永久 owner、执行未经认证的物理 runtime 指令或将 Cancel 伪装成安全释放的路径。

### Important — Should Fix

#### I1 — 纯比较空分支 Txn 实际可读到旧 fence，续期错误成功

- **位置：** `internal/storage/state/etcd/operation_lifecycle.go:54`；同根因 sibling `internal/storage/state/etcd/runtime_preparation.go:252`。
- **触发：** 另一客户端已经成功重写 mutation lock、重建 placement、改变 control/restore 或失效其他原 fence；renew/replay 的空 Then/Else Txn 落在尚未应用该变更的 follower。`Txn.If(...).Commit()` 与 `.Then().Commit()` 并不因含 Compare 自动获得 linearizable barrier。
- **证据：** controller 在当前 source 的全仓测试真实失败：`/tmp/etcd-operation-repo-final.log:32` 起，`TestRenewOperationIrrevocableLoss/mutation_rewrite` 和 `/placement_recreate` 在 `operation_lifecycle_test.go:172` 期望 error，却收到 nil。本次读取核对了失败日志。controller 随后一次 focused PASS 说明交错敏感，不能撤销原失败或当作环境噪声。
- **根因：** 已独立核对固定 server 3.6.15 源码：`IsTxnReadonly` 与 `IsTxnSerializable` 对 success/failure 都为空时返回 true，比较条件不参与 serializable 分类；server 因此跳过 `linearizableReadNotify`，用本地 read view 执行比较。[etcd 3.6.15 transaction classification](https://raw.githubusercontent.com/etcd-io/etcd/v3.6.15/server/etcdserver/txn/txn.go#L608)，[etcd 3.6.15 Txn RPC](https://raw.githubusercontent.com/etcd-io/etcd/v3.6.15/server/etcdserver/v3_server.go#L145)。这一源码机制与观测失败相符；不声称日志单独证明了具体请求所在 follower 的 applied index。
- **影响：** Renew 可在已失效原 control/lock 等条件后 KeepAlive 原 Lease、返回成功并保留本地能力，违反“原 fences 改变即失败并 irrevocably lost”。KeepAlive 成功只证明 Lease 可续，不重验领域 fence。Bind/Consume replay 同样可错误声称服务器 claim/control fence 仍有效；它们当前仅返回元数据，因此不扩大为物理执行越权结论。成功 Stage 写事务自身仍执行 Raft/CAS，未因本项绕过原写比较。
- **完整同类核对：** 已检查 native package 全部生产 Txn 调用。发现上述两处空分支；`creation_claim.go:224` 虽 Then 为空，Else 含默认非 serializable OpGet，因此整个请求会强制 barrier；其余只读路径有默认 point Range，写路径有 Put/Delete。不能把所有 `.Then()` 都机械认作同病，也不能只修新的 Operation 而遗漏 preparation。
- **修复：** 在这两笔比较事务自身包含明确默认线性一致的固定 point Get，并验证更新后的 response shape/header/key；保持 no-write、原比较、固定点与预算。可在 Then/Else 同读一个稳定 metadata point，让响应规则一致；不要加 `WithSerializable`。另一次独立前置 Get 不能保证随后空 Txn 在另一个 endpoint 上使用相同线性快照。
- **回归：** 增加真实 transport-boundary request 断言，证明含至少一个非 serializable Range 且仍无写，覆盖 Renew 与 Bind/Consume replay；保留真实 changed-fence/recreate 测试并覆盖失败不 KeepAlive/标记 lost。修复后完成 controller fresh gates；无需靠大量重跑未改代码来碰碰运气。

#### I2 — failed-admit 的半套原证据仍能发布 known historical outcome

- **位置：** `internal/storage/state/etcd/operation_admission.go:406`、`:418`、`:429`；覆盖缺口 `operation_admission_fault_test.go:213`。
- **触发：** failed branch 的 point envelope、identity/restore 均合法，receipt 是匹配 reference/Lease 的 committed，但 token 或 guard 缺失；或者 guard 同值重建后首次 revision 不再是原 guard。当前循环跳过 nil，且只有 token 非 nil 才调用 completion validator；没有校验 guard 的原 `c.guardRevision`，也没有校验 completion 在 guard 之后。于是 receipt candidate 最终写到 `result.Outcome`，返回 `ErrConflict`，将不完整/不再属于原 guard 的历史证据当作已验证结果。
- **具体路径：** 缺 token 时 `values[2] == nil` 跳过 `validateOperationCompletion`；缺 guard 时循环跳过 `values[1]`，即使 token/receipt 自洽也会通过；单条 receipt 缺 guard/token 两者也可返回 committed。合法 aborted receipt 加缺 guard 的形状也未被拒绝。这些都是当前 Operation 明确支持的严格损坏/半套证据契约，不能以通用 Stage 旧有的 trusted RPC 边界裁决免除。
- **影响：** 没有返回 capability，但 API 暴露错误的 known historical outcome/普通 CAS conflict，违背“全部必要原 evidence 有效后才能发布结果”。同一 stored shape 在 `operation_resolve.go:142`、`:157`、`:171`、`:180` 会返回 unknown/corrupt，两个消费者裁决不一致。`470db72` 修好了先赋值后校验的问题，却没有把 nil/原 revision/时间顺序纳入“complete evidence”；原修复的四个测试只覆盖 completion revision、guard body、token Lease 和 valid historical。
- **修复：** 在赋值前明确分类完整合法形状。历史 committed 必须有原 guard、exact token 与 receipt，全部原 Lease/Reference/Record、immutable envelopes、原 guard first revision、token/receipt 同首次 revision且晚于 guard；aborted 必须有原 guard、无 token，并满足 receipt 归属及在 guard 后的首次 revision。half-set 保持 unknown 并返回对应 corruption，不用 nil-as-skip。可共享严格私有 evidence 分类以避免与 resolver 漂移，但保持各 API 可用证据的区别，不为了重构额外扩大 failure read 集合或引入 runtime inference。
- **保留裁决：** 合法、完整 failed-branch 历史证据仍可返回已知结果；成功 admit header 已证明 committed 而随后因 context/deadline/live-evidence 不能交付 capability 时，仍保留 committed。已确定无写的普通 CAS rejection、原 Lease 清理错误独立字段均不应因本修复被无意改写。
- **回归：** 扩展现有真实提交/point evidence wrapper 用例，覆盖 missing guard、missing token、receipt-only、aborted-without-guard、同值原 guard 重建与错误 guard/completion 顺序；断言 unknown、nil capability、明确 corruption、原 Lease cleanup 与 reference 保留。valid historical 和 success-header-undelivery 作为正向对照。静态分支足以确认本遗漏，reviewer 未为已报告套件重复建 fixture。

### Minor — Nice to Have / explicit triage

#### M1 — 延迟 admit 的早期断言失败没有在 fixture teardown 前 join

- **位置：** `internal/storage/state/etcd/operation_resolve_test.go:25`、`:30`、`:58`、`:86`。
- **问题及影响：** 正常路径在 `:90` 等待 done；早期 require/t.Fatal 只 defer release，没有 bounded join。defer 的 Lease hook 恢复还可能早于 release，后台 Begin 继续执行时与恢复 hook/关闭 client 交错，产生告警并掩盖原错误。goroutine 内 fatal require 也可能使 result send 不发生。已有历史 RED 告警不推翻 GREEN，但失败诊断应可靠。
- **修复与 triage：** **建议纳入同一次最终修复波，非生产协议阻断。** 注册在 fixture cleanup 之前执行的 release + bounded completion join；用独立 completion 信号保证 goroutine 即使断言退出也能报告已结束，再恢复 hooks/关闭资源。保留正常路径 assertions，避免测试 teardown 为 join 无限等待。旧 Stage/Acquire 等延迟测试也有 release-only cleanup 模式；本轮先修已明确报告的 Operation 用例，通用测试 harness 的系统整理可后续做，不要求扩大本次协议修复。

#### M2 — 通用 Stage builder 尚缺实际 Grant 边界的完整复制回归

- **位置：** `internal/storage/state/etcd/stage_attempt_test.go:185`；生产顺序见 `stage.go:93`、`:109` 与 `mutation.go` 的深拷贝。
- **问题及影响：** 当前通用测试在 Begin 返回后变更源 mutation；dispatch 的 Grant hook 覆盖 payload，但没有在通用 builder 的 Grant 边界同时修改 comparison key/range/value 与 Write bytes。代码已经在 Grant 前私有复制并算 digest，本次未发现实际 alias 漏洞。
- **triage：** **继续接受延期，非阻断。** 这是前序 whole-branch 明确保留的测试强度建议，未被本轮丢弃。以后增加 Grant hook 精确验证源容器变更不影响 digest/实际比较/持久值；不要求为它复跑无关整套测试。

## 前序裁决与集成核对

| 事项 | 本次判断 |
| --- | --- |
| publication whole-branch 的 Acquire/Dispatch Begin cleanup I1 | `a4412e0` 的两个 sibling 提取 `stageBeginFailure`，与 Bind/Consume/Publish 一致；主错误与 cleanup 分开，public Stage errors.Is 多 cause 保持。已关闭，没有重复计为当前 finding。 |
| Task 2 outcome-before-validation I1 | 赋值时机已修；本报告 I2 是完整 evidence 判定仍遗漏形状与原 revision，不能由旧 scoped approval 豁免。 |
| preparation replay 的原 server fence 裁决 | 需要这项检查的契约仍成立；现在发现该实现缺线性 barrier，列 I1，不把以前报告的“检查成功”当保证。 |
| immutable CAS 的 ModRevision + Lease0 | coherent loader 先验证内容、关联与首次 revision，再使用原 ModRevision；同 cluster/restore 下改写/重建会改变 revision。保留原 24 claim 比较与原预算，未发现需要无差别增加 Value 比较的理由。 |
| restore 记录错位错误分类 | 沿用 `domainEpoch` 的 `ErrIdentityMismatch`；其他归属/chain 错位是 corruption。这是明确 API taxonomy，不降低授权条件。 |
| fixed30s 与 server minimum TTL | `0 < response TTL <= 30` 的 fail-closed 策略保留；配置 minTTL >30 的 server 会被拒绝，并只清理已知原 Lease。这是已裁定互操作限制，不静默扩展能力寿命。 |
| all-own-three absent 与 foreign mutation lock | resolver 对合法他人 lock 仍可返回 expired；expired 只指原 metadata 能力不能提交，不证明 target 终态。malformed foreign lock 仍不能用于可信分类。 |
| data under later mutation | 已入场 data renew 不额外比较 lock absence，避免把后来 mutation 入场误作旧 data 已结束；真正排空仍需 future target gate。 |
| clock 与期限 | constructor observation 的 uncertainty + sample elapsed 有界，UTC 与 Δ 检查；本地 sent-time deadline 使用单调时间；没有 caller wall-time fallback。已有操作跨业务 expiry 排空不更新 expires_at。 |
| public metadata loader / replay | 结构只读、不重构能力；mutating APIs 另做 pinned crypto/context/clock。Publish exact-proof replay 返回历史事实，其成功已改原 publishing domain，不能照搬 Bind/Consume 的前置状态 CAS。 |
| 预期 WARN 与既有 lint | 既有 WARN 分类保留，不通过关闭生产日志“清零”；旧 lint baseline 不计本批通过项。 |

## 验证与容量证据

controller 当前 source `c3fdc6f` 的独立结果如下；本 reviewer 读取了相关日志与 ledger，没有重复执行命令：

- `bash scripts/test-etcd-state.sh -v`：fresh owned 三成员 + foreign fixture，**213 top-level PASS、0 SKIP、0 FAIL**，etcd package **race 101.634s**；日志 `/tmp/etcd-operation-owned-final.log`。controller 确认 exact project `sandbox-etcd-state-test-6236-1791297082` 的 container/volume/network 均清理。43 条 WARN 已由 controller 分类为 38 LeaseNotFound、3 Canceled、2 NOSPACE，对应故障/幂等路径；不表述为无 warning。
- 带实际 manual endpoints 的 `go test ./...`：**FAIL**，etcd package 85.464s；两个实际失败见 I1。其他包通过或 cached。一次 focused 再跑 PASS 1.234s 不能将本 gate 改成 PASS。
- `go vet ./...`、`go build ./...`：controller 报 exit 0，两个 final 日志为空。实现者报告的 gofmt/diff check 通过属于此前证据；本 reviewer 未另跑，也没有称全仓 race 或 golangci clean。
- Task-level package/race 的通过记录和本次 owned PASS 均保留为真实证据，但不能覆盖另一有效环境中的确定失败。下一修复 source 要重新建立相应最终证据，不能沿用冻结 HEAD 的绿色日志。

已实现路径的 fixed point/RPC/Lease 结构支持“空闲记录不会在这些 API 内产生 per-N 续租”的局部结论。`TestOperationActiveCostIndependentOfIdleRecords` 的 **1,000 synthetic idle** 只验证 fixed RPC/point shape；没有测真实巨大 N、真实 Pod/FUSE、持续 workload、吞吐、延迟或 SLO。

前序 creation capacity 实测是 **GC 前 20 个永久 KV raw value**：16 B / 4 KiB / 8 KiB 合成 dispatch input 对应约 **12.8 / 16.8 / 20.9 KB**；snapshot 固定 **56 B**。已包含证书/proof 原文，不重复计同一 index。未含 key、MVCC/WAL、碎片、副本、pending、历史 workspace fence 等；不是长期 B_live，更不是设计 8 KiB live 目标已达成。当前无安全 GC，必须在后续按 live 与创建速率×保留窗的历史工作集分别计量和背压。单主机三成员不代表跨主机/AZ 容灾。

## Recommendations

一次最终修复波处理 I1 的两个调用方、I2 的完整证据分类及建议纳入的 M1；保持 M2 明确延期。修复后 scoped review 检查该具体问题及改动引入的新 breakage，再由 controller 对修改后 source 完成必要的 focused/final gates；当前不能把 Operation gate 记为完全通过并向依赖其正确性的下一单元推进。

继续后续已授权实现时，保持 metadata admission → durable effect/command → target execution/terminal 的权限分界。原生 close/exclusive/task/cleanup/pool、可信 launcher/issuer 与运维 migration 都仍是整体要求；不能通过更名 Reference/Entry 或把 Lease 删除当“排空”省掉这些步骤。

## Declined to judge — 每项行为及理由，供 executor 裁定

- **真实 target create/prepare 的一次执行、去重及网络迟到命令拒绝：** 当前 dispatch 只持久固定 operation/input 与原 claim 元数据；真实可信 producer/adapter 未交付，签名测试不能证明物理执行。
- **issuer 断言的实际 ready、gate、FUSE mount 来源：** 当前只验证已签名断言与 context；没有真实 issuer/launcher 观察过程。属整体必须实现项，不能由密码学通过代替。
- **FUSE mountAttempt=1 对应物理恰好一次挂载：** 当前证明永久预消费与 operation 不替换，没有调用 mount；需后续实际 helper 故障验证。
- **Operation command 签发、durable effect intent、target terminal 与安全 End：** 本批仅 admission/resolve/renew/cancel，未产生外部指令；Cancel 与 expired 不证明执行结束，后续协议仍必需。
- **实际 CloseAdmission/exclusive/reopen/destroy、task checkpoint 与安全 owner release：** 没有这些原生实现，必须补齐 target closed gate、两层排空、发送端隔离和远端 settled/fenced evidence，当前不作完成判断。
- **业务到期检查与 etcd commit 或 target 执行时刻原子一致：** etcd 不比较业务 UTC，本批明确提供受控时间的事务前检查；真正 target 执行前复验仍待实现，不能承诺绝对墙钟边界。
- **不遵守 context/并发安全/不可变依赖契约的注入 AuthorityClock、signer/CA 对象：** 属支持依赖契约之外；本次审查了正常契约下的复制、deadline 和误差校验，没有承诺能强制终止任意阻塞依赖。
- **用户进程与管理 UID/key/IPC/FD/proc 隔离、全部后代排空及 raw exec/file/stream 旁路封堵：** 真实 PID1 launcher、镜像、安全配置尚未交付，必须按整体方案进行攻击/故障测试。
- **长期 runtime 管理证书 rotation/recertification 与业务 TTL 延长：** 初始 publication birth certificate 的有效期不能充当后续所有管理授权；独立的持续管理身份与轮换契约仍待后续实现。
- **native pool ready/quota/runtime/preparation slots 与未知创建回收：** 当前没有 pool 集成，固定点 Operation 不能证明不超卖或 unknown create 安全释放配额。
- **Docker daemon 绑定的持久恢复与 multipart Upload CAS/effect 恢复：** 属尚未交付的领域集成，不能从当前 Kubernetes 风格签名 fixture 推导完成。
- **partition scheduler、due/dirty、Watch fixed-revision/relist/compaction、共享恢复预算：** 本批没有相关实现，现有库不 scan N 不等于旧生产全量 loop 已删除。
- **真实 collector freshness/chunk/ACK、local health probe 挂死占槽、readiness 与 auto-sync 负载：** 尚未实现相应执行系统，没有 FUSE/稠密节点实测，继续作为整体必要验收。
- **安全 GC、幂等保留窗、历史 fence/pending/journal 水位与 admission backpressure：** 当前保留永久证据且未实现 GC，有限记录样本不能证明长期稳态；不得以年龄删除 unknown。
- **生产 Manager/HTTP/SDK wiring、etcd-only 配置、旧镜像全源 drain 与完整 Redis removal/rollback：** 当前生产仍使用 Redis，phases 1–5 未完成；本报告没有生产切换或删除保护许可。
- **生产 mTLS 完整握手、prefix RBAC、operator authority 注册、密钥保管和实际轮换：** 当前检查配置/逻辑约束，fixture 是 loopback HTTP；部署、权限和运维演练必须后续验收。
- **真实灾难恢复、snapshot 后额外 writer quarantine、旧凭证隔离与跨主机/AZ 故障：** 当前只有 identity/restore 值 fencing 与单机多成员故障，不证明对象写入隔离或完整基础设施恢复。
- **授权管理者伪造整套自洽永久事实或恶意受信 KV 替换，以及 generic Stage 的违约 nil-success Grant/异常 RPC envelope：** 延续已明确的 trusted etcd/client 边界，没有发现正常服务契约下的 generic Stage 错误授权。本次 Operation 计划明文要求的 partial/malformed evidence 仍在审查范围，已列 I2，不能混同豁免。
- **所有旧测试 harness 的统一 release/join 重构：** 本次已列具体 Operation M1，并观察旧延迟测试同模式；没有为非产品行为扩展成全面测试基础设施工程，后续可统一整理。
- **真实 10k/100k/1M capacity、Pod/FUSE 数、QPS/p99、GC 稳态、24h soak、低 cache hit 及恢复尾部：** 当前只有有限 byte 样本、synthetic fixed-shape 与局部协议测试，缺少相应负载证据，不能给 SLO/规模通过结论。
- **多 cell 路由、跨 cell 幂等与迁移：** 主设计阶段六明确以后单独交付，当前第一版为单 cell，不推断跨集群原子性。
- **第三方依赖生态最新漏洞及所有兼容行为：** 已读固定 go.mod/go.sum 与相关 etcd 3.6.15 源码，未做供应链漏洞扫描或全部依赖审计，不声称无漏洞。
- **无关 Sentinel 工作树修改、旧 lint baseline 和本 source 之外未实施内容：** 不属于冻结审查实现，保持不动，不将其混入本次新问题计数。

## Assessment

**Ready for the next authorized implementation unit? With fixes，当前尚未通过最终 gate。**

**0 Critical、2 Important、2 Minor。** 两项 Important 分别是实际 stale-fence 续期失败与 failed-admit 完整历史证据判定遗漏；应在一次修复波完成并核对修改后验证。M1 建议同波处理，M2 明确继续延期。完成这些修复后才能以本增量为可靠基础继续后续已授权单元；这不等于 phases 1–5、生产迁移、Redis 删除或实际巨大 N 容量已经完成。


---

## 单次最终修复波的独立 scoped review

**I1 — Empty comparison Txns could accept stale fences — ADDRESSED.** `internal/storage/state/etcd/operation_lifecycle.go:57` and `internal/storage/state/etcd/runtime_preparation.go:254` now put the default linearizable identity point Get in both branches of the comparison Txn itself. Neither branch writes, scans, uses WithSerializable/historical revision, or relies on a separate read. `fence_check.go:8` checks outer identity/positive revision, exactly one Range, nested header consistency, count/More, exact key, and coherent KV revisions; successful comparisons also require the expected permanent identity value/Lease0. Original comparisons remain intact. Renew rejects failed or malformed checks before KeepAlive and retains irreversible loss (`operation_lifecycle.go:23`, `:62`, `:73`). The actual gRPC request assertions cover Renew and Bind/Consume replay, successful and recreated-guard cases, all original comparison counts and 64/256 KiB bounds (`fence_check_test.go:34`, `:94`); the 28 malformed cases check no KeepAlive and no revival (`:119`).

**I2 — Incomplete failed-admit evidence could publish historical outcomes — ADDRESSED.** `internal/storage/state/etcd/operation_admission.go:395` explicitly classifies the three evidence records. Present guard/token must strictly decode and equal both the private original record and exact bytes (`:401`). Receipt-backed history requires a present original guard at the known first revision (`:427`), completion after that guard (`:430`, `:433`), and either no token for aborted history or the complete committed token/receipt pair at the same first revision (`:436`). Existing strict decoders enforce immutable envelopes and the original Lease, and completion validation enforces full Reference equality (`operation_records.go:112`, `:131`). Outcome publication occurs only after these checks (`operation_admission.go:444`). Missing guard/token, receipt-only, reconstructed guard, and invalid ordering therefore remain unknown/corrupt with nil capability. With neither token nor receipt, a validated known-no-write rejection still returns Aborted/ErrConflict (`:408`), as required. Success-header commitment followed by failed capability delivery remains committed (`:226`). Expanded real-commit/point-evidence tests retain valid committed/aborted controls and check original reference, revoke and independent cleanup diagnostics (`operation_admission_fault_test.go:213`, `:317`).

**M1 — Delayed admission lacked failure-safe completion join — ADDRESSED.** `internal/storage/state/etcd/operation_resolve_test.go:77` registers release, context cancellation and a five-second completion join after fixture cleanup registration, so it runs first. Hook restoration and bounded original-Lease cleanup follow observed completion; timeout reports failure without restoring the running worker's hook. The worker defers closing an independent finished channel (`:94`), including require/Fatal/Goexit paths where the result is never sent. The bounded child-process regression explicitly exercises parent and worker Fatal and checks completion/hook ordering (`:153`). Normal assertions and all four delayed scenarios remain.

**M2 — Generic Stage Grant-boundary comprehensive copy regression — DEFERRED, NONBLOCKING.** The authorized brief explicitly excludes implementation in this wave. `internal/storage/state/etcd/stage_attempt_test.go:185` remains the previously recorded follow-up; no generic Stage changes appear in the fix package.

**Scope check:** Read the complete supplied three-commit package `review-c3fdc6f..29c3cf6.diff` in bounded segments after the initial tool output was truncated: seven files, 406 insertions/40 deletions, base `c3fdc6f6ee8062ceb75fdc1e0c206b51dd7d98f1`, head `29c3cf6a40f1b2545071e565fdcc0fb655e26362`. Reviewed only I1/I2/M1 and new breakage introduced by this diff, using immediate validator/caller/fixture context. This is not a fresh whole-branch review. Original creation comparisons, no-write fixed points, original Lease/deadline/context/loss behavior and resource budgets are retained; no runtime inference, public-reference capability adoption or Lease resurrection was introduced.

**Verification check:** Read `final-fix-report.md` and the six named GREEN/race logs. All six logs contain the reported native package `ok` output and exact durations: I1 10.071s/13.587s, I2 12.805s/16.805s, M1 33.118s/33.964s. The report names the new covering tests and preserved Renew/server-fence/active-cost, all BeginOperation and delayed-resolution controls; those selections correspond to actual test declarations and the amended behaviors. Read RED diagnostics: I1 shows all six wire cases lacked a branch Range and 28 old empty-response acceptances; I2 names the 11 newly rejected incomplete/recreated/order/exact-byte shapes; M1 shows unfinished admission and premature hook restoration before closing-client warnings. These logs are consistent with the diff. Package vet/gofmt/diff-check are implementer-reported checks, not independently rerun here. No suite, fixture lifecycle, faults or external effects were run by this reviewer; fresh final-source gates belong to the controller.

**New breakage in the fix diff:** None found, including no new Critical/Important issues.

**Out-of-scope observations:** No new observations. M2 remains the explicit accepted nonblocking follow-up; prior whole-branch declined-to-judge requirements are unaffected.

**Fix round:** All required findings addressed, no new Critical/Important breakage. No open I1/I2/M1 finding. M2 remains deferred; final controller source gates are a separate completion requirement.


## Executor 全部 declined-to-judge 裁定及 Rulings

Ruling: M2 generic Stage Grant-boundary deep-copy regression remains explicitly deferred; existing prepareMutation copies before Grant and no production alias defect found — keep this nonblocking test-strength recommendation without expanding the protocol fix — cost if wrong is a future mutation alias regression escaping the current boundary test, tracked for later coverage.
Ruling: final-review Declined-to-judge items remain classified individually below; mandatory phases1–5 requirements are retained, no supported behavior or safety requirement is silently waived — this increment only establishes Operation metadata and its exact original authority — cost if wrong is rework of downstream protocols/acceptance, and production cutover remains blocked until their verification.

## Executor dispositions for all 23 final-review declined items
| # | Item | Disposition |
| --- | --- | --- |
| 1 | Actual target create/prepare, dedup, late command | Required later trusted producer/adapter and fault tests; current durable metadata not physical exact-once proof. |
| 2 | Actual issuer ready/gate/FUSE observation | Required later launcher/issuer observation; valid signatures alone do not discharge it. |
| 3 | Physical mountAttempt=1 | Required helper mount fault tests; current preconsume cannot certify physical mount. |
| 4 | Operation command/effect/terminal/safe End | Required subsequent protocol; cancel/expired never terminal. |
| 5 | Close/exclusive/reopen/destroy/task/owner release | Required durable closed gate, dual drain and settled/fenced remote evidence. |
| 6 | Absolute UTC atomic with etcd commit/execution | Unsupported absolute guarantee; retain controlled pre-Txn check and required future execution recheck. |
| 7 | Dependency violating context/concurrency/immutability contract | Outside supported injected clock/signer contract; no arbitrary blocking dependency termination claim. |
| 8 | UID/key/IPC/FD/proc isolation and descendants/raw transport | Required actual launcher/image/adapter attack and drain tests. |
| 9 | Ongoing management cert rotation/business TTL extension | Required independent ongoing identity; initial publication birth cert is insufficient. |
| 10 | Native pool/quotas/slots/unknown create | Required subsequent repository and sticky reservation; no overcommit claim. |
| 11 | Docker persistent recovery/upload effects | Required bound-daemon single-instance lifecycle and multipart CAS/effect integration. |
| 12 | Scheduler/due/dirty/Watch/recovery | Required subsequent implementation; old production loops remain until wiring. |
| 13 | Collector/probe occupancy/readiness/autosync load | Required actual execution/collector implementation and dense-FUSE acceptance. |
| 14 | Safe GC/history/pending/backpressure | Required terminal/fenced proof and capacity limits; unknown never age-deleted. |
| 15 | Production Manager/API/SDK/config/drain/Redis removal | Required phases1–5 integration; current native library does not complete replacement or authorize deploy. |
| 16 | Production mTLS/RBAC/operator/key rotation | Required deployment/permissions/operational verification; loopback fixture not proof. |
| 17 | Actual restore/writer quarantine/old creds/AZ | Required operator isolation and disaster recovery drills; logical fences/local members insufficient. |
| 18 | Malicious trusted KV/manager or generic Stage abnormal RPC | Outside trusted etcd/client contract as previously ruled; Operation partial/malformed evidence remains explicitly supported and I2 is being fixed. |
| 19 | All old harness release/join | Defer general test infrastructure cleanup; fix the reported Operation M1 now. |
| 20 | Real 10k/100k/1M and 24h/SLO/GC/recovery capacity | Required unverified acceptance matrix; 1000 synthetic idle proves bounded shape only. |
| 21 | Multi-cell/cross-cell migration | Explicit phase6 later, current one-cell semantics only. |
| 22 | Latest dependencies/all vulnerabilities/all compatibility | Unverified supply-chain certification, no vulnerability-free claim; pinned modules/source review only. |
| 23 | Sentinel edit/old lint/outside-source content | Preserve unrelated edit, do not stage or claim old lint clean. |
