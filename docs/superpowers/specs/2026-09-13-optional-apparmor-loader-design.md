# 可选 AppArmor profile 加载器设计

## 已确认需求与范围

用户已选择补充可选加载器，镜像由用户统一构建，线上卸载旧 release 后使用新版 Chart 重装。本功能不操作当前集群、不迁移历史状态，也不处理已暂缓的普通销毁/FUSE 创建性能优化。

采用 Chart 自带的轻量 profile 加载 DaemonSet，不强制引入 Security Profiles Operator。与节点初始化手工安装相比，DaemonSet 能覆盖节点重启和新增节点；与 Operator 相比，本项目只管理一个受限 FUSE mounter profile，避免引入额外 CRD/控制器。已有节点 profile 的部署仍能关闭加载器、沿用手工管理方式。

## 当前事实

- Kubernetes FUSE mounter 使用 privileged native sidecar 和 Bidirectional mount propagation；不在本功能中直接去掉 privileged，以免破坏挂载传播契约。
- 当前配置的 `config.workspace.lsmProfile` 选择节点 Localhost profile，但 Chart 没有安装它；当前测试环境清空该名称并开启 `allowMissingLSMForKind`。
- ds-ai-research 为 Debian 13、6.12 arm64、containerd 2.2.0。普通 Pod 抽查处于默认 AppArmor enforce，FUSE mounter 抽查为 unconfined。不能把它描述为整个集群没有 AppArmor。
- containerd 2.2.0 对显式 Localhost profile 有加载路径，但其它运行时对 privileged 容器不能承诺相同行为。因此实际约束验收不能省略。

