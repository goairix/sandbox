# 原生 Operation 准入验证记录

验证源码：`29c3cf6a40f1b2545071e565fdcc0fb655e26362`。分支 `codex/etcd-state-management`，用户指定基线 `feat/workspace-fuse-mount` 的 `b99b823`。本记录只完成 Operation 原生元数据增量；Manager/config/HTTP/runtime仍在旧Redis路径，整体阶段一至五未完成。

## 单元与审查

| 单元 | 提交范围 | 独立审查 |
| --- | --- | --- |
| leased records/coherent control | fe73d57..f04fe2b | Spec Passed、Quality Approved，无C/I/M |
| original Lease admission | f04fe2b..c391d9f，fix470db72 | 首轮Important赋值时机缺陷修复，scoped确认；后续whole-branch另发现完整证据I2，最终eec014e关闭 |
| resolve/renew/cancel | 470db72..c3fdc6f | Spec Passed、Quality Approved；Minor test cleanup最终29c3cf6关闭 |
| whole branch | b99b823..c3fdc6f，39提交/101文件 | 初始0C/2I/2M，见完整final-review；非只看最后提交 |
| 单次最终修复波 | c3fdc6f..29c3cf6，3提交/7文件 | I1/I2/M1全部ADDRESSED，无新C/I、无新OOS；M2显式延期 |

修复逐功能提交：358a21a同事务linearizable point检查；eec014e完整原guard/token/receipt与revision顺序；29c3cf6测试earlyFatal bounded join。真实RED/GREEN、focused/race与预算细节见final-fix，完整独立复核见final-review。没有用Task-level PASS覆盖之后真实失败。

## 修改后独立最终 gates

| 命令 | 结果 |
| --- | --- |
| `bash scripts/test-etcd-state.sh -v` | exit0，216 top-level PASS、0SKIP、0FAIL；etcd package race107.208s |
| `TEST_ETCD_ENDPOINTS=http://127.0.0.1:50987,http://127.0.0.1:50989,http://127.0.0.1:50979 TEST_ETCD_FIXTURE_PROJECT= TEST_ETCD_CONTAINERS= go test ./...` | exit0，etcd实际89.676s，其余通过或cached；不是全仓race证明 |
| `go vet ./...` | exit0，日志为空 |
| `go build ./...` | exit0，日志为空 |
| `git diff --check` | exit0，保留未涉源码文档与无关Sentinel用户修改 |

完整本地日志：`/tmp/etcd-operation-fixed-owned.log`、`/tmp/etcd-operation-fixed-repo.log`、`/tmp/etcd-operation-fixed-vet.log`、`/tmp/etcd-operation-fixed-build.log`。

fresh fault fixture为脚本独占project `sandbox-etcd-state-test-11594-1791299125`，三成员及foreign server固定3.6.15镜像；涵盖原foundation leader/restore/foreign/compaction/NOSPACE与新增Operation真实unknown/lateTxn/lease等用例。脚本trap清理后，controller分别按精确compose label检查container/volume/network均0。开发fixture `sandbox-etcd-state-dispatch-codex-20261006-a70c18d2` 曾仅用于namespace局部测试；Operation gate完成后，controller以精确compose project执行down --volumes --remove-orphans并确认container/volume/network均0。后续纯协议测试不需要fixture；下次native实现须重新建立独占fixture，旧endpoint已失效。

owned日志43 WARN：38 LeaseNotFound、3 Canceled、2 NOSPACE，来自故障/幂等路径；不宣称零warning或旧golangci baseline clean。M1 child测试故意触发parent/worker Fatal，父测试检查退出、hook顺序与completion；不是未解释的最终失败。

初始c3fd gate曾真实全仓FAIL：Renew/mutation_rewrite和placement_recreate期望error却nil。etcd3.6.15空Txn分支被分类serializable，跳过ReadIndex；不能因一次focused PASS忽略。I1在同一比较Txn的Then/Else增加默认nonserializable identity点读，保持原比较与no-write，并在实际gRPC请求边界验证；新的fullrepo/owned均通过。初始日志 `/tmp/etcd-operation-repo-final.log` 与原审查报告保留。

## 成本与容量边界

Operation无后台ticker/goroutine，只有活跃C才Grant原30s Lease，guard/token/receipt及mutation lock同Lease；永久owner/control不租约化。固定两active Operation（各Begin/Renew/Resolve/Cancel）在加1000 synthetic idle point前后，实际shape完全相同：16 Txn、38执行point reads、2Grant、2KeepAlive。每次Renew同Txn新增一个固定point，不增RPC。该局部结构证据不能代替10k/100k/1M真实N、Pod/FUSE、p99/GC/24h容量实测。

现有Begin正常读3次coherent discovery Txn，然后Grant、guard Txn、admit Txn、final evidence Txn；Renew为fence Txn+KeepAlive；Resolve committed为一次固定point Txn；Cancel为原Lease Revoke。不能把所有过程简化成两次业务写。records硬上限guard/token各4096B、receipt2048B；data存三leasedKV，mutation另同Lease lock。raw value不包含key/MVCC/WAL/副本放大；本批没有给bytes吞吐或SLO保证。

前序creation实测20永久KV GC前工作集，16B/4K/8K dispatch input约12.8/16.8/20.9KB，snapshot payload固定56B。不是长期B_live，也没有达到8KiB live目标证明。必须实现safeGC、幂等历史λ×retention、pending水位/背压与实际backend放大测试；unknown不可按年龄删除。Lease按C而非N只是性能方案的一部分，生产全量扫描/常驻续期/FUSEexec轮询还未移除。

## 完整 Rulings

1. 存储record restore错位沿用domainEpoch ErrIdentityMismatch，其他ownership/runtime/chain错位ErrCorruptRecord；符合已有原生loader taxonomy，成本是错误分类/API测试调整，不改变fencing。
2. operation Grant/Renew服务端TTL要求0<TTL<=30，而非强制等于30；服务器minTTL>30 fail closed，只清理已知原Lease。成本是与高minTTL部署的互操作限制，需明确协议/config调整，不能静默延寿。
3. M2 generic Stage实际Grant边界的完整cmp/write复制回归继续延期；代码已在Grant前复制、无实际alias defect。成本是未来别名回归可能漏过这项尚缺的边界测试，保留建议。
4. 全部23项Declined逐项裁定，整体必需需求均保留；unsupported absoluteUTC/恶意trusted依赖边界不作承诺，phase6 multicell后续单独交付。成本是后续协议/验收返工；未完成前不作production切换或整体完成判断。完整表见final-review。

前序publication/dispatch全部Rulings和容量边界见已提交 `2026-10-06-etcd-runtime-publication-verification.md` 及 `2026-10-06-etcd-runtime-publication-final-review.md`，本批不撤销这些裁定。

## 未完成的整体要求

后续持续管理身份、durable command/effect/terminal安全End、task claim/close/reopen/exclusive/destroy、安全ownerrelease；actual launcher与UID/BootID/IPC/key/FD/descendant isolation；native pool/配额、Docker/upload、scheduler/due/dirty/Watch、collector/shared renewal、safeGC/backpressure/capacity；Manager/API/SDK/config接入、全源drain/Redis删除、生产mTLS/RBAC/灾备writerquarantine。Cancel/expired只指短期元数据能力失效，不能替代target排空和外部remote-write结算。工作区报告暂保留供整体实施恢复，不因一个子计划完成而停止。
