# etcd + FUSE 全新部署手册

本手册对应 `feat/etcd-state-management`，供运维人员在**新命名空间、新 etcd 前缀**独立安装。Helm Chart 安装 sandbox API、权限、网络策略和可选 AppArmor 加载器；**etcd 集群和 S3/MinIO 对象存储是前置依赖，不由该 Chart 创建**。本手册不迁移 Redis 数据，也不接管旧沙盒。

下文的 `sandbox-fuse-etcd`、镜像 tag 和端点是示例。API tag 更新为 2026-10-26 在 DS 与 CCE 验证过的恢复版本；安装前仍须核对仓库摘要和目标架构。不要复制 DS/CCE 的 etcd 前缀、PVC、Secret 或对象存储账号。首次安装保留独立前缀和凭据，确认 API 后再由业务侧切流。

## 1. 准备与边界

在仓库根目录确认版本；Chart 的 `Chart.yaml`、`templates/`、`files/`、`values.yaml` 和 `values.schema.json` 必须来自同一提交。

```sh
git branch --show-current
git rev-parse HEAD
helm show chart deploy/helm/sandbox
```

准备以下资源，并记下**实际**名称、地址和镜像摘要：

| 依赖 | 要求 |
| --- | --- |
| Kubernetes | Linux 节点；FUSE 节点有 `/dev/fuse`、mount propagation、可用的 Localhost AppArmor/CRI 支持；镜像架构与节点一致 |
| etcd | 建议三成员、不同节点和独立持久卷；所有 API 副本可访问客户端端口；为本安装分配一个此前不存在的 `/sandbox/<cell>/` 前缀 |
| 对象存储 | 已创建 bucket；API 到 endpoint 的 TLS/网络与 AK/SK 权限可用；为本安装选独立 `subPath` 或 bucket |
| 镜像仓库 | API、runtime、gateway、mounter、AppArmor loader 镜像可被目标节点拉取；必要时先创建本 namespace 的 `imagePullSecret` |

etcd **不要求证书或用户名密码**。`existingSecret: ""` 时客户端使用明文连接，适用于已做网络隔离的内部 etcd；若 etcd 启用 mTLS，则 Secret 须含 `ca.crt`、`client.crt`、`client.key`。不要把 etcd 客户端端口暴露到公网。无论是否使用 TLS，三成员健康和持久卷恢复都要由 etcd 的运维方案保证；本 Chart 不会创建或备份它们。

AppArmor loader Ready 只证明策略装载，**不证明 CRI 能启动受该策略约束的 FUSE 容器**。先在目标节点做实际容器创建与 FUSE 挂载验收；不能通过设 `allowMissingLSMForKind=true` 或关闭 LSM 来代替生产验收。CCE 曾出现 loader Ready 但 CRI 报 `apparmor is not supported`；不要据此推断另一套或当前节点的状态，逐节点复核。

部分 CCE 的 NodeLocal DNS 准入会改写 loader Pod 的 DNSPolicy、nameservers 和 searches，使严格启动门禁等待超时。若现场观察到这类改写，按 [Kubernetes DNS 准入兼容说明](kubernetes-dns-admission.md) 在环境 values 的 `config.runtime.kubernetes` 下显式设置 `disableNodeLocalDNSInjection: true`，并声明该文档限定的两项 `dnsOptions`；重渲染后确认 loader 与受管 Pod 都带预期标签和选项。不要通过放宽 Pod 身份匹配来绕过它。

## 2. 创建 namespace 与引用的 Secret

示例变量只在当前 shell 使用；`KUBECONFIG` 指向**目标集群**，不要依赖默认 context。创建前检查同名 namespace 是否已有资源，避免覆盖业务对象。

```sh
export KUBECONFIG=/secure/path/to/target-kubeconfig.yaml
CTX=<目标-context>
NS=sandbox-fuse-etcd
RELEASE=sandbox

kubectl --context "$CTX" get nodes -o wide
kubectl --context "$CTX" get namespace "$NS" || kubectl --context "$CTX" create namespace "$NS"
kubectl --context "$CTX" -n "$NS" get all,pvc,secret
```

用权限为 `0600` 的本地文件保存对象存储的 access key、secret key 和 API key。命令行只传文件路径，不把密钥写进 values、Git、终端历史或文档。`access-key`/`secret-key`、`api-key` 是 Chart 读取的**精确键名**。

```sh
kubectl --context "$CTX" -n "$NS" create secret generic sandbox-storage-credentials \
  --from-file=access-key=/secure/path/to/access-key \
  --from-file=secret-key=/secure/path/to/secret-key

kubectl --context "$CTX" -n "$NS" create secret generic sandbox-api-key \
  --from-file=api-key=/secure/path/to/api-key
```

