# Launcher kernel boundary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现真实 Linux PID1/monitor 全线程不可逆降权，并用实际用户exec验证五种capability、NNP、UID/groups与管理proc/FD隔离。

**Architecture:** Task1交付kernel模型、真实syscall与角色检查，并由Root执行实际PID1/monitor与negativefixture；Task2在已验收边界上补真实非零用户exec、并发/Fatal清理反例、canonicalnative脚本与最终gates。没有生产raw-start入口，authority/target/全部后代排空仍是之后完整迁移必需部分。

**Tech Stack:** 既有Go1.25.x、CGO0、x/sys/unix、真实Linux/proc、现有pinnedlocalimage；无新依赖或生产部署改动。

**Spec:** docs/superpowers/specs/2026-10-07-launcher-kernel-boundary-design.md。前journal永久审查与验证已在2026-10-07-runtime-target-journal-final-review.md/verification.md，source4dd09c8、script34b9a03；所有当前findingsclosed。

## Global Constraints

- 新package internal/runtime/launcher；kernel_types.go、kernel_linux.go、kernel_unsupported.go（!linux），必要同责任inspect/mutate文件允许；canonical脚本scripts/test-launcher-kernel.sh。无新dependency、不改controlprotocol/etcd/journal/Manager/config/runtimeadapter/image/FUSEmounter、不push/merge/deploy、不碰Sentinel/用户配置凭证。
- BootstrapPID1()(*KernelBoundary,error)、PrepareMonitor()(*KernelBoundary,error)、(*KernelBoundary).Snapshot()KernelSnapshot、(*KernelBoundary).ValidateCurrent()error；privateboundary不从snapshot/history/descriptor重建，Snapshot仅复制最后诊断，无执行/ready/terminalauthority。
- 初始PID1必须getpid()==1，所有线程四UID/GID=0、P/E/B=0x1e0、I/A=0、NNP=1、securebits=0，dumpable0或1；cap_last_cap8..63。最终P/E/I=0xe0、A/B=0、dumpable0、subreaper1；瞬时SETPCAP不可逆移除。
- monitor必须PID>1、四UID/GID=0、初始P/E/I=0xe0、A/B=0、NNP1、securebits0；最终P/E=0xe0、I/A/B=0、dumpable0、subreaper1。真实rootexec继承后必须再PrepareMonitor，不能假定subreaper/dumpable继承正确。
- 全线程setter只用CGO0 AllThreadsSyscall，capset pointer稳定/runtime.KeepAlive；失败无boundary/无回滚/无gate监听用户代码。proc线程page128/max4096、status≤64KiB、退出导致完整retry≤3；ValidateCurrent真实重新检查，无idle timer/goroutine/cache/etcd访问。错误ErrUnsupported/ErrUnsafeKernel/ErrKernelUnavailable保留cause，nil/zero Snapshot全零/ValidateUnavailable。
- controller owns真实Linuxfixture，worker不自行Docker生命周期。reuse gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1；freshsandbox-launcher-kernel-test前缀/exactlabels/networknone/default独立PIDnamespace/UID0/root+binaryreadonly/test-ownedtmp/dropALL四cap/NNP；memory64MiB、pids128、CPU0.5、GOMAXPROCS2。negative初始条件另freshownedcontainer，no privileged/hostPID/hostsocket/pull/用户配置mount。
- 真实用户exec四UID/GID1000、groups空、P/E/I/A/B0、NNP1、明确完整env、仅stdio；真实setuid0/cap增加/NNP反转/proc/ptrace/管理FD/secret读反例，不用stdout自报代替父读取proc。新async清理release/cancel/≤5sjoin先于hookrestore/FDcleanup，真实Fataloracle。
- actual exited/Runningfalse/test+attach+script退出/独立exactinventoryzero留证；CGO0compile不是执行、hostskip不是Linux、native不是Linuxrace/整体隔离证明；不声称本单元完成实际业务launcher、后代排空、原镜像overhead或整体phases1–5。

## Review Focus

1. 单线程capability变化与新runtime线程：Task1真实threadpreflight/后验/ValidateCurrent覆盖，不能以一条capget作为全线程证据。
2. rootexec后dumpable/subreaper/I变化与用户groups：Task1monitor实际exec，Task2用户新增thread/proc验证。
3. 部分不可逆失败或错误bootstrap条件：Task1缺NNP/缺多cap/错PID/线程差异拒绝，真实fixture不能返回boundary/启动用户代码。
4. FD/env/proc读取旁路：Task2真实rootsecret/管理FD/环境/ptrace与用户syscall拒绝，不用单纯mode断言。
5. Fatal时仍阻塞user/helper：Task2先release/join及真实子进程negativeoracle；Rootfixturetrap/exactterminal+inventory验证。

## Task 1: actual kernel bootstrap and fresh monitor boundary

**Files:** Create internal/runtime/launcher/kernel_types.go、kernel_linux.go、kernel_unsupported.go、kernel_models_test.go、kernel_native_test.go、kernel_unsupported_test.go；按责任加kernel_inspect_linux.go/kernel_mutate_linux.go。Task1worker可准备自己编译的固定test-mode binary/Rootfixture调用说明，Task2负责canonical产品script。

