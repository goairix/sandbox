# 可信 workspace 同步接线准备

这是后端归档交付之后的实现约束与源码研究记录，不是 provider、安全释放、生产迁移或容量验收。主方案仍为 `2026-10-06-etcd-state-management-design.md` 的阶段一至五。研究期间未访问云端、输出凭证、改变依赖或部署。

## 已确认的输入边界

归档接收结果仅公开 Close，没有路径、FD、通用 Reader 或 provider authority。后续消费者必须先登记原始 body/consumer owner，再读取已验证 spool；结果 Close 与 Backend.Close 必须封闭新读取、取消并物理等待现有 owner，之后才能删除文件或归还额度。provider 工作可以晚于历史归档会话完成，但须使用新的当前 claim/native intent 授权，不能延长归档票据或复活已失效的发送窗口。

`TaskFinalSyncPlan` 当前只有 Mode 和 Digest；原始不可变记录携带 task、claim、历史排空归属和 AttemptID。实际同步计划还须规范绑定 WorkspaceIdentity、provider、物理 storage identity、bucket、canonical prefix、排除规则与删除策略，并核对 Task.WorkspaceHash 与预留 Plan.Digest。目标 URL 或调用者填写的成功摘要不能授予权限。etcd 已依赖 internal/storage，接线避免反向循环。

样例 `configs/config.yaml` 是 local/sync；现有 factory 还支持 S3、COS、OSS、OBS、MinIO，不能猜测线上 provider 或只实现远端对象存储后宣称全部接线完成。local 需要受信 scoped directory、身份校验、有界流式写入和物理/fsync 证据；bind/no-sync 不能单独证明安全释放，也不能把网络/FUSE 文件系统当作已结算的本地磁盘。

## 原有语义与有界资源

`internal/sandbox/workspace.go` 原同步按 runtime/storage manifest 差异上传，并删除源中不存在、且未被排除的对象。新链路保持现有对象布局、排除和删除语义，禁止排空后再次普通 exec/DownloadTAR。原有全量 map、ScopedFS 全文件 buffer 不适用于大 workspace；采用受限归档索引、已知长度的流式 body、有界分页 listing 和有限分片并发。Listing 只用于观察，不是陈旧 DELETE 的授权。保留 workspace root marker；目录、排除和索引哈希碰撞处理需明确。

每个真实 wire 请求都要有不可变描述符、原始受信 worker UID/incarnation、当前 fences、持久意图和结算结果。SDK 重试、跳转、region discovery、分页、分片及 CompleteMultipart 都是独立请求边界。逻辑 entry 的长度/hash 与签名封装后的 wire 字节可以不同，描述符不能记录认证秘密。全 cell/partition 的意图数量与字节须背压，unknown 不随 lease、年龄或 worker 退出自动回收。

## 已安装 SDK 与 Go 的实际边界

- MinIO v7.0.91 `api.go`：seekable body 可重试；MaxRetries=0 保留默认，1 才是单次 SDK attempt。未指定 region 时存在附带 discovery 请求。错误 body 的 io.ReadAll 与忽略 drain/Close 错误需要独立限额与终结观察。已知 size/DisableMultipart 的单 PUT 也必须遵守实际 provider object 上限；大文件采用有界 multipart。
- MinIO `api-put-object-streaming.go` 的 HTTP 200/UploadInfo 与 deferred `utils.go` closeResponse，不能单独证明实际 response framing EOF 和物理 Close 成功。限额溢出不能伪造 EOF；响应头、ETag 或 SDK nil 也不能代替原始结算证据。
- OBS v3.25.4 有原生 context；MaxRetryCount=0 仍可能跳转。旧 ScopedFS 接线忽略 context。AWS S3 CompleteMultipartUpload 的 HTTP 200 可能包含业务错误，必须验证实际协议结果。
- Go 1.25.6 net/http Transport 可在 RoundTripper 内部重试，包括 HTTP/2 cached-connection fallback。若采用私有 HTTP/1 实现，需显式限制协议、路由与重用，并用真实 TCP/TLS fixture 验证 wire 边界，不能只数 wrapper 调用。
- Request.write 的 WroteRequest 可发生在外层 bufio Flush 之前；提前响应也可能先于 writer 完成。body EOF、SDK nil、DisableKeepAlives 或 CloseIdleConnections 都不能通用证明全部 writer 已物理退出。需要私有原始连接/body IO owner，封闭新 IO、取消/关闭原始 socket 并物理等待实际 in-flight Read/Write。无需 fork net/http。

