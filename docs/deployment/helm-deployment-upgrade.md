# Helm 部署与升级

本文只说明服务器上实际怎么替换 Chart、填写 values、构建镜像和执行升级。FUSE 原理与后端差异见 [Workspace FUSE 运维说明](workspace-fuse.md)。

## 1. 先记住结论

- 新版 `Chart.yaml`、`templates/`、`files/` 和 `values.schema.json` 必须整套同步，不能新旧混搭；推荐保留新版默认 `values.yaml`，用外部环境 values 覆盖。
- 服务器自己的配置单独放在 Chart 外，例如 `/opt/sandbox/env/values-prod.yaml`。
- AK/SK 直接写在环境 values 的 `config.storage.filesystem.accessKey/secretKey`，不再创建 workspace credential Secret。
- 一个 release 只配置一个后端，但同一后端可以同时服务 sync 和 FUSE。
- 普通升级执行一次 `helm upgrade`；backend、凭据、FUSE 镜像、有效 LSM/nodeSelector 或 cleanup protocol 变化时，Chart 会先排空。
- AppArmor 加载器需要单独构建、推送镜像，默认关闭；真实隔离验收通过前不能直接开启线上开关。

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
4. 对照新版 schema 迁移旧环境值，不要只覆盖 `templates/`；启用 AppArmor 时还必须有 `files/apparmor/workspace-mounter.profile`。

推荐使用新版 Chart 默认值加外部环境覆盖。如果继续维护 Chart 内的自定义 `values.yaml`，也可以保留，但须核对新版字段/schema，并完整同步其余 Chart 文件；不能保留已删除字段或混用旧模板。可选 AppArmor 配置缺失时默认关闭，不需要为了未启用的组件手动补齐所有默认项。

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

未启用可选加载器时，`lsmProfile` 是节点上已经加载的 AppArmor localhost profile 名称，Chart
不会代替节点安装 profile。可选自动加载方案见 [AppArmor 加载器](apparmor-loader.md)。所有可能调度 FUSE Pod 的节点都必须加载同名
profile；否则 containerd 会报
`failed to generate apparmor spec opts: apparmor profile not found`。在仅用于
验证、且明确接受 mounter 不受自定义 LSM 约束的集群中，可临时使用：

```yaml
config:
  workspace:
    allowMissingLSMForKind: true
    lsmProfile: ""
```

生产环境不要使用这个兼容开关。可选择本 Chart 的可选 AppArmor 加载器，或通过节点初始化/Security Profiles Operator 加载受限 profile；无论哪种方式，全部目标节点和实际 mounter/子进程的约束都须验收。

### 本次 AppArmor 构建后需要增加什么配置

先按第 6 节构建并推送加载器。将下面字段合并到已有环境 values，重复的顶层块须合并，不要直接覆盖整份配置：

```yaml
apparmorLoader:
  enabled: false # 构建后先准备配置，尚不启用线上加载器
  image:
    repository: registry.i.huaxisy.com/library/ai-infra/sandbox-apparmor-loader
    tag: v0.1.0 # 必须与实际推送版本一致
    pullPolicy: IfNotPresent
```

如果 API 本轮也重建了，再更新已有的 `image.repository/tag`；如果当前 API 已包含加载器门禁和私有 enforce 检查，可以复用，不用重新打包。此准备阶段保留现有 workspace、Redis、网络和节点选择器配置，不要在加载器仍关闭时提前关闭测试绕过开关或换成未加载的手工 LSM 名称。

下面片段只用于获准的隔离环境，不能构建完就直接合到线上启用。隔离环境自己的对象存储、Secret、网络与 namespace 须另行配置，不能指向业务环境：

```yaml
apparmorLoader:
  enabled: true
  priorityClassName: "" # 有 globalDefault 时填写管理员确认的既有可信类
  image:
    repository: registry.i.huaxisy.com/library/ai-infra/sandbox-apparmor-loader
    tag: v0.1.0
    pullPolicy: IfNotPresent
  checkIntervalSeconds: 10
  parserTimeoutSeconds: 10
  startupTimeoutSeconds: 180

startupProbe:
  enabled: true

config:
  runtime:
    type: kubernetes
    kubernetes:
      namespace: "" # 跟随隔离 release 命名空间，不填写线上 namespace
      nodeSelector:
        kubernetes.io/hostname: ds-ai-worker-2 # 测试节点示例；加载策略须另获授权，不是线上配置
  workspace:
    enabledMountModes: [sync, fuse]
    allowMissingLSMForKind: false
```

