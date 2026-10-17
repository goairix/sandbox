# Detached 冷恢复与保留对象图组件验证

2026-10-18，代码提交 `45ed1ca`；唯一 reviewer 的组件 Spec/Quality PASS，接受可提交。
此记录只覆盖原 job 的新 Run 元数据恢复及保留对象图，不代表
managed consumer、实际 PID1 终止、最大输入或大 N 容量验收。

## 实现与恢复边界

恢复入口重新认证实际 E/S、原 registry/job/Kernel、历史 source 与真实 Joined
Run；调用者 DTO 或租约消失不能开放恢复。原 actor 消费一次候选，使用48比较、
1写入、2失败读取的51节点事务。Unknown 只读协调同一候选；原 physical
continuation 收到成功回复之后才释放旧 draft 和 gate。已确认但尚未接收的回复
允许精确候选的只读重入，不能再发 Put。原 local 分支保留43节点。

只有真实 Joined、物理 work 已 join、owner 已返回且原 Claim 永久 lost 后，才
退休原 close/quiescence draft 中已提交并 cleaned 的 Stage 指针。原 Claim、
prepared、E/wire、attempt 和发送 latch 继续保留，旧 Claim 仍不能重新发送。

对象图计量包含 slice capacity、原 Claim 和两个恢复 roots，以精确地址和长度
去重共享字符串；独立 Clone 分别计费。显式共享基础设施仅在指定根豁免，嵌套
foreign pointer 不按类型豁免。每 job 128KiB、遍历65536节点上限未扩大。
这依赖受控构造器的有界 backing，不是通用 Go allocation 测量器。

## 最终验证

Linux/arm64、CGO=0、实际 etcd3.6.14 三成员；最终 ELF SHA-256：
`24d1a1f0576cfbcd881dd103411cb4acc21d360e2e09aa5e678cd233bd387b23`。

| 场景 | 结果 |
| --- | --- |
| 实际保留对象图与 finish | PASS，85209字节；不是最大值结论 |
| 未 join / 伪造 Joined 拒绝 | PASS，Stage 保持 |
| 真 join 后退休 Stage，旧 Claim 零新 Grant/send | PASS |
| 实际 absence→新 Run→新 Claim→fresh drain，旧 Put 被 fence | PASS |
| 实际提交丢 ACK，复用原候选只读协调 | PASS |
| 永久 fact 跨两次真实新 Run 仍可读取 | PASS |
| 实际 CAS 后保留未接收回复，重复 continuation 单 Put | PASS |

以上七项在 `run-dd36890cf2af4d9aa795e8235175ca44` 全部通过。
纯对象图边界和字符串 backing 两项在 `run-551db2cb3c4240e5ac68884d202b6cd6`
通过。原 native fixture 的 `TestCleanupSlotsWire` 在
`run-eeb745b77d3541079621e36fd5be851c` 通过，原 recovery 两种投影仍为43节点；
这是实际 wire inventory 验证，没有重验 local 物理恢复。三场均无 SKIP/FAIL/OOM，
三个原 etcd 子进程实际 Wait/join，精确自有容器删除；17项 source 与 ELF 指纹稳定。

`detachedcoldfinalbuild02`、`detachedcoldfinalvet01` 各9245输入通过；
`detachedcoldcompat01` 定向 race 四项通过，9150输入，2.590秒。
每次 Go 的完整输入、工具及前后指纹审计通过，共用缓存和内容寻址源码快照。

保留真实失败：共享字符串重复计费 RED；Stage 退休前的实际 nil 断言 RED
`run-5fb36c1d39894680be9e2f2cfcbb51a6`；修正退休后，确认候选第二 continuation
实际 ErrInvalidRecord RED `run-52c8eeaf9cbd4c498162eaa848a22e83`。此前超 cap
而未到达候选分支的失败没有冒充候选行为 RED。

## 固定合法长 signed 输入补验

`7102368`（辅助分支 `ff57b76`）增加三测试文件：只在首次 native identity provisioning 与
所有签名前设置长字段，仍走原 producer、签名、enrollment 发布、dual drain、
Pending ACK、64节点单 Put 与真实历史读取。没有修改已签字段或生产上限。

`run-353b5a47def847489fd0363713a9045b` PASS14.19秒：namespace512字节，实际
quiescence16949、ticket5791、close receipt2972、settlement9136字节。
19项 source 和 ELF 指纹稳定，三个 etcd 子进程 join、无 SKIP/FAIL/OOM，容器已删。
build03/vet02 各9246输入，完整审计通过；唯一 reviewer 对固定场景 Spec/Quality PASS。

