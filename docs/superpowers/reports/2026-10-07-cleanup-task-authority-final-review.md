# Cleanup task authority — 一次完整单元最终审查

范围：`23e4337..9f31ae55319cbf8f6b69cdb7b20165a69e859ccc`。独立复用审查席位 `/root/launcher_kernel_bootstrap_implementation`，遵循 Ruling2；本人未编写此范围任何变更。**本单元可合入：Yes；Critical 0 / Important 0 / Minor 7；Declined-to-judge 56 项。** 此结论是元数据单元门禁，不是完整 Redis 迁移、生产部署或物理清理完成证明。

## Strengths

1. **严格记录与诊断/授权边界清楚。** [task_records.go](/Users/dysodeng/project/go/cloud/sandbox/internal/storage/state/etcd/task_records.go:57) 校验 canonical 非零 UUID、完整 namespace/restore/reference、workspace partition、runtime/generation/gate/snapshot/expiry；三个不可变永久记录与可更新 checkpoint 使用不同 revision 规则。4096/2048 字节限制、严格递归 schema、UTF-8 与临时对象赋值一起防止遗漏/null/重复字段或失败 decode 改写目标。[task_read.go](/Users/dysodeng/project/go/cloud/sandbox/internal/storage/state/etcd/task_read.go:17) 的公开精确点读取只给 owned diagnostic values；私有 claim bundle 另外验证完整关联链，没有把诊断 reference 当作能力。
2. **destroy 与 operation 的排序使用同一权威 control。** [task_destroy.go](/Users/dysodeng/project/go/cloud/sandbox/internal/storage/state/etcd/task_destroy.go:20) 要求调用者的原 positive control revision；同一 Stage 比较五点各 value、Lease0、CreateRevision、ModRevision，以及四个新 key 不存在，原子写入 destroying control、task、intent、link、initial checkpoint 和协议 receipt。它保留原 owner/fence/index/runtime/gate/expiry/snapshot 与原 operation token。原 admission 的 control ModRevision CAS 被这次写入实际阻断；begin-first、destroy-first、延迟旧 admission 和竞争 destroy 的测试检查了这条接口。准备 Stage 本身不被称为关闭成功。
3. **claim 的永久出生关系和租约权威没有混淆。** [task_read.go](/Users/dysodeng/project/go/cloud/sandbox/internal/storage/state/etcd/task_read.go:85) 将 discovery 的 task 在最终十点读取中重新验证；要求 task/intent/link/checkpoint 同 birth、destroying control ModRevision 等于 birth、旧 active revision 更早，以及原五点 tuple 一致。[task_claim.go](/Users/dysodeng/project/go/cloud/sandbox/internal/storage/state/etcd/task_claim.go:96) 先预检再 Grant，以原 Lease 原子创建唯一 claim/guard，拒绝已有 claim，不从引用或旧 Lease 重建能力。self/origin 检查先于锁，公开 Reference 是不可变诊断副本。
4. **本地不可复活与服务端提交围栏各自明确。** 八个永久点、claim 和 guard 都比较 value/Lease/Create/Mod；[task_claim.go](/Users/dysodeng/project/go/cloud/sandbox/internal/storage/state/etcd/task_claim.go:211) 的两条分支都有默认线性一致 Get，不把空分支 compare-only Txn 当新鲜性证据。Grant/KeepAlive 的发送前单调时间与实际有界 server TTL 形成保守截止；Renew 在原 deadline 下完成前后围栏检查，错误/取消/过期后 lost，不授予替代 Lease。Release 先 lost，只撤销原 Lease。原服务器最小 TTL 大于请求值时允许有界实际 TTL，真实 short-request 测试已覆盖。
5. **可变 checkpoint 没有钉死在初始 ModRevision。** [task_checkpoint.go](/Users/dysodeng/project/go/cloud/sandbox/internal/storage/state/etcd/task_checkpoint.go:28) 保留 birth/reference/合法 attempt/归因校验，每次独立读取最新 checkpoint 并 CAS ExpectedRevision；同一 live claim 可连续两次更新，stale CAS 可在仍有效的同一 claim 下重新准备。Stage 捕获的是原 claim/guard 全部围栏，新 claim 无法给旧 Stage 重新授权。只有 pending / needs_reconciliation，不能写入物理成功状态；parent cancel 不是已排队 Stage 的远端撤回证明，这一点在源码注释和真实测试中一致。
6. **预算与成本按当前作用域落实。** destroy 的 24 比较 + 5 写 + 14 协议保留 = 43；checkpoint 的 40 原权威比较 + 4 checkpoint 比较 + 1 写 + 14 = 59，均在 64 内。typed 最坏形状、字节限制和 owned mutation 预检在 Grant 前；新代码没有 per-idle-sandbox client、goroutine、timer、ticker 或 Watch，也没有新增依赖/Manager/backend 选择。持续 claim 的 Lease 与显式 renewal 工作仍有实际成本，永久历史增长未伪装成免费或已 GC。
7. **测试包含真实有意义的对抗路径。** 递归 codec 失败和目标原子性、40 个 claim 围栏破坏、8 个 admission bundle 破坏、restore/post-renew fence、真实 RPC 后损坏/丢失响应、延迟跨过原 deadline、natural expiry/revoke、新 claim 后旧 Stage 拒绝、连续 checkpoint 与原 receipt 仲裁均有对应断言。故障 wrapper 在真实服务端 RPC 后干预返回，不能等同于纯内存模型。原始失败、host skip、actual native、fixture 告警和 source 演变被分别保留。

