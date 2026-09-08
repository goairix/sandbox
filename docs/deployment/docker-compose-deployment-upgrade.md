# Docker Compose 部署与升级 Runbook

配套资料：

- [Workspace 存储部署与运维手册](workspace-fuse.md)
- [Helm 部署、升级与镜像发布 Runbook](helm-deployment-upgrade.md)
- [Workspace 容器内 FUSE 直接挂载设计](../superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md)

本文面向使用 `docker/docker-compose.yml` 部署 sandbox-api 的 Linux Docker 环境。Docker Compose 没有 Helm backend fingerprint hook，涉及 backend 的升级必须由运维先使用旧配置手工执行 release drain。

## 1. 先选升级方式

### 1.1 无状态或状态可丢弃：直接升级

如果当前没有需要保留的 active/persistent sandbox，Pool 和 Redis 中的运行态也允许重建，并且不切换 backend、凭据或 FUSE 镜像，直接使用新版源码和原服务器环境文件执行：

```bash
PROJECT=<docker-compose-ls中的NAME>
NEW_RELEASE=/opt/sandbox/releases/sandbox-new
ENV_FILE=/opt/sandbox/env/production.env

docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$NEW_RELEASE/docker/docker-compose.yml" \
  up -d --build -t 600 sandbox-images redis sandbox-api
```

这就是常规的 Docker Compose 快速升级：构建新版 sandbox-api，准备 runtime/FUSE 镜像，保留未变更的 Redis volume，并替换 API 容器。`-t 600` 不是额外迁移步骤，只是给旧 API 最多 600 秒完成普通 Pool/FUSE 空壳清理，避免 Compose 默认短超时把它强制终止。

升级后检查：

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$NEW_RELEASE/docker/docker-compose.yml" ps

docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$NEW_RELEASE/docker/docker-compose.yml" logs --tail=200 sandbox-api
```

不要执行 `down -v`，也不要改变 `PROJECT`；前者会删除 Redis volume，后者会另建一套网络、容器和 volume。即使业务没有持久化 sandbox，Redis named volume 仍保存 Pool、租约等运行态，原地 `up` 会自然复用它。

以下情况不能套用这条快速命令，必须执行本文第 6 节的 release drain：

- 旧 workspace 的数据还必须完成最后一次同步或卸载；
- 切换 endpoint、bucket、preset、凭据、CA、storage identity 或 system egress；
- 修改 FUSE mounter/sandbox 镜像或 staging root；
- 首次从不具备当前 FUSE lifecycle contract 的旧版本升级。

### 1.2 需要保留状态或首次迁移：使用完整流程

不要只复制一份新的 `docker-compose.yml`，也不要直接用新版 `.env.example` 覆盖服务器 `.env`。

当前 Compose 会从仓库根目录构建 sandbox-api 和默认 ordinary runtime，因此一个发布版本应包含完整项目源码。推荐新旧版本并排存放，把服务器环境文件、凭据和持久目录放在 release 目录之外：

```text
/opt/sandbox/
├── releases/
│   ├── sandbox-old/                 # 旧完整项目
│   └── sandbox-new/                 # 新完整项目
├── env/
│   └── production.env               # 服务器环境参数
├── secrets/
│   └── workspace/                   # accessKey、secretKey、可选 CA
├── state/
│   └── workspace-secrets/           # Docker FUSE 临时凭据 staging root
└── backups/
```

文件处理规则：

| 文件或目录 | 升级方式 |
|---|---|
| 新版项目源码 | 完整放入新的 release 目录 |
| `docker/docker-compose.yml` | 使用新版，不能与旧源码/Dockerfile 混用 |
| `docker/.env.example` | 只作为新版字段清单，不能直接用于生产 |
| 服务器 `.env` | 保留为外部文件，按新版 `.env.example` 逐项迁移 |
| workspace AK/SK/CA | 使用 release 外的 root-only 文件目录 |
| Redis named volume | 保留；不得执行 `down -v` 或删除 volume |
| FUSE staging root | 保持同一个宿主机绝对路径，除非先完成 release drain |

环境文件中的以下路径必须使用宿主机绝对路径，避免切换 release 目录后解析到其他位置：

```dotenv
WORKSPACE_CREDENTIAL_DIR=/opt/sandbox/secrets/workspace
WORKSPACE_SECRET_STAGING_ROOT=/opt/sandbox/state/workspace-secrets
DOCKER_AUTH_CONFIG_FILE=/opt/sandbox/secrets/docker/config.json
```

## 2. 必须保持 Compose project identity

Compose project name 决定 Redis volume、默认网络和服务容器名称。切换到新的源码目录时，如果不显式指定同一个 project name，Compose 可能创建一套新的 Redis 和网络，表现为历史 session、Pool 和租约全部“消失”。

先在旧环境查看实际 project name：

```bash
docker compose ls
```

后续每一条命令都显式使用同一个 `-p`：

```bash
PROJECT=<docker-compose-ls中的NAME>
OLD_RELEASE=/opt/sandbox/releases/sandbox-old
NEW_RELEASE=/opt/sandbox/releases/sandbox-new
ENV_FILE=/opt/sandbox/env/production.env
OLD_COMPOSE="$OLD_RELEASE/docker/docker-compose.yml"
NEW_COMPOSE="$NEW_RELEASE/docker/docker-compose.yml"
```

检查该 project 当前连接的服务和 volume：

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" ps

docker volume ls --filter "label=com.docker.compose.project=$PROJECT"
```

