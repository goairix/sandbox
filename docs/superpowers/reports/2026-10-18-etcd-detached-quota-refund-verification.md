# Detached 配额归还组件验收

主分支代码提交：`e01e233`；辅助工作树原提交：`49802d0`。本片接通真实 v2 owner
历史到原配额返还协议，包含公共与 paid worker 的准备、journal、提交和恢复消费者。
旧 local v1 字节、事务结构和容量限制保持不变；没有通过将 detached 转成 local
证明来绕过认证。唯一组件审查的 Spec/Quality 均通过。

## 已验证的可用行为

- 真实三成员 TLS etcd、实际 PID1/运行时终止及 owner 释放之后，七个 paid 历史阶段
  产生可用 Done，三个缓存读取保持相同历史对象、上下文、来源和额度。
- 原配额返还 journal、Stage Grant/guard/Commit/Revoke 和 Postread 完成后，可通过
  原 Acquire/CreationClaim/Reserve 重用配额，epoch 加一；旧返还结果重放不影响新 owner。
- 公共42节点返还事务真实提交后丢失响应，返回 Unknown；原 lease 的实际 Revoke
  返回之后，同一 reference 的公共重放和 paid 恢复识别不可变已提交结果，不再提交返还。
- 原生合法准备结果的两个篡改副本被拒绝；不将伪造签名 fixture 当作 v2 正向。

三个实际 Linux 场景分别为 `run-6baedbc2b8e04745b5bf352d2a29982d`（返还/重用）、
`run-18364cd01dc04d6cb4eb5f72845287af`（真实提交后丢响应）和
`run-7c2aa43c3a5c450095013373806b80d2`（七阶段及缓存 Done）。均 exit0、无 OOM，
原工作进程、PID1 和三个 etcd 子进程完成 Wait，精确归属的容器已删除。
各场景完整9493个源码/工具输入核对通过。保留原始日志中的正常停服告警。

返还和丢响应场景的保留对象图峰值分别为96687字节/41键和72518字节/38键，
均含13键 reserve。这些仅是组件 fixture 的实测，不构成32并发、大量已有沙盒、
吞吐、容量或生产性能验收。公共 wrapper 的调用计数不能说明底层传输重试次数。

## 验证与集成

辅助工作树定向 host89组通过；补充 profile 反例后 race161组通过，Linux vet 和
state/controlrunner 构建通过。主分支重新执行定向 host、两项 Linux 构建并完成
全部输入核对；9284/9489个构建输入、环境与工具分别和原生已运行版本双向等值。
保留的 go.mod 注释/分组差异已按161项依赖和其他指令语义核对，go.sum 相同。
主分支没有重复声称重新执行三个原生场景，而是复用严格等值的实际执行证据。

原始命令、日志、完整输入、源码快照及唯一审查记录保存在辅助工作树
`.superpowers/sdd/2026-10-18-etcd-detached-runtime-native/`；主分支等值记录为
`.superpowers/sdd/2026-10-18-etcd-detached-cleanup/detached-quota-refund-native-input-equivalence.json`。

## 仍未交付

自动发现 v2 返还候选和最终 P 删除门禁尚未开放；managed Claim、完整 phase
准入、自动销毁消费者及 HTTP detached destroy 仍未接完。默认入口仍使用 Redis，
生产切换为0%。本片不能描述为自动清理或生产迁移完成。
