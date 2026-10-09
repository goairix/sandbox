# Workspace sync durable terminal and generation slots

本设计落实已批准 etcd 迁移的持久结算和有预算回收，不扩大为新产品需求。基线 `3fcbf1304fd695d51712c65014ae3681bf3f0bee`；已有本地写入完成，仅进程内 witness，原 intent/slot 永久收费。生产 main 仍使用 Redis。

## Problem and selected protocol

每分区 S1..64 个永久占用 slot 无法支持连续的大 workspace 同步。直接删除 slot 使旧 reserve 的 absence 条件再次成立；仅 TTL、worker 消失或重新读取目标文件不能证明旧请求结束。选择独立的 operator-provisioned recycling profile：永久 slot key 的状态 `free → occupied → terminal → free`，每次 mutation 的原生 ModRevision 是不可复用的 generation。slot 从不删除，reserve 必须比较已观察 free 的 ModRevision 和 canonical Value。旧 reserve、terminal、GC 请求在后一代全部失败。

### Configuration and bounded storage

- 新 `Options.TaskWorkspaceSyncRecyclingBudget *TaskWorkspaceSyncBudgetOptions`，与旧 `TaskWorkspaceSyncBudget` 互斥。旧三字段 options、v1 budget/slot 编码和默认行为不变；新 profile 的 record Version=2、元 key `meta/workspace_sync_recycling_budget`，仍 immutable Version1/Lease0/Create=Mod。Backend 只读 pin，不自动创建或填补 slots。
- 内部 budget pin 增加 profile=1/2。profile2 在消费适配完成前不得误入旧 reserve/gate/diagnostic；旧入口 fail closed。显式 profile2 使用独立路径 `p/<partition>/workspace-sync-recycling-slots/<BudgetID>/<slot>` 与 `p/<partition>/workspace-sync-recycling-intents/<BudgetID>/<IntentID>`，不复用旧 absence 命名空间。
- operator 预建全部 256×S free keys；每 slot≤512B、intent≤B4096..16384、key≤1024B。每分区最多 S occupied/terminal intents；正常原始执行只使用固定 scratch，无 N map、全局 usage counter、额外未预算 proof KV、按 sandbox 常驻 actor。逻辑 bound 沿用 `256*S*(B+512+2*1024)`，额外唯一 immutable 配置≤1024B；MVCC/WAL/复制/历史版本不在此逻辑 bound 中。
- 新 slot 平坦 mandatory fields：Version=2、BudgetID、Partition、Slot、State、IntentID、IntentDigest、IntentBirth、EvidenceDigest。free 的最后四字段固定空/0；occupied 的 ID/digest 有效、birth=0、evidence空；terminal 的 ID/digest有效、birth>0、evidence有效 SHA256。所有状态仅接受 canonical 完整 JSON，拒绝 missing/null/duplicate/unknown、溢出、过长、跨预算/分区/slot。原生 KV permanent/Create>0/Mod≥Create/Version>0，版本递增溢出拒绝，恢复 epoch/full guard 仍原样执行。free 初建 Version1/Create=Mod，回收 free 可有更高版本，不能靠 Value alone 判断代际。
- intent 保持现有 v2 local 归属编码，BudgetRevision 指向新 immutable config。只有 profile2 原始已绑定 local body 可以 reserve，legacy v1、冷诊断、复制引用不能派发。

### Original reserve and local IO

读取候选 free slot 是诊断，不授予 IO 权限。真正 reserve 保留原50历史 pins、完整当前 prefix、budget3，另 compare intent absence + free ModRevision + free Value，成功2Put（intent、occupied slot）/失败5Get，总64 helper nodes。已捕获候选 generation 在原始 draft 请求逃逸前固定。未知响应只能读取固定候选仲裁，不改变候选、不重新 mutation、不授权本地写入。只有明确完整原生 ACK + 本地/物理 postguard 创建 one-shot authority。

原始 local gate 适配 profile2 occupied slot：永久 native slot 可被修改，必须匹配已捕获 CreateRevision、known original ACK 的 ModRevision/Version、canonical occupied Value 和 intent birth。不能使用旧 immutable Version1 规则错误放行或拒绝。实际 local 写入仍用原 worker/body/target、native guards、32KiB copy、hash/fsync/Close/postguard；所有失败保留收费，无自动失败终态。

### Durable terminal and recycling

新 private source `settleLocal(ctx, witness) (*taskWorkspaceSyncLocalSettlement,error)` 仅消费 `witness.original()` 及其原 source/body/target/draft，且 put 已同步返回、所有真实 syscall 已 join、完整 count/hash/file identity 与原 intent 相符。不得复用 finishIO 后已 cancel 的 put.ctx。必须重新登记 source/body/target 的 active ownership，直接检查当前 context/worker/root/claim/native 权限，Close 实际等待结算 RPC 与物理 postguard。此阶段不再次写文件。original witness 一次 terminal mutation，未知后仅只读；同一 source 当前 body 必须保留到结算/回收结束。