检查周期、超时和资源可以省略以使用 Chart 默认值。启用后 Helm 自动创建 profile ConfigMap、DaemonSet、专用 ServiceAccount 和 API 只读 Role/RoleBinding；无需手工创建这些资源或在节点安装 parser。`lsmProfile` 自动使用摘要命名策略，原手工名称不再覆盖它。节点须已启用 AppArmor，准入须允许这个可信 privileged/hostPath 组件；Helm 不改变内核开关。

隔离环境完成实际 profile/enforce、mount/flush/unmount、越权拒绝和重载验证后，再安排生产维护窗口启用。生产 nodeSelector 应使用全部已验收节点的共同标签，不照抄单个测试 hostname；选择器同时影响普通池、FUSE 池和加载器。完整配置、权限及验收边界见 [AppArmor 加载器](apparmor-loader.md)。

`sandbox-apparmor-loader:v0.3.24-arm64` 在 worker-2 的组件级验收见 [2026-09-14 结果](../testing/2026-09-14-apparmor-live-prepared-results.md)。[五节点公共 API 验收](../testing/2026-09-16-all-node-apparmor-api-results.md) 已于2026-09-17续测完成：worker-1至worker-5普通/FUSE生命周期、真实s3fs enforce、四类内核拒绝、自己的loader Pod重建及精确清理全部通过，失败实验也已正常清理。没有升级业务release或改策略，本次记录无需重建镜像或升级Chart；冷拉取/准备长尾仍未查明，不扩展为节点冷启动、其它版本组合、Redis HA或其它CNI通过。

## 4. 全新部署

内置 Redis 高可用的身份准备、三节点/PVC 要求和完整配置见 [内置 Redis Sentinel](built-in-redis-sentinel.md)。默认 standalone 不会自动升级为 Sentinel。

内置 Sentinel 的最低 Kubernetes 版本为 1.29，不再把原生 sidecar 的 1.33 正式稳定时间
当作支持下限；1.31 生产集群可按实际功能验收。`SidecarContainers` 和 `PodIndexLabel`
必须启用，Helm 渲染或服务端 dry-run 不能替代真实启动/身份/恢复验证。生产仍要求三个
不同节点和三个独立 PVC；同节点三成员只可用于隔离部署功能测试，不代表节点级 HA。
本次版本门禁整改只需同步 Chart，未新增生产 values 配置，也不需要重建镜像。

以下安装/升级命令中的 `--atomic` 仅适用于 standalone/external；内置 Sentinel 必须去掉 `--atomic`，失败时保留当前身份状态，通过新的 upgrade 修复，不自动 rollback。AppArmor 未完成隔离验收时继续保持关闭。

