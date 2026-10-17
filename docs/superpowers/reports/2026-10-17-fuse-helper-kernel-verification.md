# FUSE helper 本地原生进程身份与资源生命周期

本片将 `workspace-mounter supervise` 接到固定 `NewNativeSupervisor`。生产配置只接受
四个本地绝对路径；固定真实 CommandRunner、bundled profiles 和内核／文件系统检查，
不接收 PID、FD、身份字符串、runner 或验证回调。Locked 启动允许目录尚未创建，
构造阶段不创建缓存目录，也不要求 `/dev/fuse`。

Linux 构造捕获实际 PID1 的 proc、pidfd、PID／mount namespace 和 executable 五个
CLOEXEC FD，检查 procfs／nsfs、真实 boot ID、starttime、dev/inode 与 pidfd 存活。
后续检查同时比较保留对象、新读当前对象和当前调用线程的 namespace。Bootstrap／
Authorize 的最终校验与本地发布／Start 保持在同一 OS thread；不宣称全线程隔离。
失败永久关闭新准入，但保留原句柄和原 Shutdown 清理能力。原 Supervisor 指针绑定
在获取 mutex 前拒绝值拷贝，拷贝不能撤销或关闭真实 owner。

Close 在原始子进程尚未 join 时返回 pending 并保留全部 FD；原 processDone 关闭后才
逆序释放，重复返回首次完整关闭结果，不触碰随后复用的 FD。main 的立即 defer 覆盖
启动早错；服务组 join、原有 bounded Shutdown、最后 Close 的顺序保持不变。没有
改变既有 main 忽略 Shutdown 返回值的条件，也没有把进程退出后的 OS 回收作为业务
safeRelease。非 Linux 明确拒绝 native 构造；可注入 NewSupervisor 仍供原模型使用。

## 验证

证据目录：`.superpowers/sdd/2026-10-17-fuse-helper-kernel`。

- `helper-kernel-red-01` 是初始编译 RED：仅缺批准的新 NativeConfig／factory API，
  未执行行为断言。2099 个实际输入，日志 299 字节，SHA-256
  `f846d5ff2dc1293932e1718249bf852cba4db152c3b45f7e1d2385ef47fffda5`。
- `helper-kernel-host-01`／`helper-kernel-race-01`：完整 mounter 包 110 组通过，
  输入分别 2101／2108。两份日志均 19536 字节；SHA-256 分别为
  `95ee26ee487715a0a5eabca2f2dd2c16b831ea22dac8d11caf3f1787a823117c`、
  `2715545a9173ff19a7995d7c57dad57a477ba59c774ce796a635aeb5ea82388f`。
- `helper-kernel-build-01`／`helper-kernel-vet-01`：Linux arm64 测试编译与 vet
  通过，各 2157 个实际输入。Root 独立核对上述各轮全部 17 类 Go 输入、source／
  tools 前后摘要、完整归档和当时源码后才继续下一轮。
- `helper-kernel-native-01`：测试 ELF 本身作为真实 Linux PID1，九个子例全部通过，
  无 skip。包括原生 factory 正例、身份失配后不可复活、FD 故障、发布前重验、
  原 Shutdown 保留、busy Close、拷贝拒绝和实际 EMFILE 部分构造回收。
  日志 1757 字节，SHA-256
  `606242352123010dae3b94b267c3b65e5eec294f7272e8125c6a23648ad5e346`；
  ELF SHA-256
  `547ea14abf2e1f90c97a76d47803e51986699f40dd40b2feabf11c8606f631f0`。

Native 容器固定镜像摘要为
`sha256:26ad49adcb6923b3faa00ee6654943de9bdda7cfcb1c42a9b9cef334a48dfb33`，
仅借用其 arm64 文件系统。network none、private IPC、只读根文件系统、cap-drop ALL、
无新增 capability、默认 seccomp、no-new-privileges，128MiB／2CPU／64PID，tmpfs
32MiB，nofile 512，只挂载一个只读 ELF。Root 独立核对实际 inspect、原始日志、
二进制前后摘要、退出码 0、无 OOM 和精确容器删除，均通过。

