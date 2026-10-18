# Detached plain cleanup 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. 每 Task 一个新实现者和一个唯一完整 Task reviewer。Root 负责 Go/Git/Docker/gofmt 与提交。

**Goal:** 实现 HTTP detached business 创建产物的实际自动销毁消费者。

**Architecture:** 从明确 detached/plain 来源生成独立 no-effect settlement，新增严格
终止 profile，复用原 DueWorker/Run/join/owner/quota/P 管线，最后接部署与 HTTP。

**Tech Stack:** Go1.25.6、etcd3.6.14、原 controlprotocol/launcher/observer。

**Spec:** `docs/superpowers/specs/2026-10-18-etcd-detached-cleanup-design.md`；继承已批准主方案。

**最新接续点（2026-10-18）：** `e01e233` 已完成 v2 配额返还 writer/公共与 paid
恢复消费者，实际返还、合法重用和提交后丢响应均通过组件验收。不要重做此前已关闭
单元。随后 `a76d373` 已完成 wire/preparation 组件与实际Kernel交接验证，仍有pre-E
门禁。下一交付验收必须闭合真实 Claim→销毁→释放→配额归还→Pending清除→HTTP；
按依赖小步提交，但不再把单独preflight或新增诊断阶段计为业务链路交付。冻结新增
协议层、平行worker及预算体系，优先复用原实现。managed Claim、完整phase准入、
自动发现与最终P删除、销毁自动消费和HTTP接线仍是主链剩余工作。

接续记录：`f23fcc4` 已关闭真实退款后 Joined occupation 的安全结算/槽释放，
`d515e87` 已关闭新 Q 预约先写后提交的孤儿窗口，组件验证与主分支整合通过。
不重播已关闭单元。`816a73d` 接通严格v2自动发现/退款/settlement/P，`82dc3b6`
接入正式入口，`c20835c` 修复Observer的原子Q预约兼容，`57cc453` 完成原
HTTPDestroyNative/正式serve-cold-quota的正常managed全链。真实HTTP成功、运行时
退出、原配额归还与S/P清除已通过；同二进制local和未配置拒绝回归通过。
主分支定向测试、构建与9502/9280输入等值集成通过，原生证据来自验证分支。
接续只推进未关闭的early/Unknown故障组合、真实mounted OpenHTTP原生组合、最大
输入与容量，以及主方案其余API/FUSE/collector/部署恢复和Redis移除；不把正常链
通过当成完整迁移完成，也不重播本次三个已通过场景。

## Global Constraints

256分区/4 Relist/20MiB/s Range/100 claim-s/32永久任务；64 native节点、128KiB请求、
256KiB回复、64MiB/10000keys worker预算。Unknown不重发、不提前释放；所有实际
native调用和原物理 goroutine 完成 join。源码 apply_patch；AGENTS注释/分段/包职责；
共用主仓库缓存。Sentinel及无关go.mod保持原状，不切默认入口、不push或部署。

## Review Focus

- snapshot v1、none计划、缺 archive 都不证明无远端 effect。
- 任意配置 digest 不证明 plain contract；必须从 pinned release image 计算。
- 旧 local v1 字节、比较及信任边界不变；新版禁止混合 profile。
- 丢 ACK/取消不产生新 attempt 或遗失原 Run，原请求 Query/receipt 必须可恢复。
- 重建 quota 身份不读取已删 ownership；大历史只使用原有限 reader/预算。

## Task 1：明确 detached/plain 来源与配置

**Files:** 新 `internal/storage/state/etcd/task_cleanup_detached_profile.go`、
`task_cleanup_detached_options.go` 及相应测试；修改 `client.go` 和
`domain_detached_identity.go`。若需复用签名 fixture，只允许窄构造参数提取，旧行为不变。