同一台 Docker daemon 上不要运行多个共享 `sandbox.managed=true` 资源边界的 sandbox release；不同环境也不得共享 Redis DB。release drain 会审计和清理受管 Docker runtime，多个 release 共享 daemon 或 Redis DB 会破坏隔离边界。

## 3. 升级前备份和预检

```bash
UPGRADE_ID="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="/opt/sandbox/backups/$UPGRADE_ID"

umask 077
install -d -m 0700 "$BACKUP_DIR"
cp -a "$ENV_FILE" "$BACKUP_DIR/production.env"
cp -a "$OLD_RELEASE/docker/docker-compose.yml" "$BACKUP_DIR/docker-compose.yml"

docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" config > "$BACKUP_DIR/compose-rendered.yaml"

REDIS_CONTAINER="$(docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" ps -q redis)"
test -n "$REDIS_CONTAINER"
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" exec -T redis redis-cli SAVE
docker cp "$REDIS_CONTAINER:/data/dump.rdb" "$BACKUP_DIR/redis-dump.rdb"
```

`.env`、渲染后的 Compose 和 Redis 备份可能包含敏感信息，目录必须保持 `0700`，不得提交 Git 或复制到普通共享目录。

检查新文件和新环境配置，但此时不要启动新 API：

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$NEW_COMPOSE" config >/dev/null
```

把新版 `docker/.env.example` 与服务器 `production.env` 逐项比较。不要再设置已经废弃的 `STORAGE_PROVIDER`、`WORKSPACE_PROFILE` 或 `WORKSPACE_MODE`；只通过 `STORAGE_PRESET` 选择一个后端。

## 4. 先判断属于哪种升级

### 4.1 普通升级，不需要 release drain

以下变更可以保留 persistent sandbox：

- 只更新 sandbox-api 代码或镜像；
- 只更新 ordinary runtime 镜像；
- 调整 API 端口、日志、telemetry、资源或普通 Pool 数量；
- backend、凭据和 FUSE system egress 完全不变。

旧 API 收到 SIGTERM 后会完成 ephemeral finalization、排空未绑定普通/FUSE 空壳，并保留 persistent sandbox。新 API 使用同一 Redis DB 和 Docker daemon 恢复 persistent 状态。

### 4.2 Backend 变化，必须先 release drain

以下任一变化都必须先使用新版 drain 二进制配合旧 backend 配置、旧凭据和现有 Redis 执行 `--drain-release`：

- `STORAGE_PRESET`、endpoint、bucket、region、TLS 或 subPath；
- `STORAGE_IDENTITY`；
- workspace credential 文件内容或路径；
- `WORKSPACE_CREDENTIAL_GENERATION`；
- CA、endpoint IP、DNS/CIDR/端口、system egress mode；
- `FUSE_MOUNTER_IMAGE` 或 `FUSE_SANDBOX_IMAGE`；
- `WORKSPACE_SECRET_STAGING_ROOT`；
- 第一次从不具备当前 hybrid/FUSE lifecycle contract 的旧版本升级；
- 从 sync-only 启用 FUSE，同时还修改了任一 backend contract 字段。

仅在已经运行当前 lifecycle contract、且 backend/凭据/system egress 完全不变时，单独把 `WORKSPACE_ENABLED_MOUNT_MODES=sync` 扩展为 `sync,fuse` 才可按普通升级处理。

Docker Compose 不会自动比较这些字段，也不会自动创建升级前 hook。不能直接修改 `.env` 后执行 `up -d`。

## 5. 普通升级步骤

先让入口停止向 API 发送新请求。只停止 sandbox-api，保持 Redis 运行；显式提供 600 秒优雅停止时间，不能依赖 Compose 默认的短超时：

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" stop -t 600 sandbox-api
```

