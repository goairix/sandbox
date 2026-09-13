# Helm 部署与升级

本文只说明服务器上实际怎么替换 Chart、填写 values、构建镜像和执行升级。FUSE 原理与后端差异见 [Workspace FUSE 运维说明](workspace-fuse.md)。

## 1. 先记住结论

- 新版 `Chart.yaml`、`templates/`、`values.schema.json` 和 Chart 自带的 `values.yaml` 必须整套使用，不能新旧混搭。
- 服务器自己的配置单独放在 Chart 外，例如 `/opt/sandbox/env/values-prod.yaml`。
- AK/SK 直接写在环境 values 的 `config.storage.filesystem.accessKey/secretKey`，不再创建 workspace credential Secret。
- 一个 release 只配置一个后端，但同一后端可以同时服务 sync 和 FUSE。
- 普通升级执行一次 `helm upgrade`；backend、凭据、FUSE 镜像或 cleanup protocol 变化时，Chart 会先排空。

原有网络规则不能改：开放公网访问时仍禁止内网访问；确需访问其他内网服务时必须加明确白名单。FUSE 的 system egress 由 sandbox-api 自动解析，只开放对象存储 endpoint 的精确地址和端口。

## 2. 已有服务器目录怎么更新

假设服务器上已经有一套旧 Chart，最稳妥的方式是把新版 Chart 放到新目录：

```text
/opt/sandbox/
├── charts/
│   ├── sandbox-old/
│   └── sandbox-new/       # 从新代码完整复制 deploy/helm/sandbox
└── env/
    └── values-prod.yaml   # 服务器配置，长期保留
```

如果必须原地覆盖，也可以这样做：

1. 备份旧 Chart 和旧 `values.yaml`。
2. 用新版 `deploy/helm/sandbox` 覆盖整个旧 Chart，包含新版 `values.yaml`。
3. 把旧环境值逐项迁移到新版字段，最好保存成 Chart 外的 `values-prod.yaml`。
4. 不要把旧 `values.yaml` 原封不动放回新版 Chart，也不要只覆盖 `templates/`。

也就是说，不能采用“保留旧 `values.yaml`，只覆盖其他文件”的方式。旧 values 只能作为迁移参考。

迁移旧环境 values 时，删除 `caSecretKey`、`endpointHostIPs`、`systemEgressMode`、`dnsCIDRs`、`systemEgressCIDRs`、`endpointPorts`、workspace `proxyURL`，以及根级 `workspaceCA`。这些值现在全部由程序自动处理或已不再支持；保留在 `filesystem` 中会被新版 schema 明确拒绝，避免旧配置悄悄生效。

## 3. 环境 values 最小示例

MinIO：

```yaml
image:
  repository: registry.example.com/sandbox-api
  tag: v0.2.13

config:
  runtime:
    type: kubernetes
    kubernetes:
      namespace: sandbox-runtime
  storage:
    filesystem:
      preset: minio
      bucket: sandbox-workspace
      endpoint: minio.example.com
      region: "" # MinIO 自动使用 us-east-1
      accessKey: "<access-key>"
      secretKey: "<secret-key>"
      useSSL: true
      subPath: workspaces
      storageIdentity: production-minio
      credentialGeneration: rotation-1
  workspace:
    defaultMountMode: sync
    enabledMountModes: [sync, fuse]
    fuseImages:
      mounter: registry.example.com/sandbox-fuse-mounter:v0.2.13
      docker: registry.example.com/sandbox-fuse-docker:v0.2.13
    lsmProfile: sandbox-fuse
  images:
    sandbox: registry.example.com/sandbox-runtime:v0.2.13
    gateway: registry.example.com/sandbox-gateway:v0.2.13
```

只需把 `preset` 改成 `huawei-obs-public` 或 `huawei-obs-private`，并填写对应 endpoint；标准 `obs.<region>.<domain>` endpoint 的 region 可以留空自动派生。三种后端使用同一套 API、runtime 和 mounter 镜像，一次部署只选一个 preset。

AK/SK 轮换时同时递增 `credentialGeneration`，例如从 `rotation-1` 改为 `rotation-2`，确保旧 FUSE 空壳不会进入新凭据对应的 Pool。该字段不包含凭据。

因为 AK/SK 直接进入 values，它们也会出现在 Helm release 历史和 `sandbox-api` 环境中。环境 values 文件应限制为运维账号可读，集群 RBAC 也应限制读取 release Secret、Deployment 和 Pod 详情；排障时不要输出完整 values、渲染清单或容器环境。

`lsmProfile` 是节点上已经加载的 AppArmor localhost profile 名称，Chart
不会代替节点安装 profile。所有可能调度 FUSE Pod 的节点都必须加载同名
profile；否则 containerd 会报
`failed to generate apparmor spec opts: apparmor profile not found`。在仅用于
验证、且明确接受 mounter 不受自定义 LSM 约束的集群中，可临时使用：