**Interfaces:** Options.DetachedCleanup *DetachedCleanupOptions；
DetachedCleanupOptions.PinnedPlainImage string；Backend 私有不可变配置。
`detachedCleanupSource(task TaskRecord, snapshot SandboxSnapshotRecord,
intent CreationIntentRecord, publication *RuntimeEnrolledPublicationEntry)
(WorkspaceIdentity,error)` 为私有来源分类，普通返回值不授予 effect 权限。
实现者须在测试前用≤20行冻结必要历史 verifier 调用及精确参数绑定。

- [x] 真实元数据与错误归属/配置的测试先行；Root确认真正 RED 后实现。
- [x] 构造前校验 release image/contract，复制配置并保持原 identity 公式。
- [x] 来源精确绑定、签名历史验证或明确复用已有认证入口，拒绝 local/FUSE/错绑定。
- [x] 定向 host/race/Linux vet；唯一 Task 审查通过；提交 `361003b`。未开放 HTTP/consumer。

## Task 2：严格 version2 detached 终止 wire

**Files:** `internal/runtime/controlprotocol/task_runtime_termination_authority.go`、
新的 `task_runtime_termination_detached_test.go`；必要新私有 validator 文件。

**Interfaces:** spec 中的 DetachedRuntimeEffectBindings 与 intent.DetachedEffects；
原 Encode/DecodeTaskRuntimeTerminationIntent 签名不变，ticket v1不变。

- [x] 新 v2 正向、缺 proof/混合 profile、旧 v1精确字节与新完整 digest ticket 测试先行。
- [x] Root真正 RED后实现严格variant，保持旧 canonical v1。
- [x] 全 controlprotocol host、相关race、Linux检查、唯一Task审查通过；提交 `9697527`。

Task1与Task2可在独立已有工作树并行；没有共享源码文件。两者共同的profile常量
以协议spec为准，来源分类不产生协议 settlement 或 signature authority。

## 后续接续门槛（不能跳过）

### Task 3 冻结合同：持久 no-effect 与原 Run 恢复

组件接续：`45ed1ca` 已通过 cold/memory 唯一限定审查，实际 native7/graph2、
原 local43 inventory、race/vet与完整输入审计通过；见冷恢复组件验证记录。
signed 长输入与完整 Task3 的容量及消费接线仍开放，不重播已验收组件。

固定长输入接续：`7102368`真实signed Q16949及main合并Q16952原链通过，
唯一限定审查接受；证明该合法场景，不关闭全部最大值、长输入内存与全局容量。

唯一共享纯wire类型及Encode/Decode/BindingDigest放在controlprotocol，供etcd与
独立observer复用；私有etcd adapter保留native及原job权限。原E以有界精确bytes/
digest保存，不复制全E schema、不预建导出matcher。历史WriterClaim、原none Claim
与后续termination Claim可不同；present永久fact不会因后续合法Run失效，absence
恢复则必须证明原真实Joined fence。共享codec本身不构成权限。

独立私有 settlement 键为 `p/<partition>/tasks/<TaskID>/detached-noeffect`，
lease0、create-once、严格 canonical codec。初始记录上限32KiB，必须用合法最大
输入实际编码确认；不能为通过预算而截断合法来源。记录保留原 Task/birth、
WriterClaim、Run/Worker、incarnation、AttemptID/BindingDigest、plain contract、
quiescence/原 none 及 source fences、原 E canonical+stamp/S stamp。
普通来源 DTO 和合法 codec 均不授予 native 权限。

原 job/Claim/Kernel/registry 指针、E/S 出生及 Run 认证后，使用真实 signed enrolled
reader、原 dual-drain revalidation 和 native source 点读。原 `none` reservation
不得为新 Claim 覆写或重新 Reserve；旧 final-sync Claim 与当前 WriterClaim 分开
保留。只有最终来源分类与全部 native 历史都通过才能提交。

新 Pending purpose=`detached-noeffect` 复用现有 AttemptID/Digest 标量，必须有
ClaimID/LeaseID，StageID/RequestID 为空；保持原结构可比较和旧 canonical。
先固定原 attempt/binding，经原 progress 得到持久 ACK，再固定 E.mod/wire。
BindingDigest 排除 Pending/E.mod 以避免循环，但最终 settlement digest 覆盖完整
原 E。draft 自绑定 Backend/job/Claim，复制不能获得提交权限。

