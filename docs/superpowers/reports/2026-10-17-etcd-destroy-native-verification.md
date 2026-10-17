# HTTP DELETE 与原自动物理清理组合

## 实际范围

新 `TestHTTPDestroyNative` 每场使用独立容器、真实 TLS etcd、原冷创建 service、真实
launcher/bridge/用户 Exec、原签名 observer 与 production cleanup-worker ELF。fixture
Kubernetes API 在本地提供原 UID/node/cgroup 的严格响应；这不是生产 Kubernetes 验收。

local 场景从真实 service.Create(local) 的原 capsule、publication 和 local v2 descriptor
开始；Exec 实际写入目录与文件。HTTP DELETE202 原子写 Task/intent/link/checkpoint/P
和 committed receipt，Stage guard 已释放，再次 DELETE409。独立非 root、零 capability
的正式 worker 自动发现原 Task，完成关闭／排空、实际归档及 mode 检查、终止、provider
删除、签名 owner release、E settled/Run joined、永久槽释放与正常后续 P 删除。
worker 收 SIGTERM 后实际 Wait=0、session lease 已撤销；原 runtime Wait=SIGKILL，18 条
真实 bridge 的 Cmd.Wait 和 IO 都完成 join。

原 owner-release 只删 owner/control/runtime index，保留 placement/fence。新测试核对
三个确切键消失与保留记录的完整 bytes/MVCC 不变，原 ReadSandboxView 此时严格返回
nil+ErrCorruptRecord。公开删除后终态查询尚未接线，没有404承诺。cold quota 仍实际
occupied；没有 test-only refund，不把本片称为自动配额闭环。

detached 场景使用真实 HTTP CreateBusiness201，然后 DELETE501
unsupported_cleanup_profile；完整有界 namespace 的98条原记录字节及 native字段不变，
之后 GET 与真实 Exec(exit7/stdout)仍成功，原 runtime 与六条 bridge 实际 join。

## 运行记录与边界

| 独立场景 | 结果 | 原始字节／SHA-256 |
| --- | --- | --- |
| destroy-native-detached-02 | PASS 1.62s | 5039 / `45a6296f223f1eb6806063f894128885d2b6d24673934a0e92f78c57c6592c98` |
| destroy-native-local-06 | PASS 32.87s | 9220 / `8b1aae0afe9b934232a94d89e0b7fb9142f4e6977306e88f375440181aab67ca` |

detached-02 使用最初 build-01；其后 helper 的 local epoch 恢复、身份窗口修复及 local
诊断不属于该次二进制，不能把旧阴性冒充最终字节。profile拒绝产品路径未因这些修改
改变；最终 local 使用 build-05；最终 build-05/vet-04 均9342个实际输入。每轮 Go 后都由 Root
独立核对17类输入、source/tool全归档、前后摘要与当时字节，最终两轮再次完整审计通过。

保留的失败及针对性修复：

- local-01/detached-01 的最小 etcd rootfs 没有 `/bin/sh`，故真实 Exec 失败；仅改 fixture
  文件系统镜像，不修改产品或断言。镜像仅作文件系统使用，不启动其 Kubernetes。
- local-02 新 setup JSON 未恢复 Identity 的非序列化 RestoreEpoch；按原绑定恢复，
  不放松身份检查。local-03 真实进入14条桥后卡归档，定位为业务TTL截断receipt identity；
  独立产品修复及 RED/host/race/vet 记录见 `2026-10-17-etcd-cold-identity-window-verification.md`。
- local-04 完成物理清理后，新父测试错误要求 placement 也消失；改为原三键删除与保留
  placement/fence 的精确断言。未扩大已满60 compare+4 write的 owner-release。
- local-05 所有业务/物理断言已通过，原 fixture 最后 WebSocket Close 帧出现 EPIPE。
  原 relay 的 IO/Wait/ctx 错误与 Success 状态错误都有独立包装，裸EPIPE只可能来自
  最后的 Close 写入。仅增加实际完成阶段诊断：local 专用检查仍要求完整 IO、空stderr、
  Cmd.Wait=0及Success写成功；非nil结果只接受同一原 finalCloseErr 且 errors.Is(EPIPE)。
  原 requireJoined 不变，错误原样保留。local-06 实际观察到两次该尾帧EPIPE并通过严格
  分类；不忽略任何更早IO/状态/上下文/进程错误，也不把原日志声称为 pristine。

Root 独立检查成功场 actual command、四个只读 ELF 摘要、前后 inspect、真实 exit0、
no OOM、无 SKIP 及精确容器清除。rootfs固定为 `sha256:26ad49adcb6923b3faa00ee6654943de9bdda7cfcb1c42a9b9cef334a48dfb33`；
资源1GiB/2CPU/256PID、network none、private cgroup、drop ALL后仅七项setup capability、
no-new-privileges与默认seccomp；tmp768MiB、workspace1MiB。正式worker实际UID1000且
五组capability全零。fixture setup不代表生产部署权限已验收。

人工检查注释、逻辑阶段、包职责和所有 goroutine／OS进程生命周期；新代码仅为原
controlrunner native组合，不增加物理权限接口。本片不是detached消费者、FUSE、
大N容量、组合故障全覆盖或整体生产迁移。

唯一完整入口 Task 审查：Spec Compliance / Code Quality 均通过，无新增 C/I/M。
审查明确接受本片支持的 local v2 接线与两场有限 native 证据，不推导入口丢 ACK、
Resolve/Release 故障交错全部动态通过；上述配额、终态查询及整体迁移缺口保持开放。
