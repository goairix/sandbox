# Workspace 存储部署与运维手册

配套设计：[Workspace 容器内 FUSE 直接挂载设计](../superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md)

Helm 已有 release 升级、全新安装、镜像构建和发布验收的逐步命令见 [Helm 部署、升级与镜像发布 Runbook](helm-deployment-upgrade.md)。

Docker Compose 已有环境升级、backend/凭据切换、release drain 和回滚的逐步命令见 [Docker Compose 部署与升级 Runbook](docker-compose-deployment-upgrade.md)。

本文是当前 Helm、Docker Compose、后端切换和发布验收的执行入口。旧的 `workspace.mode` 和 `workspace.providers` 只用于应用读取旧配置，不再是部署接口。

## 1. 当前合同

一个 `sandbox-api` release 只选择一个物理对象存储后端，但可以同时提供 sync 和 FUSE workspace：

| preset | provider | 固定 profile | 地址方式 |
|---|---|---|---|
| `minio` | `minio` | `minio-sigv4-path-style-v1` | path-style，只批准 endpoint |
| `huawei-obs-public` | `obs` | `huawei-obs-public-v1` | virtual-host，批准 endpoint 与 `<bucket>.<endpoint>` |
| `huawei-obs-private` | `obs` | `huawei-obs-private-2023-v1` | virtual-host，批准 endpoint 与 `<bucket>.<endpoint>` |

preset 到 provider/profile 的映射固定在受版本控制的 Chart helper 和 Compose 入口中。请求只能选择 `workspace_mount_mode`，不能更换 endpoint、bucket、profile、镜像、Secret、system egress 或注入 s3fs 参数。

推荐发布配置为：

```yaml
config:
  workspace:
    defaultMountMode: sync
    enabledMountModes: [sync, fuse]
```

未提供 `workspace_mount_mode` 的请求走 sync；显式使用 FUSE：

```json
{
  "mode": "persistent",
  "workspace_path": "tenant/project",
  "workspace_mount_mode": "fuse"
}
```

`workspace_path` 是唯一在 Acquire 时才确定的存储输入。预热 FUSE 空壳不会提前挂载业务 prefix。

## 2. Runtime 与 Pool

- Kubernetes：每个 FUSE sandbox 是一个 Pod；可信 mounter sidecar 管理 s3fs，sandbox 容器以 UID/GID 1000 使用 Pod 私有 `emptyDir` 中传播后的 `/workspace`。不存在业务 hostPath，也没有节点级 FUSE DaemonSet。
- Docker：FUSE sandbox 使用专用容器镜像；容器内 root supervisor 管理 s3fs，用户代码仍以 UID/GID 1000 执行。宿主机不挂载业务 `/workspace`，也不运行宿主机常驻 s3fs 进程。
- `sandbox-api` 同时维护普通 Pool 和 FUSE Pool。`config.pool` 控制无 workspace/sync 空壳数量，`config.workspace.fusePool` 控制 FUSE locked 空壳数量。Redis 只保存跨副本库存、租约和 CAS 状态，不负责创建 Pod/容器。
- FUSE 空壳在获得一次 mount authorization 后，无论成功、失败或取消都必须销毁；不能卸载后回池。
- sync 与 FUSE 对相同 canonical prefix 共用一把 Redis 租约，因此跨模式也只能有一个写 owner；不同 prefix 可并发。

## 3. 生命周期语义

| sandbox/workspace | 销毁语义 | Redis 可见性 |
|---|---|---|
| ephemeral，无 workspace | 不访问对象存储，直接回收普通 runtime | 不创建用户 session，也不创建 workspace lifecycle |
| ephemeral + sync | DELETE/超时/停机先执行最终 sync-out，再删除 runtime 和租约 | 只使用私有 ephemeral cleanup record，不恢复为用户 session |
| ephemeral + FUSE | 先 quiesce、durable flush、正常卸载，再删除 runtime 和租约 | 同上 |
| persistent + sync | 最终 sync 失败时保留 runtime、session 和租约，返回 cleanup-pending，可重试 | session 保留至完成 |
| persistent + FUSE | durable flush 或证明失败时 fail closed，不删除 runtime/owner | session 保留至完成 |

进程普通滚动停止会保留 persistent sandbox，只完成 ephemeral finalization 并排空未绑定空壳。release 删除或后端变化必须使用 `--drain-release`，完成全部 persistent/ephemeral finalization、双 Pool 排空和 Redis/Kubernetes 零状态审计。

## 4. 镜像与 profile evidence

三个 preset 在同一架构上共用两份 FUSE 镜像：

- Kubernetes：`sandbox-fuse-mounter:v0.2.12`；
- Docker：`sandbox-fuse-docker:v0.2.12`。

