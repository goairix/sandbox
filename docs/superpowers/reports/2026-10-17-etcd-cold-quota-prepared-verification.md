# 冷运行时配额返还：Grant 前准备原语

本片将原 `SettleColdRuntimeQuota` 接到同一私有 prepare/continue 原语：原历史认证与
已完成返还的 replay 检查保持原顺序，准备阶段冻结 copied mutation 和 exact Stage
reference，不发送 Grant、guard 或业务写入。continue 绑定同一个 Backend、登记中
owner 与 context，一次消费后仅用原 locator，错误不能开启第二次 Grant。

原 34 compare / 4 success / 4 failure 事务、ReleaseStage 和返还记录／回执后读保持；
unknown 后只能仲裁原 reference。已提交 replay 在读取可复用的 quota slot 前返回。
公共签名不变，没有新增导出权限或修改通用 Stage、owner-release、manifest、E/S/P。

## 定向验证

selector `^TestColdQuotaPrepared.*$` 的六组测试使用原 bounded gRPC 模型，检查 owner
拒绝、复制／外来绑定／取消／重复消费拒绝、8 个并发调用最多一次 Grant、原 attempt
一致、Grant/guard/commit/postread 未知结果保留原引用，以及原引用先 aborted 后不能退款。
这些模型向私有 continuation 提供 codec history，不证明完整历史认证或物理释放。

- `quota-prepared-red-01`：仅缺批准私有 API 的编译 RED，未执行行为断言。
- `quota-prepared-host-01`：首轮仅 postread 子例失败；新 matcher 错把含 identity/epoch
  的三点读取当成单点，故未注入。修正实际两分支三点形状，并要求恰好命中一次。
- `quota-prepared-host-02`、`quota-prepared-race-01`：全部通过，无 SKIP 或 DATA RACE。
- `quota-prepared-vet-01`：Linux arm64 vet 通过。

Root 独立核对每轮实际 17 类 Go 输入、前后摘要、source/tool 全归档及当前字节。
最终 host/race/vet 输入分别 9074/9079/9167；host 原始日志 2610 字节，SHA-256
`5078c550077641a1b3207f91c017bfb0c3fdc1c4215ae7e6514c552675bab817`；race 同为
2610 字节，SHA-256 `437459ede6052266d46763659f5bce97861b5f373c6ff3e4b4136ab9e25aaae5`。

人工检查了注释、阶段分隔、consume/Close 所有权和包边界：私有准备能力与原 quota
事务保持同包，不扩大接口。本片提供持久化 original reference 的插入点，尚未接入
自动 worker，也没有 durable quota checkpoint。真实认证成功、旧 slot 复用后的 replay
和自动配额归还仍待实际设施验收，不能把本片算作生产配额闭环完成。

独立 Task 审查：Spec Compliance / Code Quality 均 PASS，无新增 C/I/M；明确仅批准
本片私有原语与公共 API 等价接线，不代表真实 prepare 或自动配额闭环已验收。
