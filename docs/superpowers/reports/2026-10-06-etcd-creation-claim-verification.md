# etcd 创建 intent claim 验证与最终审查

日期：2026-10-06。分支：`codex/etcd-state-management`。承接永久领域模型 `9cdcefa`、原子申请 `0bbfead`，本批单独提交创建管理权。

## 交付与重要界限

`LoadCreationIntent` 线性读取永久 intent。`ClaimCreation` 固定点读并核验六种永久记录的归属、generation、digest、restore 及 phase；仅 pending/publishing 的合法未绑定图可领取。新 UUID 的 claim 和 guard 同事务绑定原始 Lease、value 与 CreateRevision；永久 owner/control/request/intent/fence/placement 不修改、不附 Lease。

能力绑定原 Backend，reference 只用于归因；private comparisons 深拷贝六个永久 key 的 revision/value/Lease0 及两个 leased key 的 value/Lease/CreateRevision，共24条。失租后重建相同 key/value 仍不能让旧能力提交。24条比较加14条 Stage 预留，额外26项允许、27项在 Grant 前拒绝。

续租使用发送前单调时间加服务端 TTL 计算保守截止，检查旧截止与 caller cancellation；RPC unknown 或验证失败后同一本地能力不可恢复。mutex 串行续租，锁等待计入 caller context。释放只撤销原 Lease，不能删除永久 owner 或撤销新 claimant 的 Lease。

本地 lost 不撤回已经复制到 Stage 的比较条件，也不能证明在途事务 aborted。旧 Stage 仍由真实 guard/claim/control CAS、原 Lease 已确认撤销或过期，以及 exact receipt 仲裁决定。claim 只管理元数据，不授权外部创建、重复挂载或 runtime 数据操作。

## TDD 与自审闭环

真实三成员 assertion RED→GREEN 覆盖永久 lookup、原 Lease 原子领取、renew/release；另外复现并修正旧截止越界但新候选截止仍未来、正回复后 context 取消、已发布图误报损坏、领取正回复后取消却返回能力，以及 cleanup/runtime/phase 矛盾误报普通冲突。

合法已发布或待清理图分类为 `ErrConflict`；字段关系或 phase/runtime 矛盾为 `ErrCorruptRecord`，两者均不 Grant。此分类避免把不能再领取 creation claim 的正常生命周期记录误报损坏。

30个顶层真实 claim 用例包含16 contenders、原 Lease与永久图不变、新 claimant更大 epoch、old Stage/control改变/guard和claim同值重建、跨Backend、wrong restore/cluster/key/envelope、损坏或带 Lease 的永久记录、输入和TTL边界、实际提交丢回复、迟到回复、迟到 KeepAlive、unknown续租不可复活、并发续租和context取消。服务端可提高短TTL，截止测试依赖实际返回TTL而非假定请求值。

## 独立审查与实际验证

- Spec review PASS：无P0/P1/P2；独立真实三成员 `-race -run '^TestCreationClaim'` exit0，9.896s，30个顶层用例全部执行，无 skip/race。
- Quality与当前全分支跨模块最终review PASS：新claim与 Stage/AcquireIntent/readDomain/restore 的交互未发现P0/P1/P2；独立组合race exit0，12.791s，无skip/FAIL/race。之前 foundation/domain 最终审查记录见同目录 `2026-10-06-etcd-foundation-domain-final-review.md`。
- 父线程 fresh `bash scripts/test-etcd-state.sh -v` exit0：真实三成员server3.6.15，race19.040s，无skip/FAIL/DATA RACE；包含本批claim及原有NOSPACE、leader切换、异集群、未知结果与迟到事务测试。script完成后仅清理自己project的资源。
- Implementer fresh定向真实race exit0，9.884s；`go test ./...`、etcd包`go vet`、sandbox构建均exit0；gofmt与diff检查通过。全仓测试中依赖其他外部环境的用例沿用原有机制，etcd完整fixture没有跳过。
- `golangci-lint`完整etcd包仍有14项前批baseline（client测试未检查Close、TLS deprecated API及旧字符判断样式），exit1；六个新增claim文件没有lint issue。没有将完整lint误报为通过，也未在本批修改旧基础代码。

## 下一步

下一单元实现 durable runtime dispatch/effect journal，再以 target gate/mount证据和 exact UID/BootID完成Publish、runtime索引、request completed及receipt组合事务。后续继续operation、pool、scheduler/GC、runtime launcher与Redis移除。

本批没有切换生产分配器或删除Redis；没有按存活沙盒N创建常驻claim或goroutine。实际存量容量、历史记录、数据库放大与性能改善幅度仍待完整路径的容量测试。
