# 持久 workspace 同步意图与共享槽位

沿用已批准的etcd迁移主方案。原始归档消费者61fe19a/f1e67e6已验收。此单元提供真实native意图登记与跨Backend共享背压；记录是元数据归属，不能授予provider派发、实际worker身份、结算或owner释放权限。实际可信worker/物理target/每个wire的adapter证据将在后续接线，不能把Claim.WorkerID描述成OS UID。

## 固定预算

新增可选`Options.TaskWorkspaceSyncBudget *TaskWorkspaceSyncBudgetOptions`：BudgetID为非零UUID，SlotsPerPartition为1..64，MaxIntentBytes为4096..16384。nil保持原有行为且拒绝意图申请；先验证所有上限再复制或联网。配置必须已由operator初始化在namespace/meta/workspace_sync_budget，Version1、Lease0、Version1且CreateRevision=ModRevision，canonical严格JSON及不超过1024bytes；New只读、精确匹配配置并固定create/mod/value，禁止自动初始化或改写。配置改变需新预算namespace，不能回收未知旧slot。选项值与native配置均不可被调用者后续变更影响。

256个既有partition各有固定SlotsPerPartition个虚拟slot，不预写空slot、不开后台actor、不维护所有sandbox镜像。未知/活跃意图占用永久slot，Lease0；全cell至多256*S个占用意图。意图value至多B bytes，slot value至多512bytes，每个key至多1024bytes，逻辑key/value收费上界为256*S*(B+512+2*1024)。这不是MVCC/WAL/复制或实际容量验收。每partition上界S及相同固定块收费；不使用每Backend计数或每请求global usage热点。无结算证据前本单元不提供退款、slot重用或GC接口。

## 原始意图申请

私有`(*PreparedTaskWorkspaceSyncSource).reservePutIntent(context.Context,*taskWorkspaceSyncBody) (taskWorkspaceSyncIntentResult,error)`只接受该source当前、原始、尚未读取且未关闭的regular body；source/entry/body拷贝、foreign实例、disabled budget、失效ctx/claim等在native写之前拒绝。与source.begin/end共同登记实际RPC，所有Close仍物理等待；不持claim/store/source/body mutex跨RPC，不增加idle-N actor。只能记录`put_object`候选归属，保存原始task/final-sync/command/ticket/archiveSHA、plan/workspace/storage身份、exact relativepath、mode/size/bodySHA/offset，以及原始ClaimReference。无URL/凭证/公有Reader/callback或可伪造的实际worker证明字段。

body只保留一个有界原始draft，IntentID在第一次native写前生成并永不替换。原始同body重入只有元数据仲裁，不创建第二意图；可复制公开reference作诊断，不能重建申请能力。quota纯拒绝、尚无写attempt时可以按当前有效source重试；任何不确定已尝试写不得切换slot或重发事务。为公平选择槽，可由IntentID确定起点，只探测最多S个本partition候选，一次一个有界原生请求；不得扫描整个cell。

申请同一原子Txn比较原始archiveDelivery的全部immutable comparisons、完整operations prefix VERSION=0 range compare、固定budget create/mod/value、intent不存在、候选slot不存在，然后同时Put永久intent及永久slot。slot仅保存必要IntentID/digest等固定归属；intent与slot生成相同revision。完整prefix检查必须与写在同一Txn，禁止先读empty后无prefix条件写。Txn请求及两分支回复使用既有taskCheckWire逐一实际protobuf检查64nodes/64KiB record/256KiB总请求回复限制，不能删除历史pins腾空间或猜节点数。验证native VERSION-range对空prefix/任意有key的真实语义。

写之前固定draft.writeAttempted；未知reply/超时、source关闭或claim失效以后，即使metadata实际上已提交也只返回diagnostics+error，绝不生成dispatch capability。成功reply须验证cluster/header/nestedPut/count/revision，然后复查source当地有效性。Txn失败区分原始权限/预算身份失效与真实slot占满：不能把权限失效伪装quota继续试；预算全满仅在真实当前pins和slot证据确认下返回既有背压错误。source取消或Close不删除/退款持久intent/slot。

## 仲裁与诊断

原始draft仲裁只读取、核对固定intent+slot的完整canonical字节、原始id/digest、Lease0、Version1、create=mod及同revision；一方缺失/ABA/重写/extra JSON均拒绝。writeAttempted后absent仍是unknown（迟到Txn可能提交），禁止根据absent宣告未派发或再次写。原始已知commit重复调用只能获取元数据outcome，不重新收费。

公开`LoadTaskWorkspaceSyncIntent(ctx, TaskWorkspaceSyncIntentReference) (*TaskWorkspaceSyncIntentEntry,error)`提供复制历史诊断；严格namespace/task/partition/key归属、bounded record及base/restore身份检查，无原始body/native申请/发送/退款权限。冷Backend可读诊断，但不能从record/reference重建source或worker。本单元保留所有intent/slot；实际adapter终态证明与安全退款另行实现，不加公共success bool或caller callback。

## 验证

仅新selector`^TestTaskWorkspaceSyncIntent`。意义明确compiled RED先真实receive/source/body再预期登记失败；错误选项/codec/复制等不以undefined符号为RED。Host测试严格JSON、nil/foreign/copied/partialbody与key/record/byte上限、收费公式、固定64候选上界。真实owned etcd/TLS fixture在New之前初始化budget并提供选项，原有fixture默认路径保持。新native测试实际intent/slot Lease0与same revision、同bodyone-shot、两个独立Backend竞争同partition有限slot（不可复制原始source）、quota不会随Release/Close/lease到期释放、unknown committed/absent保守仲裁、late original Txn、restore/ABA/new operations-prefix fencing、budget rewrite/identity mismatch、constructor选项copy/缺失marker，以及挂起真实RPC时Backend Close等待且client/原始spool额度保留。日志无warning/重跑旧selector；race、vet、Linuxarm64CGO0编译，唯一独立审查。公开诊断不是trusted worker/实际provider/safe-release/容量证明。
