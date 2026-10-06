# Runtime binding / publication 验证与容量记录

验证 source HEAD：`a4412e05e52a15741fd61a238c47c775de6dcdac`，分支 `codex/etcd-state-management`，分支基点 `b99b823d69de6baa860eb95d47bd269599b03f42`。本批是已批准完整替代中的增量交付，未切换服务端后端；launcher 的实际 gate/mount、配置接入与 Redis 删除仍待后续阶段。

## 可独立审查的提交

| 提交 | 单元 |
| --- | --- |
| `71fc77b` | 根签 runtime 委托证书和委托签 ready proof，严格 codec/time/context |
| `384704f` | 永久 binding/certificate/UID index/mount-intent 与 coherent point loader |
| `9095a93` | constructor 固定 roots/authority/controlled clock |
| `8625256` | exact runtime 固定和一次性 mount intent 消费 |
| `1d3d3ab` | 永久 publication journal/proof、历史恢复与共享 strict receipt |
| `780586f` | signed Publish 六写+receipt 同事务，原24比较保留，总预算64 |
| `6ed468d` | Task4 review I1：Begin初始化失败时清理错误单独诊断 |
| `a4412e0` | whole-branch I1：Acquire/Dispatch同类路径单独诊断及组合回归 |

每项在真实测试、自审后立即提交；独立review修复另提交，未累积成一个最终大提交。

## 独立任务 review

Task1 / Task2 / Task3 spec compliant、quality Approved。Task2 Minor M1（错误receipt重写测试先撞到immutable envelope，没有进入receipt内容验证）由Task3补充真实同首次revision corrupt receipt案例和mutation RED覆盖。

Task4首次review提出Important I1：guard-init回复丢失同时Revoke失败，shared Begin将清理失败join到primary error，result.GuardCleanupError为nil。修复用私有error transport保留公开Stage的errors.Is及error文本，高层单独提取cleanup；真实Publish/Bind/Consume/Stage组合故障RED→GREEN。Scoped复审I1 ADDRESSED，无新增Critical/Important。Whole-branch随后发现Acquire/Dispatch两个sibling也有同类遗漏；`a4412e0`补齐，真实组合回归RED→GREEN、定向race七项PASS2.243s；最终scoped复审ADDRESSED，无新增breakage。已交付增量没有遗留Critical/Important；通用builder Grant边界深拷贝测试的既有Minor仍明确延期，源码未发现alias漏洞。完整whole-branch报告和最终收敛见同目录`2026-10-06-etcd-runtime-publication-final-review.md`。

密码学测试认证的是合成断言，不能证明真实mount/gate状态。公开metadata loaders不构造能力；缺少runtime/clock/trust或证据错误在任何Grant前拒绝。历史metadata读不刷新证据期限、不允许重建旧claim。

## Controller 最终验证

全部在source `a4412e0`运行；较早 `780586f` / `6ed468d` 的通过记录不代替最终修复后的检查。

| 命令/范围 | 结果 |
| --- | --- |
| `bash scripts/test-etcd-state.sh -v` | fresh owned三成员+foreign fixture，189 top-level PASS、0SKIP、0FAIL；脚本启用race，33.982s |
| `TEST_ETCD_ENDPOINTS=<owned development endpoints> go test ./...` | 全仓PASS；etcd包26.634s；manual project全局故障开关关闭 |
| `go vet ./...` | exit0 |
| `go build ./...` | exit0 |
| `git diff --check` | exit0 |
| implementer最终shared Stage/preparation/publication定向race | PASS12.725s |
| protocol独立unit/race | PASS；该protocol源码在后续单元未修改 |

fresh项目 `sandbox-etcd-state-test-95801-1791292607`，etcd server3.6.15。三独立member `59419ceb557bda34`、`3ec0c43bdac94112`、`fad7207c29fdfa09`，测试cluster `f45703ebe16eae30`。执行真实leader pause/unpause与NOSPACE注入；foreign member拒绝测试包含在同次suite。脚本trap删除其containers/network/volumes，未向另一个手工development project注入全局故障。

