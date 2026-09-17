# Kubernetes DNS 准入兼容设计

## 状态与目标

2026-09-17：用户已同意“管理员显式配置、默认行为不变、保留严格安全校验”的方向。
本文细化配置与验收边界，等待书面设计确认；不是修复已实现或集群测试通过的声明。

目标是解除当前双华云 CCE 1.31 的 DNS 准入契约阻塞，同时不改变此前
ds-ai-research 已验收的默认运行时功能。最低支持 Kubernetes 1.29，不提高版本下限。
不创建 worktree、不检查分支/main、不回滚、不构建或发布镜像、不委派子代理。

## 证据与根因边界

实测证据见 [CCE 兼容测试报告](../../testing/2026-09-17-sentinel-kubernetes-129-compatibility.md)。
当前测试集群已有两个 Ready 节点；三成员 Redis/Sentinel 功能子集通过，完整 API 未通过。
相关隔离测试的 namespace、release、PVC/PV 对象已经清理。

已确认三件事：

1. NodeLocal 准入把 loader 的 ClusterFirst 改为 None，并注入 nameserver/search。
   当前 loader 门禁要求实际 Pod 与当前 DaemonSet 模板一致，因此拒绝。
2. 对隔离 namespace 禁用 NodeLocal 注入后，nameserver/search 改写停止；仍出现额外
   options：`single-request-reopen` 空值、`timeout=2`，其顺序不固定。
   ClusterFirst、Default、None 的服务端 dry-run 均观察到这两项。
   尚未确认这层 options 默认化的控制面实现，不将其全部归因于 NodeLocal webhook。
3. 在唯一测试 DS 模板显式写入上述 options 后，API 越过 loader 门禁，随后普通池
   Pod 意图比较失败。日志指向 `spec.Priority`，但匹配函数会正规化无 class 的
   Priority=0，诊断函数没有复用该正规化，因而不能把日志路径直接当作拒绝根因。
   普通/FUSE 构造器都未请求这些 DNS options，仍存在已确认的准入差异。

