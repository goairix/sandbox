# Enrolled Launcher Transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 正式 launcher 使用 enrolled Supervisor，并通过真实管理 TLS 返回原签名 publication 证据。

**Architecture:** 一个可独立拒绝的完整 Task。严格版本2 protected config 固定节点身份/UDS；新增有限只读 request/response，复用已验收 enrollment/ready 验证与现有连接生命周期；新真实 CLI PID1 fixture 验证完整接线。

**Tech Stack:** 现有 Go1.25.6、Ed25519、TLS、Linux arm64/amd64；不加依赖。

**Spec:** docs/superpowers/specs/2026-10-16-enrolled-launcher-transport-design.md

## Global Constraints

- 正式 serve 只接受 v2 enrolled config；固定节点 UDS、canonical 非零 UUID node identity，无环境选择/legacy fallback，不改旧二进制排空合同。
- config≤16384；新请求≤32768/独立length-framed响应≤65536，publication certificate≤4096，各嵌套证据保持原codec上限；无 stdin/extra bytes，无任意 signer/成功 bool。
- 只读 publication，原 verifier/native guards 仍是 PublishEnrolledRuntime 的权限来源；关闭/未知不重开 gate，不写 etcd/Kubernetes。
- 无新增按已有 N 的 actor/map/timer/轮询，沿用16连接及5s有限IO；原实际Close/handler/signing join，TLS config immutable。
- 不读 CLOSED OWN，不碰 Sentinel，不访问真实Redis/Kubernetes/secret/完整inspect，不升级/push/deploy；仅 apply_patch 写源码/测试/文档/格式。
- 每作者≤1/总≤2 approved isolated native fixtures；全17字段实际 GoList匹配race/GOOS/CGO→build/test→post filename/SHA；保留原Wait/失败/exactcleanup/binary+source绑定。
- Git仅Root expectedHEAD+exactpaths窗口，common→OPEN OWN fcntl锁，branch/HEAD/initialempty/exactstaged/diffcheck/postempty，小步验证提交。
- 一个 fresh author、一个 fresh 完整Taskreview，同pair scoped修复；不重放已验收矩阵；本单元不接普通Pod创建/main/manifest/Redis移除。

## Review Focus

1. 缺失节点配置或旧v1不能悄悄构造非enrolled Supervisor；protectedsocket替换/权限错误不能成为新的信任来源。
2. 新只读purpose不能进入exec/close/archive分支或丢失旧EOF/一次send约束。
3. TLS peer/ready certificate/真实enrollment不一致、gate关闭或Close并发时不产生可用假证明；copied response不授权。
4. 超界response/trailing/取消/halfclose不会遗留原连接/handler或隐式重试。
5. native正例必须运行正式CLI PID1及signedclock/真实observer，不能只复用直接NewEnrolledSupervisor旧测试。

---

### Task 1: Enrolled CLI startup and finite publication query

**Create:** `internal/runtime/controltransport/runtime_publication_frame.go`, `runtime_publication_destination.go`, `runtime_publication_test.go`; `internal/runtime/controlrunner/supervisor_publication_transport.go`, `supervisor_publication_transport_test.go`; `cmd/sandbox-launcher/enrolled_native_linux_test.go`; `scripts/test-enrolled-launcher.sh`。

**Narrow modify:** `cmd/sandbox-launcher/protected_config.go`, `serve.go`, `serve_test.go`; `internal/runtime/controltransport/bridge.go`（固定socket常量）, `task_close_frame.go`（RuntimeRequest discriminant读取）, `task_quiesce_types.go`（Publication指针）, `task_archive_session.go`（新增固定 requestPublication mode）, `destination.go`（仅有必要的现有session接点）；`internal/runtime/controlrunner/supervisor_transport.go`（新purpose分派）。单一职责可调整新文件名，修改原enrollment/codec/validators必须先Root具体裁决。

**Interfaces:** `Destination.ReadEnrolledPublication(ctx context.Context, certificate []byte, claim controlprotocol.ClaimReference) (controlprotocol.EnrolledRuntimeEvidence, []byte, error)`；固定新 DTO/读写方法由transport拥有，RuntimeRequest新增Publication指针，Supervisor新private dispatch仅消费原Enrollment和EnrolledPublicationProof。独立copieddiagnostics不成为origin权限。正式CLI使用NewEnrolledSupervisor，不增加publicfactory/test成功接缝。

- [ ] Step1 阅读原config/request decoder/ready verifier实际上限，报告任何精确矛盾；保留新RED。实现v2config和新bounded wire，host finiteunit/race与format/vet通过后小提交。
- [ ] Step2 新目的client/server真实TLS接线；覆盖positive/身份与证书错配/closedgate/overflow/trailing/取消/一次send与actualjoin，保留RED→GREEN。仅新增选择器，不重复旧suite；通过后小提交。
- [ ] Step3 正式serve只构造enrolled实例；新有限Linux fixture运行真实CLI PID1、protected config/signed clock/独立observer→bootstrap activate→新TLS查询→原verifier认证；错node/关闭拒绝。使用新OWN原产物、全部原Wait/exactcleanup/sourcebinary绑定，GREEN小提交。
- [ ] Step4 affectedhostgate/Linuxcompile/vet/bash-n/gofmt-l/scopedBASEdiffcheck；报告所有实际命令/输入/失败/限制，声明不是真实Kube/容量/main切换。源码冻结后Root唯一fresh完整审查，同pair修复、tracked验收后继续master。

## Root preflight

本单元落实已批准 master，并行于冻结的 local-runtime-safe-release Task审查。文件无重叠：新Task只消费已验收Supervisor/enrollment及已存在transport，不改安全释放26paths。两Task若需要实际编译closurequiet由Root串行协调；当前reviewer只读、不执行旧suite。

| Pair/task | Producer / consumer | Finding and ruling |
| --- | --- | --- |
| Step1 → Step2 | canonical新DTO → TLS分派/response | 同一Task共享frame契约，不能各自放宽长度/EOF；wire限额取原codec更小值。 |
| Step2 → Step3 | Destination query → real CLI fixture | 必须运行正式serve与同一wire；不以直接方法调用代替native正例。 |
| Task1内部 | 原enrollment验证 → 版本2配置与新transport | 不复制validator或改变其cap/join；用原方法，新入口仅编排。 |
| Task1 → 后续Kubernetes driver | 固定protectedpaths/节点身份 → Pod/agent接线 | 当前cleanup profile不接受新mount；不在本Task通过放宽profile掩盖，后续driver必须整体设计/真实验收。 |

Ruling: 用户已批准完整迁移并明确“继续，不要停”，本spec/plan是master既定真实launcher路径的具体接口细化，沿用持续执行授权，不重复停下索要批准。成本：真实driver若需不同protected provisioning可能需接口调整；不放宽当前安全合同。