写事务原 taskEmptyComparisons53 + E/S/finalSync/snapshot/intent/publication
六个真实 positiveMod pins + settlement absence =60比较；Then Put1、Else三点，
总64原生节点。不能复制旧 reservation 的65节点预读。source点读22节点，原
draft仲裁10节点；所有实际 protobuf 请求/回复仍须验证128KiB/256KiB上限。
writeAttempted 在 native 发送前不可逆，Unknown只能读仲裁原 draft、不重发；
absent 单独仍 Unknown，exact committed 在失权后仅返回诊断加 error。

cold recovery 只读认证的真实 E/S 与 Run.Joined=true、E.mod 严格晚于旧原 E，
才证明原 join CAS 已 fence delayed Put。无 settlement 时也不能靠 lease消失、
none或 caller Joined 值恢复。新私有 job-bound preflight 保存完整历史；原 actor
owner 消费后才能单次 CAS 新 Run、新 Claim、重新 dual-drain 和新 attempt。
现有 local archive wrapper 保留所有 profile/inventory 检查；若提取公共 CAS，
只提取严格 one-candidate E/S 操作，不扩大调用者权限。未接入实际 detached
inventory/actor 的私有恢复 gate 不计端到端恢复完成，接线仍属Task4。

冷恢复内存修正边界：仅在原registry job、实际E/S Joined及原Run/birth/occupation全验，
物理effectsJoined、owner已返回、j.claim=nil且旧原Claim在锁内确认lost后，释放旧close/Q
中OutcomeCommitted并cleaned的Stage指针；不改Stage对象、旧Claim/prepared/E/wire/attempt/
Unknown latch或none。未Joined/伪Joined不能释放，未cleaned仍保留；旧lost Claim不能因
stage=nil获得重新构造或发送权限。此释放不增加RPC；128KiB阈值不扩大。

文件按 records、writer、native reader、recovery 分职责；窄改现有 Pending/job
状态及必要共用 CAS。先测试真实signed来源、copied authority拒绝、Claim/Run/S
ABA、64节点/最大字节、Unknown/丢ACK/原实际join fence与cold恢复，再实现。
首次运行前必须再冻结 recovery 的实际比较和 native 字节投影，不能把估算当验收。

### Task 4A 并行边界：实际 termination 与 observer

读取组件已以 `809838d`/`bc9dc8a` 接受：实际writer→loader→observer、四类来源ABA、
真实Claim取消RED/GREEN、旧v1定向race、Linux普通/显式tag构建与vet通过；同一唯一
reviewer限定Spec/Quality PASS。见reader验证报告。只关闭读取组件，生产provider/
retained Query、laterRun、最大合法预算和完整Task4A仍开放，不重放已验收读取矩阵。

Task3 的 writer/read 连贯单元已以 `58f38fd` 接受并提交：真实三成员六顶层及四个
子场景、独立有界五点组件、Linux构建/vet与限定审查通过。仅该组件单元关闭，
本节之前的冷新Run门禁、内存和真实signed最大输入门槛仍开放；见对应验证报告。

独立复用D工作树，与B的Task3并行；唯一实现者
`detached_runtime_native_implementation`，合同在其OWN/CONTRACT.md。
拥有controlprotocol原termination key helper、etcd `task_runtime_termination*`
及nodeobserver相关实现/测试；不改Task3 shared settlement文件、due worker、
owner-release、quota、terminal、部署及HTTP。后续4B/4C接后者，不得因此漏项。

保留原KeysFor严格v1包装，新显式DetachedTaskRuntimeTerminationKeysFor只派生
固定v2键；Snapshot必须先读严格nativeTask后推导，不能caller指定。完整variant
比较包括Version/DetachedEffects，不能只比较common Authority。新私有
loadDetachedRuntimeAuthority(ctx,j,c)核原registry job/Claim/Kernel/Run、真实
永久settlement及signed source/q/none，独立保存真实q context/current certificate，
不伪造archive DTO。共用原provider/termination状态机与retained Query/no-resend。

