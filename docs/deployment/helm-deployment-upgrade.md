# Helm 部署、升级与镜像发布 Runbook

配套资料：

- [Workspace 存储部署与运维手册](workspace-fuse.md)
- [Docker Compose 部署与升级 Runbook](docker-compose-deployment-upgrade.md)
- [Workspace 容器内 FUSE 直接挂载设计](../superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md)

本文面向负责 Kubernetes 发布的运维人员，说明如何升级已有 release、安装全新 release，以及需要构建和发布哪些镜像。本文只讨论 Helm/Kubernetes；Docker 环境必须遵循独立的 Docker Compose 部署与升级 Runbook。

## 1. 发布合同

一个 release 只能选择一个对象存储 preset：

| preset | provider | profile |
|---|---|---|
| `minio` | MinIO/S3 | `minio-sigv4-path-style-v1` |
| `huawei-obs-public` | 华为公有云 OBS | `huawei-obs-public-v1` |
| `huawei-obs-private` | 2023 私有云 OBS | `huawei-obs-private-2023-v1` |

同一个 release 可以同时启用 sync 和 FUSE。默认推荐：

```yaml
config:
  workspace:
    defaultMountMode: sync
    enabledMountModes: [sync, fuse]
```

MinIO、公有云 OBS、私有云 OBS 在同一 CPU 架构上共用 Kubernetes mounter 镜像和 Docker FUSE 镜像，不按后端重复构建。请求只能选择 `workspace_mount_mode` 和 `workspace_path`，不能覆盖 release 的 endpoint、bucket、profile、Secret 或网络出口。

始终保留现有用户网络规则：允许公网时仍拒绝内网，内网服务必须通过白名单开放。FUSE system egress 只批准当前对象存储的精确 endpoint 和端口，不能通过修改默认拒绝策略解决连通性问题。

## 2. 发布前准备

### 2.1 Kubernetes 条件

- Kubernetes 最低版本 1.29，生产推荐 1.33 或更高版本。
- 节点提供 `/dev/fuse`。
- 生产节点配置受约束的 AppArmor 或 SELinux profile。
- `cilium-fqdn` 模式要求集群已安装并启用 Cilium FQDN policy。
- 集群能够拉取 values 中全部 digest-pinned 镜像。
- 生产 Redis 必须使用持久化存储或外部高可用 Redis。

`config.workspace.allowMissingLSMForKind=true` 只允许在 kind 或明确的验收集群使用。没有专用 LSM 的集群不能作为生产环境。

### 2.2 选择 values

仓库提供三份不含凭据的验收 overlay：

- `testdata/values-fuse-minio.yaml`
- `testdata/values-fuse-obs-private.yaml`
- `testdata/values-fuse-obs-public.yaml`

一次 Helm 命令只能传入其中一份后端 overlay。它们包含单 API 副本、关闭 Redis 持久化等验收配置，不能原样作为生产 values。生产 overlay 应基于对应文件维护，并至少调整：

- API 副本和 HPA；
- Redis 持久化或外部 Redis；
- 生产 LSM profile；
- 经过发布门禁的镜像 digest；
- 生产 Secret 名称和非敏感的 `credentialGeneration`；
- 当前 endpoint 对应的精确 DNS/FQDN/CIDR 和端口。

不要使用 `--reuse-values` 升级。使用完整的新 overlay 和 `--reset-values`，避免旧字段或已经废弃的 provider map 残留。

### 2.3 创建 Secret

凭据必须通过文件创建，不能写进 values、命令行参数、镜像或日志。控制面 namespace 与 runtime namespace 不同时，需要在两个 namespace 分别创建同内容 Secret。

