# Sandbox 镜像构建简化设计

## 背景

`main` 的发布习惯是分别执行普通的 `docker buildx build` 构建项目镜像，再把新镜像引用更新到 Helm values 或 Docker Compose 配置。当前 FUSE 分支要求运维先在宿主机执行 `go build`、创建临时 build context、复制二进制和 profile bundle，增加了不必要的发布步骤，也容易造成源码 revision、目标架构和镜像内容不一致。

## 目标

- 每份项目镜像都通过一条独立的 `docker buildx build` 命令完成构建和推送。
- 运维不再手工编译或复制 `workspace-mounter`、`workspace-probe`。
- 构建后只需要更新 Helm values 或 Docker Compose 环境中的镜像引用，再执行原有部署命令。
- 五份项目镜像使用同一个人工指定的发布版本 tag，例如 `v0.2.12`；部署配置直接引用该版本 tag。
- MinIO、华为公有云 OBS、华为私有云 OBS 共用同一份 mounter 镜像和同一份 Docker FUSE 镜像。
- 保留基础镜像 digest、s3fs artifact URL/SHA256、profile bundle 和镜像自检等现有安全约束。

## 方案

### Dockerfile

`docker/images/workspace-mounter/Dockerfile` 使用多阶段构建：Go builder stage 从仓库源码构建 `workspace-mounter`，最终 stage 安装该二进制、s3fs 和公共 profile bundle。

`docker/images/sandbox-fuse/Dockerfile` 使用多阶段构建：同一个 Go builder stage 从同一源码 revision 构建 `workspace-mounter` 和 `workspace-probe`，最终 stage 同时安装两者。

普通 runtime 继续在自身 Dockerfile 的 builder stage 构建 `workspace-probe`。API 和 gateway 保持现有构建方式。

需要读取 Go 源码的 Dockerfile 统一使用仓库根目录作为 build context；不需要源码的 gateway 可继续使用自身目录作为 context。运维不需要创建临时目录或生成构建产物。

### 发布流程

文档只保留五类镜像的直接构建命令：

1. sandbox-api；
2. ordinary runtime；
3. Docker gateway（仅 Docker runtime 需要）；
4. Kubernetes workspace mounter（仅 Kubernetes FUSE 需要）；
5. Docker FUSE runtime（仅 Docker FUSE 需要）。

构建命令直接指定 `--platform`、Dockerfile、镜像 tag 和 `--push`。FUSE 镜像额外传入经过批准的基础镜像与 s3fs artifact 参数，这些是构建输入，不再通过宿主机预处理步骤组装。

发布版本由运维显式设置，不从 Git commit 自动派生，也不在部署镜像名中增加架构后缀：

```bash
VERSION=v0.2.12
REGISTRY=registry.i.huaxisy.com/library/ai-infra
```

对应的部署镜像为：

```text
registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.2.12
registry.i.huaxisy.com/library/ai-infra/sandbox-runtime:v0.2.12
registry.i.huaxisy.com/library/ai-infra/sandbox-gateway:v0.2.12
registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-mounter:v0.2.12
registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-docker:v0.2.12
```

多架构镜像由现有 `docker manifest` 发布流程组装，本次调整不新增或替代该流程。构建说明只负责生成各架构镜像；Helm values 和 Docker Compose 始终引用最终的统一版本 tag，例如 `:v0.2.12`。

发布完成后：

- Helm：更新 API、ordinary runtime、mounter，以及实际需要的 gateway/Docker FUSE 镜像字段，再执行 `helm upgrade`。
- Docker Compose：更新 API/ordinary runtime/gateway/mounter/Docker FUSE 对应镜像字段或环境变量，再执行 `docker compose up -d`。

部署工具不承担镜像构建职责。Compose 中的 `sandbox-images` 只负责确认或拉取已配置镜像；兼容未配置镜像的本地开发默认值，但生产发布不依赖该 helper 构建项目镜像。

## 兼容性与安全

- Go builder 和最终基础镜像按生产要求使用 digest-pinned 引用；这里约束的是 Dockerfile 的基础镜像，不要求 Helm/Compose 把项目发布镜像的版本 tag 改写成 digest。
- mounter 与 probe 必须由同一 build context、同一源码 revision 构建。
- s3fs artifact 必须使用无凭据、无 query/fragment 的 HTTPS URL，并在镜像内校验 SHA256。
- profile bundle 固定从仓库路径复制，并继续执行镜像自检。
- 不改变 FUSE Pool、延后挂载、workspace prefix、网络策略或 backend preset 行为。

## 验收条件

- 文档不再包含 `mktemp` build context、宿主机 `go build` 或手工复制二进制步骤。
- 五类镜像均有一条可独立执行的 `docker buildx build` 示例。
- 构建示例使用人工指定的 `VERSION=v0.2.12`，不使用 Git SHA 或 `${VERSION}-${ARCH}` 作为部署 tag。
- Helm values 和 Docker Compose 示例使用统一的 `:v0.2.12` 镜像引用；多架构 manifest 组装不纳入本次实现。
- mounter/Docker FUSE Dockerfile 可从仓库根 context 自行编译所需 Go 二进制。
- Helm 与 Compose 文档只要求回填镜像引用并执行升级命令。
- `scripts/test-fuse-images.sh`、Helm 测试、Compose config 校验和相关 Go 测试通过。
