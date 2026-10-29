# Helm 正式环境部署手册（0.4.0）

本文对应当前代码整理的 Chart `0.4.0`，主线为 **Kubernetes + Chart 内置三成员 etcd（PVC 持久化）+ FUSE + 外部对象存储**。项目镜像统一规划为 `v0.4.0`，**均待从当前代码构建、推送和验收，本文不是发布成功记录**。没有执行生产部署。不要仅因 tag 已填写就认为仓库已有该镜像。

## 1. 发布物与镜像版本

完整使用 `deploy/helm/sandbox/`，包含 `Chart.yaml`、`templates/`、`files/`、`values.yaml` 和 `values.schema.json`。Chart 版本为 `0.4.0`，`appVersion` 为 `0.4.0`；实际镜像由以下 values 控制，`appVersion` 不会自动替换镜像 tag。

仓库前缀：`registry.i.huaxisy.com/library/ai-infra/`。

| 组件 | repository 与精确 tag | values 字段 | 本主线用途 |
| --- | --- | --- | --- |
| API / drain / resume / rollback guard | `sandbox-api:v0.4.0` | `image.repository` + `image.tag` | 必需；升级前 drain 可能使用已安装的旧 API 镜像 |
| 普通与 FUSE 沙盒 runtime | `sandbox-runtime:v0.4.0` | `config.images.sandbox` | 必需，包含当前文件 v2 helper |
| FUSE mounter | `sandbox-fuse-mounter:v0.4.0` | `config.workspace.fuseImages.mounter` | 必需 |
| AppArmor loader | `sandbox-apparmor-loader:v0.4.0` | `apparmorLoader.image` | 示例启用；手工预加载受限 profile 时可关闭 |
| gateway | `sandbox-gateway:v0.4.0` | `config.images.gateway` | 固定版本；主要供 Docker runtime 使用 |
| Docker FUSE runtime | `sandbox-fuse-docker:v0.4.0` | `config.workspace.fuseImages.docker` | 固定版本；Kubernetes 不启动该镜像 |
| Redis bootstrap | `sandbox-redis-bootstrap:v0.4.0` | `redis.sentinel.bootstrapImage` | 仅内置 Sentinel 使用 |
| 内置 etcd | `quay.io/coreos/etcd:v3.6.14`（完整引用） | `etcd.image` | 默认三成员集群，每成员独立 PVC |
| Redis / Sentinel | `redis:7.4.2`（完整引用，不使用上述前缀） | `redis.image` | etcd 主线不部署 |

etcd 默认由本 Chart 部署，固定使用官方 `quay.io/coreos/etcd:v3.6.14`，不需要构建项目镜像；对象存储由平台独立维护。若生产节点无法访问 Quay，先将同版本、多架构镜像镜像到内部仓库，并覆盖 `etcd.image.repository`，保持精确 tag `v3.6.14`。Redis 镜像如需内部仓库，保持 `7.4.2` tag 并覆盖 `redis.image.repository`。

本轮统一 tag 是一套待发布基线；以后 API、runtime、mounter、loader、bootstrap 可分别发版。发布过的 tag 不覆盖、不重复打包不同内容；保存 manifest digest 用于核对，不要求把部署引用改成纯 digest。禁止使用 `latest`、`7-alpine`、空 tag 或 `REPLACE_WITH_...`。离线渲染不会验证镜像是否存在。

## 2. Chart 资源与配置入口

| 资源 | 管理范围 |
| --- | --- |
| Deployment / Service / API ServiceAccount | API，多副本启动、健康检查及服务发现；Service 默认 ClusterIP |
| HPA / PDB | 示例最少 3 个 API，最多 10 个；PDB 至少保留 2 个可用副本 |
| RBAC / runtime network inventory | API 管理受管 Pod、网络策略及只读集群网络发现所需权限 |
| etcd StatefulSet / Headless Service / ClusterIP / PDB / NetworkPolicy | 默认三成员，独立 PVC、稳定成员 DNS、至少保留两成员的 PDB；客户端与 peer 入口受限 |
| etcd ConfigMap / backend fingerprint | etcd 专用配置与运行时契约；etcd 模式不注入 Redis 配置 |
| NetworkPolicy | 受管 runtime 默认拒绝入站和出站；具体存储通路由 API 创建 |
| AppArmor ConfigMap / DaemonSet / 专用 ServiceAccount | 在选定节点加载随 Chart 发布的策略，生成摘要命名的 Localhost profile |
| Helm Hook Jobs | pre-upgrade drain、post-install/upgrade/rollback resume、pre-rollback guard、pre-delete drain |
| Redis StatefulSet / Sentinel Jobs 与 Secret | 仅 Redis 模式渲染；不属于 etcd 主线 |

