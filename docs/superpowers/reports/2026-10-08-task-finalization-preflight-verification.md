# Task finalization preflight verification

日期：2026-10-08。Spec：[design](../specs/2026-10-08-task-finalization-preflight-design.md)，Plan：[implementation](../plans/2026-10-08-task-finalization-preflight.md)。原 user-quiescence 单元已验收，不重放；本轮仅接续新入口。

## 交付与冻结

源码提交：8b94418b6cd70e0501f24a22d2d4cb19f9f2f97f，BASE53b8f9afb9e27eeb21751ae98b5af7dd12c6a9a0。五个 source/test 路径，共494新增/2删除；实现者 /root/finalization_preflight_implementation。43f3511 单独记录三个实施裁决和 [driver assessment](2026-10-08-final-sync-driver-assessment.md)，没有新增源码或物理测试。

`Backend.RevalidateTaskQuiescence` 校验原对象/Backend/claim/reference/保存证据与原 deadline；复用原 `PrepareAfterDualDrain` 算法，比较保存的不可变 pins 和 terminal digest，执行两次新认证 query、中间一次新的完整 operations prefix limit1 native Range，最后复核 pins/fences/live。保持53compare、55literal native nodes。返回 error，成功不产生替代 prerequisite 或其他授权对象，不签票、不改变元数据、不申请租约、不发送效果、不释放 owner。

三个裁决均已写入规格/计划并通知实现者：入口已过期的缓存前提不毒化续租仍有效的 claim，但中途超时沿用原保守 lost 策略；操作员重建 original-operation 产生新 birth，按既有 decoder 拒绝为 ErrCorruptRecord，不伪造旧 revision；现有 mTLS 测试目标只增加 atomic response override，保留默认行为，避免复制 TLS server loop。

## 实际验证

使用已有 `scripts/test-etcd-state.sh` 创建五个分别命名的临时 fixture；每个含真实三成员 etcd 与独立 foreign member，固定server3.6.15镜像，复用本地 digest；go.mod 固定 api/client/client-pkg v3.6.14，client WARN header 也记录该版本。Root 新计划曾误写客户端3.6.15，经审查指出并读取 go.mod22/23/140修正；没有升级依赖、改变源码或重跑测试，验收只归于这个实际版本组合。Host Go1.25.6、GOPROXY=off，没有安装或拉取。三成员在同一 Docker host，不声称覆盖独立故障域。每个 testcase 验证 endpoint/member/cluster 身份，使用 genuine mTLS 的协议测试目标；目标不是物理 PID1。

| 验证 | 实际结果 | 证据 |
| --- | --- | --- |
| 有意义 RED | exit1；临时可编译全拒绝实现导致合法前提断言失败；真实三成员、元数据和签名前提准备已成功，无缺环境 SKIP | red.log，56,665bytes，SHAd267339597713936e0325b2c5163e50e690f034272496ecb0a96fe0fe2c4eb63 |
| 初次 GREEN 纠正 | exit1，仅新 operation 的错误码预期不符；旧 birth 约束正确拒绝，新测试按裁决纠正，生产 decoder 未改 | focused-green.log，70,035bytes，SHA30b07cc83afb72a504882605b32280ba674b7b1d33326aef5eb4aaa09f9ddd6e |
| 最终 focused GREEN | exit0，9top-level/46nested-inclusive PASS，0FAIL/0SKIP，17.602s；含新增入口和原 conjunction/final-barrier 回归 | final-focused-green.log，35,816bytes，SHA02bda6820f9a2163f8aefa0ac3025ba271c6e71f45e0f93234d0a8d151be71d5 |
| 全受影响 etcd 包 race 一次 | exit0，345top-level/2679nested-inclusive PASS，0FAIL、5明确 gated SKIP，211.809s | full-native-race.log，1,098,181bytes/10,059lines，SHA60ad1d47af81e91a28471352b05d043428ea6d542faed4e411da2775de4710da |
| vet | exit0，无诊断 | vet.log，216bytes，SHA691dd7ad7cdd92548a7194466a332059ae5194a3d5f6327ff405be3fd0a59960 |
| Linux arm64 CGO0 | compile exit0，未执行 Linux 测试 | linux-arm64-compile.log，380bytes，SHA1fac760e49d1c16b4583cef82ac9fc28f9e200a1a8d6fe99e4fee01cfa9a7002；test ELF32,598,994bytes，SHA94663160bccb7efa663ccf8490b55355697fcda4d24bf3d96ea0eacdc13d4aae |

完整原始命令、输出、实际退出码、cleanup inventories、源码提交和完整作者报告留在本单元 `.superpowers/sdd/2026-10-08-task-finalization-preflight/`，不 force-add ignored OWN。Root 独立读取生产共享算法与全部新增测试、完整作者报告，解析所有最终 raw 日志的结果/JSON诊断并核对 bytes/hash；没有重跑套件。

成功用例观察到两次新 authenticated terminal query 与一个新 linearizable 全前缀 limit1 Range；两分支范围与预算被断言。保存对象及 native 值/create/mod/lease 均未变化，signer/certificate 调用与 Grant/KeepAlive/Revoke 不增加，效果发送为0。新查询允许 native revision 未增加。