```bash
CTX=ds-ai-research
NS=sandbox-fuse
RELEASE=sandbox-fuse
CHART=/opt/sandbox/charts/sandbox-new
VALUES=/opt/sandbox/env/values-prod.yaml

kubectl --context "$CTX" create namespace "$NS" 2>/dev/null || true
test "$(helm show chart "$CHART" | awk '$1 == "version:" { print $2 }')" = "0.3.0"
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
test "$(helm show chart "$CHART" | awk '$1 == "version:" { print $2 }')" = "0.3.0"
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

Chart `0.3.0` 是本轮 Helm 包版本，不代表镜像已经统一发布为 `v0.3.0`；当前
`appVersion` 和 values 中各镜像 tag 仍按各自实际构建版本维护。后续统一清空并重建
镜像时，再一次性更新这些 tag。不要仅凭 Chart 版本推断容器版本。

不要使用 `--reuse-values`，否则已经删除的 credential-file/Secret 字段可能被旧 release 带回来。

只改 API tag、普通 runtime tag、副本或资源，且 cleanup protocol 未变化时是普通滚动升级。修改 preset、endpoint、bucket、AK/SK、`credentialGeneration`、FUSE 镜像、有效 LSM 或 nodeSelector 时，会改变 backend fingerprint；cleanup protocol 版本变化也会独立触发排空。首次开启 AppArmor 加载器会改变有效 LSM，须安排排空维护窗口。Chart 的 pre-upgrade hook 会先执行 release drain。DNS 地址变化只会淘汰旧的未绑定空壳，不需要修改 values。

内置 Sentinel 的 `prepare`、`identity`、身份/初始化 Job 和 rollback guard 使用
`redis.sentinel.bootstrapImage`，不再复用 `image.repository/image.tag`。完成首次迁移
后，只更新 API tag 不会改变 Redis StatefulSet PodTemplate，也不应重启 Redis。
bootstrap 镜像 tag 只在 `cmd/redis-bootstrap` 本身改变时更新；没有隐式 API fallback。
首次从旧 Chart 迁移会因为 helper 镜像字段改变而执行一次预期的 OrderedReady Redis
滚动，须按 Sentinel 维护窗口观察 quorum。相关根因和验收边界见
[API 升级触发内置 Redis Sentinel 重启记录](../testing/2026-09-15-sentinel-api-upgrade-restart-incident.md)。

Sentinel 的两个一次性 Job 只在确有工作时渲染：身份 Secret 不存在时创建
`*-redis-sentinel-identity-<revision>`，state phase 为 `Pending` 时创建
`*-redis-sentinel-initialize-<revision>`。身份完整且 state 已是 `Initialized` 的普通
upgrade 不会再次创建这两个 Job或对应 RBAC。Helm 只会处理当前 release manifest 仍在
跟踪的旧资源；更早 revision 已经遗留成孤儿的 Completed Job/Pod 不保证被后续 upgrade
回收。旧 Job 自带 `ttlSecondsAfterFinished: 86400`，由集群 TTL controller 到期清理；
新 revision 不再继续产生这类资源。保留 state/PVC 但身份 Secret 缺失、或 state phase
非法时仍会在模板阶段拒绝升级，不能通过重新生成身份绕过。

严格 API-only 升级使用仓库脚本留证。先在健康基线执行 snapshot，再手工执行只改变
`image.tag` 的 upgrade，最后 verify：

```bash
scripts/verify-sentinel-api-only-upgrade.sh snapshot \
  --context "$CTX" --namespace "$NS" --release "$RELEASE" \
  --output /tmp/sandbox-sentinel-before.json

# 此处手工执行只改变 image.tag 的 helm upgrade。

scripts/verify-sentinel-api-only-upgrade.sh verify \
  --context "$CTX" --namespace "$NS" --release "$RELEASE" \
  --snapshot /tmp/sandbox-sentinel-before.json
```

脚本不执行 upgrade、不读取 Secret，只比较 API rollout、Redis StatefulSet revision 和
PodTemplate、三个 Redis Pod 的 UID/restartCount/imageID 以及一次性 Job 集合。任一
Redis Pod 被替换、重启或出现新 Job 都会返回失败。

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
standalone/external 的同 backend rollback 仍可正常执行；内置 Sentinel
例外，pre-rollback Hook 明确拒绝历史状态重放，也不要使用 `--atomic`。
需要更换应用版本时使用新的 `helm upgrade` 保留当前初始化状态和卷组。

## 6. 镜像怎么构建

所有镜像使用新版本 tag，不覆盖已发布版本，不要求手工填写 SHA256，也不需要设置 `GO_BUILDER_IMAGE` 或 `SANDBOX_BASE_IMAGE`；Dockerfile 已有默认基础镜像。下面命令都在包含本次实现的仓库根目录执行，不从尚未合入功能的旧 main 打包。构建及推送由部署者执行，本文不是镜像发布成功记录。

### 构建环境及首次完整镜像准备

以下为完整镜像准备清单，不表示每次升级都要全部重建；已有兼容镜像可以复用。仅处理 AppArmor 时跳到下一小节的两类镜像要求。

```bash
cd "$(git rev-parse --show-toplevel)"
docker login registry.i.huaxisy.com
docker buildx version

# 首次创建；同名 builder 已存在时跳过 create，不要删除已有 builder
docker buildx create --name sandbox-apparmor-build --driver docker-container
docker buildx inspect sandbox-apparmor-build --bootstrap