| 文件 | 使用方式 |
| --- | --- |
| `values.yaml` | 通用默认值及固定镜像清单；默认 etcd 三成员和 PVC；生产环境再叠加生产 overlay |
| `values-production.yaml` | 全新生产部署的内置 etcd + FUSE 覆盖层，启用生产校验和 loader |
| `values-etcd.yaml` | 最小内置 etcd 接线示例；生产主线不需要再叠加它 |
| `values-external-etcd.yaml` | 可选外部 etcd 覆盖层，关闭内置集群后显式填写 endpoints |
| `values-production-redis.yaml` | 原有外部 Sentinel 生产示例，显式使用 Redis backend |
| `values-builtin-sentinel.yaml` | 内置 Sentinel 示例；认证、PVC 和升级要求见专门手册 |
| Chart 外的 `values-prod.yaml` | 环境地址、Secret 名称、节点选择器及业务资源配置；最后加载 |

新主线不迁移 Redis 数据、不接管旧沙盒。已有 Redis 安装继续使用自身 Redis 配置，不能直接换成 `values-production.yaml`。新建独立 namespace、etcd 集群/PVC、prefix 和存储路径，验收后再切流。

## 3. 上线前置条件

- 内置 etcd 固定三个成员，至少需要三个可调度的 Linux 节点；默认强制按 hostname 分散。准备可用的 StorageClass，每成员默认 `10Gi`（合计三块卷），支持 `ReadWriteOnce` 和 Pod 的 fsGroup 写权限。`storageClass: ""` 使用默认类，`"-"` 表示显式不指定类、需自行提供匹配 PV。填写本 release 专属 prefix，并落实快照/恢复。内置客户端及 peer 使用内部 HTTP，无用户名/密码和 TLS；NetworkPolicy 只放行同 release 的 etcd 成员、API 与 drain Pod，命名空间写权限及 CNI 策略执行也是安全边界，不对公网暴露。生产安全校验禁止关闭 `networkPolicy.enabled` 或强制反亲和；仍须由平台确认实际 CNI 执行这些策略。需要 mTLS 的环境可采用下文外部 etcd 入口。
- 对象存储 bucket 已创建，TLS/网络可用，凭据具备所需路径读写权限。每个 cell 使用独立 bucket 或 `subPath`，AK/SK 存在 Secret 中。
- 目标 Linux 节点支持 `/dev/fuse`、mount propagation 和实际 CRI/AppArmor 限制。先完成真实 FUSE 挂载、flush、卸载及越权拒绝验收，再启用生产配置。loader Ready 不代表 CRI/AppArmor 实际可用。
- 选定节点有足够 CPU、内存与临时磁盘；HPA 需要可用的资源指标 API。至少把 API 的 requests/limits、池大小与节点容量一起核算。`fusePool.maxSize=20` 仅限制准备池，不能用它限制业务存活数；历史 20 个 FUSE 功能测试不能证明更大容量。
- CNI 实际执行 NetworkPolicy。按真实 CNI 设置 `standard` 或 `cilium`；`auto` 仅按 API 可见性发现。外部 IPAM 无法完整发现时明确填写完整的 `podCIDRs` 和 `serviceCIDRs`，不照抄测试网段。特殊 DNS 准入配置见 [说明](kubernetes-dns-admission.md)。
- 集群准入允许经过验收的 FUSE/loader 权限和 hostPath。API Service 由平台入口提供 TLS、访问控制及流量切换；当前 API ingress 策略为 `ingress: [{}]`，不限制调用来源，业务访问还需 API key。

