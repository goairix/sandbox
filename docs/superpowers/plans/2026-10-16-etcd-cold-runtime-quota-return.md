# 冷建 runtime quota 安全归还实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: subagent-driven-development。一个完整可独立验收 Task，多个已验证小提交，随后一个 fresh Task review；连续推进总迁移。

**Goal:** 原 runtime 的真实安全终止、provider cleanup、owner release 已认证后，精确一次归还其冷建 runtime quota。

**Architecture:** 复用 Backend 私有认证历史，增加独立 quota-return 记录与 fresh Stage；保持原64节点 owner release 不变。重放认证原结算 receipt，不依赖已复用 slot 的当前归属。

**Tech Stack:** Go1.25.6、现有 etcd v3 协议、原 Linux native fixture。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-cold-runtime-quota-return-design.md

## Global Constraints

- 前置 cold Create/Resume Task 验收后才设置 immutable BASE/START-GO；此前仅 Root 只读和文档准备。
- 原64节点 release 不改；locator/DTO/404/失租/取消/超时都不是退款权限。
- 原 signed termination/provider cleanup/release 与 publication/reservation 全归属认证，prep 先结算；不改 settled prep/work/receipt，不影响新 owner 或新 slot epoch。
- 归还只走 fresh Stage，pre-Grant exact ref；固定点读与完整≤64节点 protobuf，无按 N map/ticker/Lease/扫描。
- Go1.25.6/apply_patch/Root-only 精确小提交；唯一 fresh author+reviewer、同 pair 修复，无 nested/旧 OWN/Sentinel/外部部署或依赖升级。
- 每次 native 单独 Root preflight；新 collector/独立 epoch actual17/prepost/archive/ELF/script；原 Wait/Serve/IO/transport/container 实际 join/exact cleanup。

## Review Focus

- return 重放时 quota 已复用：不能再写旧 free index 或新 owner 的 slot。
- release 已成功但 ACK 丢失：只读认证原结果，不重发 cleanup。
- guard/commit unknown：首次 Grant 前保留 exact StageReference，未确认前不报归还成功。
- 旧 workspace 已新 owner 或 prep 复用：无需当前 owner，不能改其记录。
- receipt/history 篡改或跨 restore/Node/UID：保守拒绝并保持原预算。

## Task 1: Authenticated runtime quota return

**Files:** 新 `internal/storage/state/etcd/preparation_quota_return.go`（提交/恢复）、`preparation_quota_return_records.go`（wire/键/归属）、`preparation_quota_return_native_linux_test.go`（固定 metadata/fault case）；按需修改现有 namespace 源文件。新 `internal/runtime/controlrunner/cold_quota_return_native_linux_test.go` 和 `scripts/test-cold-quota-return-native.sh`，可对 tracked local_runtime_release fixture 增加 default-false 的冷建路径，旧 selector 不变。

**Consumes:** `WorkspaceIdentity`、原 intent ID、`TaskReference`、私有 `readLocalOwnerRelease`/`readLocalRuntimeHistory`、原 `loadColdPublicationHistory`/`verifyColdSettlement`、`BeginStage/CommitStage/ResolveStage`。公开 TaskOwnerReleaseEntry 不作权限。

**Produces:** `SettleColdRuntimeQuota(context.Context, WorkspaceIdentity, string, TaskReference) (ColdRuntimeQuotaReturnResult, error)`；结果含 `Outcome`、`Reference StageReference`、typed只读Entry。精确新增记录/entry字段由 Step1 基于实际接口固定，不引入任意factory。

- [ ] Step1 只读预检：列出完整 runtime/TaskBirth/workspace/generation/Node/UID/BootID 归属链、原不可变 release/termination/settlement pins、record及键、读取与42节点预算；确定实际原 safe-release fixture 的可复用源码接点。Root裁决后才 START-GO。
- [ ] Step2 写新 metadata selector 的缺证/篡改/跨作用域 RED；实现 canonical 记录和独立结算、pre-Grant ref、settled replay。用真实 TLS etcd 两Backend 验证 guard/commit ACK 丢失、原release ACK丢失只读恢复、quota/prep复用、新workspace owner及实际预算。测试只声称其真正覆盖的元数据范围。
- [ ] Step3 对新真实 native selector 写 RED→GREEN：初始化并占原双槽，原 publication/prep settlement，再走原真实 archive/fullsync/observer终止/providercleanup/ownerrelease，最后 quota结算及复用重放；所有原 owner 实际join/exact清理。不重跑旧矩阵。构建/运行均有新collector和Root具体检查。
- [ ] Step4 完成限定 host/race/Linux vet 及 final来源冻结；达到提交条件立即按连贯范围提交，不攒一个最终大提交。新native成功后仅因有新修改/缺口才扩大测试。
- [ ] Step5 wholeTask报告/新鲜单reviewer spec+quality审查，同pair修复及限定复审；tracked验收/更新plan/关闭本OWN，接续总迁移，不停在本Task。

## Preflight relationships

| Producer → consumer | Required check |
| --- | --- |
| publication/reservation → quota return | 完整原 intent/workspace/runtime/slot epoch 绑定，不信当前owner。 |
| prep settlement → quota return | 原settled receipt/record认证，prep已复用也不读写其slot。 |
| original safe release → quota return | 认证原终止签名/cleanup/TaskBirth，独立不可变release receipt提交pin。 |
| return record → replay | 原Stage receipt、canonical版本/revisions/完整归属，slot已复用也不再归还。 |
| unknown reference → recovery | exact Stage仲裁，保留原预算及原task寻址，不重新清理或创建。 |

Ruling: 本单元是已批准总迁移的配额生命周期收尾，用户持续授权和执行方法沿用；不重复请求设计批准。Step1核对原39候选漏掉仍可变work marker，Root接受增加其Mod/Value/Lease三比较，固定完整42节点=34compares+4success+4failure；不改原64节点release，不删认证或提升限制。前置Task已验收956536e，当前作者只读预检，须完整接口裁决后START-GO。
