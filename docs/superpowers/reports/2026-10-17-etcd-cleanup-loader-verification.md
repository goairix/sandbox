# 本地 cleanup mounted loader 验证

本片把已有明确 Task 的本地 ordinary/enrolled cleanup 消费者接到真实挂载配置，
提供 OpenLocalCleanup、Execute、Close 和四用途 CleanupIssuer。入口仍需调用者
持有真实 worker KernelBoundary；没有创建主程序角色、HTTP 入口、metadata 或
inventory，没有完成自动 pending 发现或 Redis 默认切换。

严格 v1 manifest 限制 64KiB，全部字段必填、精确小写 ASCII，拒绝未知、重复、
null、字段别名、尾部 JSON、数值时间和越界预算。静态校验先于凭据 IO。原
session60s、256 分区、4 Relist、20 Range、100 Claim share 以及最多32本地任务
约束保留；持久全 cell32槽仍由此前协议执行，不由配置上限代替。文件相对 manifest
目录解析，数据/恢复目录显式绝对。节点配置有界，不随历史 sandbox 常驻增长。

CleanupIssuer 复制密钥与证书，验证公开根、binding、可信时钟、选定 issuer、
固定用途和证书窗口；仅调用原四个 protocol signer。归档完整 session 必须落在
证书窗口内。实际签名持读锁，Close 独占后清零私有副本，不清调用方 backing。
它签署归属信息，不提供 TaskClaim、runtime 或 filesystem 执行权限。

部署对象拥有 worker、Backend、provider/bridge 和凭据，Kernel 仅借用。关闭先
封住 Execute，实际 Stop/Backend.Close 和已接收调用全部 join，再关闭依赖、清
密钥；并发 Close 等待同一结果。构造失败按同一图清理并保留错误。Open 的 context
控制 worker 生命周期，取消后仍必须 Close，不能把取消当作资源释放完成。

源码按 manifest/issuer/composition 分为三个文件。包依赖保持 deployment→etcd/
transport/launcher；共享事务和 actor 权限仍私有，没有为拆包导出内部权限。
人工检查了导出注释、复杂生命周期约束、逻辑分段、依赖边界；gofmt/diff-check通过。

固定 Go1.25.6。六组 host/race selector：
`^(TestLocalCleanupManifestStrict|TestLocalCleanupManifestBounds|TestOpenLocalCleanupRefusal|TestMountedCleanupIssuerSigns|TestMountedCleanupIssuerRefusal|TestMountedCleanupIssuerClose)$`。
host/race 为 darwin/arm64 CGO1，均未配置测试 etcd；vet 为 linux/arm64 CGO0。
每次后续执行或源码修改前独立核对全部17类输入、前后 SHA、源码、工具和每个
归档。后续仅注释变更及已单独提交的时钟修复没有重复重放未受影响的六组测试。

| 运行 | 结果 | 输入数 | 原始日志字节与 SHA256 |
|---|---|---:|---|
| loader-red-01 | 缺新接口的编译失败；不冒称行为 RED | 9018 | 1193 / 55bddc30ad8151049742b05838a8335d9be46ebe63bd10739333a3d997a5b086 |
| loader-host-01 | 6组64子例通过，包1.322s | 9021 | 8609 / ef8c598c86dafc5a9168171ed12d9479cf0bd2060dacf6a97014b591bd35947c |
| loader-race-01 | 6组64子例通过，包2.403s，无数据竞争 | 9026 | 8609 / 74239e2fc46dc16e2b861b1a865f227ea93441b5ab9919603369bd6ba355acfe |
| loader-vet-02 | deployment Linux vet通过 | 9109 | 0 / e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 |
| loader-native-vet-01 | controlrunner Linux vet通过 | 9283 | 同上空日志 SHA |

Root 的 vet-01 selector 参数曾在 harness 前置断言被拒绝，未执行 Go、未产生运行
归档；随后 fresh vet-02 使用合法 anchored selector。没有修改工具来绕过断言。
host Open 只证明无效调用拒绝；正向挂载证明来自下述真实 Linux 场景。

