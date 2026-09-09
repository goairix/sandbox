# Helm 部署与升级

本文只说明服务器上实际怎么替换 Chart、填写 values、构建镜像和执行升级。FUSE 原理与后端差异见 [Workspace FUSE 运维说明](workspace-fuse.md)。

## 1. 先记住结论

- 新版 `Chart.yaml`、`templates/`、`values.schema.json` 和 Chart 自带的 `values.yaml` 必须整套使用，不能新旧混搭。
- 服务器自己的配置单独放在 Chart 外，例如 `/opt/sandbox/env/values-prod.yaml`。
- AK/SK 直接写在环境 values 的 `config.storage.filesystem.accessKey/secretKey`，不再创建 workspace credential Secret。
- 一个 release 只配置一个后端，但同一后端可以同时服务 sync 和 FUSE。
- 普通升级执行一次 `helm upgrade`；backend、凭据或 FUSE 镜像变化时，Chart 会根据 backend fingerprint 先排空。

原有网络规则不能改：开放公网访问时仍禁止内网访问；确需访问内网服务时必须加明确白名单。FUSE 的 system egress 只开放对象存储 endpoint、DNS 和精确端口。

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
      region: us-east-1
      accessKey: "<access-key>"
      secretKey: "<secret-key>"
      useSSL: true
      subPath: workspaces
      storageIdentity: production-minio
      credentialGeneration: rotation-1
      caSecretKey: ""
      endpointHostIPs: []
      systemEgressMode: cilium-fqdn
      dnsCIDRs: ["223.5.5.5/32"]
      systemEgressCIDRs: []
      endpointPorts: [443]
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

只需把 `preset` 改成 `huawei-obs-public` 或 `huawei-obs-private`，并填写对应 endpoint、region 和网络出口配置，即可使用同一套 API、runtime 和 mounter 镜像。一次部署只选一个 preset。

AK/SK 轮换时同时递增 `credentialGeneration`，例如从 `rotation-1` 改为 `rotation-2`，确保旧 FUSE 空壳不会进入新凭据对应的 Pool。该字段不包含凭据。

普通公网证书不需要 CA 配置。只有企业私有 CA 场景才设置 `caSecretKey` 与根级 `workspaceCA.secretName`；这是 CA 文件兼容入口，与 AK/SK 无关。

因为 AK/SK 直接进入 values，它们也会出现在 Helm release 历史和 `sandbox-api` 环境中。环境 values 文件应限制为运维账号可读，集群 RBAC 也应限制读取 release Secret、Deployment 和 Pod 详情；排障时不要输出完整 values、渲染清单或容器环境。

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

只改 API tag、普通 runtime tag、副本或资源时是普通滚动升级。修改 preset、endpoint、bucket、AK/SK、`credentialGeneration`、system egress 或 FUSE 镜像时，会改变 backend fingerprint，Chart 的 pre-upgrade hook 会先执行 release drain。

## 6. 镜像怎么构建

所有镜像使用版本 tag，不要求手工填写 SHA256，也不需要设置 `GO_BUILDER_IMAGE` 或 `SANDBOX_BASE_IMAGE`；Dockerfile 已有默认基础镜像。下面命令都在仓库根目录执行：

```bash
VERSION=v0.2.13
REGISTRY=registry.i.huaxisy.com/library/ai-infra

docker buildx build -f docker/Dockerfile -t "$REGISTRY/sandbox-api:$VERSION" .
docker buildx build -f docker/images/sandbox/Dockerfile -t "$REGISTRY/sandbox-runtime:$VERSION" .
docker buildx build -f docker/images/gateway/Dockerfile -t "$REGISTRY/sandbox-gateway:$VERSION" docker/images/gateway
docker buildx build -f docker/images/workspace-mounter/Dockerfile -t "$REGISTRY/sandbox-fuse-mounter:$VERSION" .
docker buildx build -f docker/images/sandbox-fuse/Dockerfile -t "$REGISTRY/sandbox-fuse-docker:$VERSION" .
```

Kubernetes FUSE 必须构建 `sandbox-api`、`sandbox-runtime`、`sandbox-gateway` 和 `sandbox-fuse-mounter`。Docker FUSE 还需要 `sandbox-fuse-docker`。你可以按现有流程分别构建各架构，再用 `docker manifest` 合并同一个版本 tag。

## 7. 升级后检查

```bash
helm --kube-context "$CTX" status "$RELEASE" -n "$NS"
kubectl --context "$CTX" -n "$NS" get deploy,pod,job
kubectl --context "$CTX" -n "$NS" logs deploy/"$RELEASE-api" --tail=200
```

然后通过 sandbox-api 分别创建 sync 和 FUSE sandbox，验证创建、执行、文件读写、销毁以及 FUSE Pool 回补。不要为了排查存储连通性放开内网默认拒绝策略。
