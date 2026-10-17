# Cleanup observer ACK 丢失后恢复验证

本片接通 `local-complete`、旧 Run 已真实 joined、原终止 reservation 存在时的恢复。先以独立收费轮次调用原两次 RPC 历史认证，验证原 reservation、provider 已完成步骤、本地完成记录和 enrollment；同一注册 job 再完成 manifest/tar/目标归属检查及真实 Close，回到原 actor 进行一次新 Run CAS、原预算和 managed Claim。恢复跳过 Finalize，不再连接已终止的 runtime。

实际终止入口再次读取原记录并匹配 reservation 的完整 key/value/Lease/Create/Mod、Version、Task/birth 和 reserved phase。缺失、重建或变更在签名和 provider 变更前拒绝，不能退回首次 Terminate。公共 `TerminateTaskRuntimeLocal` 保留原 nil-reservation 行为；恢复通过私有入口只 Query 原 observer 结果，保留原 proof 验证、provider 剩余步骤、安全释放及实际 join。

产品仅五个既有 etcd 文件，新增检查与原事务/生命周期保持私有同包关系，未导出权限 DTO。已检查 GoDoc、逻辑分段、包职责及 gofmt/diff-check。新 `recovery-termination-run` 投影实际43 nodes、request12286B、success96B、failure563B；原128KiB请求、256KiB响应和64-node上限未变。

## 验证结果

- **真实行为 RED**：`observer-red-01`，11.64s。原 observer 真正返回签名终态 proof 后，测试 TLS 转发器扣留响应；原五秒 deadline 使首 worker 超时退出，RunJoined=true、两 Claim、一次归档，原 reservation rev107、本地完成 rev101、provider步骤0/1完成。新 worker仍返回 `missing-release-proof`，零TaskClaim/零Query。日志13815B，SHA256 `b66e9450d27d31ed49ae61bcb7744abb4c07ae2e1c0a301b4fda70677b847b9e`。该失败在完整运行/输入/工具/真实退出及清理复核后才授权产品修改；不是 TLS/fixture 错误。
- `observer-host-01`：52组203子用例通过，80.256s，9017输入；日志94955B，SHA256 `7e88983cf941a857a2ceb452a6952b25ae491902d6efa6ca75a44da7de5d7504`。包含阶段选择、reservation缺失/ABA/原值变更/非法原生元数据及原终止入口回归；纯元数据正向不能描述为物理权限。
- `observer-race-01`：52/203通过，87.852s，无数据竞争，9022输入；日志94499B，SHA256 `50a01ad40e5457db78fe9b955d02976e90f57beb1ae0d5caf46cd8549c65fb29`。host/race分别8/7条WARN，取消与lease故障原始记录保留，没有FAIL/SKIP。
- Linux arm64/CGO0 vet通过，9108输入；新runner构建通过，9274输入，ELF64 AArch64 SHA256 `7fe17a81db7adfc36d14a0c6bc0dbb78cd0e5468b7e7cd3d72b43bc7aab68add`。每个Go epoch全部17类输入、源码/工具、前后及当前SHA和所有归档成员均在后续Go或源码修改前独立核对。monitor无etcd依赖，2042个当前输入与ELF/buildinfo重新核对后复用。
- **`observer-native-01/observer-ack-recovery`通过，12.41s。** 原真实双向TLS、原证书和有界frame转发；转发器不合成proof、不取消调用者，收到真实终态proof并验签后扣ACK，观察调用者五秒超时关闭连接。首worker实际Wait=nil，保留local-complete/pendingtermination/RunJoined。独立新worker取得一个新Claim，仅发一次Query，Terminate总数仍1，返回settled/committed；原reservation身份、归档命令、本地完成记录保持，第二worker不重连runtime，原provider已完成步骤不重写，最终1PATCH/1DELETE、S为空、重复结果一致。两worker与UID1000清理进程实际Wait=nil。日志12193B，SHA256 `d0507f9f08172a5c818721bffdcd1683e2d6581eab8b44091e8c27557b713384`。
- `observer-bound-native-01/bound-recovery`回归通过，9.03s，保留归档ACK丢失、真实双worker、第二worker一次Claim/零归档、内容/模式、provider、释放及结算断言。日志10697B，SHA256 `b15dca5dc1b2ea3301bea05531e6ca9dbf7fe3897e991030fc8af14b67bf0151`。
- 两轮native实际attach0/Wait0、无OOM/timeout、固定参数/工具归档/二进制和etcd输入、容器删除与空清单独立核对通过。observer转发器的listener、全部已接受raw/TLS连接、handler、credential实际Close/join完成。observer场景7WARN/3ERROR，bound场景8WARN/3ERROR，ERROR为fixture关闭监听，未静默。
- observer-01三节点host实际退出均0，无OOM，容器/专用网络删除、空清单、实际Compose/工具归档/生产者和全部host环境独立核对通过；成员WARN18/21/21、ERROR各5，包含取消Range及关闭监听，记录保留。

## 未完成范围

冷unjoined、首次Run写未知以及本片未支持的历史阶段继续保守保留。跨Backend真实32/33任务、close-join、部署loader与完整Task审查仍待完成；这不是完整容量、生产默认切换或Redis移除验收。