```bash
kubectl --context <context> -n <control-namespace> create secret generic <control-secret> \
  --from-file=accessKey=/secure/path/accessKey \
  --from-file=secretKey=/secure/path/secretKey

kubectl --context <context> -n <runtime-namespace> create secret generic <runtime-secret> \
  --from-file=accessKey=/secure/path/accessKey \
  --from-file=secretKey=/secure/path/secretKey

kubectl --context <context> -n <control-namespace> create secret generic <api-key-secret> \
  --from-file=api-key=/secure/path/sandbox-api-key
```

控制面和 runtime 使用同一 namespace、同一 Secret 名称时，对象存储 Secret 只创建一次。私有 CA 通过同一 Secret 的附加 key 提供，并在 `config.storage.filesystem.caSecretKey` 中填写 key 名。

## 3. 升级已有 release

### 3.1 服务器上已有 Chart 目录时，先处理文件

不要只覆盖 `templates/` 并保留旧 Chart 内的 `values.yaml`。`Chart.yaml`、`templates/`、`values.yaml` 和 `values.schema.json` 是同一个 Chart 版本的完整合同，混用新模板和旧默认值可能导致字段缺失、旧字段残留，或者 backend fingerprint 计算错误。

Helm 安装后，服务器上的 Chart 目录不会被运行中的 release 持续读取。把新版 Chart 放到新目录不会改变集群；只有执行 `helm upgrade` 才会更新集群。因此推荐保留旧目录，新旧版本并排存放：

```text
/opt/sandbox/
├── charts/
│   ├── sandbox-old/             # 旧 Chart，只用于回看
│   └── sandbox-0.2.0/           # 新 Chart，整套复制
├── env/
│   └── values-prod-minio.yaml   # 本服务器的环境配置
└── backups/
    └── <upgrade-id>/            # 升级前导出的现场
```

各文件的处理规则如下：

| 服务器现有文件 | 升级时如何处理 |
|---|---|
| `templates/*` | 整套使用新版，不能新旧混合 |
| `Chart.yaml` | 使用新版 |
| `values.schema.json` | 使用新版 |
| Chart 自带 `values.yaml` | 使用新版，不能保留旧版 |
| 服务器环境参数 | 从旧配置中迁移到 Chart 目录外的 `values-prod-*.yaml` |
| AK/SK、API key | 保留在 Kubernetes Secret，不写进任何 values 文件 |

如果服务器以前直接修改了 Chart 自带的 `values.yaml`，先把它备份为参考文件；不要再把它放回新版 Chart。升级步骤如下。

第一步，定义实际目录并保存现场：

```bash
CTX=<context>
NS=<namespace>
RELEASE=<release>
OLD_CHART=/opt/sandbox/charts/sandbox-old
NEW_CHART=/opt/sandbox/charts/sandbox-0.2.0
ENV_VALUES=/opt/sandbox/env/values-prod-minio.yaml
UPGRADE_ID="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="/opt/sandbox/backups/$UPGRADE_ID"

umask 077
install -d -m 0700 "$BACKUP_DIR" "$(dirname "$ENV_VALUES")"
cp -a "$OLD_CHART" "$BACKUP_DIR/chart"

helm --kube-context "$CTX" get values "$RELEASE" -n "$NS" -o yaml \
  > "$BACKUP_DIR/release-user-values.yaml"

helm --kube-context "$CTX" get values "$RELEASE" -n "$NS" --all -o yaml \
  > "$BACKUP_DIR/release-effective-values.yaml"

helm --kube-context "$CTX" get manifest "$RELEASE" -n "$NS" \
  > "$BACKUP_DIR/release-manifest.yaml"
```

`release-user-values.yaml` 可能为空：如果旧部署是通过直接修改 Chart 自带 `values.yaml` 完成的，这些参数在 Helm 看来属于旧 Chart 默认值。此时以备份的 `chart/values.yaml` 和 `release-effective-values.yaml` 为迁移参考。历史 values 或 manifest 可能包含旧部署写入 Helm 的敏感值，备份目录必须保持 `0700`，不得提交到 Git 或复制到普通共享目录。

