# Cleanup 归档绑定后重启续接验证

本片支持已持久记录 `archive-bound` 且旧 Run 已真实 joined 的清理任务，由新 worker 续接原归档完成 Finalize、终止、安全释放和结算。仍使用同一个固定 32 项 registry job；manifest/tar 检查和真实文件 Close 在原 job goroutine 中完成，期间不占 native actor。完成后回到原收费 actor，验证注册指针、Task、worker/backend/self 与当前 owner，再进行一次新 Run CAS 和原 managed Claim 流程。

恢复校验原 Task/birth/digest、StoreID/slot、binding digest、spool root、目标目录归属和同步策略。新 worker 只按原同 boot、PID/mount namespace 和目标归属规则接续；复制的字段不能构造物理权限。新 Run 写未知时保留原候选和占用，不换 UUID 重试，不冒认旧 Run joined。正常 rollover 与恢复共用原 Finalize，恢复不重发归档或 quiescence 命令，不增加正常 rollover。

新 Run 事务实际为 W20+T/I/R12+E4+S4 共40比较、1写和2个 failure points，共43 nodes；纳入原 pre-Grant inventory，原128KiB请求、256KiB响应和64-node限制不变。产品改动集中于四个 etcd 文件；原事务和并发所有权紧密共享，保留私有同包实现，未新增公共权限接口。已人工检查注释、逻辑分段和包职责，并检查 gofmt/diff-check。

## 验证

- 已有真实 Linux RED 证明首 worker archive-bound/RunJoined/实际退出完成后，第二 worker仍保留任务。本片没有重复制造 RED或削弱原第二 worker断言。
- `bound-host-01`：48组、161子用例通过，79.543s，9016输入；日志88105B，SHA256 `2974100b808fcf093ce19eb56c21d55473a7d1b365fb0716f41c488124a44999`。新增纯元数据匹配包含23个变异拒绝场景，不能将它们描述为物理权限或完整恢复证明。
- `bound-race-01`：相同48/161通过，87.450s，无数据竞争，9021输入；日志89355B，SHA256 `de7bbd6cc1fc9e12096f911f3bb63e9550f76591c293ff9b4b56a76b7f37db55`。host/race分别7/10条WARN，保留取消和lease故障日志；没有FAIL/SKIP。
- Linux arm64/CGO0 vet通过（9107输入），runner构建通过（9272输入），ELF64 AArch64 SHA256 `38e1c49af8191a295a97fc9b32a971679798d5745f9905d2c752a9e1c76f94c4`。每个Go epoch的全部17类输入、源码/工具、前后及当前SHA、每个归档成员均在后续Go或源码变更前独立核对。monitor没有etcd依赖，全部2041当前输入和ELF/buildinfo重新核对后复用。
- **`bound-native-01/bound-recovery`通过，9.07s。** 原真实archive-bound写在rev85成功，TLS代理扣留ACK至原deadline，首worker返回Unknown，RunJoined=true且实际Wait=nil。独立新worker使用新Backend/session、不同真实PID/incarnation，保留同Task/occupation/StoreID/slot，恰好一次TaskClaim put、零archive-command put；原归档命令内容和revision不变。最终settled/committed、重复结果一致、S为空、文件内容/模式正确、provider恰好1PATCH/1DELETE；两worker和UID1000目录清理进程实际Wait=nil。日志10699B，SHA256 `90a39b20d306e9b4f8c334111370bb9e202ed490f973eb47b80464733f84582a`。
- `bound-complete-native-01/complete`通过，5.70s：原正常两Claim、due推进65条、文件内容/模式、provider1PATCH/1DELETE、安全释放和重复结果断言保持。日志6479B，SHA256 `d204fc2bb6df85fe37fb856ad1fb80c4d8ef35e3485b1009b3dc7c3e9a128461`。
- 两轮native的固定运行参数、工具归档、二进制/etcd输入、真实attach0/Wait0、无OOM/timeout、容器确切删除及空清单独立核对通过。各8WARN/3ERROR，ERROR为fixture关闭监听，未静默。此次一并提交三份恢复native测试文件；其中释放后结算场景的正向验证见terminal验证报告。
- bound-01真实三节点host退出均为0，无OOM，容器及网络删除、空清单、实际Compose/工具归档、生产者及全部host测试环境独立核对通过。成员WARN21/19/21、ERROR5/5/4，包含取消的Range和关闭监听，原始记录保留。

## 剩余边界

冷unjoined与首次Run写未知继续保守保留。observer ACK恢复、跨Backend真实32/33任务、close-join、部署loader和唯一完整Task审查尚未完成；本片不是完整容量、生产切换或Redis移除验收。
