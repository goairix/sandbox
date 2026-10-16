# 无workspace业务申请实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: subagent-driven-development。一个完整Task，验证后小步提交，唯一fresh wholeTask reviewer；连续接续总迁移。

**Goal:** 普通无workspace申请拥有明确detached身份，重试恢复原IDs/配置并只创建一次。

**Architecture:** 保留workspace identity哈希，新增独立detached domain；现plain service配置固定principal/defaults，CreateBusiness在原有界生命周期内做有限request/snapshot恢复，再消费原v2 Create/Resume协议。

**Tech Stack:** Go1.25.6、现snapshot codec/etcd request-control-publication读取、原正式launcher/TLS servicefixture。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-detached-business-acquisition-design.md

## Global Constraints

- immutableBASE36ec725e2610609ce043d0e47bded0b91b1f88cb；前置OWN/pair关闭，无重复任务或旧artifact访问。
- 原workspace hash不变；detached独立kind/domain绑定原Backend storage authority，不能虚构存储prefix或授予operation权限。
- 服务端固定principal，key可选但回传实际key/请求ID；原key同normalized请求沿旧resolved/IDs，不同请求409。
- 单次准入覆盖全部读取/动作，Close取消并join，无双重占槽/无界循环/常驻N状态；原AcquireIntent摘要/原能力协议不弱化。
- codec的数值e/E exponent绝对值≤MaxBytes61440，在ParseQuantity前验证，E/Ei后缀兼容；不运行危险巨指数。此为新实际入口的依赖计算预算，不重开旧OWN。
- apply_patch/Go1.25.6/Root-only精确小提交；唯一author/reviewer/无nested；无Sentinel、依赖升级、部署或Redisfallback。
- 独立OWN/epochs/actual17匹配平台race/prepost/逐成员/script/真实ELF；每次Docker Root具体preflight/实际joins/精确cleanup。

## Review Focus

- 同key首次并发absent且生成不同IDs：只一个POST，输家恢复原持久ID，不能伪造配置相同。
- defaults更改/原owner释放/旧v1记录：恢复原resolved和可读历史，旧不兼容记录明确拒绝。
- 客户端key变更或配置变更：不同key隔离，同key不同请求409，无额外Grant/POST。
- Close在request lookup或恢复读期间：单准入内取消并join，不晚调用provider或返回新成功。
- detached私有字段损坏/storage authority变化：校验拒绝，原workspace稳定hash完全保持。

## Task 1: Detached identity and idempotent business acquisition consumer

**Files:** 修改 internal/storage/state/etcd/domain_identity.go及对应identity tests，新增domain_detached_identity.go分离最小Backend绑定入口；task_archive_recovery_codec.go仅把原positional WorkspaceIdentity literal转named，task_workspace_sync_plan.go显式拒绝detached kind，不改持久schema/同步协议。修改 internal/provisioning/plain_cold_service.go、plain_cold_create.go（单准入主体提取）并新增plain_cold_business.go、plain_cold_business_test.go；按职责必要时新增plain_cold_business_replay.go。依赖补强可新增internal/sandboxsnapshot/quantity.go及quantity_test.go并将snapshot.go两个ParseQuantity点接入guard。不移动旧Manager/API类型。扩展原internal/runtime/controlrunner/cold_create_service_native_linux_test.go最小variant，新增detached_business_service_native_linux_test.go固定selector。collector位于本OWN复用trackedhelper。

**Consumes:** 原WorkspaceIdentity/Backend身份绑定，LoadRequest、LoadControl/历史publication、LoadSnapshot、strict decodeColdCapsule/validateColdBusiness、snapshot.Build/Encode/Decode、原service enter/leave/Close/Create/Resume与formalnativefixture。

**Produces:** 明确detached identity构造/验证；可选固定business配置与CreateBusiness(ctx,input)业务入口，返回实际key/原request与intent IDs、unknown参考、独占原business配置。具体字段/函数签名和历史ref寻址/首次并发有界算法由Step1冻结。

- [x] Step1 Root接受完整只读预检C1–C7：kind uint8/workspacezero+独立domain，Backend.DetachedIdentity绑定原StorageID；固定Business{Principal,Defaults}/CreateBusiness input{Key,Request}/result{原结果,Key,ownedBusiness}。private acquireEntered保留typed outcome/public旧行为，单准入+最终Close gate；intentpublished或requestcomplete优先历史publication ref，再pendingcontrol/有限fallback；仅首次acquire争用一次认证恢复。exponent guard≤MaxBytes、namedliteral compile兼容和sync kind拒绝纳入最小范围。START-GO tests/collector first，Root审后实际RED。
- [x] Step2 正指数61441实际行为RED后guard补强，9组codec GREEN/race通过，Root提交101082b。identity compile RED后5组GREEN/race通过，Root提交ef92972；原workspace固定hash不变，detached sync拒绝。
- [x] Step3 business compile RED后完整配置/原IDs恢复/单准入/Close和有限acquire仲裁实现；7组host GREEN/race通过，含实际loopback Backend查询取消/handler/server join（不冒充运行时成功）。Root提交ea1845d。CompareLoss实际unknown+ErrConflict，一次ResolveStage确认Aborted后才恢复竞争请求，unknown保留原参考。
- [x] Step4 native01原构建18ed87/session74071→0d9afd exit0；Root核对actual17 runner9134/launcher2007、prepost/逐成员/scripts/真实ELF与Go1.25.6 Linuxarm64 CGO0。Root运行1f09c2/session25351→b63639 exit0，3场景PASS/noSkip，3launcher/9bridge IO/3TLS etcd join，attach/container0/noOOM，精确b438e9900905ba76357d772bd1400779eec6b99074ebce91883c95aeae8aba3c删除/inventory空。最终4pkg vet01退出0/raw0B/实际9137输入。Root测试提交4864e09，无大N或新Destroy验收。
- [ ] Step5 原BASE→最终HEAD唯一wholeTask review，同pair修复；tracked验收/关闭本OWN，继续HTTP/Manager与生产接线，不在Task边界停下。

Ruling: 细化已批准无workspace创建和principal/key幂等，不引入fake storage path。固定资源/部署跨profile路由不纳入当前Task；预检发现原Backend读取或Close契约不足时，Root裁决最小实际接点，不另起重复任务、不重复问用户授权。

Ruling: 实际编译还需将task_archive_recovery_codec_test.go原测试helper的positional identity literal改named，仅编译兼容，不改持久schema。新增plain_cold_business_lifecycle_test.go将真实查询取消/Close fixture独立为单一职责；属于原计划的生命周期验证范围。private acquireEntered保留public Create原结果语义和typed outcome；原CompareLoss unknown不得直接等同aborted。
