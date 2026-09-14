# 内置 Redis Sentinel 部署

默认仍为单节点 `standalone`。启用 `redis.mode: sentinel` 后，Chart 部署固定三个成员，每个成员拥有独立 PVC，并运行 Redis、Sentinel 和一个仅做身份核验的原生辅助容器。Sentinel 是唯一选主组件；辅助容器不转发业务、不选主，初始化 Job 验证复制和写入 ACK 后才开放 API。

从旧内置 standalone 切换时，按本次约定先排空、备份，再由管理员卸载旧 release 后安装新 Chart；本功能不在线迁移旧 Redis 数据。下面的 `upgrade --install` 仅用于全新安装或保留原 Sentinel 卷组的重新部署，不能据此直接切换旧数据库拓扑。

## 部署前提

- Linux，Kubernetes 1.33 或更高版本；须启用原生 sidecar 和 StatefulSet pod-index 功能。
- 至少三个可调度 Linux 节点：强制按主机分散，不能在两个节点上凑三个成员。
- 可用的 RWO StorageClass，三个独立 PVC；不得复用原 standalone 的 PVC。
- CNI 必须执行标准 NetworkPolicy；规则兼容 Cilium、Calico 和支持该能力的云 VPC，不依赖 Cilium CRD。
- 用户构建当前 `sandbox-api` 镜像，其中包含 `/app/redis-bootstrap`；Redis 使用配置的 Redis 7 镜像，无需重建项目 runtime/gateway/mounter 镜像。

## 一次性创建身份

身份 Secret 必须在安装前创建，不放入 values 或 Helm release。名字与 StatefulSet 一致，每个成员独立密钥；它与保留 PVC/状态 ConfigMap 绑定。下面只在**全新安装身份**时执行一次：

```bash
set -euo pipefail
CTX=ds-ai-research
NS=aiadp-sandbox-fuse
RELEASE=sandbox-fuse

# 使用当前代码生成；命令 stdout 只用于管道，不落入日志。
go run ./cmd/redis-bootstrap identity-secret \
  -namespace "$NS" -statefulset "$RELEASE-redis-sentinel" \
  | kubectl --context "$CTX" --namespace "$NS" create -f -
```

返回的 Secret 是 immutable。不要改用 `apply`，不要重新生成或覆盖已有身份；已有同名 Secret 时 `create` 失败是保护措施，不应自动删除重建。备份身份 Secret、认证 Secret、三个 PVC 和状态 ConfigMap，并限制读取权限。

## 认证与 values

推荐自行创建一个 `sandbox-redis-auth` Secret，包含 `password`、`sentinel-password` 两个键。两密码必须不同，各为 32~256 位字母、数字、`_`、`-` 的随机 token；此限制确保 Redis/Sentinel 重写配置后仍能冷恢复。通过受保护的文件或 Secret 管理系统创建，避免密码进入命令行和日志。

```yaml
redis:
  enabled: true
  mode: sentinel
  persistence:
    enabled: true
    storageClass: "YOUR_RWO_STORAGE_CLASS"
    size: 10Gi
  sentinel:
    identitySecretName: sandbox-fuse-redis-sentinel-identity
    existingSecret: sandbox-redis-auth
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
```

不使用现有认证 Secret 时，可填写 `redis.password` 和 `redis.sentinel.password`，但密码会进入 Helm release 历史，不推荐生产这样管理。数据密码和 Sentinel 密码互不回退。内置模式自动启用 `requireHA`、`replica_ack`、ACK1、三固定 DNS 地址以及初始化门禁，无需手工填写 `redis.external`。

```bash
helm lint ./deploy/helm/sandbox --kube-version 1.33.0 -f /opt/sandbox/env/values-prod.yaml
helm --kube-context "$CTX" upgrade --install "$RELEASE" ./deploy/helm/sandbox \
  --namespace "$NS" -f /opt/sandbox/env/values-prod.yaml \
  --wait --wait-for-jobs --timeout 15m
```

不要新旧 Chart 混用。初始化 Job 是普通资源，不是 post-install hook，避免 Helm 等 API Ready、API 又等 Job 的死锁。Job 名含 release revision；仅有指定状态 ConfigMap 的 get/update 权限。Redis Pods 不携带 API token，不使用 Pod fsGroup；一次性 root init 只准备本 Pod 的挂载权限，长驻进程均为 UID/GID999。prepare 只挂自己的 seed 文件，Redis/Sentinel/API/Job 只能挂公钥。

内置 Sentinel 必须启用 API startupProbe；Chart 自动补足等待预算（默认至少15分钟），并保留用户更长的预算，避免初始化期间 liveness 重启风暴。API 门禁单次等待上限10分钟；超时会明确退出，由 Kubernetes 重试，不开放尚未初始化的 API。慢存储或较大的池子应相应延长 Helm timeout。

内置 Sentinel 禁止 `helm rollback`，也不要使用会自动回滚的 `--atomic`：Helm 会重放历史 Pending 状态，而不是重新执行 lookup，可能丢失初始化登记。pre-rollback Hook 会在改写资源前明确拒绝。需要部署旧应用版本时，将目标镜像和配置作为新的 `helm upgrade`，保留当前卷组、认证、身份及状态；这不是数据库回滚。

## 可用性与保留状态

单成员故障时，两个存活成员可由 Sentinel 自动切换；API 不经过核验器转发。普通 readiness 只读状态、不反复写 ACK。启动恢复有明确预算，无法证明既有主身份时拒绝启动，不自动 seed、清空数据或降级 best_effort。

`WAIT 1` 是复制确认，不是共识、磁盘同步提交或零数据丢失保证。AOF everysec 也不是同步刷盘；仍需可靠存储、备份和故障演练。

只停止 Redis 而保留辅助容器时，如果 Sentinel 已改变 monitor、但停机 Redis 的保留角色尚未收敛，该成员会拒绝 Inventory。初始化/升级核验不能把一个可达但无法核验的成员简单当作离线；正常业务选主仍由 Sentinel 处理，旧主启动包装器会保守恢复为从节点。

卸载默认保留三个 PVC 和 `*-redis-sentinel-state` ConfigMap；身份/认证 Secret 为人工管理，也必须保留。同名重新安装会恢复原集群身份，**不等于清空状态**。不要换 release/namespace/DNS 后缀、重建密钥或手工改 Pending 来复用旧卷。真正从零安装必须由管理员先确认业务排空、备份及精确旧资源处理范围，不能依靠 hook 自动删数据。

AppArmor 加载器的启用和节点验收见 [可选 AppArmor 加载器](apparmor-loader.md)。开启生产安全检查仍要求受限且经过验收的 LSM，不能使用 `allowMissingLSMForKind`。
