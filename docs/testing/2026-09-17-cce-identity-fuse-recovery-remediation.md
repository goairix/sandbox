# CCE 身份绑定与未启动 FUSE 清理整改

日期：2026-09-17。依据 [已确认设计](../superpowers/specs/2026-09-17-cce-identity-fuse-recovery-design.md) 与 [实施计划](../superpowers/plans/2026-09-17-cce-identity-fuse-recovery-implementation.md)。

## 结论与边界

本轮落实两个本地代码修复，不改变 Kubernetes 1.29 最低支持，不新增 values/config、Schema、RBAC、Helm 资源、协议、池指纹或默认 DNS 配置；Chart 仍为 0.3.0。

- 身份：正常 PVC 绑定只改变 resourceVersion 时有界重新校验，不再误判为安装身份无效。
- 清理：已调度但未启动容器的 exact FUSE Pod 可以进入正常 UID 删除，再等待 kubelet 终止证明；不执行无法运行的 shutdown，也不绕过证明。
- AppArmor：修正“无需宿主机 parser”的无条件说明，区分镜像 parser、内核和 CRI 能力。

本轮没有集群写入、云卷创建、节点配置修改或镜像构建/推送。没有切换/检查分支、创建 worktree、回滚或委派。本地验证不等于 CCE 全量验收；现场历史结果仍见 [v0.3.33 报告](2026-09-17-cce-v033-live-validation.md)。

## 实现与安全边界

### PVC 稳定性

`stableFreshPVCs` 在既有身份创建前检查中复用原 context/预算，固定原 CM UID、resourceVersion、clusterID、成员、Pending 和无 registration 条件。每轮严格检查 PVC 的固定名称/namespace、UID/RV、删除状态和本次安装注解，跨重试保留所有已观察 UID；新 PVC 首次出现时也钉住 UID。

仅 RV 改变会按既有 250ms 间隔重读。消失、replacement、删除、许可漂移或 CM 变化立即拒绝。持续变化使用原总预算退出；重试前不生成密钥。最后的 Secret GET、最多一次 Create、不确定写入后的服务端解析、保留原 Secret 和私有 seed 清零保持原有契约。

稳定路径的 fake 请求计数断言为 15 次 GET + 1 次 Secret Create，与修复前一致；每个 RV 漂移轮次仅在这个异常路径增加 1 次 CM GET 和 6 次 PVC GET。

### FUSE 终止证据

新增纯判定完整覆盖 init/native sidecar、普通和 ephemeral 容器；每个声明必须唯一、有对应状态。只允许单一 Terminated 或严格 never-started Waiting；Waiting 有 ContainerID、重启、Started=true 或历史状态，或出现 Running、缺失/未知/重复/混合状态时不选择新路径，继续既有 shutdown/fencer 流程。合法 Terminated 不要求从未运行。

判定只授权请求正常删除，不是 ProcessExited 证明。DELETE 保留原 grace period、UID precondition 和受管 finalizer；真实 REST 明确关闭 Retry-After 隐式重试，不在同次调用重放不确定 DELETE。随后在原 termination timeout 内观察同 UID、namespace、节点、prepare attempt、bootstrap 身份、finalizer 和删除状态；全部容器必须取得合法终止状态。Waiting 只能在 deleting 且 Failed/Succeeded、严格从未启动时计入。

NotFound、replacement、finalizer/身份漂移、超时或不完整状态不能推导退出，仍需要原 exact UID/节点基础设施 fencing 后备，否则返回终止未确认。新成功证明包含原 UID/NodeName 和 ProcessExited，不伪造 GracefulUnmount；终态恢复路径也保留 NodeName。原 Confirm → Redis evidence CAS → Finalize 顺序未改，终止前不移除策略/finalizer 或释放 owner。现有未绑定节点的 Prepare 补偿规则不改。

## 测试记录

先写回归并在旧代码观察行为失败：

- `TestIdentityPVCBindingVersionAdvance`：同 UID 的 RV 1→2 导致 `ErrIdentityInvalid`，没有创建身份。
- `TestFUSEUnstartedPodNormalDeletion`：CreateContainerError/PodInitializing、mounter 无法执行 shutdown 时返回 `ErrTerminationUnconfirmed`，没有请求正常删除。

修复后新增正向和负向用例覆盖：一次/多次 RV 变化、稳定请求计数、共享 deadline/cancel、跨重试已有/新出现 PVC 的换 UID/消失、删除/注解/名称/namespace/UID/RV 异常、CM UID/RV/cluster/成员/phase/registration/删除变化、并发 Secret 和 committed/uncommitted/AlreadyExists。

