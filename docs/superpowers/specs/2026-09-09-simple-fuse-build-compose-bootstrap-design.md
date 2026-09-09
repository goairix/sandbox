# 简化 FUSE 镜像构建与 Docker Compose 初始化设计

日期：2026-09-09

## 目标

把 FUSE 发布入口恢复成项目已有镜像的使用习惯：发布人员只指定平台、目标镜像名和版本 tag，不再准备基础镜像引用、s3fs 下载地址或 SHA-256。Docker Compose 首次安装通过一个初始化脚本创建运行所需的本地凭据文件和临时目录，并明确区分对象存储凭据、可选私有 CA 和镜像仓库认证。

本次不改变 workspace 的挂载协议、Pool 生命周期、后端 profile、网络策略或 AK/SK 不进入进程环境的安全边界。

## FUSE 镜像

### 自包含构建

`workspace-mounter` 和 Docker FUSE 镜像在仓库 Dockerfile 内完成以下工作：

1. 从当前源码编译 `workspace-mounter` 和所需的 `workspace-probe`；
2. 从上游 s3fs 仓库的固定提交 `561ce1e20600bab42133b5e51511483800d3ef76` 构建已验证的 s3fs 1.95；
3. 安装 FUSE、TLS 和 s3fs 所需运行时库；
4. 计算最终 `/usr/bin/s3fs` 的 SHA-256 并写入 profile bundle，供已有镜像自检验证。

基础镜像、s3fs 版本和提交均在 Dockerfile 中版本化。完整性校验仍属于镜像构建和发布验证的一部分，但不再是运维需要传入的 build arg。删除 `BASE_IMAGE`、`S3FS_PACKAGE_URL` 和 `S3FS_PACKAGE_SHA256` 外部参数，也不再要求基础镜像使用 `@sha256:` 才能开始构建。

### 普通 runtime 与 Docker FUSE

普通 runtime 继续不包含 s3fs 或 root supervisor。`docker/images/sandbox-fuse/Dockerfile` 继续作为独立的直接构建入口；普通 runtime 和 Docker FUSE 复用仓库内受版本控制的 runtime 安装脚本，避免复制整套 Python、Node 和工具安装逻辑，也不依赖预先发布的普通 runtime 镜像。

发布命令只保留版本相关参数：

```bash
docker buildx build --platform "$PLATFORM" \
  -f docker/images/workspace-mounter/Dockerfile \
  -t "$REGISTRY/sandbox-fuse-mounter:$VERSION" --push .

docker buildx build --platform "$PLATFORM" \
  -f docker/images/sandbox-fuse/Dockerfile \
  -t "$REGISTRY/sandbox-fuse-docker:$VERSION" --push .
```

多架构仍由现有的分架构构建和 `docker manifest` 流程组装；Dockerfile 不增加新的发布脚本或私有 CI 约定。

## Docker Compose secrets

### 目录用途

`docker/secrets/workspace` 是被 Git 和镜像构建排除的宿主机运行时目录，只包含：

- `accessKey`：对象存储 access key，必需；
- `secretKey`：对象存储 secret key，必需；
- `ca.crt`：对象存储 endpoint 使用企业私有 CA 时的 CA bundle，可选。

该目录不保存 Docker registry 凭据，也不生成服务端证书。MinIO、华为公有云 OBS 或使用公网可信证书的私有 OBS 不需要 `ca.crt`。使用企业私有 CA 时，CA bundle 必须由证书签发方或云平台提供；项目不能自行生成一个无法验证现有服务端证书的新 CA。

### 初始化脚本

新增 `docker/prepare.sh`，作为 Docker Compose 首次安装的唯一准备入口。脚本应可重复执行，并完成：

1. `docker/.env` 不存在时从 `docker/.env.example` 创建，已存在时不覆盖用户配置；
2. 交互式读取 AK/SK，secret key 输入不回显；同时支持从现有文件导入，禁止通过命令行参数或环境变量传入明文 secret；
3. 原子写入 `accessKey`/`secretKey`，目录权限设为 `0700`、文件权限设为 `0400`；
4. 可选导入 CA bundle，并拒绝空文件或不含 PEM certificate 的文件；未提供 CA 时不创建 `ca.crt`；
5. 创建 Docker FUSE credential staging root，并校验 canonical 绝对路径、`root:root` 和 `0700`；只有这一步需要时才调用 `sudo`，不要求整段初始化以 root 运行；
6. 当 `.env` 中 `SECURITY_API_KEY` 仍为空或为 `change-me` 时生成随机 API key；
7. 检查 Docker daemon、Compose、凭据文件、选定 backend 必填配置和镜像配置；
8. 执行 `docker compose config`，成功后输出唯一的启动命令。

脚本不得打印 AK、SK、API key 或 registry token。写文件失败、权限不正确、CA 无效、必填配置缺失或 Compose 校验失败时立即退出，不启动服务。

### Registry 认证

`docker/docker-auth-public/config.json` 是无认证 registry 的空配置，不属于 `docker/secrets/workspace`。使用私有 registry 时，运维只需执行一次 `docker login --config /opt/sandbox/secrets/docker registry.i.huaxisy.com`，并在 `.env` 中设置 `DOCKER_AUTH_CONFIG_FILE=/opt/sandbox/secrets/docker/config.json`。初始化脚本只检查文件存在且是合法 JSON，不复制或输出 registry credential。

## 安装与升级体验

首次安装：

```bash
./docker/prepare.sh
docker compose --env-file docker/.env -f docker/docker-compose.yml up -d
```

同一 backend、凭据和持久化路径不变的普通升级，只修改 `.env` 中五份项目镜像的版本 tag，再执行同一条 `docker compose up -d`。只有切换 backend、凭据、CA、FUSE 安全配置或存在需要 finalization 的 sandbox 时，才执行 release drain。

## 验证

- Dockerfile 契约测试确认不再要求三个外部 FUSE build arg；
- 两份 FUSE 镜像完成 ARM64/AMD64 构建检查，并验证 s3fs 版本、实际二进制 SHA 与 profile bundle 一致；
- 普通 runtime 镜像确认不包含 s3fs 和 supervisor；
- 初始化脚本覆盖首次创建、重复执行、已有 `.env`、文件导入、交互输入、可选 CA、错误权限和缺少配置；
- Compose config、sync 模式、FUSE 模式以及现有 MinIO/华为 OBS profile 测试继续通过；
- 文档不再出现要求运维填写 `BASE_IMAGE`、`S3FS_PACKAGE_URL` 或 `S3FS_PACKAGE_SHA256` 的命令。

## 与原设计的关系

本文替代主 FUSE 设计中“基础镜像 digest 和 s3fs artifact SHA 必须由 CI 作为构建参数提供”的发布接口。仍保留固定 s3fs 版本、镜像内自检、TLS 校验、profile 证据、SBOM、扫描、签名和 attestation；变化仅是把供应链输入固化进受版本控制的 Dockerfile，避免转嫁给部署运维。