如果已有 Secret，先核对名称、键名和轮换计划，不重复 `create`。私有镜像仓库的拉取 Secret 也必须位于这个 namespace。`image.pullSecretName` 供 API ServiceAccount 使用；当前受管 sandbox Pod 使用本 namespace 的 `default` ServiceAccount，因此还要给它设置同一个拉取 Secret，并确认后续 Pod 能拉取 runtime 和 mounter 镜像：

```sh
kubectl --context "$CTX" -n "$NS" patch serviceaccount default \
  --type=merge -p '{"imagePullSecrets":[{"name":"<镜像拉取Secret>"}]}'
```

启用 etcd mTLS 时，另外创建只含上述三个证书键的 Secret，并把名称填入下一节 `existingSecret`；明文 etcd 不需该 Secret。

## 3. 环境 values

把下面内容保存为 Chart **目录外**、权限 `0600` 的 `values-etcd-local.yaml`；替换尖括号内容。这里不包含真实密钥，镜像版本以 DS 已验证的 arm64 组合为示例，自己构建时应使用新 tag，不能覆盖已发布 tag。`fusePool.maxSize` 只限制准备中的空壳，**不是存活沙盒总数上限**。

```yaml
productionSafetyChecks: true
replicaCount: 2
autoscaling:
  enabled: false
podDisruptionBudget:
  enabled: true
  minAvailable: 1
redis:
  enabled: false

image:
  repository: registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-api
  tag: v1-publication-guard-85b2b98-arm64
  pullSecretName: <同命名空间的镜像拉取Secret；不需要时删除本行>

apparmorLoader:
  enabled: true
  image:
    repository: registry.i.huaxisy.com/library/ai-infra/sandbox-apparmor-loader
    tag: v0.3.24
    pullPolicy: IfNotPresent

config:
  runtime:
    type: kubernetes
    kubernetes:
      namespace: sandbox-fuse-etcd
      networkPolicyProvider: auto # 如需固定 Cilium，先确认目标集群的实现
  pool:
    minSize: 3
    maxSize: 20
  images:
    sandbox: registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-runtime@sha256:ec108aeee183c1cb0f3a5c481bf38038626fde8da96ae5a4624df396d5f6d7d8
    gateway: registry.i.huaxisy.com/library/ai-infra/sandbox-gateway:v0.2.12
  storage:
    state:
      backend: etcd
      etcd:
        endpoints:
          - <etcd-0-client-host>:2379
          - <etcd-1-client-host>:2379
          - <etcd-2-client-host>:2379
        prefix: /sandbox/<新cell名>/
        timeoutMs: 3000
        existingSecret: "" # 明文 etcd；mTLS 时改为证书 Secret 名
    filesystem:
      preset: minio
      bucket: <已创建的bucket>
      endpoint: <对象存储主机名，不带scheme>
      region: ""
      subPath: workspaces
      useSSL: true
      credentialsSecretName: sandbox-storage-credentials
      storageIdentity: <本安装固定的非敏感存储身份>
      credentialGeneration: rotation-1
  workspace:
    defaultMountMode: fuse
    enabledMountModes: [fuse, sync]
    fuseImages:
      mounter: registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-mounter@sha256:618cdc54af7677a62d9efa96d5c97c07963900eb9c928138d380332e07f1bceb
      docker: registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-docker:v0.2.12
    fusePool:
      minSize: 3
      maxSize: 20
      refillIntervalSeconds: 10
      prepareTimeoutSeconds: 120
    allowMissingLSMForKind: false
  security:
    apiKeySecretName: sandbox-api-key
```

`apparmorLoader.enabled=true` 时，Chart 根据随包策略计算新的 Localhost profile 名；不需要手填 `lsmProfile`。若对象存储是同 namespace 内的 MinIO，按其**实际 Pod label 和端口**配置 Chart 已有的 `networkPolicy.fuseStoragePodEgress`；外部对象存储不要照抄 DS 测试 MinIO 的 label。保留原有用户网络策略，不为修复存储访问新增宽泛出站规则。

## 4. 渲染、安装和验收

安装前再次确认 context、namespace、空前缀和 Secret 键名。`helm lint`/`template` 只检查配置与渲染；它们不证明镜像可拉取、节点可挂载或 etcd 可写。首次安装不要用 `--reuse-values` 引入旧 Redis 配置。

```sh
VALUES=/secure/path/to/values-etcd-local.yaml
helm lint deploy/helm/sandbox -f deploy/helm/sandbox/values-etcd.yaml -f "$VALUES"
helm --kube-context "$CTX" upgrade --install "$RELEASE" deploy/helm/sandbox \
  --namespace "$NS" --reset-values \
  -f deploy/helm/sandbox/values-etcd.yaml -f "$VALUES" \
  --wait --timeout 10m

helm --kube-context "$CTX" -n "$NS" list
kubectl --context "$CTX" -n "$NS" rollout status deployment/"$RELEASE"-api --timeout=5m
kubectl --context "$CTX" -n "$NS" rollout status daemonset/"$RELEASE"-apparmor-loader --timeout=5m
kubectl --context "$CTX" -n "$NS" get pods -o wide
kubectl --context "$CTX" -n "$NS" get svc "$RELEASE"-api
```

