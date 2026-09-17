# CCE 身份创建与未启动 FUSE 清理整改设计

## 状态、证据与范围

用户在 v0.3.33 现场报告后要求继续处理。本设计已获确认，实施与本地回归完成。
结果见 [整改记录](../../testing/2026-09-17-cce-identity-fuse-recovery-remediation.md)；现场验收仍需新镜像与节点 CRI 前提。
证据见 [现场报告](../../testing/2026-09-17-cce-v033-live-validation.md)。

本轮聚焦两个可复现代码缺口，并修正 AppArmor 部署前提说明：

1. 当前 `createFreshIdentity` 比较两次完整 PVC 快照，只有 resourceVersion 改变也
   返回身份无效。fake client 已复现，稳定 RV 成功创建一次、变化 RV 无创建且失败。
   现场首次身份 Job 失败、原样诊断重试成功，但缺少请求追踪，不能断言现场就是该分支。
2. exact FUSE Pod 的 mounter 为 CreateContainerError、全部容器从未启动时，清理仍
   尝试 mounter shutdown；既无响应，也未发起正常删除，因此不能进入 kubelet 终态。
   现场正常 UID 删除后取得 Failed/完整状态证明，现有 hooks 随后成功卸载。
3. CCE 的 CRI 明确拒绝 AppArmor。loader Ready 不能代替 CRI 能力证明；代码修复
   不能解决节点运行时不支持。2026-09-17 后续只读 configz 检查两节点，均未见显式
   AppArmor/AppArmorFields feature gate；configz 不包含 containerd 配置，不能据此
   认定 CRI 的禁用配置或 host 检查结果。

最低 Kubernetes 仍为 1.29。保留已验证环境的 DNS、安全、网络、领用、运行中 shutdown、
普通沙盒及 Docker 行为。不创建 worktree、不检测分支/main、不委派子代理、不回滚、
不构建/推送镜像、不重新创建自动监控。当前阶段不做集群写入或新建云卷。

## 方案比较

采用“有界重新校验 + 保留对象的正常删除/终态证明”，分别在原身份创建和 FUSE
两阶段清理内部补小范围逻辑，不引入新控制器、HTTP 转发、配置或状态协议。

- **推荐方案：**仅正常版本变化可重读，身份/授权仍钉住；仅无运行中容器的完整状态
  可选正常删除路径，最终仍要求 kubelet 精确证明。额外请求只发生在异常/竞态路径。
- **直接忽略 RV、Pending/NotFound 直接视作销毁：拒绝。**省改动，但跳过稳定性检查
  或失去进程终止证明，不能保证 replacement、删除竞态及节点分区安全。
- **仅加 Job 重试、人工删除或无限等待：不采用。**首次 Job 重试无法修复内部身份
  判断，也不能闭合未启动 mounter 的清理流程；无限等待影响可用性。基础设施 fencer
  仍作为原有异常后备，不成为正常启动/卸载的依赖。

## 一、身份创建：有界稳定性重新校验

### 不变的授权和写入契约

保持 Helm 渲染时的新安装授权、固定 namespace/CM/Secret/三成员、两分钟总预算、
250ms 可取消等待，以及客户端禁止隐式重试敏感 POST 的规则。
已有身份 Secret 只读验证；旧 PVC/状态缺身份仍要求恢复原 Secret，不生成替代密钥。
CM UID、resourceVersion、clusterID、成员、Pending 和无 registration 的约束不放宽。

### 新的 PVC 校验流程

在 `createFreshIdentity` 内建立仅本次调用持有的三成员观察记录：

1. 读取并校验三个固定 PVC；已观察到存在的对象钉住 UID，新出现的匹配本次 clusterID
   的 PVC 按现有 Parallel StatefulSet 规则允许出现，并在首次观察时钉住 UID。
2. 按原规则复核 CM，再读取第二轮 PVC。每次读取仍检查名称/namespace、UID/RV
   非空、无删除时间戳及本次 clusterID 注解；不从实际对象学习新的身份/授权。
3. 已观察对象消失、UID replacement、删除或注解漂移立即拒绝。仅同 UID 的非空 RV
   改变且所有身份条件仍合法时，等待原 250ms 后重新做完整 pre-create 校验。
4. 跨重试保留所有已观察 UID 和原 CM/安装身份，不能通过重置快照接受 replacement。
   必须观察到可接受的两轮快照后才进入最后的 Secret GET 和密钥生成/Create。
5. 全部重试共用原 context/总预算；持续更新返回 deadline/cancellation，不无限循环。

稳定路径不增加原有 Kubernetes 请求次数。重试期间不生成密钥，不扩展 RBAC。
成功或不确定的 Create 后不重新生成/重放 Create：保留原服务端 GET 解析、
AlreadyExists 并发验证、创建后 PVC UID 复核和 private seed 清零规则。

## 二、FUSE 清理：请求删除与终止证明分开

保留原 `ConfirmPreparedSandboxTermination` / Redis evidence CAS /
`FinalizePreparedSandboxRemoval` 两阶段协议、cleanup lease/token/revision 与 finalizer。
不新增 evidence 字段，不改变 cleanup protocol、池指纹、Helm hooks 或租户 API。

### 选择新增删除路径

在 exact FUSE 身份验证及受管 finalizer 确认后，对三类声明容器做完整状态检查：
init（包含 native sidecar）、普通、ephemeral。数量/名称必须一一覆盖，无重复、无额外
状态；每个容器只允许单一 Terminated，或严格 never-started Waiting：
ContainerID 为空、RestartCount=0、Started 不为 true、LastTerminationState 为空，
当前 state 不能混入 Running/其它状态。

