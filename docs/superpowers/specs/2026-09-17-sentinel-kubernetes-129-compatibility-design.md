# 内置 Redis Sentinel：Kubernetes 1.29 兼容整改设计

## 用户要求与本轮范围

用户于 2026-09-17 明确最低支持 Kubernetes 1.29，并在版本限制原因解释后要求继续。
本设计恢复原内置 Sentinel 设计中“启用了原生 sidecar 的受支持版本”这一边界，
不改变选主、身份协议、拓扑、持久化或业务请求路径。

本轮先整改本地 Chart、测试与部署说明；不操作 ds-ai-research 业务 release，
不检查分支差异、不创建 worktree、不回滚、不构建或发布业务镜像。
用户提供的 CCE 测试环境另按隔离验收方案执行，不安装不满足条件的 Sentinel。

## 根因及已取得证据

- `templates/_helpers.tpl` 的 `sandbox.validateRedis` 将内置 Sentinel 写死为 >=1.33。
- `internal/helmtest/redis_sentinel_test.go` 还断言 1.29 必须拒绝，理由是只接受正式稳定契约。
- 原设计允许已启用原生 sidecar 的受支持版本，因此实现门禁比原设计更窄。
- 本次使用无业务凭据的 fixture 做本地渲染：1.29.0、
  1.31.14-r20-31.0.62.9-arm64 均因 >=1.33 门禁失败，1.33.0 成功。
  渲染输出不保存、不打印；这些结果不代表实际部署通过。
- 身份核验器使用 `initContainers[].restartPolicy: Always`；prepare、identity、
  Redis 和 Sentinel 从 Downward API 的 `apps.kubernetes.io/pod-index` 获取 ordinal。
  当前 bootstrap 只接受字符串 0、1、2，缺失或非法值在读取卷及启动服务之前拒绝。

官方功能状态：原生 sidecar 从 1.29 起 Beta 且默认启用，1.33 正式稳定；
PodIndexLabel 从 1.28 起 Beta 且默认启用，1.32 正式稳定。
默认启用不等于每个托管集群都实际启用。

来源：[原生 sidecar](https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/)、
[功能开关状态表](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/)。

## 方案选择

采用最小兼容整改：最低版本改为 >=1.29，保留原生 sidecar、pod-index 和现有失败关闭行为，
通过分层测试确认实际能力。不增加部署配置项、不依赖供应商名称、不新增稳态网络请求。

不采用只删版本检查的方案：低于支持下限的集群仍须在渲染时明确失败。
不采用普通 sidecar 或从 Pod 名猜 ordinal 的替代方案：这会改变启动顺序或固定身份契约，
不是修复版本门禁所必需，增加恢复风险和变更范围。

## 兼容契约与错误处理

1. 内置 Sentinel 的版本门禁使用 >=1.29.0-0，正确处理托管集群的发行版后缀；
   低于 1.29 必须明确拒绝。Chart 包版本、appVersion 和所有镜像 tag 不随之改变。
2. 不把 Helm KubeVersion 或 API 字段存在当作功能开关已启用的证明。
   Helm 无可靠的通用 feature-gate 查询接口，不增加虚假的“自动发现成功”配置。
3. 原生 sidecar 必须实际保留 restartPolicy=Always、先启动并通过本地 startupProbe，
   才允许后续业务容器启动。API server dry-run 只证明准入，不证明 kubelet 的生命周期行为。
4. PodIndexLabel 必须由 StatefulSet controller 实际注入，通过 Downward API 得到准确 ordinal。
   不提供默认 0，不接受缺失 label，不从 shell、Pod IP 或自由字符串猜身份。
5. 功能被禁用或被准入改写时，拒绝准入或在启动阶段失败关闭；不以 PING、Pod Ready、
   共享密码或 DNS 同名替代身份核验。API 必须继续等待合法身份、Initialized、拓扑及 ACK。
6. 保留三成员/三不同 Linux 节点、三个独立 PVC、AOF、持久 Sentinel 配置、
   replica_ack/ACK1、quorum、认证、NetworkPolicy、LSM 和正常 drain 的全部门禁。
   productionSafetyChecks 不因版本整改被关闭。

## 测试与验收分层

### 本地整改验收

- 先补失败回归，再改门禁。覆盖 1.29、1.30、1.31、实际 CCE 1.31 后缀、1.32、1.33；
  低于 1.29 的版本应失败，错误指向支持下限及必需功能。
- 对各支持版本检查结构化 Sentinel 契约：原生身份 sidecar/探针、四个进程入口的 ordinal、
  三副本、强反亲和、三个 PVC 模板实例对应关系、公私钥隔离和 API 初始化门禁。
- 覆盖低版本的自动身份和外部身份引用两条路径；非法持久化、生产 HA、认证和安全配置
  继续失败。原有 API-only 更新不改变 Redis PodTemplate 的测试继续运行。
- 运行完整 Helm 测试及 bootstrap 命令/身份/恢复测试，不修改现有安全断言来求通过。
- 同步当前部署文档及 values 注释。历史报告中的当时结果保持原样，追加整改引用，
  不把历史失败改写成通过。

### 实际 Kubernetes 能力验收

使用独立临时 namespace 中无凭据、无 PVC、非特权的 StatefulSet fixture，检查真实
pod-index/Downward API、原生 sidecar 启动顺序及单业务容器重启后 sidecar 仍运行。
驱动必须显式绑定 kubeconfig/context/集群身份，等待有界，按本次 namespace/对象 UID 清理；
不修改业务资源或系统组件。真实 1.29 验证优先使用独立本地测试集群，CCE 1.31 证据单列，
不能以较高版本结果代替最低版本结果。网络功能 fixture 及完整 API/存储矩阵另行记录。

完整三节点 Sentinel 的首次初始化、非 0 号主冷恢复、容器重启、原卷 Pod replacement
只能在满足节点/卷条件的独立环境验收；单节点 CCE 不放宽反亲和、不使用 emptyDir 冒充 PVC，
不将功能 fixture 或 Go 恢复测试称为真实 Kubernetes Sentinel HA 已通过。

## 当前 CCE 测试环境的独立边界

该环境为单节点 Kubernetes 1.31，当前存储后端仍是 MinIO，尚未验证可用持久卷。
版本整改不能消除这些 Sentinel 前提。用户此前已同意测试专用 standalone Redis、
固定三个 API 进程、关闭 HPA 和缩小池子的方案；这只用于可信测试，不能描述为生产 HA。
真实 NetworkPolicy 执行及 AppArmor exact profile 仍需先取得证据。

## 设计自审与完成声明

本设计无新增配置或协议，无稳态性能开销；依赖检查与现有身份核验不降级。
明确区分渲染、准入、生命周期、完整 Sentinel 恢复/HA 四层证据。
不能在只通过渲染时宣称所有 1.29 集群兼容，不能以 CCE 1.31 代替 1.29 实测。
本文件是整改设计，不是已实施或测试通过的声明；书面设计确认后再编写实施计划。
