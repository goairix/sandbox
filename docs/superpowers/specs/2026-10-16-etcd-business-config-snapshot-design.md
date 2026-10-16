# 业务配置快照与冷建消费

2026-10-16。接续已批准的 etcd 总迁移；前置有界快照读取验收9b2de478989cf0fc7b63c6212a23b3763bf17a5b。本单元把完整业务配置变成可恢复、可验证的持久快照，并让已有 plain cold 服务真正消费。沿用用户连续执行、小步提交、不静默丢参数的授权；不重新启动总方案讨论。

## 数据契约

新增独立 internal/sandboxsnapshot 包，复用 pkg/types.CreateSandboxRequest，避免移动旧 Manager 类型或引入 sandbox/provisioning 循环。Snapshot 包含固定 Version、规范化 Requested、Resolved 与 RequestedDigest。Requested 只保存一次：mode、timeout、resources、network、dependencies、workspace_path、workspace_mount_mode、workspace_sync_exclude 全部保留。Resolved 仅保存首次解析的 TTL 种类/秒数、四项资源和 mount mode，不复制依赖和列表。配置不包含服务器生成 ID、CreatedAt、凭据、健康或执行权限。

公开 Build(request types.CreateSandboxRequest, defaults Defaults) (*Snapshot,error)、Encode(*Snapshot) ([]byte,error)、Decode([]byte) (*Snapshot,error)。Defaults 提供 TimeoutSeconds、types.ResourceLimits、WorkspaceMountMode；Snapshot/Resolved/TTL具体类型由只读预检明确后冻结。Timeout 0取默认，-1表示永久，低于-1拒绝；有限秒数必须在转换 time.Duration 前防溢出。永久配置可以表达，但当前冷建服务必须在任何副作用前拒绝。mode仅ephemeral/persistent；mount仅sync/fuse，显式mount需要workspace，空workspace无有效mount。默认mount解析与旧Manager语义一致。

资源空字段按首次默认解析；有效量必须正值并符合现有资源单位语义，沿用已有依赖，无新依赖。RequestedDigest为规范化 Requested 的SHA256，不受部署默认值改变影响；Encode/Decode必须验证摘要和resolved结构，并检查显式请求与resolved不矛盾，不能把调用方伪造对象当已验证。默认来源无法从记录证明，部署匹配由consumer执行。nil/empty resources/network/slices按当前handler语义规范化；列表顺序保留，不排序依赖、策略或exclude。构建/解码返回独占数据，无调用方别名。

业务wire最多60KiB，UTF8严格有效，固定schema、unknown/duplicate字段拒绝，版本拒绝，canonical JSON编码及精确往返校验。禁止先解析无限数据：Decode先字节上限；Build/Encode输入尺寸也须在大额拷贝/序列化前有界，不能单靠最终Marshal再报超限。限额应从最终wire上限派生，避免任意削减既有合法业务内容。最终完整Backend snapshot envelope仍≤65536B，实际编码后在Grant之前检查。

## 实际消费与兼容

PlainColdRequest新增可选 *sandboxsnapshot.Snapshot。nil保持原v1 capsule的精确字段和字节，非nil明确v2 capsule包含完整业务快照。业务对象必须在Create内首次副作用前复制/重验，并与固定服务profile匹配：有限TTL等于请求TTL，resolved四项资源与实际provider spec等价，network Enabled匹配部署，Whitelist/BlockPrivate、workspace/挂载、依赖及其他当前plain-empty服务无法执行的有效配置明确拒绝。不得为了过测试放松provider或Bootstrap验证。

ongoing与completed均严格解码两版本，执行相同profile校验；不接受v1夹带业务字段或v2缺业务。恢复仅沿持久快照中的resolved，不重新用当前默认构建。保留原deployment/workspace/partition/request/intent/sandbox/version/claim校验，原completed历史LoadSnapshot路径不改成必须当前owner存在。业务解码只提供配置，无执行能力。可新增只读capsule业务提取入口供后续适配使用，预检须明确输入验证和返回所有权；不暴露任意factory。

当前Main/HTTP/Manager仍未迁移，此单元不能宣称支持完整业务创建、永久TTL或生产迁移完成；但必须有真实服务成功场景使用v2经过Create、ACK丢失Resume与coherent读取，不能仅交付未消费codec或纯负例。

## 验证与边界

先新限定测试RED，再最小实现GREEN。纯Go测试覆盖全字段往返/默认冻结/请求摘要独立于默认值/所有权/永久及溢出/边界尺寸/unknown duplicate非canonicalUTF8/矛盾字段。consumer host验证v1 bytes不变、v2严格性及unsupported/profile mismatch在Grant/POST前拒绝。限定race和最终相关Linuxarm64 CGO0 vet；不重跑已验收旧矩阵。

新增固定native selector复用原TestColdCreateServiceNative的正式launcher/真实TLS etcd/原进程Wait与IO join，提供v2正常成功及activation ACK丢失后恢复；小量v1兼容回归。所有native运行须Root逐次审具体参数，固定缓存镜像、无网络端口、只读/NNP、精确caps/resources/mounts/cleanup；不创建多套collector框架。OWN收集独立epoch实际17类GoList输入、匹配race变体、pre/post、归档逐成员、脚本helper和真实ELF/buildinfo；不得复用前置已关闭OWN文件。

apply_patch唯一源码/测试/文档写入，Go1.25.6；Root-only精确小提交，现有feature分支，无无关Sentinel修改、依赖升级或部署。一个fresh author负责完整单元，一个fresh wholeTask reviewer覆盖原BASE→最终HEAD，同pair修复。常驻状态不随N增加，无新每sandbox goroutine/map/watch/timer。
