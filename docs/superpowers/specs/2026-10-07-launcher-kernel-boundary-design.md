# Launcher Linux kernel boundary

## 目标与范围

沿已批准的 etcd 完整替代方案继续实现真实 launcher。当前受保护 journal 只写 closed/unknown，生产 Manager、Docker/Kubernetes adapter 仍未迁移。本单元实现真实 Linux PID1 与可信 command monitor 的多线程降权初始化，以及真实 exec 子进程的隔离验收；不把 kernel 状态、descriptor、签名或 journal 历史当作执行授权。后续仍须完成认证 transport、独立 birth/BootID/activation、accepted/terminal journal、全部后代排空、双 drain、任务/调度/collector/GC、原资源开销和完整 Redis 移除。

大量已有空闲沙盒不能产生新的 etcd Lease、轮询、watch、常驻工作 goroutine 或后台 capability 检查。本单元仅在进程初始化和显式 ValidateCurrent 时观察内核。不改变生产镜像、securityContext、FUSE mounter、配置或旧调用路径；不能仅凭库存在宣布已部署 PID1。

## 方案选择与内核依据

选择 CGO0 的 Go AllThreadsSyscall，使不可逆降权覆盖已有 runtime 线程，后创建的线程继承降权状态。单线程 prctl/capset 加 Go Credential 不能证明整个管理进程安全；额外引入 C bootstrap/动态运行库会增加普通/FUSE 镜像及供应链复杂度，暂不采用。

初始化短暂要求 KILL、SETGID、SETUID、SETPCAP 四项 capability。先设置仅三项 inheritable，再清空全部 bounding，最后不可逆移除 SETPCAP，只保留 KILL/SETGID/SETUID 的 permitted/effective/inheritable；ambient 和 bounding 始终以最终零集合验收。可信 root monitor 的 exec 可通过 root 的 inheritable 规则保留三项；monitor 随后清空 inheritable，再降到非零用户 UID/GID exec，使用户 permitted/effective/inheritable/ambient/bounding 全为零。

