# Docker Compose 部署与升级

本文适用于 Linux Docker 主机上的 `docker/docker-compose.yml`。普通升级不需要准备脚本、凭证文件、`docker/secrets` 目录或证书生成步骤。

## 1. 最常用的升级方式

如果没有必须保留的 active/persistent sandbox，直接更新 `.env` 中的版本 tag，然后执行：

```bash
docker compose --env-file docker/.env -f docker/docker-compose.yml up -d
```

就是这一条。Compose 会替换镜像有变化的容器，并保留同一 project 下的 Redis named volume。不要执行 `docker compose down -v`，否则 Redis volume 会被删除。

如果 Compose 文件放在新目录，必须继续使用原来的 project name，否则会被当成另一套部署：

```bash
docker compose ls

PROJECT=<原project名称>
docker compose -p "$PROJECT" --env-file /opt/sandbox/env/production.env \
  -f /opt/sandbox/releases/new/docker/docker-compose.yml up -d
```

## 2. `.env` 怎么写

原来的 AK/SK 仍然直接写在 `.env`，不再生成任何凭证文件。下面是 MinIO 的完整常用配置；没有列出的字段使用 Compose 默认值：

```dotenv
STORAGE_PRESET=minio
STORAGE_BUCKET=aiadp-dev
STORAGE_ENDPOINT=minio.example.com
STORAGE_ACCESS_KEY=<access-key>
STORAGE_SECRET_KEY=<secret-key>
STORAGE_REGION=us-east-1
STORAGE_SUB_PATH=workspaces
STORAGE_USE_SSL=true
STORAGE_IDENTITY=production-minio

SECURITY_API_KEY=<sandbox-api-key>
REDIS_PASSWORD=
REDIS_DB=0

SANDBOX_API_IMAGE=registry.example.com/sandbox-api:v0.2.13
SANDBOX_IMAGE=registry.example.com/sandbox-runtime:v0.2.13
GATEWAY_IMAGE=registry.example.com/sandbox-gateway:v0.2.13

WORKSPACE_DEFAULT_MOUNT_MODE=sync
WORKSPACE_ENABLED_MOUNT_MODES=sync,fuse
WORKSPACE_CREDENTIAL_GENERATION=rotation-1
FUSE_MOUNTER_IMAGE=registry.example.com/sandbox-fuse-mounter:v0.2.13
FUSE_SANDBOX_IMAGE=registry.example.com/sandbox-fuse-docker:v0.2.13
FUSE_LSM_PROFILE=sandbox-fuse

FUSE_SYSTEM_EGRESS_MODE=cidr
FUSE_DNS_CIDRS=1.1.1.1/32
STORAGE_ENDPOINT_HOST_IPS=<对象存储解析出的IP>
STORAGE_ENDPOINT_CIDRS=<对象存储IP/32>
STORAGE_ENDPOINT_PORTS=443
```

华为公有云 OBS 改为：

```dotenv
STORAGE_PRESET=huawei-obs-public
STORAGE_ENDPOINT=https://obs.<region>.myhuaweicloud.com
STORAGE_REGION=<region>
```

2023 私有云 OBS 改为：

```dotenv
STORAGE_PRESET=huawei-obs-private
STORAGE_ENDPOINT=https://<私有云OBS endpoint>
STORAGE_REGION=<私有云region>
```

同一部署只配置一个 preset。MinIO、公有云 OBS、私有云 OBS 共用同一组 FUSE 镜像；sync 和 FUSE 也共用同一组 `STORAGE_*` 凭据。

`WORKSPACE_CREDENTIAL_GENERATION` 是非敏感的 Pool 轮换标记。AK/SK 变更时把它从例如 `rotation-1` 改成 `rotation-2`，不要把 AK/SK 或其哈希写进该字段。

`.env` 应设置为仅部署账号可读，例如 `chmod 600 docker/.env`。AK/SK 会存在于 `sandbox-api` 容器环境中，因此应限制 Docker daemon/Socket 权限，排障时不要粘贴完整的 `docker compose config` 或 `docker inspect` 输出。

Docker FUSE 的 system egress 必须填写对象存储的精确 IP/CIDR 和端口。不能用 `0.0.0.0/0`，也不能为了连通对象存储修改“开放公网、禁止内网”的原网络策略；访问内网业务服务仍必须走白名单。

## 3. 全新部署

1. 把仓库中的新版 `docker/docker-compose.yml` 和你的 `.env` 放到服务器。
2. 确保五个版本镜像已经在本机，或服务器可以从 registry 拉取。
3. 启动并检查：

```bash
docker compose --env-file docker/.env -f docker/docker-compose.yml config >/dev/null
docker compose --env-file docker/.env -f docker/docker-compose.yml up -d
docker compose --env-file docker/.env -f docker/docker-compose.yml ps
docker compose --env-file docker/.env -f docker/docker-compose.yml logs --tail=200 sandbox-api
```

启用 FUSE 的 Docker 主机必须是 Linux，并提供 `/dev/fuse` 和受约束的 AppArmor/SELinux profile。macOS Docker Desktop 只能用于有限的构建或 kind 验证，不能等同于真实 Linux Docker FUSE 运行环境。

## 4. 已有部署怎么替换文件

- 使用新版 `docker/docker-compose.yml`。
- 保留服务器现有 `.env`，参照新版 `docker/.env.example` 补充新字段。
- 删除旧的 `WORKSPACE_CREDENTIAL_DIR`、`WORKSPACE_SECRET_STAGING_ROOT` 和 credential-file 配置；它们不再生效。
- 在 `.env` 中直接保留 `STORAGE_ACCESS_KEY`、`STORAGE_SECRET_KEY`。
- 更新需要升级的镜像 tag，执行一次 `docker compose up -d`。

不需要先 stop，不需要运行 `prepare.sh`，也不需要创建 `docker/secrets`。

## 5. 什么时候要先排空

以下变化发生时，如果还有必须保留的 active/persistent workspace，应在维护窗口使用旧 backend 配置先执行 release drain，再修改 `.env` 并 `up -d`：

- 更换 preset、endpoint、bucket、region、subPath 或 storage identity；
- 轮换 AK/SK；
- 修改 FUSE mounter/sandbox 镜像或 system egress；
- 从不具备当前 FUSE lifecycle 的旧版本首次升级。

如果明确没有需要保留的 sandbox，Pool 和运行态可以重建，则仍可直接 `up -d`。

## 6. 镜像怎么构建

使用版本 tag；不要求输入 SHA256，也不需要设置额外 builder/base-image 环境变量。以下命令在仓库根目录执行：

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

分别构建各 CPU 架构后，可以继续使用现有 `docker manifest` 流程合并成同一个版本 tag。构建完成后只需把这些 tag 更新到 `.env`，然后 `docker compose up -d`。

## 7. 升级后验证

```bash
docker compose --env-file docker/.env -f docker/docker-compose.yml ps
docker compose --env-file docker/.env -f docker/docker-compose.yml logs --tail=200 sandbox-api
```

再通过 sandbox-api 分别验证 sync 和 FUSE 的创建、代码执行、文件读写、销毁和 Pool 回补。若 FUSE 失败，优先检查 `/dev/fuse`、LSM、endpoint DNS/CIDR 和对象存储权限，不要重启 Docker daemon。
