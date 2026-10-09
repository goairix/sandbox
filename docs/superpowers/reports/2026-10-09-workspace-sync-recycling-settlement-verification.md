# 原始本地写入终态与 generation slot 回收验收

源码提交 `8d32040bd2bba9ee9fe692206054b67f9eb5277f`、`7f67def2a8dd13812d0c450d7fcf6b0b28e008e8`；审查修复 `9d58c07dba60c0aa1d51e3a88e111a0a2dd8de16`。本单元支持原始进程内成功 local 文件写入的持久 terminal、原子 free+intent 删除和下一文件消费。主程序仍使用 Redis；正常冷恢复、持久 cursor/checkpoint、完整同步与安全 owner 释放尚未完成。

只有原始 draft 注册的唯一 put、实际 IO witness 与唯一 settlement 可以结算。新鲜 source/body/target 注册覆盖 native RPC 和物理 postguard，不复用已取消的 put context；Close 取消并实际等待，nativeMu 在物理检查前释放。terminal/GC 保留全部50历史 pins、当前 prefix 与预算 pin，实际 protobuf 预检保持64 nodes、请求/回复256KiB、record64KiB、slot512B。槽位永不删除，比较 native generation 与 canonical value；unknown 固定原始事务只读仲裁，不重复 IO/mutation，不把冷 metadata 升级为权限。结果 version 将达到不可接受的 MaxInt64 时派发前拒绝。

## 验证与审查

- 实际 RED 为原始成功写入缺少 terminal/GC 消费者；GREEN 完成原始文件→terminal→GC→next。版本耗尽边界另有真实 RED→GREEN。
- 修复前冻结源码 `7f67def`：16个固定 Linux arm64 CGO0 批次49实际叶子 PASS，host race18实际叶子 PASS，均零 FAIL/SKIP。覆盖权限、复制拒绝、unknown、旧事务重放、cursor 保守等待、source/body/target 在 RPC/postguard 的关闭等待。
- S1 单原始 source 连续65个实际文件均检查字节/hash及 occupied/terminal/free 的 Create/MOD/Version、active keys2/2/1；最后256 slots/0 intents。文件64的 MOD237/238/239、Version194/195/196、Create36。捕获的旧申请、terminal、GC 原生事务迟到执行均拒绝。12.635秒是 tmpfs 功能试验耗时，不是性能或断电持久性证明。
- R1 仅复用既有 Put ACK validator：当前 `9d58c07` 两批4实际 native 叶子、5实际 host race 叶子 PASS，零 FAIL/SKIP；vet、Darwin编译、format、bash/diff 检查通过。49叶与65文件证据明确属于修复前源码，不声称当前源码重跑49叶；Root 未重复作者套件。
- 唯一独立 reviewer 完整审查 SpecCompliant、Missing0/Extra0；I1重复 helper 经同一 reviewer 限定复审已解决，最终 C0/I0/M1，Approved。环境告警限制保留。

## 独立证据核对

Root 核对1187个 artifact 的字节/SHA256；冻结与当前源码分别绑定12条 working/Git/compiled 路径、实际选中3427个 Linux/3404个 Darwin compiler inputs。修复前16个 native 二进制相同；R1两个 native 二进制相同，并核对 Darwin 二进制。依赖 TestGoFiles/XTestGoFiles 的早期5439/5366超集保留为 metadata，未作为真实编译输入证明。

27个实际环境的81个原始 child Wait、attach/controller 实际退出、完整日志与清理已核对。75条修复前与6条R1独立查询使用脚本实际 `sandbox.test.project` 和 `sandbox.test.fixture` labels，全部 exit0、stdout/stderr 空；Root 同样重新查询这些精确对象，结果为空。

作者报告 SHA256 `543f33f5d79102cf6b005690042ea5622bdd4aca22deb11c67ad957bbdb7a1ad`；最终 manifest SHA256 `d83191855fdb05021fb9de65a664b8282abe8124e15826de52874aa0cb42faac`。修复前 snapshot SHA256 `d56b16cc3f411d6fd7e958211cf5e24dabede813fb5069ec7c33f76def28af5d`；R1 snapshot SHA256 `759b53e44a612b75e96fa5f9f19cce5480e15e12fed202569aea3e331fd7b82d`。本报告保留封闭 OWN 后的验收结论，禁止重读或重新派发已验收单元。

## 失败留存与限制

原始 RED、编译缺少 Validate、旧 witness 测试归属不符、vet 测试复制锁、宽 host 选择产生 SKIP 和首次 provenance 断言均保留；最终限定验证通过。65文件初试 Entries32 拒绝，随后 Root 错误索引公式4352又在 New 拒绝；使用既有 `taskArchiveIndexBytes(67)=8448` 后通过。测试专用 profile2/custom-data Entries≤67、65文件 source TTL180s；生产/default/legacy 额度、RPC时限和容器资源未变。

首次 manifest 将仍写入的生成日志提前哈希，Root 拒绝。作者修正时覆盖了首次 manifest，未保存其原字节；已记录首次 SHA `e05a010857510c7fb13ad5a1eb0a613e5839d79c03aba33a4b26e39cb33ae884`、原生成日志/退出码和三次未匹配的恢复尝试，不伪称原快照。这是生成 metadata 的留存缺口，原始测试、失败、源码、二进制证据未变。稳定 manifest 排除自身及仍写入的生成输出，作者和 Root 完整核对通过。Root 首次最终核对对相对路径使用了错误基目录，纠正为 repository-relative 后通过，无源码或测试变化。

冻结批次 client WARN8/ERROR0，server INFO5573/WARN966/ERROR192；R1 client 无 WARN/ERROR，server INFO707/WARN116/ERROR24。原始 peer/health/leadership/closed listener 等消息完整保留，不统一归因或声称可靠性通过。历史 Task2 child SIGKILL 原因仍未知，本次没有重跑或修复声明。

同一物理 worker 内跨 Backend 竞争不证明多进程 Deployment；postguard hook 位于实际 target.check 后，只证明该观测点的锁与实际关闭等待。GC 删除了历史 terminal，不能据此冷恢复进度。后续持久 checkpoint、目录/delete/cloud SDK、实际运行时终止与安全释放、生产接线、分区/FUSE/collector、大量已有 sandbox 的性能容量、部署恢复和 Redis 移除仍须实现与验收。