**Interfaces:** 定义spec完整KernelSnapshot/KernelBoundary/三sentinelerrors与全部公共签名；private kernelRole uint8（PID1=1、monitor=2）及validateKernelSnapshot(KernelSnapshot,kernelRole,bool)error，bool=true检查初始角色、false检查最终角色。平台实现依spec。Task2消费公共Boundary/实际TestMain/helper接口，worker报告exactprivate测试模式、参数/env、exit/stdio/wait接口，不猜未实现全局。

- [ ] TestKernelModels写精确validPID1初始/最终与monitor初始/最终；每个UID/GID位置、五capsetbit差异、NNP/securebits/dumpable/subreaper/PID/CapLast/Threads边界与nil/zero；无schema自反测试。编译stub下`go test ./internal/runtime/launcher -run '^TestKernel(Models|Handles|Unsupported)' -count=1 -v`真实行为RED，helper/compile错误另记。
- [ ] 实现模型+unsupported及实际Linux proc/syscall顺序；真实proc不能fake，全线程setter不可单线程fallback。TestMain固定nativefixture模式使positive真正PID1；rootmonitor实际exec再PrepareMonitor、读取copy/ValidateCurrent/currentrole。native测试仅Controller明确环境开启，host无权限/平台限制如实报告。
- [ ] 实际negativefixture覆盖NNP缺失、缺/多cap、非PID1、重复role、单线程差异；必须读取实际前后状态、返回nilboundary/error而不是伪kernel成功。worker sourcefreeze+CGO0GOOSlinux/实际imagearch编译，Root实际运行positive全Task1package0skip与必要negativecases后workerDONE；source修改需Root重跑必要selector。native行为RED若产生须真实保存，纯model RED和编译错误分开。
- [ ] focusGREEN/race、pkgvet/gofmt/diff、自审coherentkernel/model/inspection小切片即commit；fullreport全部错误/skip/真实source与nativeproof/privateTestMain接口，独立spec+qualitygate后Task2。报告内核线程/UID/masks/exec继承/role失败边界；0clock/Lease/authority/用户生产入口。

## Task 2: actual unprivileged exec, protected parent evidence and repeatable native gates

**Files:** Create internal/runtime/launcher/kernel_user_native_test.go、kernel_cleanup_native_test.go、scripts/test-launcher-kernel.sh；仅必要同package native TestMain/helper edits，不增生产raw-startCLI/API或修改别处。生产Kernel代码仅处理真实发现的问题，不补无用抽象。

**Interfaces:** 消费Task1已报告公共API与actualprivatefixedtest-mode。真实parent→monitor→用户使用已编译同一CGO0 testbinary固定模式；Task2完整测试driver负责明确Credential1000/1000/groups0/AmbientCaps空/stdio/cancel释放≤5秒wait。父读取活着的用户每个实际线程proc，stdout只传同步或诊断，不能自认证capset。

- [ ] 写TestKernelUserIsolation真实额外thread、rootexec父/monitor状态与用户四UID/GID/groups/五capset/NNP，独立parent实际proc证据；setuid0/capraise/NNP关闭/ptrace/proc environ/fd/secret反例及仅stdio/无管理env。编译可运行stub或真实缺失检查行为RED；只改变oracle前置错误不算RED。Root owns实际LinuxRED/GREEN必要执行。
- [ ] 写TestKernelWorkerCleanup与实际Fatalchildpositive/negative independentoracle；先登记release/cancel/有界join后可能Fatal，恢复hooks/关闭FD晚于join，缺证据/没wait必须不打印成功marker。保留每个user/helper实际退出和超时，真实错误不可被预存t.Failed掩盖。
- [ ] canonical脚本实际inspect既有pinnedimagearchitecture，编译CGO0 testbinary，fullnativepositive0FAIL0SKIP与必要freshnegativecontainers；上述exactresource/security/terminal/trap/inventory契约。worker仅bashsyntax/编译+freeze，请Root实际执行后DONE；清理只有exactowned资源，脚本个别rm没有观测则不claim。
- [ ] focusedGREEN/race（host适用纯model；Linuxnative与race区别）、pkgvet/gofmt/diff/source自审即小commit；fullreport真实proc/FD/syscall结果、setup/behavior错误、nativeexit/source/counts、可测RSS/FD/threads，明确没有原镜像/全N/namespace/后代terminal/远端settlement证明；独立gate。
- [ ] controller fullhostpkg/race、actualcanonicalLinux、新repo test/vet/build/scopeddiff及一次fullNEWdelta+namedaffectedintegration review；ONEgroupedfinalfix/scoped复核如必要，永久保存全部裁定/代价/证据再清自己workspace。继续完整认证target/monitor后代排空与整体phases1–5，不能停在kernel前置层。

## 自审

| pair/task | 产生/消费与检查 |
| --- | --- |
| 1→2 | 公共KernelBoundary与actualTestMain fixedmode→用户exec/parentproc证据；私有接口Task1实际报告再传Task2。 |
| Task1 | snapshot四角色精确数值/全线程proc与setter→模型RED+实际PID1和rootexec；host不假称root通过，Task2script不提前占位。 |
| Task2 | 真实非零用户exec与worker清理→直接proc/syscall/FD/Fatal反例；不新增未授权生产raw-start入口。 |

两个独立task分别允许reject实际kernel初始化或其user隔离验收。瞬时SETPCAP与NNP前置、错误无法回滚、FUSE后续拓扑、platform/native/race/resource成本已明确，无未定义Task2接口（必须Task1报告实际testdriver）、无用户审批/外部部署动作。