第二步，把新版 `deploy/helm/sandbox` **完整复制到一个不存在的新目录**，包括新版 `values.yaml` 和 `values.schema.json`：

```bash
test ! -e "$NEW_CHART"
cp -a /path/to/new-source/deploy/helm/sandbox "$NEW_CHART"
```

如果受服务器目录限制必须原地替换，正确顺序是：先备份旧 Chart，整套覆盖为新版（包括新版 `values.yaml`），再参考旧文件把环境参数迁移进**新版** `values.yaml`。不能先保留旧 `values.yaml`，再只补几个新字段。这个原地方案技术上可行，但会继续把 Chart 默认值和服务器环境值混在一起；并排 Chart + 外部环境 values 更容易审计和回看，因此优先使用并排方式。

第三步，创建 Chart 外部的环境 values。不要把 `release-effective-values.yaml` 直接作为 `-f` 输入；它包含旧 Chart 的全部默认值，会把废弃字段和旧默认行为带回新版。以新版后端 overlay 为字段结构参考，从备份中逐项迁移以下环境配置：

- API 镜像、副本、HPA 和资源限制；
- Redis 持久化或外部 Redis；
- runtime namespace、普通 Pool 和 FUSE Pool 数量；
- ordinary runtime 和 gateway 镜像；
- 单一 backend preset、endpoint、bucket、region、TLS、storage identity；
- system egress 的 DNS、FQDN/CIDR 和精确端口；
- workspace Secret 名称、mounter/Docker FUSE 镜像和 LSM profile；
- API key Secret 名称；
- `networkEnabled=true`、`networkBlockPrivate=true`；业务请求需要访问内网时仍由调用方提交明确白名单。

后端和凭据都没有变化时，保持原 Secret 名称、`storageIdentity` 和 `credentialGeneration` 不变。只更新 API 镜像不会触发 backend drain。若轮换了凭据，使用新 Secret 名称并递增 `credentialGeneration`，按照第 5 节执行。

最终升级命令始终指向**完整的新 Chart 目录**和**Chart 外部的环境 values**：

```bash
helm --kube-context "$CTX" upgrade "$RELEASE" "$NEW_CHART" \
  --namespace "$NS" \
  -f "$ENV_VALUES" \
  --reset-values \
  --atomic \
  --wait \
  --timeout 15m
```

不要执行下面这种混合升级：

```text
新 templates + 新 Chart.yaml + 旧 values.yaml
```

### 3.2 保存现场并检查升级能力

以下变量用于后续通用示例；在服务器上操作时，`CHART` 和 `VALUES` 应分别指向上一节的 `NEW_CHART` 和 `ENV_VALUES`：

```bash
CTX=ds-ai-research
NS=sandbox-fuse
RELEASE=sandbox-fuse
CHART=/opt/sandbox/charts/sandbox-0.2.0
VALUES=/opt/sandbox/env/values-prod-minio.yaml
```

```bash
helm --kube-context "$CTX" get values "$RELEASE" -n "$NS" -o yaml \
  > "/tmp/${RELEASE}-values-before-upgrade.yaml"

helm --kube-context "$CTX" status "$RELEASE" -n "$NS"

kubectl --context "$CTX" -n "$NS" \
  get configmap "${RELEASE}-backend-fingerprint"
```

当前 Chart 通过 backend fingerprint 决定是否需要全量排空：

- 只修改 API 镜像、副本、资源等非 backend 字段：不改变 fingerprint，执行普通滚动升级，保留 persistent sandbox。
- 修改 preset、endpoint、bucket、region、TLS、subPath、storage identity、Secret 名/key、`credentialGeneration`、CA、system egress、mounter 或 Docker FUSE 镜像：改变 fingerprint，升级前自动执行 release drain。
- fingerprint 缺失：按 backend 变化处理，先排空再升级。

非常老的 release 可能同时缺少 fingerprint 和 drain RBAC。升级前检查 service account 权限：

