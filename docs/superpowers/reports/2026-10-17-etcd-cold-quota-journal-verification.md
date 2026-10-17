# 冷运行时配额返还：Grant 前持久化 pending journal

本片将现有公共 `SettleColdRuntimeQuota` 接到真实的 journal-before-Grant 调用链。
原历史认证与已完成返还的 replay 优先执行：认证到原 immutable return 和 committed
receipt 后即返回，不读取可复用的 quota slot，也不读取新的 pending journal。
只有需要新的返还 attempt 时，才将 copied mutation 的 exact StageReference 及原始
owner／quota／task／task birth／release stamp 归因写入服务端派生的私有 journal。
该记录不含 mutation 或调用权限；严格 canonical version-1 codec 上限 4096 字节，
要求 native Lease=0，并验证可替换永久 KV 的版本、修订号和原历史绑定。

fresh journal CAS 为 30 compare / 1 success / 3 failure，共 34 节点；替换为
37 / 1 / 3，共 41 节点。六个原认证点分别保留 Value／Create／Mod／Lease 完整围栏，
替换还固定旧 journal 和原 aborted receipt。Ready／slot 继续由原返还 mutation
固定；原退款事务 34 / 4 / 4、owner-release 60+4 及通用 Stage 协议均未改动。
模型实际 fresh 请求 3280 字节、success reply 28 字节，竞争失败 reply 1491 字节；
替换请求 5449 字节。这些是本次模型的实际字节数，不是全部合法输入的最大值。

可信 journal CAS 成功 ACK 通过身份、形状及字节检查后才能继续 Grant。同一 prepared
只允许一个 journal 提交者；该提交者遇到拒绝、取消、未知或 malformed ACK 都烧掉
其 continuation，重复并发调用只拒绝自身，不烧掉已获准提交者。CAS loser 不 Grant
失败 candidate，本次调用不循环造新 candidate。持久 pending 只通过原 reference
调用 ResolveStage；Unknown 保留原引用，不重建原 Stage。只有重新读取并认证原
aborted receipt 后才允许 fresh prepare 和 fenced 替换；Committed 必须认证原 return
及其 receipt。迟到的原 continuation 与 Resolve 仍在同一 receipt 上互斥。

公共调用始终持有原 register owner 和 context，包含 ReleaseStage 的整条调用链完成后
才 finish；Close 取消并等待该 owner。Generic BeginStage 拒绝私有 intent journal
键族及其 descendants，相邻 intent 业务键仍可用。未新增 committed journal phase、
完成补记 CAS、worker charged-turn 接线、E/S/P 改动或 journal 存量历史回收。

## 定向验证与证据

本 Task 的唯一证据目录为 `.superpowers/sdd/2026-10-17-etcd-cold-quota-journal`。
selector 为 `^TestColdQuotaJournal.*$`，host／race 各执行 12 组测试，全部通过，无
SKIP 或 DATA RACE；另完成 Linux arm64 `go vet ./internal/storage/state/etcd`。

- `quota-journal-red-01`：初始真实 compile RED，仅因批准的新私有 API 尚未实现；
  exit 1，未执行行为断言。原始日志 2222 字节，SHA-256
  `172fb6915227cf61eb8c28b3fb94cb6c7147c66ab4a19a4c552c1b66b03f5dd7`。
- `quota-journal-host-01`：12 组通过，exit 0；原始日志 4909 字节，SHA-256
  `9f83977b9c52518f04881a8dd56f41d2f8ab7b85cdd0588e4dcbebab2ed5a1cc`。
- `quota-journal-race-01`：同 12 组通过，exit 0；原始日志 4909 字节，SHA-256
  `09cdbec193444d036e643d8a184a5b50584a955704e844e887fcb8b97ea89c01`。
- `quota-journal-vet-01`：Linux arm64 vet 通过，exit 0。

Root 已独立审计四轮实际 17 类 Go 输入、source／tools 前后摘要、完整归档与当前
源码；对应输入数依次为 9075、9077、9082、9171，审计全部通过。验证包括实际
持久化先于 Grant、同 prepared 并发一次、未知 ACK 与 owner 拒绝后 burn、CAS loser、
原引用冷仲裁、原 abort fenced 替换、迟到 continuation、commit proof、Close 等待
ReleaseStage，以及 strict codec／保留键边界。本报告不替代独立 Task 审查。

这些测试复用 `coldQuotaPreparedModel`，直接向私有原语提供已解码、视作已认证的
codec history，并通过 bounded gRPC 元数据模型执行实际客户端请求。它们不证明
公共入口真实完整历史认证成功，不证明真实 etcd／native 配额归还或物理运行时死亡，
也不证明 automatic worker 自动返还配额已闭环。真实 cold Create → DELETE →
safeRelease → public quota return、旧 slot 复用后的 replay 及故障恢复，仍需另片
接入已有物理 observer／provider 设施进行验收；journal 历史 GC 仍属后续维护工作。