镜像内包含只读的三个 profile descriptor bundle。profile 仍各自保存 mount、durable flush、服务版本、s3fs/Everest 版本、TLS、故障和 API 生命周期证据，不能用一个 backend 的证据替代另一个。所有 profile evidence 必须记录同一 mounter digest 和同一 Docker FUSE digest。

生产镜像必须：

- 使用与节点架构匹配的 immutable digest；
- 从 Dockerfile 固定的 s3fs 1.95 上游提交构建，并自动记录最终二进制 SHA-256；
- 通过 `scripts/verify-fuse-image.sh ... release-check`；
- 归档 SBOM、漏洞扫描、签名和 attestation；
- 不包含 AK/SK、registry token 或任意可执行 s3fs options。

仓库的 profile report 只有在完整 evidence 签发后才可设为 `enabled: true`。`blocked-*` 不能通过修改 values 绕过。

## 5. Secret 与轮换

只支持长期 AK/SK；STS/session token 不在当前范围。凭据不得写进 values、Compose environment、命令行、镜像、日志或 evidence JSON。

Kubernetes 需要两个同内容 Secret，因为 Secret 不能跨 namespace 投射：

```bash
kubectl --context <context> -n <control-namespace> create secret generic <control-secret> \
  --from-file=accessKey=/secure/path/accessKey \
  --from-file=secretKey=/secure/path/secretKey

kubectl --context <context> -n <runtime-namespace> create secret generic <runtime-secret> \
  --from-file=accessKey=/secure/path/accessKey \
  --from-file=secretKey=/secure/path/secretKey
```

如使用私有 CA，在两份 Secret 中增加同名 key，并把该 key 写入 `config.storage.filesystem.caSecretKey`。控制面固定读取 `/run/secrets/workspace/ca.crt`，runtime 使用配置的 Secret key。

Docker 的 `docker/secrets/workspace` 不是证书生成目录，也不会进入 Git 或镜像。它只包含必需的 `accessKey`、`secretKey` 和可选的 `ca.crt`。只有对象存储 endpoint 使用企业私有 CA 时才需要由证书签发方提供 `ca.crt`；项目不生成 CA，公网可信证书无需该文件。

首次安装运行 `./docker/prepare.sh`。脚本安全读取或导入 AK/SK、设置目录和文件权限、按需导入 CA、生成 API key，并在 Linux FUSE 模式下创建和校验 `${WORKSPACE_SECRET_STAGING_ROOT}`。`${WORKSPACE_CREDENTIAL_DIR}` 随后只读挂到 `/run/secrets/workspace`；sandbox-api 只为特殊 FUSE 容器生成短生命周期 root-only 文件。

每次 AK/SK 或 CA 内容轮换必须：

1. 保持旧 Secret 可用并排空当前 release；
2. 更新控制面与 runtime Secret；
3. 递增不含敏感信息的 `credentialGeneration`；
4. 重新部署并确认旧 generation 的 runtime、Pool 和 Redis key 已清空。

## 6. 网络边界

用户网络规则保持不变：

- `enabled=false`：用户流量不出网；
- `enabled=true, block_private=true`：允许公网，拒绝 RFC1918、loopback、link-local 和 metadata 等内网目标；
- 内网服务只有出现在显式 `whitelist` 中才可访问；
- 禁止用 broad allow-all CIDR 替代白名单。

FUSE system egress 是另一条平台维护的窄路径，即使用户网络关闭也只允许：公共 DNS resolver + 当前对象存储精确 endpoint/port。Kubernetes sidecar 与 sandbox 共享 Pod 网络命名空间，因此标准 NetworkPolicy 只能把 endpoint 限定到平台批准范围，不能阻止用户进程直连同一个已批准 endpoint；它不会开放其他内网目标。若必须做到仅 mounter 可达，应使用容器级网络身份、认证 egress proxy 或 CSI/独立挂载模型。

- Kubernetes `cilium-fqdn`：MinIO 只加入 endpoint；OBS 加入 endpoint 和 `<bucket>.<endpoint>`。
- Kubernetes/Docker `cidr`：只填写当前 endpoint 的精确 `/32` 或 `/128`；`endpointHostIPs` 必须落在这些 CIDR 内。
- Docker 不支持 Cilium FQDN，必须使用经过审批的 CIDR，并在 DNS 地址变化后排空、更新、重建。
- 私有云 endpoint 若解析到内网，必须显式开 system whitelist；这不改变用户流量的内网拒绝规则。

不要修改 `sandbox-runtime-default-deny` 来解决 mounter 连通性；应修正 preset 的 system egress 配置。

## 7. Helm 部署