## 4. 从当前代码构建并发布镜像

以下命令由发布人员执行。etcd 官方镜像不在项目构建清单中，部署前另外验证 `quay.io/coreos/etcd:v3.6.14` 的目标架构和节点可拉取性。先核对源码提交及工作区，不从尚未包含本轮修改的旧提交构建；构建全部镜像时固定同一份源码。发布前先在镜像仓库确认目标 repository 的 `v0.4.0` 尚未占用；若已占用，停止推送，选择新的精确版本并同步 Chart/values/文档，不能覆盖旧内容。这里只列最终部署镜像的 tag，Dockerfile 的构建基础镜像不会作为 Helm 工作负载部署。

```bash
cd "$(git rev-parse --show-toplevel)"
git status --short
git rev-parse HEAD

docker login registry.i.huaxisy.com
# 复用已有 builder；没有时只创建一次。
docker buildx inspect sandbox-release >/dev/null 2>&1 || \
  docker buildx create --name sandbox-release --driver docker-container
docker buildx inspect sandbox-release --bootstrap

SANDBOX_REGISTRY=registry.i.huaxisy.com/library/ai-infra
SANDBOX_VERSION=v0.4.0
SANDBOX_PLATFORMS=linux/amd64,linux/arm64

docker buildx build --builder sandbox-release --platform "$SANDBOX_PLATFORMS" \
  -f docker/Dockerfile -t "$SANDBOX_REGISTRY/sandbox-api:$SANDBOX_VERSION" --push .
docker buildx build --builder sandbox-release --platform "$SANDBOX_PLATFORMS" \
  -f docker/images/sandbox/Dockerfile -t "$SANDBOX_REGISTRY/sandbox-runtime:$SANDBOX_VERSION" --push .
docker buildx build --builder sandbox-release --platform "$SANDBOX_PLATFORMS" \
  -f docker/images/workspace-mounter/Dockerfile -t "$SANDBOX_REGISTRY/sandbox-fuse-mounter:$SANDBOX_VERSION" --push .
docker buildx build --builder sandbox-release --platform "$SANDBOX_PLATFORMS" \
  -f docker/images/apparmor-loader/Dockerfile -t "$SANDBOX_REGISTRY/sandbox-apparmor-loader:$SANDBOX_VERSION" --push .
```

本主线需构建以上四个项目镜像，另需可拉取的 etcd 官方镜像。准备可选 Docker/内置 Sentinel 发布物时才执行：

```bash
docker buildx build --builder sandbox-release --platform "$SANDBOX_PLATFORMS" \
  -f docker/images/gateway/Dockerfile -t "$SANDBOX_REGISTRY/sandbox-gateway:$SANDBOX_VERSION" --push docker/images/gateway
docker buildx build --builder sandbox-release --platform "$SANDBOX_PLATFORMS" \
  -f docker/images/sandbox-fuse/Dockerfile -t "$SANDBOX_REGISTRY/sandbox-fuse-docker:$SANDBOX_VERSION" --push .
docker buildx build --builder sandbox-release --platform "$SANDBOX_PLATFORMS" \
  -f docker/images/redis-bootstrap/Dockerfile -t "$SANDBOX_REGISTRY/sandbox-redis-bootstrap:$SANDBOX_VERSION" --push .
```

核查实际推送 tag 的架构清单和摘要，并保存发布记录。需要单架构时明确修改 `SANDBOX_PLATFORMS`。构建机登录仓库不等于节点具备拉取权限。

```bash
for component in sandbox-api sandbox-runtime sandbox-fuse-mounter sandbox-apparmor-loader; do
  docker buildx imagetools inspect "$SANDBOX_REGISTRY/$component:$SANDBOX_VERSION"
done

# 在每种目标架构可运行容器的验收机分别执行，检查最终仓库 tag。
FUSE_IMAGE="$SANDBOX_REGISTRY/sandbox-fuse-mounter:$SANDBOX_VERSION" \
SANDBOX_IMAGE="$SANDBOX_REGISTRY/sandbox-runtime:$SANDBOX_VERSION" \
  bash scripts/verify-fuse-image.sh kubernetes release-check
```