```bash
kubectl --context "$CTX" -n "$NS" \
  auth can-i update deployment/"${RELEASE}-api" \
  --as="system:serviceaccount:${NS}:${RELEASE}-api"

kubectl --context "$CTX" -n "$NS" \
  auth can-i delete hpa/"${RELEASE}-api" \
  --as="system:serviceaccount:${NS}:${RELEASE}-api"

kubectl --context "$CTX" -n "$NS" \
  auth can-i list pods \
  --as="system:serviceaccount:${NS}:${RELEASE}-api"

kubectl --context "$CTX" -n "$NS" \
  auth can-i list networkpolicies.networking.k8s.io \
  --as="system:serviceaccount:${NS}:${RELEASE}-api"

kubectl --context "$CTX" -n "$NS" \
  auth can-i list ciliumnetworkpolicies.cilium.io \
  --as="system:serviceaccount:${NS}:${RELEASE}-api"
```

任一结果为 `no` 时，不得直接跨版本升级。先在维护窗口补齐 drain RBAC，并使用旧 backend 配置完成 release drain；不能通过删除 Redis、强删 Pod 或跳过 hook 迁移。

### 3.3 渲染检查

```bash
helm lint "$CHART" -f "$VALUES"

helm --kube-context "$CTX" upgrade "$RELEASE" "$CHART" \
  --namespace "$NS" \
  -f "$VALUES" \
  --reset-values \
  --dry-run=server
```

在缺少专用 LSM 的验收集群中，渲染和正式升级都必须显式追加：

```bash
--set config.workspace.allowMissingLSMForKind=true \
--set-string config.workspace.lsmProfile=''
```

生产环境不得追加这两个参数。

### 3.4 正式升级

```bash
helm --kube-context "$CTX" upgrade "$RELEASE" "$CHART" \
  --namespace "$NS" \
  -f "$VALUES" \
  --reset-values \
  --atomic \
  --wait \
  --timeout 15m
```

若 fingerprint 发生变化，Chart 会创建 `<release>-backend-change-drain` Job。Job 使用新 API 镜像中的 drain 实现，但继承旧 Deployment 的后端环境与 Secret volume，确保先使用旧凭据完成最终 flush、卸载、runtime 删除和零状态审计，再应用新后端。

fingerprint 变化时，在另一个终端观察 drain；成功的 hook Job 会被 Helm 自动删除：

```bash
kubectl --context "$CTX" -n "$NS" \
  logs job/"${RELEASE}-backend-change-drain" -f
```

排空失败时 Helm 不会切换后端。保留旧 endpoint、旧 Secret、Redis 和受管 runtime，修复失败原因后删除失败 Job，并用完全相同的目标 values 重试 `helm upgrade`。

### 3.5 升级后验证

```bash
helm --kube-context "$CTX" status "$RELEASE" -n "$NS"

kubectl --context "$CTX" -n "$NS" \
  rollout status deployment/"${RELEASE}-api" --timeout=5m

kubectl --context "$CTX" -n "$NS" get pods -o wide

kubectl --context "$CTX" -n "$NS" \
  logs deployment/"${RELEASE}-api" --since=15m \
  | grep -E 'ERROR|termination is unconfirmed|teardown stage failed' || true
```

预热 FUSE Pod 尚未绑定 workspace 时，mounter readiness 失败、Pod 显示 `1/2 Ready` 是预期状态；Pool 以 supervisor 的 prepared 状态判断空壳健康，不以 Pod Ready 判断。

## 4. 安装全新 release

```bash
CTX=<context>
NS=<namespace>
RELEASE=<release>
CHART=deploy/helm/sandbox
VALUES=<production-values-file>

kubectl --context "$CTX" create namespace "$NS"
```

按照 2.3 节创建对象存储和 API key Secret，再执行：