新增唯一 selector `^TestCleanupExecutorNative$/^mounted-loader$`，child 仍为
原 `^TestCleanupExecutorWorkerChild$`。固定 Linux/arm64 image、network none、
私有 PID/cgroup、只读 root、原六 cap、512MiB/2CPU/256PID、原三只读挂载和七项
环境不变。仅新场景在现有 parent private mount namespace 建立1MiB /run tmpfs，
nosuid/nodev、uid/gid1000、0700，最后实际 unmount。父120s、worker100s/进程110s、
top150s、attach180s。无环境扩权或伪 Kernel。

UID/GID1000、零 cap 的真实 worker 写0600配置、issuer PKCS8、TLS文件、静态
kubeconfig，并创建原固定 UDS signed-clock 服务。API 使用 HTTPS/静态 CA，
clock key 独立于 root signing key；root 私钥仅原 parent 内存签证书，没有挂载给
loader。实际 Open 经原 SignedClockClient 签名验证，没有用 callback 替换。

部分构造失败使用真实另一 session 占住 Range0；Created-confirmed 精确 Watch
观察目标 Relist0 的 put29/delete30、同目标 session 历史 fence 与删除，检查 blocker
share 字节/revision未变。实际释放 blocker 后，同配置、workerID和份额再次 Open
成功，验证失败构造已释放自身占用；没有只从 ErrConflict 推断执行阶段。

首次 loader-native-01 在 local-complete 后真实失败于终止票据认证窗口，5.34s；
原始5393字节，SHA `f2c2d822c3be51aa140e0a1af89d08e8c002a3e8b009685a119979e7667c78d3`。
实际退出1/worker Wait1，时钟已join、失败 supervisor及etcd实际Wait，确切容器清理
完成；独立环境/输入/工具/退出/清理审计通过，但 native_pass=false。根因和单独
修复提交 `7f0d95f` 见同日 cleanup-clock-verification 报告；未放宽原认证校验。

修复后两程序重新构建并逐次 full17 审计。runner build02：9284输入，ELF64 AArch64
SHA `6b813fe1487c1d90d63770d7c8d32ae46e08e6d72d7447d4d7483ba64d96e9ff`；
monitor02：2044输入，SHA `f9ee85c397b65a725ceb83e0efece85c99522c57b136d40c9f9822d49617fa02`。
均确认 Go1.25.6/linux/arm64/CGO0/non-race 和实际二进制 hash，再运行组合场景。

**loader-native-02 通过5.81s**：部分构造冲突/同份额重开、真实 Open→Execute
到 settled/committed/non-retained、两不同原 TaskClaim 的 put/delete/put/delete
且无重叠、非空内容和目录0750/文件0640、同 Task 重复结果一致、Close 后 Execute
拒绝、borrowed Kernel 两检查仍有效、实际 signed-clock Serve join=nil。parent 的
worker OS Wait=nil，exact provider1PATCH/1DELETE、bridge14次、永久E settled且
RunJoined、无occupied S均通过。没有增加mtime合同或构造替代物理完成证明。

执行已物理结算；随后 Close 实际返回原 Unknown+contextCanceled，不掩盖为nil。
重复 Close 返回同一结果；实际 worker/Backend及其依赖已join，签名时钟也join。
日志含8条JSON WARN、4条ERROR和1条gRPC WARNING，涉及取消只读事务和服务
关闭，保留原始记录；未宣称无警告。首次失败也保留7WARN/3ERROR。

最终原始日志8478字节，SHA
`04f1f7f7882b9e371fe13a275862626e716671cfe56eb4851f42ffe20d3dd0f7`。
实际 attach/Wait均0，无OOM/timeout，确切CID已删除且标签清单为空。独立literal
runtime、输入、工具归档、退出、清理审计 native_pass=true；两程序源码/工具/
前后hash/每个归档/current17再核对通过，之后才进行提交或后续源码修改。

本片验收真实 mounted local/enrolled loader，完整 cleanup Task仍未关闭：早期
joined close/quiescence/dual-drain、cleanup-complete owner-held恢复及唯一完整Task
审查仍需完成。后续还需pending发现/共享actor生产worker、各业务入口、FUSE/
collector/remote sync、部署恢复、大N容量和Redis移除。此处不是32个Pod并发、
大量已有沙盒性能或生产迁移验收。