该脚本校验镜像内 mounter/probe/s3fs 包契约，不代替真实节点 mount/flush/unmount 与 AppArmor 验收。当前文件 v2 功能需要当前源码构建的 API 和 runtime；只换 API、沿用不含 `sandbox-file-op` 的旧 runtime 不构成当前版本发布。

## 5. 准备 namespace、Secret 和环境 values

以下示例使用同一个 release/runtime namespace，避免跨 namespace 的 Secret 和 ServiceAccount 差异。替换 context，先确认 namespace 未被其他安装占用。

AK/SK、API key 和 Redis 密码均可直接写在 Chart 外的环境 values，**不要求手工创建 Secret**。下文的创建命令和完整环境示例使用“引用已有 Secret”方式；选择直接填写时，跳过对象存储/API key Secret 的手工创建，将环境示例中相应字段替换为下面的配置。不要在同一文件里重复顶层 `config`：

```yaml
config:
  storage:
    filesystem:
      credentialsSecretName: '' # 清空生产 overlay 预填的引用，避免与 inline 冲突。
      accessKey: 'REPLACE_WITH_ACCESS_KEY'
      secretKey: 'REPLACE_WITH_SECRET_KEY'
  security:
    apiKeySecretName: '' # 清空预填引用，由 Helm 自动创建 <release>-api。
    apiKey: 'REPLACE_WITH_YOUR_API_KEY'
```

AK/SK 会直接注入 API 环境；API key 和 Redis 密码由 Chart 自动创建认证 Secret。明文值会进入环境文件与 Helm release 历史，环境文件按凭据文件保管。单行字符串不用 `|` 多行块，YAML 文件正常末尾换行不会给单行值补换行。仓库认证和外部 etcd TLS 仍按其 Secret 引用接口处理；默认内置 etcd 不需要认证 Secret。

两种方式及各 Secret 的精确键名、**无末尾换行写入**、创建后回读逐字节比较和修复步骤见 [凭据配置与 Secret 手册](helm-secrets.md)。选择引用已有 Secret 时，先准备原始文件并通过本地检查，再执行下面的创建命令；AK/SK、API key、Redis 密码拒绝 CR/LF，PEM 证书保留正常换行。

```bash
CTX=your-production-context
NS=sandbox-prod
RELEASE=sandbox
CHART=./deploy/helm/sandbox
VALUES=/opt/sandbox/env/values-prod.yaml

kubectl --context "$CTX" get nodes -o wide
kubectl --context "$CTX" get namespace "$NS" || \
  kubectl --context "$CTX" create namespace "$NS"
kubectl --context "$CTX" -n "$NS" get deploy,sts,pod,pvc

# 下面仅用于“引用已有 Secret”；AK/SK、API key 已直接写 values 时跳过。
# 先按 Secret 手册交互写入并校验文件；不要用 echo 或编辑器自动补行。
# 文件只含原始密钥字节，权限 0600；--from-file 不需要预先 base64 编码。
kubectl --context "$CTX" -n "$NS" create secret generic sandbox-storage-credentials \
  --from-file=access-key=/secure/sandbox/access-key \
  --from-file=secret-key=/secure/sandbox/secret-key
kubectl --context "$CTX" -n "$NS" create secret generic sandbox-api-key \
  --from-file=api-key=/secure/sandbox/api-key
```

采用手工 Secret 方式时，创建后按 Secret 手册第 7 节回读比较，只有内容一致且无意外换行才继续。已有 Secret 先比较后复用，不重复 create、不直接删除。私有仓库可从专用 Docker auth 文件创建 Secret（须包含实际 auths，不能只有本机 credHelpers/credsStore）：

```bash
kubectl --context "$CTX" -n "$NS" create secret generic sandbox-registry \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson=/secure/sandbox/registry-config.json

# 受管 runtime 使用 default ServiceAccount；以下 patch 仅用于原列表为空。
# 已有 imagePullSecrets 时先合并保留原列表，详见 Secret 手册。
kubectl --context "$CTX" -n "$NS" patch serviceaccount default \
  --type=merge -p '{"imagePullSecrets":[{"name":"sandbox-registry"}]}'
```