## 审查方法、计划符合性与证据边界

- 完整读取 fresh dispatch、58 行正式 spec、71 行 plan、当前 OWN 的三份最终实现报告、三份任务审查、完整 progress/Ruling1–8；完整阅读原范围 packaged diff 的 2,990 行 / 148,293 字节直至 EOF，含 13 commits、19 files、2,836 additions。先前显示截断的文档区间另行补齐；没有据抽样 diff 给 whole-unit verdict。没有读取未来 gate 草稿作为当前要求。
- 这是一次完整新单元审查，并非第二次独立审查各任务。Task1 的跨任务 CV1、Task2 的未来 Task3 CV4 已在本次完整源代码和关联证据中得到检查；未把它们继续当作尚未实现。Task3 的 cross-task seam 要求也已检查。原 Stage 的成功 CAS 没有 guard ModRevision 或本地单调截止是正式 Ruling7 保留的原协议，不是漏实现的新 TaskClaim 要求；新的 claim/guard 四字段和 deadline 仍完整。
- 仅为具体接口风险读取已有依赖：`operation_read.go:26–151`（五点一致 bundle）、`domain_read.go:27–86`（当前 identity/restore 的精确点读取及固定结果槽）、`runtime_preparation_records.go:127–200` / `domain_records.go:324–342` / `operation_records.go:96–135`（strict schema、UTF8、临时赋值）、`stage.go:77–245` / `mutation.go:1–100` / `receipt.go:25–122`（owned mutation、Grant 前预算、原 guard/receipt/abort 仲裁）、`operation_admission.go:275–284,330–379`（同 control CAS 和严格响应）、`fence_check.go` / `stage_response.go`（线性读取、完整 envelope/nested header）。没有借此宣称重审全部旧实现。
- 读取现存测试结果，没有启动 tests/race/vet/build、fixture、Docker 或子代理。唯一只读格式检查 `gofmt -l` 对 14 个新增 Go 文件输出为空；未使用 `-w`。唯一写入为本报告。检查过程曾有一个只读 Python 表达式括号错误，修正后成功；一次不存在路径的 shell glob 没有读取任何文件，随后未再使用。两者均为审查工具错误，不是产品/测试结果。
- 三份任务报告里的历史 label 已核实：Task1 schema baseline 的 omission/null/duplicate 是实际行为 RED；名为 `task-1-records-green.json` 的记录真实 exit1，是错误 UTF8 测试输入被 JSON 替换后的断言失败，后续固定测试 exit0。Task2/3 tests-first exit1 是缺少新 API 的编译失败，不能称为行为 RED；TTL 修复也没有伪称 host 上观察到真实 server 行为 RED。Task1 原 setup mount assertion 在 Go 运行前失败，Root 的原 18-command audit保留为 setup-only，不计产品 PASS/FAIL。
- **独立解析并核对 accepted 121 个 raw JSON 的完整字节、SHA256 和 exit**：Task1 21 个、Task2 25 个、Task3 A/B/C 各25个，均匹配其 retained audit，均 exit0。解析完整 stdout/stderr 到 EOF，逐类查看测试与 metadata WARN/ERROR；Task1/2/3 的结果如下。源文件/编译器的大规模 frozen-copy/Git custody 与实际 fixture安全配额/启动/清理仍是 Root retained operations；我没有重做这些环境动作，也没有独立重哈希每个 frozen compiler/source 副本。