依据：[Linux AppArmor](https://docs.kernel.org/admin-guide/LSM/apparmor.html)、[Kubernetes AppArmor](https://kubernetes.io/docs/tutorials/security/apparmor/)、[containerd 2.2.0 profile 处理](https://github.com/containerd/containerd/blob/v2.2.0/internal/cri/sputil/apparmor_linux.go)。

## 配置与兼容性

- 新增顶层 `apparmorLoader`，默认 `enabled: false`，包含加载器 image、resources、检查周期及启动超时；values 和部署说明使用中文注释。
- 开启时要求启用 FUSE、关闭 `allowMissingLSMForKind`，有效自动名称不得为空，禁止 unconfined、complain 模式及有效名称/内容不匹配；不要求被自动名称替代的手工字段非空。加载器不能用来自动改变宿主机内核启动参数。
- 提供随 Chart 分发的受限 mounter profile。先对包含固定名称占位符的规范化、自包含策略模板做 SHA-256，再生成 `sandbox-fuse-<digest>` 名称，最后替换占位符；不对含最终名称的文本反复求哈希，不依赖节点上的外部 include。模板/rules 变化必须产生不同名称。
- 开启加载器时由同一个 effective LSM helper 产生名称，用于 ConfigMap、加载器、API、productionSafetyChecks、backend fingerprint 和离线 drain；手工 `lsmProfile` 不再覆盖该自动名称。加载器关闭时仍使用手工名称。已安装 deployment 的 upgrade drain 继续保留原实际环境。
- 加载器关闭时保持现有手工 `lsmProfile` 路径。默认普通沙盒的 AppArmor 行为不变，不把本功能扩展为普通沙盒策略重设计。
- 新增 `config.runtime.kubernetes.nodeSelector`，默认空，供普通/FUSE runtime 使用。加载器开启时，双方都在同一有效选择器中合入 `kubernetes.io/os=linux`，显式冲突拒绝；关闭时不额外改变现有调度选择。选择器按稳定排序编码进入池运行时契约及 backend fingerprint，避免复用不符合新调度范围的旧 prepared Pod。
- 生产检查校验有效配置，不把配置通过当作真实 enforcement 通过。本功能包含启动门禁及调度范围接入，需要重建 sandbox-api，并新增加载器镜像；不因此重建 mounter/probe。

## 加载器与权限边界

1. 每个可能调度 FUSE 的 Linux 工作节点运行一个加载器；选择器与 runtime 同源，不允许单独收窄加载器范围。混合节点环境通过同一显式选择器绑定受支持节点；全部目标节点覆盖单独验收，不作为 API 全节点健康门禁。每个实际 prepared/授权 Pod 都必须证明进程受限，未加载节点不能静默绕过该检查。
2. 独立可信镜像包含 apparmor_parser，提供 linux/amd64 和 linux/arm64 构建方式。加载器用于节点安全管理，不接收租户输入、不接触 AK/SK 或 Redis/API 密钥。
3. 加载器作为可信节点管理员使用 privileged 容器，以访问节点 securityfs 并加载策略；它不是受限租户沙盒。只挂载必要 securityfs、只读 profile ConfigMap 和自身临时目录。禁止挂载宿主机根目录、containerd socket、业务 workspace，禁止 hostPID/hostNetwork；独立 ServiceAccount 不挂载 API token，不授予 Node patch 或集群范围写权限。部署说明明确 privileged/hostPath 对 Pod Security Admission 的要求。
4. 检查 AppArmor 已开启、securityfs 可访问、profile 可解析且以 enforce 加载；失败保持 NotReady 并给出脱敏原因，不回退 unconfined。
5. 策略成功后周期检查 exact 名称/enforce 状态；不存在时重新加载，持续错误时 NotReady。稳态不重复调用 parser，不扫描业务沙盒。
6. DaemonSet/Pod 退出或 Helm uninstall 不卸载内核 profile；仍运行的进程可能使用它，跨 release 也可能共享相同版本。旧策略的节点垃圾回收由独立管理员审计处理，不放进卸载 hook。

## 受限策略边界

允许 workspace-mounter、s3fs、fusermount3、私有 confinement 读取工具及镜像自检所需固定工具/动态库；限制写入到 `/workspace`、FUSE cache 和 mounter runtime 目录，允许所需 `/dev/fuse`、进程/挂载状态读取及后端连接。仅允许实际 FUSE 挂载和正常卸载所需的 mount 规则，不使用无约束的文件写入、mount 或 profile 逃逸规则；固定子进程通过继承型执行规则保持约束，不允许 unconfined 执行/任意 change_profile，并禁止 MAC_ADMIN/MAC_OVERRIDE 能力使用。镜像自检会对 workspace anchor 做 chmod，策略必须允许这项初始化而不能泛化成任意路径写入。

策略针对当前镜像和挂载契约，不只提供一个名为 sandbox-fuse 的 allow-all profile。构建自检、真实挂载/flush/unmount、明确越权负例共同决定策略是否可发布。

## 启动、更新与失败行为

- API 启动门禁验证本 release DaemonSet 的 UID、observedGeneration 与当前 generation 一致、策略摘要和目标实例数非零，并至少有一个当前策略的 Ready 实例，检查有总超时。用现有 namespace Pod 读取权限核对该实例的 controller owner UID、当前模板/策略摘要和 Ready 条件，不能把 DaemonSet 的总 Ready 数当作新策略 Ready 数。门禁不要求所有工作节点同时健康，避免一个节点故障阻止全部 API 重启；对全部目标节点的覆盖检查另列部署验收，不增加运行期的全节点就绪依赖。
- DaemonSet Ready 仅证明节点加载器成功，不代表实际 mounter 已受约束。加载器开启时，runtime 在 prepared 发布和挂载授权前必须通过固定私有 `/proc/1/attr/current` 读取校验 exact profile/enforce，并联合校验 Pod UID、mounter 零重启和安全模板。未受约束、读回失败或身份变化均不能领取/授权，不退回 unconfined。不能只把此项留给人工验收，也不向公共 Exec 暴露 mounter 或通用 shell 能力。
- 门禁新增权限仅为本 release DaemonSet 的 namespace 级、指定 resourceName get；核对实例复用 runtime 已有的 namespace Pod get/list 权限，并按 release/owner/template 精确筛选，不增加 Node 写或集群级读取权限。加载器本身不需要 Kubernetes API 能力。DaemonSet 状态与实际进程检查都保留独立超时，不把配置错误导致的失败误报为安全成功。
- 节点重启/新增节点由 DaemonSet 重载，Kubelet 在 profile 缺失时拒绝相关 Pod，不能静默退回不受约束状态。
- 策略或镜像更新保留现有池契约、精确 UID/generation、租约和清理机制；新策略名称进入 LSM 配置，旧活动 Pod 不因节点策略原地替换而受影响。
- 真实 enforcement 检查要读取 mounter 及其挂载子进程的实际 AppArmor profile，并执行有界拒绝负例。若目标运行时忽略 profile，本环境不能按生产加固验收通过；不通过自动扩大权限解决。

## 验收

- Helm 默认不渲染加载器，启用时名称/API/drain/profile/fingerprint 一致、名称哈希无循环、双方调度集合一致，配置错误和缺失安全前提明确拒绝；测试验证 hostPath/RBAC 不越界。
- 加载器测试覆盖 parser 失败、内核未开启、profile 缺失、enforce/complain 区别、检查失败恢复、只加载自身策略，以及退出不卸载策略。
- Linux 镜像测试验证策略语法，隔离 Kubernetes 环境验证初次加载、重启重载和启动门禁。不把 macOS Docker Desktop/kind 的有限验证当成宿主机 enforcement 证据。
- 增加 runtime 回归：加载器 Ready 但实际 mounter unconfined/complain/不同 profile、Pod 替换、容器重启及私有读取失败都不能发布/授权；一个加载器节点故障不使启动门禁形成全节点故障依赖。私有检查开销单独测量，不承诺没有额外 API Exec 成本。
- 用户部署后验收全部目标节点覆盖、普通与 FUSE API 生命周期、跨副本操作、mounter/挂载子进程真实受限、flush/unmount、越权拒绝和最终清理；当前线上不由本轮擅自安装 DaemonSet。

## 设计自审

加载器是可选节点管理组件，不是内核启用工具；profile 加载不等同进程受限；保留 privileged 挂载传播但在发布/授权边界要求实际约束证据；不承诺跨运行时普遍有效；旧 profile 不自动卸载。自审修正记录见 [双项规格自审](../../testing/2026-09-13-apparmor-sentinel-design-review.md)。以上边界均作为测试/文档要求，不作为已实现结果。