`image.pullSecretName` 用于 API ServiceAccount、使用它的 Hook 及内置 etcd Pod；不会自动配置受管 runtime 或 loader 的专用 ServiceAccount。loader 私有镜像还需平台已配置的节点仓库凭据，或经平台验证的 ServiceAccount 凭据注入机制。安装前确认四类项目镜像及 etcd 镜像均可拉取。内置 etcd Pod 也使用 `image.pullSecretName`；外部 etcd 的 mTLS 配置另见后文。

下面的完整环境示例采用 Secret 引用；直接填写密钥时，清空对应的 credentialsSecretName/apiKeySecretName，并合入上面的 inline 字段。把下列覆盖配置保存在 Chart 外的 `$VALUES`，替换所有环境地址及节点标签。项目镜像直接继承本 Chart 的 `v0.4.0` 固定清单；若仓库变化，同时覆盖所用组件的 repository。

```yaml
etcd:
  persistence:
    enabled: true
    storageClass: production-ssd # 替换为生产集群实际存在的 StorageClass。
    size: 10Gi # 每成员一块卷；安装后不要直接修改 claim template。
image:
  pullSecretName: sandbox-registry # 不需要拉取 Secret 时设为空字符串。
config:
  runtime:
    kubernetes:
      nodeSelector:
        sandbox-fuse-ready: "true" # 管理员已标记且完成真实验收的一组节点。
      networkPolicyProvider: standard # 按现场 CNI 选择 standard/cilium。
  storage:
    state:
      etcd:
        endpoints: [] # 内置模式自动生成三个成员端点，无需填写地址。
        prefix: /sandbox/prod-cell-a/
        existingSecret: "" # 内置 HTTP 模式不填写客户端 TLS Secret。
    filesystem:
      preset: minio
      endpoint: minio.prod.internal # 主机名，不带 scheme。
      bucket: sandbox-production
      region: us-east-1
      subPath: prod-cell-a
      credentialsSecretName: sandbox-storage-credentials
      storageIdentity: prod-minio-cell-a
      credentialGeneration: rotation-1
```

OBS 使用 `huawei-obs-public` 或 `huawei-obs-private` preset，填写实际 endpoint/region。对象存储若为同 namespace 的 Pod，按实际 label/端口填写 `networkPolicy.fuseStoragePodEgress`，见 [Workspace 运维说明](workspace-fuse.md)。不要为存储故障放开沙盒的全部出站访问。

已由节点管理员预加载受限 profile 时，可在环境文件中设置 `apparmorLoader.enabled: false`，并填写 `config.workspace.lsmProfile`；仍须保持 `allowMissingLSMForKind: false`。启用 loader 且集群存在 globalDefault PriorityClass 时，填写平台确认的 `apparmorLoader.priorityClassName`。

## 6. 渲染、打包与首次安装

先确认镜像已发布、对象存储端点/节点标签/StorageClass 已替换、三块 PVC 能绑定、etcd prefix 未占用、Secret 和架构正确。`helm lint` 不连接 etcd、不检查 prefix 是否为空，也不验证镜像可拉取。

```bash
helm lint "$CHART" -f "$CHART/values-production.yaml" -f "$VALUES"
helm template "$RELEASE" "$CHART" --namespace "$NS" \
  -f "$CHART/values-production.yaml" -f "$VALUES" >/dev/null

# 可分发一个完整且带版本的 Chart 包；生产 overlay 也包含在包内。
mkdir -p dist
helm package "$CHART" --destination dist

# 首次安装使用 install，明确区分与已有 release 升级。
helm --kube-context "$CTX" install "$RELEASE" "$CHART" \
  --namespace "$NS" \
  -f "$CHART/values-production.yaml" -f "$VALUES" \
  --wait --wait-for-jobs --timeout 20m
```

