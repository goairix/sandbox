# Workspace 存储与 FUSE 运维说明

## 1. 当前支持范围

一个 sandbox-api 部署只配置一个后端：

| preset | 后端 | s3fs profile |
|---|---|---|
| `minio` | MinIO / S3 兼容存储 | `minio-sigv4-path-style-v1` |
| `huawei-obs-public` | 华为公有云 OBS | `huawei-obs-public-v1` |
| `huawei-obs-private` | 2023 年部署的华为私有云 OBS | `huawei-obs-private-2023-v1` |

三个后端共用同一套镜像。部署可以只启用 sync、只启用 FUSE，或同时启用两者；两种模式读取同一组 endpoint、bucket 和 AK/SK。

API 请求只决定 `workspace_mount_mode` 和 `workspace_path`。`workspace_path` 最终规范化为对象存储 prefix，不能由请求覆盖 provider、endpoint、bucket、profile、凭据、镜像或 system egress。

## 2. Pool 与延迟挂载

sandbox-api 像维护普通 Pool 一样维护 FUSE Pool 的最小和最大数量。FUSE Pool 中的 Pod/容器已经启动，但还没有对象存储凭据、prefix、s3fs 进程或有效 `/workspace` 挂载。

Acquire 时才知道 `workspace_path/prefix`，流程如下：

1. sandbox-api 从 FUSE Pool 领取一个未使用空壳；
2. 通过 runtime 的私有 exec stdin 一次性发送 runtime identity、prefix、lease generation 和 AK/SK；
3. root mounter 在自己的 `/run/s3fs` tmpfs 内生成 mode `0600` 的 `passwd-s3fs`；
4. mounter 启动 s3fs，把后端 prefix 挂到容器或 Pod 内的 `/workspace`；
5. 探针完成写、读、删验证后，sandbox 才对外可用。

授权是一次性的。授权、挂载或探针任一步失败，整个空壳都会销毁并由 Pool 异步补充，不能卸载后回池复用。

## 3. 两个 runtime 的形态

Kubernetes 使用同一 Pod 内的 restartable init sidecar `workspace-mounter` 和普通 sandbox 容器，通过 memory `emptyDir` 的 mount propagation 共享 `/workspace`。宿主机不需要出现 `/workspace` 挂载目录，但节点必须提供 `/dev/fuse`。

Docker 使用专用 `sandbox-fuse-docker` 容器，root PID 1 只负责 mounter，公开代码执行始终以 UID/GID 1000 运行。宿主机同样不挂载 workspace，只需 Linux `/dev/fuse`。

普通 sync/code-executor sandbox 不使用 `/dev/fuse`、SYS_ADMIN、mounter 或 passwd 文件，继续走原来的创建与同步逻辑。

## 4. 凭据处理

- Docker Compose：AK/SK 直接配置为 `.env` 中的 `STORAGE_ACCESS_KEY`、`STORAGE_SECRET_KEY`。
- Helm：AK/SK 直接配置为 values 中的 `config.storage.filesystem.accessKey`、`secretKey`。
- sandbox-api 启动时读取配置并把独立内存副本交给所选 runtime。
- AK/SK 不进入 Redis、PoolKey、label、annotation、动态 Pod/容器环境、日志、API 响应或用户 exec 环境。
- prepared FUSE 空壳不含 AK/SK；凭据只在 authorize stdin、mounter 内存和 `/run/s3fs/passwd-s3fs` tmpfs 中短暂存在。
- 持久化的 bootstrap 与 mount-generation marker 已清除凭据。

因此正常部署不需要 `prepare.sh`、`docker/secrets`、workspace credential 文件或 Kubernetes workspace credential Secret。

AK/SK 轮换时应先排空需要保留的 sandbox，并递增非敏感的 `credentialGeneration`，让新 Pool 使用新的配置身份。

## 5. 自定义 CA

公网可信证书不需要额外 CA。只有 endpoint 使用企业私有 CA 时，才需要由证书签发方提供 CA bundle；项目不会生成 CA 或服务端证书。

自定义 CA 是独立的可选文件兼容入口，不用于传递 AK/SK。Helm 使用 `config.storage.filesystem.caSecretKey` 与 `workspaceCA.secretName`；Docker 自定义 CA 属于高级部署场景。没有私有 CA 时这些字段保持空值，不创建目录或 Secret。

## 6. 网络安全边界

原有用户网络规则保持不变：

- `networkEnabled=true` 且没有白名单时，可以访问公网但禁止 RFC1918、loopback、link-local 等内网地址；
- 需要访问内网服务时，调用方必须提交明确白名单；
- FUSE system egress 独立于用户网络，只允许 DNS 和当前对象存储的精确 FQDN/CIDR、端口；
- 禁止使用 `0.0.0.0/0`、通配符 FQDN或代理绕过默认拒绝。

Kubernetes 的 `cilium-fqdn` 需要 Cilium FQDN policy。Docker 使用精确 CIDR，运维需要解析 endpoint 并填写 `STORAGE_ENDPOINT_HOST_IPS` 和 `STORAGE_ENDPOINT_CIDRS`。

## 7. 后端切换与升级

Helm 通过不含凭据内容的 backend fingerprint 判断是否排空。preset、endpoint、bucket、storage identity、credential generation、system egress 或 FUSE 镜像变化时，pre-upgrade hook 先停止旧 API 并执行 release drain。

Docker Compose 没有 Helm hook。如果没有要保留的 sandbox，可以直接更新镜像/配置并 `docker compose up -d`；如果还有需要保留的 active/persistent workspace，先用旧后端配置完成 release drain。

具体命令见：

- [Helm 部署与升级](helm-deployment-upgrade.md)
- [Docker Compose 部署与升级](docker-compose-deployment-upgrade.md)

## 8. 故障定位

优先按以下顺序检查：

1. sandbox-api、mounter 和 runtime 的版本 tag 是否一致；
2. `/dev/fuse`、LSM profile 和 mount propagation 是否可用；
3. endpoint DNS、精确 system egress 与 TLS/CA 是否正确；
4. bucket、prefix 和 AK/SK 权限；
5. Redis 中的 owner/lease/Pool 状态与 teardown 日志。

出现 `runtime termination is unconfirmed` 时，系统会保留 owner/lease 并把实例视为不可复用，直到确认 exact runtime 已终止。不要直接删除 Redis 记录或强行把实例放回 Pool。

不要为了排查单个 sandbox 重启 Docker daemon；只检查或重启具体服务。daemon 重启会影响无关容器和本地 Kubernetes。