主 `values.yaml` 只定义配置模型，三个 `testdata/values-fuse-*.yaml` 是完整的后端验收 overlay。每个 overlay 固定该后端的 endpoint、bucket、storage identity、Secret 引用、精确网络输入，以及同一组已验证 API/mounter/Docker/runtime 镜像；凭据内容仍只存在于 Kubernetes Secret。发布新镜像时必须在同一提交中更新三个 overlay，并通过 `scripts/test-helm-chart.sh` 的一致性检查，禁止只在一次 Helm 命令中临时覆盖某个后端。

```bash
helm lint deploy/helm/sandbox -f testdata/values-fuse-minio.yaml

helm --kube-context ds-ai-research upgrade --install sandbox-fuse \
  deploy/helm/sandbox \
  --namespace sandbox-fuse --create-namespace \
  -f testdata/values-fuse-minio.yaml
```

三个可选 overlay 分别为 `values-fuse-minio.yaml`、`values-fuse-obs-private.yaml` 和 `values-fuse-obs-public.yaml`；一个 release 每次只能选择其中一个。三者显式保持 `networkEnabled=true`、`networkBlockPrivate=true`，即用户流量允许公网但拒绝内网，不能为解决对象存储连通性而关闭内网拒绝。

本地 kind 使用同一 Chart，只替换 context、registry 镜像和 `allowMissingLSMForKind=true`。该开关只允许缺少 AppArmor/SELinux 的开发或验收集群；生产必须使用受约束 LSM profile。当前 `ds-ai-research` 若仍未配置受约束 LSM，只能作为验收环境使用，并在命令中显式追加以下两项，不能写入后端 overlay：

```bash
--set config.workspace.allowMissingLSMForKind=true \
--set-string config.workspace.lsmProfile=''
```

```bash
kind create cluster --name sandbox-fuse
kubectl config use-context kind-sandbox-fuse
helm upgrade --install sandbox-fuse deploy/helm/sandbox \
  -n sandbox-fuse --create-namespace \
  -f testdata/values-fuse-minio.yaml \
  --set config.workspace.allowMissingLSMForKind=true \
  --set config.workspace.lsmProfile=''
```

### 7.1 Backend fingerprint 与排空

Chart 对 preset/provider/profile、storage identity、endpoint、TLS、region、bucket/subPath、Secret 名/key、credential generation、CA key、system egress 和两份公共镜像计算不含凭据的 fingerprint，并写入 ConfigMap 与 Pod template annotation。

- API image、replica、资源等非 backend 变化不会改变 fingerprint，不触发 release drain；普通 rolling stop 保留 persistent sandbox。
- fingerprint 缺失或变化时，`pre-upgrade,pre-rollback` Job 先删除 HPA、把旧 API Deployment 缩到 0，再继承已安装 Deployment 的旧环境和旧 Secret mount 执行 `--drain-release`。只有零状态审计通过才允许应用新 backend。
- `pre-delete` 独立执行相同 drain，避免 Redis 先被 Helm 删除。

排空失败时 release 不会切换到新 backend。处理顺序：

1. 查看 `kubectl logs job/<release>-backend-change-drain`，修复旧 endpoint、旧 Secret、runtime 或 Redis；
2. 不要删除旧对象、旧 Secret、Redis 或受管 runtime；
3. 删除失败 Job 后，使用相同目标 values 重试 `helm upgrade`；
4. 确认 drain 成功和新 Deployment Ready 后再清理旧 Secret。

禁止直接 rollback 到不含 fingerprint/drain hook 的旧 Chart。若必须回退应用代码，使用仍含 guard 的 Chart，仅回退 API image；后端回退同样必须触发并通过 drain。

## 8. Docker Compose 部署

编辑 `docker/.env`，只选择一个 `STORAGE_PRESET`。Compose 入口严格派生 provider/profile；不要再设置旧 `STORAGE_PROVIDER`、`WORKSPACE_PROFILE` 或 `WORKSPACE_MODE`。

启用 hybrid：

```dotenv
STORAGE_PRESET=minio
WORKSPACE_DEFAULT_MOUNT_MODE=sync
WORKSPACE_ENABLED_MOUNT_MODES=sync,fuse
SANDBOX_API_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.2.12
SANDBOX_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-runtime:v0.2.12
GATEWAY_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-gateway:v0.2.12
FUSE_MOUNTER_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-mounter:v0.2.12
FUSE_SANDBOX_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-docker:v0.2.12
```

按项目正常启动链路执行，不打印 `.env`：

```bash
docker compose --env-file docker/.env -f docker/docker-compose.yml config >/dev/null
docker compose --env-file docker/.env -f docker/docker-compose.yml up -d -t 600
docker compose --env-file docker/.env -f docker/docker-compose.yml ps
```

