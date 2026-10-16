# 普通 sandbox 当前查询与实时健康

本任务落实总方案的按ID业务查询及复用前实时检查。现有ReadSandboxSnapshot提供一致元数据，已完成的publication是历史证据，均不能直接声明当前runtime可用。普通冷建服务需要一个可供HTTP适配使用的完整查询入口。

## 行为

- 新增PlainColdService.Lookup(ctx,sandboxID)返回当前业务metadata、原request/intent/sandbox IDs、owned Business配置、原创建时间和当前到期时间。不存在返回明确not-found（具体typed返回/错误在预检冻结）；只有当前active、TTL尚未到期、真实runtime管理入口正常且最终原状态确认通过才返回可用成功。
- 全部读取、bridge和健康交换属于原service单次准入/20秒总期限/MaxActive/Close取消与join；无每N状态、goroutine、后台轮询或无界重试。任意未知、状态变化、期限或健康失败均不返回可用成功；查询不创建、不续TTL、不Grant、不执行用户代码。
- 最初ReadSandboxSnapshot；严格恢复原capsule与deployment、IDs/hash/partition/version，并验证历史runtime链与当前control/owner/runtime一致。原CreatedAt必须来自原immutable dispatch payload，不能用当前ExpiresAt减TTL推算。v1/v2兼容能力及返回Business为空的情况在同Task预检明确，不能默默编造业务配置。
- Backend新增同origin原SandboxReadView绑定的只读publication/dispatch读取入口，保持原历史14点严格验证、epoch/receipt/原证书归属。拒绝nil、复制、foreign/stale view；最后再次Confirm。不得伪造WorkspaceIdentity、改变其domain/kind/hash或放松公开workspace读取的身份验证。private decoder可最小提取只读hash/partition locator，不作为Acquire/operation授权。
- runtime新增独立小型健康查询，不复用/proc diagnostics采样。通过原pinned management mTLS和原Kubernetes bridge，绑定exact RuntimeIdentityContext、data gate epoch和随机nonce；响应只含有限标量、同nonce/identity/gate及当前gate状态。严格canonical/unknown/duplicate/trailing/size/EOF验证；请求/响应各≤4096字节。健康响应是当前观察，不是可序列化执行能力或未来复用许可。
- Supervisor在实际受保护连接处理，验证同一activation/runtime/gate，检查admission/closed/failed/failurePending并有界TryLock；不改变持久gate、不创建进程、不读全量/proc或全N状态。使用既有受控时钟、当前TLS验证和原连接/handler关闭机制，无本地墙钟权威fallback。
- 服务在健康交换前后按受控UTC保守检查 `now + 1s < expires_at`，最终确认同一owner/fence/placement/control revision未变。Close或context到期不得晚返回新成功。SDK/HTTP/主进程切换不在此Task，不将观察当Exec/file gate。

## 验收

协议非法/错nonce/错runtime或gate/截断和trailing/取消关闭；view nil/copy/foreign/stale拒绝，原workspace公开方法保持验证；实际服务完整Lookup健康成功、到期/metadata变化/target gate关闭/坏bridge或旧响应失败；单准入/Close join。Host/race与相关Linux vet、真实TLS etcd+正式launcher+原bridge运行时验证。原创建时间来源、owned返回配置及只读无副作用也需证据。唯一fresh wholeTask reviewer，原始失败保留，小步提交，不重跑已验收旧矩阵。