SANDBOX_BUILD_VERSION=v0.3.24-apparmor.1 # 示例，按自己的新发布版本替换
SANDBOX_BUILD_REGISTRY=registry.i.huaxisy.com/library/ai-infra
SANDBOX_BUILD_PLATFORMS=linux/amd64,linux/arm64
```

只处理 AppArmor 时，执行以上准备后直接进入“本次 AppArmor 加载器”小节，不执行下面的完整镜像构建清单：

```bash
docker buildx build --builder sandbox-apparmor-build -f docker/Dockerfile --platform "$SANDBOX_BUILD_PLATFORMS" -t "$SANDBOX_BUILD_REGISTRY/sandbox-api:$SANDBOX_BUILD_VERSION" --push .
docker buildx build --builder sandbox-apparmor-build -f docker/images/redis-bootstrap/Dockerfile --platform "$SANDBOX_BUILD_PLATFORMS" -t "$SANDBOX_BUILD_REGISTRY/sandbox-redis-bootstrap:v0.1.0" --push .
docker buildx build --builder sandbox-apparmor-build -f docker/images/sandbox/Dockerfile --platform "$SANDBOX_BUILD_PLATFORMS" -t "$SANDBOX_BUILD_REGISTRY/sandbox-runtime:$SANDBOX_BUILD_VERSION" --push .
docker buildx build --builder sandbox-apparmor-build -f docker/images/gateway/Dockerfile --platform "$SANDBOX_BUILD_PLATFORMS" -t "$SANDBOX_BUILD_REGISTRY/sandbox-gateway:$SANDBOX_BUILD_VERSION" --push docker/images/gateway
docker buildx build --builder sandbox-apparmor-build -f docker/images/workspace-mounter/Dockerfile --platform "$SANDBOX_BUILD_PLATFORMS" -t "$SANDBOX_BUILD_REGISTRY/sandbox-fuse-mounter:$SANDBOX_BUILD_VERSION" --push .
docker buildx build --builder sandbox-apparmor-build -f docker/images/sandbox-fuse/Dockerfile --platform "$SANDBOX_BUILD_PLATFORMS" -t "$SANDBOX_BUILD_REGISTRY/sandbox-fuse-docker:$SANDBOX_BUILD_VERSION" --push .
```

`ds-ai-research` 当前节点为 arm64，镜像必须包含 `linux/arm64`。只部署这个架构时可将变量改为 `linux/arm64`；其它 amd64 集群需要 amd64 镜像。先核对 builder 支持所选架构，不在业务节点临时安装 privileged 模拟器。`--push` 直接推送目标架构清单和镜像，无需再执行 `docker push`；不能省略输出选项并假设构建结果已经可被集群拉取。多架构发布方式见 [Docker 官方说明](https://docs.docker.com/build/building/multi-platform/)。

Kubernetes FUSE 需要已有兼容的 API/runtime/mounter 镜像，Docker FUSE 还需要 `sandbox-fuse-docker`；按实际部署使用的镜像构建，不因只升级 API 就统一改所有 tag。你也可以沿用现有流程分别构建各架构，再用 `docker manifest` 合并同一个新版本 tag。

### 本次 Redis bootstrap 独立镜像

首次使用新版 Sentinel Chart 时必须构建并推送这个最小镜像。它与 API 独立发版，
初始 tag 为 `v0.1.0`；以后只在 `cmd/redis-bootstrap` 或其依赖行为改变时使用新 tag。
普通 API Go 修复、API 资源调整或 `image.tag` 更新不需要重建它。
若已经执行上面的全量多架构构建清单，则无需重复执行下面的构建命令；这里单列用于只发布
bootstrap 镜像的场景。

```bash
SANDBOX_BOOTSTRAP_VERSION=v0.1.0

docker buildx build \
  --builder sandbox-apparmor-build \
  --file docker/images/redis-bootstrap/Dockerfile \
  --platform "$SANDBOX_BUILD_PLATFORMS" \
  --tag "$SANDBOX_BUILD_REGISTRY/sandbox-redis-bootstrap:$SANDBOX_BOOTSTRAP_VERSION" \
  --push .

docker buildx imagetools inspect \
  "$SANDBOX_BUILD_REGISTRY/sandbox-redis-bootstrap:$SANDBOX_BOOTSTRAP_VERSION"
```

将实际 repository/tag 写入环境 values：

```yaml
redis:
  sentinel:
    bootstrapImage:
      repository: registry.i.huaxisy.com/library/ai-infra/sandbox-redis-bootstrap
      tag: v0.1.0
      pullPolicy: IfNotPresent
