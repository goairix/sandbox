# CCE v0.3.34 / Redis bootstrap v0.3.1 现场验证

时间：2026-09-17 22:52–22:55（北京时间）；独立清理复核于 23:05 完成。

## 结论

本轮验证了 `00e2279` 整改后的两个新镜像：**首次身份初始化与 Sentinel 初始化通过；CRI 拒绝启动的 FUSE Pod 可随正常 Helm 卸载清理，不再需要手工介入。**

**完整 API/FUSE 验收仍未通过。** CCE 的 CRI 仍返回 `apparmor is not supported`，三个 API Pod 在现场诊断时均未 Ready。未将 loader Ready、普通池 Pod Ready 或卸载成功计作业务 API、真实 FUSE 挂载及 AppArmor enforce 验收。

没有修改节点配置、关闭 LSM、强制删除、手工移除 finalizer、跳过 Helm hooks、回滚或补跑身份 Job。没有构建/推送镜像；没有修改 `ds-ai-research` 集群。无需新增 values/config 项。

## 测试环境与范围

- 双华云 CCE、未启用 DataPlane V2；两节点 Ready，实际节点架构 amd64。
- Kubernetes 1.31 测试环境，不替代最低支持 Kubernetes 1.29 的生命周期验收。
- 容器网段 `10.0.0.0/16`，Service 网段 `10.247.0.0/16`；使用标准 NetworkPolicy 后端。
- 继续使用原测试 MinIO TLS 后端，不属于 OBS 验收。
- 随机隔离命名空间 `sandbox-cce-sentinel-test-a8d10a7794f7`，UID `cb3d39a1-d9e5-44ce-a0aa-539ef24b05d2`。
- 三副本 API、两个 AppArmor loader、三个 Sentinel 成员；仅测试后处理器将 Sentinel 的 hostname 硬反亲和改成软反亲和，不修改 Chart 生产默认值。
- 沿用 DNS 注入 opt-out 与 `single-request-reopen` / `timeout=2` 测试配置；本轮未完成业务 DNS 路径验收。
- 最多三块 `csi-disk`、1Gi 测试 PVC；productionSafetyChecks 保持启用，allowMissingLSMForKind 保持 false。

## 镜像核验

两个用户提供的新 tag 均可读取 amd64/arm64 manifest；实际 Pod 的 imageID 与 registry 的 manifest list 摘要一致：

| 镜像 | tag | manifest list 摘要 / 现场 imageID 摘要 |
| --- | --- | --- |
| `registry.i.huaxisy.com/library/ai-infra/sandbox-api` | `v0.3.34` | `sha256:48c673e5b01ce72d187938012ddf6bbd63364614f91697273de6ed1a31cfd352` |
| `registry.i.huaxisy.com/library/ai-infra/sandbox-redis-bootstrap` | `v0.3.1` | `sha256:f3ee05dfa6bd9f3e3b2c827ce6f4c8d371ce6a4d297aed2b4ecb5e88c62c0636` |

加载器沿用 `sandbox-apparmor-loader:v0.3.24`，Redis 服务镜像沿用 `redis:7.4.2`。没有改仓库或私有 values 的默认 tag。

## 首次初始化：通过

22:52:32 Helm 首次安装提交后：

- 首个身份 Pod `sandbox-fuse-redis-sentinel-identity-1-f4pqw` 于 22:52:36 观测到 Succeeded，UID `b1580baf-8a8c-466c-bb2d-38071d4f7b73`；零重启。没有创建第二个身份 Job、手动重试或恢复原模板补跑。
- 初始化 Pod `sandbox-fuse-redis-sentinel-initialize-1-8pp79` Succeeded；22:54:17 三成员均 Ready，prepare、identity、redis、sentinel 的重启计数全部为零。
- 原状态 ConfigMap 的 `cluster.json` 为 `Initialized`，存在非空 `registration.json`；三块 PVC Bound。

这是本轮首次安装/初始化通过，不把上一轮 v0.3.33 的人工补跑改写成通过，也不推断本次一定发生了 PVC RV 漂移。两节点的软反亲和测试不证明三节点故障隔离、Sentinel 冷恢复或节点 HA。

## AppArmor / API：环境阻塞

两节点 loader 均 Ready、零重启；逐一读取 securityfs，确认本轮预期 profile 为 enforce：

`sandbox-fuse-37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d`

22:55:02 实际 FUSE Pod `sandbox-pool-d5d90w8z14`（UID `e10eb3a6-2e07-429b-bf2d-29e6da35ba90`）在节点 `172.16.30.166` 上仍 Pending：workspace-mounter 为 CreateContainerError，CRI 事件明确为 `apparmor is not supported`；普通容器仍 PodInitializing。本轮只在该节点观测到实际 FUSE 创建拒绝，不推断另一个节点已通过或失败。

诊断时三个 API Pod 均 Running、未 Ready、零重启；一个普通池 Pod Running/Ready，但没有执行真实业务 API 请求。检测到明确 CRI 阻塞后停止业务用例并进入正常卸载，不等待整个启动超时，也不关闭 FUSE/LSM 以制造通过结果。

具体 CRI 缺口（有效禁用配置、宿主机 parser、能力缓存或厂商实现）尚未取得平台侧证据；不能从 loader 加载成功反推 CRI 支持。平台只读核查与维护窗口边界见 [AppArmor 节点前提](../deployment/apparmor-loader.md#节点前提loader-ready-不等于-cri-支持)。

## 正常卸载与资源清理：通过

诊断结束后直接执行正常 `helm uninstall`（不跳过 hooks）；22:55:25 命令成功并确认该隔离命名空间全部 Pod 消失，观测耗时约 20 秒（含卸载与 Pod 消失确认，不作为普通/FUSE 业务销毁性能指标）。

没有手动删除失败 FUSE Pod、重试卸载、强制删除或移除 finalizer。该结果验证新镜像下此类未启动失败 Pod 不再阻塞正常 release 清理；未单独抓取 Redis 内部 CAS 或每一步 finalizer/策略顺序，不能把黑盒卸载成功替代本地证据顺序回归。

按原 PVC/PV UID、claimRef 和 Delete 回收策略核验后，仅删除本轮三块测试 PVC；等待三块原 PV 消失，再以 namespace UID precondition 删除本轮命名空间。22:55:40 确认完成；未生成额外 PVC。

23:05 独立只读复核：本轮命名空间不存在、所有 `sandbox-cce-` 测试命名空间为零、三块原 PV 名称和 UID 均不存在、无 claimRef 指向本轮命名空间、无命名空间关联的 ClusterRole/ClusterRoleBinding；两节点 Ready，kube-system 无非终态异常未就绪 Pod。云平台账单未独立核验，不宣称计费资源审计通过。

## 仍需完成

平台确认并解决 CRI AppArmor 支持后，在获准隔离环境继续三副本 API、普通/FUSE 池命中、跨 API exec/TTL/文件操作、真实挂载与持久化、PID 1/s3fs enforce 和内核拒绝证据、业务销毁、升级及正常卸载验证。

标准 NetworkPolicy 的实际拦截、DNS 完整业务路径、最低 1.29 生命周期、DataPlane V2、OBS、Sentinel 冷恢复/节点 HA 仍未验收。本轮原始报告、私有配置、凭据、Secret、Pod env 与原始日志均不入提交。
