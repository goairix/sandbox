# etcd foundation + domain acquisition 整体分支审查

结论：PASS；在本次提交范围内未发现需要修复的新或遗漏 P1/P2。

审查范围：`b99b823d69de6baa860eb95d47bd269599b03f42..0bbfead`，覆盖 namespace/client/identity、永久 mutation 与预算、原始 stage guard/receipt 仲裁、领域模型/codec/key、point reads 与 AcquireIntent、fixture/fault tests。依据完整设计及 foundation/domain acquisition 两份实施计划，并核对各验证报告。审查期间 HEAD 前进到 `957357c`，该额外提交只有 creation claim 计划；并行新增的 creation_claim 文件未纳入本结论。没有修改仓库文件或提交，原 Sentinel 文档改动未触碰。

## 关键核验

- New 不初始化 operator metadata；启动逐 endpoint 核验 cluster，身份与 restore 必须存在且永久。事务 base comparisons 使用原始 metadata 字节与 Lease=0。
- mutation 只允许 namespace 内单 key 永久写，拒绝 meta/attempt/stage 协议 key、重复写、超额 operations/bytes；comparison oneof 与 slice 被复制，caller 修改不会改变已固定摘要。
- CommitStage 比较原 guard value、Lease、CreateRevision、restore 与 receipt absence。Commit/Resolve 竞争同一永久 receipt；CAS conflict/transport unknown 不误报 abort。已到期 guard 没有公开重建能力；Reference 只有仲裁能力，不能恢复提交能力。
- AcquireIntent 把 request、owner、fence、control、snapshot、intent、全局 placement 和 committed receipt 放在同一事务。所有新 key 比较 absence，旧 fence 比较 revision/raw value/Lease0；generation overflow 拒绝。业务 TTL 不改变原 stage 的 30 秒 Lease。
- 相同 workspace 与跨 partition 同 sandboxID 竞争分别由 owner/global placement CAS 排他；相同 principal/key 的 request 不依赖 workspace partition。重放校验 configurationDigest 与 workspaceHash，不重新写原 TTL/IDs。
- 读取使用 identity-fenced 线性 point Txn，领域 KV 精确 key、Lease、schema、restore 与规定关联均校验；owner/fence 同事务匹配 generation，control 关联 placement，snapshot 精确匹配版本与摘要。read/AcquireStage committed 不授予 runtime 操作能力。
- NOSPACE/leader 故障在执行前验证 disposable fixture 的 project、三个容器 ID/service、运行状态、唯一性与 loopback published endpoints；手动 fixture 不具备这些声明时跳过 global faults。

## 特别检查但未升级为缺陷的边界

`stage.go:79-98` 没有单独核验 Grant response header，而后续 guard Txn 核验身份比较和实际 cluster header；不符时不返回 Stage。未发现该遗漏在正常 etcd client/server 契约及当前隔离恢复前提下可产生错误业务提交或复活失效能力的路径。

`receipt.go:114-123` 的旧共享 metadata response validator 比 `domain_read.go` 少 nil-KV/exact-key 防御，部分 stage 成功回复也未全面校验响应形状。可通过人为返回不符合 point RPC 契约的 fake response 触发差异，但没有找到真实 etcd 响应、持久记录内容或公开输入能触发错误授权的路径，因此不把 mock-only 异常机械列为 P2。若今后把不可信代理/自定义 KV 实现纳入支持契约，可统一响应校验作进一步加固。

## 本次独立验证

实际执行：

```sh
TEST_ETCD_ENDPOINTS=http://127.0.0.1:63849,http://127.0.0.1:63848,http://127.0.0.1:63847 go test -race ./internal/storage/state/etcd -run '^(TestDomain|TestAcquireIntent|TestMutation|TestStageCommit|TestStageAbort|TestStageCAS|TestStageReference|TestStageRequestDelayed|TestStageLost)' -count=1
```

退出 0，`ok .../internal/storage/state/etcd 4.375s`。只运行 namespace 范围的领域/并发/unknown/CAS 测试及纯校验测试；未运行手动 fixture 的任何 global fault。`git diff --check b99b823d69de6baa860eb95d47bd269599b03f42 0bbfead` 退出 0。

父线程提供的独立 fresh 三成员 server3.6.15 全套 race 10.403s、无 skip/global faults 验证、全仓/vet/build 结果已与验证报告核对；这些全量检查没有在本审查重复执行。

本结论仅覆盖已交付 foundation 与元数据 acquisition。不把尚未接 runtime/Publish/GC、尚未切换生产或尚未移除 Redis 当成本批缺陷；也不将本结论解释成这些后续阶段已完成。
