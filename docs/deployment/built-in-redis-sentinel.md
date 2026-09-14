# 内置 Redis Sentinel 部署

默认仍为单节点 `standalone`。启用 `redis.mode: sentinel` 后，Chart 部署固定三个成员，每个成员拥有独立 PVC，并运行 Redis、Sentinel 和一个仅做身份核验的原生辅助容器。Sentinel 是唯一选主组件；辅助容器不转发业务、不选主，初始化 Job 验证复制和写入 ACK 后才开放 API。

从旧内置 standalone 切换时，按本次约定先排空、备份，再由管理员卸载旧 release 后安装新 Chart；本功能不在线迁移旧 Redis 数据。下面的 `upgrade --install` 仅用于全新安装或保留原 Sentinel 卷组的重新部署，不能据此直接切换旧数据库拓扑。

## 部署前提

- Linux，Kubernetes 1.33 或更高版本；须启用原生 sidecar 和 StatefulSet pod-index 功能。
- 至少三个可调度 Linux 节点：强制按主机分散，不能在两个节点上凑三个成员。
- 可用的 RWO StorageClass，三个独立 PVC；不得复用原 standalone 的 PVC。
- release 名不超过 39 字符；release、namespace 与 clusterDomain 拼出的完整成员 DNS 不超过 253 字符。
- CNI 必须执行标准 NetworkPolicy；规则兼容 Cilium、Calico 和支持该能力的云 VPC，不依赖 Cilium CRD。
- 用户构建当前 `sandbox-api` 镜像，其中包含 `/app/redis-bootstrap`；Redis 使用配置的 Redis 7 镜像，无需重建项目 runtime/gateway/mounter 镜像。

## 自动创建与复用身份

`redis.sentinel.identitySecretName: ""` 是默认自动模式。真正的新安装无需手工创建 Secret：普通一次性身份 Job 创建 `<StatefulSet>-identity`，每个成员独立密钥。生成的 Secret 为 immutable、带 `helm.sh/resource-policy: keep`、无 Job ownerReference，不把私钥写入 values、Helm release manifest 或 Job 日志。已存在的身份只做验证和复用，不旋转、不更新、不覆盖；卸载不会通过 Job 垃圾回收删除身份。

Chart 只对固定状态 ConfigMap、实际身份 Secret、固定 StatefulSet 和三个固定 PVC 做六个 GET，不扫描命名空间；Helm 部署身份需具备这些固定对象的读取权限。只有渲染时没有旧 StatefulSet/状态/PVC、没有身份 Secret且处于自动模式，才向 Job 提供全新 clusterID 创建授权。新 StatefulSet 的 PVC 模板标记 `sandbox/redis-cluster-id`、CM 和身份 Job 共用同一个 clusterID；已有 CM 完整保留原 data 并检查固定 DNS 成员。旧状态即使是 Pending，也不是重新生成身份的许可。

保留状态或 PVC 却缺失身份 Secret时，安装/升级会失败，必须恢复原身份 Secret；保留 PVC 却缺失状态 CM 时，即使 Secret 尚在也会失败，必须恢复原状态 CM。不要以新 Secret、新 CM 或手工改 Pending 替代恢复。备份身份 Secret、认证 Secret、三个 PVC 和状态 ConfigMap，并限制读取权限。

填写非空 `identitySecretName` 则只读引用外部身份 Secret；即使填写自动模式的同名 Secret，也不会授予创建权限。该路径保留用于现有人工管理身份，不需要默认安装者执行手工生成命令。

## 既有 Sentinel 的升级兼容