terminal summary `EvidenceDigest` 为固定 canonical 版本化 completion（intent identity/birth/digest、实际 witness bytes/body hash/dev/ino）的 SHA256，字段仅加入已有 slot。摘要是历史结算归属，复制摘要不授予执行或 owner release 权限。terminal compare54原 pins + immutable intent create/mod/value3 + occupied slot create/mod/value3；成功1Put terminal slot/失败3Get(identity/restore/budget)，总64。完整成功 ACK 记录 terminal Mod/Version；丢 ACK 仅可在同一 guarded point-read 中确认相同原生 Create、Version=occupied+1、Mod>occupied、canonical terminal 值及原始 immutable intent，不能升级任何冷 witness 或重复 IO。

新 private source `recycleLocal(ctx, settlement) (Outcome,error)` 仅原始已知持久 terminal 的 settlement 可调用。compare54原 pins + intent3 + terminal slot Mod/Value2；成功 Put canonical free + Delete exact intent/失败3Get，总64。slot永不删除；slot Create 已被前序 original pin 和 exact terminal Mod/Value 约束。正常ACK严格验证2responses及删除数1；丢ACK只读固定slot+intent，在 same native transaction 验证原 Create、Version=terminal+1、Mod>terminal、canonical free 且intent不存在，才可确认回收。若 free 已被下一代复用，结果保持 unknown；不再 mutation，不再次 IO。

每个 mutation 原生64nodes/256KiB request及reply/64KiB record 上限不变，实际 protobuf preflight 和 worst legal B/key reply 必须验证。若超限，Root 裁决重新拆分协议，不能删 pins 或提高 cap。current source 的 busy/ops/body.active/target.ops 在 RPC 前登记，source/body/target mutex 不跨 RPC 或 syscall；nativeMu 可以序列化原 native 请求，不跨物理 postguard。结算/回收一旦尝试其 draft 不重置；失败 claim/restore/root/worker 保守保留占用。

## Acceptance and limits

Root overflow clarification: native slot Version 或 ModRevision 等于 MaxInt64 时拒绝，因为下一次递增不可安全表达。此保守停止不是自动切换 budget 或提高上限。

Root original-owner clarification for Task3: 不仅检查 witness.self/origin.self；原始 intent draft 必须在实际 put owner 构造时保存唯一 original put 指针，terminal 消费时同时验证该绑定和 original issued witness。这样 joint-copy put+witness 后重设 self 也不能重建权限。settlement 同理保存在原始 draft，并以指针身份约束 GC；全部进程内字段不增加 KV，不改变旧 profile wire。

Root cursor clarification for Task3: profile2 已实际消费 local put 的 body，在 GC 明确完成之前 `next` 必须拒绝推进，保留当前原始 body 身份；unknown/failed IO 不得通过 body.complete 偷渡到下一条。未派发的纯读取及 legacy 游标不改变。误序调用可以保守取消 source，不能放弃占用或创建成功终态。

1. 新配置互斥、strict slot/schema/native generation、缺失或错误配置不初始化、legacy canonical兼容；host race 和真实 operator metadata 读取证明。
2. 真实 Linux worker/3member etcd/local IO：S=1 reserve→put→terminal→GC 连续至少65文件；实际观察每步 key/state/revision、bounded active keys；两个独立 Backend/client 通过原生事务竞争同分区 S1，仅一原始 ACK；捕获旧 native Txn 在下一代 replay 失败。未知 reserve 不派发，未知 terminal/GC 不重写，真实 claim/restore/slot变化和 Close wait 拒绝。同一物理 worker 进程中的两个 Backend 不能声称完成多 worker 进程或部署验证，后者留到实际 Deployment/容量单元。
3. 原始 evidence 记录所有命令/退出/实际叶子/SKIP/WARN/server日志、原 source/binary hashes、原始 child/attach/controller Wait 和独立 exact inventories。失败批次保留；同源分批通过不得称整轮全绿。tmpfs/nativeCGO0不证明断电持久性或 race。

这仅支持当前原始 owner 连续本地成功文件的有预算循环。失败/unknown 仍占用；不凭进程死亡退款，不处理目录/delete/云SDK。GC 后不保留每条历史 terminal；正常 manager 冷恢复需要后续有预算 cursor/checkpoint 协议，本单元不证明完整 final-sync、重启恢复或安全释放。生产接线、分区调度、FUSE/collector、大N性能/容量、备份部署和 Redis 移除仍未完成。Source重复归档SHA/每chunk guard成本保留，不宣称性能提升。
