# etcd 状态容量与发布验收（进行中）

当前结论：**未通过 10 万存活沙盒容量验收，也未完成新部署切换**。以下仅是活跃仓库的元数据测量，用于定位高 N 维护成本；不包含真实 sandbox、Pod、FUSE、配额、HTTP 或 Redis 对照。

## 已测范围

环境为本机 Docker Desktop 中的单节点 etcd v3.6.14，loopback 明文连接，独立测试前缀。每个合成沙盒写一条 publishing 记录，再激活；16 个固定写入 worker。快照只有 `{"ready":true}`，远小于真实业务快照。扫描使用生产仓库的 `Scan(cursor, 128)`，定点读取抽样 100 次。

命令：`TEST_ETCD_ENDPOINT=127.0.0.1:64104 TEST_ETCD_SCALE_N=<N> go test -v ./internal/storage/state/etcd -run '^TestActiveResidentScale$' -count=1 -timeout=180s`。Go 构建和模块缓存复用主仓库；测试没有启用 race，结果不能替代并发正确性回归。

| 存活记录 | 前缀键数 | 16 worker 写入 | 一次完整 Scan | 定点读取 p95 | 单节点 DB 增量 |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 10,000 | 20,000 | 2.474 s | 0.207 s | 0.272 ms | 12.97 MB |
| 100,000 | 200,000 | 23.685 s | 4.886 s | 0.268 ms | 129.90 MB |

## 阻塞结论

现有生命周期协调器每 5 秒遍历所有活跃记录。10 万条小快照的**纯存储扫描**已用 4.886 秒；真实快照解码、恢复、清理、TLS/Kubernetes 调用和多副本扫描只会增加负载。因而不能把 10 万存活沙盒归为已支持，更不能用快速定点读取掩盖周期性全量扫描。

下一步须让周期维护只读取待处理状态和到期项，且到期索引与原记录更新保持同一事务和相同栅栏。索引发布、续期、删除、ACK 丢失和重启恢复均需真实 etcd 反例验证；不能靠本地时钟或进程缓存直接授权退款。完成后再测真实 TTL 分布、集中到期、多副本、三成员故障及业务 Pod/FUSE。当前没有这些数据，也没有可声明的单 cell 上限。

新部署方面，Chart 已有可选明文或 mTLS 的 etcd 模式，渲染不包含 Redis 资源或环境变量；下面的 DS 状态层安装不等于 API 与业务切换，真实运行时启动、物理 TTL 退款和 Redis 退役仍未验收。

## 本机明文 etcd 整进程启动（2026-10-25）

在当前分支编译 `./cmd/sandbox`，连接独立的单节点 etcd v3.6.14
（`127.0.0.1:64105`，无 TLS、用户名和密码）。Docker 运行时、local
文件系统、`pool.min_size=0`、sync 工作区用于限定此启动检查的范围。
共享主仓库 Go 构建和模块缓存；二进制 SHA-256 为
`8ac79c869e3f1231e49ce2f82eea92206fe68fbdb6aa86c71150018c36fa4215`。

实际进程两次启动都记录 `session store connected to etcd`，两次
`GET /ready` 均为 200。首次 `GET /health` 为 200，未认证的沙盒请求为
401，带测试 API key 的不存在沙盒请求为 404。首次 SIGINT 后进程记录
`shutdown complete` 并退出 0；同一配置和 etcd 前缀重启后再次 ready，
第二次 SIGINT 也正常退出。独立 etcd 容器已停止并自动删除，已审计的
123,933,186 字节可重建二进制已清理。

这证明当前可选无认证配置能穿过真实主程序启动、HTTP 就绪和重启路径，
**不证明** Kubernetes 分布式生命周期、实际沙盒创建、FUSE 物理清理、
配额退款、三成员故障或大量存活沙盒容量。集群安装仍未执行。

## arm64 发布镜像纠错与 DS 启动探针