```bash
helm lint "$CHART" -f "$VALUES"

helm --kube-context "$CTX" install "$RELEASE" "$CHART" \
  --namespace "$NS" \
  -f "$VALUES" \
  --atomic \
  --wait \
  --timeout 15m
```

全新安装没有旧 release，因此不会运行 backend-change drain。安装成功后必须执行 3.5 节检查，并分别通过 API 验证：

1. ephemeral 无 workspace；
2. ephemeral + sync；
3. persistent + sync；
4. ephemeral + FUSE；
5. persistent + FUSE；
6. 同 prefix 的 sync/FUSE 冲突；
7. 不同 prefix 的 sync/FUSE 并发。

## 5. 切换后端与轮换凭据

切换 MinIO、华为公有云 OBS 或华为私有云 OBS 时，只替换 release 的完整 backend overlay，不叠加多份 overlay。

凭据轮换推荐使用新 Secret 名称：

1. 保留旧 Secret；
2. 提前创建新控制面/runtime Secret；
3. values 改为新 Secret 名并递增 `credentialGeneration`；
4. 执行带 `--atomic` 的 `helm upgrade`；
5. 等 backend-change drain 和新 Deployment 全部成功；
6. 验证旧 generation 的 runtime、Pool 和 Redis key 已清空后，再删除旧 Secret。

不要直接覆盖正在被旧 Deployment 使用的 Secret 内容。Kubernetes projected Secret 可能立即更新，导致排空阶段突然失去旧后端访问能力。

## 6. 回滚与卸载

禁止 rollback 到不含 backend fingerprint 和 drain hook 的旧 Chart。若只回退应用代码，继续使用当前 Chart，仅把 API image 改为旧的 digest。

后端回退同样属于 backend 变化，必须完成当前后端 release drain 后才能切回。不要使用 `--no-hooks`。

正常卸载使用：

```bash
helm --kube-context "$CTX" uninstall "$RELEASE" \
  --namespace "$NS" \
  --timeout 15m
```

pre-delete Job 会先缩容 API、完成 persistent/ephemeral finalization、排空普通 Pool 和 FUSE Pool，并进行零状态审计，然后 Helm 才能删除 Redis 等 release 资源。

## 7. 镜像构建清单

仅部署 Kubernetes runtime 时需要三份项目镜像：

| 镜像 | 必需 | 用途 |
|---|---|---|
| `sandbox-fuse-api` | 是 | API、Pool、租约、排空控制面 |
| `sandbox-fuse-runtime` | 是 | 普通/sync/无 workspace sandbox，内含非特权 probe |
| `sandbox-fuse-mounter` | 是 | Kubernetes Pod 内可信 FUSE sidecar |

同时交付 Docker runtime 时再增加：

| 镜像 | 必需 | 用途 |
|---|---|---|
| `sandbox-fuse-docker` | 是 | Docker 特殊 FUSE sandbox |
| `sandbox-fuse-gateway` | 是 | Docker 用户网络和 FUSE system egress gateway |

Redis 使用批准的仓库镜像，不在本项目构建。mounter 和 Docker FUSE 镜像内包含全部三个受信 profile，不按 MinIO/OBS 重复构建。

以下示例构建 `ds-ai-research` 使用的 ARM64 镜像。AMD64 应重新以 `GOARCH=amd64` 和 `--platform linux/amd64` 构建；FUSE 二进制在 Docker build 前生成，不能把 ARM64 二进制放进 AMD64 镜像。

```bash
REG=registry.i.huaxisy.com/library/ai-infra
ARCH=arm64
PLATFORM=linux/arm64
VERSION="$(git rev-parse --short=12 HEAD)"
```

### 7.1 API

```bash
docker buildx build \
  --platform "$PLATFORM" \
  -f docker/Dockerfile \
  -t "$REG/sandbox-fuse-api:${VERSION}-${ARCH}" \
  --push \
  .
```

### 7.2 普通 runtime

生产构建必须向两个基础镜像变量传入 digest-pinned 引用：

