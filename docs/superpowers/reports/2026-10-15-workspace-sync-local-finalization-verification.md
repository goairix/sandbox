# 本地完整同步与持久完成标记验收

接受源码提交：`44673b4`、`0488e3c`、`3b1e4e6`、`30b5fc9`，审查修复 `ba7a3dc`。初轮独立审查 C0/I1/M0；同一 reviewer 的 I1 限定复审 Approved，剩余 C0/I0/M1，无未解决源码阻断项。本次仅验收本地完整同步组件，生产迁移尚未完成。

`FinalizeTaskWorkspaceSyncLocal` 接通本地归档的普通文件、目录与空目录、文件/目录类型替换、按策略删除多余项、保留 excluded 子树、恢复最终目录权限、实际关闭及持久完整完成标记。原 regular-only API 和 v1/v2 wire 保持。目录归档走真实原始绑定再冷恢复，无须伪造普通文件或额外收费。

固定 progress 记录依次为 regular→metadata→effects-done→sealed；immutable complete 与 sealed progress 在同一事务发布。先不可逆封闭效果入口、等待实际工作和 FD Close，再只读复核并发布。公开结果是诊断与查找引用，不能直接授权 runtime 关闭或 owner 释放。同 task 原始接收与恢复在同一 mutex 下对称排斥，quarantine 也保留原始身份。

| 验证范围 | 证据与结果 |
| --- | --- |
| 普通路径 | 实际 S1 已占旧 put 时恢复、文件↔目录、empty dirs、文件0444、目录0555/0000、根0700；完成后零效果和零 native 写入。 |
| 清理与恢复 | delete true/false、excluded 的原始0000祖先保留；临时权限 ledger 在实际中断后由新 claim 恢复。延迟旧 ledger-pop 在新 progress generation 后被真实 etcd 拒绝。 |
| 故障与关闭 | mkdir/unlink/rmdir/chmod/fsync/Close/target Close 七处中断；五个 reserve/progress/terminal/GC/complete 提交丢 ACK；真实 chmod/Close 阻塞期间不能提前重新准入。 |
| 拒绝条件 | protected 类型冲突、symlink、hardlink、非 DFS、leased/malformed progress、root ABA；原始 receive 和 PreparedSource 在完成标记后拒绝并实际 join。 |
| 边界 | 单实际 task partition 的 S64、depth128、fanout256、转义255字节名和≤4 archive passes；native 请求与两分支响应≤256KiB，最多64 nodes。 |
| I1 修复 | A regular progress 被 B 恢复时，先 CAS 持久采用 B，再允许普通文件效果。实际延迟 B GC 后，同 B 重试在物理效果前拒绝；真正新 C 使原 B 事务失败并完成同步。 |

I1 的行为 RED 如实保留。首轮 GREEN 已通过 B adoption/同 B 拒绝，但测试随后使用已关闭的旧客户端提交捕获事务而失败；修正测试连接后，保留原比较和操作的真实事务在 GREEN2 被 etcd 拒绝，测试 PASS3.45s。未知 adoption CAS 会在恢复效果前返回，不修改旧 regular GC 的64节点协议。

复审 M1 是 GREEN2 原始 embedded-etcd 关闭输出中的 `mux: server closed`、关闭连接 error 与 closed-peer-stream warn。实际测试通过、原始三 child Wait/attach join 和清理成功；这不等于原始输出无告警。保留诊断作为环境限制，无产品变更或重跑。

新增 host race 与必要 owner/monitor/checkpoint/wire/profile 兼容检查、affected vet、Linux arm64 编译、格式及 shell 检查通过；R1 focused host race PASS2.375s。测试按实际源码 epoch 分批执行，不能称同一源码全矩阵或完整可靠性验收。

Root 已核对首21个 native binary/manifest SHA 与实际三 child Wait、attach join；R1 三个 epoch 另作增量核对。最终26个本 Task 源码/脚本中，25个 Linux 选择路径匹配依赖清单，另一 !linux 文件按提交检查；R1 三个修改路径逐 SHA 匹配 GREEN2。静态36文件清单只是 legacy/context/script 子集，不冒充全部编译输入。GREEN2 binary SHA256 为 `981d59c649105ed7ce6707debaebfe2f15a59782b4f08883575e8e84f9500ff9`。精确 owned 容器、卷和网络库存为空，失败原始输出未替换。

原生事务保留全部50历史/当前 pins、whole operations-prefix 和 budget pins。新 progress create59、phase CAS60、v3 reserve64、ledger62、physical guard62、terminal63、GC64、sealed+complete64 nodes；旧 regular GC 仍61+3=64。固定 S1..64、33字节 index 和归档槽保持，无按文件/目录增加 key。

I+4M 是受准入约束的新 application backing，不能解释为进程 RSS。M=65536 时，source65536+retained context65536+suspended walk42024+serialized scratch81920=255016≤262144；metadata gate 串行化新复制/编码/native 调用。借用的既有 authority 与 SDK/gRPC 各自仍受原有独立边界约束。两条持久记录最多24KiB payload，后续安全回收未实现。

**明确限制：** 两次 full-cell16384槽 fixture 初始化在 finalizer 前失败，实际记录 container OOMKilled=true；各 child SIGKILL 的独立原因未隔离。后来 S64 单 partition 通过不等于全 cell bootstrap 或容量验收。foreign UID/实际 mount crossing、backend Close/claim-loss 的额外阻塞排列未在本单元重新执行。仅支持同 boot/mountns、未改变的 recovery root 和升级后的合作 writer；不证明断电、主机丢失、混合旧 writer 或同 UID 越界写。

尚未完成：外部 runtime 物理终止与 provider 清理、owner 安全释放、cloud/FUSE 与 none 独立协议、生产 API/Pool/Docker/Upload/main 接线、分区调度/collector、大 N 容量、部署恢复和 Redis 移除。当前主程序仍使用 Redis。
