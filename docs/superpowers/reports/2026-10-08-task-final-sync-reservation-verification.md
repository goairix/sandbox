# Task final-sync reservation verification

日期：2026-10-08；[设计](../specs/2026-10-08-task-final-sync-reservation-design.md)、[计划](../plans/2026-10-08-task-final-sync-reservation.md)。本轮从已验收 preflight 接续，没有重放旧物理 gate。

## 源码与实际行为

源码3b5a60b54e8f1a1d349e728abfe47ccb3c02b5d5，BASEa8ab6a430541af10c4d21825cd1fbac06acd3e8a。六个明确 source/test 路径1006新增；原 TaskClaim 只新增一个私有 draft 字段，原 fixture/依赖没有修改。

`ReserveTaskFinalSync` 首次执行新的双排空观察，然后在完整53项 fences之上以absence compare创建每task唯一永久 `final-sync-attempt`。writeAttempted在发送前置位，后续相同原 claim/plan/prepared只读仲裁，不再Put；absent保持unknown。原已确认native revision被pin，删除/相同bytes重建/改变immutable envelope拒绝复用。新claim可以首次登记空slot并合法引用旧quiescence，但已有slot禁止新producer替换。

写事务58 literal nodes，仲裁读59 nodes，均以taskCheckWire检查请求和保守响应预算。native捕获验证实际53fences+absence、固定期限、2次新认证query/1次新全operations prefix Range；无额外Grant/KeepAlive/Revoke、签名或效果调用。full日志一个示例：write20804bytes、read18822bytes、保守read response2763bytes、wire1989bytes；示例不是容量或性能结论。

result与Load都是metadata诊断，不返回sender。原claim失效/保存期限到期时可以有committed诊断和error，并没有恢复执行权限。`none`只是计划mode。未知首次写没有可信native birth时，只能读当前UUID/wire归属；不能证明最初物理birth，也不能检测第一次可信revision之前发生的同bytes ABA。已确认revision的后续ABA实际测试拒绝。

## 验证与证据

Host为Go1.25.6 darwin/arm64，etcd api/client/client-pkg3.6.14，真实三成员server3.6.15加foreign member；同一Docker host，不证明故障域HA。后续命令固定Go PATH和GOPROXY=off。所需最终验证的完整命令参数、UTC时间、实际exit、原始stdout/stderr及manifest保留在本单元 `.superpowers/sdd/2026-10-08-task-final-sync-reservation/task-1-evidence/`，不force-add ignored OWN；早期直接执行脚本的准备错误和补充host检查缺少原始环境/时间记录，补录说明不能恢复这些缺失，也不替代最终native验证。

| 验证 | 实际结果与证据 |
| --- | --- |
| meaningful RED | `bash scripts/test-etcd-state.sh -v -run '^TestTaskFinalSync'`，exit1；真正三成员准备成功后合法reservation断言因all-reject stub失败。red-positive.raw.log55884bytes，SHAae97066fb45b285151b389873c0d0ba943480df3dd4947e8c015ec087e23e32f。 |
| final focused GREEN | 同一selector，exit0，12top-level/71nested-inclusive PASS，0FAIL/0SKIP，23.739s。green-fixed.raw.log43468bytes，SHA0cb9ad48df44ede348b2dddc9af1261259675b8e295af65ff9053857a5873984。 |
| full affected etcd race | `bash scripts/test-etcd-state.sh -v`，脚本使用-race -count=1，exit0，357top-level/2750nested-inclusive PASS，0FAIL/5gatedSKIP，232.017s。full-race.raw.log1137369bytes，SHAc9b4c8129f380073fde0de45d498be6986317d9bf5eef741ddfc8483fdd9ec35。 |
| vet | `go vet ./internal/storage/state/etcd`，exit0/空输出。 |
| Linux arm64 compile | `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c`，exit0；未执行Linux测试。ELF32695568bytes，SHA0546cbc14373e17537b11bdc5ccec59f8ced1acd39f2ef10001f05693ec27c43。 |

Root完整读取作者报告与新source/tests，独立解析全部保留raw结果/JSON诊断，46个manifest文件的实际bytes/hash全部匹配，来源保持冻结，index为空。没有再跑测试套件。

## 失败、跳过、诊断和清理

准备期直接执行非executable脚本的PermissionError、scaffold不存在Op.Lease的编译错误、key stub提前拒绝的RED均保留，均不计meaningful RED。初次GREEN后补强的历史linkage测试曾改WorkspaceHash导致partition无效，正确被encoder拒绝；测试改为合法ExpiresAt变化后再跑focused/full，未放宽产品validator。初次绿色记录不作为最终源码验收。