```bash
: "${GO_BUILDER_IMAGE:?set digest-pinned Go builder image}"
: "${SANDBOX_BASE_IMAGE:?set digest-pinned sandbox base image}"

docker buildx build \
  --platform "$PLATFORM" \
  -f docker/images/sandbox/Dockerfile \
  --build-arg WORKSPACE_PROBE_BUILDER="$GO_BUILDER_IMAGE" \
  --build-arg SANDBOX_BASE_IMAGE="$SANDBOX_BASE_IMAGE" \
  -t "$REG/sandbox-fuse-runtime:${VERSION}-${ARCH}" \
  --push \
  .
```

### 7.3 Docker gateway

```bash
docker buildx build \
  --platform "$PLATFORM" \
  -f docker/images/gateway/Dockerfile \
  -t "$REG/sandbox-fuse-gateway:${VERSION}-${ARCH}" \
  --push \
  docker/images/gateway
```

Kubernetes runtime 不创建 gateway 容器；该镜像只用于 Docker runtime。

### 7.4 Kubernetes mounter 与 Docker FUSE

两个 FUSE 镜像的 `workspace-mounter` 和 `workspace-probe` 必须来自同一源码 revision：

```bash
BUILD_DIR="$(mktemp -d)"
cleanup_build_dir() {
  test -n "${BUILD_DIR:-}" && rm -rf -- "$BUILD_DIR"
}
trap cleanup_build_dir EXIT
mkdir -p "$BUILD_DIR/mounter" "$BUILD_DIR/docker-fuse"

CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -trimpath -ldflags='-s -w' \
  -o "$BUILD_DIR/workspace-mounter" ./cmd/workspace-mounter

CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -trimpath -ldflags='-s -w' \
  -o "$BUILD_DIR/workspace-probe" ./cmd/workspace-probe

cp docker/images/workspace-mounter/Dockerfile \
  docker/images/workspace-mounter/profile-bundle.json \
  "$BUILD_DIR/mounter/"
cp "$BUILD_DIR/workspace-mounter" "$BUILD_DIR/mounter/"

cp docker/images/sandbox-fuse/Dockerfile "$BUILD_DIR/docker-fuse/"
cp docker/images/workspace-mounter/profile-bundle.json "$BUILD_DIR/docker-fuse/"
cp "$BUILD_DIR/workspace-mounter" "$BUILD_DIR/workspace-probe" \
  "$BUILD_DIR/docker-fuse/"
```

构建输入要求：

- `MOUNTER_BASE_IMAGE`：digest-pinned，提供 `fusermount3`、CA 和 s3fs 所需动态库；
- `DOCKER_FUSE_BASE_IMAGE`：digest-pinned，除上述依赖外还提供 Python、Node、`/usr/sbin/ip` 和 UID/GID 1000 的 `sandbox` 用户；
- `S3FS_PACKAGE_URL`：无 credential、query、fragment 的固定 HTTPS artifact URL；
- `S3FS_PACKAGE_SHA256`：与 artifact 完全一致。本次验证的 s3fs 1.95 ARM64 artifact SHA-256 为 `fb45cbc9f8303ae6d919b8b27ee9f443e285b08e1250f4aa85ca50ea2cfea695`。

