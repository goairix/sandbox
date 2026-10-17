# Detached plain sandbox 自动销毁

这是已批准主方案 `2026-10-06-etcd-state-management-design.md` 中“明确无对象存储
effect 证据”的实施细化。完整迁移范围、默认 Redis 入口及最终切换验收不变；
不重新执行已验收的 local cleanup、自动 quota 和 v3 native 单元。

## 用户结果与来源

HTTP CreateBusiness 的 detached sandbox 可提交销毁，由原 DueWorker 自动完成
关闭 admission、持久关闭 runtime gate、双层排空、精确终止、provider 清理、owner
释放、cold quota 归还与 P 结算，最后旧 ID 查询为404。取消及未知结果仍保留原
Task/Run/attempt，不把 token 消失或记录缺失当作外部效果已经终止。

现有 business capsule v2 对应 storage snapshot v1，没有 LocalCleanupDescriptor。
它使用固定 enrolled plain Pod 的有界 EmptyDir，不应伪造 local 目录、空 archive
或把无 descriptor 当作无 effect。原 LocalCleanup v1 authority 保持严格。

## 选定边界

1. 增加显式 `DetachedCleanupOptions{PinnedPlainImage string}`。构造器调用
   `internal/kubecontract.EnrolledPlainContractDigest` 得到私有预期 digest，并匹配
   EnrolledPublication 配置。空值、tag-only image、外来 contract 在首个 RPC 前拒绝。
   该选项本身不启用 consumer、不赋予终止或 no-effect 权限。
2. 从原 CreationIntent.RequestHash 与 Backend 不可变 StorageID 重建 detached
   identity，精确绑定 Task、snapshot、intent、namespace/restore、partition、
   generation、runtime 和 gate。签名 publication/enrollment 的历史认证仍独立必需；
   普通身份值、复制的 publication DTO 和分类结果均不能授予 effect 权限。
3. 配额、终止和终态 reader 使用同一来源约束；不能为读取旧 quota 而重建已删除的
   owner/control/runtime-index。配置 pinned image 避免 etcd 导入 provisioning 或
   临时解析 opaque dispatch JSON。历史 dispatch image 的存在不免除原 envelope 认证。
4. 双层排空后持久写入独立 no-effect settlement，绑定原 Task birth、Claim、Run、
   plain contract、原 final-sync none 计划及 quiescence/source fences。none 计划只是
   计划，不是 settlement。新记录及其原 attempt 遵循原 Unknown/仲裁规则。
5. 终止 intent 新增显式 version2 detached variant。保留 version1 的原 canonical
   字节和全部 local stamps。version2 的 Archive/LocalComplete 必须为精确零值，
   必须另有非空 detached effect bindings；禁止混合两个 profile。
   原 ticket 继续通过完整 IntentDigest 绑定新 intent，无需改变 ticket v1 格式。
6. 新 variant 复用原 occupation、Kernel、Run、续 Claim、observer/provider 与实际
   join。只跳过经证据证明不存在的 workspace/archive effect，不跳过 close/drain、
   runtime termination、provider cleanup、原 owner-release CAS 或 quota/P gate。
7. 最后才接显式部署配置和 HTTP admission。消费者没有完成前保持现有501拒绝，
   不以返回202代替实际消费。最终 native 必须从真实 HTTP business 创建开始。

## 第一批固定接口与兼容性

来源分类留在 etcd 包的 `task_cleanup_detached_profile.go`，配置复制/校验与身份
重建分职责组织。Backend 新字段持有不可变私有配置，不增加跨包可变 registry。
`detachedCleanupSource` 返回普通 WorkspaceIdentity；它不是 absence-of-effect proof。
实际 no-effect writer 必须重新核验原 Task/Claim/Run 和所有已认证持久来源。

协议新增 `DetachedRuntimeEffectBindings`：Version=1、
Profile=`detached_enrolled_plain`、ContractDigest、Settlement RecordStamp。
在 `TaskRuntimeTerminationIntent` 上新增可选指针字段
`DetachedEffects *DetachedRuntimeEffectBindings`，JSON 为 `detached_effects,omitempty`。
不要向可比较的 TerminationAuthorityBindings 塞入指针，也不要改变旧 authority 相等
比较。v1 必须 nil；v2 必须完整有效，MountAttempt=0；其余13个原 stamp 和原
quiescence stage 仍完整有效。这个 wire tuple 本身不授予权限，native reader 还必须
验证原 no-effect KV、其 Task/Claim/Run 来源及所有 runtime/contract 绑定。

## 验收与界限

- 首先用真实 acquire/bind/mount/enrolled publish 生成 detached 元数据，测试错
  request hash、StorageID、Task/snapshot/intent、contract、runtime/generation/gate
  及 FUSE/local profile 拒绝；禁止用字段替换伪造正向历史。
- codec 验证旧 v1 canonical 字节不变、新 v2 往返、缺 proof/混合 profile/非法版本/
  非法 digest/stamp/非规范字节拒绝；原 ticket 精确 pin 新完整 intent digest。
- 每个持久 writer 冻结原事务、wire/RPC/预算形状；不得扩大64节点、128KiB请求、
  256KiB回复、32永久任务、64MiB/10000keys或新增 per-sandbox goroutine/lease。
- 最终真实 Linux：HTTP CreateBusiness→DELETE202→自动发现→原 PID1/cgroup 终止→
  provider 清理→owner/control/index 删除→quota receipt→P 删除→GET404。
  原请求 Unknown/Query、取消和替代 Run 的恢复另需实际验证。
- 组件 host/race 或单 Task native 不代表大 N 容量、全故障恢复或生产迁移完成。
