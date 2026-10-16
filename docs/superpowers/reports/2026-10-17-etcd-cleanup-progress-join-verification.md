# Cleanup 原任务未知进度与退出标记验证

本片只修复仍注册的原 cleanup job：进度事务已提交但 ACK 丢失后，保存唯一实际提交值及原 E/S 完整凭据，在实际工作退出后做一次精确重读，再执行原 joined CAS。复制出来的 job、槽 ABA、非预期进度都拒绝。没有新增公共权限、物理重发或 cold recovery。

产品范围为 `due_worker_cleanup.go`、`task_cleanup_local_lifetime.go`、`task_cleanup_slot.go` 和新增 `task_cleanup_local_progress.go`；新增一个 host 测试文件。原 native point envelope 只做私有提取复用；新读与既有 E/S 两点请求同形，仍检查真实 protobuf 大小。持久槽及 job 生命周期共享内部不变量，保留在 etcd 包，避免导出内部权限。已检查复杂逻辑注释、逻辑分段和包职责，gofmt/diff-check 通过。

## 验证

- 修复前 `progress-join-red-01`：真实 etcd 上原 job 因旧修订号发生 join 冲突，非注册副本却错误写入；ABA 与非预期值拒绝通过。日志 SHA256 `23f9088d56bbe82d4dac90d46804cfbd12287a0a65407b3cdcee10f1871638d5`。
- `progress-join-host-01`：45 个测试组、92 个子场景通过，70.943s；9013 个输入。日志 SHA256 `e48a2788daa4c0613c57f2f3c643d4339a509498ac955ccc65d709fcd93f03fc`。3 条 WARN 为原取消 Grant、取消 Txn 和 LeaseNotFound。
- `progress-join-race-01`：相同 45/92 通过，74.636s，无数据竞争；9018 个输入。日志 SHA256 `400174c0f9214abc8905b8469eea42c2f9aed8c553ab0517b24d80515f562f73`。7 条 WARN 为 6 次取消及一次 LeaseNotFound。
- Linux arm64/CGO0 `progress-join-vet-01` 通过，9104 个输入；新 runner `progress-join-build-01` 通过，9269 个输入，ELF64 AArch64 SHA256 `179c9ced16cffff607d1c0bbd18ee84ad9fef84db9bfad0a2b609d01ba9b806e`。
- 每个 Go epoch 的全部 17 类输入、源码/工具清单、前后及当前 SHA、每个归档成员均在后续 Go/源码修改前独立核对。monitor 不依赖本片 etcd 文件，native 测试未再变化；`recovery-monitor-red-02` 的全部 2041 个当前输入及 ELF/buildinfo 重新核对通过后复用，未用过期源码凭据替代验证。
- `progress-join-complete-01/complete` 真实 Linux 成功链通过，5.83s：两次不同 TaskClaim、归档内容及目录/文件模式、终止、安全释放、重复查询及原 worker/Backend 实际退出；原 due 在清理期间推进 65 条记录，provider 1 PATCH/1 DELETE。原 worker Stop=nil，父容器 attach/Wait 均为 0。日志 5852B，SHA256 `4680dac15290ff79fe6ca0f4bbec367d109b484609ea75d2ebdd10055d9204ea`。
- `progress-join-bound-01` 是未完成恢复功能的诊断，整例仍 FAIL：真实归档绑定事务 rev85 成功后扣 ACK 至 deadline，原 worker 现在确认 archive-bound、写入 Run.Joined=true 并实际 Wait=nil；第二个新 worker 仍返回 retained-run。日志 12203B，SHA256 `b6897a8e42c05a4ae61aa39ff063e2d5a9a689efca738c1b3d4ff6e48d2f0312`。不能将此前置通过描述为 bound recovery 完成。
- 两轮 native 的固定运行参数、工具归档、二进制/etcd 输入、实际退出、无 OOM、确切 CID 删除及空标签清单独立核对通过。complete 有 7 WARN/3 ERROR，bound 有 9 WARN/3 ERROR；ERROR 均为 fixture etcd 关闭监听，原始日志保留。后续恢复 native 三个测试文件尚未纳入本片提交，完整源码凭据保存在当前 Task 的验证归档。
- 三节点 host fixture recovery-01 实际 Wait=[0,0,0]，无 OOM，容器及专用网络删除、空清单、实际 Compose/工具与生产者归档及各 host epoch 环境均独立核对。各成员日志 WARN 为 21/23/19、ERROR 为 5/4/4；包含取消的 Range 及关闭监听，未宣称零告警。

## 边界

Host 测试仅证明元数据收尾，真实物理证据来自 Linux 测试。首次 Run 写未知且尚未观察到 Run、解析或 marker 自身未知均继续保留占用；cold worker 不能代写原 Run 的退出标记。冷启动已退出任务的恢复、释放后的永久历史结算、observer ACK 恢复、跨实例 33 条真实 Task、close-join、部署 loader 和完整 Task 审查仍未完成。此片不代表容量验收、生产接线、默认切换或 Redis 移除完成。