确认旧 API 已停止、Redis 仍在运行：

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" ps -a
```

使用新源码构建 API，并拉取或构建新 runtime 镜像：

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$NEW_COMPOSE" build sandbox-api

docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$NEW_COMPOSE" run --rm --no-deps sandbox-images
```

最后只替换 API，不重建 Redis：

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$NEW_COMPOSE" up -d --no-deps --no-build sandbox-api
```

如果构建或启动失败，保留 Redis、persistent sandbox 和旧 release 文件，使用旧 Compose 重新启动旧 API；不要执行 `down`：

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" build sandbox-api

docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" up -d --no-deps sandbox-api
```

## 6. Backend、凭据或 FUSE 镜像变化的升级步骤

本节必须在覆盖旧 `.env` 或修改旧凭据文件**之前**执行。旧版本二进制可能没有 `--drain-release`，因此标准流程使用新版 API 二进制，但给它传入旧 backend、旧 Redis、旧镜像合同和旧凭据路径；这与 Helm backend-change hook 的原则一致。

第一步，停止入口流量并优雅停止旧 API：

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" stop -t 600 sandbox-api
```

第二步，按新版 `docker/.env.example` 创建一个临时的旧后端环境文件。它使用新字段名，但值必须描述当前正在运行的旧环境，不能填写准备切换的新后端：

```bash
DRAIN_ENV=/opt/sandbox/env/drain-old-backend.env
```

`DRAIN_ENV` 至少要保持旧环境的以下值：

- `STORAGE_PRESET`、endpoint、bucket、region、TLS、subPath 和 storage identity；
- 旧 workspace credential 目录和 credential generation；
- 旧 CA、system egress、runtime/gateway/FUSE 镜像；
- 当前 Redis DB、Pool 和超时配置；
- `WORKSPACE_ENABLED_MOUNT_MODES`：旧 sync-only 环境填 `sync`，旧 hybrid 环境填原值。

不要通过 shell `source` 旧 `.env`；dotenv 文件不等同于可信 shell 脚本。使用编辑器逐项迁移，并先渲染确认：

```bash
docker compose -p "$PROJECT" --env-file "$DRAIN_ENV" \
  -f "$NEW_COMPOSE" config > "$BACKUP_DIR/drain-compose-rendered.yaml"
```

第三步，在 Redis 仍运行时构建新版 API，然后使用新版 Compose wiring 和 `DRAIN_ENV` 启动一次性 drain 容器：

```bash
docker compose -p "$PROJECT" --env-file "$DRAIN_ENV" \
  -f "$NEW_COMPOSE" build sandbox-api
