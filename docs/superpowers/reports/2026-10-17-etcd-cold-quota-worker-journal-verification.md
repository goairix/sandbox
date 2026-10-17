# Cold quota worker journal 验证记录

## 范围与证据边界

本片只增加包内 worker paid journal 方法及其 sealed permit，不接通自动
consumer。原 public journal、Stage API、记录格式和调用顺序保持不变；后续
四次 mutation RPC 的 continuation 另行实现。本片既不 Grant、创建 Stage，
也不退还 quota 或删除 preparation 记录。

测试使用 bounded host gRPC 协议模型、真实 backend worker session、partition、
Range/Claim 原始成员及 enrolled owner。quota history 由已有 decoded codec
fixture 提供。模型结果不是 signed native etcd 证据，也不证明 physical death、
实际 quota return 完成或自动 quota consumer 已启用。

## 实现约束

- 原 `prepared.journalAttempted` CAS 只允许一个 submitter。重复败者直接失败，
  不 burn 赢家；赢家遇到拒绝、错误或未知 journal ACK 时，消费自己的原
  `prepared.consumed`，不能重用该 preparation 继续 Grant。
- 保持原 backend、prepared self、enrolled owner、reference 和 digest 绑定。
  从冻结 mutation 的原 returnKey 写入中解码 canonical quota-return 值，比较
  namespace、restore epoch、partition、requestID、stageID 和 attemptID 的原
  locator。该步骤不伪造 MVCC KV、不重算新的 reference，也不补充 RPC。
- 复用原 `coldQuotaAttemptComparisons` 和 attempt record builder，保留全部
  原 history fences。worker partition 必须同时匹配原 reference 和 history
  owner partition。
- 调用原 `beginColdQuotaWorkerCall`，在同一原始 admission 中支付 Range
  262144 bytes 和 Claim 1。失败或取消不退还已经支付的 credit。
- body base4 与 W24 base4 分别和本 backend fresh base4 做完整 protobuf 比较，
  只有完全相同的这四项可以合并；其余同 key 比较也不能去重。实际 fresh
  请求为 50 comparisons、1 Put、3 failure points，共 54 nodes；replacement
  为 57 comparisons、1 Put、3 failure points，共 61 nodes。failure 仍为原
  identity、restore、journal 三点，不替换成 worker 通用 failure 分支。
- 最大 wire envelope 从实际私有 worker 方法发出的请求派生，使用已有
  canonical domain 长度、opaque codec caps、worker record 2048-byte cap、
  1024-byte keys 及最大正整数字段宽度。断言 request 不超过 128 KiB，且
  request 加最大 failure response 不超过一个 262144-byte paid cohort。
  widened 请求只用于 sizing，不提交为业务 history 或权限。
- 只有可信 journal ACK 和原 call postcheck 成功才返回包内 sealed permit。
  permit 保存原 backend、prepared、owner、reference、self 和实际 native
  header 标量值副本，不持有可变 response pointer。ACK stamp 不是 physical
  或 quota completion proof。
- 原 worker call 在实际 native client 返回后才 finish；原 enrolled owner
  继续注册，context 保持原值，供下一阶段继续使用。Close 和 session Release
  必须 join 实际 client 返回，取消本身不能发布 call 完成。

`decodeColdRuntimeQuotaReturnValue` 是最小纯值 decoder 提取，共用原 strict
metadata 和 canonical codec 校验。原持久化 decoder 仍检查 immutable KV、
lease/version/revision 约束，以及 return KV 必须晚于 publication、reservation
和 release source stamps 的顺序；纯值解码结果不能作为持久化或物理 proof。

## Root 验证记录

所有 Go、race、vet、格式化、Git 与 native 验证均由 Root 持有；本片作者未
执行这些命令。以下数据来自 Root 返回的已审计记录。

### RED01

tests-only 冻结后，Root 确认唯一失败为批准新增但尚未实现的 API：
`coldQuotaWorkerJournalPermit`、`journalColdRuntimeQuotaReturnWorker` 和
`coldQuotaWorkerJournalComparisons`。Root 的 FULL17 输入、source、tools、
pre/post、archive 和 current 审计通过。