```bash
: "${MOUNTER_BASE_IMAGE:?set digest-pinned mounter base image}"
: "${DOCKER_FUSE_BASE_IMAGE:?set digest-pinned Docker FUSE base image}"
: "${S3FS_PACKAGE_URL:?set immutable HTTPS s3fs artifact URL}"
S3FS_PACKAGE_SHA256=fb45cbc9f8303ae6d919b8b27ee9f443e285b08e1250f4aa85ca50ea2cfea695

docker buildx build \
  --platform "$PLATFORM" \
  --build-arg BASE_IMAGE="$MOUNTER_BASE_IMAGE" \
  --build-arg S3FS_PACKAGE_URL="$S3FS_PACKAGE_URL" \
  --build-arg S3FS_PACKAGE_SHA256="$S3FS_PACKAGE_SHA256" \
  -t "$REG/sandbox-fuse-mounter:${VERSION}-${ARCH}" \
  --push \
  "$BUILD_DIR/mounter"

docker buildx build \
  --platform "$PLATFORM" \
  --build-arg BASE_IMAGE="$DOCKER_FUSE_BASE_IMAGE" \
  --build-arg S3FS_PACKAGE_URL="$S3FS_PACKAGE_URL" \
  --build-arg S3FS_PACKAGE_SHA256="$S3FS_PACKAGE_SHA256" \
  -t "$REG/sandbox-fuse-docker:${VERSION}-${ARCH}" \
  --push \
  "$BUILD_DIR/docker-fuse"
```

构建结束后可以提前删除本次临时目录；退出 shell 时上面的 trap 也会清理：

```bash
cleanup_build_dir
BUILD_DIR=
```

`BUILD_DIR` 必须是本次 `mktemp -d` 返回的精确路径；不要把仓库根目录或通用环境变量作为删除目标。

### 7.5 获取 digest 并回填 values

```bash
docker buildx imagetools inspect "$REG/sandbox-fuse-api:${VERSION}-${ARCH}"
docker buildx imagetools inspect "$REG/sandbox-fuse-runtime:${VERSION}-${ARCH}"
docker buildx imagetools inspect "$REG/sandbox-fuse-mounter:${VERSION}-${ARCH}"
docker buildx imagetools inspect "$REG/sandbox-fuse-docker:${VERSION}-${ARCH}"
docker buildx imagetools inspect "$REG/sandbox-fuse-gateway:${VERSION}-${ARCH}"
```

Helm/Compose 最终配置必须使用 registry 返回的 `@sha256:<digest>`，不能只使用可变 tag。API values 的写法为：

```yaml
image:
  repository: registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-api
  tag: <tag>@sha256:<digest>
```

runtime 和 FUSE 镜像字段填写完整的 `repository@sha256:<digest>`。

## 8. 发布验证

代码与 Chart 门禁：

```bash
go test -count=1 ./...
go vet ./...
go test -race -count=1 \
  ./internal/sandbox \
  ./internal/runtime/kubernetes \
  ./internal/runtime/docker

./scripts/test-fuse-images.sh
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
git diff --check
```

镜像门禁：

```bash
FUSE_IMAGE=<mounter-image@sha256:digest> \
SANDBOX_IMAGE=<ordinary-runtime-image@sha256:digest> \
  ./scripts/verify-fuse-image.sh kubernetes release-check

SANDBOX_IMAGE=<docker-fuse-image@sha256:digest> \
  ./scripts/verify-fuse-image.sh docker release-check
```

生产发布还必须归档 SBOM、漏洞扫描、签名、attestation，以及对应后端和 CPU 架构的完整功能/故障矩阵证据。

## 9. 当前发布限制

当前仓库中的三份 `testdata/fuse/profiles/*.yaml` 仍为 `enabled: false`，表示完整的两 runtime 故障证据尚未全部归档。验收集群中的功能通过不等于生产 release gate 已通过。

此外，两份 FUSE Dockerfile 依赖外部提供的 base image 和 s3fs artifact URL；仓库目前没有正式的 FUSE base image Dockerfile/CI 定义。当前验收 mounter 内记录的 base 引用来自本地中间仓库，不能作为长期可复现的生产构建来源。正式交付前必须把以下内容纳入 CI：

1. FUSE base image 的受版本控制 Dockerfile；
2. digest-pinned 基础镜像与固定 s3fs artifact URL/SHA；
3. ARM64/AMD64 分架构构建；
4. package-check、release-check、扫描、SBOM、签名和 attestation；
5. 自动取得发布 digest，并在同一变更中更新全部后端 overlay。