```yaml
config:
  workspace:
    allowMissingLSMForKind: true
    lsmProfile: ""
```

生产环境不要使用这个兼容开关，应通过节点初始化或 Security Profiles
Operator 把允许 FUSE mount 的受限 profile 加载到全部目标节点。

## 4. 全新部署

```bash
CTX=ds-ai-research
NS=sandbox-fuse
RELEASE=sandbox-fuse
CHART=/opt/sandbox/charts/sandbox-new
VALUES=/opt/sandbox/env/values-prod.yaml

kubectl --context "$CTX" create namespace "$NS" 2>/dev/null || true
helm lint "$CHART" -f "$VALUES"
helm --kube-context "$CTX" upgrade --install "$RELEASE" "$CHART" \
  --namespace "$NS" \
  -f "$VALUES" \
  --reset-values \
  --atomic --wait --timeout 15m
```

## 5. 升级已有 release

先渲染检查，再升级：

```bash
helm lint "$CHART" -f "$VALUES"
helm --kube-context "$CTX" upgrade "$RELEASE" "$CHART" \
  --namespace "$NS" \
  -f "$VALUES" \
  --reset-values \
  --dry-run=server

helm --kube-context "$CTX" upgrade "$RELEASE" "$CHART" \
  --namespace "$NS" \
  -f "$VALUES" \
  --reset-values \
  --atomic --wait --timeout 15m
```

不要使用 `--reuse-values`，否则已经删除的 credential-file/Secret 字段可能被旧 release 带回来。

只改 API tag、普通 runtime tag、副本或资源，且 cleanup protocol 未变化时是普通滚动升级。修改 preset、endpoint、bucket、AK/SK、`credentialGeneration` 或 FUSE 镜像时，会改变 backend fingerprint；cleanup protocol 版本变化也会独立触发排空。Chart 的 pre-upgrade hook 会先执行 release drain。DNS 地址变化只会淘汰旧的未绑定空壳，不需要修改 values。

普通滚动升级不等于重建所有沙盒。Kubernetes 普通池和 FUSE 池按固定运行契约维护：

- 只改 API tag、日志或 API 副本数时，健康兼容的预热 Pod 保留；即使所有 API 暂时退出，也不会清空共享池。
- runtime 镜像、资源、安全属性、Pod 模板或控制协议契约改变时，先补齐新契约预热容量，再退掉无活跃 API 租约的旧契约空闲库存。运行模板版本由代码维护，不需要在 values 中新增配置。release scope 参与池身份，不能让两个 release 登记并领取同一个 Pod。
- 已领取、挂载中、业务使用中的沙盒不会因换池重启；旧 FUSE 业务实例按原持久化记录、不可变 UID、workspace owner 和运行健康证明接管，并在业务正常结束时清理。存储后端变化仍走上面说明的显式排空，不属于兼容换池。
- 普通池周期清理不会仅凭领取超时或副本租约丢失删除已领取 Pod，这些条件不能证明正在进行的申请已停止。无法确认的遗弃 claim 保守保留，交由受控 release drain 清理。
- API 租约无法确认时暂停领取和退旧，恢复后自动重新登记；旧库存状态损坏或删除未确认时保留记录并重试，不阻断健康新池。退旧单轮有清理时间预算，避免异常旧 Pod 长期拖住启动。
- `helm uninstall` 的 release drain 才负责清空库存和池代登记状态；不能把普通 API Stop 当作 uninstall。

首次引入这套契约指纹会更新预热库存一次。早期 FUSE 记录没有 release-scoped 池代登记，无法可靠确认其所属 release 及是否仍有旧 API 使用时，不自动删除；这类存量由受控 release drain/uninstall 清理，不能靠清空 Redis 或强删业务 Pod 处理。后续已登记的旧代可自动退役。

**排空边界：** 当前 release drain 的历史孤儿扫描覆盖整个 sandbox namespace 和 FUSE Redis 库。部署时必须让该 namespace 和 Redis logical DB 专用于当前 release，并在排空前停止这一范围内所有 API。运行期的池指纹 scope 隔离不代表已支持共享 namespace/Redis DB 的多 release 独立卸载；不要在这种共享部署中直接执行单个 release 的 drain/uninstall。

从不含 drain protocol 标记的旧 Chart 首次升级时，先保持 backend
fingerprint 不变，只更新新版 Chart 和 `sandbox-api`。这一步会给 Deployment
写入 `goairix.github.io/sandbox-drain-protocol: v1`，并把安全的 upgrade/resume/
rollback guard Hook 写入 release revision。确认这次同 backend 升级成功后，
才能在下一次 `helm upgrade` 中修改 backend fingerprint。若把协议迁移和
backend 切换混在第一次升级里，pre-upgrade Hook 会在缩容前拒绝，避免旧
revision 缺少 resume Hook 时发生不安全的 atomic rollback。

