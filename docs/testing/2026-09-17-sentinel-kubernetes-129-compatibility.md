# 内置 Sentinel：Kubernetes 1.29 兼容整改及 CCE 1.31 实测

## 结论

2026-09-17 16:28（北京时间）完成首轮资源收尾复核；补齐第二节点后的差分复测见末尾。

Chart 的内置 Sentinel 版本门禁已从 1.33 恢复到 1.29；原生身份 sidecar、ordinal、
生产强反亲和、独立 PVC、认证、初始化门禁及安全校验不变。没有新增生产配置项、
修改镜像 tag、构建镜像或变更 Chart 版本。

CCE 1.31 单节点上，三个独立持久卷的 Redis/Sentinel 成员实际启动，初始化、鉴权、
仲裁、复制确认和单 Redis 容器重启后的恢复通过。**完整 Helm/API 部署未通过**：
AppArmor loader 已 Ready，但其 Pod 的 DNS 字段被准入改写，与 DaemonSet 模板不同，
当前严格模板门禁不接受这一差异。没有关闭门禁或绕过真实 LSM 来让 API 启动。

所有本轮隔离测试 namespace、Helm release、PVC 和关联 PV 对象均已确认消失。
这不是双华云网络策略、MinIO/FUSE、OBS 或节点级高可用的通过报告。

## 目标与隔离范围

- 显式使用私有 kubeconfig 的 `internal` context；不修改全局 context。
- kube-system UID：`cebeee78-9003-49bf-9442-3fea70515499`。
- Kubernetes：`v1.31.14-r20-31.0.62.9-arm64`；唯一节点实际为 amd64，
  Node UID：`69a79f2b-4f61-44c7-b46a-c46230c3530c`。
- 用户确认可申请测试云卷：先一个探测卷，回收后再三个成员卷；最大同时存在三个。
- StorageClass：已有 `csi-disk`，1Gi、RWO、Delete 回收；没有创建 StorageClass、
  用 emptyDir/hostPath 冒充持久卷或修改 Everest/系统副本数。
- 首次执行 release 为 `sentinel-compat`；测试专用 post-renderer 只把目标 StatefulSet 的
  hostname 强反亲和改为权重 100 的软反亲和。生产模板仍要求三个不同节点。
- 原私有 values 内容不变，文件权限收紧为 0600；凭据、完整清单和原始日志未入报告。
- 固定三 API、关闭 HPA；ordinary 池 1/3、FUSE 池 1/2；保留 productionSafetyChecks、
  真实 AppArmor、TLS 和 `allowMissingLSMForKind=false`。
- Pod/Service CIDR 使用用户提供的 `10.0.0.0/16`、`10.247.0.0/16`。
  隔离存储子路径为 `workspaces/sentinel-compat-ac4ae02c5fa9`，本轮未请求沙盒，测试驱动
  未执行 MinIO 对象操作；不是对象前缀审计通过的证据。

## 本地回归证据

按 RED/GREEN 实施：先加入支持版本矩阵，旧门禁在 1.29–1.32 因 >=1.33 拒绝而失败；
再最小修改门禁，矩阵通过。原来“1.29 必须拒绝”的测试改为独立资源预算断言，
没有删除原有结构/公私钥/网络/初始化安全断言。

版本矩阵覆盖 1.29、1.30、1.31、实际 CCE 后缀、1.32、1.33；每个版本分别检查
外部身份、自动身份以及两者的生产配置路径。1.27/1.28 拒绝；1.29/1.31 的无持久化、
认证密码/Secret key 相同、LSM 豁免、unconfined 和生产 standalone 均继续拒绝。

post-renderer 先因文件不存在失败，再实现并通过：恢复预期反亲和字段后，所有渲染对象
完整深度相等；无环境、业务 namespace、错误 release/namespace、目标缺失/重复、错误
selector/topology、混合反亲和、非法 YAML 及 alias 均固定提示失败，stdout 为空。

最终重新运行（`-count=1`，非测试缓存）：

