# etcd Exec Service Implementation Plan

> REQUIRED SUB-SKILL: subagent-driven-development。一个完整业务执行 Task，fresh author/reviewer；连续执行，小步提交。

**Goal:** service 真正消费现有可信 exec 下发/续期协议，并由 mounted deployment 提供其 signer。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-exec-service-design.md

**Immutable BASE:** `436bef88405406036fb5c2cb159edd98bc70e151`

## Global Constraints

同一 MaxActive 覆盖所有 service 调用；Create/Lookup20秒不变，Exec 原绝对 timeout+有限余量；原私有 capability/固定目标/下发 fences 不弱化。无 raw 用户 exec 旁路、Replay/Redis fallback、虚构 business End 或 N 常驻对象。挂载 signer owned key、严格 purpose/digest、只读 Open、关闭失败路径完整回收。固定 Go1.25.6；apply_patch 唯一编辑；Root 唯一 Go/Docker/Git；Sentinel禁止。旧 OWN/pairs 全部关闭，不访问、不重放旧矩阵；新 cache `/Users/dysodeng/Library/Caches/sandbox-etcd-go-mod-20261016`。

## Task 1: Mounted issuer and actual bounded Exec consumer

**Candidate files:** internal/provisioning/plain_exec_service.go 及 tests；plain_cold_service.go 准入窄重用、plain_cold_lookup.go 必要只读私有重用；internal/provisioning/deployment/open.go 和新 issuer.go/tests；新 internal/runtime/controlrunner/exec_service_native_linux_test.go 与原 cold/startup fixture 最小扩展。确切 source-grounded 路径由预检冻结，不为计划猜测新通用接口。

**Consumes:** 原 read view/history、BeginOperation/Prepare/Deliver/Wait/Close/RenewExecDelivery/CancelOperationCapability、原签名函数、mounted cert/key、原 fixed pod/profile/TLS bridge。

**Produces:** 实际 streaming-writer Exec service、有限原续期与清理、明确 local terminal/unknown/ref、实际 mounted signer/只读 deployment 接线和 native 证明。

- [ ] Step1 同一作者只读预检 exact DTO/identity/current target/normal terminal 与 unknown、admission/refactor、续期竞态、signer key 生命周期及实际 native fixture；Root裁定后 frozen tests/collector。
- [ ] Step2 新入口最小 RED 后实现；产品按 signer、实际 service 等连贯 slice 验证即提交，不能所有完成才大提交。
- [ ] Step3 targeted host/race/Linuxvet；真实 native 含超20秒执行/原续期、输出/非零退出、gate/cancel/Close/unknown 和所有原 Wait/IO joins。匹配实际GoList来源/当前binary/脚本，Root具体Docker预检。
- [ ] Step4 唯一fresh reviewer审原BASE→HEAD，同pair定点修复；tracked验收关闭OWN，立即继续生产业务路径。

## Root 初始接口检查

| Producer→consumer | 共享接口/文件 | 结论 |
| --- | --- | --- |
| mounted key→Backend Prepare | ExecCommandIssuer/ExecRenewIssuer/Open | 构造不注册；lazy register已存在，必须保持startup只读 |
| current Lookup/history→BeginOperation | profile/identity与capability.Control | 不把DTO当能力；获取原capability后再次核对目标 |
| service admission→long Exec | plainColdCall/Close | 统一active预算；仅期限可分，不能绕原wg/Close |
| RenewExecDelivery→Wait terminal | same original handle | 原renew已续operation；只一个worker，不重复KeepAlive；竞态须明确裁定 |
| Wait receipt→caller result | local_terminal/ref/unknown | local receipt不等businessEnd，Lease撤销不等排空 |
| Task1内部 | tests/product/fixture | 一次交付可实际执行的consumer，非孤立signer；长执行native是必需的期限/续期证据 |

Ruling: 用户已批准总方案并要求连续加速；上述为已有协议的实际消费，不重开Redis/etcd选型或请求重复授权。已解析ExecutionRequest避免本层猜镜像环境；HTTP shell/default映射后续单独接线。若错误，成本为adapter接口调整，不影响已有wire或权限协议。
