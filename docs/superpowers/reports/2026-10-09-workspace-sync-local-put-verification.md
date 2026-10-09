# 实际 local regular-file IO 验收

源码提交 `96c21cc99b2e3f664f24652672e74840de10646c`、`2f59c937ee97e29e297f4a7b373d50d574888cd0`：原始 v2 known-ACK body/worker/target 完成真实本地 regular-file 写入与进程内 witness。当前主程序仍用 Redis；没有持久终态、退款、slot GC、安全释放或生产切换。

## 实现与检查

- 原写前门消耗 one-shot，source.busy/ops、body.active/done、target.ops 保持至所有实际 IO 与错误 cleanup Close 返回。owned read 保留原 native guard/count/hash，固定32KiB，不嵌套普通Read或临时释放 owner；成功不取消整个source，连续hello/bye已实际验证。
- already-resolved workspace root 下只使用一次 canonical relative path；openat2 BENEATH/NO_XDEV/NO_SYMLINKS/NO_MAGICLINKS，4096path/128depth。parent worker所有/0700，bounded mkdir/fsync；existing file 先无O_TRUNC打开并验证regular/nlink1/UID/GID/identity，随后truncate；missing file exclusive create，无staging/rename布局。
- 原始 bytes/hash、file identity/size/mode、file fsync/实际Close、modified parent/root fsync及所有子FD Close后，经当前native/worker/root和直接ctx检查发行exact original self/origin/issued-pointer witness。copy、自重设self和诊断重建拒绝。失败/取消/EFBIG partialwrite无witness，无重复派发，无退款。
- 保留合法archive mode0000；scoped O_PATH仅观察regular身份，不用于write/fsync，目录仍RDONLY。openat2 O_PATH不带NONBLOCK，普通打开保留NONBLOCK以有界拒绝FIFO；未新增不可写旧文件的权限策略。
- body/source/target/claim/Backend及并发Backend Close等待真实owner。实际Write或file.Close返回后的pause证明取消不提前释放，普通Read/next/重复put拒绝、原spool FD保留、nativeMu在IO期间free。

## 覆盖与限制

最终同一15个Go源码字节的24个不同实际Linux叶覆盖来自两batch：`task2-final-green`23PASS/1FAIL（EFBIG在旧ReserveTaskFinalSync setup deadline，put未开始），加`task2-error-final`1PASS/0FAIL。零SKIP，但没有单轮全绿；同源完整retry15PASS/9setupFAIL保留，不循环重跑或降低原限制。最后script只增加fixed error selector，最后完整16文件compiled snapshot等于working/Git。这只验收组件行为，不证明完整性能或容量。

实际场景含空、75000bytes/三chunk、nested/private parents、覆盖、连续两文件、mode0；symlink/hardlink/FIFO、错误parent/root权限、UID0 ownership、真实spool body hash破坏、noACK/unknown；8路关闭/取消/变化；真实Write EFBIG partialbytes="he"后intent/slot保留收费。成功输出实际SHA/mode/dev/inode及scoped FD库存核对。初put缺实现RED、移除ownership检查导致真实UID0文件改写的语义RED、mode0 permission-denied RED均有原始日志；错误TAR/frame与O_PATH|NONBLOCK EINVAL失败也保留。

Host限定race14PASS/24SKIP、0FAIL（20PASSnodes），不bootstrap；vet、Linuxamd64/Darwinarm64 CGO0编译、exactformat/bash/diff均exit0。实际Linuxarm64CGO0不是race，tmpfs不证明重启持久性。原default TAR/TLS fixture由真实连续文件覆盖，未重放Task1整套/未触及的HA或容量测试。

fixture仍pinned本地image/pullnever/networknone/workerUID65532/caps0/NNP/readonly-root/tmpfs，三个真实etcd child与真实TLS归档。独立UID0/caps0 helper只创建固定foreign leaf，原worker不获得root；最终原独立session supervisor启动并实际Wait helper与attach，任何未完成join都不允许删除container。正常ownershipbatch helperWait0；失败retry真实docker exec Wait1、无normal-ending，suite66.64s与helper60sdeadline相符，但不进一步断定根因。先前137/PID1终止推断已由raw纠正。

14个实际batch均保留三child真实Wait/full rawlogs与attachWait，42个fresh exact container/volume/network查询为空。最终23PASSbatch serverWARN62/ERROR12，补EFBIGWARN60/ERROR12；失败retryWARN62/ERROR12另client/protocolWARN4，其余client/protocolWARN0。服务器日志未过滤；每child signal与最早cluster signal作为时间边界分类，最终12errors均在首次signal后为关闭listener，不把启动schema错误当teardown。全部失败exit原貌保留。

独立review完整两个提交并核原始RED/Close/分批PASS、报告helper纠正：Spec Compliant，C0/I0/M0。Root独立审计426artifact SHA/bytes、16source working/Git/compiled records、两个最终Go snapshots、所有14batch实际叶/Wait/full-log标记、server JSON类别/数量/时间、host节点/叶、coveringexits、精确提交范围与42fresh inventories；没有重复作者suite。第一次Root审计仅phase标签拼写不匹配，改用manifest原`pre-first-cluster-signal`后核对通过，未修改原始证据。

报告SHA256 `c49989613b0c26097739ae80fe729069bc620791e1d41c5524dc0e3ea8e04e81`；manifestSHA256 `80d0adc7a08914170c6b052d95e50ea173f7e8619936222f14b284319bbce5bd`。初期controller exit有真实tool transcript及原始attach/container exit，但不是落盘raw controller文件；后期retry/error另有raw controller-exit，未伪造旧退出文件。

下一步仍需原生持久terminal/有预算的slot回收、正常故障恢复与stickyunknown、目录/删除/云provider、实际基础设施终止与安全释放，再做生产接线、分区/FUSE/collector、大N容量/部署恢复和Redis移除。重复整归档SHA、每32KiB/每FS阶段native guard的性能成本未解决。