observer从persisted canonical intent分支，独立检查native settlement envelope/
stamp、Task birth、source/q/none及signed publication/activation plain contract，
原E/S完整语义由唯一严格create-once writer/native adapter保证，不复制schema。
继续核原domain/Claim/Guard/TTL/prefix/clock/journal/PID1/proof。请求128KiB、
回复256KiB必须分别验证，保留旧v1实际合法形状。所有静态节点候选仍待实际pb.Size。

先测试v2 fixed keys、foreign variant拒绝和128KiB请求上限，再实际native live
authority/observer与历史Query。Task3真实writer尚未可用时，不拼签名字段伪造正向
authority；同一独立Task审查在该子任务实际完成后进行，不能当自动consumer完成。

Task3 为原 dual-drain 后的持久 no-effect settlement 与原 Claim/Run历史认证，
Task4 为新终止/owner-release authority 与实际 actor/配额/终态 reader 接线，
Task5 为显式部署/HTTP及新真实 Linux组合。每项开始前冻结具体原生读写/RPC/预算、
恢复引用和精确接口到本计划，再测试先行、实现、验收、小步提交；不能用当前
Task1/2的单元通过把这些门槛记为完成。旧local与已验收quota底层只测必要回归，
不重放旧完整矩阵或重复创建已完成任务。

### Task 4A 下一片：生产 provider reservation 与一次发送

沿用原唯一实现者/reviewer及D工作树。私有入口
`terminateTaskRuntimeDetached(ctx,j,c,destination,reservation)`验证原job/Claim，复用
原bounded runtime owner、Close/join和attempt marker。共享执行只提取固定三点读取
与原七槽状态机，不接受caller authority或任意比较。旧公开local入口、v1 matcher与
冷历史reader保持严格；detached重复同Claim仅live-profile只读诊断，不再进入provider。

私有issuer/contract选择器：v1使用原archive/配置，v2仅使用loader认证的certificate/
settlement contract。原provider Prepare/descriptor、UID/RV、effect ID、槽intent→
guard→一次Apply→readback→complete顺序保持；原不确定槽不重发。明确reserved历史
只Query原intent，不Sign/Terminate；错误或丢ACK立即保留原引用并返回Unknown。

三业务点含identity/restore共K5，base4+Then5+Else5=14节点。v2 provider初写56节点、
intent前59/后60；生产reserve59、terminated/cleanup-complete60。每个实际protobuf
请求/回复检查128/256KiB、64节点，记录实际Commit及HTTP/observer接收数；候选组成
不当transport上限。截止时间仍为caller/捕获Claim/owner30s的最小值，每步fresh guard。

首片测试复用真实B writer/job/enrollment，在配置前加入专用TLS Kubernetes API
fixture；Pod由正式plain builder构造，provider KV只能由原生产writer生成。真实执行
slot0/1及59节点reserve后，observer显式返回Unknown，检查唯一发送与同Claim不重发。
不raw种provider、不伪造terminal proof。完整slot2..6正向要真实PID1/cgroup证明；
freshClaim/newRun retained Query等待B实际cold gate验收。此组件不能计消费者/HTTP完成。

首片接续：`54e3d08` provider reservation/slot0/1及sameClaim诊断组件通过，
真实native、cutoff续租RED/GREEN、旧v1 race/Linuxvet及唯一限定审查完成。
后续从已接受的cold gate接续freshClaim/newRun retained Query，保持原请求不重发。

查询接续：`9c5ad2e`完成实际新Run/新Claim下原请求Query与原owner安全退休；
真实native+18race+Linuxbuild/vet、完整审计及唯一限定审查通过。完整Task4A仍开放。

