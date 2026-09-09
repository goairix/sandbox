# Workspace 配置内凭据设计

日期：2026-09-09

状态：已确认

关联设计：[Workspace 容器内 FUSE 直接挂载设计](2026-09-01-workspace-container-fuse-mount-design.md)

## 1. 目标

Docker Compose 和 Helm 延续原有运维方式：对象存储 AK/SK 直接写在部署配置中，启动和升级不依赖初始化脚本、外部凭证文件或预先创建的 Kubernetes Secret。

- Docker Compose 从 `.env` 的 `STORAGE_ACCESS_KEY`、`STORAGE_SECRET_KEY` 读取凭据。
- Kubernetes 从 Helm values 的 `config.storage.filesystem.accessKey`、`config.storage.filesystem.secretKey` 读取凭据。
- `sandbox-api` 仍是唯一可信控制面。请求不能提交、覆盖或选择后端凭据。
- sync 和 FUSE 使用同一组 release 级凭据。

运维明确接受凭据会存在于 `.env`、Helm values、Helm release 历史及 `sandbox-api` 环境变量中的风险。实现仍不得把凭据写入日志、Redis、PoolKey、label、annotation、事件或 API 响应。

## 2. 不采用的方案

- 不要求运行 `docker/prepare.sh`。
- 不要求维护 `docker/secrets` 或其他宿主机凭证目录。
- 不要求运维预先创建 Kubernetes Secret。
- 不把 AK/SK 放进 sandbox Pod/container spec、用户 exec 环境或用户可读文件。

`docker/prepare.sh` 和外部凭证文件入口从默认部署流程移除；如果代码中暂时保留兼容入口，也不能成为启动前置条件或文档主路径。

## 3. 配置模型

应用继续使用已有的 `storage.filesystem.access_key`、`storage.filesystem.secret_key` 字段，二者必须同时为空或同时非空。选中远端对象存储后端时，二者必须同时非空。

Docker Compose 映射：

```dotenv
STORAGE_ACCESS_KEY=<access-key>
STORAGE_SECRET_KEY=<secret-key>
```

Helm values 映射：

```yaml
config:
  storage:
    filesystem:
      accessKey: <access-key>
      secretKey: <secret-key>
```

Chart 将这两个 values 只注入 `sandbox-api` Deployment 的环境变量，不创建 Secret，也不注入动态 sandbox Pod。

AK/SK 或 CA 内容变化时，运维仍必须递增 `credentialGeneration`。PoolKey 和 backend fingerprint 只包含该 generation，不包含凭据内容。

## 4. 凭据数据流

### 4.1 控制面加载

`sandbox-api` 启动时从配置读取 AK/SK。对象存储 API 客户端直接使用内存中的凭据，sync 路径维持现有行为。

配置校验禁止只提供一半凭据，也禁止同时混用 inline 和 file credential source。错误信息只能指出字段缺失或冲突，不能包含字段值。

### 4.2 FUSE 挂载

预热 Pool 空壳不携带凭据，也不提前挂载 prefix。Acquire 获得 `workspace_path/prefix` 和独占租约后，`sandbox-api` 才通过已有私有 bootstrap 控制通道，把本次挂载所需的 AK/SK 与固定后端参数一起发送给可信 `workspace-mounter`。

- Kubernetes 使用对 mounter sidecar 的私有 exec/stdin 控制通道。
- Docker 使用对特殊 sandbox 容器 root supervisor 的私有 exec/stdin 控制通道。
- 凭据不进入 `WorkspaceFUSESpec` 的持久化表示、Redis、Pod env、container env 或 Docker label。
- bootstrap、错误和审计日志对凭据字段执行固定脱敏，禁止记录原始请求体。

`workspace-mounter` 收到凭据后，只在容器私有 `/run/s3fs` tmpfs 中生成 s3fs 必需的 root-only `passwd-s3fs`。挂载完成后 supervisor 清除内存中的原始凭据；卸载或容器销毁时 tmpfs 自动消失。用户进程始终以 UID/GID 1000 运行，不能读取该文件。

### 4.3 自定义 CA

本次先保持现有可选 CA 文件入口，不把 CA 与 AK/SK 迁移绑在一起。公网可信证书不需要 CA 文件；需要企业私有 CA 的华为私有云 OBS 部署仍显式提供 CA。后续若需要把 CA 也配置内联，应使用独立的 base64 PEM 字段和同一私有 bootstrap 通道，不复用 AK/SK 字段。

## 5. 生命周期与失败处理

- 缺少或只提供一半 AK/SK：`sandbox-api` 启动失败。
- 私有 bootstrap 传输失败：Acquire 失败，FUSE 空壳按 single-use 规则销毁，不回池。
- mounter 无法创建 root-only 临时密码文件：挂载失败并销毁 runtime，不降级为 sync 或本地目录。
- credential generation 变化：沿用 release drain，旧 Pool 和活动 sandbox 清零后才启用新配置。
- teardown：先完成 durable flush 和卸载，再销毁 runtime；任何阶段都不把凭据写入恢复状态。

## 6. 部署行为

Docker 全新安装和普通升级只需要维护 `.env` 并执行：

```bash
docker compose --env-file docker/.env -f docker/docker-compose.yml up -d -t 600
```

Helm 全新安装和升级只需要维护 values 并执行现有 `helm upgrade --install`。不再增加创建 Secret 或运行准备脚本的步骤。

后端、凭据、credential generation 或 FUSE 合同变化仍遵守已有 drain 规则；简化凭据输入不改变生命周期安全要求。

## 7. 验收

- 旧 Docker `.env` 的 `STORAGE_ACCESS_KEY`、`STORAGE_SECRET_KEY` 可直接启动 sync 模式。
- 同一 `.env` 增加 FUSE 配置后，可创建、挂载、读写、flush 和销毁 Docker FUSE sandbox。
- Helm values 内联 AK/SK 后，Kubernetes sync 与 FUSE 均可工作，且动态 Pod spec 不含 AK/SK。
- 缺半组凭据、混用 inline/file、bootstrap 中断和 mounter 临时文件失败均 fail closed。
- Redis、Pool registry、Docker/Kubernetes metadata、日志和 API 响应扫描不到测试凭据。
- 升级文档不再要求 `prepare.sh`、`docker/secrets` 或预创建 Kubernetes Secret。

