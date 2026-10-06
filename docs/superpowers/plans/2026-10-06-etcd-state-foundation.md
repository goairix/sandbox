# etcd 原生状态基础实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 在新功能分支交付独立可测试的etcd基础包：稳定命名空间、failclosed身份校验、专用Lease guard、持久stage attempt回执与真实三成员协议测试。

**Architecture:** 第一批不实现旧Redis Store接口，不连接生产allocator。领域事务由固定namespace、identity/restore guard、immutable leased attempt guard及持久committed/aborted receipt保护；应用Open只能验证已有identity，不自动bootstrap。后续领域Repository复用这些原语，并按主方案一至五阶段完成替代后再切换。

**Tech Stack:** Go1.25、client/v3 v3.6.14、真实etcd server v3.6.15三成员Docker fixture、testify、race detector。client补丁版本保留Go1.25兼容；测试server使用3.6最新补丁并固定image digest。

源码基线：`feat/workspace-fuse-mount`，`b99b823d69de6baa860eb95d47bd269599b03f42`。目标分支：`codex/etcd-state-management`。已有Sentinel兼容计划修改保留，不加入本次提交。

依据：[完整方案](../specs/2026-10-06-etcd-state-management-design.md)、[最后完整审查](../reports/2026-10-06-etcd-state-management-final-review.md)。本计划是阶段一的第一批基础交付，不等于阶段一全部Repository或完整Redis替代。

## 文件与边界

| 文件 | 责任 |
| --- | --- |
| internal/storage/state/etcd/namespace.go、namespace_test.go | 固定root/partition/key生成；拒绝路径逃逸与非法配置 |
| internal/storage/state/etcd/client.go、client_test.go | Options、Backend、TLS/timeout、全部endpoint身份校验、Close；Open不初始化 |
| internal/storage/state/etcd/identity.go | 明确authority/schema/cluster/storage/runtime/restore契约、线性读取和精确值guard |
| internal/storage/state/etcd/errors.go | 配置/身份/冲突/失租/unknown分类，可errors.Is检查 |
| internal/storage/state/etcd/mutation.go、stage.go、receipt.go、stage_test.go | 有界不可变mutation、专用Lease guard、持久receipt与重启仲裁 |
| internal/storage/state/etcd/stage_fault_test.go、fixture_test.go | 真实迟到/丢响应/expiry/race/NOSPACE/leader故障；全局故障限定owned fixture |
| internal/storage/state/etcd/integration_test.go | 真实三成员测试helper、并发与Lease/leader/restore/lateTxn用例 |
| testdata/etcd-state/compose.yaml、README.md | 独立三成员、临时卷、localhost随机端口、版本契约 |
| scripts/test-etcd-state.sh | 创建独立fixture、等待健康、设置测试endpoints、运行race、无论成功失败清理 |
| go.mod、go.sum | 固定client依赖，不新增etcd server进生产依赖 |

## Task 1：命名空间与原生客户端身份

- [x] 写拒绝空scope、cell、非规范绝对prefix、路径segment逃逸的表驱动测试，并运行观察RED。

```go
func TestNamespaceRejectsPathEscape(t *testing.T) {
    n, err := NewNamespace("/sandbox/v1", "authority-a", "cell-01")
    require.NoError(t, err)
    for _, segment := range []string{"", ".", "..", "a/b", "a\x00b"} {
        _, err := n.Key("controls", segment)
        require.Error(t, err, segment)
    }
    key, err := n.Key("p", "07", "controls", "sandbox-a")
    require.NoError(t, err)
    require.Equal(t, "/sandbox/v1/authority-a/cell-01/p/07/controls/sandbox-a", key)
}
```

- [x] 实现NewNamespace/Root/Key/Partition（固定256、SHA256首byte），Key不接受客户端原始完整路径。
- [x] 定义Identity，绑定SchemaVersion、AuthorityID、Cell、ClusterID、StorageID、RuntimeID、RestoreEpoch；Options显式Endpoints、Namespace、Identity、DialTimeout、RequestTimeout、TLS。
- [x] 写Open在空backend、任一身份字段错误、任一endpoint异集群时拒绝的真实测试。默认TLS；测试仅通过显式AllowInsecureLoopback使用localhost HTTP；不接受跨网络plain endpoint。
- [x] 实现New仅线性读取operator预置identity/restore_epoch并验证所有endpoint cluster ID；保存原identity value做Txn compare。不开初始化/reset入口。

```go
// 每个危险Txn的共同前置条件，后续所有stage必须追加。
func (b *Backend) baseComparisons() []clientv3.Cmp {
    return []clientv3.Cmp{
        clientv3.Compare(clientv3.Value(b.identityKey), "=", b.identityValue),
        clientv3.Compare(clientv3.LeaseValue(b.identityKey), "=", 0),
        clientv3.Compare(clientv3.Value(b.restoreKey), "=", b.restoreEpoch),
        clientv3.Compare(clientv3.LeaseValue(b.restoreKey), "=", 0),
    }
}
```

- [x] 运行unit与真实fixture身份测试；验证空backend未被写入；spec review通过后做质量review并提交。

Task 1审查及验证记录：[客户端验收](../reports/2026-10-06-etcd-client-verification.md)。root≤512 bytes、key≤1024、component及storage/runtime/restore≤128、原始identity JSON≤4096。TLS配置持有独立可变数据副本，共享signer和CA证书对象遵守不可变契约。