最终focused有31条LeaseRevoke NotFound WARN；full有258条WARN：247NotFound、6Canceled（1LeaseRevoke+5Txn）、3DeadlineExceeded、2原NOSPACE用例ResourceExhausted。全部原文与故障/cleanup上下文保留；没有error/fatal、FAIL或race failure，不声称pristine。失败run的server日志保留共享HTTP/gRPC端口、0755目录、unsigned simple token和已失效lease apply诊断。这次没有出现前序RED的schema/storage-version错误，但前序未查明的问题仍属于部署/恢复待处理项，不能据此宣称已修复。

五个SKIP仍为TaskDualDrainFixtureInitialize、FixtureRelay、NativeActive、NativeLateArrival、NativeUnknown，要求独立物理fixture；本轮刻意不启用，不计PASS。新的final-sync native用例实际执行，响应丢失通过客户端wrapper截断真实commit回复；这不是部署故障/网络分区/物理PID1新验收。

7个各自唯一的owned compose project按精确label查询containers/volumes/networks，全部exit0/空清单；Root读取完整cleanup记录。它只证明测试资源清理，不是业务终止或owner释放。源码提交在OWN/git-commit.lock下进行空index检查、精确六路径stage、cached check和提交；无关Sentinel未读取/修改/stage/revert。

流程偏差：作者首次RED格式化直接用gofmt -w写了五个新文件，违反apply_patch-only要求；完整实际命令在作者报告，后续内容/格式化/报告均以apply_patch处理。Root明确记录为非阻塞流程项，不声称全程合规，不因格式化来源重跑已验证源码。用户的编辑偏好继续适用。

## 审查与剩余范围

唯一独立任务/单元审查 `/root/final_sync_reservation_review` 已完成：Spec compliance PASS、Task quality PASS with Minor，Critical0/Important0/Minor1；完整报告保留在本OWN/task-1-review.md。审查完整六路径43,397byte差异，独立读取所有raw证据并核对46manifest，不重跑测试或旧单元。Root读取完整报告并逐项裁决，本Task1按以下范围accepted，无源码修复或重复review要求。

| 审查项 | Root 裁决 |
| --- | --- |
| M1：首次gofmt直接写文件 | 非阻塞流程偏差，保留实际命令及后续apply_patch纠正方式；不声称全程合规。最终源码功能、格式和验证身份未因此失效，不重跑语义验证。 |
| CV1：首次未知birth前ABA | 无法证明，保留限制；当前UUID/wire归属是metadata诊断，不能作最初birth或sender证据。仅known revision后的ABA拒绝已验证。 |
| CV2：cold/restart的terminal真实性 | Load只验证结构历史，没有原terminal wire独立认证，也不重建原prepared；不能把ReceiptDigest作为sender或远端结算证明。 |
| CV3：absent、none与no-effect | absent仍unknown；测试截断/不发native写只覆盖本地保守分支。none仍为计划声明，没有no-send/no-effect证据交付。 |
| CV4：物理source/worker/Boot | 本轮未建立，保留上一物理gate的原范围，5SKIP不算新证据；可信source及精确worker身份待实现。 |
| CV5：provider/HTTP/FUSE/request生命周期 | 实际发送、逐请求永久intent、provider确认、FUSE task回执、终止和释放均未完成，不由metadata committed推导。 |
| CV6：已有大量sandbox与生产行为 | 容量、吞吐、生产接线/调度/拓扑/恢复/Redis移除仍pending；本单元没有性能提升或生产ready结论。 |
| CV7：环境可移植性与早期补录 | Linux arm64仅compile；没有Linux/部署执行。早期缺失环境/时间不能补录成原始记录；最终所需native验证的原始记录已验收。 |
| CV8：临时RED源码快照 | raw正向失败、可执行测试与真实成员身份已核对，但临时all-reject stub未独立保存。接受本轮行为RED，不声称从最终diff重建该临时实现，不重放已完成测试。 |
| CV9：实时cleanup/checkout | Reviewer按只读边界只检查保留记录。Root另行确认当前source仍冻结/index空；Docker只按本轮已保留的精确owned项目空inventory验收，不声称进行了新的live查询。 |
| Declined checks | 不重跑Git/测试/native/Docker，不审查已验收前序、无关Sentinel或整个迁移；限定gate覆盖本地唯一任务全部源码，迁移最终review仍pending。 |

仍缺可信runtime只读文件源、OS worker/Boot存活证明、逐请求发送ledger、具体provider结算、FUSE task receipt、精确终止及owner安全释放。下一步文件源调查见 [source assessment](2026-10-08-final-sync-source-assessment.md)，它只是源码调查。生产接线、分区调度、collector、大量已有sandbox的容量、部署恢复、Redis移除及整个迁移最终review均未完成。
