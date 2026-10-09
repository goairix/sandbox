# Workspace sync 原子进度 checkpoint

本设计接续用户已批准的 etcd 完整迁移阶段一至五及小步验证提交要求。目标是原始本地成功写入回收时，进度与回收原子持久化，支持以后正常恢复的可信输入。基线 `17fb66ec16e2f3d40cf2731b653bcd263036903e`；主程序仍 Redis。本单元不实现冷权限重建、完整 manager 恢复、目录/delete/cloud SDK 或安全 owner 释放。

## 选择与预算

当前 GC 删除 terminal/intent，只剩可重复使用的 free slot，进度不能恢复。另建每文件 completion KV 会随文件数增长；GC 后另写 checkpoint 会存在进度丢失窗口。因此复用任务创建时已有的永久 checkpoint，原子执行 checkpoint Put、slot free Put、intent Delete。保持全部50历史 pins、prefix 与预算 pins：原GC59 comparisons，加 checkpoint Mod/Value2，共61；成功3 writes，共64 nodes；失败分支空，不追加3个 Get。请求/回复仍256KiB，record≤4096B，slot≤512B，intent≤B，key≤1024B；实际 protobuf 预检，不提高 cap。

checkpoint 保持原 task birth Create/Lease0。捕获前明确验证 Create/task/Version/nativeVersion/Lease/canonical value，nativeVersion/MOD 及结果Version不允许 MaxInt64。Mod+Value CAS 绑定已验证 generation，禁止 delete+重建；实际响应或只读观察必须验证原Create与nextVersion。没有新 key、N map、常驻 actor 或 TTL 退款。

## 兼容记录

现有 `TaskCheckpointRecord` version1 JSON 字节、strict schema、pending/needs_reconciliation 行为保持。新增 exported diagnostics pointer `WorkspaceSync *TaskWorkspaceSyncCheckpoint`；version1 wire 不编码该字段且必须 nil。version2 wire 必须恰有全部旧字段与非null workspace_sync，拒绝缺失/未知/重复/null/非canonical/超限字段。使用独立 wire 类型保留v1，避免 `omitempty` 放宽strict mandatory contract。现有 LoadTaskCheckpoint 只返回深复制诊断，不产生执行权限。

version2顶层 State=pending、ClaimID=原始当前claim UUID、Attempt=已有checkpoint的初始stage attempt、Reference原task。DetailDigest 为 canonical WorkspaceSync payload SHA256。只有 private 原始 GC 可以产生此版本；legacy PrepareTaskCheckpoint 对version2显式拒绝，不能覆盖/降级持久进度。claim bundle/claim creation 和诊断读取须能严格验证version2，在新claim下继续读取永久metadata；读取成功不意味着可以重新消费原archive。

`TaskWorkspaceSyncCheckpoint` 的固定 mandatory字段为：Version uint32=1；FinalSyncAttemptID UUID；PlanDigest SHA256；ArchiveSHA256 SHA256；ArchiveBytes uint64；ArchiveEntries uint64；TicketDigest SHA256；TargetDigest SHA256；EntryOrdinal uint64；BodyEnd uint64；PathDigest SHA256；BodySHA256 SHA256；IntentID UUID；IntentBirth int64；EvidenceDigest SHA256。无path原文、任意map或可扩展Blob。ArchiveBytes≥1536且可表示int64；Entries>0；1≤EntryOrdinal≤ArchiveEntries；0<BodyEnd≤ArchiveBytes；IntentBirth>0非Max，所有digest严格64hex。整个record4096B内，digest由既有 bounded encoder/shared hash 构造。

EntryOrdinal 是现有 source.count，计入已扫描目录/排除条目；BodyEnd=原entry.offset+size，不是TAR header随机恢复地址。仅说明最后成功本地regular-file；不把纯读取/排除/目录或EOF当完成。TargetDigest 是 canonical 原始 local attribution 的SHA（包含原worker和root物理pin），metadata 不授权新worker，后续恢复须独立重新验证。全绑定：task/claim、finalsync UUID、plan、原archive bytes/entries/SHA/ticket、target及实际 witness completion。

## 原始 GC 状态机

继续使用唯一 original witness/draft/settlement、原始 source/body/target 登记和当前 native/worker/root guard。source.busy 排他保护新增私有 checkpoint attempt 状态，任何mutex不跨RPC/物理IO，nativeMu 在物理postguard前释放。

首次GC派发前，在包含原54comparisons的 guarded native Txn读取checkpoint，严格验证task birth、canonical record与generation。只允许 virgin v1 pending（ClaimID/DetailDigest为空）或相同 claim/attempt及完全相同 archive/plan/ticket/target绑定的v2；v2 EntryOrdinal/BodyEnd必须严格增加。其他pending写入、needs_reconciliation、foreign/damaged/missing、版本耗尽均拒绝，terminal/intent保持charged。不覆盖任意既有进度。

构造完成payload，捕获原 checkpoint envelope/value 和唯一新canonical value，储存在已注册original settlement上；attempt一旦开始不得重置或换before。原子64node Txn执行slot free、intent Delete、checkpoint Put，failure空；共享现有 native执行/校验/lifetime流程，仅扩展内部可选failure policy，不复制ACK/header/wire验证循环。

明确成功ACK严格3个response及顺序、两个Put无PrevKv/nested header正确、Delete=1无PrevKvs、revision大于terminal/checkpoint旧MOD且非Max；postguard通过才设置completed。既有next只在此时允许advance。

unknown只在原句柄、新鲜guard下同一只读native事务观察slot+intent+checkpoint。仅当 slot原Create/terminalVersion+1/freecanonical、intentabsence、checkpoint原Create/旧Version+1/新canonical 且 checkpoint.Mod==slot.Mod>两者旧MOD，才识别同一原子GC。已复用/后续checkpoint变化仍unknown，不能单独凭checkpoint或free升级原结果，不能再次mutation或退款。明确CAS拒绝保持denied/charged。只读失败分支按现有 identity/restore/budget 辨别，≤64nodes；原mutating失败分支空须验证response为空并返回Conflict。

## 证明与退出条件

真实 RED→GREEN 证明旧GC缺少持久checkpoint；host strictv1兼容/v2字段/上界/worstwire64/65拒绝/unknown同点generation与scope负例。真实Linux S1两文件及65文件逐次 native检查checkpoint与free slot同MOD、nativeCreate不变/Version增长、ordinal/bodyend/digest绑定；每次activekeys仅原固定checkpoint+slot/intent，不新增key。丢失/畸形ACK、escaped late GC后readonly识别以及复用后unknown须实际测试，不用mock物理效果。

覆盖checkpointmissing/corrupt/changed/foreign/needs_reconciliation/版本耗尽、claim/restore/root变化、Close在新checkpoint读取及GC RPC/postguard的实际等待。优先复用现有 bounded fixture/mode，固定小批次，测试原始controller/child Wait及exactcleanup。仅新增触及必要的用例，旧全部迁移套件不重放。

限定hostrace/nativeCGO0、vet/cross/format/bash/diff，独立review和一次Root raw/source/binary/cleanup审计，小步提交。server WARN/ERROR及失败/skip原始保留，tmpfs非断电持久、分批非整轮性能容量、diagnostics非恢复权限。完成后接续正常恢复和安全释放，不能以本单元作为整条迁移完成。