## Task 2：持久stage attempt与专用Lease guard

- [x] 定义Mutation为有限Comparisons与单keyWrites（RangeEnd也在复制前限制1024 bytes），Write仅Put/Delete持久业务key；不接受任意嵌套Txn或范围Delete。Stage保存mutation副本，创建后不能换payload。
- [x] 写重复key、跨namespace、保留meta/attempt/receipt路径、超64操作/比较预算、单记录64KiB及总256KiB字节预算被拒绝的测试，运行RED。
- [x] 实现BeginStage：request/logicalStage合法segment、生成不可复用attempt UUID、计算mutation摘要，Grant前编码并检查receipt，Grant独立Lease后Txn写guard。guard value包含ID/Lease/restore/digest，附原Lease；创建unknown不能返回可提交的Stage，也不能重建同ID。
- [x] CommitStage追加baseComparisons、guard精确Value/Lease/CreateRevision、receipt不存在以及业务Comparisons；写业务状态与committed receipt同Txn。receipt不附Lease；永久业务Write也不附Lease。

```go
compares := append(b.baseComparisons(),
    clientv3.Compare(clientv3.Value(stage.guardKey), "=", stage.guardValue),
    clientv3.Compare(clientv3.LeaseValue(stage.guardKey), "=", int64(stage.leaseID)),
    clientv3.Compare(clientv3.CreateRevision(stage.guardKey), "=", stage.guardRevision),
    clientv3.Compare(clientv3.CreateRevision(stage.receiptKey), "=", 0),
)
compares = append(compares, stage.mutation.Comparisons...)
ops := append(stage.businessOperations(), clientv3.OpPut(stage.receiptKey, stage.committedValue))
response, err := b.client.Txn(ctx).If(compares...).Then(ops...).Else(
    clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey),
    clientv3.OpGet(stage.receiptKey), clientv3.OpGet(stage.guardKey),
).Commit()
// err归类unknown；Succeeded证明committed；Else依据原子读区分已有receipt、
// restore/identity改变、guard失效与业务冲突。没有receipt不等于aborted。
```

- [x] 写正常原子提交、并发CAS同owner只有一个赢家、业务状态与receipt永久性、错误restore epoch、原Lease撤销后迟到commit拒绝等测试。
- [x] 实现Commit重试读取既有同digest receipt，重复不重写业务状态；Revoke仅回收guard，不删除持久receipt；bounded请求context。
- [x] spec review通过后做质量review，运行race并提交。

Task 2及resolver审查与验收：[事务验收](../reports/2026-10-06-etcd-stage-verification.md)。

## Task 3：事务结果仲裁与真实迟到请求

- [x] 写同attempt commit/abort竞争测试，以及absent只返回unknown/pending、CAS明确失败返回conflict的测试。
- [x] 实现ResolveStage：base guard加receipt不存在时写持久aborted marker；否则同一Txn读取已有receipt/restore/identity。resolver不依赖旧guard继续存活，旧commit因marker或guard缺失永久失败。
- [x] 已aborted的logical stage合法重试用新attempt；原receipt不覆盖。metadata receipt不同于target执行receipt，本批不提供“撤回外部效果”接口。
- [x] transport timeout、NOSPACE或resolver写失败保留ErrOutcomeUnknown，不能猜测未提交；回执格式/digest错误failclosed。
- [x] 在真实client中用可控KV wrapper在服务器收到之前暂扣完整Txn：先Resolve写abort，再放行原Txn，确认业务key没写入。另用wrapper在真实commit后丢响应，确认Resolve读committed而不重做业务写。
- [x] guard创建丢响应测试确认不返回可提交能力；原guard受Lease约束。Lease自然到期后持久receipt仍在，旧Stage不复活；leader单节点暂停并触发新选举后，剩余两成员仍可仲裁。
- [x] spec review通过后做质量review，运行race并提交。

## Task 4：可重复三成员fixture与第一批验收

- [x] compose定义三个独立成员，peer静态名称、临时独立volume、localhost动态client端口；不读取项目.env或现有etcd配置。
- [x] 脚本使用独立project，up后轮询endpoint健康，trap down --volumes清理，仅清理该project。image用server3.6.15固定digest；不宣称同主机三成员抵抗主机故障。
- [x] 设置TEST_ETCD_ENDPOINTS运行`go test -race ./internal/storage/state/etcd -count=1`，集成测试显式报告连接的member/version/cluster ID并确认三个member，不把跳过当作集成通过。
- [x] 运行`go test ./...`、新包race、`go vet ./internal/storage/state/etcd`、`git diff --check`；检查未提交Sentinel文档未被暂存。
- [x] 最终独立review完整首批diff，修复阻断项并复测；保存实际测试结果与剩余阶段清单，分批提交客户端、事务协议、故障测试。

第一批最终验收：[验证记录](../reports/2026-10-06-etcd-foundation-verification.md)。客户端`9bb3c1a`、事务协议`efffc2a`，故障测试独立提交。

## 后续阶段衔接

本批退出条件是上述基础协议在真实三成员下通过，属于“开始实施”的可审查代码交付。后续依次编写并执行领域Repository、request/operation/pool、runtime launcher gate、partition/due/dirty调度、collector/sync、部署/drain/删Redis各阶段具体计划。任何中间分支不得打开新allocator或移除旧保护；阶段一至五与完整恢复/容量验收全部通过后才执行单环境切换。多cell保留阶段六，不进入本批代码。
