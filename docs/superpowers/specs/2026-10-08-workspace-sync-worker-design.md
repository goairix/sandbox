# Workspace 同步 worker 的原始进程身份

继续已批准的2026-10-06主迁移方案，持久put意图7391a5e/27feaf6已验收，验收记录3688e5c。此单元补齐实际worker原始kernel/process归属，供下一provider接线使用；不是provider派发、凭证隔离的完整部署证明、请求结算或安全释放。当前KernelBoundary只支持root PID1/monitor，不能把它们或配置WorkerID复用成sync worker证据。

## Bootstrap 与原始身份

在既有launcher新增私有roleWorkspaceSyncWorker；公开`BootstrapWorkspaceSyncWorker() (*KernelBoundary,error)`复用同一个process-global单次initialization门闩。先检查实际kernel，再做固定硬化，再检查，失败或重复尝试均不产生boundary且调用者必须终止该worker进程。只在owned Linux CGO0静态native fixture执行真实硬化；禁止在开发主机测试进程执行。旧PID1/monitor语义、caps、入口与seal完全保持。

worker允许实际PID>=1（独立Deployment容器通常为PID1），要求各thread全部四UID等于同一非零UID、全部四GID等于同一非零GID，且进程所有thread UID/GID完全一致；不接受调用者标签代替kernel observation。Permitted/Effective/Inheritable/Ambient/Bounding五cap集合均0，deployment已设置所有thread no_new_privs=true，securebits0，capLast8..63、threads1..4096沿用原限制。initial dumpable只允许0/1；固定硬化清空cap/ambient、所有thread dumpable0、process subreaper=true，最终重新检查。不能通过临时提升、添加cap或放松旧role通过。

新增复制诊断`WorkspaceSyncWorkerReference`与`KernelBoundary.WorkspaceSyncWorkerReference()`，严格绑定随机非复用incarnation UUID、真实Linux boot UUID、实际PID/start ticks、实际非零UID/GID、PID与mount namespace dev/inode、原始executable dev/inode。public literal/reference.Validate至多验证结构，不产生原boundary或实际worker权限。nil/zero/copied/foreign-role boundary不得返回可用worker reference；Snapshot与reference都为复制诊断。原boundary固定UID/GID、process start、boot/ns/exe和incarnation，ValidateCurrent新鲜核对真实kernel及原seal，发现任一变化拒绝，不能把新进程重新认作原worker。

capture只打开固定/proc及当前os.Getpid的数字目录（禁止caller路径），pin真实PROC_SUPER_MAGIC；复用procOpenAt/procReadAt/sealStat等既有有界helper，固定ns magic links仅在当前原始proc目录下打开并核对NSFS_MAGIC。boot_id<=64bytes、stat<=16384bytes；PID/start字段严格解析（comm可含空格/括号，不能按整行简单Fields），拒绝missing/overflow/zero PID或start、PID不等于实际self。所有FD均CLOEXEC、一次observation结束即Close，不开放FD/path/callback。seal中不保存凭证或无限proc dump。constant boot与incarnation不能由env或caller传入。

原KernelBoundary可以保存私有workerseal，但禁止复用PID1seal伪造worker；worker ValidateCurrent应确认原UID/GID和完整seal，而不是仅重新接受任意非零UID。shared inspector增加跨thread worker UID/GID一致检查，旧roles不变。无需每sandbox actor、goroutine、map、后台ticker，bootstrap一process一次，ValidateCurrent按实际active work调用。

## 证明范围

这个boundary证明原始process当前kernel状态和可复查incarnation，不证明该进程已登记为生产Deployment、用户进程已排空、对象存储target权限、凭证不可读或provider IO已结算。未来真实adapter必须同时绑定原boundary、物理target、native持久意图和登记/Close gate，并建立凭证/FD隔离及实际请求物理等待。不得将此diagnostic reference写作Prepared provider capability，不能增加公有可信callback/success bool或由reference重建原boundary。Redis/main/Helm/凭证与部署本单元不改。

## 验证

新selector仅`^TestWorkspaceSyncWorker`。先host模型语义RED（实际合规非零UID/caps0快照被当前unknownrole拒绝），再实现；严格role/UID/GID/thread/cap/NNP/dumpable/subreaper bounds与referencecopy/null/badUUID/badPID/start/ns/exe结构。Host/unsupported不得运行真实bootstrap硬化，darwin返回ErrUnsupported。

新独立script使用已本地pin的etcd3.6.15 image（与现有launcher native fixture同digest，--pull=never），编译Linux原架构CGO0静态testbinary；只运行新selector。positive容器user65532:65532、cap-drop ALL、NNP true、read-only、network none、64MiB/pids128/cpus0.5，实际worker bootstrap/ValidateCurrent原seal/ref/Snapshotcopy/role-copy/repeat失效均验证。另外真实negative容器root UID、GID0、缺NNP、额外boundingcap拒绝；各容器通过实际terminal状态及docker wait/attach退出核验，不以client退出代替。TestMain只加固定new native mode分支，旧env/modes/default保持。

独立worker子进程使用同一固定testbinary验证真实PID/start/incarnation与原进程不同；不把其JSON/ref当权限或重建boundary。进程组/attach超时必须cancel/kill并物理wait，cleanup仅确切project/fixture labels的本次owned容器，结束库存为空；不下载image、不读凭证、不使用完整docker inspect或运行旧selectors。原system kernel/thread mutable tests必须freshprocess，不让host模型执行syscall mutation。

native CGO0是AllThreadsSyscall契约，无race instrumentation（race要求CGO）；host纯模型跑race，vet/Linuxarm64CGO0compile、gofmt-d与diffcheck；清楚区分host/native实际leaf数、zeroSKIP/WARN和失败native诊断，不宣称kernelrole等于provider/生产迁移或大N容量证明。所有代码/测试/script/docs/格式修改apply_patch，小而连贯提交，唯一独立review。
