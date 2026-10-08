# 原始 workspace 同步 worker 身份验证

源码`cdb56126c9faeb680c19f34c36a4cd77b9b228eb`，脚本修复`b75fbeb5c0cc003044d940fa41f4765fa24f47d5`，基线`06cce140d52cf53243a739f6862001503a494177`，总计11个源码/测试/script路径。唯一独立review发现一个attach超时句柄保留缺口，原作者独立小提交修复，针对性复审ADDRESSED。最终Spec Compliant、Quality Approved，剩余C/I/M=0/0/0，本单元已验收。

## 实现与证明边界

新增原始非root sync worker kernel role，复用process-global单次bootstrap，失败也消费尝试；真实非零UID/GID在全部四凭证槽与全部观察thread一致，五cap集合均0、NNP已设置、securebits0，硬化后dumpable0/subreaper=true。原PID1/monitor条件及旧TestMain默认/modes保持。

原始私有seal固定真实self PID/start ticks、boot UUID、PID/mount namespace dev/inode/type、executable dev/inode、procfs与UID/GID及随机incarnation。ValidateCurrent对照原身份，不接受另一个合规非零UID/GID。public reference/Snapshot是复制诊断，literal/复制记录不能重建原boundary，更不授予provider权能。固定proc路径、严格有界stat/boot解析、CLOEXEC和FD关闭，无per-sandbox actor、后台ticker或全量镜像。

真实native首次硬化后读取namespace目录EACCES，诊断确认非root+dumpable0令该目录root-owned/read-restricted；worker-only O_PATH目录FD解决搜索/观察需求，并保留Fstat/Fstatfs/PROC/NSFS/type检查。没有加cap、恢复dumpable或改变旧procOpenAt。原失败及清理日志保留。

这只证明原始process/kernel身份，不证明Deployment登记、凭证/FD隔离、实际物理target权限、provider派发/settlement、安全释放、容量或生产迁移。真实IO adapter仍须绑定原boundary/source/native意图、可信target、登记与关闭等待，以及确定终态或sticky unknown。

## 最终验证

Go1.25.6，仅新selector`^TestWorkspaceSyncWorker`，未重跑旧验收suite：

- Host darwin/arm64 race：34实际纯模型/reference/unsupported叶子，36全部PASS，2.217秒，零FAIL/SKIP/WARN；无真实硬化syscall。
- 五个实际fresh Linuxarm64 CGO0容器：positive user65532:65532/caps0/NNP；root UID、GID0、缺NNP、额外CHOWN bounding cap各拒绝。positive6实际kernel/身份子叶，4实际拒绝叶。全部真实terminal、attach join、docker wait与确切owned清理通过，container/volume/network库存为空。
- positive日志59PASS/55总叶，negative各53PASS/50总叶包含重复模型、14 parser叶和mode/platform guarded no-op；这些不计作额外实际kernel场景。所有最终native日志零FAIL/SKIP/WARN。
- 实际原进程PID1/start2386347/incarnation c76e2cd3..与固定binary子进程PID12/start2386355/incarnation2be5ad17..不同，子进程物理Run/Wait已完成。private saved-pin测试损坏的是保存期望，不是改变live kernel；混合非零thread UID/GID未在caps0进程中注入。
- 包vet、Linuxarm64CGO0编译、gofmt-d、bash-n、diffcheck均exit0。实际native与cross binary同为6030361bytes，SHA256 `a842499c6cd7e0ca6818f5d255bf9ac95f49bd4140e6af46e20159c2ef2cb3bc`，独立确认static ELF64 ARM aarch64。native CGO0不是race，AllThreadsSyscall与CGO契约明确；host模型race不能冒充native硬化race。
- 使用已本地pin的etcd3.6.15镜像`sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1`，pull=never/read-only/network none/64MiB/pids128/cpus0.5；focused inspect，无凭证或完整Docker dump。

## 超时清理修复与证据

首轮Important finding只涉及新script：join失败后timeout忽略错误并清空attach，cleanup丢失未退出PID。修复保留PID、传播失败，只有实际wait后清空；有限期限仍无法join时保留PID/mode/project/fixture/unjoined状态，不打印完成。Go源码未改。

ACTIVE OWN harness提取真实函数及timeout代码，先RED后GREEN；真实TERM child、忽略TERM后KILL child均物理wait。第三个真实child仅对其精确PID抑制TERM/KILL，kill-0仍是真实观察，timeout与cleanup两次失败后PID保留、inner exit1/unjoined diagnostic；原始supervisor最后真实KILL/wait137并join。该故障用例模拟空Docker库存，只证明客户端生命周期/句柄保留，不证明不可杀死的Docker进程或kernel阻塞。Terminated/Killed是预期真实wait输出，未伪称无信号输出。

修复后仅重跑一个fresh native positive，wrapper在内存收窄当前script循环，其实际create/attach/terminal/wait/owned cleanup保持，positive6实际子叶和重复模型分开，零FAIL/SKIP/WARN，库存为空；四negative与Go/race/vet/cross未重复执行。bash-n及script diffcheck通过。baseline五kernel场景属于cdb5612，fix scoped证据属于b75fbeb。

Root两轮独立核验最终11文件current/Git SHA/size/blobID/content共55组checks；baseline97与fix27原始metadata/log证据、实际binary、report的长度/哈希通过；十个Go文件及actual binary相对基线不变，5 baseline和1 fix actualterminal及空库存核实。无重复执行测试。

baseline报告9023bytes，SHA256 `0ba8c7756706698f35fa0651cd8128f0d819efd7ba6f9ab7ee2e0789c3c23102`；baseline evidence33942bytes，SHA256 `ebccf04578775a95a7728e510efa58883155b299ad6091d4c1bc648d0b4ba5c9`。追加fix后的报告13647bytes，SHA256 `8399ce6086283333bdc326ab94108dc50d8323aa54f5fe4b6abdf03ecc5c5869`；fix evidence13743bytes，SHA256 `e4d0b0a3d7edb435218e34f87d7c45859d37b413b2c9f7fa9e2f2395ab88fe47`。baseline evidence未覆盖，原report hash属于追加前快照。

唯一review读完整任务diff及具体shared helper/inspect风险，核对有界读取/FS/FD契约；fix复审仅script diff、提取harness和RED/GREEN/positive证据，确认原finding已解决，无新增问题。全分支review仍待完整迁移实现后执行。