Kubernetes FUSE 崩溃恢复协议从 v1 升级到 v2 时，Deployment 会写入
`goairix.github.io/sandbox-cleanup-protocol: v2`。即使 backend fingerprint 不变，
pre-upgrade Hook 也会执行一次完整 drain。已安装 Deployment 必须先具有
`goairix.github.io/sandbox-drain-protocol: v1`；缺少该标记时 Hook 会在缩容前拒绝，
应先升级到包含 drain/resume guard 的过渡版本。v2 drain 会给仍在运行的旧 FUSE Pod
补加 cleanup finalizer，并在 Redis 持久化终止证据后完成删除。

启用 HPA 时，Deployment 始终省略 `spec.replicas`，普通升级不会改写 HPA
当前容量。pre-upgrade Hook 会在运行时比较集群中的 backend fingerprint；
只有 backend 或 cleanup protocol 确实变化时才排空并把 API 缩到 0。独立的
post-upgrade/post-rollback resume Hook 始终存在，但只有实时副本为 0 时
才恢复到 `autoscaling.minReplicas` 并等待可用，因此普通升级不会缩容。
首次安装也会执行同样的就绪检查。
release drain 还会清理“NetworkPolicy 已创建但 Pod 尚未创建”的未绑定
attempt 残留，不需要人工删除策略。

不要使用 `helm rollback` 跨 backend fingerprint 回退；Chart 会在
pre-rollback 阶段明确拒绝它，因为目标 revision 保存的环境无法安全排空
当前 backend。需要切回旧 backend 时，把目标配置作为一次新的
`helm upgrade` 执行，让运行时 fingerprint 检查和 release drain 正常完成。
同 backend rollback 仍可正常执行。

## 6. 镜像怎么构建

所有镜像使用版本 tag，不要求手工填写 SHA256，也不需要设置 `GO_BUILDER_IMAGE` 或 `SANDBOX_BASE_IMAGE`；Dockerfile 已有默认基础镜像。下面命令都在仓库根目录执行：

```bash
VERSION=v0.2.13
REGISTRY=registry.i.huaxisy.com/library/ai-infra
PLATFORM=linux/arm64

docker buildx build -f docker/Dockerfile --platform "$PLATFORM" -t "$REGISTRY/sandbox-api:$VERSION" .
docker buildx build -f docker/images/sandbox/Dockerfile --platform "$PLATFORM" -t "$REGISTRY/sandbox-runtime:$VERSION" .
docker buildx build -f docker/images/gateway/Dockerfile --platform "$PLATFORM" -t "$REGISTRY/sandbox-gateway:$VERSION" docker/images/gateway
docker buildx build -f docker/images/workspace-mounter/Dockerfile --platform "$PLATFORM" -t "$REGISTRY/sandbox-fuse-mounter:$VERSION" .
docker buildx build -f docker/images/sandbox-fuse/Dockerfile --platform "$PLATFORM" -t "$REGISTRY/sandbox-fuse-docker:$VERSION" .
```

Kubernetes FUSE 必须构建 `sandbox-api`、`sandbox-runtime`、`sandbox-gateway` 和 `sandbox-fuse-mounter`。Docker FUSE 还需要 `sandbox-fuse-docker`。你可以按现有流程分别构建各架构，再用 `docker manifest` 合并同一个版本 tag。

如果本次只升级 Docker multipart tmpfs、Docker pair network 自动回收或 Docker FUSE AppArmor 默认行为修复，只需重新构建 `sandbox-api`，并在环境 values 中更新 `image.tag`。其他项目镜像不需要因此重建，也不需要重启 Docker daemon。以 Kubernetes runtime 运行时，Docker pair network 回收逻辑不会参与 Pod 网络管理。

如果本次只升级 Kubernetes FUSE cleanup protocol v2 修复，同样只需构建
`sandbox-api` 并同步使用新版 Chart；`sandbox-runtime`、`sandbox-gateway`、
`sandbox-fuse-mounter` 和 `sandbox-fuse-docker` 不需要重建。首次 v2 升级会执行一次 drain，
正常情况下会短暂看到带 finalizer 的 Pod 处于 `Terminating`。

## 7. 升级后检查

```bash
helm --kube-context "$CTX" status "$RELEASE" -n "$NS"
kubectl --context "$CTX" -n "$NS" get deploy,pod,job
kubectl --context "$CTX" -n "$NS" logs deploy/"$RELEASE-api" --tail=200
```

然后通过 sandbox-api 分别创建 sync 和 FUSE sandbox，验证创建、执行、文件读写、销毁以及 FUSE Pool 回补。不要为了排查存储连通性放开内网默认拒绝策略。