验收顺序：

1. 三个 etcd endpoint 健康、PVC 绑定；API 的 `/ready` 返回 200；普通与 FUSE 准备池都达到 `minSize`，没有持续补池失败。
2. 用独立 `workspace_path` 和短 TTL 经带 API key 的 `POST /api/v1/sandboxes` 创建 FUSE 沙盒，确认响应 `workspace_mount_mode=fuse`；在 `/workspace` 写入并读取文件，再从对象存储同 prefix 读回。
3. `GET /api/v1/sandboxes/:id/files/download?path=/workspace/index.html` 返回 `text/html; charset=utf-8`。新写入的 HTML 对象在 MinIO 上应为 `Content-Type: text/html`；旧对象不会被自动改写。
4. 测 `POST /exec`、公开文件上传/下载、`POST /workspace/sync`、`DELETE` 与后续 `GET` 404。FUSE 的 `from_container` sync 执行排空、flush、恢复；`to_container` 只校验现有实时挂载，不会重新复制对象。再用另一条短 TTL 沙盒验证自动销毁、配额归还和池补充。测试结束删除**自己创建**的对象 prefix。
5. 分别在计划承载 FUSE 的节点检查实际 mounter/s3fs 进程处于预期 AppArmor profile；至少验证一次挂载、flush、卸载和拒绝越权。一个节点成功不代表全节点成功。

测试环境可以用 `kubectl port-forward -n "$NS" svc/"$RELEASE"-api 18080:8080` 访问 ClusterIP；正式入口和切流由部署者配置。**不要把 `/health` 200、Helm `deployed`、loader Ready 或准备池 Pod Ready 单独当作业务通过。**

空闲 FUSE 准备池 Pod 可能显示 `1/2 Running`：其 mounter 已处于 `prepared`，而容器探针检查领取并挂载后的 `ready` 状态。结合 Pod 的 `sandbox.pool.state=prepared` 标签、API 补池状态和实际创建后的读写结果判断；若已领取的业务 Pod 仍未 Ready，再检查 mounter 事件与挂载日志。

## 5. 升级、停止和恢复

- 小版本升级先固定镜像 tag/digest，备份环境 values 和 etcd snapshot，检查活跃沙盒，再执行 `helm upgrade`。存储身份、凭据代次、mounter 镜像或 LSM 策略改变会触发池/清理契约变化；安排排空窗口，不能只重启 Pod。
- 三成员 PVC 整体恢复时，先停止新申请并安全排空业务与准备池，确认 API 已停机、受管状态归零；保存并校验最新 etcd 快照。将 StatefulSet 缩为 0 且确认三个 Pod 都退出后，分别挂载三块原 PVC，用**同一快照**及固定的三成员 peer 列表恢复到各 PVC 的暂存目录。三个暂存目录都验证通过后才替换各自的 `member`，原目录先改名保留；卸载临时 Pod，再启动三成员。必须核对三个 endpoint 的 cluster ID、member ID、revision 和快照标记，最后恢复 API/池并执行公开 FUSE 创建、读写、flush、删除到 404。不要把三个 PVC 分别从不同时间点的快照恢复，也不要在 etcd 或 API 仍写入时替换目录。CCE 实测记录见 [容量与发布报告](../superpowers/reports/2026-10-24-etcd-capacity-and-release.md)。
- API 多副本重启后需验证既有沙盒的 GET/exec 与 TTL；etcd 故障恢复应验证原 owner 仍占用。无法确认清理完成的状态保持 Unknown，不手删 etcd 键、finalizer 或 PVC 来“归还”配额。
- 不再需要测试安装时，先停止业务申请并完成公开 DELETE/TTL 排空，核对受管 Pod 与状态，再执行 `helm uninstall`。**卸载 Chart 不会删除外部 etcd、对象存储数据和它们的 PVC**；是否保留或销毁由部署者按各自备份策略决定。
- 本次 DS 验收是 20 个 FUSE 驻留及若干故障用例；它不是 10 万存活、大量同时到期、跨节点或业务流量的容量验收。上线前按目标存量单独测申请尾延迟、维护写入、etcd 磁盘/延迟、Pool 补充和故障恢复。

排障先看事件中的 `apparmor is not supported`、`profile not found`、镜像拉取、DNS/TLS/MinIO 认证、etcd endpoint 与前缀；不要通过关闭 AppArmor、放宽用户网络、重启业务节点或手工改 etcd owner 来消除报错。旧版 Redis 部署说明在 [Helm 部署与升级](helm-deployment-upgrade.md)；本手册的全新 etcd 安装以本页和 `values-etcd.yaml` 为准。