也可把 `$CHART` 换成分发的 `sandbox-0.4.0.tgz`，生产 overlay 另存为发布附件后用 `-f` 引用；所有配置必须来自同一发布物。不要仅替换 `templates/`，不要用 `--reuse-values` 带入旧安装状态。首次生产发布保留失败现场，不使用自动回滚或 `--no-hooks` 绕过清理门禁。

## 7. 验收与切流

```bash
helm --kube-context "$CTX" -n "$NS" status "$RELEASE"
kubectl --context "$CTX" -n "$NS" rollout status statefulset/"$RELEASE-etcd" --timeout=10m
kubectl --context "$CTX" -n "$NS" get pvc -l app=sandbox-etcd
kubectl --context "$CTX" -n "$NS" rollout status deployment/"$RELEASE-api" --timeout=10m
kubectl --context "$CTX" -n "$NS" rollout status daemonset/"$RELEASE-apparmor-loader" --timeout=5m
kubectl --context "$CTX" -n "$NS" get pod,hpa,pdb,job -o wide
kubectl --context "$CTX" -n "$NS" logs deployment/"$RELEASE-api" --tail=200
kubectl --context "$CTX" -n "$NS" get pods \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .status.containerStatuses[*]}{.image}{" "}{.imageID}{" "}{end}{"\n"}{end}'
```

外部 etcd 安装跳过本 Chart 的 StatefulSet/PVC 检查；关闭 loader 的安装跳过 DaemonSet 命令。确认 API 的 `/ready` 返回 200；用带 API key 的业务请求完成以下验收后再切流：

1. 创建无工作区普通沙盒和独立路径的 FUSE 沙盒，执行命令；FUSE 响应明确为 `workspace_mount_mode=fuse`。
2. 通过当前公开文件 API 读写、上传、下载；核对 `/workspace` 与对象存储结果，执行 flush/sync 后再次读回。当前 API 路由见 [项目说明](../../README.md)。
3. 公开 DELETE 后查询返回 404；另建短 TTL 实例确认自动回收、准备池补充及对应 Pod/策略清理。测试使用独立对象路径，只清理自己的测试数据。
4. 每个目标节点验证实际 mounter/s3fs profile 与越权拒绝；确认 etcd 多数派、API 重启后的存活实例 GET/exec 和到期回收。

需要临时访问 ClusterIP 时，在独立终端执行 `kubectl --context "$CTX" -n "$NS" port-forward svc/"$RELEASE-api" 18080:8080`。准备池中 FUSE Pod 可显示 `1/2 Running`，须结合 `sandbox.pool.state=prepared`、API 池状态及领取后读写判断。Helm deployed、Pod Ready 或 loader Ready 单独均不构成业务验收通过。

## 8. 升级、回退和卸载

升级前保存旧 Chart、环境 values、已发布镜像及摘要、Helm revision、etcd 快照与活跃业务清单；备份文件按敏感数据管理。不要覆盖旧镜像 tag，pre-upgrade drain 需要旧 API 仍可拉取。已有 Redis 安装不执行下面的 etcd 升级命令。

```bash
# 对已部署本主线的 release：环境文件保留当前 endpoint、prefix、存储身份和 Secret。
helm --kube-context "$CTX" upgrade "$RELEASE" "$CHART" \
  --namespace "$NS" --reset-values \
  -f "$CHART/values-production.yaml" -f "$VALUES" \
  --wait --wait-for-jobs --timeout 20m
```

API-only 的兼容更新通常不改变 backend fingerprint；runtime tag 本身也不在该 fingerprint 中。不能因此假定任意 runtime/API 组合兼容：不兼容控制协议须同步更新 Chart 的契约。mounter/Docker FUSE 镜像、存储 preset/身份/路径/凭据代次、LSM、节点选择器、DNS 显式配置和控制契约等变化可能触发完整 release drain，终止业务沙盒。提前停新请求并安排维护窗口；HPA 与 resume 由 Hook 协调，不手工缩放来绕过它。