```bash
go test -tags helmtests ./internal/helmtest -count=1
go test ./cmd/redis-bootstrap ./internal/redisbootstrap -count=1
go test ./cmd/sandbox -run RedisBootstrap -count=1
go test ./internal/runtime/kubernetes -run 'AppArmorLoader|PreparedPod|CreatePod' -count=1
ruby -c testdata/sentinel-kubernetes-compatibility/post-renderer.rb
git diff --check
```

这些是本轮范围的回归，不代替全仓测试或最低版本实际集群验收。

## 真实集群结果

| 检查 | 结果与证据边界 |
| --- | --- |
| 单 PVC 供应/挂载/读写 | 通过；非特权 Pod 写入随机 token、sync、读回匹配，正常终止后回收卷 |
| 三成员启动 | 通过；三个 ordinal、三个独立 Bound PVC，同节点运行，identity 原生 sidecar 常驻且 Ready |
| 身份与初始化 | 通过；身份 Job、initialize Job 成功，公开状态为 Initialized、固定成员数 3 |
| Redis 复制 | 通过；0 号 master 有两个 replica，1/2 号 replica 的 master link 为 up |
| Sentinel | 三成员均通过 CKQUORUM；这是同节点仲裁功能验证，不是节点级 HA |
| 鉴权隔离 | 正确凭据可用；无密码及交叉使用 data/Sentinel 密码均拒绝，未打印凭据 |
| 写入 ACK | 同一 redis-cli 连接执行独有短期 key 的 SET、WAIT 1 1000、DEL；结果 OK、1、1，测试 key 已删除 |
| 单容器重启 | 2 号 Redis 正常 SHUTDOWN 后由 kubelet 重启一次；Pod UID、三个 PVC UID 不变，identity restartCount 仍为 0，复制恢复 up 且 Pod Ready |
| 完整 Helm/API | 未通过；三个 API 均未 Ready，阻塞在 AppArmor loader 门禁，不是 Redis 初始化 |
| 正常卸载 | 通过；停止本次 Helm 等待客户端后正常 uninstall，不使用 --no-hooks/force/rollback，exit 0 |

SHUTDOWN 会关闭 exec 连接，最初脚本把该请求的非零返回当作失败。后续只读核对确认
请求已经生效，并以真实 restartCount、Pod/PVC UID、身份 sidecar 和复制状态判定恢复。
没有重复 SHUTDOWN、删除 Pod 或使用 NOSAVE/强杀来改变测试条件。

## 独立发现：AppArmor 门禁与 CCE DNS 准入

Loader DaemonSet generation/observedGeneration 均为 1，desired/current/ready/available
均为 1；Pod owner UID 指向当前 DS，Pod Ready 且零重启。API 的上一容器日志经私有
分类显示 AppArmor startup gate/deadline，未显示 Redis 初始化或 FUSE manager 启动失败。

在内存中默认化并去除已有门禁认可的 DaemonSet controller 差异后，当前 Pod 与模板只剩：

- 模板 `dnsPolicy: ClusterFirst`，没有 dnsConfig；
- 实际 Pod `dnsPolicy: None`，nameservers 为 `169.254.20.10`、`10.247.3.10`，
  并注入当前 namespace 的 cluster.local searches 及 resolver options。

在内存中仅恢复模板 DNS 两字段后，剩余差异为空；没有修改实际资源。
`internal/runtime/kubernetes/apparmor_loader_gate.go` 的模板比较没有接受这一 DNS
准入变化，因此这两项差异足以使当前比较返回 false。尚未部署 DNS 兼容修复后的版本，
不能把 Loader Ready 写成 sandbox 的真实 AppArmor enforce 已通过。

该问题单列，不在本次“只改版本门禁”的实现中扩大行为。后续须设计可信、精确、
可核验的 DNS 准入兼容处理，并同时检查 prepared Pod 的契约；不能直接忽略所有 DNS
差异、关闭 loader 门禁或降低网络/LSM 校验。

## 精确资源收尾

