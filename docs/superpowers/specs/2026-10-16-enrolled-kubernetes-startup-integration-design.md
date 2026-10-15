# Enrolled Kubernetes startup and bridge integration

继续已批准etcd master；本完整Task修复当前正式启动入口的实际EmptyDir权限不兼容，并首次将实际Kubernetes WebSocket管理client接到真实正式launcher bridge/PID1/observer/TLS组合。保持唯一fresh author/reviewer和多个小提交，不重开前Task/旧矩阵。它仍不是真实kubelet/CRI/创建quota/main/容量验收。

## 已确认的问题

当前provision_linux.go只接受control directory root:root 0700/0755；旧profile fixture特意创建0755。Kubernetes v1.35.3 emptydir/empty_dir.go:45、414–434使用0777；memory setupTmpfs:293–314创建目录后mount tmpfs，generateTmpfsMountOptions:530–540只加size/noswap，没有mode覆盖。必须实际挂新tmpfs（size=1m，不指定mode），从内核读实际初始mode，不能继续手工chmod0755制造兼容。

依据：[Kubernetes固定版本源码](https://github.com/kubernetes/kubernetes/blob/v1.35.3/pkg/volume/emptydir/empty_dir.go)，[Linux tmpfs文档](https://docs.kernel.org/filesystems/tmpfs.html)。旧验收只证明其0755 fixture范围，不能补写为默认kubelet已通过。新Task专用script的/workspace私有tmpfs保持size1MiB，改为root:root/mode0777，记录/tmp实际owner/mode；不再使用旧uid1000/mode0700来声称默认EmptyDir权限。这仅为permission analogue，workspace实际filesystem仍tmpfs而非kubelet磁盘EmptyDir；生产volume/security/caps不变。

## 安全启动修复

正式serve-provisioned仍只实际root/PID1。固定root/run FD祖先NOFOLLOW/root-owned/不可group/world写不变；control FD必须实际directory、root UID/GID0，仅初始exact0700/0755/0777/01777接受，其他specialbit/mode/owner/type拒绝。在持有control原FD后、检查任何子项或创建配置之前先Fchmod0700，并Fstat确认同一FD现在root:root exact0700。只有唯一enrollment.sock且root0600、无其他birth数据时，才O_EXCL/NOFOLLOW创建config0600/file+dirSync，再原serve/BootstrapPID1。检查失败只允许已做的权限收紧，所有旧文件内容/inode保留；无unlink/覆盖/重新出生/fallback。没有用户程序运行、无init/sidecar、RestartNever的固定Pod合同是允许收紧fresh目录的前提；不让任意hostPath/其他进程据此进入旧弱分支。

不改变共享profile五volumes/readonly hostPath/command/image/authority/UIDGID校验，不增加生产cap或CHOWN/DAC_OVERRIDE，不使用init脚本。允许一个纯固定mode策略私有函数用于host边界回归，但实际FD检查/收紧必须在正式Linux程序而非测试factory执行。

## 真实桥接组合

新native selector `TestEnrolledKubernetesStartupBridgeNative`，复用tracked旧fixture的源级真实服务/原Wait/join代码，不读任何CLOSED OWN。新OWN构建正式launcher及runner，从新OWN fresh never-started cachedetcd export取得binary；全部actual17输入/源码归档/ELF/script绑定独立记录。

新fixture用生产BuildEnrolledPlainPod构造同一固定Pod/Node，root目录权限来自实际无mode选项的独立control tmpfs；保留真实readonly clock/socket bind、独立observer的SO_PEERCRED和signedclock。必须先使用旧产品做新增默认权限native RED：到达正式CLI的invalid fresh control directory错误和原Wait，不允许setup/timeout失败充数；随后修复产品，fresh新build/epoch做GREEN，不重跑旧profile矩阵。

GREEN bootstrap Hello/activate可沿原已批准有限直连路径准备；管理TLS publication必须经生产NewEnrolledBridgeClient.Dial→真实GET Pod/Node→固定GET WebSocket/v5 exec→正式 `/usr/local/bin/sandbox-launcher bridge` 子进程→原protected UDS→真实PID1 Supervisor。fixture exec server只选择这两个固定command字面值，不接收任意program/targetcallback。测试helper只能从原已持有launcher child PID/namespace FD进入其真实mount namespace，完成setup后丢掉SYS_ADMIN/SYS_PTRACE，再exec本OWN正式ELF bridge；不提供可配置路径或伪造peer/kernel证据。Bridge自身仍只root字节relay，原管理TLS/Root cert/runtime boot/gate/Backend fence边界不变。

server的stdin/stdout/stderr/normalstatus转发需原实际copy和bridge Cmd.Wait+Close join；v5 `[255,0]`必须传成真实child stdin EOF并保留stdout。通过原Destination发送只读publication，请求精确runtime/issuer证书，原verifier验证ready，不以返回JSON当证明；随后原child/PID1/observer/clock/etcd Wait/Serve join、exact容器清理及emptyinventory。只需这个新组合positive和最多两个默认1777 stale/foreign出生数据negative，不重新重放其他已验收case。

## 验证与隔离

host新增mode策略RED→GREEN/race，Linux affected compile/vet和新增native RED→GREEN；原失败/源快照保留，当前与历史输入明确区分。最终一位fresh完整Taskreview，同pair scoped修复，tracked验收后继续真正创建/Pool/manager生产编排。

只apply_patch写源码/测试/文档/格式；Go1.25.6；无依赖升级/真实Redis/Kube/secrets/env/fullDockerinspect/push/deploy/Sentinel/CLOSED OWN。Root-only expectedHEAD/exactpaths common→本OPEN OWN fcntl锁小提交。每作者≤1、总≤2原隔离fixture，cachedarm64runner sha256:684aa844d79d5b300db6586a25503607838055db7e7f480a50abb7c7ee7b4e32，networknone/private/nohostPID/readonlyroot/NNP/512MiB/2CPU/256PID。仅本Task新增fixture setupcap SYS_CHROOT，原因及执行前丢弃要求见下；其余原setup白名单不变。Root明确允许本Task private /run tmpfs≤1MiB之外再有exact control tmpfs≤1MiB以模拟实际MemoryEmptyDir；无host/run mount。每次实际native命令仍先报Root检查批准；只是方案记录不授权任意运行。新源码全部Go写/GoList/native须前置bridge验收后START-GO，之前只读准备。

## 原namespace进入所需的测试权限纠正

原计划假定已有SYS_ADMIN足够，实际[Linux setns(2) mount namespace规则](https://man7.org/linux/man-pages/man2/setns.2.html)还要求caller自身user namespace内SYS_CHROOT，并禁止共享CLONE_FS。Root与作者在运行GREEN前各自核实；未把EPERM/setup失败充作产品RED。为保留原持有namespace FD和真实同namespace执行，只在新startup隔离fixture增加SYS_CHROOT；不采用另建broker、重建相似namespace或跳过setns。没有修改生产Pod/cap合同。

两个固定test helper（原PID1启动helper的startup-only分支及新bridge helper）在正式exec之前必须移除SYS_ADMIN/SYS_PTRACE/SYS_CHROOT的bounding位，清空ambient，permitted/effective只保留原四个生产cap、inheritable清零，并实际核验。旧helper在原六cap场景只drop前两项，新增CHROOT不会自动从bounding消失，故startup-only额外drop/核验不可省略。bridge helper仍LockOSThread、unshare(CLONE_FS)、只setns原传入FD，绝不重开任意PID/path。setup错误保留原退出，不能报告集成通过。代价是这个明确记录的测试setup权限例外；正式程序、现有旧selector和部署权限不扩大。

Ruling: 用户持续授权包括修复真实集成缺陷；旧Task已CLOSED，保持原历史证据，独立新完整startup集成Task修复和验证实际行为，不重复申请许可或复用已关闭pair。此前0755假设错误由本Task明确纠正，不能宣称原fixture已代表kubelet。

## 实际组合失败的新增修复范围

首个green-bridge尝试真实失败：正式PID1已从初始01777收紧并启动，但production bridge上的TLS publication约0.18秒返回i/o timeout，原bridge/PID1清理Wait为killed，原attach/container均1且无OOM，exact容器已清除。原脚本却exit0并打印FINISHED，失败日志/退出矛盾均保留；不能据包装器结果验收。

已查明Go1.25.6 crypto/tls/conn.go closeNotify:1481–1485在写出close_notify后将底层write deadline设为now；bridge当前取read/write最早期限并中止整条连接，导致尚需读取响应/发送transport FIN时立即超时。本OPEN Task允许最小修改enrolled_bridge_conn.go及controltransport/destination.go，并新增独立真实TLS→production bridge回归。写方向闲置期限到期不得杀读，恢复本次bounded write deadline后才发送真实transport FIN；方向性、已进入/排队IO、首因/partialn、物理关闭和slot等待仍须验证。不能用延迟deadline、吞错误、提前successstatus或fake transport代替修复。具体私有同步方案以原真实socket/TLS RED→GREEN确认，旧CLOSED OWN/pair不重开；仅受本次行为变动影响的旧selectors可限定复验，禁止无理由重放整套历史矩阵。

Root在本机独立复现/bin/bash3.2的裸[[false]]即使-e仍继续/exit0，显式`|| exit 37`则正确退出37。新startup脚本的必要断言须显式失败出口，保留实际日志、attach/container Wait和预期RED各自原状态；增加不启动Docker的失败出口回归，不能靠errexit或FINISHED字样认定通过。新native build/epoch须独立归档修改后的全部输入，后续具体run另经Root检查。

## 完整审查 I1：有限响应后的原 transport 终态

三次源码小提交后，唯一完整审查确认一个结束顺序缺口：ReadEnrolledPublication验证TLS close_notify便defer Close，而实际bridge exec的Wait/Success/normalclose可能稍后才到；后续requireJoined不能补回已经发生的客户端物理关闭。最终green-bridge-r1的真实Wait0/relaynil仍是有效历史证据，但不是排除该竞态的证明。

本OPEN Task同pair R1允许最小修改runtime_publication_destination.go及专用回归：成功有限publication必须在原boundedctx、readDeadline及物理取消所有权内，先验证现有严格TLS EOF，再确认原底层有限字节通道n0/io.EOF终态，然后才成功返回并Close。对生产WS，该EOF由已有client的实际正常exec status/1000校验产生；错误status、额外字节、超时和取消均不得变成功。可用Session已有私有original raw conn完成读取，不能公开rawgetter/任意callback，也不能在验证TLS close_notify之前绕过strictTLSwrapper。不得sleep、提前Success、吞status/Wait错误或无限graceful等待。真实Destination/TLS受控gate延迟原transport EOF的新RED→GREEN及等待点取消/退出验证必需；生产WS终帧受控检查和新native只做此变动所需的限定验证，原所有历史矩阵不重放。
