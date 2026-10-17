# 永久 32 槽与真实释放后的第 33 次申请

## 验证范围

新增一个 Linux native 场景：同 cell 的独立 metadata 进程以原协议建立 31 个永久 S/E，
第 33 条 Task 先处于 selecting；另一个原 automatic mounted worker 执行真实 runtime
清理，占据第 32 槽。原 TLS proxy 在 archive-bound 真正提交后丢 ACK，原 job 完成
join 但保留 occupation。metadata 角色不创建 Pod、Run 或 physical TaskClaim。

在自然自动恢复前，额外申请遍历 32 个候选全部冲突；原 64 条 S/E 字节和全部 MVCC
字段不变，另有 selecting E33，总数 65。原同 session worker 随后正常恢复，完成真实
归档、终止、provider 清理及签名 owner release，E settled、Run joined、原 S 删除。
只读 enrolled Backend 认证 release 历史后，原第 33 条申请才重试并占用同一释放槽。
其余 31 个 occupation 保持不变；STOP 不删除它们，最终仍有 32 个永久槽。

两个 test ELF 使用 root-owned 0600、有 64KiB 上限的 typed 输入传递原 fixture 材料，
固定双 pipe 的 CHECK33/RETRY33/STOP 协议，各 ACK 至多 4096 字节。metadata 使用原
Claim 32–99 的自然 earned credit；物理 worker 使用 0–31。两角色均按原会话续租，
所有命令、reader、RPC、proxy、worker 原进程和 supervisor 都实际 join。

## 实际结果

- selector `^TestCleanupPendingPersistentCapacityNative$`，env
  `SANDBOX_CLEANUP_CAPACITY_NATIVE=1`；固定第二 ELF `/fixture/capacity-metadata`。
- `capacity-native-01` 失败，原物理 worker 已成功退出，但父观察器先读到旧 E，再收到
  exit 通知而误判。仅修正新观察器：exit 后做一次最多两秒的最终 E/S 读取，全部原
  身份／Run／phase／release／S 断言保持；不满足立即失败，不扩测试或生产超时。
- `capacity-native-02` PASS，40.85 秒；seed/check/retry 各 276/63/62 个原计费 turn，
  CHECK33 新增 32 个冲突，最终恰复用槽 2。原物理 job 与恢复使用两次 TaskClaim，
  archive/close/quiescence 不重复，provider 恰 1 PATCH、1 DELETE。
- metadata build/vet 各 9166 个实际输入，最终 runtime build/vet 各 9354 个；四轮
  均由 Root 独立核对 17 类输入、source/tool 全归档、前后摘要与当前字节通过。
  两份 ELF 分别构建，未将 controlrunner 的私有类型导出给 etcd 包。

成功原始日志 6832 字节，SHA-256
`d022ed6e68a2608f0b362474c91114de691ae76debef47861c49e4e37dbe801d`。
首轮失败原始日志 7389 字节，SHA-256
`f3c711d4c8daa73be1e88324355de61a73ae27bd368be96d4417185f2f0426bf`，完整保留。

Root 独立核对 actual Docker command、四个只读 ELF 摘要、前后 inspect、512MiB／
2CPU／256 PID、private cgroup、network none、drop ALL 后六项 fixture setup capability、
no-new-privileges、默认 seccomp、128MiB tmpfs、真实 exit0/no OOM 和容器精确清除。
原 TLS fixture 单端口/token 告警、故意丢 ACK 的 deadline、已归还 lease 的重复清理
NotFound 及 etcd listener 关闭日志均保留，没有静音或声称日志 pristine。

本片仅证明全 cell 持久槽仲裁与一条真实物理释放的互操作，不是 32 个物理沙盒并发、
三成员容量、万／十万／百万存量容量或完整 I3 验收。真实重复／busy／unknown 与后继
物理任务公平性、压缩后完整 frontier 恢复及其 Close 组合仍开放。人工检查了新文件
职责、约束注释、阶段排版和 pipe／goroutine／进程回收顺序；旧 helper 和断言未改。

同一原 pending Task 审查员限定复核：本 slice Spec/Quality 通过，无新增 C/I/M，
I3 持久槽拒绝与释放后复用证据项 addressed；完整 I3 仍 OPEN，I1/I2 不重开。