Task3固定长图接续：`c0735b2`在原native Joined及lost Claim门禁内，退休完成的Q
发送/授权缓存及精确close canonical cache，保留none/Claim/prepared/E/wire/pins。
两次真实超限失败保留，最终固定合法长形状完整图129326字节及51节点恢复通过。
不关闭绝对最大输入、其他长历史组合、全局admission和消费者；不得增加容量上限
或用共享类型豁免掩盖剩余owned图。

确定字段组合接续：`95525f0`补首次Acquire的SandboxID/SnapshotVersion128字节，
真实companion8068字节及writer通过后再现完整图超限；完整已完成close缓存退休
使该组合122085字节、原long117652字节，四项native与唯一限定审查通过。
主分支配置hooks整合通过；该确定组合关闭，普遍最大值和全phase全局容量仍开放。

### Task 4A 物理证明接续

`5f540a2`三成员TLS fixture已实际通过，保留原单成员入口；只关闭该测试资产片。
下一片复用正式plain builder、真实launcher PID1/cgroup和NodeObserver Serve，
由observer在真实死亡及cgroup empty后内部签发proof，接真实detached writer与
原七槽状态机的slot2..6。测试不可直接签terminal proof或传入Claim/Run/authority
DTO；setup中的ID仅定位材料。原worker UID1000/caps0/NNP、实际进程Wait、
严格native来源及128/256KiB和64节点预算仍须验证。独立物理组合不得设置原
SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE，以免启动另一套HTTP fixture。
旧物理镜像缺失；新fixture改用真实Go用户子进程在ready前退出并Wait，避免外部
/bin/sleep依赖。`1e4d836`已实际通过真实PID1死亡/cgroupempty、observer内部签发proof
及原七槽complete；辅助和主分支组合、构建/vet、指纹审计及唯一限定审查通过，见
[物理验证](../reports/2026-10-18-etcd-detached-physical-verification.md)。只关闭该组件；
原test parking不是managed admission，普通probe在ready前已Wait；完整Task4A仍开放。

### Task 4B/4C 接续边界

4B基础片`0c6a009`已完成独立严格v2 seal和纯59cmp+3Delete+1Put的63节点投影，
原五项RED/GREEN、race/Linuxvet和主分支整合及唯一限定审查通过，公开v1保持原样；
这两者不产生删除权限。随后原live job/Claim/Kernel认证、cleanup-complete及七槽
digest认证后才能执行。Unknown保留原write latch，同Claim只读仲裁，不生成新请求。
删除仅Owner/Control/RuntimeIndex；新历史reader独立认证13个存活来源，加termination、
provider、seal共16pins，不能重建已删index或依赖当前E/S。历史发送Claim与新释放Claim
分别验证，不能因不同而拒绝合法恢复，也不能把历史Claim当当前权限。

live片固定共享纯接口为`decodeTaskOwnerReleaseProfile`、
`taskOwnerReleaseHistoryKeys(intent,actualTask)`与`decodeTaskOwnerReleaseProof`，
返回互斥local/detached私有proof；paid reader复用已读实际Task，standalone把Task并入
head读，历史重复Task必须全envelope/bytes相等。13点value caps合计208896字节，仍
须以实际pb核128/256KiB。原owner增加一次赋值的canonical candidate（最多8KiB），
同Claim丢ACK只认原bytes，finish退休ctx/cancel但保留候选，未来全局账本必须计费。
纯source校验局部提取保持旧publication Reference门禁，不构造假receipt/index/E/S。

4C复用原paid quota/terminal阶段。v2单独13点signed history需分阶段付费，保持v1
原五RPC路径；原一次性260480/262144字节上界容不下替换后的历史。选用仅定位的
metadata候选，完整paid认证前不得Grant/TaskClaim/Stage或物理/配额副作用。quota后
pending release同样分阶段认证，再原stamp CAS删除P。最终terminal v2最多22pins，
原v1最多18pins不变；原guard/cohort/Unknown/真实join及公平调度规则继续保留。