探测 namespace：`sandbox-cce-sentinel-test-71ef83b0191b`，
UID `ec9db390-9a49-455f-b9c0-f09870e8a69b`；成员测试 namespace：
`sandbox-cce-sentinel-test-ac4ae02c5fa9`，UID `e00f1f08-3512-4592-8cca-54f5422e45ba`。

| PVC UID（PV 名为 `pvc-<PVC UID>`） | PV UID |
| --- | --- |
| `c6333091-8cdb-4401-8ee4-26d32c994b5a`（探测卷） | `60c68e0f-3877-411a-b0e4-aab445ced958` |
| `ee0daf9c-9c74-41ce-bf79-54fd326af053`（0 号） | `44716c0a-b2d1-41e2-943c-a1b10211d958` |
| `9f30f5dc-82fc-4f2e-b47c-b8c54d45b82c`（1 号） | `eafca65a-d5cc-4205-a124-ade3d1f0dc1a` |
| `5055f968-6b78-4da5-befc-3031a336c9ed`（2 号） | `d13d5b36-2166-4c59-b448-fa80edc66fef` |

先正常卸载并确认 Pod 数为 0，再校验 claimRef/PV UID/Delete 回收并使用 UID 前置条件
删除各自 PVC。等待 PV 对象消失后，以精确 namespace UID 删除隔离 namespace，
其中保留的身份 Secret 与状态一并删除。本轮测试数据已移除，不涉及业务数据。
最终复核两个 namespace、四个 PV 均不存在，属于这两个 namespace 的残留 PV claimRef
及本次 release 数均为 0。没有直接删除 PV、finalizer、业务资源或云系统组件。

Loader 卸载不会主动卸载已经加载的内核 profile；本轮没有运行 parser 的 profile 删除
操作，也没有把 Kubernetes 资源回收写成节点 LSM 状态已恢复到测试前。

尚未查询独立云账单/云盘 API，不能仅凭 Kubernetes PV 对象消失宣称计费已核销。

## 未验收项

- 真实 Kubernetes 1.29 的生命周期；本轮没有创建本地 1.29 集群，较高版本或渲染
  不能代替最低版本实测。
- API/普通池/FUSE 功能、真实 LSM enforce、跨 API 状态和性能；API 门禁未打开。
- 未启用 DataPlane V2 的真实 NetworkPolicy 执行、隔离撤销/恢复、DNS/Service DNAT。
- 双华云 OBS，当前配置仍为 MinIO；未读取 OBS 业务对象。
- 非 0 号主冷恢复、原卷 Pod replacement、全组冷恢复、跨节点故障切换及节点级 HA。

本次完成的是版本门禁整改和上述 CCE Sentinel 功能子集；完整部署仍有明确阻塞项。

## 第二节点补齐后的差分复测

2026-09-17 用户新增节点后要求继续，并追加约束：测试资源前缀统一；兼容整改不得
影响之前集群已经验收的功能。本地测试工具已统一接受 `sandbox-fuse`，仍只允许独立
随机测试 namespace；旧执行名称作为历史事实保留，没有重命名或重建业务资源。
名称调整同样经过 RED/GREEN：统一名称的正向 fixture 先被旧 renderer 拒绝，修改
固定 release 后通过；业务 namespace、错误 release 等失败关闭断言保留。

新增 Node `172.16.30.193`，UID `b5ada28b-b127-4851-a0f2-a574ae572f13`；原节点 UID
不变，两个节点均 Ready、amd64、可调度。kube-system 16 个 Pod 无非终态未 Ready；
CoreDNS、Everest controller、NodeLocal DNS admission 和节点问题控制器均达到 2/2，
Everest driver、NodeLocal DNS 和 NPD 均达到 2/2。没有修改系统组件。

差分测试 namespace 为 `sandbox-cce-sentinel-test-d65e42b108a7`，UID
`b6703b03-f0ee-4b82-85bd-73969644860b`；使用原有镜像及相同安全参数、三个独立 PVC，
只在该测试 namespace 标记 `node-local-dns-injection=disabled`，不关闭 DNS、LSM 或
NetworkPolicy。该标记与实际 webhook selector 一致，命名空间准入保留它。
这是测试差分覆盖，不是生产配置整改，且仍不足以声明节点级 HA。