没有需要保留的 active/persistent sandbox 时，更新五个版本 tag 后执行上述 `up -d -t 600` 就是完整升级动作；不需要预先 `down`、本地 build 或单独排空。`-t 600` 用于给旧 API 足够时间清理 Pool/FUSE 空壳。需要保留旧 workspace 的最终写入，或 backend contract 有变化时，才执行完整 release drain。

Compose 不创建长期 mounter service，也不挂载宿主机业务 `/workspace`。生产镜像在部署前单独发布；Compose 只确认或拉取配置的镜像，实际特殊容器仍由 sandbox-api Pool 动态管理。

切换 Docker backend 前，必须先停止旧 API，再使用新版 drain 二进制配合旧 backend 配置、旧凭据和现有 Redis 运行一次 `--drain-release`；确认 `sandbox.managed=true` 的 container/network/volume 以及 Redis 受管前缀为空后，才能切换新 `.env`。旧版本二进制可能没有 drain 参数，完整兼容步骤以 Docker Compose 部署与升级 Runbook 为准。不要靠删除 Redis 或强删容器完成切换。

不要为了恢复单个应用 deployment 重启 Docker daemon 或 Docker Desktop。先查看 Compose logs、容器状态、Secret mount、registry/network；只重启具体 service。daemon 级操作必须由宿主机管理员单独批准，因为它会影响无关容器与本地 Kubernetes。

macOS Docker Desktop 的 Linux VM 若没有 `/dev/fuse`，Docker FUSE preflight 必须失败；不能用 privileged 绕过或伪造 evidence。改用具有 `/dev/fuse` 和受约束 LSM 的 Linux Docker 宿主机。

## 9. API 与验证

默认 sync 请求省略 `workspace_mount_mode`：

```bash
curl -fsS -X POST "$SANDBOX_API/api/v1/sandboxes" \
  -H "Authorization: Bearer $SANDBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"mode":"persistent","timeout":-1,"workspace_path":"team/project"}'
```

FUSE 请求显式添加：

```bash
curl -fsS -X POST "$SANDBOX_API/api/v1/sandboxes" \
  -H "Authorization: Bearer $SANDBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"mode":"persistent","timeout":-1,"workspace_path":"team/project-fuse","workspace_mount_mode":"fuse"}'
```

手工 sync 和 FUSE durable flush 使用同一 API：

```bash
curl -fsS -X POST "$SANDBOX_API/api/v1/sandboxes/$ID/workspace/sync" \
  -H "Authorization: Bearer $SANDBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"direction":"from_container"}'
```

发布门禁：

```bash
./scripts/test-fuse-images.sh
./scripts/test-helm-chart.sh
./scripts/test-workspace-fuse-matrix.sh

WORKSPACE_FUSE_RUN_INTEGRATION=1 \
  ./scripts/workspace-fuse-preflight.sh kubernetes \
  --profile testdata/fuse/profiles/minio-sigv4-path-style-v1.yaml
```

真实矩阵必须覆盖 MinIO、公有 OBS、私有 OBS × Kubernetes、Linux Docker，并在每个组合记录：

1. ephemeral 无 workspace（对象存储 marker/list counter 不变）；
2. ephemeral + sync 最终回写；
3. persistent + sync 手工 sync、销毁、重建；
4. ephemeral + FUSE 最终 durable flush；
5. persistent + FUSE 手工 durable flush、销毁、重建；
6. 同 prefix sync/FUSE 两种顺序均返回 HTTP 409；
7. 不同 prefix sync/FUSE 并发成功。

`scripts/workspace-fuse-matrix.sh` 还要求 fault driver、对象计数器和三份 profile 共用相同两份镜像 digest。证据只记录 preset、profile、runtime、digest 和布尔结果，不记录凭据。

## 10. 故障定位

按顺序检查，避免扩大故障面：

1. API/Job logs 中的 lifecycle state、runtime UID、generation 和 cleanup-pending；
2. Redis、对象 endpoint、DNS、精确 system egress 与 Secret generation；
3. Kubernetes Pod 的 `sandbox.pool.state`、sidecar readiness、NetworkPolicy/CiliumNetworkPolicy；
4. Docker 特殊容器、gateway network、cache volume、`/dev/fuse` 和 root-only staging；
5. `workspace/info` 中 mount type/state/flushed/last flush。

prepared 空壳的 sandbox container NotReady 是正常状态，告警应排除 `sandbox.pool.state=prepared`，改看 Pool capacity、prepare timeout 和 mounter health。

任何 flush proof、mount identity、runtime UID 或 Redis generation 不一致都必须 fail closed。不要 lazy/force unmount，不要跨 storage identity 猜测删除，也不要把 FUSE 失败静默降级成 sync 或空目录。