FUSE 覆盖正常删除、等待中启动后再终止、删除响应丢失、已有 deleting、失去 Pod/finalizer/身份、同名 replacement、三类容器状态完整性、混合状态与历史痕迹、超时/取消、精确 fencing、无内存证明重启恢复及原 Finalize。等待中验证策略与 finalizer 保留。原运行中 shutdown、policy UID rebound、Redis CAS/池和 Docker 回归不放宽。

真实 HTTP transport 验证：PVC 漂移后不确定 Secret POST 仍恰好一次，admission Warning 不进入日志处理器，原 Secret 不变；FUSE DELETE 的 committed/uncommitted 500 + Retry-After 均恰好一次，私有警告不泄漏。不把这些 HTTP fixture 称为真实 kubelet、CSI、CRI、RBAC 或节点 HA 验收。

纯运行中判定 benchmark 在 darwin/arm64、Apple M5 分两组各五轮运行：第一组 9.786–14.05 ns/op，最终复跑 9.215–9.560 ns/op；均为 0 B/op、0 allocs/op。新增运行中判定不发请求、不 exec、不等待、不增加 goroutine/全局锁。该数字不是 CCE 创建/销毁性能，不解决既有冷准备或存储挂载长尾。

最终验证均以实际退出码 0 为准：

| 验证 | 结果与覆盖边界 |
| --- | --- |
| `go test ./... -count=1 -timeout=3m` | 通过；包含 Go 模块现有测试、Docker、FUSE pool/manager 与 Redis 状态/CAS 回归 |
| `go test -tags helmtests ./internal/helmtest -count=1 -timeout=3m` | 通过；含 1.29/1.31/1.33 渲染、DNS 默认基线、Sentinel 版本及 loader 默认值 |
| `go test -race ./internal/redisbootstrap ./cmd/redis-bootstrap ./internal/runtime/kubernetes ./internal/sandbox -count=1 -timeout=3m` | 通过；两轮运行，无竞态报告 |
| `go build ./...`、`go vet ./...` | 通过 |
| `GOOS=linux GOARCH=arm64 go build ./...` | 通过；交叉编译，不是 linux/arm64 真实运行验收 |
| 主机及 linux/arm64 `golangci-lint run --timeout=3m --max-issues-per-linter=0 --max-same-issues=0` | 均为 0 issues；未放宽 lint 配置 |
| `bash scripts/test-helm-apparmor-loader.sh` | PASS |
| `git diff --check` | 通过 |
| 身份相关新回归/不确定 Create/HTTP：`-count=3` | 三次重复通过 |
| FUSE 新回归及现有 RemovePreparedSandbox：`-count=3` | 三次重复通过 |

## 内联自审

逐项对照设计与当前 diff 自审，未发现需要遗留的本轮阻断项。审查中补上新删除路径的 REST `MaxRetries(0)`，并用 committed/uncommitted 的真实 HTTP 用例验证只写一次；还复核了 namespace、prepare attempt、bootstrap 和节点漂移，不把 finalizer 丢失或 Pod 消失当作退出。

复核所有重试仍在创建前、跨重试不重置 CM/PVC UID、没有隐式 Secret 更新或不确定 Create 重放；未改 Redis CAS/owner 释放顺序，未新增强制删除/零宽限期/手工移除 finalizer；正常运行中路径没有新增请求。Helm、配置默认值、网络策略生成和 Docker 代码未改，原负向断言保留。公开报告仅含安全的结果摘要，私有 values/kubeconfig、环境、Secret、原始日志及临时诊断文件不入提交。

## 构建与后续验收

由部署者从包含修复的代码重建 **sandbox-api 和 sandbox-redis-bootstrap**；后者的身份依赖改变，不能仅重建 API。沿用已有兼容的 loader/mounter/runtime/gateway/Redis 服务镜像。无新增配置，只更新实际镜像 tag；完整命令见 [本次部署说明](../deployment/helm-deployment-upgrade.md#本次-cce-身份绑定与未启动-fuse-清理整改)。更新 bootstrap 会改变 Sentinel helper 的 PodTemplate，须维护窗口观察 quorum，不删除旧安装身份 Secret/PVC。

CCE 的 CRI 拒绝 AppArmor 尚未解决。loader Ready 不代替 CRI 能力与真实 enforce 验收；平台只读核查、能力缓存和维护窗口边界见 [节点前提](../deployment/apparmor-loader.md#节点前提loader-ready-不等于-cri-支持)。不能关闭 LSM 或盲目重启 CRI 来宣称通过。

新镜像和节点前提就绪后，再单独执行 CCE 隔离安装/升级、Sentinel 身份/初始化、普通/FUSE API、真实 enforce、正常 drain/uninstall 和精确资源清理；网络隔离、最低 1.29 生命周期、DataPlane V2、OBS 与节点 HA 保持未验收项，不因本地回归通过而勾选。