新的实际证据：

- NodeLocal 的 nameserver/search 改写停止，但即使 namespace/Pod 已禁用该 webhook，
  Pod 仍额外出现 `single-request-reopen`（空值）和 `timeout=2`，顺序不固定。
  ClusterFirst、Default 和 None 的服务端 dry-run 均如此；None 的 nameservers 不变。
  尚未确认这层 options 默认化的控制面实现，不把它全部归因于 NodeLocal webhook。
- 三个 Redis/Sentinel 再次 Ready/Initialized；正确鉴权、无密码拒绝、交叉密码拒绝、
  三成员 CKQUORUM、主从复制均通过。三个成员实际仍在原节点，本轮未验证跨节点复制。
- 两节点的 loader 均 Ready，DS UID 为 `c5a8794a-61be-4de5-b630-7ff28d7673d7`。
  通过 UID/resourceVersion 前置条件，仅在这一个测试 DS 模板显式写入上述 options，
  generation 从 1 变成 2；新 loader Pod 的 DNS 配置与当前模板一致。
- API 随后越过 loader 门禁，启动普通池时失败：`created ordinary Pod` 意图比较拒绝，
  当前诊断字段为 `spec.Priority`。没有申请任何沙盒，未进入 FUSE 池挂载/授权验收。
- `preparedPodIntentMismatchReason` 没有复用匹配函数的 Priority/其它准入正规化；
  服务端 dry-run 的 Priority 为 0、class 为空，匹配函数会正规化它。因此当前诊断
  字段不能直接当作拒绝的根因。DNS options 仍是已确认的额外准入差异，需要与诊断
  一致性一起整改；不通过放宽 Priority、删除 DNS 比较或忽略全部 Pod 差异求通过。

NodeLocal opt-out 的含义可对照[华为云说明](https://support.huaweicloud.com/intl/en-us/usermanual-cce/cce_10_0362.html)；
本集群的实际 selector、dry-run 和 Pod 差异才是上述结果的证据，官方示例不能替代实测。

这次完整 Helm install 仍未通过。确定阻塞后只停止本次等待客户端，不 rollback；正常
Helm uninstall（含 hooks）exit 0，无 drain blocked/config-load 错误。确认 Pod 为 0，
校验以下 PVC/PV 的 claimRef、UID、Delete 回收后，以 UID 前置条件删除 PVC：

| PVC UID（PV 名为 `pvc-<PVC UID>`） | PV UID |
| --- | --- |
| `ef241d1c-da59-4e4d-8a8d-875f9c6bc522`（0 号） | `84ddb0f5-8fa9-4f61-9bcf-e0e21d511a15` |
| `a3a5b9b1-4cd8-4c7c-a730-77509afe79e9`（1 号） | `da0f17de-d5a5-4823-bb88-833b7266d9a7` |
| `fb433a5b-2f56-4fb7-bb65-a007ec5b6d08`（2 号） | `dc4a853e-5790-458e-ba69-cb820f5f32de` |

PV 全部消失后删除精确 namespace，其保留状态/身份一并移除。最终累计三个测试
namespace、七个 PV 对象均不存在，属于这些 namespace 的 PV claimRef/release 残留均
为 0；系统 Pod 仍全部就绪。累计卷数量包括前面的一个探测卷和两次三成员测试，并非
七卷同时存在；未独立核验云盘账单。内核 profile 不主动卸载。

本次生产代码仍只有版本门禁变化，尚未实现 DNS 兼容；此前 ds-ai-research 已验收的
运行时路径未修改，也未操作该业务 release。后续兼容设计优先采用显式的管理员配置，
未配置时原行为不变，保留 resolver/search/安全字段精确校验及未知变化拒绝，并对
已配置、无重复 key 的 options 做明确语义比较。原集群路径与安全负向回归必须作为
交付门禁，云环境通过不能替代它。
