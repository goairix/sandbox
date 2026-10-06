# etcd 第一批基础交付最终验收

日期：2026-10-06。分支`codex/etcd-state-management`，从`feat/workspace-fuse-mount`的`b99b823d69de6baa860eb95d47bd269599b03f42`切出。按照可验证边界提交设计、客户端、事务协议、故障测试，未积累完整Redis替代为一个大提交。

## 审查结论

最后完整设计review无必要P1/P2；客户端spec/quality及stage spec/quality先审查后复核，阻断项均修复，目前本批无剩余必要P1/P2。

- 客户端：metadata必须永久；每个endpoint验证cluster；静态证书不能由callback覆盖；TLS数据独立复制；metadata/namespace/restore大小及字符有界。
- 事务：receipt预编码与预算；原guard Lease不可重建；永久committed/aborted marker同key仲裁；业务CAS冲突与RPC错误不猜测aborted；RangeEnd1024上限在复制前验证。
- 故障测试：原NOSPACE可影响任意配置cluster的P1已闭合。NOSPACE及leader fault在任何bootstrap/故障前要求脚本fixture声明；检查全部实际container ID、project/service labels、running、unique及127.0.0.1 published endpoint映射。Docker子进程10秒deadline，Alarm请求10秒、cleanup5秒。

## 实际执行

最终`bash scripts/test-etcd-state.sh -v`退出0，`go test -race -count=1 ./internal/storage/state/etcd`通过（9.262s），所有集成case实际运行，无skip。server3.6.15，三个不同member同cluster，另有独立foreign cluster。fixture脚本退出已清理自己创建的容器、卷和网络。

关键故障证据：

- 真client完整Txn在发往server前暂停，resolver先写aborted；放行后业务key仍不存在。
- 真commit完成后丢响应，调用返回unknown，resolver读到永久committed；不重写业务状态。
- guard创建完成后丢响应，Begin返回nil能力与unknown；只回收原Lease。
- guard自然到期后已提交receipt仍可恢复；未提交旧guard到期后不能提交或重建。
- 16次commit/abort并发竞争每次仅一最终结果，业务存在性与receipt一致。
- 真实NOSPACE alarm下Commit/Resolve保持unknown；解除alarm后通过持久仲裁恢复。
- docker pause实际leader，剩余两成员选出另一leader后Commit/Resolve成功；unpause并确认恢复后才继续测试。此项验证leader停滞/选举，不等于进程崩溃或跨主机容灾测试。

额外检查：`go test ./...`退出0；`go vet ./internal/storage/state/etcd`、sandbox构建、脚本`bash -n`与提交diff检查通过。普通全仓测试未配置etcd时集成case会显式跳过；真实协议通过依据上述独立fixture运行。设置不可连接endpoints但缺少fixture声明时，两项全局fault测试0.00秒跳过，证明不会先联网或初始化。

边界测试先RED再GREEN，覆盖超长identity/namespace、TLS浅复制、RangeEnd1025及oversized receipt。源码使用固定client3.6.14、server3.6.15固定digest，没有将server引入生产Go依赖。

## 尚未交付的主方案阶段

本批是阶段一第一部分，不是完整Redis替代。未接入生产allocator，现有Redis流程继续运行。接下来分别交付workspace/sandbox领域Repository、request/operation/pool、runtime launcher及外部效果fence、partition/due/dirty调度、collector/sync、bootstrap/部署/drain/Redis删除。回执GC必须保留防迟到证明；完整恢复、生产mTLS、跨主机故障和大存量容量压测均待后续阶段。多cell留在阶段六。

用户原有Sentinel兼容计划修改完整保留，未纳入这些提交。
