# Task Runtime Cleanup Client Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 交付独立可验证的精确Kubernetes清理客户端，让etcd authority/termination/release实现可以并行推进。

**Architecture:** 已提交的6160c36提供固定types和七值enum；本单元只实现具体HTTP客户端/opaque handle/严格descriptor。Backend保留持久意图和全部native/physical权限，客户端不是fencer或physicalproof来源。

**Tech Stack:** Go1.25.6、现有client-go/net/http/TLS，无新依赖。

**Spec:** docs/superpowers/specs/2026-10-16-task-runtime-cleanup-client-design.md

## Global Constraints

- 文件域仅kubernetes/task_runtime_cleanup*.go；旧ownership validator只读，额外旧接点先Root。
- 七动作/至多四policy，无List/自动retry/redirect/替换UID删除/其他finalizer覆写。
- descriptor≤16384，request/EACHresponse/body≤262144；MaxActive1..32、RequestTimeout正值≤5s。
- 只支持ordinary enrolled plain/no-mount；精确nodeUID/boot/creation/runtime身份，unknown资源合同拒绝。
- actualIO/Close/signers真实join，publishedTLS/config immutable；nil/copy拒绝，diagnostics不是native/physical权限。
- Apply每槽至多一次mutation、unknown不自动重发；Backend仍负责durableeffectintent和真实proof。
- Root协调Git窗口和实际Go依赖closurequiet；不读CLOSED OWN、不碰Sentinel/线上Redis/Kubernetes、不升级/push/deploy。
- 只apply_patch写源码/测试/文档/格式；raw工具输出例外，小步验证提交，一个作者一个完整review。

## Review Focus

1. client-go/HTTP在429/RetryAfter、307/308、断连时实际收到的mutation仍≤1。
2. Pod/policy同名新UID或RV变化、未知mount/foreignpolicy/finalizer都不能误删或宣称terminal。
3. 取消返回后原body或signer仍运行时Close必须join，资源/额度不能提前退款。
4. descriptor和response超界、null/duplicate/未知字段、copyhandle不能扩大分配/对象集合/权限。
5. Prepare/Observe只读、相同logical/runtime名称去重，observations绝不证明物理死亡或ownerrelease。

### Task 1: Concrete exact-resource one-request cleanup

**Files:** create task_runtime_cleanup.go/transport.go/test.go under internal/runtime/kubernetes; modify committed task_runtime_cleanup_records.go only for privateclient/handle state and strict descriptorcodec. Same-prefix新文件可按职责拆分。

**Interfaces:** exact public constructor/Prepare/Close and handle Descriptor/Observe/Apply signatures in Spec1; consume existing TaskRuntimeCleanupOptions/Binding/Step/Observation. 公共字段/enum不得擅改，opaque descriptor格式由本单元内部定义，不把可变API观察冒充权限。

- [ ] **Step1 RED→GREEN boundedclient/descriptor/Close。** 新TestTaskRuntimeCleanupClientProfile/Descriptor/Owners验证nil/copy/config拒绝、strictcaps、真实原IOjoin。constructor不发API，Prepare/Observe只读。GREEN即小提交。
- [ ] **Step2 RED→GREEN exact7effects。** TestTaskRuntimeCleanupClientEffects用实际HTTP/TLS server记录method/path/body/UID+RV；正向七动作、同名去重、replacement/foreignpolicy/finalizer/unknownprofile拒绝。复用strict ownership，不List；GREEN即小提交。
- [ ] **Step3 one-requesttransport。** TestTaskRuntimeCleanupClientNoReplay逐个429+RetryAfter/307/308/brokenconn数实际收到请求，验证MaxRetries(0)/no redirect/HTTP1禁复用；blockedbody与Close实测。保留RED/failedattempt，再GREEN小提交。
- [ ] **Step4 affectedgate/selfreport。** 有限 `^TestTaskRuntimeCleanupClient` race、kubernetesvet、Linuxarm64编译、gofmt-l和scope diffcheck，实际sourceclosure前后quiet/SHA；报告scope/原日志/真实Wait和限制，不称整链路/生产/容量完成。
- [ ] **Step5 solefreshTaskreview。** Root只给本单元commits完整diff和binding spec；同pair限定fix。验收后通知组合作者消费，不重复组件矩阵；组合整链路仍独立验证durableintent/physicalproof/release。

## Root execution ruling

固定API已在6160c36建立；该客户端不消费etcd新authority结果即可独立测试，且原组合作者确认尚未开始Kubernetes文件。抽出独立依赖可并行推进，不增加功能范围，不复用accepted作者/审查，不创建重复实现任务。Root协调共享编译依赖与Git；若接口冲突由Root立即裁决，不让两作者互改对方文件。