故障用例覆盖 same-bytes receipt/reservation revision 在 query 前、Range 前后变化；原 claim revoke、取消和保存 deadline 中途失效；失联/accepted-only/未知/同证书下不同 terminal 在 Range 前后变化。保存 zero 后操作员重建真实原 wire/lease 的 entry，被新 Range 观察为Count1/entry1，再因晚于 destroy birth 拒绝；它不是生产可合法新入场的 operation。过期保存 deadline 经真实原 lease 续租后仍拒绝，入口拒绝无 KV/Dial、不毒化 claim；中途过期保留原 lost 策略。

## SKIP、失败与诊断

五个完整包 SKIP 是 `TestTaskDualDrainFixtureInitialize`、`TestTaskDualDrainFixtureRelay`、`TestTaskDualDrainNativeActive`、`TestTaskDualDrainNativeLateArrival`、`TestTaskDualDrainNativeUnknown`；需要已验收旧单元的独立物理 fixture，此轮刻意不启用。新增三成员测试实际执行，SKIP 未计作 PASS。

最终 full race 的225条 JSON WARN 全部分类：214个重复/已经撤销 Lease 的 NotFound、6个显式取消、3个显式 deadline、2个原 NOSPACE fault suite 的 ResourceExhausted。没有 error-level、FAIL、race report、panic 或未解析 JSON。最终 focused 为27NotFound/2Canceled/1DeadlineExceeded，均属指定故障或 fixture cleanup。输出不宣称 pristine。

RED/初次失败的脚本输出包含服务器日志：共享 HTTP/gRPC port、目录0755与 unsigned simple-token 是原临时 insecure-loopback fixture 的启动诊断；已失效 Lease 的 server apply 错误是重复 cleanup 的警告。RED 另有2个 schema-version/missing-term WARN 和1个 failed-to-update-storage-version ERROR，发生于健康/用例准备之前；随后四服务健康、真实三成员准备成功。此轮未诊断/修复其底层启动条件，保留为部署/恢复阶段的未验证项，不用后续测试成功覆盖该错误。作者报告保留每类逐项计数与 raw context。self-review 的三个 no-index diff exit1 是新文件相对 /dev/null 存在差异，不是测试失败。

五个项目按各自精确 compose project label 查询 containers/networks/volumes，15次 inventory 均exit0且为空；脚本 teardown 均完成。cleanup 证明测试资源已清理，不能当业务终止/释放回执。源代码单独五路径提交，index 在前后均空；预先存在的无关 Sentinel 文档未读取、改动、暂存或回退。

## 独立审查与剩余范围

独立审查者 `/root/finalization_preflight_review` 已完成唯一单任务/单元 gate：Spec compliant，Task quality Approved，Critical0/Important0/Minor2。审查范围为 BASE53b8f9a 到源码8b94418的完整五路径差异；Root 已读取完整审查报告、逐项裁决如下，Task1 按此限定范围 accepted。源码冻结后未改动，审查没有重跑测试或旧物理单元；完整审查原件保留在本单元 OWN/task-1-review.md。

| 项目 | Root 裁决及后续义务 |
| --- | --- |
| M1：预期故障/cleanup WARN | 不阻塞本单元；保留全部原始诊断与225条分类。后续 fixture 维护须区分预期和意外诊断，不静默丢弃原始输出。 |
| M2：RED bootstrap schema/storage-version 诊断 | 不阻塞新增只读入口；原因尚未诊断，保留为部署/恢复阶段待处理项。后续健康和 GREEN 不证明启动错误已修复。 |
| CV1：物理排空、部署目标 | 本轮 mTLS 目标只证明认证协议归属，不能证明 PID1 排空或部署后的目标替换。沿用上一物理 gate 的限定验收，五个 gated SKIP 不计 PASS。 |
| CV2：真实生产者及释放链 | 原子 intent、每次发送 fences、远端结算、精确终止、owner 释放及 FUSE 管理回执仍是主迁移未完成项。本 API 成功不能代替这些证据。 |
| CV3：容量、吞吐、HA、生产部署 | 未测量，专门验收 gate 保持 pending；不推断性能提升、已有大量 sandbox 的容量上限或生产可用性。 |
| CV4：RED schema 错误原因/解决 | 未验证，与 M2 一并保留到部署/恢复阶段；不能把本单元测试作为 schema/恢复健康验收。 |
| CV5：依赖版本身份 | Root 已读取 go.mod22/23/140确认 api/client/client-pkg3.6.14及fixture server3.6.15；纠正本人新计划的版本笔误。文档身份问题已解决，无依赖/源码变更，不重跑已有验证。 |
| D1：没有重复运行验证 | 接受有意避免重放；作者和 Root 已核对本轮完整保留证据，审查者独立解析原始输出。不是新增测试缺失，也不扩大原证据范围。 |
| D2：没有重放前序或全迁移审查 | 接受限定 gate；本地唯一任务的所有源码均已审查。整个迁移的最终审查仍待后续实现完成。 |
| D3：无关文件及只读审查边界 | 接受；审查者只写 OWN 审查原件，没有读取 Sentinel 或改动源码/index/HEAD。Root 验收只暂存明确的两份本单元文档。 |

本单元是只读入口，没有生产 final-sync 消费者、远端 attempt ledger、FUSE 新 task 回执、精确终止或 owner 安全释放。后续生产者仍需在自己的原子 intent 与每次外部发送边界验证当前 claim/permanent fences；这里的 error-only 成功不能作为远端结算或释放证据。保留已有 user-quiescence 验收范围；这次没有新的实际 PID1 排空、Linux native race、故障域 HA、N/吞吐容量、生产部署恢复或 Redis 移除验收。