[华为云 NodeLocal 说明](https://support.huaweicloud.com/intl/en-us/usermanual-cce/cce_10_0362.html)
说明可用 Pod 的 `node-local-dns-injection=disabled` 标签排除自动 DNSConfig 注入。
本集群实际 webhook selector 与标签行为已核对；官方说明不能代替本集群差分证据。

## 方案选择

- **采用：显式排除 NodeLocal 改写，并在请求中钉住已知 options。** 接受管理员声明的
  精确意图，不接受从实际 Pod 自动学习出的意图；原集群不配置则没有行为变化。
- 不采用自动探测云厂商并接受准入后的 DNS：难以建立可信期望，会把未知改写误认为
  合法，也容易改变 Cilium、Calico 或其它 VPC 环境的既有行为。
- 不采用修改系统 webhook、全局禁用 NodeLocal 或忽略 DNS 字段：影响范围过大；
  单独排除 NodeLocal 也不能解决当前额外 options。用户不需要手工创建 Kubernetes 资源。

本次只处理 DNS 意图与错误诊断，不顺带重构网络策略、销毁、Redis 或 AppArmor 策略。

## 管理员配置契约

新增两项，均位于现有 Kubernetes 配置，不进入租户 API 请求参数：

| 入口 | 排除 NodeLocal 注入 | 显式 DNS options |
| --- | --- | --- |
| Helm values | `config.runtime.kubernetes.disableNodeLocalDNSInjection` | `config.runtime.kubernetes.dnsOptions` |
| 本地 config.yaml | `runtime.kubernetes.disable_node_local_dns_injection` | `runtime.kubernetes.dns_options` |
| 环境变量 | `SANDBOX_RUNTIME_KUBERNETES_DISABLE_NODE_LOCAL_DNS_INJECTION` | `SANDBOX_RUNTIME_KUBERNETES_DNS_OPTIONS` |

默认值分别为 `false`、`[]`；省略与显式默认值语义一致。两项可以独立启用：其它环境
可能仅有 options 准入，不需要 NodeLocal opt-out。启用非默认值要求 runtime 为 kubernetes。
本轮不增加任意 DNSPolicy、nameserver、search 或任意 Pod 标签的配置接口。

当前 CCE 的预期覆盖值如下，确认实现和测试后才能作为部署配置发布：

```yaml
config:
  runtime:
    kubernetes:
      disableNodeLocalDNSInjection: true
      dnsOptions:
        - name: single-request-reopen
          value: ""
        - name: timeout
          value: "2"
```

第一版仅支持本次实际需要的两类 options，不扩大为任意 resolver 扩展：

- `single-request-reopen`：value 必须为空字符串；它是无值开关。
- `timeout`：value 必须为字符串形式的规范十进制整数，范围 1..30，无前导零。
- 每项必须且只能包含 `name`、`value`；两字段均为字符串，名称区分大小写。
  最多两项；同名重复、未知名称/属性、错误类型、null、缺失 value、非法值均拒绝。
- options 环境变量必须为 JSON 数组，最多 1024 字节；拒绝重复 JSON 属性、尾随内容、
  非数组和弱类型转换。bool 环境变量只接受 `true`、`false`，不把错误输入转为 false。
- 各项独立遵循 env > 文件 > 默认值；严格解析实际采用的新字段，不因为未采用的
  文件值推翻 env 覆盖。未声明新增项的旧文件保持原有读取规则。
- 新字段的文件配置读取保留原始类型与重复属性信息；声明该项所用的结构路径及
  options 拒绝 YAML merge 和 alias，避免 Viper 的弱类型/大小写转换遮蔽非法输入。
  Helm schema、模板校验、
  Config.Validate 和 runtime 构造校验保持同一范围。省略字段可默认化，显式 null 不可。
- 错误仅输出配置字段路径/类别，不回显原始配置、凭据或完整清单。

在边界验证后，按名称排序为进程自有的不可变配置副本；调用方或一个 Pod 的修改不能
污染其它 Runtime/Pod。schema 允许省略新增字段，不能要求现有线上 values 补全它们。

## 行为与组件边界

### Pod 意图构造

新增聚焦的 DNS 配置/校验模块，不把厂商分支散落进 pod.go 或 runtime.go。
普通创建、prepared FUSE 创建及模糊创建结果验证使用同一已验证的管理员配置。

启用 opt-out 时，在普通/FUSE **物理 Pod** 写入固定标签
`node-local-dns-injection=disabled`。管理员配置覆盖创建意图，租户不能设置其它值、
删除该标签或通过 UpdateLabels/claim 改写它；冲突在 Kubernetes 写入前拒绝。
标签来自 Pod metadata，不依赖 namespace 标签或请求用户修改 namespace。
关闭此配置时不新增标签限制，保持原标签行为；现有普通池 claim 仍只改 UID 绑定的
业务身份注解，不改变物理 CNI 身份，也不移除此标签。

DNS options 显式写入普通/FUSE 的现有 DNSConfig；保持它们原有 DNSNone、
nameservers、空 searches、FUSE endpoint/HostAliases 和网络策略计算不变。
不要因 opt-out 改用公共 DNS 替代原 FUSE resolver，也不改变存储 endpoint 解析授权。

### Loader 模板与门禁

Helm 在 loader PodTemplate 写入相同 opt-out 标签和 options。Loader 原 DNSPolicy
保持不变；不改 profile、digest、args、探针、资源、SA、挂载或调度策略。

启用非默认配置时，runtime 门禁先验证 DS 模板请求的 DNS options 与管理员配置
精确一致；启用 opt-out 时验证 DS 模板和实际 Pod 均保留 disabled 标签。
模板自身不能充当可信 options 配置源。随后继续检查实际 Pod 与当前模板的完整契约，
保留已有 UID、owner、generation、Ready、digest、调度及安全检查。
未启用时不添加新的 loader 模板限制或 DNS 兼容正规化。

这些行为只作用于 loader 与沙盒物理 Pod；API、Redis/Sentinel、bootstrap/identity
Jobs 的 DNS/PodTemplate 不变，也不修改系统 webhook、节点或 namespace DNS 配置。

### 比较正规化与错误诊断

DNSPolicy、nameservers、searches 始终精确比较，不接受 ClusterFirst→None 的自动改写。
只在管理员显式配置 options 时，对已验证的 options 集合进行按名称排序比较：

- desired 集合必须与管理员配置一致；实际集合必须完整、无重复且名称/值一致。
- 忽略这组配置的排列顺序。无值开关在 Kubernetes 对象中 nil value 与指向空字符串
  的 value 视为等价；仅适用于已显式配置的该开关，不泛化到任意字段或 timeout。
- 额外、缺少、重复、未知选项或值变化全部拒绝；不能删掉全部 DNSConfig 再比较。
- 默认配置没有新的正规化，维持当前 DNS 精确匹配的拒绝范围。

把 preparedPodIntentMatches 与 mismatch reason 使用的深拷贝/默认化/已有正规化集中，
并传递相同的 allowScheduledNodeName 与 DNS 配置上下文。匹配结果与诊断路径必须
一致；现有 Priority、SA imagePullSecrets、AppArmor、scheduler 正规化条件不扩大。
当前构造身份/UID/finalizer/labels/annotations 早期拒绝亦须得到对应原因，不在匹配
通过时假报 `spec.Priority`。错误只描述字段路径，不输出字段值。

Loader 仍保留独有的 DaemonSet controller 正规化；仅复用小范围 DNS 语义工具，
不把 loader 的调度/Priority 宽容规则带入普通或 FUSE matcher。

### 池子契约、升级与 drain

非默认 DNS 配置按排序后的规范形式生成摘要，加入 WarmPoolContract，继而影响普通池
fingerprint 和 FUSE pool key；Helm backend fingerprint 使用相同的规范配置语义。
打开、改变或关闭兼容配置时遵循现有安全 drain/过期池代际退休流程，不领用错误契约的 Pod。

两项均为默认值时，不追加任何字段、摘要或占位符，不提高 warmPoolTemplateVersion；
现有 WarmPoolContract 与 Helm backend fingerprint 必须逐字节保持不变。
options 改变顺序不能产生新代际。不要新建一个无条件参与指纹的版本字段。

API 与 drain/audit 的配置环境使用同一条件渲染 helper，非默认时才输出新 env，避免重复。
inspection/drain 不启用 loader 启动等待，但保留 DNS 配置语义与指纹一致性。
旧 Pod 的 UID 精确终止和资源审计不依赖新 DNS 意图匹配，不能因此无法卸载旧代际。

## 默认行为、性能与可用性门禁

默认路径的修改只允许纠正错误诊断；匹配通过/拒绝结果、构造模板、网络、LSM、
领用、准备、授权、终止规则必须不变。使用当前工作区确定性 fixture 保存基线，
不读取 main、不检查分支，也不将私有部署配置作为测试基线入库。

验证默认与显式 false/空列表下普通/FUSE Pod、loader/API/Redis PodTemplate 的结构
一致性，以及现有指纹的逐字节一致性。Helm 基线使用稳定的公开假数据 fixture，
排除测试输入的时间戳/随机身份因素，不通过删除安全字段求相等。

配置仅启动时解析并复制；创建/比较时最多处理两项，没有新增发现扫描、DNS查询、
Kubernetes API 请求、HTTP 转发、goroutine、全局锁或依赖。默认路径不做 options 排序。
并发 Runtime/Pod 自有副本以 race 测试验证。增加比较基准，比较前后默认 fixture 的
allocs/op 与 ns/op，并记录显式路径结果。默认 matcher 的 allocs/op 不增加；同一机器
至少五组重复采样，若 ns/op 中位数增加超过 10%，必须定位并解决或另行确认设计变更，
不能只因功能通过忽略性能，也不把一次采样抖动当作结论。
本次不声称关闭 NodeLocal 提高 loader DNS 性能；这是保持精确模板的兼容选择。

## 验收矩阵与交付边界

采用 RED/GREEN，不修改旧安全负向断言以获得通过：

1. 配置：文件/env/Helm 的默认、合法、独立配置、优先级与全部非法输入矩阵；
   验证重复属性、重复选项、alias/merge、大小写、类型、边界、错误不泄露原文。
2. 构造/契约：普通/FUSE 与 loader 配置一致；默认模板/指纹不变；显式配置改变代际、
   排列不改变代际；租户冲突和标签删改在写入前失败；claim 保留物理标签。
3. 准入：精确选项和排列变化通过；遗漏/追加/重复/值改写、nameserver/search/policy
   改写失败；显式 template 篡改失败。安全字段、AppArmor、volume、HostAliases、
   UID/replacement、owner/generation 和模糊创建补偿继续拒绝或精确清理。
4. 诊断：实际失败 DNS 字段不被已认可的 Priority=0/SA/default/调度差异遮蔽；
   修改显式 Priority 等既有非法差异仍失败。拒绝理由不包含值或凭据。
5. 本地交付门禁：完整 Helm 测试、config、cmd/sandbox、Kubernetes runtime 和相关池子
   测试，加 race/bench；完成后执行全仓 Go 测试与仓库既有 lint，不放宽规则。
   Helm 渲染版本包含 1.29、1.31（含当前 CCE 后缀）、1.33；低于 1.29 继续拒绝内置 Sentinel。
6. 已验收环境：原 ds-ai-research 的默认/Cilium 路径本地回归为硬门禁；镜像由用户构建
   更新后再验普通/FUSE 池命中、真实 AppArmor、跨 API 领用/操作、网络隔离及销毁。
   默认上线未完成真实验证时不得以 CCE 测试替代原集群回归结论。
7. 当前 CCE：镜像由用户更新后，使用显式配置在随机隔离 namespace、固定 release
   `sandbox-fuse` 验证三 API、三独立 1Gi PVC Sentinel、两节点 loader、普通/FUSE 池
   启动及有限 API/存储操作。最大同时三个测试云卷，不改业务/system 资源。
   单独的测试 renderer 仍只软化 Sentinel 反亲和，不能改 DNS、安全或其它模板。
   不做云节点宕机/强删 PVC/扩大计费范围；确认当前窗口和集群身份后再执行。
8. 收尾：正常 uninstall 含 hooks，确认 Pod 消失后校验 claimRef/UID/Delete 回收，
   以 UID 前置条件清理本轮 PVC/namespace，等待关联 PV 对象消失；不能只清 Job，
   不移除 finalizer或卸载内核 profile，不把 PV 消失表述为云账单已核销。

真实 1.29 生命周期尚未验收，1.31 实测/1.29 渲染不能代替它。当前 CCE 未开启
DataPlane V2，NetworkPolicy 是否执行仍需单列取证；DNS 修复不能写成网络隔离已通过。
当前后端是 MinIO，不是 OBS；OBS、开启 DataPlane V2 的集群与节点级 Sentinel HA
另行验收。若 API 通过后出现不同阻塞，先分类取证，不能通过本配置扩大信任边界求通过。

交付时同步 configs/config.yaml、Helm values/schema、中文部署/升级说明和新测试报告，
列出新增配置及哪些镜像需要用户重建。只有 Go API 行为需要新的 sandbox-api 镜像；
loader/Redis/bootstrap/mounter 的二进制和安全策略本次不变，不要求无依据地重建所有镜像。

## 设计自审

- 范围只含显式 DNS 意图与诊断一致性，未扩大为厂商自动信任或全局准入改造。
- 默认 false/空列表不产生模板或指纹变化；显式非默认变化则必须影响池代际。
- 配置源是管理员，模板/实际 Pod 不参与自动学习；保持 resolver 与安全字段精确比较。
- 正规化只覆盖已配置 options 的顺序及无值开关表示，不改变其它 matcher 规则。
- 分清本地回归、两个实际集群、最低版本和网络/存储/HA 的不同证据，不提前宣称完成。
- 无占位需求；书面设计确认后进入实施计划、实现、自审和回归，随后提交代码。
