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
- apply_patch/Go1.25.6/Root-only精确小提交；唯一author/reviewer/无nested；无Sentinel、依赖升级、部署或Redisfallback。
- 独立OWN/epochs/actual17匹配平台race/prepost/逐成员/script/真实ELF；每次Docker Root具体preflight/实际joins/精确cleanup。

## Review Focus

- 同key首次并发absent且生成不同IDs：只一个POST，输家恢复原持久ID，不能伪造配置相同。
- defaults更改/原owner释放/旧v1记录：恢复原resolved和可读历史，旧不兼容记录明确拒绝。
- 客户端key变更或配置变更：不同key隔离，同key不同请求409，无额外Grant/POST。
- Close在request lookup或恢复读期间：单准入内取消并join，不晚调用provider或返回新成功。
- detached私有字段损坏/storage authority变化：校验拒绝，原workspace稳定hash完全保持。

## Task 1: Detached identity and idempotent business acquisition consumer

**Files:** 修改 internal/storage/state/etcd/domain_identity.go及对应identity tests，必要时新增domain_detached_identity.go分离最小Backend绑定入口；修改 internal/provisioning/plain_cold_service.go、plain_cold_create.go（单准入主体提取）并新增plain_cold_business.go、plain_cold_business_test.go；按职责必要时新增plain_cold_business_replay.go。不移动旧Manager/API类型。扩展原internal/runtime/controlrunner/cold_create_service_native_linux_test.go最小variant，新增detached_business_service_native_linux_test.go固定selector。collector位于本OWN复用trackedhelper。

**Consumes:** 原WorkspaceIdentity/Backend身份绑定，LoadRequest、LoadControl/历史publication、LoadSnapshot、strict decodeColdCapsule/validateColdBusiness、snapshot.Build/Encode/Decode、原service enter/leave/Close/Create/Resume与formalnativefixture。

**Produces:** 明确detached identity构造/验证；可选固定business配置与CreateBusiness(ctx,input)业务入口，返回实际key/原request与intent IDs、unknown参考、独占原business配置。具体字段/函数签名和历史ref寻址/首次并发有界算法由Step1冻结。

- [ ] Step1 唯一fresh author只读预检：identity完整消费点、类型/身份绑定、配置owned验证、单准入提取与Close边界、request/control/history寻址及有限冲突恢复、最小实际native测试。Root裁决后START-GO，不提前Go/源修改/Docker。
- [ ] Step2 新identity/业务host tests与OWNcollector，Root审后实际RED；实现detachedidentity+后端绑定并限定GREEN，Root小提交。
- [ ] Step3 固定business配置/CreateBusiness/原快照恢复/首次并发接续与错误unknown保留；限定host/race GREEN，Root提交自洽业务代码，不等所有native完成。
- [ ] Step4 独立冻结构建、Root来源/ELF/script核对、具体native运行验证真实同key/默认漂移/409/并发/ACK恢复；完整joins/cleanup，最终相关Linuxvet，Root测试小提交。
- [ ] Step5 原BASE→最终HEAD唯一wholeTask review，同pair修复；tracked验收/关闭本OWN，继续HTTP/Manager与生产接线，不在Task边界停下。

Ruling: 细化已批准无workspace创建和principal/key幂等，不引入fake storage path。固定资源/部署跨profile路由不纳入当前Task；预检发现原Backend读取或Close契约不足时，Root裁决最小实际接点，不另起重复任务、不重复问用户授权。