|实际 retained gate|通过 top/sub|FAIL / SKIP|直接证据|
|---|---:|---:|---|
|Task1 strict records / keys / read copies / identity|4 / 10|0 / 0|`real-etcd-sandbox-etcd-state-test-12556-1791360153/raw-015.json`|
|Task2 destroy atomic / ordering / unknown / fences|4 / 38（37 native + 1 pure）|0 / 0|`real-etcd-sandbox-etcd-state-test-20646-1791361861/raw-015.json`|
|Task3 A fences|1 / 50|0 / 0|`real-etcd-sandbox-etcd-state-test-31210-1791365851/raw-015.json`|
|Task3 B checkpoint|2 / 13|0 / 0|`real-etcd-sandbox-etcd-state-test-31514-1791365895/raw-015.json`|
|Task3 C unknown / copies / lifecycle|3 / 34|0 / 0|`real-etcd-sandbox-etcd-state-test-31847-1791365958/raw-015.json`|

- Task3 接受合计为 **6 top / 97 sub，95 native + 2 pure，FAIL0/SKIP0**；它们是三次 fresh 同配额 fixtures 的结果。实际 Go 是 Darwin/arm64 Go1.25.6、CGO1 race，服务端是 pinned Linux/arm64 etcd3.6.15；后续 Task2/A/B/C四成员 status 显示 storageVersion3.6.0 不能倒推已退役 Task1/初次失败成员恢复。
- **首次 Task3 full selector 明确失败。** 独立核对另外 25 个 raw 的 SHA/exit，raw015 exit1，raw016–018 status exit1；raw021 三个自有成员 OOMKilled=true/Exit137，foreign 未 OOM；raw022 cleanup exit0，raw023–025 exit0/empty。原 3 top/85 sub partial PASS 没计入 accepted coverage。首个 keep/delayed 清理报 ErrOutcomeUnknown/context deadline，随后 fixture binding 校验拒绝已停止成员；最终3 top/12 sub FAIL。完整 log 中15 client WARN =10 NotFound +5 EOF/Deadline/connection-refused revoke/delete 问题；metadata 68 WARN +1 ERROR，包括30 Lease-not-found apply、16基线配置、2 missing-term schema、20 peer/leader/read-index/slow-range/reachability WARN，以及 failed storage-version-update ERROR。没有将全部15/68都归成“预期噪声”。
- **host source 演变独立复核。** 解析并对现存文件重新 SHA：`task-3-final-host.json` / `task-3-final-build.json` 115匹配，后改的 `task_claim_fault_test.go`、`task_checkpoint_test.go` 两测试不匹配；它们是历史输出，build 不消费测试。`task-3-final-vet.json` / `task-3-final-diff.json` / `task-3-fault-final-host.json` 各117匹配、无 drift、exit0。前者 host suite 是6 top+2 pure PASS/95 native SKIP；后者4 top PASS/89 SKIP，没有 native 成功含义。当前候选 native 绑定的是 a5bd5ce 冻结源码，最终9f31ae5是后续文档归档；没有把历史测试哈希冒充最终测试哈希。

## Issues

### Critical (Must Fix)

无。没有发现可复活旧 claim、丢失永久意图、错误删除 owner/token、绕过原 control admission 或把 checkpoint 提升成物理权威的具体路径。

### Important (Should Fix)

无。完整本单元合同中的源码、跨任务接口与实际 protocol case 已有相应实现/证据；未发现需要在本单元增加修复波次的缺口。下面七项保留为**非阻塞 Minor 证据/fixture 观察**，不计为七个产品协议 bug，也不能自动关闭未验证属性。

### Minor (Nice to Have)：七项逐项裁定