日志存在8条预期etcd client WARN：4条失效/已撤销Lease的NotFound、2条取消Txn、2条NOSPACE。没有宣称输出无warning，也未为美化测试而关闭production日志。未运行全仓race或宣称golangci全仓clean；此前记录的无关旧lint问题不在本批修复范围。

## 实际保留值容量样本

最终fresh fixture `TestRuntimePublicationCapacitySample`读取真实提交KV，按类别验证20个永久非meta值，没有将同一UID index重复计数。只在测试隔离namespace进行一次prefix统计；生产仍固定point读取。申请snapshot payload固定56B，完整snapshot record270B，与下表合成dispatch input大小独立。

| raw serialized value bytes | 合成16B input | 合成4096B input | 合成8192B input |
| --- | ---: | ---: | ---: |
| dispatch3 | 1941 | 6021 | 10117 |
| binding4，含UID index一次 | 3183 | 3182 | 3183 |
| mount2 | 1275 | 1275 | 1275 |
| publication3 | 3594 | 3593 | 3594 |
| 新journal/index/receipt12值小计 | 9993 | 14071 | 18169 |
| 原六领域值（不含snapshot） | 2168 | 2168 | 2168 |
| 固定snapshot1 | 270 | 270 | 270 |
| Acquire receipt1 | 338 | 338 | 338 |
| 完整20值合计 | 12769 | 16847 | 20945 |
| 外部配置公钥32B加入后的进程+保留值合计 | 12801 | 16879 | 20977 |

证书wire约1087–1088B、proof wire约1637–1638B已在对应record内计入（proof包含certificate），不可再重复累加。时间编码造成几字节波动。公钥32B是配置内存，未放入etcd小计。

这是安全GC前的完整创建工作集样本，包含历史journal；不把20值全部认定为长期live字段，也不假定GC已经实现。若同形状保留十万份，仅values约1.277/1.685/2.094GB（十进制），尚未计入key、MVCC/history、WAL、索引、碎片、replica、pending、历史唯一workspace fence、其他任务和备份。合成manifest分布、56B snapshot不能代表真实业务；没有测出十万/百万实际sandbox容量、QPS或延迟SLO。

原设计8KiB B_live是建模目标，不能据此忽略本次实测保留工作集。接入生产前必须分开实测长期live占用和`新申请速率×保留窗×历史工作集`，加入backend放大和reserved预算；当前代码尚无安全GC，不能承诺十万份适配默认4GiB quota。未来producer应引用immutable snapshot/version/digest，避免把完整配置复制进dispatch；GC只收terminal且永久fenced的记录，unknown不按年龄删除。按实际in-use/history/GC lag/pending bytes做准入backpressure，超过水位先限流/扩容；multi-cell仍在阶段六。

## 本批裁定（按发生顺序，完整保留）

1. 在Publish前增加persistent binding和pre-consumed mount单元：root签caller自选runtime不足以实现exact incarnation与once-only mount。若错，成本是额外功能单元及每个保留intent的固定记录，已纳入容量统计。
2. ALL immutable CAS输入来自同一fixed-point Txn，比较ModRevision+Lease0：避免拼接不同快照，revision绑定当前cluster/restore且原24/总64不放宽。若错，成本是有界额外读取和bundle helper返工。
3. 公共preparation loader只提供结构验证metadata和copied opaque cert；Bind/Consume/Publish必须constructor pinned crypto核验：恢复读不能构造权限。若错，成本是明确API文档及未来caller纪律/接口返工。
4. preparation readonly replay额外no-write identity/restore+原24比较：本地live不足以确认server claim/control没变。若错，成本是每次replay多一个固定RPC；不续Lease。
5. 将receipt strict schema检查放入真正shared decoder，保留既有错误优先级：Task2实际严格检查位于preparation reader，先前context误述为shared已严格。若错，成本是小范围shared修改及同首次revision回归；未重构generic ResolveStage。

前一dispatch批两项裁定及whole-branchreview保存在同目录 `2026-10-06-etcd-runtime-dispatch-verification.md` / `2026-10-06-etcd-runtime-dispatch-final-review.md`，包括private claim保留真实WorkspaceIdentity和逐单元先验证提交再独立review规则。