```

下面的 preset 映射必须保持与新版 `docker-compose.yml` 一致：

```bash
docker compose -p "$PROJECT" --env-file "$DRAIN_ENV" \
  -f "$NEW_COMPOSE" run --rm --no-deps \
  --entrypoint /bin/sh sandbox-api -ec '
    case "$SANDBOX_WORKSPACE_BACKEND_PRESET" in
      minio)
        export SANDBOX_STORAGE_FILESYSTEM_PROVIDER=minio
        export SANDBOX_WORKSPACE_BACKEND_PROFILE=minio-sigv4-path-style-v1
        ;;
      huawei-obs-public)
        export SANDBOX_STORAGE_FILESYSTEM_PROVIDER=obs
        export SANDBOX_WORKSPACE_BACKEND_PROFILE=huawei-obs-public-v1
        ;;
      huawei-obs-private)
        export SANDBOX_STORAGE_FILESYSTEM_PROVIDER=obs
        export SANDBOX_WORKSPACE_BACKEND_PROFILE=huawei-obs-private-2023-v1
        ;;
      *)
        echo "unsupported old STORAGE_PRESET: $SANDBOX_WORKSPACE_BACKEND_PRESET" >&2
        exit 2
        ;;
    esac
    if [ -n "$SANDBOX_WORKSPACE_BACKEND_CA_SECRET_KEY" ]; then
      export SANDBOX_STORAGE_FILESYSTEM_CA_FILE=/run/secrets/workspace/ca.crt
    fi
    exec /app/sandbox --config /etc/sandbox/config.yaml \
      --drain-release --drain-timeout=10m
  '
```

只有输出包含以下内容并且命令退出码为 0，才能继续：

```text
release drain completed with zero managed state
```

确认没有受管 runtime 残留：

```bash
docker ps -a --filter label=sandbox.managed=true \
  --format 'table {{.ID}}\t{{.Names}}\t{{.Status}}'

docker network ls --filter label=sandbox.managed=true
docker volume ls --filter label=sandbox.managed=true
```

如果 drain 失败，不能修改旧凭据、Redis 或受管容器。修复旧 endpoint、旧凭据、Docker/FUSE 故障后，使用同一条旧配置命令重试。

如果需要恢复旧服务，先用旧源码重新构建被新版 build 覆盖的 `sandbox-api:latest`，再使用旧 Compose 和旧环境文件启动：

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" build sandbox-api

docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$OLD_COMPOSE" up -d --no-deps sandbox-api
```

第四步，drain 成功后才创建新的凭据目录或新的 `.env`。凭据轮换不能原地覆盖旧 `accessKey`/`secretKey`；先写入新的 root-only 目录，再让新环境文件引用新目录：

```bash
install -d -o root -g root -m 0700 /opt/sandbox/secrets/workspace-new
install -o root -g root -m 0400 /secure/new/accessKey \
  /opt/sandbox/secrets/workspace-new/accessKey
install -o root -g root -m 0400 /secure/new/secretKey \
  /opt/sandbox/secrets/workspace-new/secretKey
```

在新环境文件中同步更新：

```dotenv
WORKSPACE_CREDENTIAL_DIR=/opt/sandbox/secrets/workspace-new
WORKSPACE_CREDENTIAL_GENERATION=<new-non-secret-generation>
```

第五步，使用新 Compose 和新环境文件完成 `config`、build、image helper 和 API 启动，仍保持同一个 `PROJECT` 和 Redis volume：

```bash
NEW_ENV_FILE=/opt/sandbox/env/production-new.env

docker compose -p "$PROJECT" --env-file "$NEW_ENV_FILE" \
  -f "$NEW_COMPOSE" config >/dev/null

docker compose -p "$PROJECT" --env-file "$NEW_ENV_FILE" \
  -f "$NEW_COMPOSE" build sandbox-api

docker compose -p "$PROJECT" --env-file "$NEW_ENV_FILE" \
  -f "$NEW_COMPOSE" run --rm --no-deps sandbox-images

docker compose -p "$PROJECT" --env-file "$NEW_ENV_FILE" \
  -f "$NEW_COMPOSE" up -d --no-deps --no-build sandbox-api
```

后端切换已经销毁旧 backend 的 persistent/ephemeral sandbox 和两类 Pool。新 API 启动后会按新配置重新补池。