1. **F-M1 / Task1-M1：两个 canceled-Txn WARN。** 位置：[task_read_test.go:117](/Users/dysodeng/project/go/cloud/sandbox/internal/storage/state/etcd/task_read_test.go:117)，[Task1 raw015](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/real-etcd-sandbox-etcd-state-test-12556-1791360153/raw-015.json:1)。实际 `/KV/Txn` canceled 与先取消 context 后要求 err/nil 的两次 loader 调用一致，未发现意外成功或授权泄露。**非阻塞，保留 log-hygiene Minor，不需要产品修复。** 风险是把整段 stderr/stdout 判成干净或将额外错误淹没；后续若改 harness，可精确断言这两个已知输出，不能关闭通用 warning。
2. **F-M2 / Task1-M2：18 metadata WARN +1 未证实恢复的启动 ERROR。** 位置：[Task1 raw016](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/real-etcd-sandbox-etcd-state-test-12556-1791360153/raw-016.json:1)，[classification](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/root-task-1-log-classification.json:31)。8 shared-port、4 permission、4 unsigned-token、2 schema WARN，以及 etcd-1 的 `cannot detect storage schema version: missing term information` / failed update ERROR 均真实。**非阻塞于本单元的明确 read/identity 结果，但保持未解决 Minor。** 成功读取不是该成员 schema/upgrade/snapshot 恢复证明；后续需要这些属性时，必须检查 exact binary/member 并验证其恢复，不能拿新 fixture 的 storageVersion 替代或压掉 ERROR。
3. **F-M3 / Task2-M1：3 client NotFound +9 member apply WARN。** 位置：[task_destroy_test.go:31](/Users/dysodeng/project/go/cloud/sandbox/internal/storage/state/etcd/task_destroy_test.go:31)、[Task2 raw015](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/real-etcd-sandbox-etcd-state-test-20646-1791361861/raw-015.json:1)、[raw020](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/real-etcd-sandbox-etcd-state-test-20646-1791361861/raw-020.json:1)。显式 ReleaseStage 与注册清理重复撤销原 Lease；现存 ReleaseStage 对 LeaseNotFound 按幂等清理处理。**非阻塞，保留输出分类 Minor，未发现授权错误。** 后续可精确统计预期撤销噪声，保留其他 revoke 错误；没有理由改变产品的原 Lease 清理语义。
4. **F-M4 / Task2-M2：16 基线配置 WARN +1 schema WARN。** 位置：[Task2 raw020](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/real-etcd-sandbox-etcd-state-test-20646-1791361861/raw-020.json:1)，[Task2 classification](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/root-task-2-log-classification.json:1)。完整 metadata 是26 WARN/ERROR0（加上上一项9条）。当前成员直接观察 storageVersion3.6.0，只支持该次状态，不能抹掉启动 schema warning、证明生产安全或恢复旧成员。**非阻塞的 fixture Minor。** 后续 fixture/部署 gate 应分别验证权限、port/auth配置及所需 schema 健康，当前不扩大成部署配置改动。
5. **F-M5 / Task3-M1：整批 OOM 容量问题没有被修复或解释。** 位置：[failed raw015](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/real-etcd-sandbox-etcd-state-test-30309-1791365514/raw-015.json:1)、[raw021](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/real-etcd-sandbox-etcd-state-test-30309-1791365514/raw-021.json:1)、[failed audit](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/root-task-3-failed-full-gate-audit.json:1)。所有三个主成员真实 OOM137；原 full selector 是失败，且日志含真实服务丢失/清理失败。三次 fresh batch 在原64MiB/0.5CPU/128PID配额下完成所有 current cases，足以补充当前元数据协议门禁，**不足以给持续运行或容量背书。非阻塞于本单元、仍未解决的 Minor/容量风险**，不是已定位产品算法无界增长也不是已排除该可能性。Ruling8 允许分批的代价是额外三组四成员启动与重复历史 partial cases，不能称性能优化。下一次持续/生产容量声明前应独立调查内存归属与增长、服务端配额和负载；不得仅提高配额或删掉失败记录。
6. **F-M6 / Task3-M2：11 client +33 member apply NotFound WARN。** 位置：[task_claim.go:291](/Users/dysodeng/project/go/cloud/sandbox/internal/storage/state/etcd/task_claim.go:291)、[Task3 classification](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/root-task-3-log-classification.json:1473)，B/C 的 raw015/raw020。B为4/12、C为7/21；都属于 absent original Lease 的 fault/expiry/idempotent cleanup，accepted metadata ERROR0。**非阻塞输出分类 Minor。** 这11条可以解释，不能推及失败 full run 的另外5条网络/超时警告；后续 harness 可单独断言预期数量/类别而保留未知噪声。
7. **F-M7 / Task3-M3：三个成功 batch 仍有48配置基线 WARN。** 位置：[Task3 classification](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/root-task-3-log-classification.json:1286)、[A raw020](/Users/dysodeng/project/go/cloud/sandbox/.superpowers/sdd/2026-10-07-cleanup-task-authority/real-etcd-sandbox-etcd-state-test-31210-1791365851/raw-020.json:1)、B/C raw020。每批8 shared-port、4 `/etcd-data`0755 permission、4 unsigned simple-token。**非阻塞 fixture Minor，不能声称 warning-clean 或 production security。** 保留精确 census；后续单独清理 fixture hygiene 或在部署 gate 验证生产配置，不在本协议单元悄然修改运行面。