首次以共享 Go 缓存编译 `-trimpath` 的 `./cmd/sandbox`，SHA-256 为
`cd7818a7f9d7faf712ff0fce620fcd1a2e9179e978fbe65db8fffc67064248d5`。
该二进制实际是 **macOS Mach-O arm64**。将其与 `configs/config.yaml` 放入临时构建上下文，使用
`docker/Dockerfile.prebuilt` 打包，没有在 Docker 内重新下载 Go 工具链或
模块。镜像 `sandbox-etcd-reset:local-smoke` 的 ID 为
`sha256:a13b5797d82e7c263e481265b979472d5ab9332d359c4de094e2faacac340cc4`，
架构为 `linux/arm64`，大小 163,316,128 字节；容器内 `/app/sandbox`
的 SHA-256 与构建输入完全一致。镜像平台标签及内容哈希都未验证可执行文件格式，
因此该镜像不可运行。错误标签
`registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-api:v1-reset-33620a9-arm64`
已推送，但不得部署；它在错误 kubeconfig 下的单个探针 Pod 返回 `exec format error`，
该 Pod 已删除。该配置的 context 是 `internal`，API 为 `172.16.30.240:5443`；
不能将它称为 `ds-ai-research`。

首次使用 Docker Hub `alpine:3.22` 时，匿名令牌请求连接被重置，构建失败。
核验内部镜像仓库的 `library/alpine:3.22` 同时提供 arm64 和 amd64
清单后，改用该明确基础镜像，完整构建和不带覆盖参数的重复构建均通过。
纠错提交 `5579225` 在镜像构建时检查 Linux ELF 魔数及目标架构的 ELF machine。
原 Mach-O 输入实际构建失败；用 `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`
重新编译后，ELF 二进制 SHA-256 为
`f6e59b7ef5ae8581874e3538573203d3829f3140a6f2d9bec9ce0c70f5dbad39`，
构建通过且容器中运行 `/app/sandbox --help` 退出 0。新标签
`registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-api:v1-reset-5579225-arm64`
的 registry digest 为 `sha256:6e65df74f0a63aaa07bb54adf2d8b07ec1e3a5d7b6d999db47f01721d6cb0e57`。

正确的 DS kubeconfig 为 `var/tmp/k8s-test/ds-ai-research.yaml`，context 为
`ds-ai-research`，API 是 `https://kube.i.huaxisy.com/k8s/clusters/c-dx2c8`。
按已授权的全新部署要求，删除其旧 `sandbox-fuse-etcd` 命名空间；仅从已处于
Succeeded/Failed 且正在删除的旧 Pod 移除阻塞 finalizer，再删除绑定旧 namespace
且处于 Released/Retain 的 13 个 PV 对象。确认没有 Pod 引用旧宿主目录后，
仅删除 worker1/2/3 上旧 namespace UID
`4570b61f-eeaa-420b-94dc-f9e6df620805` 对应的宿主目录。重建 namespace 后 UID 为
`d6757bda-154f-46c6-bdb1-31f1bcb1eddd`，旧 Helm release 和工作负载均已不存在。

在 **正确 DS** 命名空间运行新镜像的 `--help` 探针，Pod 被调度至
`ds-ai-worker-1`，镜像成功拉取，进程退出 0；探针 Pod 和对应事件已删除。
最终仅保留 Kubernetes 自动创建的 `kube-root-ca.crt` ConfigMap、`secret-edge`
拉取凭据和默认 ServiceAccount。此检查只证明镜像可在 DS arm64 节点启动，
**没有**安装 etcd、MinIO 或 API Deployment，也没有证明实际沙盒创建和容量。

## 正确 DS 集群的独立 etcd 状态层