可并行的4C基础片仅新增独立v2 quota-return codec与测试，内嵌已接受v2 release；
原v1嵌套record与字节不改。仍用16KiB上限，需同时有效的最大schema模型及严格
canonical/envelope/variant测试，不能通过cast或伪Complete接原quota执行链。这片
不产生refund权限，不改Grant/guard/journal/Resolve/Commit或自动消费者。
该基础片已以`0bd7852`提交，六项RED/GREEN、race/Linuxvet、整合与唯一限定审查
通过；同时最大codec模型15360字节，完整quota/consumer仍开放。

4B首个live组件`1049226`已通过真实63CAS提交后丢ACK、dead Claim只读仲裁和冷13
认证，以及Host9/race14/Linuxbuild-vet与唯一限定审查。只关闭该组合；普通/损坏ACK、
cutoff/Close仍须逐片验证，不能称完整4B关闭。`268cd22`已补实际pre-send absence、
原Joined/actor51、新Claim/普通ACK63及cold13组合；独立普通/损坏ACK仍开放。

4C传输组件`9328ab4`补齐13点历史/22 pin新profile，单请求128KiB、单回复256KiB、
八次总回复512KiB独立校验及原flat parser验证通过；原v1维度不变。只关闭传输基础，
完整终态合同、quota writer/paid cursor及统一内存准入仍开放。后续`12af159`实际后端
终态正向与actual Task绑定已通过。`366869d`补齐四companion各四类拒绝及认证后
真实activation同值ABA；原八次RPC/21pins、原生实际Else及零effects通过，主整合
完整输入等值复用已核对。v2合法后继物理仍开放，不能伪造申请/来源关闭该项。

### Task 4 消费者的统一内存准入约束

`4fd2a15`已接通原 actor 的 detached source 认证和串行24MiB/20键预留；完整
Pending 避让、缓存失效重建、原调用返回后退款及主分支整合通过，见managed-source
验证记录。只关闭来源阶段；wire inventory、后续重图准入、真实 managed Claim
仍开放。`38ec47d`已关闭完整W20封包同时最大模型和超限前置拒绝，不能计作退款。
`cb78621`补齐七阶段 paid cursor 的真实完整 detached 历史认证，原4MiB/64键
计量和3次无付费 cached refusal通过；真实Linux与唯一限定审查接受。v2 writer、
journal/Commit/Postread及自动发现/P删除仍须实际接线，不能延用 nil-local 输入。

`75fae0b`先修复三处source严格JSON尾随检查对非法第二值的整树分配；真实
allocation RED/GREEN、22组race及Linuxvet通过，限定独立审查接受。该错误路径
优化不代替下述workspace或phase准入，详见source-json-tail验证记录。

`f32f5bd`已独立关闭原cache容量公式重复和Status未publish时漏记新cache的问题，
原automatic/cold预留不变；定向验收通过。仅作现有账目基础，不关闭下述job token、
phase重叠及退款要求，不把metadata模型回归当全局物理任务容量证明。

独立复核确认，原fixed8MiB+transient16MiB覆盖discovery固定表及单页/Watch/RPC
暂存；automatic4MiB与coldQuota4MiB已有各自归属，不能重复借给detached。
冷恢复128KiB只测原draft+candidate，不能外推live/Unknown或32物理job峰值。
当前enqueue、cache admission、OwnedBytes与finish缺少detached统一收费，尚未验收。

接线前先列各phase同时存活的Claim/Stage、native history、provider/transport、
重复编码/clone及slice backing上界；单独wire上限不等于整个owned图。signer结果
在复制前的边界也要核对。没有构造边界证明时，不用猜测常数开启容量承诺。
原worker账本使用单一detached reservation token：compact enqueue后，首次重图
构造/Grant/send前取得额度；phase升级预扣旧retained+新构造+暂存的重叠峰值。
空间不足保留旧phase，Unknown保留原token/attempt；取消或租约消失不代替join。
仅在原actor reply、transport/renewal/Claim实际join后缩额；finish在原registry锁
内移除并恰一次归还。cache admission和Status发布同账，自动/冷quota额度独立。
实际32原job、Unknown留存、finish释放及大N backlog仍需后续验证，不能以模型替代。
