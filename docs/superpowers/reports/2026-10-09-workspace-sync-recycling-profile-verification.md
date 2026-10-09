# Workspace sync recycling profile 验收

范围仅 recycling 配置、严格 slot 编码、独立 key 路径和旧入口拒绝混用；尚无 generation reserve、terminal、GC 或生产接线。源码 `a6d340c`（10文件），独立审查修复 `9e49f60`（2文件）；基线 `fa7798d`。生产 main 仍 Redis。

新增 `TaskWorkspaceSyncRecyclingBudget` 与旧配置互斥，New 复制 options 并只读绑定独立 Version2 配置，保持旧 Version1 编码和默认行为。free/occupied/terminal 九字段严格 canonical、slot≤512B、native permanent mutable generation、Version/ModRevision 到 MaxInt64 拒绝。新 profile 在旧 reserve/load 上 fail closed；此提交不会启用不完整的回收链路。

| 最终覆盖验证 | 结果与边界 |
|---|---|
| Darwin arm64 scoped host race | 149实际叶 PASS，0 FAIL/SKIP/clientWARN；含旧 intent/v2 record targeted 回归 |
| 真实 etcd3.6.15 三成员配置 RPC，host race binary | 13实际叶 PASS，0 FAIL/SKIP/clientWARN；New 前后 exact KV/revision 保持，只读无初始化，测试原始 options 与 config birth |
| 包 vet、Linuxarm64CGO0/Darwinarm64CGO0编译、format/exactdiff | exit0；cross仅编译，不是Linux worker执行 |
| R1覆盖上述host/native与静态检查 | 149/13实际叶再次 PASS；没有虚构新行为的镜像测试 |

保留 RED 缺少独立 Options、声明编译 RED、额外 unknown-profile runtime RED。首两次 GREEN 中测试赋值 typo、非法 foreign namespace fixture 失败已保留并修正，不把失败批次计为通过。R1是行为保持的共享 pin helper，原 nil/no-RPC、read/immutable/typeddecode/options/canonical/pin 次序及具体 Version1/2 schema 保持。

独立 reviewer 首审 SpecCompliant Missing0/Extra0、C0/I1/M1；I1重复安全验证流程已抽取共享 private typed helper。限定复审 I1 addressed、C0/I0/M1，M1是已披露环境告警观察，按 Root 裁决保留为验收限制，无未解决阻断项。

服务器输出非零告警：基线完整 pre-down raw INFO40/WARN18/ERROR0（port8、permission4、simple-token4、live-heartbeat2）；R1 INFO40/WARN16/ERROR0，只有上述配置告警。旧22字符串匹配混入4INFO，Root已按JSONlevel独立纠正。两个心跳告警04:40:39Z发生在首次metadata批04:41:21Z之前，不解释为teardown，也不证明容量或性能。两轮日志仅覆盖startup→compose down前捕获，移除container之后shutdown日志未保留；该边界明确披露。

Root独立审核167artifact字节/SHA、R1当前302repo source/3336selectedGo/Cgo/embed输入、三个R1binary、10 working/Git/compiled变更源码，以及baseline/final/R1实际JSONL叶计数、原始预期失败/静态/lifecycle退出和告警。Go native assembly/toolchain identity作为单独工具边界；没有重跑作者套件。初始与final/R1快照和原99artifact保留，原报告前缀保留。仅核查当前OPEN OWN，未读旧CLOSED OWN。

两轮独立operator fixture使用原trackedcompose及pullnever，实际四container IDs/targetedinspect/rawlogs/events/命令Wait退出保留；metadata RPC没有worker/attach IO。native inventory空（revision1→107、1→54）；teardownexit0，12 pre/post project inventories空。Root再次现场查两project container/volume/network共6inventories为空。精确10文件源码提交与2文件修复使用OWN fcntl LOCK_EX，index前后为空；无关Sentinel未读/改/暂存。

最终作者报告SHA `ae2feda1739e0361ae4a4564bc58e48152f1385dcf3df1172ca4333276410312`；manifestSHA `d39c1ca6ca0a6dc2b4e322c71ce6250d3119f0e576c227b3021feb0a66d6c8a3`。实际原始记录属于当前 `2026-10-09-workspace-sync-recycling` OPEN OWN；Task2/3继续使用该计划，不能重复实施已验收Task1。

下一步 generation reserve/原始 local gate，再原始 witness terminal和GC。此次不证明完整sync、冷恢复、安全释放、多worker部署、HA/容量/性能；不移除Redis或切换main。