- inputs：9090
- raw bytes：859
- raw SHA-256：`7d71d5d67e62f07c0d52e315f432cda679b8a0234a83de33307dd22a5f2a1b3e`

### host01：真实 fixture cleanup 失败

首次 host 执行的产品运行断言通过，但测试 cleanup 确实失败，不能记为整体
通过。Root FULL17/source/tools/pre-post/archive/current 审计通过。

- inputs：9095
- raw bytes：186342
- raw SHA-256：`96da1c499c7da866da135fe90b8c151fc2515298a366bad60bebb6592af97928`

九种 unknown ACK 和 duplicate 测试的新增 interceptor cleanup 先关闭原
client；随后 fixture 的 `Backend.Close` 再用已经关闭的连接 Revoke worker
lease，产生 outcome unknown。release-session/close 测试还重复关闭了已由
显式 Close 关闭的临时 transport，返回 context canceled。

修复严格限定在新增 `coldQuotaWorkerJournalIntercept` 的 cleanup 和说明：
先确保原 enrolled owner finish，恢复仍打开的原 client，再调用并保留
`require.NoError(Backend.Close())`；最后只对 `Client.Ctx().Err() == nil` 的
test-owned client 各自执行一次 physical Close，保留真实 Close 错误断言。
产品代码、旧 `worker_test`、测试行为断言、超时和实际 client-return/join
条件均未修改，没有吞掉任意错误。

### host02：fixture 修复后

全部 8 个顶层 `TestColdQuotaWorkerJournal` 测试通过，用时 **1.844s**。
Root FULL17/source/tools/pre-post/archive/current 审计通过。

- inputs：9095
- raw bytes：12229
- raw SHA-256：`6b46d0408a9fa2deb9800fd450ad7c4e99ee1a234305bc4966833e63c4ac2032`

覆盖实际 fresh/replacement journal、54/61 节点和最大 wire envelope、五组
worker full fences 的逐字段失权、未知 ACK burn、重复败者不 burn 赢家、
Release/Close join 实际 client 返回、binding/locator/credit 拒绝，以及
body/W24/both base 比较各字段被修改时的拒绝。

### race01 与 Linux vet

- race01：PASS，4.572s；42 个顶层测试，覆盖新 Journal/RPC/Admission 和原
  Prepared/Journal，包含共享 decoder 和 public 原顺序回归。9100 inputs，
  raw 39386 bytes，SHA-256
  `da47e2e0cceddcad0cbcf3eacb458937fc41dce497751b4e850df2705f3477f0`。
- vet01：Linux arm64、CGO=0，`go vet ./internal/storage/state/etcd` 通过；
  9190 inputs，日志为空。

每轮结束后 Root 独立完成全部 17 类输入、source/tools、pre/post、归档内容及
当时当前文件哈希审计，再开始下一轮。没有数据竞争、失败或跳过；刻意取消和
异常 ACK 测试的原客户端 WARN 保留。最终 gofmt/diff check 干净。
唯一完整 Task 审查已核对最终证据及 fixture delta，Spec/Quality 均通过，
无 Critical、Important 或 Minor 问题。

## AGENTS 职责检查

注释覆盖 one-shot 消费、原 owner/context、paid call 生命周期、未知结果、
ACK 证据边界和 decoder 的值/持久化职责。函数按绑定校验、原 body 准备、
付费 admission、完整 fence 合并、实际 RPC、ACK 校验和 permit 发布分段，
不使用挤在同一行的独立语句。

新增实现保留在 etcd 包内的独立 worker journal 文件，因其紧密共享原
prepared、history transaction、worker admission 与 lifecycle 不变量，不为
文件长度扩大导出接口或拆出可重构 authority。值 decoder 放在原 record
文件，原持久化 reader 的 MVCC 责任仍留在原 wrapper；native transport 通过
固定私有接口调用，不引入通用 callback/router 或按 key 去重机制。