这是从 [capabilities(7)](https://man7.org/linux/man-pages/man7/capabilities.7.html)、[PR_CAPBSET_DROP](https://man7.org/linux/man-pages/man2/PR_CAPBSET_DROP.2const.html) 推导的方案，须由本单元真实 Linux 测试确认，不能以公式代替测试。四项初始 capability 是明确的瞬时 bootstrap 契约调整，不是额外长期 launcher 权限；生产部署与 FUSE 可信 mounter 初始权限拓扑仍须后续单独接入审查。NNP 必须由部署预先设置，库拒绝缺失状态；[内核 NNP 文档](https://docs.kernel.org/userspace-api/no_new_privs.html) 说明其继承与不可逆性质，并不保证其他隔离。

## 文件与公共接口

新 package `internal/runtime/launcher`。`kernel_types.go` 为数据/错误/纯模型校验，`kernel_linux.go` 为实际 Linux 检查和 syscall，必要的同责任 `kernel_inspect_linux.go`/`kernel_mutate_linux.go` 分解文件允许。`kernel_unsupported.go` 为互补 `!linux` 构建。测试与 product 重职责分别拆分，canonical native script 为 `scripts/test-launcher-kernel.sh`。不新增依赖，复用现有 x/sys/unix。

```go
type KernelSnapshot struct {
    PID int
    UIDs, GIDs [4]uint32 // real/effective/saved/filesystem，来自实际 proc
    Permitted, Effective, Inheritable, Ambient, Bounding uint64
    CapLast uint32
    NoNewPrivileges bool
    Dumpable int
    Subreaper bool
    Securebits uint32
    Threads uint32 // 本次实际观察数，不承诺持续不变
}
type KernelBoundary struct { /* 私有 role/PID/verified snapshot */ }
func BootstrapPID1() (*KernelBoundary, error)
func PrepareMonitor() (*KernelBoundary, error)
func (b *KernelBoundary) Snapshot() KernelSnapshot
func (b *KernelBoundary) ValidateCurrent() error
```

错误为 `ErrUnsupported`、`ErrUnsafeKernel`、`ErrKernelUnavailable`，保留底层 syscall/proc cause 的 errors.Is。零/nil boundary Snapshot 返回全零；ValidateCurrent 返回 ErrKernelUnavailable。Snapshot 是复制的最后验证诊断，不是持续授权。Boundary 私有字段不能由 Snapshot 创建；ValidateCurrent 必须重新读取真实内核，检查当前 PID/角色及全部线程条件，不修改任何状态。三个函数与方法无 context 参数：不可逆 bootstrap 只在启动阶段执行，不承诺内核 syscall 可抢占。

## 精确角色契约

数字 capability：KILL=5、SETGID=6、SETUID=7、SETPCAP=8。初始 mask=0x1e0，长期三项 mask=0xe0。cap_last_cap 从真实 `/proc/sys/kernel/cap_last_cap` 读取，必须 8..63；超出范围拒绝支持，不能静默遗漏高位。所有线程的四种 UID/GID 必须全零，NNP=1、securebits=0；初始 dumpable 只能0或1。

BootstrapPID1 只允许真实 getpid()==1；每个已观察线程 P/E/B 必须精确0x1e0，I/A=0。任何缺失/多余/线程差异在修改之前拒绝。其操作次序固定：设置所有线程 I=0xe0、P/E仍0x1e0；清 ambient；对0..cap_last_cap逐一全线程 PR_CAPBSET_DROP；全线程 capset P/E/I=0xe0；设置 dumpable=0、child-subreaper=1；重新真实检查全部线程及属性，再返回非零 boundary。最终 B/A=0、P/E/I=0xe0、NNP=1、dumpable=0、subreaper=1、securebits=0。

PrepareMonitor 只允许真实PID>1、四种UID/GID全零，初始P/E/I=0xe0、A/B=0、NNP=1、securebits=0。exec可能重置dumpable，初始可0或1；subreaper不能假定fork继承。全线程清I/A，保留P/E=0xe0，设置dumpable=0与subreaper=1，再重新检查后返回。最终P/E=0xe0、I/A/B=0，其余与父相同。直接从未经Bootstrap的进程调用、重复调用或错误角色均拒绝，不自动修补缺少的权限。

NNP与所有capability集合按线程检查；dumpable/subreaper是process属性，按真实当前prctl读取。securebits是线程属性且proc不暴露；仅此getter允许CGO0 AllThreadsSyscall6(PR_GET_SECUREBITS)验证Go runtime全部线程返回值一致且为0，不使用共享输出指针。统一非零值拒绝，线程间返回不同会触发Go runtime fatal，此时进程结束且无boundary；不把当前线程单独GET声称为全线程证据。初始化与ValidateCurrent都执行这个只读检查。管理进程必须是无外部手工线程的可信CGO0 Go程序，foreign/native线程不属于此库的支持契约。使用实际 `/proc/self/task`，页128、线程上限4096，每个status最多64KiB；不保留全量线程map。枚举过程中线程退出允许完整重新检查最多3次；仍不稳定返回不可用，不用缓存补齐。PID/UID/GID、五集合、NNP、securebits、cap_last_cap、thread计数解析必须有明确边界，重复/缺失/非法数字拒绝。实际线程创建/退出带来的count变化不当作固定值漂移；角色安全属性必须一致。

AllThreadsSyscall 用于setters，唯一GET例外是上述期望全线程一致0的securebits检查；其他getter不用此方式。capset pointer稳定与runtime.KeepAlive必须正确；现有Go runtime可在全线程syscall发生不一致错误时fatal，故bootstrap不承诺可回滚/总返回error。任一失败不得返回boundary、恢复原权限、开放gate、监听或启动用户代码；真正caller必须结束该管理进程并保持外部unknown/closed。对CGO或不支持syscall返回明确错误，不提供单线程降级。未支持平台BootstrapPID1/PrepareMonitor返回ErrUnsupported；ValidateCurrent对nil/zero先返回ErrKernelUnavailable，对非零handle返回ErrUnsupported。

## 实际用户exec验收

用户进程必须由真实PrepareMonitor后的可信monitor启动，显式非零UID/GID=1000/1000、补充groups空、AmbientCaps空、仅stdio继承，明确完整环境，无管理环境/目录/secret/IPC FD。使用编译的同一CGO0 testbinary的固定测试模式，无shell、安装或用户可替换helper。不添加生产 raw-start CLI/API。

真实 parent→root monitor exec 前后读 `/proc/.../status` 验证上述集合；用户进程在额外创建线程后阻塞等待，可信父读取每个实际thread状态验证五集合全零、四UID/GID均1000、NNP=1、补充groups空，随后释放并有界wait。用户实际尝试setuid0、增加capability、关闭NNP、ptrace/读管理proc environ与FD、读root0700/0600secret以及继承管理FD均须失败。管理密钥/nonce只用测试临时数据，不读取宿主凭证。namespace/cgroup/remote-write全隔离和完整后代terminal仍不是本单元证明，后续仍需实现。

新异步worker必须在阻塞/Fatal前登记release/cancel/≤5秒join，恢复hooks与清理FD/fixture晚于join；测试子进程有界终止与wait，成功marker不得因既有t.Failed而无条件输出。报告真实PID/thread/capset/UID/group/NNP、proc/FD反例、错误、skip、child实际退出与allocated/RSS/FD观察（测不到如实说明；本单元不是原镜像overhead证明）。不把model、CGO0编译或hostskip称真实Linux通过。

## Linux fixture与当前边界

Controller owns Docker生命周期；worker只编译CGO0 binary并请求freeze后的真实执行。reuse已经本地pinned `gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1`，实际inspect Linux/arm64或amd64；不pull、不运行etcd。fresh `sandbox-launcher-kernel-test-<pid>-<time>`、exactproject/fixturelabels、networknone、独立默认PID namespace、UID0、read-only root/binary、test-ownedtmpvolume、dropALL只加四项、NNP、无privileged/hostPID/hostsocket/hostwritable配置mount。测试limit memory64MiB、pids128、CPU0.5、GOMAXPROCS=2，不改变生产用户quota。

Native positive必须作为实际PID1执行，full新package0FAIL0SKIP；需要不同初始NNP/cap集的negative额外使用精确ownedfreshcontainer，不以普通子进程伪装PID1。NNP缺失/缺cap/多cap/线程不一致/角色不符均拒绝，修改失败不产生boundary的证明使用实际子进程与实际内核状态；不替换内核返回值为fake成功。stdout必须明确“kernel boundary verified”，不得伪称runtime ready/physical terminal。脚本捕获actual exited/Runningfalse/processExit与attach/script exit，trap只清exactowned资源、独立inventory观察空；不声称best-effort rm必定成功。

最终host model/unsupported全package/race、真实Linuxnative、fullrepotest/vet/build/scopeddiff、独立task与一次全新delta/affectedintegration review；按coherent slice自审立即小提交。本单元安全性来自真实内核验证与caller启动契约，仍不授予业务执行能力。journal/fencing旧源保持，phase1–5整体不可在这里宣布完成。

## 自审

无待定接口/占位协议。两角色初始与最终集合及exec继承顺序、CGO0/全线程要求、PID1真实fixture和unsupported边界已明确。瞬时SETPCAP与FUSE拓扑是公开成本，不修改生产cap配置或把mounter替换掉。实际kernel实现与用户exec隔离反例可分两个独立reviewgate，永久证据与裁定保存后继续monitor/target执行排空整合。