内置 etcd 的 release/namespace/clusterDomain 是持久成员身份，安装后保持不变；Chart 会核对现有 StatefulSet 和保留 PVC 的身份，拒绝不一致的数据复用。禁止直接扩缩成员数；更换故障成员须按 etcd 成员替换流程进行。etcd endpoint/TLS Secret 的改变也需要单独评估状态身份与可达性，不能当作迁移工具；不要更换到空集群或不同 prefix 来接管原 release。更换 Secret 内容不会自动等同于一次完整的配置变更发布；对象存储轮换同时递增 `credentialGeneration`。

不执行跨 backend/fingerprint/控制契约的 `helm rollback`；旧 revision 的 Hook 会按保存的契约校验。需要返回旧配置时，经过受控排空后以新的 `helm upgrade` 部署兼容组合。同契约回退仍要先核对 revision 与现场；内置 Sentinel 禁止 rollback/`--atomic`，见 [专门手册](built-in-redis-sentinel.md)。默认镜像版本本轮整体更新，旧安装必须审查组件差异；内置 Sentinel 的 bootstrap tag 更新会触发 Redis 滚动，不能当作 API-only 升级。

卸载先停新请求并通过公开 API 排空业务，确认状态和受管资源，再运行：

```bash
helm --kube-context "$CTX" uninstall "$RELEASE" --namespace "$NS" --wait --timeout 20m
kubectl --context "$CTX" -n "$NS" get pod,job,networkpolicy,pvc
```

pre-delete Hook 完成 release drain 后才移除 Chart 资源。失败时检查 Hook 日志与 etcd/存储/节点状态，通过正常恢复流程重试；不强删 finalizer、不裸删 etcd owner、不用 `--no-hooks`。卸载会删除内置 etcd 的 StatefulSet/Service/策略，但默认保留三块 PVC 和其中的数据，不自动删除数据或重建成员；重新安装保留相同 release/namespace/clusterDomain 和 StorageClass/容量，复用原卷。对象存储、外部 etcd、已有 Secret/PVC 不因 Chart 卸载而自动销毁；loader 退出不会卸载内核 profile，由节点管理员另行审计。

旧部署的控制协议过渡与排空细节见 [升级参考](helm-deployment-upgrade.md)，etcd 恢复及历史有界测试证据见 [记录](../superpowers/reports/2026-10-24-etcd-capacity-and-release.md)。历史测试记录描述当时组合，不替代 `v0.4.0` 的新发布验收。


## 9. 内置 etcd 运维与外部模式

