# etcd 原子申请与领域读取验证

日期：2026-10-06。分支：`codex/etcd-state-management`。模型提交 `9cdcefa` 之后的独立交付。

## 已交付

`AcquireIntent` 在固定 7 个 point key 上线性读取，并通过原始 Stage guard 将 request、owner、fence、intent、publishing control、immutable snapshot、全局 sandbox placement 与 committed receipt 原子提交。永久领域记录不附 Lease；generation 递增且溢出拒绝。业务 TTL 与固定 30 秒 stage Lease 分开。

公开 point read 验证同一事务中的 identity/restore、返回 key、schema、永久 Lease 与关联字段。幂等重放保留原 ID、期限与配置；不同摘要冲突。未知提交保留 exact reference，resolver 只裁决该元数据尝试，不将 pending request 当 runtime 已完成。

## 验证与审查

- TDD 首轮真实 RED：API stub 未实现。另有 nil metadata KV 回归 RED，读取入口现已 nil-safe 且校验精确 metadata key。
- 规格审查发现 P2：同摘要 request 重放没有验证 workspace hash 关联。真实三成员探针复现；新增回归先 RED，修复后定向 race GREEN 3.191s；规格复审 PASS，原探针现返回 `ErrCorruptRecord`。
- 独立质量审查 PASS，无待修复 P1/P2；独立新 fixture 的 24 个顶层定向 race 测试全部通过，3.946s，无 skip/race。
- 修复后父线程 `bash scripts/test-etcd-state.sh -v`：exit0，race 10.403s，无 skip/FAIL/DATA RACE；fixture 为真实三成员 etcd3.6.15，含 NOSPACE、leader 切换、异集群、未知回复与迟到事务仲裁，脚本清理仅自己的资源。
- `go test ./...`、`go vet ./internal/storage/state/etcd`、`go build -o /tmp/sandbox-etcd-domain-api ./cmd/sandbox`、脚本语法、gofmt 与 diff 检查均通过。全仓测试中需要另外配置外部环境的测试沿用原有机制；以上 etcd fixture 测试没有跳过。
- 新用例覆盖同 workspace 16 并发、跨 partition 同 sandbox ID 并发、幂等冲突/重放、permanent generation/overflow、lost commit reply、先 abort 后迟到完整 Txn、损坏/leased/错 key/错 cluster/restore、caller payload mutation、输入与 wire budget、cleanup 错误与业务结果分开，以及 1ms/48h/365d TTL。

## 交付界限

本批只交付永久记录与创建申请元数据。取得 committed AcquireStage 不授予 runtime 能力；尚未接生产 Manager、创建 claim、runtime dispatch/Publish、operation gate、scheduler/GC，也未切换运行后端或移除 Redis。

固定点读不扫描已有沙盒 N；这不是容量或性能提升幅度的实测结论。存量规模、历史记录/receipt、数据库放大、Watch/relist 与 runtime 资源预算仍须后续真实容量验证。