## 7. 全新 Docker Compose 安装

全新环境必须选择唯一且固定的 project name：

```bash
PROJECT=sandbox-production
RELEASE_DIR=/opt/sandbox/releases/sandbox-new
ENV_FILE=/opt/sandbox/env/production.env
COMPOSE_FILE="$RELEASE_DIR/docker/docker-compose.yml"
```

创建 release 外部的凭据和 staging 目录：

```bash
install -d -o root -g root -m 0700 /opt/sandbox/secrets/workspace
install -d -o root -g root -m 0700 /opt/sandbox/state/workspace-secrets
```

根据新版 `docker/.env.example` 创建外部环境文件，使用绝对路径并只选择一个 `STORAGE_PRESET`。启用 FUSE 时必须配置 digest-pinned `FUSE_MOUNTER_IMAGE` 和 `FUSE_SANDBOX_IMAGE`；Docker 主机必须是提供 `/dev/fuse` 和受约束 LSM 的 Linux 主机。

```bash
docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$COMPOSE_FILE" config >/dev/null

docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$COMPOSE_FILE" build sandbox-api

docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$COMPOSE_FILE" run --rm --no-deps sandbox-images

docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$COMPOSE_FILE" up -d --no-build redis

docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
  -f "$COMPOSE_FILE" up -d --no-deps --no-build sandbox-api
```

不要在 macOS Docker Desktop 缺少 `/dev/fuse` 时用 privileged 绕过 preflight；该环境只能验证 sync 或改用真实 Linux Docker 主机验证 FUSE。

## 8. 升级后验证

先显式指定本次实际启动使用的 Compose 和环境文件。普通升级填写 `NEW_COMPOSE`/`ENV_FILE`，backend 切换填写 `NEW_COMPOSE`/`NEW_ENV_FILE`，全新安装填写 `COMPOSE_FILE`/`ENV_FILE`：

```bash
ACTIVE_COMPOSE="$NEW_COMPOSE"
ACTIVE_ENV="$ENV_FILE"

docker compose -p "$PROJECT" --env-file "$ACTIVE_ENV" \
  -f "$ACTIVE_COMPOSE" ps

docker compose -p "$PROJECT" --env-file "$ACTIVE_ENV" \
  -f "$ACTIVE_COMPOSE" logs --since=15m sandbox-api \
  | grep -E 'ERROR|termination is unconfirmed|teardown stage failed' || true
```

确认：

- sandbox-api 和 Redis 健康；
- 普通 Pool 与 FUSE Pool 回到目标数量；
- 无持续增长的 cleanup/tombstone；
- sync、FUSE、无 workspace 三类 API 路径分别通过；
- `networkEnabled=true`、`networkBlockPrivate=true` 语义未改变；
- 公网可访问、内网默认拒绝、显式白名单仍生效。

FUSE 空壳在尚未获得 `workspace_path` 时只处于 prepared/locked，不会提前挂载对象存储 prefix。

## 9. 回滚

普通代码升级失败时：

1. 用 600 秒超时停止新 API；
2. 保持同一个 `PROJECT`、Redis volume、旧 `.env` 和旧凭据；
3. 使用旧 release 目录重新构建/启动旧 API；
4. 不删除 persistent sandbox。

如果新 backend 已经启动并创建了 sandbox，回滚 backend 也属于一次 backend 切换：必须先用新 Compose、新 `.env` 和新凭据执行第 6 节的 release drain，成功后才能恢复旧 backend。不能直接换回旧 `.env`。

## 10. 禁止操作

- 不执行 `docker compose down -v`；
- 不删除 Redis volume 或 Redis 受管 key；
- 不强删 `sandbox.managed=true` 容器、网络或 volume 来代替 drain；
- 不修改 Compose project name；
- 不在 drain 前覆盖旧 AK/SK/CA 文件；
- 不在 backend 变化时直接执行 `up -d`；
- 不为单个 sandbox-api 升级重启 Docker daemon 或 Docker Desktop；
- 不为对象存储连通性关闭“公网允许、内网拒绝”规则。
