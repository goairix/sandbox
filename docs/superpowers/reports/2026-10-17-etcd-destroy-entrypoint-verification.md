# etcd DELETE 生产入口阶段验证

基线为服务首片 `222f79d33753cdd337eeb59d73732e43013b9d69`；该提交仅新增 service，
不代表完整 HTTP 或物理清理验收。本报告记录后续 profile 限制与 gateway 接线的局部验证。
主方案为 `2026-10-06-etcd-state-management-design.md`，AGENTS 注释、分段、包职责均适用。

## 行为与边界

- 新 DELETE `/api/v1/sandboxes/:id` 沿原固定 operator-wide API key、UUID、速率和并发准入。
- 元数据、原始 capsule/profile、retained history、runtime UID/generation 仍经原加载器核对。
- 从同一次 ReadSandboxSnapshot 保留 owned snapshot；仅 v2 有效 local descriptor 可准入。
  v1、当前 HTTP detached CreateBusiness 目标及无支持消费者的 profile 返回501
  `unsupported_cleanup_profile`；在 Prepare/Grant/commit 之前拒绝，不关闭准入、不写 Task/P。
- 支持的过期 ACTIVE 目标可手动销毁；不调用 Lookup、健康查询或 TTL gate。
- exact ControlRevision 交给既有 PrepareDestroy；一次 Prepare/Commit，至多一次原 Stage
  Resolve。关闭准入、永久 Task/intent/link/checkpoint/pending 与 committed receipt 原子提交。
- 202 `destroy_pending` 仅表示入队，含原 sandbox/request/generation/runtime/Task/Stage 诊断；
  不表示物理终止、归档结算或 owner/quota 归还，与 legacy 同步200成功语义明确区分。
- notfound404，already-closing/并发替换409；未知提交或原 Stage 清理失败503
  `outcome_unknown` 保留原归因，不能自动换 ID/attempt 重试；RequestID 不提供持久幂等。
- Idempotency-Key 明确拒绝；X-Request-ID 可省略生成，没有 destroy 查询索引。
- 原 service admission 覆盖同步5秒独立 context 的 Stage release；Close 等其实际返回。
  未新增控制器、worker、租约恢复 goroutine、权限 DTO 或存储协议。
- 独立自动 worker 必须覆盖相同 namespace/restore epoch/partition/local storage；HTTP 不启动它。

## 已执行验证

每次 Go action 均使用本 OWN collector、Go1.25.6、独立缓存及 GOMAXPROCS=2；下一次执行
或源码变更前，完整核对17类输入、前后 manifest、源码/工具逐 archive member 和当前文件。

| epoch | 结果及完整输入审计 |
| --- | --- |
| service-red-01 | 缺方法/类型的预期 compile RED；9043 inputs PASS |
| service-host-01 / service-race-01 | 首片 admission/身份/具体 Backend 取消 join PASS；9044/9049 |
| service-vet-01 | 首片 Linux arm64 vet PASS，空日志；9133 |
| profile-red-01 | 缺 profile guard/error 的预期 compile RED；9044 |
| profile-host-01 | service 四组及 profile 七子例 PASS；9044 |
| gateway-red-01 | 实际路由仍501的预期 RED；9089 |
| gateway-host-01 / gateway-race-01 | 新 DELETE 四组及旧 AuthStrict 回归 PASS；9090/9095 |
| profile-race-01 | 新 service 六组含 Lookup 两个边界回归 PASS；9049 |
| profile-vet-01 / gateway-vet-01 | Linux arm64 vet PASS，空日志；9133/9179 |

预期取消导致的原 etcd-client Canceled warning 完整保留；未静音、未伪造无告警。
旧 unsupported 测试仅把 DELETE 探针换为仍未实现 PUT，其他旧断言不变。
Root 最终独立审计确认 profile-race/vet 与 gateway-race/vet 的全部输入及当前字节匹配。
gateway-host-01 是接口 GoDoc 补充前的历史记录；Root 对归档/current 的比较仅有两行
注释差异，不将该历史 epoch 冒充最终源码。最终 gateway race 覆盖同一组行为测试。
新增/修改 Go 文件 gofmt-d 无差异；状态逻辑在 provisioning，HTTP 映射在 etcdgateway，
未修改 etcd/core、执行器、签名、索引或旧 Create/Lookup/Exec 行为。

## 尚待 Root 集中验收

本作者未执行 Docker/native，也未修改 controlrunner/native helper。实际正向须使用
真实 service.Create(local) ->HTTP DELETE202 ->原自动 worker ->物理释放及 pending 删除；
HTTP detached 创建须验证 DELETE501 且 control/Task/P/Stage 无变化。已有 cleanup fixture
的任意测试 payload 不满足 service 原 capsule/history 校验，不能替代真实创建。
还需原 commit 丢 ACK/精确 receipt、并发 closing/替换、过期目标、Stage release 与 Close
实际 join。局部 host/race/vet 不构成完整 Task 审查、大 N 容量、生产部署或后端切换完成。