StatefulSet 的 `volumeClaimTemplates`（包括模板 annotations）不可升级变更，Kubernetes 会比较允许变更字段之外的完整 spec。见 [Kubernetes 1.33 StatefulSet 更新验证](https://github.com/kubernetes/kubernetes/blob/v1.33.0/pkg/apis/apps/validation/validation.go) 中的 `ValidateStatefulSetUpdate`。因此旧版 Sentinel 的 `data` 模板没有 clusterID 注解时，Chart 继续保留无标记状态，不在升级时补写；身份 Job 只读验证和复用原 Secret，不获得全新创建授权。原模板已有标记时，必须等于原状态 CM 的 clusterID，并保留现有注解；不一致会拒绝渲染，不能通过改写注解修复。兼容逻辑只处理现有 annotations，不复制 PVC 模板的其他业务字段。

既有 Sentinel 升级应继续保留原 CM、三个 PVC、身份 Secret 和相同认证密码，不改 release/namespace/DNS 或卷组。若需重新创建 StatefulSet，应由管理员明确执行同名卸载/重装并保留这些原对象，不能把它当作新身份安装。此前普通 standalone 模式切换为 Sentinel 仍按本次约定先排空、备份、卸载再重装，不提供在线数据迁移。

## 认证与 values

下面使用两个不同的 inline 安全 token，让 Chart 自动创建认证 Secret，因此无需手工创建任何 Secret。两密码必须不同，各为 32~256 位字母、数字、`_`、`-` 的随机 token；此限制确保 Redis/Sentinel 重写配置后仍能冷恢复。演示 token 必须替换，values 文件应使用受保护的存储与权限。

```yaml
image:
  repository: registry.i.huaxisy.com/library/ai-infra/sandbox-api
  tag: REPLACE_WITH_NEW_SANDBOX_API_TAG # 从本轮提交构建的新 API 镜像
startupProbe:
  enabled: true # 初始化期间必须启用，等待预算由 Chart 自动补足
redis:
  enabled: true
  mode: sentinel
  password: REPLACE_WITH_RANDOM_DATA_TOKEN_0123456789
  persistence:
    enabled: true
    storageClass: "YOUR_RWO_STORAGE_CLASS"
    size: 10Gi
  sentinel:
    identitySecretName: "" # 默认自动创建/复用安装身份
    existingSecret: "" # 空值使用两个 inline 密码自动创建认证 Secret
    password: REPLACE_WITH_RANDOM_SENTINEL_TOKEN_0123456789
    dataPasswordKey: password
    sentinelPasswordKey: sentinel-password
    clusterDomain: cluster.local # 自定义集群 DNS 后缀时同步修改
    masterName: sandbox
    downAfterMilliseconds: 10000
    failoverTimeoutMilliseconds: 60000
    parallelSyncs: 1
    startupTimeoutSeconds: 600
    initializeTimeoutSeconds: 720
    ackTimeoutMs: 1000
apparmorLoader:
  enabled: false # 本次不启用加载器；不是实际内核 LSM 验收结论
```

inline 密码和认证 Secret 数据会进入 Helm release 历史，能读取 Helm 存储或 values 的主体可能获得凭据；自动创建身份并不消除此暴露边界。生产推荐由 Secret 管理系统管理认证 Secret，设置非空 `existingSecret`，包含 `password`、`sentinel-password` 两个键，并清空 inline 密码；外部认证与外部身份引用是两个独立选项。避免密码进入命令行和日志。数据密码和 Sentinel 密码互不回退。内置模式自动启用 `requireHA`、`replica_ack`、ACK1、三固定 DNS 地址以及初始化门禁，无需手工填写 `redis.external`。

```bash
CTX=YOUR_KUBE_CONTEXT
NS=YOUR_NAMESPACE
RELEASE=YOUR_RELEASE
# 离线结构检查不会执行现有对象 lookup；丢弃渲染结果，避免 inline Secret 写入终端/日志。
helm template "$RELEASE" ./deploy/helm/sandbox --namespace "$NS" \
  --kube-version 1.33.0 -f /opt/sandbox/env/values-prod.yaml >/dev/null
helm --kube-context "$CTX" upgrade --install "$RELEASE" ./deploy/helm/sandbox \
  --namespace "$NS" -f /opt/sandbox/env/values-prod.yaml \
  --wait --wait-for-jobs --timeout 20m
```

不要新旧 Chart 混用。身份 Job、状态 CM、StatefulSet 和初始化 Job 都是普通资源，不依赖 hook 等待顺序，避免 Helm 等 API Ready、API 又等 Job 的死锁。两个 Job 名均含 release revision 且不超过 63 字符；初始化 Job 仍仅有指定状态 CM 的 get/update 权限。身份 Job 预算 120 秒，只 GET 固定 Secret/CM/三个 PVC；自动模式额外授予所在 namespace 的 Secret CREATE，外部身份模式没有 CREATE。Kubernetes RBAC 不能用 resourceNames 约束 CREATE，因此该权限可创建 namespace 内任意名字的 Secret，固定名字与新安装授权由程序检查；没有 list/update/patch/delete 或 ClusterRole 权限。身份 Job 不挂 seed、公钥、认证或 PVC，不接收密码参数/ENV，仅专用 ServiceAccount 自动投影必要 API token；Job、SA、Role 和 RoleBinding 随 release 正常卸载。

身份 Job 的 120 秒是总预算，调度和镜像拉取也计入；程序的 `-timeout 2m` 是运行上限，不额外延长 Job deadline。先确认新 API 镜像可拉取，避免首次身份生成之前就超时。

若原身份 Secret 已生成，解决调度/镜像问题后可以新 upgrade revision 只读验证重试，不能删除保留身份。若首次安装中断、身份尚未生成，而普通 CM/PVC 已出现，缺身份保护会拒绝直接 upgrade，**不会自动重新授权或清理旧对象**。此时须由管理员核验是否确为未产生身份、无登记、无业务的失败首次安装，备份并确认精确资源处理范围后再开始新安装；不能把此情况当作已存在数据库的换钥匙许可，也不应盲目尝试恢复一个从未生成的 Secret。此限制是保留身份安全与跨资源非原子创建的明确边界。

Redis Pods 不携带 API token，不使用 Pod fsGroup；一次性 root init 只准备本 Pod 的挂载权限，长驻进程及身份 Job 均为 UID/GID999、drop ALL、只读根文件系统、RuntimeDefault seccomp。prepare 只挂自己的 seed 文件，Redis/Sentinel/API/初始化 Job 只能挂公钥。

内置 Sentinel 必须启用 API startupProbe；Chart 自动补足等待预算（默认至少15分钟），并保留用户更长的预算，避免初始化期间 liveness 重启风暴。API 门禁单次等待上限10分钟；超时会明确退出，由 Kubernetes 重试，不开放尚未初始化的 API。慢存储或较大的池子应相应延长 Helm timeout。

内置 Sentinel 禁止 `helm rollback`，也不要使用会自动回滚的 `--atomic`：Helm 会重放历史 Pending 状态，而不是重新执行 lookup，可能丢失初始化登记。pre-rollback Hook 会在改写资源前明确拒绝。需要部署旧应用版本时，将目标镜像和配置作为新的 `helm upgrade`，保留当前卷组、认证、身份及状态；这不是数据库回滚。

## 可用性与保留状态

单成员故障时，两个存活成员可由 Sentinel 自动切换；API 不经过核验器转发。普通 readiness 只读状态、不反复写 ACK。启动恢复有明确预算，无法证明既有主身份时拒绝启动，不自动 seed、清空数据或降级 best_effort。

`WAIT 1` 是复制确认，不是共识、磁盘同步提交或零数据丢失保证。AOF everysec 也不是同步刷盘；仍需可靠存储、备份和故障演练。

只停止 Redis 而保留辅助容器时，如果 Sentinel 已改变 monitor、但停机 Redis 的保留角色尚未收敛，该成员会拒绝 Inventory。初始化/升级核验不能把一个可达但无法核验的成员简单当作离线；正常业务选主仍由 Sentinel 处理，旧主启动包装器会保守恢复为从节点。

卸载默认保留三个 PVC、`*-redis-sentinel-state` CM 和自动创建的不可变身份 Secret；外部身份/认证 Secret 也必须自行保留。inline 认证 Secret 随 release 管理，不是 keep 对象：卸载前备份认证或使用外部认证 Secret，重新安装必须恢复相同密码。同名重新安装会复用原集群身份，**不等于清空状态**。不要换 release/namespace/DNS 后缀、重建密钥或手工改 Pending 来复用旧卷。真正从零安装必须由管理员先确认业务排空、备份及精确旧资源处理范围，不能依靠 Job/hook 自动删数据。

本示例关闭 AppArmor 加载器，不声称完成真实内核 AppArmor/LSM 验收。加载器的启用和节点验收见 [可选 AppArmor 加载器](apparmor-loader.md)。开启生产安全检查仍要求受限且经过验收的 LSM，不能使用 `allowMissingLSMForKind`。
