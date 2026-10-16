# 真实 cleanup 执行与持久并发槽

本单元接续已批准的2026-10-06状态管理主方案，以及常驻due worker验收c6cee0f。目标是把既有关闭、排空、归档、本地同步、终止和安全释放协议组成可实际调用的cleanup执行消费者，并在真实工作开始前占有全cell持久32槽。不是新增一套任务状态机；复用现有永久Task/intent、原TaskClaim、不可变结果和认证runtime协议。

## 用户目标与本次交付

完整迁移仍需消除Redis状态依赖，且控制面开销不能随大量空闲已有sandbox逐个轮询增长。本次消费一个已经存在的明确cleanup TaskReference，实际执行支持的本地ordinary/enrolled Kubernetes清理链及恢复；下一单元再将它接入共享分区发现和独立worker role。已知引用消费者必须是可测试的完整实际编排，不能只交付无人使用的slot载体或把metadata任务创建称为物理清理完成。

现有原生组合测试已有真实链：PrepareDestroy/Commit → ClaimTask → CloseData → QuiesceUsers → dual drain → ReserveWorkspaceSync → archive receive → real KernelBoundary绑定 → local finalization → observer termination → exact provider cleanup → owner release。该测试是接口与证据的参考，生产实现应形成按职责组织的消费者，并保留全部未知结果与恢复约束。

明确Task引用的入口使用既有due actor的小型有界mailbox/内部准入，共用该actor原partition/session和Claim份额，不要求停止due来执行cleanup，也不向调用者外借其私有权限。公共入口可以位于原DueWorker或严格绑定其原owner的executor；确切签名由preflight冻结。本单元仅消费明确引用，cleanup-intent Range/Watch自动发现仍后续接入。

## Binding constraints

- 沿用已批准的256分区、原worker profile及全cell固定预算：最多4个relist、20MiB/s Range、100claim尝试/s。实际任务最多32个持久inflight slot，不能把每进程semaphore、本地MaxActive或预算Lease当作全cell硬上限。原TaskClaim和worker session分别保留自己的原Lease和实际生命周期。
- 当前due actor独占自己的原partition/session/份额。执行消费者的预算分配、原权限入口及未来共享调度关系必须在只读preflight明确；不能另建同分区竞争控制器、假设有额外空Relist份额、重借已释放tombstone或复制诊断引用为权限。本单元不新增全量sandbox扫描。
- 原actor只在已有有限native call/Watch实际停止join后串行处理mailbox准入；原预算计费、slot确认和实际TaskClaim开始的顺序明确。queue与active jobs各有固定上限，最多32实际任务，满时早拒绝；公平轮转和续租不能被新任务淹没。物理job保留原TaskClaim/effect权限，停止时先join全部实际job，再发布actor workDone/原session清理/final done。不能用无界队列、任意授权callback或一Task一session绕过共享原权限。
- 持久slot不附可自动过期的Lease。exact Task、原birth/attempt、槽位incarnation和恢复身份必须绑定；并发申请不能给同一未决Task重复占不同槽，也不能把旧终态确认用于新occupation。unknown reserve/claim/physical effect/settlement保留原身份和slot，不能以context取消、Lease到期、进程死亡、runtime404或一次空读释放。
- slot在真实Task执行开始前持久确认，32占满时拒绝或有界等待第33个实际开始；不能先Grant/派发副作用再补记slot。每个后续恢复步骤仍须取得原TaskClaim和相应真实权限；slot不是runtime、filesystem或owner释放权限。
- 只有确切已确认的任务物理终结/安全释放证据，且实际本地工作已join，才允许返还slot。现有owner release已满64节点，不能直接追加slot归还或候选删除。可采用独立终态结算，但必须精确锚定不可变release receipt、当前occupied slot和原Task身份，抵抗ABA/未知ACK/迟到结算。
- 保持64个native事务节点、128KiB请求和256KiB响应上限，执行前对成功/失败最大分支做具体protobuf预检；永久格式有明确版本、canonical严格字段/大小限制及native KV envelope验证。不能扩大generic Stage预算或放宽旧record validator来容纳新协议。
- 既有Task/cleanup-intent创建已经原子完成；本单元不增加松散postcommit索引。已知引用必须真实重验Task/intent/initial attempt/receipt，不能信任调用者或发现缓存。终态receipt与历史身份不能被提前删除。
- 消费者必须调用真实backend/runtime接口，保留关闭屏障、用户后代排空、原sync计划与不可变complete证据、exact observer enrollment/NodeUID/BootID、provider UID/RV以及安全owner/quota释放检查。不存在的支持能力明确拒绝，不能靠跳过归档/同步/终止形成虚假complete。
- 本单元明确支持已具备协议的本地ordinary/enrolled Kubernetes链。FUSE、远端对象写、Docker、cloud/none或其他profile不因此被视为完成；未知远端请求必须保留owner/intent，不能借本地完成证明远端结算。
- 使用真实可信worker KernelBoundary与已挂载的有限配置/credential/恢复目录。不得在HTTP进程伪造零cap边界、接受任意路径/目标地址/issuer回调以绕开原身份，或将测试签名/模型转换为生产权限。确切导出API、依赖所有者与关闭顺序由preflight冻结后才实现。
- 任务阶段、持久槽、恢复与实际请求等待有界；新增goroutine/队列由配置或最多32个实际任务限制，不能按N个历史sandbox常驻。停止与Backend.Close必须等待实际callback、传输、filesystem/provider请求及原清理返回，超时不是物理完成。
- 一致地落实AGENTS.md：导出API/重要配置和复杂不变量注释，逻辑分段，明确包/文件职责。原存储权限协议保持私有；业务阶段编排在有清晰依赖边界时单独组织，不能为拆包导出任意写或可采用权限。
- Root独占Go/Git/Docker/gofmt和提交；所有文件修改使用apply_patch。固定Go1.25.6/etcd3.6.14。保留已有无关Sentinel改动；不访问CLOSED OWN/pair，不重做旧验收矩阵，不创建重复实现任务。

## 验证与验收

先冻结真实调用边界、slot状态及严格格式、具体事务节点/字节表、共享预算分配、未知恢复和实际join图，再写定向实际边界回归并由Root观察RED。host/race覆盖多backend争抢32/第33拒绝、相同Task幂等/ABA、unknown ACK、失租不释放、已知终态结算及关闭/慢请求等待；不能仅测试载体序列化。

新增有限Linux原生组合用例必须走新的真实消费者到既有真实runtime/observer/provider/etcd链，验证完整工作与未知恢复；另验证实际etcd中的固定槽争抢/结算。32个元数据slot的验证不等于32个真实Pod/FUSE容量。Root提前冻结新selector、原生执行环境、源/工具/17类输入和实际清理证据，不读取旧OWN或重放原矩阵。

每个连贯通过的切片及时提交。一位新作者和一位唯一完整Task审查员，修复使用原pair限定复核；最后以已证明的明确范围验收。没有主程序接线、分区pending发现、独立部署角色或大N结果时继续明确报告这些剩余项，之后接着完整迁移。