## 结算、故障恢复与后续验收

原始 provider witness 必须由私有 adapter、原始 request、已验证 body/hash、持久 intent 与物理目标共同产生。任意 callback、io.Writer.Close(nil)、新 HEAD/list 或相同内容都不能证明旧未知写不会晚到。明确区分远端语义终态与 etcd 终态提交结果；终态写响应丢失仅可仲裁元数据，不能重新发送 bytes。

杀死 worker/runtime 只能阻止未来请求，不能结算已发 PUT/DELETE/CompleteMultipart。未知保留 owner/pending，不启动同 prefix 的新写入。无 dispatch 阶段的恢复也需要旧实际 worker 隔离及精确无待发请求证明，不能只看 lease 过期。第一版不引入 gateway/staging，不改变当前对象布局。

当前原始 final-sync/command/发送能力在 manager 重启后不能由记录重建。它们证明保守拒绝，并未完成主方案承诺的正常 manager 故障恢复；后续必须分别验收无外部效果的重启恢复和有未知远端效果的 sticky pending。长期空闲 runtime 的 query/TLS/issuer 证书协调更新同样未完成，不能放松旧证书 Match 或为每个已有 sandbox 增加常驻 actor。

下一实现先规范绑定实际同步计划与原始归档消费身份，再落地持久 per-wire intent、原生 provider/本地 IO 结算、实际基础设施终止及安全释放。之后仍须生产 API/Pool/Docker 接线、分区调度、FUSE/collector、实际大 N 容量/性能、部署恢复与 Redis 移除。

## 规范计划之后的消费接缝

当前 `taskReceivedArchive.inspect` 校验原始 seal/store/witness 与关闭状态，没有授予新时效权限。接收 ctx 仍固定在归档 SessionNotAfter；新消费需要独立当前 claim ctx，不能直接延长或复用归档 ctx。`task_archive_spool.go` 的 active/ownerDone 已保护 reserve/receive，但 cancel 只覆盖旧 receive ctx；新消费者需在任何读取之前登记原始 owner、独立取消入口和物理完成通道。Claim Renew/Release 与 Backend.Close 还须发布新 owner 的有效期、取消并等待其 native monitor 与实际 IO，之后再关闭 client、删除文件和退款。

`task_archive_spool_unix.go` 的现有检查覆盖 UID0600、regular/nlink1、dev/inode 和原始目录名，不覆盖 exact length/content hash。未来消费须在效果之前核对 artifact size/SHA256，按原始 body descriptor 保持完整性；已发 body 的 hash 不符要保留远端未知，不能撤销已发字节。私有 FD 不公开也不能代替内容检查。

现有33-byte slot 仅为 path hash/kind，无 body offset/hash；不能无额度扩大 index。可以顺序解析有界 TAR、逐 entry 算 hash 并以受注册 owner 保护的 SectionReader 流式消费，但 PAX 的逻辑 body offset 必须明确。索引缺失和索引损坏不可都当作“源中没有该文件”以授权删除。当前 task checkpoint 仅 pending/needs_reconciliation；没有 terminal safe-release 实现，现有 Lease-attached OperationRecord 也不能代替永久 provider unknown。

per-wire ledger 必须把全 cell 与 partition 数量/字节收费和不可变 intent 一起提交，崩溃/unknown 后继续收费；退款须有原生 adapter 的确定结算证据，终态历史 GC 独立。预算配置身份需持久固定，不能由多个 Backend 各自复制额度。归档 fixture 已用50 comparisons，加入预算与 intent CAS 后必须重新检查实际 protobuf 节点及 request/response 字节上限。上述都是后续约束，尚未实现。

## 下一单元的预算与性能约束

