# 明确 Task 的真实 cleanup 消费者：首片验证

## 本片行为与边界

新增 `DueWorker.ExecuteCleanupLocal`，通过原 actor 的有界 mailbox 消费明确 TaskReference，共享原 partition、session 和 Claim 预算。配置及节点切片由 Backend 拷贝，具体 worker、bridge、credential 和 provider 依赖按 Backend 生命周期借用。目标由不可变 snapshot v2 descriptor 与认证 enrolled history 派生；不解析 opaque 业务 payload。

真实链已接通关闭、quiescence、dual drain、归档接收与本地绑定、释放原 Claim 后重新计费申请第二 Claim、本地同步完成、observer 终止、exact provider 清理及 owner release。物理调用、Received、续租与 Claim 清理实际返回后才能发布原 Run 的 joined 标记，再以私有 release proof 和确切 E/S 修订独立结算。取消与结果未知保留原身份。完整请求投影只驻留于准入窗口。

配置、目标读取、actor、物理编排、查询等待及 join/settlement 分文件。保留在 etcd 包内是为了共享私有事务、原 actor 所有权、归档 manifest 和 sealed proof；传输、Kubernetes 和 launcher 继续拥有各自的实际效果实现，没有导出任意 comparator、权限 DTO 或成功 callback。提交前检查 GoDoc、逻辑分段、gofmt 与上述包边界。

## 实际验证

| 验证 | 结果 |
| --- | --- |
| 最终 host 直接回归 | 44 个顶层、88 个子场景通过，52.764s |
| 最终同范围 race | 44 个顶层、88 个子场景通过，59.141s，无数据竞争 |
| 最终 Linux vet | `consumer-final-vet-01` Linux/arm64、CGO0，通过 |
| Linux fresh build | runner `consumer-build-06`、launcher `consumer-monitor-04` 通过 |
| Linux 实际组合 | `consumer-native-07/complete` 通过，5.42s |

44 组范围为新增消费者 7、原 actor/lifetime 19、managed Claim 2、配置与启动 2、永久记录/CAS 9、完整请求预检 5；没有重新执行无关旧矩阵。每次 Go 执行均检查 17 类输入、执行前后清单、归档每个成员、工具及当前 SHA；失败后先完成审计，再授权修改。两个最终二进制另核验 ELF AArch64、Go1.25.6、Linux/arm64、CGO0 和非 race 构建信息。

真实 Linux 场景通过公开入口，使用生产 Pod builder、真实 PID1 Supervisor、零 capability worker、TLS etcd、原 bridge 和 node observer。验证非空文件内容、目录/文件 mode、两次不同 Claim 的 `put/delete/put/delete`、原 due 的实际发现进度、重复调用的相同 Slot/Release、实际 Done、Backend.Close、worker Wait=0、Supervisor SIGKILL 与空 cgroup。实际 provider 为 1 PATCH、1 DELETE、14 次 bridge upgrade；永久 E 包含 joined 与 release，S 已归还。容器 attach/Wait 均为 0，无 OOM；精确 CID 消失与空所有权清单已独立核验。

该场景的 Stop 如实返回 `ErrOutcomeUnknown` 与 `context.Canceled`，测试只接受这一组合或 nil，并检查实际 Done 和 stopped/unknown 终态。保留 JSON WARN8、ERROR4 及额外 gRPC warning1：包含取消读事务、LeaseNotFound、测试服务配置与 SIGTERM 关闭日志，没有零告警声明。物理任务结算与随后 due actor 的取消未知结果分别记录。

最终 native 原始日志 8919B，SHA256：`76a273a87e553f7c5a5a89bac3884b18a7747758ec04766ca7abcb71ad84f7f2`。本轮工具、构建输入、运行 inspect、实际退出和清理证据保存在本计划的独立执行目录。

最终 host 日志 SHA256：`e0ddbd7830a9ffc297a47f70849a216624dc5ba73d84eec179b93b933bb52cd9`；race：`af6fd634d41e8643386a6f3beb66d9370d1e2b596befddd07ffa1f0cc105c03d`。两者无 FAIL/SKIP，分别保留 WARN3/WARN6，均为取消 RPC 或 LeaseNotFound。最终 host/race/vet 及两个构建的完整输入审计通过后才进入提交阶段。

独立三成员 Compose fixture `consumer-01` 已停止：实际 Wait 均为 0，无 OOM，精确 CID 删除、空网络后移除及空所有权清单均核验。各服务 WARN21/ERROR4，错误均为 SIGTERM 时的 mux/http/listener 关闭；原日志保留。Compose、运行配置、工具/输入归档和七轮测试使用的实际 endpoints/project/CID 已独立核验。本轮没有遗留 Go 或测试容器/网络。

## 失败与修复记录

- 初始 missing API 是编译 RED；合法 64hex StoreID 随后实际触发 codec RED，修正原 UUID 假设及 future wire 上界。
- 首轮 host 暴露新测试 E/S 同 birth 的非法样本及跨四个计费 turn 的过短等待；改为真实分步 CAS 和有依据的有限等待，保留原安全断言。
- native01 在启动前因 Root 未处理 Docker 的 `CAP_` 规范化中止，已清理并保留旧工具；只修正精确六项 capability 的记录检查。
- native02–04 证明生产 Pod 初始 retained 与旧 step0 冲突；沿用原 Apply 的受保护 no-op 和真实 readback 修正编排，intent/unknown 不获得重发资格。未修改真实 Pod 的初始 finalizer。
- native05 仅作诊断：Root 在旧 launcher 输入清单发现新测试 drift 后仍批量发起运行，未计最终验收。该轮也暴露新增 mtime 断言超出既有同步合同；只修正测试，内容和权限断言保留。既有 local sync **不恢复归档 mtime**。
- native06 的 Stop 无错断言超出原关闭合同；只修正测试并加强 Done/status 断言，产品继续返回真实未知结果。最终 native07 使用重新构建并完整核验的两个二进制。

## 尚未完成

本片只证明一个明确 Task 的真实成功链。cold joined 恢复、bound-recovery、observer-ack-recovery、persistent32 的 33 条独立真实 Task 身份、close-join 四类 native 场景、deployment loader 和完整 Task 唯一审查仍待完成。冷 Run 当前保留。host 的 32 个排队名额及旧 metadata CAS 测试不能替代上述消费者争抢验证。

默认生产入口仍未切换；不代表完整迁移、全 Pod32 容量、海量存量 sandbox 性能或生产部署验收。
