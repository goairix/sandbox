# Workspace 存储与 FUSE 运维说明

## 1. 当前支持范围

一个 sandbox-api 部署只配置一个后端：

| preset | 后端 | s3fs profile |
|---|---|---|
| `minio` | MinIO / S3 兼容存储 | HTTPS 使用 `minio-sigv4-path-style-v1`；内网 HTTP 自动使用私网 profile |
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

## 5. Endpoint、TLS 与网络安全边界

原有用户网络规则保持不变：

- `networkEnabled=true` 且没有白名单时，可以访问公网但禁止 RFC1918、loopback、link-local 等内网地址；
- 需要访问内网服务时，调用方必须提交明确白名单；
- FUSE system egress 独立于用户网络，只允许当前对象存储解析得到的精确 IP 和端口；
- 禁止使用 `0.0.0.0/0`、通配符 FQDN或代理绕过默认拒绝。

sandbox-api 在每个空壳准备时解析 endpoint，并通过 Docker `extra_hosts` 或 Kubernetes `hostAliases` 固定对象存储地址。Kubernetes 会自动读取 sandbox-api Pod 的 `/etc/resolv.conf`，只放行其中精确的集群 nameserver，供用户容器访问普通域名，不需要运维填写 DNS CIDR 或新增 RBAC。公网 MinIO 和华为 OBS 使用系统 CA；内网无证书 MinIO 设置 `useSSL=false`，且只有全部解析地址均为私网地址时才允许。DNS 结果变化时，旧的未绑定空壳会被销毁并补池。

## 6. 后端切换与升级

Helm 通过不含凭据内容的 backend fingerprint 和 cleanup protocol 判断是否排空。preset、endpoint、bucket、storage identity、credential generation、FUSE 镜像或清理协议变化时，pre-upgrade hook 先停止旧 API 并执行 release drain。

Docker Compose 没有 Helm hook。如果没有要保留的 sandbox，可以直接更新镜像/配置并 `docker compose up -d`；如果还有需要保留的 active/persistent workspace，先用旧后端配置完成 release drain。

具体命令见：

- [Helm 部署与升级](helm-deployment-upgrade.md)
- [Docker Compose 部署与升级](docker-compose-deployment-upgrade.md)

## 7. 故障定位

优先按以下顺序检查：

1. sandbox-api、mounter 和 runtime 的版本 tag 是否一致；
2. `/dev/fuse` 和 mount propagation 是否可用；Docker 未配置自定义 LSM 时会显式使用 `apparmor=unconfined`；Kubernetes 的 `Localhost` AppArmor profile 必须预先加载到所有可能调度 FUSE Pod 的节点，Chart 不负责安装；若事件出现 `apparmor profile not found`，先安装该 profile，验证环境才可临时清空 `lsmProfile` 并启用 `allowMissingLSMForKind`；
3. sandbox-api 是否能解析和访问 endpoint，TLS 开关是否正确；
4. bucket、prefix 和 AK/SK 权限；
5. Redis 中的 owner/lease/Pool 状态与 teardown 日志。

新版 mounter 会在创建失败信息中返回一个不含 endpoint、prefix 或凭据的固定类别：

| 类别 | 优先检查 |
| --- | --- |
| `fuse-permission` | Linux 主机是否存在可用的 `/dev/fuse`，容器是否获得 Compose/Pod 模板中声明的 FUSE 权限 |
| `endpoint-dns` | sandbox-api 解析 endpoint 的结果、容器或 Pod 的固定 host 映射 |
| `endpoint-tls` | `STORAGE_USE_SSL`/preset 是否正确，endpoint 证书链是否被系统 CA 信任 |
| `storage-auth` | AK/SK、签名版本、region 与 endpoint 是否匹配 |
| `storage-bucket` | bucket 名称以及账号对该 bucket 的访问权限 |
| `s3fs-exited` | s3fs 在挂载前退出但未匹配到以上类别；检查对应 FUSE runtime 容器或 mounter sidecar 状态 |

这些类别由 mounter 对有界 stderr 做本地分类；原始 stderr 不会进入 API 响应或普通日志。排查时不要把 AK/SK、完整 `docker inspect` 或带签名 URL 的输出粘贴到工单。

Kubernetes FUSE Pod 带有 `sandbox.huaxisy.com/fuse-runtime-cleanup` finalizer。删除期间 Pod 会先进入 `Terminating`，kubelet 确认 init/native-sidecar、普通和 ephemeral container 全部退出后，sandbox-api 才把 exact UID 的终止证据写入 Redis、移除 finalizer 并清理策略。进程在任一步重启都会从 Pod status 与 Redis cleanup phase 继续。

出现 `runtime termination is unconfirmed` 时，系统会保留 owner/lease 并把实例视为不可复用，直到确认 exact runtime 已终止。Pod 已经 NotFound 但 cleanup 记录没有终止证据时仍会 fail-closed；这通常表示旧协议清理中断、finalizer 被外部移除或节点状态无法确认。应使用 infrastructure fencer 或审计 Pod UID、节点和 kubelet 事件后修复，不要直接移除 finalizer、删除 Redis 记录或强行把实例放回 Pool。

不要为了排查单个 sandbox 重启 Docker daemon；只检查或重启具体服务。daemon 重启会影响无关容器和本地 Kubernetes。