这一纯判定**只表示可请求正常删除，不是 ProcessExited 证明**。在 mounter 或其它
容器 Running、状态缺失/未知、有历史痕迹的 Waiting 等情况下，不选择新路径，继续
原 graceful shutdown/fencer 行为。原全部 Terminated 路径仍保留。

### 正常删除与证明

1. 尚未 deleting 时按 exact UID 发起普通 DELETE，保留 Pod 原 grace period 和受管
   finalizer；已有删除时间戳时不重复发起删除。不执行 shutdown，也不向未启动容器
   发送 exec。状态读取与删除间可能发生启动，因此仍必须等待实际终止。
2. 使用原 termination timeout（默认 60s）及轮询间隔等待同一 UID。请求结果不确定时
   只读复核该 UID、finalizer 和删除状态；不在同次调用重放不确定写入。
3. **新路径只有拿到 exact Pod 的 kubelet 终止状态才成功。**Waiting 仅在 deleting
   且 Failed/Succeeded、严格 never-started 条件成立时认可；Terminated 必须状态合法，
   所有声明容器完整覆盖。复核 exact FUSE 身份，不接受异常混合 state。
4. NotFound、replacement、finalizer 保护丢失、身份漂移、等待超时或不完整状态不能
   从本次 DELETE 推导退出；保留原绑定 UID/NodeName 的 infrastructure fencer 后备。
   没有可信证明时保留 finalizer、策略、Redis cleanup/owner 状态并返回未确认。
5. 成功只产生绑定原 UID/NodeName 的 ProcessExited 证据，不伪造 GracefulUnmount。
   随后沿原 CAS 持久化和 finalize 顺序收尾；崩溃/其它副本接管可从 finalizer 保留的
   exact Pod 状态恢复，不能依赖旧进程内存。

新逻辑覆盖 Prepare 失败补偿、reconciliation、drain、孤儿收尾的共同 termination 入口，
不另设跳过 proof 的卸载快捷方式，不调整已有未绑定节点补偿规则。
终止前不得删除 NetworkPolicy 或释放 workspace owner。

## 三、AppArmor 前提：修正文档，不擅自修节点

[Kubernetes 官方前提](https://kubernetes.io/docs/tutorials/security/apparmor/)
将内核启用、容器运行时支持、profile 已加载分开。
上游 [containerd 1.7.29 CRI 能力检查](https://github.com/containerd/containerd/blob/v1.7.29/pkg/cri/server/helpers_linux.go)
包括 DisableApparmor 配置与 host 检测；
[host 检测](https://github.com/containerd/containerd/blob/v1.7.29/pkg/apparmor/apparmor_linux.go)
还涉及宿主机 `/sbin/apparmor_parser`、securityfs/启用状态和一次性缓存。
这些上游条件不等于已经确认 CCE 厂商补丁二进制的失败条件。

同步 `docs/deployment/apparmor-loader.md` 和 `helm-deployment-upgrade.md`：Helm 自动
创建 profile/loader/RBAC，不要求用户手工创建本项目资源；但 loader 镜像内的 parser
不提供宿主机 parser，也不改变 CRI 配置。删除“无需节点 parser”的无条件表述，补充
平台管理员只读核查清单及 loader Ready 与真实 mounter enforce 的验收区别。

本设计不批准安装宿主机软件、挂载 runtime socket/主机根目录、修改 containerd、重启
节点服务、注入特权 system Pod、关闭 LSM 或自动切换 SELinux。不用 helm values 掩盖
CRI 拒绝。平台条件未解决时继续明确报告完整 CCE FUSE 尚未验收。

## 测试、性能和交付

先写行为 RED，再最小实现 GREEN；保留旧安全负向断言，不放宽 lint：

- 身份：一次/多次 RV 变化后稳定成功且恰好一个 Create；稳定路径请求数不增加；
  持续变化超时、取消无写入；重试间 disappearance/replacement、删除/注解漂移、
  CM UID/RV/phase/registration 变化拒绝；新 PVC 正常出现；已有 Secret 并发验证；
  Create 不确定结果仍不重放。fake client 加真实 HTTP transport 测试，不输出密钥、
  admission Warning、API 私有 body，覆盖 retry 预算和动作边界。
- 清理：模拟 CreateContainerError→UID DELETE→Deleting/Pending→kubelet Failed，
  先 Confirm 得到 proof、再走原 finalize；没有 terminal 前不删除策略/finalizer。
  覆盖等待窗口启动后再终止、进程重启恢复、响应丢失但对象仍保留、NotFound/
  replacement 不成功、terminal 前失去 finalizer、三类状态缺失/重复/未知/混合、
  Running 和 Waiting 历史痕迹拒绝、超时/取消、policy UID rebound 保留。
- 回归：相关包、FUSE Pool/manager/Redis CAS、Docker、完整 Helm，本机全仓
  test/build/vet/lint、linux/arm64 build/lint，相关 race；最低 1.29 渲染与此前 DNS
  默认基线保留，不把交叉编译称为真实 arm64 测试。
- 性能：稳定身份不增加请求，运行中 FUSE 的新增判定不引入请求、exec、等待、后台
  goroutine/全局锁；新纯判定的运行中快路径无分配，以重复 benchmark/race 记录，
  不把本地假客户端延迟当作真实创建/销毁性能。
- 本轮无新增 values/config、RBAC、Schema、协议、tag 或 Chart 版本配置；文档列明
  实现后需由用户重建 `sandbox-api` 和 `sandbox-redis-bootstrap`，loader/mounter
  二进制不变。不要求覆盖线上完整 values 或手工创建项目 Kubernetes 资源。
- 自审、报告和代码提交后，等待用户新镜像与平台条件；完整现场验收另行执行，不把
  本地测试代替 CCE、原 ds-ai-research、1.29 生命周期、网络隔离、OBS 或节点 HA。
