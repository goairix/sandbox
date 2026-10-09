# Generation slot 申请与本地 gate 验收

验收范围是原始 local body 的 generation reserve、只读诊断和写前 gate。源码基线 `ccb1d62f6c170c8f2043570b8df2928f20325716`，两个连贯提交为 `b1c9d1284834276b9c591ff09edf9b8b7396f868`、`6922d0d94cde77bdd9ff4d2109e88e254b4ff81c`。生产 main 仍连接 Redis；本次没有 terminal、GC、正常冷恢复或安全释放接线。

新 profile 使用独立路径与 operator 预建的永久 free slot。申请比较捕获的原生 ModRevision 和 canonical Value，保留全部50历史 pins、完整当前 prefix 和 immutable budget；64 helper nodes 内原子写入 intent/occupied slot。原始明确 ACK 与物理 postguard 通过后才产生一次性 IO 权限。未知结果固定候选，只读仲裁，不能换槽、重发或升级为 IO 权限。本地 gate 对永久可变 slot 校验 Create、原 ACK Mod、Version+1、canonical value 与 intent attribution。

target 注册覆盖候选读取、申请 RPC 和 postguard，Close 取消并等待原始操作。两个受影响的成功申请分支在物理检查前释放 nativeMu。共享 source/body 注册与记录构造流程；legacy 默认与 wire 格式保持。未增加无预算 KV、全局计数器或按 sandbox 常驻 actor。

## 覆盖变更的验证

- 原始 Linux 请求 RED：profile2 尚未接入时拒绝；GREEN 实际写入 `hello` 五字节，slot Create36/ACK Mod45/Version2。另一个真实 RED→GREEN 验证 target.Close 从提前返回改为等待仍在执行的原始申请。
- 最终源码限定 host race **154实际叶子 PASS，零 FAIL/SKIP**。
- 同一最终 Go 源码、相同 Linux arm64 CGO0 二进制的九个固定批次 **24不同实际叶子 PASS，零 FAIL/SKIP**：23项 profile2 与1项受影响 legacy local IO。覆盖竞争、free 同值 generation 变化、旧 native Txn replay、slot/intent/restore/prefix/claim 变化、root/caller 失效、丢失或畸形 ACK、S2 unknown 固定候选、关闭等待。另有早期三项 targeted legacy 回归通过。这是分批覆盖，不是完整一轮全绿。
- 真实 reserve 64 nodes/request14911B；最大合法 B/key 的 host wire case request89110B/reply24753B，并拒绝第65节点和超限 request/reply。256KiB 限制与全部历史 pins 未提高或删除。
- 作者 package vet、Linux实际运行、Darwin编译、format、bash语法与 diff 检查通过。Root审核原始结果，不重跑作者套件。

## 独立审查与证据核对

唯一 fresh reviewer 对 Task2 两笔提交的完整差异审查结论：**SpecCompliant，Missing0/Extra0，C0/I0/M2，Approved**。没有阻断项；两项 Minor 作为限制保留。

Root核对684个原始 artifact 的字节与 SHA，包括失败/core/intermediate/final 批次；927个 repository source、3423个 Linux/3400个 Darwin 选中 compiler inputs、12条 working/Git/compiled source 绑定和10个最终二进制全部一致。九个 native 二进制及选中源码列表相同。编译器/链接器仍是明确工具链边界，不声称完整跨平台 SBOM。

19个原始环境的实际叶子、57个原始 child join、attach Wait、实际退出与 independent cleanup 输出已核对。最终九个环境预清单为空；全部19个项目的57条 independent post 查询 exit0、输出与 stderr 为空。Root初次查询误用了 compose label，废弃其推论后用脚本实际 `sandbox.test.project` 和 `sandbox.test.fixture` label 重新查询57次，均 exit0/空输出；验收使用纠正后的查询。

作者报告 SHA：`1438dcbfb641486b6c79033c0b54e372955b0556eb6d06dbec86cb0b9622ea28`。manifest SHA：`5c0753608821463039255e4937f95830cac88ec4e674ec569d7c750dbf71ba94`。最终源码 snapshot SHA：`2ef467bcad71473fae52033e1e5f5af2df537b989d5186e3b3a7246636af5606`。原始证据位于当前 OPEN plan OWN，封闭后不得重新读取或派发已验收任务；此报告保留结论。

## 限制与下一步

早期较大 native 批次有12个实际叶子通过，随后竞争 setup 超时，原始 local2 child 被 `signal:killed`；其余10叶失败。container OOMKilled=false，**原因未知**。原始失败、child Wait、日志和清理保留，没有提高资源限制或把分批通过当作原因已修复。

最终九批 client WARN1（Close 负例的 canceled KV/Txn）；server INFO3196/WARN550/ERROR108。ERROR 是原始停止阶段的 closed listener/mux/http 记录；WARN包含配置、启动 health 与 peer/leadership 消息，不作统一 teardown 归因。没有 warning-free、可靠性或容量结论。

PostguardClose 的暂停 hook 在实际 target.check 返回后，证明此时 nativeMu 可用与 Close 等待仍未结束的原始 guard；不能凭这个 hook 证明实际物理检查全程的锁状态。unlock-before-check 由实现顺序和独立代码审查建立。竞争使用同一物理 worker 内两个独立 Backend/client，不能声称多 worker 进程部署验证。

原始成功 IO 仍 occupied/charged。下一步实现 original witness 的持久 terminal 与原生有预算 GC，再处理 checkpoint/正常恢复和安全释放。连续65文件复用、断电持久性、provider/生产接线、分区/FUSE/collector、大量已有 sandbox 的性能容量、部署恢复与 Redis 移除均未由本次验收证明。