全局 usage 每次请求 CAS 会形成热点。7391a5e/27feaf6已验收固定partition的永久budget slots：256分区每分区S1..64，unknown/崩溃占用不会随lease过期释放；实际Txn保留50历史pins、同一写Txn完整prefix条件，63helper节点/13360requestbytes，继续受原64节点/64KiB记录/256KiB事务限制。默认nil拒绝意图，operator固定配置只读；没有自动退款/重分配。分区热点可能保守背压，尚未实测实际容量或吞吐。

每个请求归属须规范绑定逻辑 body 字节/hash 与实际 provider、物理 target、bucket/key、multipart ID/part number、删除对象及分页 cursor；先检查 metadata 上限再复制，不记录认证头或预签名 URL。原始 source 只能证明原始字节与当前 claim，不证明调用者 WorkerID 就是实际可信 worker，也不证明 provider 请求已经结算。

当前消费实现保守地在描述符/打开 body 前重复校验整份 archive SHA。固定32KiB scratch 限制内存，但多文件时总读量仍可能随 entry 数乘 archive 大小增长。该source单元已在f1e67e6验收，不能称性能改善；容量验收前必须独立解决不可变 source 与避免重复扫描的问题，不能为了速度直接取消篡改校验。原有 archive path index 未为每个 entry 预留内容 hash/offset，优化不得无额度扩张索引。

## 持久意图之后的真实 worker 接线

现有launcher只有rolePID1/roleMonitor，BootstrapPID1/PrepareMonitor的原始seal不能证明任意backend goroutine或配置WorkerID是实际sync worker。后续须建立自己的原始bootstrap/incarnation/OS UID、凭证/FD/进程隔离证据，或验证具体runtime管理任务；API进程PID字符串或runtime ref不能替代该边界。

现有FileSystemMeta的StorageIdentity/Endpoint/Bucket/SubPath是配置归属，legacy ScopedFS向opaque driver转交Create/List/Remove，不能给出每个wire、实际物理target或writer退出的证据。已验收put意图单元仅有原始body/plan/claim的native元数据归属；后续实际worker/target/派发/终态证据的额外持久记录必须先计入预算，不能假定intent+slot收费已包含无限证明空间。

目前source.body.Read每个最多32KiB chunk前后各做一次fresh nativeguard，另有250ms监控与描述符整archive检查。吞吐模型须计入约每32KiB两次native事务的活跃读成本，不能仅凭内存有界称性能提升。后续优化需实际adapter IO取消/物理等待与fencing证明，本意图单元不改既有source权限检查。

已验收cdb5612/b75fbeb原始worker kernel单元新增非零UID/GID、caps0/NNP及固定boot/start/ns/exe身份；这仅证明实际原始process及可复查kernel状态，仍不证明已登记Deployment或实际凭证/FD隔离。未来provider接口必须消费原始boundary与原始source，不能仅接收其复制reference或配置WorkerID。原生硬化依赖LinuxCGO0 AllThreadsSyscall，生产worker静态构建/安全配置接线还须验收，不能把host模型race称native硬化race。非root/dumpable0下固定namespace目录必须使用保留PROC/NSFS/type检查的O_PATH观察，不能通过加cap/恢复dumpable解决；fixture超时未join须保留ownedPID和失败诊断，不得丢句柄。

后续派发/终态描述符还须在native首次effect前绑定实际worker与物理target，并为每个额外持久proof计入固定额度。现有intent+slot申请实际已用63/64helper节点；新协议必须重新检查实际request与两分支reply，不能删历史pins腾节点或将额外records视作已经收费。首次ACK丢失且无认识birth的配对同字节ABA仅有元数据存在性诊断，任何原始worker或终态权能都不能据此重建。

## 2026-10-09 local IO 接线与 driver 源码研究

当前执行 `2026-10-09-workspace-sync-local.md` 两个顺序小单元，规划提交 `0ef0a57`。首单元 `fac3953`/`7bc4ff6`/review修复`6b411a0` 已验收：只读绑定实际 root FD/worker 与收费意图 v2 profile，保留全部50历史pins，实际59nodes/11517requestbytes/2629replybytes；原始明确ACK加postguard才有one-shot权限，unknown/cold诊断无权派发。原native21叶、修复覆盖15实际local叶和真实timeout保留/释放/Wait/reap通过，限定复审C0/I0/M0。详见`2026-10-09-workspace-sync-local-gate-verification.md`。第二单元真实 regular-file IO/fsync/Close尚待实现，成功证据仅原始进程内witness，永久intent/slot保留；没有额外native terminal key、退款或safe-release。

