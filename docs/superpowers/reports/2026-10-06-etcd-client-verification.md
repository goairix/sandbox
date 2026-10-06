# etcd 客户端首批验收

日期：2026-10-06。分支：`codex/etcd-state-management`。基线：`feat/workspace-fuse-mount` / `b99b823d69de6baa860eb95d47bd269599b03f42`。

## 交付边界

稳定namespace/256 partition、verified mTLS配置、bounded context、全部endpoint cluster身份验证、线性operator metadata校验、原始identity/restore永久key guard。应用不bootstrap，也未接入allocator。附带可重复三成员fixture和独立foreign cluster，用于真实错误endpoint回归。

## 审查

独立spec review与quality review均通过，本范围无剩余必要P1/P2。已修复TLS回调覆盖静态证书、metadata误挂Lease、跨集群endpoint测试缺口、metadata/namespace无界、TLS配置浅拷贝。补充CA池既有证书对象不可变契约。

## 实际验证

- 命名空间/客户端测试先观察RED，再实现GREEN。
- `bash scripts/test-etcd-state.sh -v`：真实etcd server3.6.15，三distinct members及foreign cluster；全包race通过，`11.064s`，fixture自动清理。客户端所有集成case实际运行；此次全包还包含下一独立提交的stage tests，不将其计入客户端交付边界。
- `go vet ./internal/storage/state/etcd`、`go build -o /tmp/sandbox-etcd-foundation-api ./cmd/sandbox`、`bash -n scripts/test-etcd-state.sh`：通过。
- client3.6.14固定在go.mod，保留Go1.25；server3.6.15固定image digest。没有关闭checksum验证。

fixture为同主机HTTP测试环境；未完成生产mTLS握手、跨主机容灾或容量压测。事务、领域Repository、runtime gate、调度及Redis替代将在独立提交交付。原有Sentinel计划修改未纳入。
