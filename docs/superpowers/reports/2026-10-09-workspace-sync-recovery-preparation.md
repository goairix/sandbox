# 正常 manager 恢复接线研究

这是当前代码的设计研究，未实现、未运行恢复测试，不能列为恢复验收。当前 checkpoint 单元仅补持久进度与 GC 原子性；用户批准的完整迁移仍需正常恢复和安全释放。

## 已有能力与实际缺口

- `task_dual_drain.go:PrepareAfterDualDrain` 支持新 claim，结合旧已提交排空历史、两次当前运行时回执查询和新鲜 whole-prefix Range 构造新 prerequisite；完成排空后 manager 更换不需重发旧排空命令。
- `ReserveTaskFinalSync` 和 `PrepareTaskWorkspaceArchive` 依赖原始进程内 draft/current claim，固定 immutable key 已占用后不能为新 claim 再申请。诊断读取不能重建这些旧权限。
- `taskArchiveOpenRoot` 拒绝非空目录，spool关闭时删除文件，没有持久 task→artifact/物理归属 manifest。文件幸存也不能正常重新打开。
- Supervisor拒绝第二次归档，StreamWorkspaceArchive消费一次，历史query只重新签署成功回执、不返回TAR。传输失败会触发isolate；简单query/retry不能恢复丢失归档。
- checkpoint TargetDigest是包含旧worker/root的单向摘要，GC删除intent后不能还原归属。需要首次效果前保存有预算的完整归属/归档/计划，checkpoint schema不需扩展。
- 当前KernelBoundary验证当前进程，尚无冷的旧worker精确终止证明。lease到期/PID不存在/复制BootID不证明物理IO结束；恢复读写前必须排除旧writer。

## 两条路径与边界

同主机、归档已完整接收的最小路径：在现有archive Job预算中保存/fsync固定artifact和manifest，重启持排他锁严格重新打开；新claim通过已有排空桥和当前archive receipt query验证历史，再建立新的worker/target/source权限。manifest保留旧canonical attribution，验证旧TargetDigest后独立比较稳定root身份，不能制造旧witness。它仅覆盖post-archive local恢复。

覆盖生产/传输期间崩溃及manager主机丢失的完整路径：runtime producer独立完成到有预算、持久、不可变的目标侧归档，允许新claim请求新的只读传输会话；旧会话需实际关闭/join/fence，新传输不重新执行producer。需要目标侧quota/journal/存储恢复、producer与传输生命周期分离、传输授权更新及重复读取协议。调整当前stream失败隔离语义前必须实测故障。仅同主机路径不能宣称任意manager故障恢复完成。

## 有界协议与实现切分

1. 首次本地效果前持久manifest：task birth、原history/command revisions、归档SHA/bytes/entries/ticket、完整plan、spool inode/存储身份和canonical local attribution。每现有Job一个固定槽，pending/sealed；建议manifest≤16KiB，需最坏编码及预算证明。原子替换最多两份，不新增无预算随机文件或etcd attempt日志。
2. 冷启动先持store锁、验证全部有界槽并恢复额度，再开放新申请；unknown/orphan继续收费，不按年龄删除。严格验证inode/hash/mode/UID/link/目录身份；unsupported boot/namespace/存储变化保持pending。
3. ClaimTask创建新generation；PrepareAfterDualDrain及新增archive-query wrapper验证当前runtime/历史，旧producer ticket不续命。物理排除旧writer的证明覆盖读写/fsync/postguard/GC；仅同进程syscall路径可用store锁证明进程退出，独立worker需额外精确join/fence。
4. 新recovery-source重开归档并重建现有有界重复路径索引。checkpoint仅是进度输入；从TAR起点解析至EntryOrdinal，核对path/body摘要及BodyEnd。BodyEnd不是可直接seek的header地址；已完成文件不再次truncate/copy。
5. 有界检查固定charged slots；未完成或terminal未GC的local effect，在旧writer确实排除后由新权限验证并fsync或修复原预期内容。保持旧结果unknown，不把当前修复冒充原成功；远端SDK unknown不能套用本地重写规则。
6. 新reconciliation自身terminal持久后原子推进checkpoint并释放精确slot/intent；绑定向新worker过渡只由此私有路径处理，普通原始GC/legacy限制保持。全部50pins、64nodes/256KiB仍需最坏实际wire证明。
7. 两个可验收单元：有预算sealed artifact和新authority/source（无目标效果）；charged local reconciliation与cursor继续（含重复崩溃）。真实测试覆盖旧进程持锁、truncate/write/fsync/terminal/GC ACK丢失各崩溃点、新claim竞争、迟到旧native事务、两/65文件继续且跳过已完成文件、tamper/quota/manifest持久化与传输失效。

同步EOF只是归档耗尽。完整目标仍需有预算sync-complete证据、实际runtime终止/mount-policy清理、外部effect结算和最终owner release CAS。缺失quiescence terminal、producer/transfer期间崩溃、远端SDK unknown须各有恢复/隔离策略；保守拒绝所有正常故障不能作为生产可用验收。