## Recommendations

- 可接受此元数据单元，不要求重跑已匹配源码的测试或新增一轮产品修复；将上述七项逐项保留到 Root disposition，特别是 F-M2/F-M5 不应因本次 Yes 而关闭。
- 后续物理 cleanup 必须消费 sealed TaskClaim 并建立独立 authenticated target/remote evidence。destroying 仅关 etcd admission，checkpoint/digest/历史 ClaimID 不提供安全释放证明；旧 Stage 仍可在原 server guards 活着时提交元数据，不能用它驱动物理补偿。
- 生产或 N-scale gate 应单独解决持续容量、永久 task/intent/link/checkpoint 增长、背压/安全 GC 和部署安全。当前没有新增按 N 常驻 actor 是源码属性，不是10k/100k/1M实测。

## Declined to judge（逐项供 Root 裁定，56 项）

1. Task-authenticated `CloseData` 的实际目标 gate 持久关闭：当前代码没有 runtime 调用/目标持久证据；这是下一物理协议的强制工作。
2. Task-authenticated `CloseAll` 的实际目标 gate 持久关闭：当前 metadata claim 不能替代完整目标关闭协议，当前测试也不执行它。
3. etcd operation prefix 的最终排空：destroy 原子关闭新准入但保留现有 token；当前没有 drain 完成实现或接受标准。
4. 目标端 accepted/execution 的排空：不是 etcd token 为空就能证明，当前没有测目标状态。
5. final sync 的外部完成：checkpoint 只允许保守状态，不能据此判定后端数据同步完成。
6. final flush 的外部完成：当前没有 durable flush evidence，未从 metadata receipt 推导。
7. exact runtime termination / runtimeGone 身份：当前保留 runtime 引用，未执行或验证精确终止。
8. remote settled 的远端结算：没有 End/remote settlement 实现，普通本地 Close/receipt 不能替代。
9. remote fenced 的隔离证明：当前原 task Lease fencing 只保护元数据写，不能证明远端停止。
10. 最终条件 owner/fence/control/index 安全释放：本单元明确不删除这些点，尚需上述物理证据链。
11. 最终 mount-policy / mount 资源清理：本单元没有该副作用或其条件安全证明。
12. publishing 失败恢复任务：active-only destroy 不覆盖未知外部创建效果，不能据 task API 接管。
13. exclusive 生命周期恢复：未接入本单元的 active→destroying 条件，属于后续生命周期工作。
14. cleanup_pending 生命周期恢复：未在当前 API 实现，不能将保守 checkpoint 当已实现恢复。
15. TTL due 触发与 due index：当前支持手动 destroy，不含时间调度/发现准入。
16. dirty index 与共享调度/背压：没有调度器接线；当前只验证实际领取任务的元数据能力。
17. 永久 task/intent/link/checkpoint 的安全 GC：当前保留历史是有意安全边界，未建立年龄删除或未决回收证明。
18. pool/upload 等下游资源最终回收：当前无相关写入/副作用，不从元数据 ownership 推导完成。
19. 生产 Manager/API/backend/images 接线：范围内未修改生产入口选择，当前结果不等于已启用新路径。
20. 老版本排空与 Redis 完全移除：master migration 仍进行中，本单元不能证明不再有旧读写者。
21. 外集群拒绝的完整端到端路径：当前 identity/restore 场景和严格响应已检查，但仅存在第四 foreign member 不等于执行了该全路径测试。
22. 生产 leader failover 恢复：当前 fault 注入与全失效 OOM 不等于系统性的切主验证。
23. 网络分区后的系统恢复：当前没有隔离、愈合、重入的完整场景证明。
24. 生产 TLS：isolated HTTP fixture 未提供加密连接配置及其实际验收。
25. 生产 RBAC：当前 fixture 没有证明生产角色配置和访问隔离。
26. 恶意网络环境：响应校验的局部断言不等于完整威胁模型下认证与服务可用性证明。
27. 生产数据目录权限安全：实际0755告警尚在，当前不能据隔离 fixture 证明生产权限正确。
28. 具有原始 etcd 写权限的 Byzantine 管理者：strict records 与原 guard 比较不提供抵抗可信管理面伪造所有历史的保证；当前不引入这种保证。
29. exact server source-to-binary 对应：pinned digest/版本是身份线索，未证明完整源码构建 provenance；caller源码路径也不是证明。
30. 退役 Task1 成员的 storage-version 恢复：直接健康/read 成功不足，当前没有该成员后续恢复证据（F-M2）。
31. 首次 Task3 OOM 成员的 storage-version 恢复：后来的 fresh 成员不是原成员，原 schema ERROR 的恢复未被验证。
32. etcd upgrade 正确性：当前 restore identity 拒绝测试不执行版本升级。
33. etcd downgrade 正确性：当前没有降级和数据可读性证明。
34. snapshot 正确性：当前没有快照生成和内容校验。
35. backup 正确性：当前没有备份完整性或可恢复性验收。
36. disaster restore 正确性：restore identity fence 是拒绝陈旧权威的协议，不能替代完整灾难恢复演练。
37. 未中断 full selector 成功：该 actual selector 已失败，三次 fresh batch 不认证这一属性（F-M5）。
38. 长期内存稳定：没有连续运行的内存曲线或稳定性证据，fresh fixture 会重置状态。
39. OOM 根因与修复：三成员OOM137已确认，但当前既未定位产品/服务端/负载贡献，也未证明修复。
40. N=10k/100k/1M 的实际资源占用：源码未增加 per-idle actor 不等于实测这些规模。
41. 规模化 QPS：有限 protocol selectors 不是吞吐 benchmark。
42. 规模化 p99：当前没有明确负载下的延迟分布验收。
43. 规模化恢复 SLO：未测试大规模失效/恢复时间和积压消退。
44. Linux 应用 race：实际 Go race 应用在 Darwin，Linux etcd 服务端不是 Linux 应用证明。
45. PID1 行为：当前没有 launcher/PID1新执行，已有平台门禁不在本次重放。
46. FUSE 行为：本单元无挂载或FUSE执行，不能以metadata结果证明文件系统行为。
47. 继承 Stage 全部 malformed/unknown/cancel 历史：只检查当前使用接口与新增真实场景，没有重审/重放全部基础原语历史；当前没有修改其实现。
48. 原 Stage guard ModRevision 成功 CAS：Ruling7明确保留value/Lease/Create的原协议，新task guard四字段测试不认证这项原Stage加固。
49. 原 Stage local monotonic deadline：Ruling7明确继承服务器Lease协议，不能把TaskClaim的新deadline说成原Stage的新保证。
50. 继承 codec 的全部旧调用场景：检查了本单元strict schema/atomic assignment接口与新增用例，未重审所有旧caller。
51. 继承 mutation budget 的全部旧操作形状：当前43/59和预检路径已判断，未重审所有旧预算组合。
52. backend identity provisioning 全流程：现有read fence接口已检查，未在本次认证整个初始化/管理过程。
53. restore administration 全流程：当前检测restore fence变化，未验证管理员恢复程序的所有行为。
54. 继承 request-budget machinery 全部边界：当前使用其bounded context接口，未重审整个共享预算实现。
55. 整个 migration branch 的最终仓库回归：本次只审新unit并消费现有scoped输出，未重跑全仓suite；Root仍需完成计划中的最终集成检查。
56. 发布、合并与部署后的实际运行：本次没有push/merge/deploy，metadata gate通过不能代替最终交付与生产验收。

其他已考虑并在本范围**作出判断**的边界不列作 declined：取消不等于 queued Stage 远端撤回是准确协议语义；未知 Grant 未知 ID 不能安全 Revoke，当前不返回 cap并依赖服务器 expiry；已知原 ID 只清理其原 Lease；普通阻塞 RPC 使用既有 request context，未宣称可抢占不合作 provider。这里没有借“范围外”放过具体已发现的授权缺口。

## Assessment

**Ready to merge? Yes — 仅对本 cleanup-task-authority 元数据单元。**

**Counts: Critical 0 / Important 0 / Minor 7；Declined-to-judge 56。**

严格永久意图、同 control revision 的 destroy 准入排序、原 Lease 不可复活能力和保守 checkpoint CAS 在完整范围及跨任务接口上符合合同；真实 etcd 分批证据支持这些协议行为。七项 fixture/证据观察保持非阻塞但可见，整批 OOM、物理 cleanup、生产接线、Redis 移除和 N 规模验收均不在本次完成结论之内。