主分支合并 provider Options/TTL hooks 时共用唯一 producer 序列，默认和新 Source
入口分开传入参数。相同 main ELF 在 `run-400f358d6a5b49bdbff8f7fbb6439663`
同时通过长 signed 输入（实际Q16952字节）和原 provider UnknownNoResend；63项
source及 ELF 指纹稳定，原三子进程 join，无 SKIP/FAIL/OOM，精确容器删除。
main `detachedmainlongbuild01` Linux构建9252输入、完整审计通过。

保留两次前置失败：最初 Runtime.BootID 未满足 activation 额外 UUID 约束；修正后
完整 activation companion 超8192字节。第二次修改只调整签名前 runtime ID/UID
的内容，长度各128不变；原 BootUUID、字段约束及全部限制保留。上述是固定合法
长输入验证；下面另行记录该固定形状的保留对象图验证，仍不代表全协议绝对
最大值、全局内存或大N容量验收。

## 固定合法长输入的完整冷恢复对象图

`c0735b2`（辅助分支 `9e7eb47`）补真实长输入的原 draft 加完整 recovery candidate
验证。首次实际 `run-21e3d82cc50b45e78f8de7372a05640f` 已获得真实 Joined E/S，
但被128KiB保留图门禁拒绝。只退休 Q 授权缓存后，`run-71b9ceb9095b46748ea5e8abd5cb6f75`
两个边界用例通过，long仍失败：原draft单独121079字节，不能据此声称完整候选通过。
两次失败及对应输入/日志均保留。

修正仅在原 authenticated Joined/job/Run/S、physical joined、owner returned 及
锁内原 Claim 永久 lost 后退休已 committed/cleaned 的冗余缓存。Q 额外匹配原
prepared reference，清除已完成发送的 certificate/issuer/claims/ticket/record
和 prerequisite 中重复 payload；保留 prerequisite 本身和 native坐标。Close
只清 canonical recordValue，且原 commandID/reference 双匹配真实 Q 的 close
command；其他 close payload 全保留。退休与 Stage 非空无关，可在前次超限失败后
幂等重试。原 Claim、none完整draft、prepared、旧E/wire/attempt/latch均保留。

旧 Prepare/Deliver 在 lost Claim 下仍拒绝；none 原写入可以在 live 检查前仲裁，
因此专门验证其原 native committed 归属加 dead-Claim error，零新 Put/Grant/send。
native Load/Query 不依赖已退休缓存。未 join/伪造 Joined 时缓存保持原样。

最终实际 `run-ac7e9ddf1b75458aae92f212fe8df8f8` 三项全部 PASS：边界14.19秒、
退休/仲裁14.12秒、完整 long恢复15.13秒。实际Q16962、fact9163、none5254字节，
原 draft加完整 recovery graph为129326字节，未扩大131072字节或65536节点上限。
原51节点actor只写一次新Run，成功回复前原roots仍在；原fact/none完整MVCC未变。
19项source与ELF前后稳定，三原etcd进程实际join，无SKIP/FAIL/OOM，容器精确删除。
ELF SHA-256：`51c17b289f63f9ed013b29a0c4f274e99c2dc3c10c40c6f83d8ebb3f03246991`。
build02/vet02各9246输入及完整审计PASS，唯一reviewer最终限定Spec/Quality PASS。

主分支合并后同三项在 `run-0e9a2a086e3b4c728db8b33f193c147a` 再通过：实际
Q16964、fact9164、完整图129334字节；65项source及ELF稳定，三原子进程join、
无SKIP/FAIL/OOM，精确容器已删。主分支Linux构建9254输入及完整审计PASS。
整合ELF SHA-256：`add5dc60d3718ea4f717a54d64bf105a7c7912fc1ac3732b962c9dc1ae48f723`。

这只关闭上述固定合法长形状；该场景余量较小，不能据此推断其他合法最大值或
64MiB全局admission、32物理并发、大量已有sandbox容量已通过。

## 尚未验收

其余合法最大输入与长历史冷恢复组合、worker 全局64MiB admission、32物理并发、
大 N、managed消费者、owner/terminal/quota全链路及
HTTP/部署恢复仍开放。默认入口保持 Redis，生产切换0%。

原始输入、命令、日志、失败记录及唯一审查记录保存在 B 工作树
`.superpowers/sdd/2026-10-18-etcd-detached-noeffect/`。