在上述镜像探针之后，于 `ds-ai-research` 的新 `sandbox-fuse-etcd` 命名空间
安装 etcd v3.6.14 三成员集群。实际使用的隔离部署输入为
`.superpowers/sdd/2026-10-24-etcd-state-replacement/ds-etcd.yaml`，
SHA-256 `a9285cd4f62a0a888cf7e25e1b1660bb29ffc9d3dae8d8e41e8c2457c5fc7ce8`。
来源镜像 `quay.io/coreos/etcd:v3.6.14` 的 arm64 manifest digest 为
`sha256:cbbd3719e2760b0d4d87dada77f5a2f5869acec9b57e44efcc308f8a49e2f4b9`，
镜像已镜像到内部仓库的 `sandbox-fuse-etcd-etcd:v3.6.14-arm64`。
采用 [etcd 官方 StatefulSet 静态引导方案](https://etcd.io/docs/v3.6/op-guide/kubernetes/)；
DS 仅有的默认 StorageClass 是 NFS，因此为本次测试将三个独立 Retain PV
固定到 worker1/2/3 的不同宿主目录，未把三个成员的数据写到同一 NFS 卷。
etcd 服务采用集群内明文、无账号模式，并以同命名空间 ingress NetworkPolicy 限制访问。

三个 Pod 均 Ready、重启数 0，分别运行在 `ds-ai-worker-1/2/3`，
三个 PVC 均 Bound 到对应 PV；成员列表显示三位 started，三个 endpoint
健康检查均为 true。经 etcd-0 写入一条隔离探针键、etcd-1 读回后，
删除 etcd-2 Pod 并等待 StatefulSet 重建；etcd-2 在原 worker3 上从其原卷读回同一键，
随后删除该探针键。这只证明单成员 Pod 重建与三成员基本读写，
**不证明**节点故障恢复、备份恢复、持续吞吐量或 10 万存活沙盒容量。
MinIO 和 API Deployment 在该状态层检查时尚未安装，发布切换未完成。

## 正确 DS 集群的对象存储与 API 首次启动

在同一新命名空间独立创建测试 MinIO：镜像为此前固定源码版本构建的
`sandbox-fuse-etcd-minio:v1-9e49d5e7-arm64`，其内部仓库 arm64 manifest digest 为
`sha256:e03aba1afec13d5f8676d4e68f962121f793d5dd004ca3c97f239cc23fb73d21`。
部署输入 `.superpowers/sdd/2026-10-24-etcd-state-replacement/ds-minio.yaml`
SHA-256 为 `d823ea2c27536f26cb712cb5ad2c1c8f1f8202c96cf9d9f67bd01296dd2f33c4`；
它使用 worker4 的独立 Retain PV、单副本、同命名空间 ingress 策略，
并以 Kubernetes Secret 提供本次随机生成的测试凭据。Deployment 为 1/1，
经本地端口转发检查 `/minio/health/ready` 为 200。这是测试对象存储，
没有执行 S3 数据或持久化故障恢复验收。

首次 Helm 安装的 API 镜像虽为 Linux ELF，却把仓库的 Redis 示例
`configs/config.yaml` 打包进了容器。API 按预期拒绝 Redis 与 etcd 同时配置，
因此该首次启动失败；不得把 Helm 安装的资源创建当作服务就绪。
提交 `ba30229` 改为专用 `docker/config.etcd.yaml`，只声明 etcd backend，
由 Helm 环境提供实际端点、运行时和 Secret。修复镜像标签是
`sandbox-fuse-etcd-api:v1-reset-ba30229-arm64`，registry digest
`sha256:5a3fe325a4c676746dba2e0d5b8f5917d6c5c7ff21432f1a3dd1f10d225aa89d`。

渲染检查还发现旧 Chart 把对象存储 AK/SK 直接写成 Deployment 环境值；
第一组测试凭据曾进入检查输出和 Helm revision 1。立即轮换 MinIO 测试凭据、
重启 MinIO，并以提交 `5ac0f58` 增加 Secret 引用及禁止与明文值混用的模板校验。
删除含旧值的本地渲染文件和已废弃的 Helm revision 1 Secret；当前 revision 2
的 API 环境只含 `ds-minio-client` 的 `secretKeyRef`，未写明文凭据。
`ds-ai-research.yaml` kubeconfig 和本地新凭据文件权限均已收紧为 0600。

Helm overlay `.superpowers/sdd/2026-10-24-etcd-state-replacement/ds-api-values.yaml`
SHA-256 为 `755ef4252d2fe375c7810cbdfebf4cd4f80d70dff6c2394e4712943a4ef8d9fc`。
当前正确 DS 上 Helm release `sandbox` 为 revision 2、`deployed`；
API 1/1，etcd 3/3，MinIO 1/1。经端口转发，`GET /ready` 和 `/health` 均为 200，
未认证的沙盒查询为 401，带测试 API key 查询不存在的沙盒为 404。
Helm 配置使用 etcd 三 endpoint、独立 `/sandbox/ds-reset/` 前缀、无认证连接，
普通池最小值 0、仅启用 sync mount，渲染检查没有 Redis 资源。
这仅是 Kubernetes 内服务启动与只读 HTTP 烟测；**尚未**实际创建或销毁沙盒、
完成自然 TTL 退款、启用 FUSE、验证大量存活 N 或进行生产容量验收。

## 正确 DS 集群的单个普通沙盒创建销毁烟测

内部仓库的 Chart 默认 `sandbox-runtime:v0.2.12` 与
`sandbox-gateway:v0.2.12` 均不存在，因此本次只将普通运行时镜像临时设为
内部仓库 Alpine 3.22 的 arm64 不可变 digest，验证最短的 etcd 状态与
Kubernetes 创建销毁路径。该镜像缺少正式沙盒依赖，不能用于执行、文件、
网络网关或 FUSE 验收。测试时命名空间默认 ServiceAccount 显式引用已有
`secret-edge` 拉取凭据；Helm revision 3 使用临时镜像，测试后将 Helm 值
恢复为默认配置，不能把临时镜像留作业务运行时。

在 `ds-ai-research`、`sandbox-fuse-etcd` 中，以测试 API key 对
`POST /api/v1/sandboxes` 发送 `ephemeral`、120 秒、网络关闭的请求，
返回 201，逻辑 ID `sandbox-4wp1zo7jjh`，状态 `ready`，运行时 Pod
`sandbox-pool-qmtyr6i44s`。随后 GET 返回 200/ready；DELETE 返回
200；再次 GET 返回 404。经 Kubernetes API 检查，该次请求的 Pod 和
NetworkPolicy 均已删除。API key 未写入报告或请求日志。

这证明本次配置下单个普通沙盒的公开创建、查询和销毁路径能够跨越 API、
etcd 和 Kubernetes。它不证明正式运行时镜像、自然 TTL、配额退款、
FUSE、同时大量存活沙盒、三成员故障或容量目标。Chart 默认的
`v0.2.12` 标签在仓库中缺失；后续发现有曾用于 DS 的发布版本，见下一节。

## DS 发布运行时的执行、自然 TTL 和 API 重启验证

内部仓库存在 `sandbox-runtime:v0.3.39` 的 arm64 清单，digest 为
`sha256:a90d72055cf7b2e57d822d68cb4e3e4b42c302b4ac09f8f00592d569946f34c9`。
将 DS 专用 Helm 值中的普通沙盒镜像固定到该 digest，Helm revision 5
部署成功；值文件 SHA-256 为
`e5c803448b91f77375040bbb0b5a25b62f9f8c90397d600a4d45ee866d58e285`。
本次沿用此前已有的 `secret-edge` 拉取凭据，不修改镜像内容。

第一条普通沙盒 `sandbox-pkpwn9dlfi` 经公开 POST 返回 201/ready，
运行时 Pod `sandbox-pool-2colw0qp04` 实际使用上述 digest。
公开 Python Exec 返回 200、退出码 0、标准输出 `etcd-ds-runtime-ok`；
DELETE 返回 200，随后 GET 404，Pod 和 NetworkPolicy 不再存在。

第二条普通沙盒 `sandbox-l3f4r5txgk` 配置 30 秒 TTL。创建返回 201；
到期附近公开 GET 先返回 503（关闭中），随后返回 404，
Pod 和 NetworkPolicy 均消失。这是一次自然到期正例，未单独核对
持久配额流水或模拟 ACK 丢失，不能概括成完整退款验收。

第三条普通沙盒 `sandbox-vmtxo2jvzi` 存活时，API Deployment 进行一次
滚动重启，Pod UID 从 `a8296bbe-eb4e-4888-b480-2141f41d760f`
变为 `4f9d0863-cc70-45d9-a7b0-ec7369287d2f`。新 API Pod 经公开
GET 读回 ready 和原运行时 ID，Python Exec 返回 200、退出码 0、
输出 `after-restart`；DELETE 返回 200，随后 GET 404，资源消失。
这验证了单副本进程重启后的普通沙盒恢复，不等于两个 API 副本并发竞争
或 etcd 成员/节点故障恢复。

DS 当前 etcd 3/3、MinIO 1/1、API 1/1；测试结束后没有受管沙盒 Pod
或其 NetworkPolicy。FUSE、实际对象存储文件操作、配额退款故障矩阵、
大量存活 N 和三成员故障仍未通过验收；上文的 10 万记录全量扫描瓶颈
仍是容量阻塞。
