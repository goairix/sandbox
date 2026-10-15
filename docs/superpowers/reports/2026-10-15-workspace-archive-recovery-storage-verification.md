# 持久归档与原始归属验收

本单元接受源码2719d7b、0d213d6、8778ead、e14c92c、77f0abd、95c9999、2773087、5a742a3、bea7f2d。最后两项分别修复初始化持生命周期锁阻挡Close，以及解锁后同prepared并发重复收费。2026-10-15基分支合并45ebc70不改变这些etcd归档源码。

## 实现与边界

Linux opt-in recovery profile，和legacy spool互斥，要求recycling budget。固定最多64槽、每槽tar/json/next，M65536、TAR≤JobBytes−2M、索引I与4M工作额度先收费。Close保留文件与收费；冷corrupt/orphan/next均收费quarantine，超额已知文件导致整次New拒绝，next不自动提升。

pending在Open/Send前持久化，received在实际Session.Close后封存，bound在首次local intent前保存完整旧plan/local pin。后续文件不能换target。历史发送会话截止时间不撤销已接收消费者的当前权限；source/body/target/claim和实际Close仍约束绑定。

严格canonical固定schema，上界53121，最大合法实测bound47925；shared bounded buffers、typed candidate和metadata等待均在明确额度内。该算术不等于进程RSS或部署容量。冷快照只返回固定诊断字段，不提供source/witness/claim/reader/retire或执行权限。

R2在短life→claim同步下封闭初始化attempt，再释放锁执行IO。重复申请在收费前拒绝；nil/error明确未收费可重试，closed原spool/error只标记已收费不确定失败并永久封闭，不退款、不构造权限。初始化通过原store active/ownerDone登记被Close取消并实际join。

## 已验证与失败记录

- 最终完整新host race24叶PASS、4个Linux-only SKIP；最后最大typed-layout检查4叶PASS。Linux针对性文件异常24、quota3、failure3、Close7均PASS。
- 原始两文件归档→本地写入、冻结binding、真实31秒发送窗口过期后首次binding/写入PASS。实际owned child的13个锁/持久窗口按原全批前4和后续3/2/4分组覆盖，并保留真实Wait和清理。
- R1有clean contract RED2→GREEN2；R2有duplicate RED2及精化quota RED2→GREEN2，受影响R1 Close2兼容PASS；最终host并发/未收费重试race PASS，vet/gofmt-l/bash-n/diffcheck通过。没有为复审重复运行作者suite。
- 早期combined native整批58PASS/19FAIL，实际OOMKilled=true；后续分组仍接近512MiB，不能宣称完整可靠性或容量通过。未采集当时cgroup历史，不能断言每个早期RPC deadline的原因都是OOM。
- R1首个失败fixture在真实RED后cleanup重复release导致panic：保留attach Wait2，但三个etcd原Cmd.Wait缺失且不可补造。Root核对精确已停止容器/双label、保存原logs后清理；修正fixture后另有完整join的contract RED/GREEN。无隐瞒或替换原始日志。
- Root复核独立稳定SHA清单300+61+59条均MATCH，源snapshot/最终host补充/实际native编译输入和二进制分epoch保留。95c9999 native源不冒充2773087或R2最终host输入。最后R2实际attachWait0、terminal非OOM/exit0、exact cleanup empty。

## 独立审查

原唯一review发现R1，scoped复审发现R2。暂停后旧reviewer进程不可恢复，Root记录ruling，由一名fresh replacement仅完成未结束的R2 gate：R2 ADDRESSED、无新C/I/M、Missing0/Extra0，R1 Close顺序保留；未重审整个单元或分支。所有阻断项关闭，M1容量风险明确留到master容量验收。

原始证据、作者报告、review/R1/R2限定报告保存在本单元OWN；完成后关闭该目录，不重读/重派已完成任务。后续使用本验收与当前源码接续。

本验收只接受持久诊断归档与原始归属，tmpfs/fsync/SIGKILL不证明断电持久性。正常恢复执行、新权限/实际writer排除、完整同步与安全释放、生产接线、分区/FUSE/collector、容量与部署恢复及Redis移除仍未完成。主程序仍Redis。
