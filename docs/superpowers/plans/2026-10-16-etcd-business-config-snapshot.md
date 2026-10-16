# 业务配置快照实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: subagent-driven-development。一个完整Task，小步验证提交，一次fresh wholeTask review；连续接续总迁移。

**Goal:** 全字段业务配置首次解析后持久化，并接入plain cold实际创建与恢复。

**Architecture:** 独立typed codec保存一次requested和紧凑resolved；v1 capsule兼容，v2严格携带业务配置；consumer对照固定部署，在副作用之前拒绝不支持配置。

**Tech Stack:** Go1.25.6、已有pkg/types及资源解析依赖、现有etcd/native服务fixture。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-business-config-snapshot-design.md

## Global Constraints

- immutable BASE9b2de478989cf0fc7b63c6212a23b3763bf17a5b；旧OWN/pair关闭，不重放、不读其artifact。
- codec60KiB/完整snapshot envelope65536B，严格UTF8/schema/duplicate/canonical/digest；所有复制/序列化有界，完整全字段保留，无凭据。
- 稳定requested摘要独立于defaults/时间/服务器ID；resolved首次冻结；永久可表达但当前service副作用前拒绝。
- v1 bytes保留；v2实际consumer及native成功，原claim/fences/历史路径不削弱；无新factory/常驻N状态。
- apply_patch、Go1.25.6、Root-only小提交；无Sentinel/旧OWN/依赖升级/外部部署；唯一author/reviewer、无nested。
- 每次Docker单独Root preflight；独立collector/epochs及真实17输入/逐成员/ELF/script/原Wait与IO/container joins、精确cleanup。

## Review Focus

- 同请求不同默认值：requested digest相同，原resolved恢复不漂移。
- 有效但当前provider不支持的参数：完整存储，service在Grant/POST前拒绝，不能丢弃后运行。
- 伪造resolved、显式资源矛盾或不兼容capsule版本：严格拒绝，不能只检查payload摘要。
- 大列表/超大单字符串/UTF8损坏/duplicate字段：先有界再分配，合法边界可通过。
- 完成后owner已释放：历史读取兼容v1/v2，不因coherent current view强行失败。

## Task 1: Business configuration snapshot with real cold service consumption

**Files:** 新 internal/sandboxsnapshot/{snapshot.go,codec.go,snapshot_test.go,codec_test.go}；修改 internal/provisioning/plain_cold_request.go、plain_cold_create.go及其对应host tests；必要时新增plain_cold_snapshot.go分离profile/兼容解码；修改原internal/runtime/controlrunner/cold_create_native_linux_test.go最小固定variant，新增business_snapshot_service_native_linux_test.go selector。collector只放本OWN复用tracked scripts/cold-create-native-common.sh。

**Consumes:** pkg/types.CreateSandboxRequest/ResourceLimits/NetworkConfig/DependencySpec；原plainCold capsule/固定s.spec/claim/coherent恢复/历史LoadSnapshot；原正式launcher/TLS服务fixture。

**Produces:** sandboxsnapshot.Build(request types.CreateSandboxRequest, defaults Defaults) (*Snapshot,error)、Encode(*Snapshot) ([]byte,error)、Decode([]byte) (*Snapshot,error)；PlainColdRequest可选typed配置、严格v1/v2恢复与可验证配置消费。具体字段和最小只读提取接口由Step1冻结，不移动旧Manager类型。

- [ ] Step1 唯一fresh author先只读源预检，明确具体types、旧默认资源/mount语义、bounded输入算法、v1 byte兼容、最终envelope preGrant校验接点与真实native variant；报告Root裁决后START-GO，无预先产品修改/Go/Docker。
- [ ] Step2 编写codec与consumer测试/限定collector，Root审collector后实际RED；实现codec并限定GREEN/race，Root提交自洽codec小单元。
- [ ] Step3 v1/v2 capsule/profile预检/进行中及历史恢复实际消费；host negative+compatibility GREEN；Root小提交consumer。
- [ ] Step4 独立冻结native构建与Root逐项来源/ELF/script检查，Root具体run正常+ACK-loss v2真实成功及必要v1回归，原Wait/IO/transport/etcd/container全部join；最终相关vet，Root小提交tests。
- [ ] Step5 完整BASE→HEAD一份reviewpackage、唯一fresh spec+quality reviewer，同pair修复；tracked验收后关闭本OWN，连续推进业务幂等和HTTP/Manager接线。

Ruling: 此单元细化已批准总方案的业务snapshot/恢复接线，不扩大生产切换范围；使用独立DTO codec避免旧Manager类型搬迁。若实际resource/default或envelope接口与候选冲突，author只读报告Root具体裁决，不重复请求用户授权。