内置模式自动生成三个 `...:2379` 客户端端点；peer 使用稳定 DNS 的 `2380`。Headless Service 提前发布未就绪地址，StatefulSet 并行创建三成员，避免等待首成员 Ready 造成首次选主死锁。启动/存活探针使用本地 `endpoint status`，就绪探针使用需要 quorum 的 `endpoint health`；无 quorum 时不会仅因就绪失败触发连锁存活重启。静态启动参数沿用 etcd [集群引导约定](https://etcd.io/docs/v3.6/op-guide/clustering/)，数据目录已有成员信息时由 etcd 正常恢复，不使用 `--force-new-cluster`。

检查三个端点及成员信息（在 etcd Pod 内执行，不依赖镜像中不存在的 shell）：

```bash
kubectl --context "$CTX" -n "$NS" exec "$RELEASE-etcd-0" -- \
  /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 endpoint health --cluster
kubectl --context "$CTX" -n "$NS" exec "$RELEASE-etcd-0" -- \
  /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 endpoint status --cluster -w table
kubectl --context "$CTX" -n "$NS" exec "$RELEASE-etcd-0" -- \
  /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 member list -w table
```

上线前核对三个 PVC 为 Bound、成员分布在不同节点、成员 ID 各异且 cluster ID 一致。在隔离验收环境中逐成员重建 Pod，确认原 PVC 和成员身份复用、数据可读写；每次等待三成员全部健康后再处理下一成员。API tag 更新不应修改 etcd Pod 模板/PVC，不应引起 etcd 无关滚动。

默认 `etcd.autoCompactionRetention: 1h` 自动压缩一小时前的历史 revision；`etcd.quotaBackendBytes: 2147483648` 明确限制数据库为 2Gi，必须小于单成员 PVC 容量。10Gi PVC 还需为 WAL、碎片和维护预留空间，不能视为 10Gi 数据库可用配额。按实际状态写入量调整保留窗口和 quota，配额最大 8Gi；调大 quota 前先确认卷容量和节点资源。

自动 compaction 只释放数据库内部历史空间，不会自动缩小文件。平台应定期观察 `endpoint status` 的 DB SIZE/DB SIZE IN USE、配额、PVC 使用率、磁盘 fsync 延迟、leader 变更及 quorum 健康，并提前告警、在维护窗口逐成员 defrag。先通过上面的 status/member list 识别 leader，选择一个 follower；完成后确认整个集群健康，再处理下一个 follower，最后才处理 leader。不能并发整理三个成员。示例中的成员名须按当时身份选择：

```bash
ETCD_MEMBER="$RELEASE-etcd-1" # 示例：先核实它是本 release 的 follower。
kubectl --context "$CTX" -n "$NS" exec "$ETCD_MEMBER" -- \
  /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 --command-timeout=60s defrag
kubectl --context "$CTX" -n "$NS" exec "$ETCD_MEMBER" -- \
  /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 endpoint health --cluster
kubectl --context "$CTX" -n "$NS" exec "$ETCD_MEMBER" -- \
  /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 alarm list
```

出现 `NOSPACE` 时先停止新请求，核查是 backend quota 还是 PVC 已满；备份并按 [etcd 维护手册](https://etcd.io/docs/v3.6/op-guide/maintenance/) 完成必要的历史压缩和逐成员 defrag，确认实际空间足够后才解除告警。不得用删除 owner/租约或删 PVC 的方式腾容量。需要 Prometheus 抓取时，按平台监控身份添加精确网络放行，不放开默认 etcd 客户端入口。

PVC 不能代替快照。使用相同版本的 `etcdctl` / `etcdutl` 在管理员机器备份，快照保存到集群外的受保护存储。先在一个终端建立仅本机可访问的转发：

```bash
kubectl --context "$CTX" -n "$NS" port-forward svc/"$RELEASE-etcd" 12379:2379
```

在第二个终端执行，并将快照纳入既有备份保留和恢复演练：

```bash
umask 077
SANDBOX_ETCD_SNAPSHOT="/secure/backups/sandbox-etcd-$(date +%Y%m%d-%H%M%S).db"
etcdctl --endpoints=http://127.0.0.1:12379 snapshot save "$SANDBOX_ETCD_SNAPSHOT"
etcdutl snapshot status "$SANDBOX_ETCD_SNAPSHOT" -w table
```

少数成员故障优先恢复原卷/成员，避免丢失 quorum；不要删除 PVC 来消除启动错误。整体快照恢复前先停止业务写入和 API，关闭三个 etcd 成员；对三块原 PVC 使用同一快照、完整固定 peer 列表与一致的恢复 token，在暂存目录验证后再替换 data 目录，保留原目录。恢复完核对三成员身份、revision 与业务主链。按 etcd [恢复手册](https://etcd.io/docs/v3.6/op-guide/recovery/) 制定具体流程，不由 Helm install/upgrade 自动清空或恢复数据。PVC 扩容须先确认 StorageClass 支持，再对原 PVC 逐一受控扩容；StatefulSet 的 claim template 不能通过普通 upgrade 任意修改。

外部 etcd 是可选入口。全新安装时，先加载生产 overlay，再加载 `values-external-etcd.yaml`，最后加载环境文件；环境文件中的 endpoints 必须改为实际外部地址，`etcd.enabled: false`，mTLS 时设置含 `ca.crt`、`client.crt`、`client.key` 的 Secret。不要把上文内置模式的 `endpoints: []` 原样带入该环境文件。

```bash
helm lint "$CHART" -f "$CHART/values-production.yaml" \
  -f "$CHART/values-external-etcd.yaml" -f "$VALUES"
```

安装/升级命令使用相同的三个 `-f` 顺序。外部模式不渲染 etcd StatefulSet、PVC、Service、PDB 或入站策略；三成员、存储和备份由平台提供。切换内置/外部不会迁移数据；已有 release 不通过改开关直接接管另一个集群。