已读取本地固定依赖 `github.com/goairix/fs@v0.3.11` 的 MinIO/S3 writer 与 local Create：远端两个 writer 都先将完整文件存入 `bytes.Buffer`，Close 才调用 PutObject；MinIO Create 返回的只是该 buffer writer，Remove 转交 SDK RemoveObject。local Create 用普通路径 mkdir/create，未提供 scoped original root FD、native task fencing 或 fsync 结算。新链路不能直接将 legacy Create/Close 的 nil error 提升为本方案证明；大文件 buffer 同样不能放入固定32KiB可信 adapter。

本地 `minio-go/v7@v7.0.91` 源码 `retry.go` 定义 MaxRetry=10，`api.go` 的 executeMethod 使用 client maxRetries；这是当前固定依赖的实现观察，不是生产配置/实际云请求证明。远端接线必须单次实际 wire 为边界：SDK内部 retry/discovery/redirect/listing/multipart 的每次新请求在永久原始意图与当前门之后进行，响应不确定时保留收费；普通 net/http cancellation 不证明服务端不会稍后生效。仅设置重试数或包裹最终 SDK nil error 还缺少实际服务端结算证明。

后续 terminal 协议仍须先明确额外持久 proof 的固定收费与 GC：现有两 key logical bound 只覆盖 intent/slot，不覆盖每请求无限终态历史。native 终态写 ACK 丢失应只仲裁精确原始 birth、不得重发 provider 字节；冷恢复不能凭 object 当前内容 hash、路径或进程已退出推断原请求已结算。需分别验证正常无外部效果重启恢复和存在未知效果时的保守 pending。

## 终态与 slot 回收的新增协议研究（未实现）

仅在原始 body one-shot 或 known terminal 后删除 v1 intent/slot 不足以允许安全回收：旧 reserve 的 VERSION(intent)==0、VERSION(slot)==0 条件可在删除后重新为真；同一 claim 仍存活时，网络中的迟到原事务可能重新登记。不能只靠 caller 不再重试、SDK 返回成功或固定 TTL 阻挡。等整个原 claim 失效后再 GC 虽保守安全，但一个大 workspace 可能先耗完本分区 S≤64 slots，无法完成剩余文件。

候选后续协议是固定 generation slots，使用新的 immutable operator budget profile/namespace，保留 v1 诊断但不暗改其 immutable-slot 规则。每个有限 slot 保留 permanent free/occupied/terminal KV，native ModRevision 是不可复用的代次；新的申请先只读取得原始 free slot，再在原全部50pins+prefix+budget条件下比较精确 slot ModRevision/Value 与 intent absence。已结算后的 GC 更新同一 slot 的 free 代次并条件删除原 intent，永远不删除 slot；迟到原申请比较旧 ModRevision 必然失败。实际物理 key 数依旧固定256×S，与已有 sandbox N 无关，operator provisioning 与 quota 要计入固定成本。

节点初算：原 reserve 63helper nodes 的两个 absence 比较，替换成 intent absence + slot ModRevision/Value 会多一节点，预计64；必须用真实 protobuf/两分支字节实测，不能先删历史pins。原始 completion witness 的摘要可放入已收费≤512-byte slot 的一次 terminal 转换（完整原始物理归属留在 immutable intent≤B），而不是新增未收费无限 proof keys。若 terminal CAS 采用原54pins+intent3+slot3、success1Put、failure3points，初算也是64；这只是设计候选，未实现/未验收，失败分支和冷仲裁的语义仍须完整设计。

退款必须来自原始 adapter 的确定结算 witness；unknown、first-ACK arbitration、source/worker退出、file当前hash均不能产生 witness。native terminal ACK丢失仅仲裁精确代次/原始intent/slot的终态，不能重发文件字节；GC原始转变结果丢失仍保守，不能获得第二份quota。generation overflow或budget配置变化拒绝/新namespace，不自动复用编号。local进程内witness尚不能解决崩溃在真实IO完成与native终态之间的窗口，正常恢复与未知效果恢复继续分开处理。
