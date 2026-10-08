# 持久 workspace 同步意图验证

源码提交 `7391a5e9c9c6d689f7372bdcacccc4651515c24a`，修复提交 `27feaf6dcdc8cf5bb2df98a841d2260e1492432f`，基线 `4f48722087df3bad917de270640754b0a23f5411`，总计精确12个源码路径。唯一独立review发现一个空body已完成读取的拒绝缺口；原作者单独修复两路径并通过针对性复审。最终Spec Compliant、Quality Approved，剩余C/I/M=0/0/0，本单元已验收。它实现native元数据与跨Backend共享背压，不授予实际worker/provider派发、结算、退款或安全释放能力。

## 行为与预算

operator初始化的不可变预算由New只读固定，nil选项保持默认路径；每个既有partition有S=1..64个永久槽，256分区最多256*S个意图。intent与slot在同一原子Txn写入，均Lease0、同出生revision；保留原50个历史comparisons、同一写Txn完整operations prefix VERSION=0比较及原始budget身份。实际请求63个helper节点、13360bytes，成功回复界145bytes、失败回复界5833bytes，受原64节点/64KiB record/256KiB request/reply限制。

原始未读body只持一个draft；提交结果不明后不重发、不换槽。已知出生的intent/slot及配置BudgetRevision拒绝ABA；冷读取仅复制诊断。Close与自然claim租约过期不删除永久记录或归还槽位。全cell逻辑key/value占用上界256*S*(B+512+2*1024)，不包含配置、MVCC/WAL或复制开销，不是容量验收。

## 最终验证

Go1.25.6 darwin/arm64，限定新selector `^TestTaskWorkspaceSyncIntent`，未重跑已验收selector：

- Host race：15个实际host叶子PASS，23个native叶子SKIP，4.295秒。8个顶层/19个全部PASS包含子项，不能据此声称native执行。
- owned隔离etcd脚本的native race：38个实际叶子PASS（23native+15host），17顶层/42全部PASS，29.276秒，零SKIP、零WARN。
- 包vet、Linuxarm64 CGO0编译、gofmt-d及diffcheck均exit0。编译产物独立确认static ELF64 ARM aarch64。
- meaningful compiled RED：实际canonical receive/source/body成功后，缺失持久登记导致断言失败；预算及严格prefix谓词各有编译后语义RED。中间失败日志保留，未用最终绿色覆盖。
- Root独立核对12文件的长度、current SHA256、Git blob内容/ID及blob SHA共60项全部一致；23原始日志长度/哈希一致。最终源码仅追加语义限制注释后重新检查格式/diff，无行为变化。

实际用两个独立Backend及两个真实创建、runtime、archive/source流程竞争同一分区S1槽。故障覆盖包括提交ACK丢失、absent与迟到原Txn、写前插入后续`zz/future` operation、restore/history/config ABA、构造器缺失/重写/leased配置与选项copy、body copy/partial拒绝、source/backend/body Close等待登记RPC、原租约实际自然到期且永久槽仍占用。optional预算fixture清理只在原cluster与原leaseID精确匹配的native TimeToLive证明TTL<0后跳过该原始Revoke；默认fixture不增加此调用。

## 证据与边界

author报告2893bytes，SHA256 `4da2e2457ffcd28256012368d989e0913e388a16bdd8386c1c4dcf1d06e8475d`；evidence21048bytes，SHA256 `a3b0d3fcb6c3cd57ff19f892539dfb1004c8dc7da94de44817f5b1054f4f7cc3`。Root已核验实际测试叶子名称及跳过列表与日志一致，无重复执行测试。

修复轮仅在body锁下增加`body.complete`拒绝，并添加bytes0/complete=true空body回归，保证ErrInvalidRecord、draft nil、在native RPC之前返回。compiled RED→GREEN；修复后scoped host race实际1叶子2.023秒、native AtomicRegistration race实际1叶子2.746秒，零SKIP/WARN，包vet/Linuxarm64 CGO0编译/格式/diffcheck均通过。原38叶子属于7391a5e基线，未声称修复后重新执行全量测试。Root另核验最终12文件60项以及修复8条命令日志长度/哈希，实际cross产物仍为static ELF64 ARM aarch64。

追加修复后的author报告4078bytes，SHA256 `526f33eba86b2e4077a60d89662837ab28fe09be2ec290a1a33c80a965ac3ff2`；独立fix1 evidence9349bytes，SHA256 `3e0d3a816ef6c4ef484f1e8e13b8376edf97551d3f7ce6ed2127f37005b91a3b`。基线evidence保持原内容；证据对应的提交和执行范围明确区分。

唯一review完整读取任务diff一次；为具体并发/契约风险核查既有begin/end/Close/local/own/Read/cursor、native回复头/配置读取/workspace/comparison校验，未扩大为整分支review。修复复审仅看两路径diff及覆盖证据，未重跑测试/Git/hashes，确认原finding已解决且无新增问题。

故障wrapper控制真实native dispatch/reply的完成边界，不是kernel/socket阻塞或Raft分区。64候选上界测试由operator注入不可变occupied slot模拟保守unknown；跨Backend竞争则使用真实原始流程，两项不能混称。

首次ACK丢失且尚未认识birth时，operator同Txn将intent+slot两者exact bytes delete/recreate，初次只读仲裁无法识别原出生；OutcomeCommitted仅表示匹配永久元数据当前存在。已知birth/coldreference继续精确拒绝ABA；未知仍禁止重发、换槽、派发、退款或据此安全释放。source重复整archive SHA和约每32KiB两次nativeguard的成本仍待优化/实测。

下一步仍须实际可信worker身份/物理target、每个真实wire的native派发与确定结算、sticky unknown恢复和安全释放，再进行生产接线、分区调度、FUSE/collector、大N容量验证、部署恢复和Redis移除。