这些是本地进程连续性证据。部分子例将真实 owner 接到测试用进程／文件系统模型，
不代表真实 s3fs 子进程或挂载成功；身份失配子例修改保存的 starttime，没有执行
namespace identity 变更或真实 setns／unshare。未验证 helper 到 Pod 的注册绑定、全线程 confinement、FUSE 挂载、
typed flush、远端 settled、collector、双进程安全释放或 etcd FUSE profile 激活。
这些仍为后续功能，不因本片通过而宣称完整 FUSE 闭环或生产迁移完成。

## 独立审查 R1 与最终验证

唯一 Task reviewer 发现两项 Important：`Status` 漏掉锁前的实例绑定拒绝，可能卡在
复制的已锁 mutex；resident readiness 漏掉 native owner 的 revoked／closed gate，
使已经封闭准入的旧 StateReady 继续显示健康。新增两组 resident-state 回归先在旧
实现上真实失败：`helper-kernel-admission-red-01`，2102 输入，日志 7837 字节，
SHA-256 `09cba7af0e7aff0b09799e701f09e8cdff8d0d8f08e072fe9c0d2163e32cb779`。
阻塞测试显式解开副本 mutex 并 join 原调用后报错，没有遗留 goroutine。

修复后 Status 在锁前拒绝副本，原实例在锁内校验 native 身份、失败返回 Unhealthy，
不改原清理状态。readiness 仅检查 resident binding／seal，不增加 handler syscall。
回归验证 revoked、closed、busy Close 三种状态立即失去健康、refresh 不再发布正例，
原 Shutdown 仍然能够 join 原始子进程。

- 最终 `helper-kernel-host-02`：完整 112 组通过，2102 输入；日志 20268 字节，
  SHA-256 `779b766e99a267abc40713e87612eae233bb0db07694522983ce35581d0ede3a`。
- `helper-kernel-race-02`：新准入回归和原 resident readiness 定向通过，2109 输入；
  日志 1657 字节，SHA-256
  `2f8807abcbe0706007442db821b2062c4c621f3400f477279a0566c23c6df351`。
- 最终 `helper-kernel-build-02`／`helper-kernel-vet-02` 各 2158 输入，Linux 编译／vet
  通过。以上各轮均由 Root 在下一轮前完成全部输入／工具／归档独立审计。
- 最终 `helper-kernel-native-02` 使用修复后 ELF，实际 PID1 下九个 kernel 子例及
  两组准入回归（三个 seal 子例）全部通过，无 skip。日志 2489 字节，SHA-256
  `c33e3f10ab699d77194b50fcda4eebdbee6c4a025d6b2de8d30ddfc6723376f6`；ELF
  SHA-256 `9d798985ab84704e720d5ca228d1bdd981d1c8d8027e3c8fb27b5ae912a787dc`。
  实际资源限制与第一轮一致；Root 独立核对 inspect、ELF／日志摘要、exit 0、无 OOM
  及精确容器删除，全部通过。第一轮 native 证据保留为修复前历史，不标作最终字节。

准入回归中的 pointer-only owner 只测试实例所有权与 resident seal，不能验证 native
身份，也不是另一个 factory 阳性。真实 factory 正例仍来自 native kernel 子例。

实际 `cmd/workspace-mounter` 的 Linux arm64 生产 ELF 已在修复前 `helper-kernel-main-01`
和修复后 `helper-kernel-main-02` 编译成功，输入分别 2160／2161；未启动真实 s3fs。
Root 已独立核对这两轮完整输入和归档。原唯一 reviewer 限定复核关闭 I1／I2，
结合首轮其余源码结论，最终 Spec／Quality 均通过，无新增 C／I／M。