```

首次迁移预期 Redis StatefulSet 逐成员滚动一次。完成后保存 StatefulSet
currentRevision、三个 Pod UID 和 restartCount，再执行一次只改变 API tag 的 upgrade；
这些 Redis 值必须完全不变，只有 API Deployment 逐副本替换。

### 本次 AppArmor 加载器

需要新增加载器镜像。API 必须包含加载器启动门禁和私有 enforce 检查：当前 API 已从包含这些实现的新版代码构建时可复用；否则同时重建 API。mounter/probe/runtime/gateway/Redis 镜像不需要因本功能重建。

在同一个 shell 中使用上面的 registry/platform 变量及 builder，先构建加载器：

```bash
SANDBOX_APPARMOR_TAG=v0.1.0 # 首次版本示例，后续变更使用新 tag

docker buildx build \
  --builder sandbox-apparmor-build \
  --platform "$SANDBOX_BUILD_PLATFORMS" \
  --file docker/images/apparmor-loader/Dockerfile \
  --tag "$SANDBOX_BUILD_REGISTRY/sandbox-apparmor-loader:$SANDBOX_APPARMOR_TAG" \
  --push .

docker buildx imagetools inspect \
  "$SANDBOX_BUILD_REGISTRY/sandbox-apparmor-loader:$SANDBOX_APPARMOR_TAG"
```

若需重建 API，单独执行，不用执行上面的完整镜像清单：

```bash
SANDBOX_API_TAG=v0.3.24-apparmor.1 # 示例，按自己的新发布版本替换

docker buildx build \
  --builder sandbox-apparmor-build \
  --platform "$SANDBOX_BUILD_PLATFORMS" \
  --file docker/Dockerfile \
  --tag "$SANDBOX_BUILD_REGISTRY/sandbox-api:$SANDBOX_API_TAG" \
  --push .

docker buildx imagetools inspect \
  "$SANDBOX_BUILD_REGISTRY/sandbox-api:$SANDBOX_API_TAG"
```

双架构清单应包含 `linux/amd64` 和 `linux/arm64`；单架构应包含所选目标。YAML 不展开 shell 变量，构建完成后按第 3 节把实际 repository/tag 合并到 values。节点还须具备仓库拉取权限，构建机的 `docker login` 不会替它自动配置凭据。完整分步骤说明见 [AppArmor 加载器](apparmor-loader.md)。

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

AppArmor 加载器已启用时，再检查本 release 的 DaemonSet 和全部目标节点实例：

```bash
kubectl --context "$CTX" -n "$NS" get daemonset "$RELEASE-apparmor-loader"
kubectl --context "$CTX" -n "$NS" get pods \
  -l "app=sandbox-apparmor-loader,release=$RELEASE" -o wide
```

API 启动门禁只要求至少一个当前策略实例 Ready，不等于全部目标节点已覆盖；DaemonSet Ready 也不代替实际 mounter/s3fs profile 和越权拒绝验收。加载器关闭时没有 DaemonSet 属于正常行为。退出/升级/uninstall 不自动卸载内核 profile，旧策略清理由节点管理员另行审计。

若加载器全部 Ready、API 仍报 `AppArmor loader startup gate incomplete`，检查 DS 模板与实际 Pod 的 `enableServiceLinks` 是否一致。旧版未在模板指定此字段，Kubernetes 1.33 给 Pod 默认补入 true，触发严格模板比较失败；修复后的 `templates/apparmor-loader.yaml` 显式指定 false。仅更新这一模板即可修复该差异，无需新增 values 项或重建 API/加载器/mounter 镜像。保留自己的镜像 tag、Redis 身份 Secret/PVC 和其它配置，重新执行 upgrade；内置 Sentinel 不使用 `--atomic` 或 rollback。升级后仍须确认 API 可用并完成普通/FUSE API 测试，不能用 DS Ready 代替，见 [门禁修复记录](../testing/2026-09-14-apparmor-loader-startup-gate-results.md)。

```bash
# 更新后的模板及当前模板 Pod 应均输出 false；空值表示模板未显式配置。
kubectl --context "$CTX" -n "$NS" get daemonset "$RELEASE-apparmor-loader" \
  -o jsonpath='{.spec.template.spec.enableServiceLinks}{"\n"}'
kubectl --context "$CTX" -n "$NS" get pods \
  -l "app=sandbox-apparmor-loader,release=$RELEASE" \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.enableServiceLinks}{"\n"}{end}'
```
