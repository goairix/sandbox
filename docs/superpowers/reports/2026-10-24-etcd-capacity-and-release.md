# etcd 状态容量与发布验收（进行中）

当前结论：**DS 全新部署的 20 个 FUSE 沙盒灰度功能验收已有真实正例；
10 万存活容量与大存量发布验收另设门槛，尚未通过**。用户确定首发先按
每 cell 20 个 FUSE 沙盒灰度，大存量单独验收。`ds-ai-research/sandbox-fuse-etcd`
已重建为全新 namespace；当前 Helm release revision 3 为双 API、三成员
etcd 和独立测试 MinIO，以 FUSE 为默认挂载模式，保留显式 sync 入口。
下文历史故障与旧 revision 状态按发生顺序保留，不能覆盖本节当前状态。
下面第一节仅是本机元数据测量，不能与后续 DS 真实 FUSE 功能烟测混同。

| 发布门 | 当前证据 | 结论 |
| --- | --- | --- |
| 新部署的 Redis→etcd 接线 | DS 全新 namespace 正式 revision 3：FUSE 双 API 2/2、etcd 3/3、MinIO 1/1；新前缀无旧状态，Chart 不安装 Redis | DS 功能接线通过，未形成大规模容量结论 |
| FUSE 20 个灰度 | 正式 Service 上 20/20 驻留及公开删除、20/20 自然 TTL 回收；文件读写、API 重启和单次 etcd leader 重建有独立正例；结束后 owner/Pod/实例策略和池汇总回到 0 | DS 有界功能测试通过；超过 20 的存活规模未验收，但不作为请求拒绝阈值 |
| 大量存活 N | 10 万普通元数据的闲时索引查询已测；修复每次续租都 Grant/Revoke 后，本机三成员、双客户端且配置全部三个 endpoint 的 6,000/10,000 条产品 controller 短时驻留均通过；完整 Pod/FUSE、集中到期和双副本故障负载未测 | 10 万门仍未通过；本机 10,000 条 controller 短测不是 DS 或完整沙盒容量验收 |
| 远端成果 | `feat/etcd-state-management` 已快进推送至内部 `hxsy` 远端；GitHub SSH 远端仍不可用 | 可在内部远端审查，尚未合并 |

### 首发灰度的证据边界（2026-10-25）

本轮 DS 单 cell 使用 20 个 FUSE 沙盒作为功能灰度测试规模，**不是**
第 21 个请求的拒绝阈值。用户明确表示超过 20 不必拦截，大存量另行
验收。当前受控测试批次已经完成；持续业务流量尚未接入。DS 的 API
Service 是 ClusterIP，没有对外 Ingress；现有 `sandbox-api-ingress`
NetworkPolicy 的 `ingress: [{}]` 允许集群内任意来源连接，API key
负责认证。它们都不限制同时存活数。Helm 的
`workspace.fusePool.maxSize=20` **只限制准备中及已准备的
FUSE 池记录**；已消费的沙盒不计入此值，不能把它当作 20 个存活沙盒的
产品准入硬限制。`pool.maxSize=20` 也只是普通预热池配置。后续放量时
记录实际存活数、创建与清理结果；超过 20 的结果作为新证据单独评估。

停止放量并调查的条件是：任一创建、文件读写或释放持续失败；到期或
公开删除后受管 Pod、owner、池记录不能归零；API/etcd 失去健康多数；
或者存活数增长时延迟与失败明显恶化。对不确定状态保持占用，不通过
手工删除状态键或强制退款来恢复容量。恢复发布须重新验证同一业务主链。

独立审查发现 `guardFUSEPoolRecord` 仍对 session、ephemeral 和 workspace
owner 做随存活数增长的检查；周期维护与启动恢复也有按 N 增长的路径。
`minSize=0` 避开常规预热，但不消除清理时的这些扫描。因此本次 20 个
灰度正例不能外推到 10 万存活。大存量单独验收需要先改为有界索引/到期
调度，再测周期维护、集中到期、恢复与多副本负载。

### 分支交付核对

当前 `feat/etcd-state-management` 从 `feat/workspace-fuse-mount` 分出；
基分支没有尚待合入的新提交。当前树的 `go test ./...`、`go vet ./...`、
`go test -tags helmtests ./internal/helmtest` 和 fresh-install Helm overlay
的 `helm lint` 均退出 0。DS release revision 3 仍为 API 2/2、etcd 3/3、
MinIO 1/1 Ready；当前无受管 sandbox Pod。以上验证不替代大存量验收。

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

新部署方面，Chart 已有可选明文或 mTLS 的 etcd 模式，渲染不包含 Redis
资源或环境变量。下文早期 DS 状态层安装当时不等于 API 与业务切换；
正式 FUSE revision 1 的新证据见文末，不能从早期状态推断当前部署。

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
删除含旧值的本地渲染文件和已废弃的 Helm revision 1 Secret；当时 revision 2
的 API 环境只含 `ds-minio-client` 的 `secretKeyRef`，未写明文凭据。
`ds-ai-research.yaml` kubeconfig 和本地新凭据文件权限均已收紧为 0600。

Helm overlay `.superpowers/sdd/2026-10-24-etcd-state-replacement/ds-api-values.yaml`
SHA-256 为 `755ef4252d2fe375c7810cbdfebf4cd4f80d70dff6c2394e4712943a4ef8d9fc`。
该阶段正确 DS 上 Helm release `sandbox` 为 revision 2、`deployed`；
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

该阶段 DS 的 etcd 3/3、MinIO 1/1、API 1/1；测试结束后没有受管沙盒 Pod
或其 NetworkPolicy。FUSE、实际对象存储文件操作、配额退款故障矩阵、
大量存活 N 和三成员故障仍未通过验收；上文的 10 万记录全量扫描瓶颈
仍是容量阻塞。

## DS 双 API 副本的跨实例与同 workspace 申请

将 API 扩至两个就绪副本，并分别端口转发到明确的两个 Pod，避免经 Service
负载均衡时无法判定请求实际落点。A 副本创建普通沙盒
`sandbox-twqhevjsa1` 返回 201；创建后 2.48 秒 B 副本查询返回
200/ready，2.92 秒在 B 执行 Python 返回 200/退出码 0；7.42 秒
B 销毁返回 200，随即查询 404，受管 Pod 和 NetworkPolicy 均消失。
先前一次 180 秒 TTL 的实验在 B 查询时已过期，返回 404；Kubernetes
事件显示 Pod 正按 TTL 退出，因此该次不计入跨副本失败或成功。

随后在两个副本同时申请同一个 sync workspace。首次部署值的 FUSE-only
模板未向 sync-only API 提供 `storageIdentity`，两个请求均在创建 Pod 前
返回 503/invalid workspace lease。提交 `58f349b` 添加 sync-only 的
backend identity、preset 和 profile 渲染测试与模板接线；第一次真实升级
又发现 HTTP MinIO 的 profile 仍按 HTTPS 输出，API 以固定映射校验拒绝启动。
该错误升级 revision 7 超时，后续 revision 8 的 drain hook 读取错误模板
失败，短暂将 API 缩至 0。使用 Helm revision 6 成功回滚，恢复两个旧 API；
提交 `5f66c28` 修正 HTTP MinIO profile 选择，完整 Helm 测试与 lint 通过，
再升级到 revision 10，API 2/2 就绪。没有放宽配置校验或在失败升级期间
发起业务请求。

修复后，同时向两个不同 API Pod 申请
`etcd-reset/ds-workspace-race-02`，均返回 201，同一逻辑 ID
`sandbox-8tzt4cgkof` 和同一运行时 Pod；一个 `reused=false`，另一个
`reused=true`。公开 DELETE 返回 200、GET 返回 404，受管 Pod 和
NetworkPolicy 消失。再次在另一副本申请相同路径，返回新的逻辑 ID
`sandbox-hwm8df6atm`、`reused=false`；再次销毁返回 200，资源清空。
这是一次两副本、单路径并发正例，尚未覆盖 ACK 丢失、节点故障、
大量不同 workspace 或对象存储文件内容的完整性。

该轮结束时 DS Helm revision 10 为 `deployed`，API 2/2、etcd 3/3、MinIO 1/1；
值文件保留发布运行时的 arm64 digest 与两副本设置。上述单项成功不能
替代 10 万存活沙盒容量、完整 FUSE 或生产发布验收。

同一环境又验证了一次 sync 文件往返：公开创建
`etcd-reset/ds-file-roundtrip-03` 对应的沙盒 `sandbox-roaex7ylj1`，
在运行时 `/workspace/probe.txt` 写入 `ds-etcd-sync-roundtrip-03`；
`POST /workspace/sync` 的 `from_container` 返回 200，销毁返回 200。
重新申请同一路径得到新的沙盒 `sandbox-3r9x55itbb` 和运行时 Pod，
其 Python Exec 从该文件读回相同内容；最后再次销毁返回 200，
受管 Pod 与 NetworkPolicy 为空。测试对象保留在独立 DS MinIO
的该专用前缀中，未触碰其他工作区。
这是单文件、单次同步的正例，不证明并发写入、对象存储故障恢复或
FUSE 挂载链。

## 升级排空前的配置校验

revision 8 曾先将 API Deployment 缩到 0，再读取旧模板中的错误 MinIO
profile，造成短暂不可用。根因是 `--drain-release` 在配置加载之前执行
Kubernetes 缩容。提交 `fe18eee` 将现有配置的解析和校验放在任何 HPA 删除、
Deployment 缩容之前；配置无效时，fake Kubernetes 测试确认副本数保持 2，
且没有 update/delete 操作。`go test ./cmd/sandbox -count=1`、
`go vet ./cmd/sandbox` 和 `git diff --check` 均通过。

修复版 Linux arm64 ELF SHA-256 为
`e3f924f82d092d0c50389253b4659d1208ccac7f262dcde57b9ea10464998179`；
内部仓库标签 `sandbox-fuse-etcd-api:v1-reset-fe18eee-arm64` 的 digest 为
`sha256:e75b75f5890b1e91cbe172e4de5423c16106a29396a8b2fab1b712371dd477c4`。
仅使用 `ds-ai-research` kubeconfig/context 以同后端 Helm upgrade 发布到
revision 11；release 为 `deployed`，API 2/2 Ready，端口转发的
`/ready`、`/health` 均返回 200。此升级没有触发 release drain，
因此还没有在真实集群人为制造错误配置来测试新拒绝分支；容量和 FUSE
仍按前述未验收处理。

## DS 三成员中的单成员重建

在 revision 11 的双 API 副本下，以公开 POST 创建 600 秒普通沙盒
`sandbox-sbq2hz6kgk`，返回 201。保持该沙盒存活时，仅删除 etcd-2 Pod
让 StatefulSet 原位重建；其 UID 从
`20ef2f6f-d9eb-43f5-85f0-a41db5e8a750` 变为
`2f7933eb-546a-4a07-ab2d-20ac70b5847b`，恢复 Ready 后 StatefulSet
为 3/3。随后公开 GET 返回 200、Python Exec 返回 200 且输出固定探针值、
DELETE 返回 200；受管 Pod 和 NetworkPolicy 均为空。

脚本位于忽略目录
`.superpowers/sdd/2026-10-24-etcd-state-replacement/ds_etcd_member_restart_smoke.py`，
SHA-256 为 `accce55800755ba839bd8f6cc4e6ba6522dd6d1d05ef52cb82a4e7065e479ef6`。
API key 只从本地受保护文件读取，未写入输出。此结果只覆盖三成员集群中
一个成员的 Pod 重建；没有使节点失联、丢失 quorum、恢复卷或验证多成员
故障，更没有解决 10 万存量记录的周期扫描瓶颈。

## DS 双 API 重启期间的自然 TTL

在 revision 11 的 DS 环境中创建 90 秒普通沙盒
`sandbox-lherr55e2t`，公开 POST 返回 201。沙盒存活时滚动重启
`sandbox-api` Deployment，等待两个新副本都 Ready；随后公开 GET
依次观察到 200、503（关闭中）、404。确认该命名空间没有受管 Pod
或 NetworkPolicy，API Deployment 最终为 2/2 Ready。运行脚本
`.superpowers/sdd/2026-10-24-etcd-state-replacement/ds_api_restart_ttl_smoke.py`
的 SHA-256 为
`0e04b72cc0e8bdd06bda1686793a2c33847aa1d37e06e6f8cf6186cc29d2e599`；
测试密钥仅从本地受保护文件读取。

这是一次重启与自然 TTL 交错的正例，补充前述“重启后手工销毁”和
“无重启自然 TTL”实验。它未模拟 etcd ACK 丢失或物理删除失败，
也未独立核对配额退款流水，不能将其判为完整故障恢复或退款验收。

## 已知 ID 的一次性执行清理

`ScheduleDestroy` 已知精确 sandbox ID，旧实现却仅发一个无 ID 唤醒信号，
协调器收到后再次遍历全部活跃记录。提交 `63ebccc` 将本地有界唤醒队列改为
精确 ID：直接 Load 并走原有 `reconcileActiveLifecycle`；队列满、跨副本写入、
重启及通知丢失仍由原周期扫描恢复。测试先因类型不匹配呈红，再确认一次
ScheduleDestroy 只加载该 ID、不调用全量 Scan；`internal/sandbox` 全包测试、
该测试的 race、`go vet ./internal/sandbox` 均通过。这没有改变每 5 秒一次的
周期扫描，**不能**消除 10 万存量沙盒的常态扫描瓶颈。

Linux arm64 构建输入 ELF SHA-256 为
`b652ca351f6c5f52cd3976da2b04fde2780fe4ca4abcbc1d70fd90ce9e5b767c`；
内部仓库镜像 `sandbox-fuse-etcd-api:v1-reset-63ebccc-arm64` digest 为
`sha256:8ebf209e34e19745754adff79417557f8597b6309ee8ed278c19b228b18059a3`。
DS 同后端 Helm 升级到 revision 12 后，API 2/2、etcd 3/3、MinIO 1/1。
公开 `POST /api/v1/execute` 返回 200、退出码 0、预期 Python 输出；
随后确认受管 Pod 和 NetworkPolicy 为空。忽略目录验证脚本
`ds_one_shot_smoke.py` SHA-256 为
`aecf915d1d08c69f5d6419f1adb58c0b5bb32ccffad33ec6938dd4b22b3094cd`。
这只是单次正常 one-shot 正例，未测高并发、通知溢出或 ACK 丢失。

## 存量规模的两类维护成本

当前高 N 问题需要按沙盒类型分别处理，不能只优化 `Scan` 的单次耗时：

| 存量类型 | 当前常态成本 | 10 万条时的必要判断 |
| --- | --- | --- |
| 普通活跃沙盒 | 每个 API 副本约每 5 秒全量遍历一次，未到期记录仍读出并解码 | 本机小快照纯存储 Scan 已耗 4.886 秒；两副本会重复读取，不可验收 |
| sync/FUSE 活跃 workspace | 上述扫描外，每条被持有的 workspace 有独立 controller 及默认每 5 秒一次的 `RenewController` | 若同一 cell 真有 10 万条此类 workspace，光续租就需要约 2 万次 RPC/秒，还未计 FUSE、Kubernetes、对象存储或故障接管 |

`63ebccc` 只删除一次性执行结束时由已知 ID 引发的额外全量扫描。
下一容量改动必须先对普通沙盒建立有界到期发现，同时保留重启/ACK
未知时的原记录栅栏；sync/FUSE 则需要按实际存量和续租能力确定单 cell
上限及分区数量。单独把 5 秒间隔调大、并发扫全库、或只增加 etcd
成员，都不能消除每个副本的 O(N) 读取与每条 workspace 的续租成本。
目前没有完成两类存量的真实容量验收，也没有可靠的 10 万单 cell 承诺。

## 有界到期索引的本机存量测量（尚未发布）

提交 `eb4cd1a` 把普通到期和 workspace 发现键与原生命周期记录放在同一
etcd 事务里；`a5fac67` 保留 API 启动时的一次完整恢复扫描，后续 tick
按两个索引各最多读取 8 页、每页 128 条，并在做任何效果前重新加载权威
记录。`ce41f5b` 验证独立客户端重建后仍能读出到期索引。完整 etcd
存储包、sandbox 包、重点 race 和 vet 已通过；真实 etcd 的普通 TTL 与
FUSE TTL 清理正例通过。DS 当前 revision 12 **尚未部署这些新提交**。

本机 Docker Desktop 单节点 etcd v3.6.14、loopback 明文、独立前缀；
16 个固定 writer，每条记录先 Publish 再 Activate。合成 JSON 包含
`created_at`、两小时 TTL、运行时标识及非空配置，但仍远小于完整真实
沙盒快照。以下是仓库 `TestActiveResidentIndexScale` 的原始测量：

| 合成普通存量 | 键数 | 两次写入总耗时 | 空闲到期查询 | 全部同时到期候选遍历 | 启动时完整库存扫描 | 单节点 DB 增量 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 10,000 | 30,000 | 2.705 秒 | 1.435 毫秒 | 0.250 秒 | 0.210 秒 | 27.57 MB |
| 100,000 | 300,000 | 25.483 秒 | 7.847 毫秒 | 4.379 秒 | 4.867 秒 | 275.56 MB |

命令为 `TEST_ETCD_ENDPOINT=127.0.0.1:64106 TEST_ETCD_INDEX_SCALE_N=<N> go test -v ./internal/storage/state/etcd -run '^TestActiveResidentIndexScale$' -count=1 -timeout=240s`，使用主仓库共享 Go 缓存。1,000 条的同一测试在 race 下通过。原先不含启动扫描的独立轮次测得 10 万条空闲查询 6.437 毫秒、同时到期候选遍历 4.754 秒；上表是增加启动扫描后的新轮次，并非给旧轮次改判。上表的“同时到期”只读取候选，**没有**创建 Pod、终止进程、回收资源或退款；它已接近 5 秒 tick 间隔，不能据此宣布集中到期容量通过。启动完整扫描虽只发生一次，也尚未测真实快照与多副本同时恢复。

本轮只说明大量远期普通记录的空闲查询不再读取全部记录。两 API 副本、
三成员故障、真实沙盒和集中 TTL 效果、p95/p99、sync/FUSE 大量续租尚未
测完，因而 **10 万存活沙盒容量门继续保持未通过**。sync/FUSE 的单条
controller 五秒续租成本仍在；本索引不降低那部分 RPC 量。

## DS 双副本索引版本的单次业务回归

索引版本的 Linux arm64 ELF SHA-256 为
`52333ed42a579c6ff19471034ded4f5b3252ae48c86e0c245d751e821c043d74`；
镜像 `sandbox-fuse-etcd-api:v1-reset-6c17a7f-arm64` 的 registry digest 为
`sha256:8c2d72892c0a05a45c4730a59fd3b8eb70cee30025f307fe1477a469628c6da8`。
升级前在**明确的** `ds-ai-research` context、`sandbox-fuse-etcd`
命名空间检查原业务 active 前缀的键数为 0；旧部署为 API 2/2、etcd
3/3、MinIO 1/1。专用 Helm 值 SHA-256 为
`7b9ef13767456f6b728687d5c793533330fa8f751d031fcfa642ab8ce2791219`，
`helm lint` 通过，Helm revision 13 状态 `deployed`，API 2/2，etcd
3/3，MinIO 1/1。FUSE 仍关闭。

公开一次性 Python 执行返回 200、退出码 0，随后受管 Pod 与
NetworkPolicy 均为空；验证脚本 SHA-256 为
`aecf915d1d08c69f5d6419f1adb58c0b5bb32ccffad33ec6938dd4b22b3094cd`。
另一条 90 秒 TTL 沙盒 `sandbox-u8wzx3lv94` 在两个 API 副本滚动重启后，
公开 GET 依次返回 200、503、404，受管 Pod 与 NetworkPolicy 清空；
脚本 SHA-256 为
`0e04b72cc0e8bdd06bda1686793a2c33847aa1d37e06e6f8cf6186cc29d2e599`。
这些是新索引版本的单次业务正例，**不证明**高并发、10 万存量、同时
大量到期、三成员压力故障、配额流水或 FUSE 物理清理。

同一 revision 13 下，另一条 600 秒普通沙盒
`sandbox-pcc7b935vz` 存活时重建 etcd-2 Pod，其 UID 从
`2f7933eb-546a-4a07-ab2d-20ac70b5847b` 变为
`585a5292-b419-49b1-aca6-79553ecd2499`。成员重新 Ready 后公开
GET、Python Exec、DELETE 均返回 200；etcd 3/3 Ready，受管 Pod 与
NetworkPolicy 为空。验证脚本 SHA-256 为
`accce55800755ba839bd8f6cc4e6ba6522dd6d1d05ef52cb82a4e7065e479ef6`。
这只覆盖单成员重建时的一条活跃记录，不覆盖三成员负载、节点掉线、
长时间分区或大量同时到期。

## 新部署状态入口与 FUSE 发布门复核

当前 DS Helm revision 13 的安装清单中 `sandbox-redis`/`REDIS_`
匹配数为 0；命名空间中的 Redis StatefulSet、Service、Deployment
匹配数也为 0。API Deployment 只暴露 `SANDBOX_STORAGE_STATE_BACKEND`
和 etcd endpoints/prefix/timeout 等状态环境项，未暴露 Redis 环境项。
代码 `openStateBackend` 只创建所选的一种 Store，并将同一个 Store
交给 session、ephemeral、multipart 与 workspace，同时用同后端的
active/FUSE pool 仓库；因此**本次 DS 新环境**没有暗中混用 Redis。
仓库仍保留显式 Redis 旧模式和 Chart 旧默认值，不能把本次部署核对
表述为全仓库 Redis 代码已移除。

FUSE 发布门仍未通过。Chart 默认的 `sandbox-fuse-mounter:v0.2.12`、
`sandbox-fuse-docker:v0.2.12` 在内部仓库缺失；只读清单检查发现
`sandbox-fuse-mounter:v0.3.35` 有 arm64 digest
`sha256:08ea7821ddb4b5fbd65a4492e85bf2e3d0a92fc195eb6d1d324931e0e11a7d78`，
而 `sandbox-fuse-docker:v0.3.35` 不存在。较早的
`sandbox-fuse-docker:v0.3.4` 有 arm64 digest
`sha256:5f21852681a0ad10ded73952fc4bc19d46cea5200832c7fc80abb04da6da2130`，
但尚未证明其 helper 协议与当前源码相同，不能混搭声明验收。
本轮没有启用 FUSE，也没有使用旧迁移分支镜像替代当前源码构建。

随后在干净工作树以主仓库共享 Go 缓存执行 `go test ./...`，所有返回的
包均通过；该轮没有设置 `TEST_ETCD_ENDPOINT`，因此真实 etcd 断言以
前述单独设置环境变量的存储和 sandbox 回归为准。全仓通过不能替代
FUSE 镜像、对象存储物理挂载或容量发布门。

## 当前源码 mounter 与 DS FUSE 灰度失败

代码核对纠正了一个发布门判断：Kubernetes FUSE Pod 需要普通 runtime
镜像和独立 mounter，缺失的 `sandbox-fuse-docker` 镜像只供 Docker 运行时，
不是 DS Kubernetes 路径的拉取依赖。原 Dockerfile 因 Docker Hub 匿名
token 连接重置未进入编译；没有把这次构建记为通过。提交 `fdef93f`
改用固定 digest 的已有 mounter 基础镜像，只替换当前分支编译的
Linux arm64 mounter。二进制 SHA-256 为
`01be1f42037c5bdeb6731bad1bce5da79260e5f6212671b6a02c58402c9704ad`；
镜像构建和容器内的 package/release 自检均通过。推送标签
`sandbox-fuse-etcd-mounter:v1-reset-fdef93f-arm64` 的 digest 为
`sha256:c5fe653022ffac4baee0264e965d99f7655306082f3282ef39b8233bfcdd8064`。
基础镜像提供 s3fs 和 FUSE 库，镜像内 profile bundle 哈希检查通过；
这仍不是 DS 的物理挂载证明。

启用 `[sync, fuse]` 的 DS 专用 Helm 灰度先通过 `helm lint`，且该命名
空间活跃沙盒记录数为 0。升级时两个 API 启动均失败，实际日志为
`unsupported root marker profile "minio-sigv4-path-style-private-http-v1"`。
测试 MinIO 当前是 HTTP，而当前 FUSE 根标记器不接受这个 profile；
失败发生在 API 启动，尚未创建 FUSE Pod，也没有完成挂载。没有通过
更换旧分支镜像或放宽标记器规则掩盖失败。

中断的 `--atomic` 回滚又被 Chart 的跨后端指纹保护拒绝，留下两条
`pending-rollback` Helm 修订。先将两个 Helm Secret 原样备份到忽略目录
`helm-recovery-20261025`，再移除这两条未完成元数据；DS 专用 values
恢复到原 SHA-256
`7b9ef13767456f6b728687d5c793533330fa8f751d031fcfa642ab8ce2791219`，
随后正常 `helm upgrade` 完成 revision 15。最终 `sandbox` 状态
`deployed`、API 2/2、活跃沙盒记录 0，FUSE 仍关闭。当前依赖另一命名空间
已安装的同 digest AppArmor profile 做测试预检；独立部署和 HTTPS
MinIO/FUSE 物理验收仍未完成。

## 三成员本地存量索引测量

另外启动三个本机 Docker etcd v3.6.14 成员，独立网络、独立临时数据目录、
三副本 Raft；测试后停止三个容器并删除专用网络。仍是本机元数据模型，
没有真实 Pod、配额、FUSE、API 副本或同时到期的清理效果。以当前
`TestActiveResidentIndexScale`、16 个写入 worker、2 小时 TTL 和固定
非空配置测得：

| 普通记录 | 前缀键数 | 写入 | 空闲到期查询 | 全部到期候选遍历 | 启动全量盘点 | leader DB 增量 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 10,000 | 30,000 | 4.371 s | 0.830 ms | 0.257 s | 0.230 s | 27.57 MB |
| 100,000 | 300,000 | 48.910 s | 7.680 ms | 5.527 s | 5.567 s | 275.56 MB |

暂停非 leader 第三成员时，再以独立前缀写入 10,000 条：写入
4.371 s，空闲查询 1.151 ms、全到期候选遍历 0.279 s、启动盘点
0.243 s，测试通过。恢复该成员后，首次健康查询返回 `too many
requests`，**该次健康检查失败**；随后三成员 Raft applied index
均到 240011，第二次三 endpoint 健康检查才全部通过。不能把暂停
期间的有限元数据通过说成故障恢复或容量验收，也不能抹掉第一次失败。

这组数字只说明远期普通记录的闲时发现成本受索引约束。
周期协调器每轮每种索引最多 8 页、每页 128 条；在集中到期场景，
单副本每轮最多发现 1,024 条，尚未计候选逐条重新读取、销毁、
Kubernetes 调用和配额处理。10 万同时到期的清理延迟不能从上述
5.527 秒候选遍历推断。当前仍没有可信的单 cell 支持上限。

### 两个独立客户端同时读取存量索引

在独立本机单成员 etcd v3.6.14 上，给上述 opt-in 测量增加
`TEST_ETCD_INDEX_SCALE_DUAL_READER=1`：写入完成后由两个不同 etcd
客户端同时遍历相同前缀的全部到期候选和启动盘点。两客户端均核对
完整数量；这模拟了两个读者的存储压力，**没有启动两个 API 副本**，
也没有实际清理效果。

| 普通记录 | 前缀键数 | 单读全部到期候选 | 单读启动盘点 | 双读各自到期候选 | 双读各自启动盘点 | 双读总墙钟 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1,000 | 3,000 | 43.60 ms | 27.23 ms | 39.71 / 38.59 ms | 25.45 / 29.47 ms | 68.06 ms |
| 100,000 | 300,000 | 5.990 s | 6.132 s | 6.217 / 6.217 s | 5.814 / 5.815 s | 12.032 s |

命令使用原 `TestActiveResidentIndexScale`，另设置双读环境变量；
10 万条写入 34.338 秒，leader DB 增量 275,558,400 字节。原始日志
位于忽略目录 `2026-10-25-etcd-resident-lifecycle/dual-read-1000.log`
与 `dual-read-100000.log`，SHA-256 分别为
`fd445ee75c50ffd926945afaf643ff30c84fbbebe4e5c0e115f1972eaab8e179`、
`e9a960c2189a8a6a0cee17d2c074dd5cb4d024aa5a2324e0986ecf5801781d4d`。
双读阶段先遍历到期候选、再盘点启动记录，因此墙钟是两个阶段之和，
不是一次查询延迟。这些快照只是非空配置模型；产品解码、Controller
续期、Pod/FUSE、清理退款和故障负载均未进入该实验。单 cell 大存量门
仍未通过。

## 活跃 workspace controller 续期的本机测量

新增 opt-in `TestActiveControllerRenewScale`，在独立、已清理的本机
etcd v3.6.14 单成员容器上，为每条记录写入 629 字节的合成 FUSE
workspace 形状快照、取得 controller，再以 16 个 worker 各续一次。
测试使用五分钟 lease，以免顺序建样本时提前过期；产品默认仍为
15 秒 lease、每 5 秒续一次。本表只测一次并发续期，不模拟五秒周期、
API 副本、真实 Pod/FUSE 或持续后台负载。

| controller 数 | 顺序建样本 | 一轮续期 | 一轮吞吐 | 单次 p95 | 单次 p99 |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 20 | 0.096 秒 | 0.013 秒 | 1,488 次/秒 | 9.76 毫秒 | 9.76 毫秒 |
| 100 | 0.346 秒 | 0.073 秒 | 1,365 次/秒 | 14.97 毫秒 | 15.78 毫秒 |
| 1,000 | 3.304 秒 | 0.737 秒 | 1,357 次/秒 | 18.62 毫秒 | 22.41 毫秒 |
| 10,000 | 32.263 秒 | 6.532 秒 | 1,531 次/秒 | 15.39 毫秒 | 24.14 毫秒 |

命令为 `TEST_ETCD_ENDPOINT=127.0.0.1:53461
TEST_ETCD_CONTROLLER_SCALE_N=<N> go test -v ./internal/storage/state/etcd
-run '^TestActiveControllerRenewScale$' -count=1 -timeout=180s`；共享主仓库
Go 缓存，四轮原始日志位于忽略目录
`.superpowers/sdd/2026-10-25-etcd-resident-lifecycle/controller-renew-<N>.log`。
其 SHA-256 按 20/100/1,000/10,000 顺序为
`901ccf1913b44edc8006934f3bfdfbb9af3abdbbe608b2b1acf49dffa7c4a9eb`、
`6c2a872d4c1f7c73617d5bc05fe0ab332f317f6a39f178d0b3cc01bfef3d68c6`、
`3d0206a1f2f55cbdeb49f799511a67958dd2d83391a786d9fe7a4db2e81132bf`、
`2c1cb40d79661e4ce05d3288268930bd080046c266576ba995e6fc1dfd14f926`。
etcd 存储包测试和 vet 通过；专用容器已删除。

按默认五秒间隔，10 万活跃 workspace 需要约 20,000 次 controller
续期调用/秒。当前一次正常续期还含同一事务快照内的两次定点读、一次 lease grant、
一次事务和一次旧 lease revoke。10,000 条的同步突发一轮已超过五秒，
但真实续期会错峰，这个单成员合成实验不能直接确定 DS 每 cell 上限。
10 万 FUSE/workspace 的容量门仍未通过；继续扩大实验前还须计入真实
快照、API 内存与 goroutine、周期 QPS、FUSE 物理负载和故障恢复。

随后把 `RenewController` 对 record 和 controller 的两次独立 GET 合成
同一 etcd 快照中的两次读取，写入时仍比较两者 revision，并保留原
lease grant、旧 lease revoke 和未知结果不重发规则。真实 etcd 存储包
回归及 vet 通过。在另一独立本机单成员容器、相同合成快照和 16 个
worker 下，新实现的单轮 1,000 条为 0.755 秒（原轮 0.737 秒），
10,000 条为 5.781 秒（原轮 6.532 秒）。这是不同容器上的单次观察，
没有受控多轮 A/B，**不能据此宣称吞吐提升**；减少一次客户端往返是
代码事实。10,000 条突发仍超过产品默认五秒续期间隔，专用容器已删除。

### 同一批 controller 在三成员集群中连续续期

将 opt-in 续期测量扩展为最多五轮。每轮复用上一轮取得的 controller
lease；计划起点相隔五秒，上一轮超时则下一轮立即开始并记录迟到量。
本轮设置 `TEST_ETCD_CONTROLLER_SCALE_ROUNDS=3`，通过
`TEST_ETCD_CONTROLLER_SCALE_N` 分别选择 1,000 和 10,000 条。
隔离的本机三个 etcd v3.6.14 成员使用三副本 Raft，16 个续期 worker，
合成快照仍为 629 字节。为保证顺序建样本和测量期间不提前过期，
测试 lease 为五分钟；**这不是产品的 15 秒 lease**，也没有运行
产品的一沙盒一 goroutine 续期器。

| controller 数 | 顺序建样本 | 三轮续期耗时 | 第二、三轮起点迟到 | 五秒周期 |
| ---: | ---: | ---: | ---: | --- |
| 1,000 | 5.309 s | 1.544 / 1.571 / 1.401 s | 0.001 / 0.001 s | 三轮均落入周期 |
| 10,000 | 59.587 s | 12.868 / 12.895 / 16.176 s | 7.870 / 15.765 s | 三轮均超时 |

10,000 条实验中所有续期 RPC 成功，测试进程退出码 0，结束后三个成员
的 endpoint health 均通过；但周期调度已持续落后，**容量判定失败**。
单次 RPC p99 在三轮分别为 55.43、37.78、48.31 毫秒，不能用低
单次延迟掩盖整轮排队。原始日志位于忽略目录
`2026-10-25-etcd-resident-lifecycle/controller-renew-three-member-1000x3.log`
和 `controller-renew-three-member-10000x3.log`，SHA-256 分别为
`9260ed10910348d76b1393569b1b4dacb08029a8680bf1c6ca97b61230e39b67`、
`0e687ceb885cfdab90339d4288d8d205c39271d4c1cba90d889c88c3195de803`。
这是本机同步批次负载，不是 DS 上的错峰真实 FUSE、两 API 副本或
节点故障恢复；它只给出当前 16 worker 测量方式下 10,000 条不能
满足五秒周期的反例，不能据此计算正式单 cell 上限。
改动后的单轮 smoke、存储包回归（58.968 秒）和 vet 均通过；
回归日志 SHA-256 为
`ba61e67a752b274504934d23a451f7d24a59c329f6fc83352bf85b3812bc080f`。
三个专用成员及其 Docker 网络已删除。

### 产品 controller 续期器的短时驻留测量

另用 opt-in `TestActiveControllerSchedulerScale` 调用真实的
`Manager.acquireActiveController`，让每个 sandbox 持有产品的一条
`activeController.run` goroutine 和五秒 ticker。隔离的本机单成员 etcd
保存 356 字节 FUSE 形状快照；controller TTL 为产品默认的 15 秒。
建样本后保持 12 秒，检查每条租约已续期且 controller 未丢失。

| controller 数 | 顺序建样本 | 续期成功 | 丢失 | 建样本后 goroutine 数 |
| ---: | ---: | ---: | ---: | ---: |
| 20 | 0.090 s | 20/20 | 0 | 27 |
| 1,000 | 2.857 s | 1,000/1,000 | 0 | 1,007 |
| 3,000 | 11.461 s | 3,000/3,000 | 0 | 3,007 |

原始日志在忽略目录 `2026-10-25-etcd-resident-lifecycle/` 下的
`product-controller-{20,1000,3000}.log`，SHA-256 依次为
`4863e4a74164f47600d8de6825a784256b6a58b7602613821e1e419148d764da`、
`add2a62f1475048c6db2e8cc3ca204794b11b1006b59469e2c22b403cd2da86b`、
`6c285fd60c246c725564772887088fff525b301c388c4c9178eb`。
定点 race 首轮发现新测试读取初始租约时未持有 controller 锁，修正后
20 条 race 及 `internal/sandbox` 全包回归通过；首轮失败记录保留。

这只证明一个本机成员上短时、无 Pod/FUSE 的续期器能维持至 3,000
条。goroutine 数随活跃 sandbox 线性增长；测试中的堆分配差值受 GC
影响，不能当成每沙盒 RSS。尚未覆盖更长时间、两个 API 副本、三成员
故障、真实 workspace 健康检查与集中到期。此前三成员同步 10,000 条
续期的五秒周期失败仍有效，**大存量容量门仍未通过**。

同一产品测试随后在隔离的本机三成员 etcd v3.6.14 上运行，客户端
连接 leader，另外两成员参与 Raft。1,000 条顺序建样本 4.407 秒，
3,000 条 20.177 秒；保持 12 秒后分别有 1,000/1,000、3,000/3,000
条租约前进，controller 丢失数均为零。日志 SHA-256 为
`49e8fbc33586ff128186dd0e8f4fd5da880fba0f05a9e6b50a77d0483df811f2`、
`5537e3ec473636d59b5acbb10182ee142b7db0e50aa360c060c4874696b71acb`。
3,000 条结束后，客户端取消期间出现少量 `Canceled` 日志；测试退出 0，
三个成员的健康检查均通过。

为避免“曾经续过一次”掩盖观察时已过期，测试又增加了当前到期时间
检查。在 1,000 条的另一轮中，暂停一个非 leader 成员约五秒；其暂停
区间 `2026-10-25 09:02:08–09:02:14 UTC` 与建样本完成
`09:02:11.698 UTC` 后的保持阶段重叠。最终观察时间为
`09:02:23.698 UTC`，1,000 条均已续期、到期元数据仍有效，丢失数零；
恢复后三个成员健康。原始日志 SHA-256 为
`277c8869e69d42240e4b728ce5e0ebd89a0e5434a6645cf7730349e16a6156bc`。
另一次较早的暂停落在启动/建样本阶段，不能作为续期故障证据。
改动后的定点 race 与 sandbox 全包回归通过；实验容器和网络已删除。
这些短时结果不包含 leader 切换、两个 API 副本、真实 Pod/FUSE、
长期稳定性或 10,000 条以上的产品调度，不能改变容量门结论。

再将 opt-in 测试按 sandbox ID 交替分配给两个独立 etcd 客户端和两个
`Manager`，它们使用同一隔离前缀、不同 `InstanceID`，仍连接本机三成员
集群。这模拟两份 API 进程中的 controller 负载，但没有启动 HTTP 服务、
Pod、FUSE watcher 或业务清理。

| controller 总数 | 顺序建样本 | 观察时已续期且到期元数据有效 | 丢失 | 建样本后 goroutine 数 |
| ---: | ---: | ---: | ---: | ---: |
| 1,000 | 5.651 s | 1,000/1,000 | 0 | 1,012 |
| 3,000 | 25.124 s | 3,000/3,000 | 0 | 3,012 |

两轮均保持 12 秒，日志 SHA-256 分别为
`c3122074dddaaa289fa233e1d323d5ed0895d75b6d1e68f4606a88c10d5c1d36`、
`53071d2dca58855887dd86585acd7fe13f5e426af4f53488e32ae94d54262543`。
定点双客户端 race、`internal/sandbox` 全包回归和 vet 均通过；结束后
三个 etcd 成员健康，隔离容器和网络已删除。3,000 条测试取消时出现
`Canceled` RPC 日志，测试本身退出 0。这些观察仍不能证明两个完整 API
副本的调度、较长期控制权保持、真实 FUSE 工作负载或可承诺的单 cell 上限。

进一步将同一测试上限开放至 10,000 条，保留双客户端、三成员、15 秒
controller TTL、五秒续期间隔和三秒 etcd 请求超时。结果为 **FAIL**：
顺序建样本期间，`acquireActiveController` 返回
`context deadline exceeded`，未进入 12 秒保持阶段，因此不能报告
10,000 条续期成功率。失败后只读统计显示隔离前缀有 5,621 条
`record/` 键；测试按 ID 顺序先发布和激活、再获取 controller，故该次
失败发生在第 5,621 条记录附近。此前返回成功的获取不等于它们在失败
时仍持有有效控制权。原始失败日志 SHA-256 为
`48f766972539a5789c1989c0dad60531b892e1bc55a9568b2a90137b74c2380e`。
请求超时与 Helm etcd 默认的 3,000 毫秒相同；不能为让实验通过而单独
放宽。测试退出后，三个成员均能提交健康探针，隔离容器和网络已删除。
这定位的是本机这一负载配置的失败点，不能直接推定 DS 集群或不同
硬件的上限，也不能把 3,000 条短时通过当成可承诺的容量上限。
后来检查发现，这些早期实验虽然启动了三个 etcd 成员，却只给每个
客户端配置了第一个 endpoint。此处保留原 FAIL，不再称它们验证了
三 endpoint 负载均衡；后续修正和对照见下节。

为区分写入与存量续期争用，`517efeb` 将 controller 获取和续期的
读、Grant、提交、Revoke RPC 接到现有 OTel 延迟直方图，仅用固定的
operation/stage/result 标签；存储包通过可选观察回调保持依赖方向。
真实 etcd 定向用例、相关三包完整回归、定向 race 和 vet 通过。
可选产品负载测试又加入固定大小的原子阶段统计，不保留逐请求样本，
观察点与停止清理后的统计分开。新隔离三成员、双客户端实验如下：

| 建样本目标 | 顺序建样本 | 12 秒观察点 | 结论 |
| --- | ---: | --- | --- |
| 1,000 | 4.356 秒 | 1,000 已续期且存活、lost 0 | 短测通过 |
| 3,000 | 25.935 秒 | 3,000 已续期且存活、lost 0 | 短测通过 |
| 6,000 | 未完成 | 第 5,153 条 `Activate` 超过 3 秒；未进入观察阶段 | **FAIL** |

1,000 档的获取 read/grant/commit 平均分别约 0.415/0.690/0.830
毫秒；3,000 档升至 1.195/1.381/1.498 毫秒。3,000 档全轮
（含停止清理）的续期 read/grant/commit/revoke 平均约
5.23/3.99/4.09/4.97 毫秒，读阶段有 108 次超过 100 毫秒。
这些是单轮的聚合观察，不能作跨轮
严格 A/B 性能结论。6,000 档失败前已有 5,152 次成功获取；失败
出现在后续 `Activate`，它内部先读记录再提交事务，本轮未进一步区分
这两个 RPC。测试停止控制器时产生的取消/超时计入 `after_stop`
阶段统计，不应误记为此前 12 秒驻留的失效。失败后三个 etcd 成员
仍能提交健康探针；专用容器和网络已删除。20 条双客户端 race 正例
通过，验证了统计器并发读写。

上述四份原始日志按 1,000、3,000、6,000、20 条 race 顺序位于
忽略目录 `2026-10-25-etcd-resident-lifecycle/controller-stages-three-member-two-client-*.log`，
SHA-256 依次为
`73852fd8bc0bb19b635c17472245d79f7dae3a51c2fd7aa3fd23a71d3a2c2da7`、
`82537f713a2a28751875f8e17d80508c0a108b77157c4fa74efc693203c926b1`、
`af7bec69812d3bb0fab6d5e6b03c10e0085588007e2befe069240472e91bd27c`、
`56eaed5046fdb9de9eb27b0fedd5a1cf7440ad7cab8a63fe30e7fb9918db8982`。
这组诊断说明当前每沙盒独立续期与新建写入相互争用；不能据此确定
唯一服务端瓶颈，也没有通过增加请求超时或放宽 fencing 来绕过失败。
20 个 FUSE 的 DS 灰度与大存量验收继续分开。

### controller 续租写入放大定位与有界复测

修正可选产品负载测试，使两个独立客户端均使用三个 etcd endpoint。
修复前，在全新本机三成员集群上建 6,000 条样本仍 **FAIL**：第 4,618
条的 `Publish` 超过原有三秒请求超时，未进入驻留观察。此轮各成员均
收到约 45,000 次 LeaseGrant 和约 44,000 次 LeaseRevoke；它们主要来自
存量 controller 每五秒重新 Grant、CAS Put 和 Revoke 旧租约。客户端
请求虽分布到三个入口，这些写入仍进入同一个 Raft 日志。原始日志
`controller-three-endpoint-6000.log` 的 SHA-256 为
`ef46aaada0ec92cf0927f9cddec2a059e42d7b1d7725d2b7e84c092b34b5bde5`；
停止时的错误与原 `Publish` 超时分开记录，不能改判该轮失败。

`3c6210a` 修正负载测试 endpoint 输入；`f7d3db1` 在 requested TTL
不变且持久 controller 仍绑定原租约时，先按 record/controller 的确切
revision 做 CAS Put，再对该租约 `KeepAliveOnce`。换 TTL 或旧格式记录
仍使用 Grant、CAS、Revoke 路径；未知结果保持失败，不重发续租。
真实 etcd 测试验证同租约越过原到期仍排斥竞争者、TTL 改变和旧格式
记录均换租约；相关包回归、定向 race、vet 通过。此改动没有调整
15 秒 TTL、五秒周期、三秒请求超时或持有者栅栏。

在分别重建的全新本机三成员集群中，两个客户端均配置全部三个 endpoint，
同一产品测试的结果为：

| controller 数 | 顺序建样本 | 12 秒驻留观察 | 完整测试 |
| ---: | ---: | --- | --- |
| 6,000 | 50.273 秒 | 6,000 已续期且存活，lost 0 | PASS，70.55 秒 |
| 10,000 | 128.179 秒 | 10,000 已续期且存活，lost 0 | PASS，155.66 秒 |

两轮原始日志 SHA-256 分别为
`07be4127fd93e937c6ffd1deb3610a44fe5cc8c8ea69fc01963fe64b7d634a16`、
`0e03d1a5c5dc30f7528019ceea20b7bc7f8991e96e38b5256c28692eb1f3703a`，
位于忽略目录 `2026-10-25-etcd-resident-lifecycle/controller-three-endpoint-postfix-*.log`。
10,000 条观察点记录约 189,172 次成功续租 commit/KeepAlive，错误 0；
三个成员的 apply 指标各增加恰好 10,000 次 Grant 和 10,000 次 Revoke，
分别对应首次取得控制权及停止清理，未再每次续租换租约。10,000 条
观察点的续租 read/commit/keepalive 最慢分别约 989/886/859 毫秒，
均低于三秒超时。测试全部退出 0，成员健康，专用容器和网络已删除。

这组有界单次复测支持“短租约续期写入放大是原本机失败的重要原因”，
但不是不同硬件上的严格 A/B 性能测量，也不证明 10,000 条长期稳定。
测试没有真实 Pod/FUSE、完整两个 API 进程、集中到期、节点或 leader
故障、同时创建/销毁和 10 万存活。因此 DS 的 20 个 FUSE 功能灰度与
大存量容量验收仍是两个独立发布门；后者保持未通过。

`34a43ea` 给可选产品测试增加有界驻留时间（12 秒至两分钟，默认仍为
12 秒）。又在全新的本机三成员集群用双客户端、全部三个 endpoint、
原 15 秒 TTL/五秒周期/三秒 RPC 超时测 10,000 条，建样 141.801 秒后
驻留 60 秒：观察点 10,000/10,000 已续期且存活、lost 0；完整测试
PASS，219.20 秒。续租 read/commit/keepalive 在观察点各约有
297,108/297,099/297,095 次调用，错误均为零；最长分别约
1.227/1.149/0.882 秒。日志
`controller-three-endpoint-10000-hold60.log` 的 SHA-256 为
`15e34e627161a7b65477e80aba1510d62fa8b0fea577e0c1b865fc91f9738d02`。
停止时取消中的 RPC 单独计在 `after_stop`；三个成员结束后健康，专用
容器和网络已删除。这比 12 秒观察更有力，但仍只是一次本机无 Pod/FUSE
的 60 秒短测，不能外推到长期运行、节点故障或 10 万存活。

### FUSE 池安全判定仍随存活数量增长

代码复核发现 `Manager.guardFUSEPoolRecord` 在取得或归还准备池记录时，
先遍历本副本的 `fuseLifecycles`，再 `SessionStore.List` 并逐条 `Load`，
然后 `EphemeralLifecycleStore.List` 并逐条 `Load`，最后调用
`WorkspaceCoordinator.HasRuntimeOwner` 遍历全部持久 owner。每段都在
寻找与该池记录的 runtime UID 或 preparation ID 相同的占用者；任何
存储或解码错误都使判定保持 Unknown，而不是把记录当作可复用。
因此未提前命中占用者的判定会经过多个随存活数量增长的列表或读取，现有
10,000 条 controller 续期测试**没有**覆盖这一成本。`fusePool.maxSize`
限制的是准备池规模，不能把这条检查或活跃沙盒数量变成常数。
DS 灰度配置的 `fusePool.minSize=0`，正常冷创建在 `ReservePrepared` 没有
候选时直接走 `prepareCold`，不执行这条 guard；它仍用于预备记录返回、
检查、回收和 release drain，启用预热池时也会影响命中路径。因此不能
把它归因为本轮 controller `Publish` 超时，也不能把未测的这些路径认定
为大存量已通过。通用 `state.AtomicStore` 只有单键 CAS，没有跨键原子
索引维护能力；用进程内缓存或单独写反向索引直接跳过扫描会存在漏判
持久 owner 的风险。大存量 FUSE 池验收前需单独设计并验证同事务的
反向保护索引或明确限制该路径，不能让缺索引时返回“可复用”。

现有会话键按 sandbox ID、临时生命周期键按 sandbox ID、workspace
owner 键按 workspace 身份组织；准备池记录只持有 preparation ID 与
runtime 身份，无法从这些字段安全地直接定位全部三类持久占用。
仅在某个副本缓存扫描结果会漏掉其他副本的写入。要改成定点判定，
须先设计与原发布、释放事务同步维护的持久反向关系，并证明丢失 ACK、
重启和旧记录不会产生“假空闲”；不能为提速跳过任一安全来源。
这项改造超出本轮普通记录到期索引的职责，当前按容量阻塞项保留，
没有声称已有可靠的 10 万 FUSE 单 cell 支持上限。

### FUSE 常态健康检查的逐沙盒成本

产品启动路径给每条 FUSE lifecycle 建立一个 watcher goroutine 和
五秒 ticker。分布式健康检查先通过 `beginDistributedOperation` 取得
操作许可并检查运行中 Pod 身份，再用 `WorkspaceHealth` 对确切 UID
读取 Pod 和 mounter 状态。原路径在这两步之间又执行一次
`GetSandbox`；这与入场检查重复。定点回归先实际失败，观察到每次健康
检查调用两次 Manager 层 `GetSandbox`；去重后为一次，并额外校验入场
持久快照的 runtime ID/UID 与当前 lifecycle 相同。非分布式路径仍保留
其原有运行时检查，`WorkspaceHealth` 的确切 UID 和重启检测也保留。
`internal/sandbox` 全包测试 11.601 秒、相关 race 1.893 秒及 vet 通过。

这只删除一项重复 Pod 读取，没有削弱原操作许可，也没有证明真实
Kubernetes QPS 改善。按固定五秒间隔，假设一个 cell 持有 10 万条
活跃 FUSE lifecycle，名义上仍要调度约 20,000 次健康检查/秒，
每条还包括持久操作许可、确切 Pod 和 mounter 状态；每条 lifecycle
仍持有自己的 watcher。真实错峰、API 内存、Kubernetes 限流和故障
下的尾延迟尚未测量，不能以此改判大存量容量门。

## 隔离 API canary 的 FUSE 创建失败

没有改动正式 API Service 或 revision 15 的 sync-only 配置。另建一个
`sandbox-api-fuse-canary` Deployment，固定当前源码 API 和 mounter 镜像，
只通过端口转发发送测试 FUSE 创建。首次请求在准备 Pod 前返回
`workspace FUSE sandbox CPU limit must not be empty`。Pod 环境变量中的
`SANDBOX_SECURITY_MAX_CPU=500m` 实际存在；无配置文件键时，Viper 的
`AllKeys` 不包含三个可选资源键，环境变量因而未绑定。提交 `e203f94`
加入失败回归并补齐空值默认键，保持原来的未设置语义。配置和启动包
测试、针对性 race、vet 均通过。修复镜像
`sandbox-fuse-etcd-api:v1-reset-e203f94-arm64` 的 digest 为
`sha256:7245e81e707a5c18385f6c35b3a01e620ca2012de4e64bd50808c003ede9afb9`。
新准备 Pod 已显示 CPU limit `500m`、request `100m`，说明此故障已越过。

随后 kubelet 首次拒绝 `sandbox-fuse` AppArmor 名称。已存在的节点 loader
实际装载 `sandbox-fuse-37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d`；
仅将隔离 canary 的 LSM 配置对齐后，mounter 镜像可以拉取、容器可以启动。
新的创建仍返回 500：准备 Pod 的 mounter 很快以 exit 1 终止，容器日志
只有通用 `workspace-mounter: request failed`，尚未确认内部启动失败原因。
没有将这一失败解释为 etcd 状态层问题，也没有将镜像自检称为实际挂载。
失败的准备 Pod 均由原清理路径删除；隔离 canary Deployment 随后删除。
复核正式 API 仍为 2/2，etcd 3/3，MinIO 1/1，FUSE 关闭。

### 后续隔离验证与未关闭风险

提交 `2715939` 修正 mounter 的 bootstrap 校验：仅对已验证的
`minio-sigv4-path-style-private-http-v1` 私有 MinIO profile 接受 HTTP，
其他 profile 仍要求 HTTPS。修复镜像
`sandbox-fuse-etcd-mounter:v1-reset-2715939-arm64` 的 digest 为
`sha256:d35348068bbe2b59a6ffc8f431f30eaf8ab7f94747729d99ac6c999caec8794a`。
再次以隔离 canary 申请 FUSE 时，受管 Pod 的两个容器达到 Running，mounter
的 health 报告实际 `mount_type=fuse`、`generation=1`；公开创建请求仍在
180 秒后超时，**不能记为 FUSE 创建通过**。

同一受管 Pod 标签的临时探针无法查询集群 DNS，也无法访问 MinIO Service
VIP。只给该实例增加精确后端 Pod 选择器的临时策略后，探针的 DNS 查询和
MinIO `/minio/health/ready` 均成功。DS 的 CNI 在该路径按 Service 转换后的
后端 Pod 地址执行策略；当前系统策略只放行 DNS/MinIO 的 Service VIP CIDR，
不足以保证此集群的实际连通性。临时策略及探针 Pod 已删除，产品策略尚未修正。

超时创建的 FUSE 清理先因运行时终止未证实而拒绝释放，随后受管 Pod
两容器均到终态、原清理记录达到 `terminated` 并移除了 finalizer。
Pod API 对象在删除宽限期后仍未自然消失；确认两个容器终态后执行强制
API 删除，原 FUSEPool 记录和实例策略随之消失。强制 API 删除不能证明
节点 FUSE 挂载已物理清理，故物理清理验收仍未通过。

`--audit-workspace-owners` 的只读审计仍发现该申请的一条 FUSE owner：
workspace hash `d0cf08d0ebb860011ace178322413027ef03c93083c5c18a33c099674ed86f86`，
状态 `runtime_scope_unconfirmed`、`recoverable=false`。现有 Kubernetes owner
恢复入口按设计拒绝在缺少 namespace/release 绑定时删除；没有直接删 etcd 键。
正式 API 保持 revision 15 的 sync-only 2/2，etcd 3/3，MinIO 1/1；
此残留和产品网络策略是下一次 FUSE 灰度前必须解决的阻塞项。

源码进一步定位到 FUSE 池对过期 `cleanup` 记录的自动恢复未调用已有的
owner 保护检查；它可能先删除池记录，使仍持久化的 workspace owner
失去清理锚点。提交 `714e011` 增加保护和失败回归：owner 受保护或检查
结果不确定时保留池记录；无 owner 的原清理路径继续执行。受影响包完整
测试、针对性 race 和 vet 通过。该提交尚未部署到 DS，也不能追溯修复
上述已失去池记录的 owner；其审计状态仍为不可自动恢复。

### DS 后端策略与运行时 probe 版本核对

在 `ds-ai-research/sandbox-fuse-etcd` 中只对标签为受管 FUSE/MinIO 的 Pod
加测试策略 `ds-fuse-backend-egress`，允许到 `kube-system` 中 `k8s-app=kube-dns`
的 53/UDP、53/TCP，以及同命名空间 `app=ds-minio-reset` 的 9000/TCP。
正式 Helm release 仍为 revision 15、sync-only；此策略尚非产品 Chart 的
通用网络修复。隔离的 `sandbox-api-fuse-canary` 用已发布的 API/mounter digest
创建 `sandbox-8dmg89anng`：POST 201（4.76 秒），GET 200 且 mount mode 为
`fuse`。DELETE 503；日志中的 `quiesce` 执行命令退出 2。该沙盒的 Pod
`sandbox-pool-mbos4yc70j` 仍在运行、租约仍有效，未强制删除或退款。

源码调用 `workspace-probe quiesce ... --cleanup`，而当前
`sandbox-runtime:v0.3.39` 的在场 probe 对该命令直接返回
`unsupported workspace-probe command`、退出 2。提交 `ab10cea` 新增可复现的
运行时叠加镜像构建配方，以固定的旧基础镜像和当前源码编译的 arm64 probe
构建；构建时校验 Linux ELF 架构并运行 probe 自检。新镜像
`sandbox-fuse-etcd-runtime:v1-reset-ab10cea-arm64` 的 registry digest 为
`sha256:ec108aeee183c1cb0f3a5c481bf38038626fde8da96ae5a4624df396d5f6d7d8`。
只更新隔离 canary 的 `SANDBOX_IMAGES_SANDBOX` 为这个 digest，正式 API 不变。

新镜像下另一条独立 FUSE 请求 `sandbox-dkgfu3hayb` 公开 POST 201
（7.21 秒）、GET 200、DELETE 200。删除后受管 Pod/NetworkPolicy 列表只剩
上一条失败请求的 `sandbox-pool-mbos4yc70j` 及其两条策略；新请求的 Pod 和
实例策略已自然消失。这证明新镜像下的一次创建/删除正例，**不证明**旧 Pod
已修复、无残留的完整回收、产品通用网络策略、长期 TTL/大量存活容量或生产
FUSE 发布。旧 owner 与旧 Pod 均保持占用，直到能通过安全证据链回收。
只读 `--audit-workspace-owners` 在新删除后仅列出两条旧失败请求的 owner，
不含 `sandbox-dkgfu3hayb`；其中 `sandbox-8dmg89anng` 变为
`active_record_present`、`recoverable=false`，不能据此直接删键。
DS 测试策略已按现有对象的精确选择器和端口保存于
`deploy/k8s/ds-ai-research-fuse-backend-egress.yaml`（提交 `d8dc7ab`），
server dry-run 通过；它不随 Helm 自动安装。

隔离 canary 随后升级到包含 `714e011` 清理锚点保护的 API 镜像
`sandbox-fuse-etcd-api:v1-reset-658db78-arm64`，registry digest
`sha256:81865e9d7e757df17b0da9c53e8213d4fc1c03576b396f63542cadd86239c49e`；
Linux arm64 静态 ELF、镜像格式检查和容器 `--help` 均通过，Deployment 1/1。
正式 API 仍为 sync-only 2/2。旧 `sandbox-pool-mbos4yc70j` Pod 仍 Running，
清理仍因旧 probe 退出 2 而失败，不能把保护修复记成既有 Pod 的回收完成。
在这组最新 API/运行时镜像下再跑独立请求 `sandbox-n9h2zfkzjf`：POST 201
（6.82 秒）、GET 200、DELETE 200；只读 owner 审计不含该 ID，受管 Pod
及实例 NetworkPolicy 仍只剩旧失败请求的三项。旧的两条 owner 分别为
`runtime_scope_unconfirmed` 与 `lease_live`，均不可自动恢复；本次正例没有
改变其结论，也不替代 TTL、故障和大量存活验收。

### 最新 canary 的 FUSE 自动 TTL 与文件读写

同一组最新 API/运行时镜像上，独立 FUSE 请求 `sandbox-71n1mtj4h2`
使用 45 秒 TTL：POST 201（7.10 秒）、GET 200，创建后约 53.77 秒
GET 503（正在关闭）、约 60.06 秒 GET 404。验证脚本没有调用兜底
DELETE。后续只读核对中，该请求的受管 Pod、实例 NetworkPolicy 和
workspace owner 均已消失。FUSE 池各元数据汇总中仅旧失败请求所在池
保留 `total=1, consumed=1`，其余池均为 `total=0`。这是一次真实
TTL 自动释放正例，尚不是大量存活、同时到期或故障恢复验收。

另一条独立请求 `sandbox-qfdemprq9m` 在 FUSE 挂载上通过公开文件 API
上传 55 字节（HTTP 200）、下载相同内容（HTTP 200，SHA-256
`3e17a6b4835e1e7b4abbb0f09f30d784b591c2093efa33ed1f9e883c62e8c0c7`），
然后公开 DELETE 200。删除后只剩旧失败请求的 Pod 与两条实例策略；
池汇总仍只有旧请求占用 1 个 consumed。这证明当前隔离 canary 的一轮
真实文件路径读写和释放，未证明长期稳定性或正式 API 发布。正式 API
仍为 sync-only 2/2；旧 probe Pod 仍运行，不能强删或虚报退款。

隔离 canary 扩至两个就绪 API 副本后，`sandbox-6l0go8f3vw` 的跨副本
FUSE 流程通过：A 公开 POST 201，B GET 200，B 上传 48 字节返回 200，
A 下载返回 200 且内容一致（SHA-256
`394db6c27341aa188299d6c5e055a57f92f1214f585732967e4237b039daa3ac`），
B 公开 DELETE 200，A 随后 GET 404。只读复核中受管 Pod/实例策略
仍只剩旧失败请求，池汇总也仍只有旧请求 `total=1, consumed=1`。
这验证了两个 API 实例间共享 etcd 状态的一条真实 FUSE 业务路径；
它不等于双副本并发容量、故障切换或正式服务已切换。

在准备全新部署前，对旧 probe 请求 `sandbox-8dmg89anng` 的唯一旧 Pod
`sandbox-pool-mbos4yc70j` 执行 Kubernetes 90 秒宽限删除。其 sandbox
容器终止、mounter 以 0 退出；`ds-ai-worker-1` 宿主机 mountinfo 对精确
Pod UID `b93c0d9d-d51e-47c7-908d-c5fb1f6ad529` 的挂载记录为 0。
随后仅对该 UID、`Failed` 状态和原 finalizer 的精确条件移除 finalizer，
删除其两条实例 NetworkPolicy。再次列举受管 Pod/实例策略为空。
该旧 owner 在 etcd 中仍不可自动恢复，不把手工终止记为产品退款通过；
它将在已批准的测试命名空间全新重建时随旧测试数据一起退役。

## DS 全新 FUSE release 与正式 Service 验证（2026-10-25）

在旧测试 Pod 退出、宿主机对其 UID 的挂载数为 0 后，以 `--no-hooks`
卸载旧 Helm revision 15；删除旧 namespace 及四个 `Retain` PV 对象，
创建新 namespace（UID `10a43f3f-2dfa-4565-a205-3f4651744f51`）。
四个新 PV 绑定到新 UID 的独立 hostPath；新 etcd 前缀查询 revision 1、
无旧键。四个必要 Secret 仅以本地 0600 文件短暂转存再恢复，恢复后
删除该转存文件。独立 MinIO 新 PVC 就绪，`sandbox-storage` bucket 已创建。
DS 专用 FUSE 后端网络策略继续按精确选择器安装。

FUSE 发布候选固定 API `v1-reset-658db78-arm64`、runtime digest
`sha256:ec108aeee183c1cb0f3a5c481bf38038626fde8da96ae5a4624df396d5f6d7d8`、
mounter digest
`sha256:d35348068bbe2b59a6ffc8f431f30eaf8ab7f94747729d99ac6c999caec8794a`，
以及 DS 已实际装载的 AppArmor profile
`sandbox-fuse-37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d`。
Helm lint 通过、渲染 SHA-256 为
`a6bef95d40453ae2ef439cdf98e54a9b9e8cf450b1672254af8c708d2a7afd51`；
正式 release revision 1 `deployed`，API 2/2、etcd 3/3、MinIO 1/1。
AppArmor 装载当前依赖集群既有 DaemonSet，仍须明确长期归属与节点重启恢复。

- `sandbox-2dpvtseieb`：正式 Service 上 FUSE POST 201、文件上传 200、
  下载 200 且 55 字节哈希一致、DELETE 200。
- `sandbox-8ojjo1pwt6`：正式 API A POST 201，B GET 200 和上传 200，
  A 下载 200 且 48 字节哈希一致，B DELETE 200，A GET 404。
- `sandbox-rvbfa67zxs`：45 秒 TTL，POST 201、约 53 秒 GET 503、
  约 55 秒 GET 404；未调用兜底 DELETE。
- `sandbox-qtvsbb8oim`：FUSE POST 201 与上传 200 后滚动重启正式
  双 API，2/2 重新就绪；下载 200 且原 55 字节内容一致，DELETE 200。

最后只读核对：workspace owner 为 `[]`，受管 Pod 与实例策略数为 0，
FUSE 池汇总 `total=0, consumed=0, cleanup=0`。旧 namespace hostPath
检查中 worker1–3 的旧 UID 路径已不存在；worker4 残留 8 KiB 因首个
UID1000 调试容器无权删除而保留失败结论，随后由 root 调试容器在无旧
挂载、旧 PV 已消失的条件下精确删除。所有调试 Pod 均已删除。

这些是 DS 正式 Service 的有界功能正例。它们不证明实际 10 万 FUSE
Pod 容量、集中到期、节点/etcd 故障下的尾延迟或长期稳定性；当前集群
的 DS 专用网络策略和外部 AppArmor DaemonSet 仍是部署依赖。

### 配置上限内的 FUSE 驻留批次：失败且已回收

对正式 Service 发起 20 个独立 FUSE 工作区创建，最多 4 个请求并发，
每条 TTL 600 秒、池配置 `maxSize=20`。最终 17 次 POST 201、3 次
POST 500；成功请求中已有超过 80 秒才返回的实例。日志确认两次
`publish FUSE binding: fuse pool revision mismatch`，另一次为
`acquire FUSE sandbox: FUSE pool is at active preparation capacity`。
Kubernetes 客户端日志同时记录 GET 在本地限速队列等待约 2–3 秒，
且有一次 API readiness 503。**此批次判 FAIL，不能把配置值 20
称作已验收驻留容量。**

清理阶段 17 条中 5 条立即 DELETE 200、12 条返回 503（清理仍在进行）；
稍后对 17 个精确 ID 的公开 GET 全为 404，受管 Pod/实例策略为 0，
workspace owner 审计为 `[]`，池汇总回到 `total=0, consumed=0,
cleanup=0`，API 再次 2/2 ready。503 不改判为立即删除成功；
它证明这轮失败后的异步回收最终完成。并发 CAS 冲突与准备中容量
拒绝的 HTTP 映射需修复或明确限流，再重新测有界驻留和创建延迟。

### 共享池 CAS 修复与 12 个 FUSE 驻留重测

`6f20463` 修复 Reserved→Binding 等 checkout 转换：只有原记录未变、
共享池汇总独立变化时，才在原请求期限内有界重读和重试。真实 etcd 的
双记录并发测试先红后绿；提交成功但回复丢失时不重试的测试、整包测试、
定向 race 和 vet 均通过。该 ARM64 API 镜像
`v1-reset-6f20463-arm64` 的 registry digest 是
`sha256:1d9cc117ec9cfacc88b22e1cbdbb2049e4dc440bafc76fb33362a971f962b97c`。
DS revision 2 升级后，12 个、最多 4 并发的同类批次中，10 个创建成功，
2 个返回 500；日志不再出现 pool revision mismatch，但两次均为
`active preparation capacity`。该轮 10 个精确 ID 最终 GET 全为 404，
受管 Pod/实例策略为 0。**该批次仍为 FAIL。**

`d66b6e1` 进一步区分冷创建准入的真实满额与独立汇总 CAS 冲突：原
refill 锁、候选 record/index/deadline 和目录均须经失败事务读回确认未变，
只有共享汇总变化才有界重试；真正的 `maxSize=1` 并发测试仍只允许一个
成功，提交回复丢失不补发。真实 etcd 双创建先红后绿、整包测试、
定向 race、vet 均通过。新 ARM64 镜像的二进制 SHA-256 为
`afe4c6a5c64884e9fcd511314681f43da9c5b1f4f388cef770d989552b2cc170`，
registry digest 为
`sha256:8f752d054fdb0c9cfd870cefe2c617c7d19928a46f453764b1e0c5745cbf23ad`。
DS revision 3 API 2/2 就绪后，用此镜像重测：12/12 POST 201，
20 秒后 12/12 公开 GET 200；创建耗时最小 4.85 秒、中位 21.29 秒、
最大 72.69 秒。并发 DELETE 中 6 条立即 200，另 6 条依 API 合同
返回 `SANDBOX_CLEANUP_PENDING` 503。随后 12 个精确 ID 全部 GET 404，
受管 Pod 和实例策略均为 0；etcd FUSE 池元数据的 total、六个状态计数
全部为 0，runtime UID owner 前缀 Count 为 0。两条 rollout API owner
租约键仍在，属于两个活跃 API 实例，并非沙盒 owner。

因此该轮证明**12 个同时存活的 FUSE 沙盒创建与最终回收**，但原试验
脚本要求所有 DELETE 立即 200，仍以退出码 1 保留失败结论。503 不改判
为同步删除成功。创建尾延迟很高，尚未完成更大存量、吞吐、集中到期、
节点故障及十万 FUSE Pod 容量验收。DS 上的实际 AppArmor、对象存储和
网络依赖仍需持续监控。

### 20 个 FUSE 创建与并发清理：安全残留，验收失败

在 revision 3 镜像和同一个 `maxSize=20` 配置下，用批次 `d0867e56`
再次发起 20 个请求、最多 4 个并发。19 个 POST 201，另一个请求在
长时间等待后连接断开；API 日志中该工作区的失败原因为
`FUSE pool refill is already in progress`。创建最慢的已返回请求约
103 秒。中途只读池快照曾为 `total=14, consumed=13, preparing=1`，
同刻受管 Pod 数 14。因为并非全部创建成功，脚本没有执行 20 秒后的
逐个健康读回，不能声称 19 个同时健康。

对 19 个已知 ID 并发删除，7 条立即 200、12 条返回清理中 503。
3 分钟有界读回只有 18/19 到达公开 GET 404。剩余
`sandbox-ogmpe9c9wa` 对应的 Pod 已不在 API 中，受管 Pod 数 0；
etcd 池汇总仍有 `total=1, cleanup=1`，精确 runtime UID owner 键仍在。
持久化终止证据仅有 `process_exited=true`，没有
`graceful_unmount` 或 `infrastructure_fenced`。原 workspace owner
释放合同因此正确拒绝，日志持续报告 `runtime exit is not confirmed`；
另有控制器 `active sandbox stale token`，以及一次其他请求的清理池
revision 冲突。没有强删 owner、改写终止证据或退款。

**整轮 FAIL，DS 当前不具备并发 FUSE 清理发布验收。** 后续须在真实
集群证明该精确 Pod 的卸载与进程域终态，并定位优雅卸载证据缺失的
原因及恢复路径；同时处理冷创建锁等待导致的失败和高尾延迟。
12 个驻留正例及前面的单条 FUSE 功能正例均不能覆盖这条失败路径。

补充只读核查：该精确 Pod UID
`8d5b9974-3a4f-4b08-9c97-40decefd6db2` 在 worker3 的
`/host/proc/1/mountinfo` 匹配数为 0；节点调试 Pod 已退出并删除。
Kubernetes 事件显示其启动后有一次 mounter readiness 请求失败，删除时
两容器收到 Killing 事件；事件不能证明卸载 ACK 曾成功。etcd 终止证据
仍是 `process_exited=true, graceful_unmount=false,
infrastructure_fenced=false`，故即使当前宿主挂载只读检查为 0，产品
仍不能把它自动升级成已签发的卸载或基础设施隔离证明。该 owner 和
配额占用保持，需独立安全恢复；不能靠重复 DELETE 或直接改 etcd
字段消除失败。

后续定位发现 API 首次对该 runtime 的 `remove-runtime` 在 03:57:56 UTC
返回 `runtime termination is unconfirmed`；稍后 pool 保存了仅
`process_exited=true` 的弱证明。现有日志不能区分 mounter shutdown 从未
成功、响应丢失，还是成功后遇到并发删除，不能据此补签 ACK。原流程的
确定缺口是已校验的 ACK 只在 API 进程内保存，直到 Pod 删除后才作为
终止证明进入池记录；另一实例先观察终态 Pod 时会看不到优雅卸载。
提交 `4212869` 在 ACK 校验后、Pod DELETE 前向同 UID/准备尝试的
finalizer 保留 Pod 写入签名回执，并确定读回；重启实例校验后才恢复
`graceful_unmount`。假回执、准备尝试替换、写入失败不放行，写入回包
丢失只作精确读回。Kubernetes runtime 完整包、定向 race、vet 均通过。
这是**预防性代码验证，尚未构建部署，也未修复当前残留 owner**；
20 个 FUSE 并发清理与发布门仍为 FAIL。现有残留需额外的产品认可
的节点隔离/卸载证明，不能用 mountinfo 的一次 0 匹配代替。

### DS 测试命名空间重建与 MinIO Pod 出站规则

用户批准清空并重建专用 `sandbox-fuse-etcd` 测试命名空间，包括测试
MinIO 和 etcd 数据。旧残留 Pod 消失且节点精确挂载检查为 0 后，重建
了独立命名空间、三节点 etcd、MinIO 和两副本 API；没有改写旧环境的
etcd owner，也没有触及 CCE。`4212869` 的持久签名卸载回执和
`8c10e2f` 的有限失败日志已包含在 API 镜像
`v1-reset-8c10e2f-arm64` 中。第一次新环境单条 FUSE 创建仍在
180 秒后超时，池记录停在 binding，随后进入 cleanup；这轮为 FAIL。

节点只读检查显示对应 `fuse.s3fs` 挂载和进程曾存在。进一步在同一
Pod 网络命名空间实测，MinIO Service 与后端 Pod 的健康请求均超时；
仅增加后端 IP 的 CIDR 放行仍超时。改为同命名空间 MinIO Pod 标签、
9000/TCP 的 NetworkPolicy 后，两种地址均可访问。DS 的 CNI 按
后端 Pod 身份匹配该流量，原产品只放行 Service IP 的 CIDR 规则
无法提供实际通路。这个诊断策略仅用于证明原因，随后删除；专用 DS
测试规则按相同 Pod 身份范围保留。它不是对其他集群网络策略的验收。

另有确定的健康探针取消竞态：挂载身份已校验后，客户端断开使缓存
扫描返回取消，原 supervisor 会永久转入 unhealthy。`cc73b6a`
先红后绿修复此状态转换，`4dc5b13` 仅记录固定失败阶段，不写入
请求、凭据或原始错误。新 ARM64 mounter 镜像 digest 为
`sha256:7f76e479ac75cbc3b29298fedeafe253355ab91da7125695a9811d824137cfa1`。
这两项代码测试通过，但不能单独解释或改判第一次新环境超时。

专用 Pod 出站规则就位后，在仍保留那条旧 cleanup owner 的环境中，
新单条 FUSE 批次 `bdb0910a` 完成 POST 201（8.17 秒）、20 秒驻留
GET 200、DELETE 200 和最终 GET 404；测试进程退出码 0。其日志是
忽略目录 `reset-rebuild/fuse-one-with-pod-egress.log`。这证明一条新建
链在该规则下可闭合；旧 cleanup owner 仍占用池容量，故整个环境、
20 并发和长期容量依然没有验收通过。下一步先保存该 owner 的失败
证据并重建专用测试命名空间，再从零执行并发 FUSE 创建和清理。

### 干净命名空间的 20 请求复测：服务端完成、响应交付失败

旧测试 namespace 经用户授权重建。残留 FUSE Pod 的精确宿主挂载先在
worker 节点核对为 1 条，并在宿主 mount namespace 对该路径执行普通
`umount`，再次核对为 0 后 Kubernetes 才完成 Pod/namespace 删除；
没有将一次 mountinfo 缺失当作产品卸载回执。新 namespace 使用独立
etcd/MinIO 数据和上述 MinIO Pod 选择器出站策略。单条 FUSE 请求
`53897883` 完成 POST 201、GET 200、DELETE 200、GET 404。

随后批次 `d512cc44` 发起 20 条 FUSE POST、最多 4 条并发。
17 条客户端收到 201；编号 00、13、14 的连接抛出
`RemoteDisconnected`，因此原测试进程以退出码 1 结束，**本轮 FAIL**。
公开 GET 与工作区路径核对证明这 3 条实际也已创建，对应沙盒 ID
`sandbox-mq6gku0vkh`、`sandbox-63vpl8bwsx`、
`sandbox-755uvqgc7n`。原脚本仅删除收到 201 的 17 条，
其中 DELETE 立即 200 一条、清理中 503 十六条，最终 17/17
公开 GET 404。随后对断连的 3 个精确 ID 分别执行公开 DELETE，
三条均返回 200，最终公开 GET 均为 404。受管 Pod、实例策略和
etcd 池的 record/state/uid-owner 均为 0；池 index/meta 保留。

API 两副本未重启。测试请求经单个 `kubectl port-forward` 转发，
本轮证据尚不能区分转发连接故障与产品响应交付问题。服务端实际完成
20 次创建和 20 次清理不能改判客户端验收失败；下一轮须从集群内
直连 Service，记录响应与清理，并验证 20 个同时驻留的健康状态。
原始日志位于忽略目录
`reset-rebuild/fuse-twenty-clean-egress.log`，SHA-256 为
`d576edfdbcfdbeebfacfd982c1a56cf33cf2b31f985c6f017f8c34c1d1af652b`。

### DS 集群内直连复测与 API readiness 修复

为移除端口转发的不确定性，在同一 DS namespace 启动独立 ARM64
测试客户端 Pod；它通过 Secret 文件读取 API key，直接访问
`sandbox-api:8080`。单条对照完整通过。首轮 20 条批次 `81cfffd3`
的 POST 全部收到 201，20 个受管 Pod 同时 Ready。驻留 GET 前，
两个 API Pod 的 Kubernetes readiness 同时为 False，Service
EndpointSlice 两个地址的 `ready/serving` 均为 false；测试客户端
新建 TCP 连接得到 `EPERM`，删除阶段无法进入 API，**整轮 FAIL**。
直接按精确 Pod 转发仍能取得 `/health=200`，而 `/ready=503`。
因此这不是端口转发导致的创建响应失败；旧请求阶段的三次断连与
Service 端点撤销相符，但没有逐连接追踪，不能断言原因完全相同。

原 readiness 每次调用 Kubernetes Pod List，和创建/清理共用
client-go 限流器；负载期间日志记录多次 1 秒以上的请求前等待，
而 kubelet 探针默认只有 1 秒超时。这是端点全部撤销的可解释路径。
提交 `e89c3c5` 使 readiness 只核对持久状态与两个池 owner，
运行时操作仍逐次检查 Kubernetes 并返回自己的错误；Helm 探针
超时设为 3 秒，覆盖处理器的 2 秒有界状态检查。该变更的定向
RED/GREEN、`internal/sandbox` 与 `internal/api` 完整测试、定向 race、
vet 和 Helm lint/渲染通过。首轮遗留的 20 个精确 ID 经 API Pod
直连公开 DELETE 清理：10 条立即 200、10 条清理中 503，最终
20/20 公开 GET 404；受管 Pod 和实例策略为 0。

另一个部署缺口是 DS CNI 对 MinIO Service 流量按后端 Pod 身份
执行策略。提交 `235b88f` 在原 Helm 网络策略模板增加默认关闭的
FUSE 专用同命名空间存储 Pod selector 与 TCP 端口；空 selector、
非法端口拒绝渲染。启用后的策略通过 DS API Server dry-run。
DS Helm revision 2 使用该策略并删除旧临时同义策略。API 镜像
`v1-reset-e89c3c5-arm64` 的 Linux ARM64 ELF 输入 SHA-256 为
`1a0efc87102dc6eadf50d4850f99f0357574011c07adef1917e4ba67b4d6d300`，
registry digest 为
`sha256:af61b80720b31e7e8fc39f20f85e436adc56676c1c4d8ae9ca7a552839d6eedc`。
两个 API Pod 在滚动升级后均 Ready，重启数为 0。

在此部署上，新的集群内直连批次 `24abde8c`、20 个独立 FUSE
workspace、最多 4 个并发，**测试进程退出码 0**：20/20 POST 201，
20 秒驻留后 20/20 公开 GET 200，创建耗时最小 5.05 秒、中位
20.63 秒、最大 52.05 秒；并发 DELETE 13 条立即 200、7 条清理中
503，最终 20/20 公开 GET 404。两个 API Pod 在满池及删除阶段
的抽检均为 Ready。受管 Pod、实例策略为 0；etcd FUSE 池 meta 的
total/prepared/reserved/binding/consumed/cleanup 均为 0，
active、session、workspace owner/lease 前缀键数也均为 0。
原始日志在忽略目录 `reset-rebuild/fuse-twenty-incluster-readinessfix.log`，
SHA-256 为
`a3175ff37b6ac628a77aca451c415b942f03c95ad4062d91e78af09d4d1b1a78`。

该结果只验收 **20 个 DS FUSE 沙盒的本轮驻留与清理**。先前 FAIL
不改判；它没有验证十万存量、集中到期、节点故障、持续吞吐或
另一套 CCE 集群，也不把测试 MinIO 规则推论为任意对象存储部署可用。

同一 Helm revision 2 又对正式 Service 做一条 FUSE 文件和重启
纵切：`sandbox-l3tbjjjzw1` 创建 201，公开文件上传 200；沙盒仍
存活时滚动重启两个 API 副本并等 Deployment 就绪，随后公开下载
返回 200、55 字节与上传内容完全一致，SHA-256 为
`67ed759d304488ede63a43db5dfb03adcac7c789368e40e6f6b061cf73da7399`。
公开 DELETE 200、最终 GET 404；受管 Pod 为 0，两个 API Pod
Ready 且重启数 0。脚本退出码 0，原始日志 SHA-256 为
`ff095f30dc4a7cb4dbb6a888d110b982f5eec775b38312fe52bb2d367daf45d3`。
该测试证明一条已挂载 FUSE 工作区的文件内容跨 API 滚动重启可读，
并未模拟节点或 etcd 成员故障。

同一正式 Service 的自然 TTL 正例 `sandbox-jewcjxmb5m` 配置 45 秒：
POST 201（5.30 秒），GET 200；创建后约 51.38 秒 GET 503
正在清理，53.48 秒 GET 404。测试没有调用兜底 DELETE，进程
退出码 0。受管 Pod、实例策略、active/session/workspace owner 与
FUSE 池 record 前缀均为 0。原始日志 SHA-256 为
`3736e8ebb9d71337b881a3b61d760584b7d9de2b437a1fa01556847a1ec91de5`。
这是一条自然到期释放正例，不证明 20 条同时到期或故障恢复。

集中 TTL 首次试验 `7419b750` 使用 120 秒 TTL：20 条 POST 均为
201，但整批创建时间加 20 秒驻留等待已使前几条自然到期，驻留
GET 只有 11/20 返回 200。脚本最终观察到 20/20 自然 GET 404，
受管 Pod 和池记录归零，但退出码 1；**不能**把该轮记为 20 条
同时驻留通过。原始日志 SHA-256 为
`5d876197f2a4624a67f36a328d85f5f1c339a43e0c527e145774151956070cc9`。

只把试验 TTL 调至 240 秒后，批次 `6d725f27` 在同一 Helm revision 2
和相同 20 条、最多 4 条并发下，20/20 POST 201；20 秒驻留后
20/20 GET 200。创建耗时最小 4.68 秒、中位 18.03 秒、最大
80.44 秒。其后**没有主动 DELETE**，20/20 个精确 ID 均由自然
TTL 清理达到 GET 404，测试进程退出码 0。受管 Pod、实例策略为 0；
etcd 池 meta 的 total/prepared/reserved/binding/consumed/cleanup
均为 0，active、session、workspace owner/lease 与池 record 前缀
键数也均为 0。两个 API Pod 在结束时 Ready。原始日志 SHA-256 为
`04478886a216e7f6caac977f8bd5165d960c4ab28fcb7b08aec2b31fbe52b455`。
该轮只覆盖 20 条、分布在创建窗口内的到期，不是 20 条同一秒到期，
也不能推出更大 cell 的容量或故障恢复能力。

同一正式 Service 再做一次存活 FUSE 沙盒与 etcd 单成员重启纵切：
`sandbox-jftii0tp8o` 创建 201、公开文件上传 200；删除 `etcd-2`
Pod，确认 StatefulSet 以新 UID 重建且 Ready。随后公开下载 200，
55 字节与上传内容相同，SHA-256 为
`747038a1cf9d579df7e562120346e51346999a59c1d1a2f79ab8704b1298f451`；
公开 DELETE 200、最终 GET 404，测试退出码 0。三个 etcd 成员
最终均 Ready、受管 Pod 为 0。原始日志 SHA-256 为
`9aa0359f4583aa72cdafc479ce2cab2d40c55429e1b93dc57b7e892d5157572c`。
这是一个成员重启的存活读写正例，不证明 leader 重选、两成员故障、
在故障窗口内提交新事务或大存量恢复吞吐。

### DS 以 FUSE 为默认挂载模式

首发优先 FUSE 后，将 DS 全新测试部署的 `config.workspace.defaultMountMode`
从 `sync` 改为 `fuse`；显式 `sync` 入口仍保留。Helm lint 通过，
revision 3 升级完成，两个 API 副本均 Ready。正式 Service 的公开
创建请求故意**不传** `workspace_mount_mode`，返回 201；随后的公开
GET 明确返回 `workspace_mount_mode=fuse`。同一沙盒上传 55 字节返回
200，下载返回 200 且内容哈希一致；DELETE 返回 200，最终 GET 404。
测试进程退出码 0，受管 FUSE Pod 为 0。原始日志位于忽略目录
`reset-rebuild/fuse-default-mode-smoke.log`，SHA-256 为
`24ccdbebd7adefcef6bc253f2c61057880f5c1883b48389cc13675511df19da3`。

这只验证 DS revision 3 的一条默认 FUSE 业务路径。Chart 的通用默认值
仍为 `sync`；全新 etcd 安装的 `values-etcd.yaml` 示例现在明确选择
FUSE 默认、保留显式 sync 兼容入口，Helm lint 和渲染已核对。
其他环境不会因本次 DS 升级自动改变。大量存活 FUSE、集中到期和
故障期间写入的容量门仍未完成。

### DS 20 个 FUSE 沙盒的对齐到期

在 revision 3 上，集群内客户端对 20 个独立 workspace 最多并发发起
4 个 POST。每条请求在发送前以同一个目标 Unix 时间
`1792908511.077` 计算自己的整数秒 TTL（实际请求 208–300 秒）；
不修改产品时钟或 etcd 状态。20/20 创建收到 201，创建耗时最小
4.97 秒、中位 19.32 秒、最大 88.76 秒；到期前 20/20 公开 GET
返回 200。随后客户端**没有主动 DELETE**，公开 GET 逐步观察到
5、12、16、19、最终 20 条返回 404；测试进程退出码 0。

结束后按实际 `sandbox.managed=true` 标签核对受管 Pod 为 0，实例
NetworkPolicy 为 0，两个 API 副本 Ready。只读 etcd keys-only 核对
FUSE `record`、`uid-owner`、各状态索引以及 active/session/workspace
相关键均为 0；常驻配置和池目录等键仍存在。测试客户端 Pod 也已
删除并等待退出。原始日志 SHA-256 为
`26593e3ef02013f4c7ce130f227ca7b595ad86546e0b1890e1b7d06cb86f62a3`，
路径为忽略目录 `reset-rebuild/fuse-twenty-aligned-expiry.log`。

该轮把请求的**目标到期时间**对齐，不保证运行时实际到期精确到同一秒；
最后一条 404 明显晚于其余 19 条。它验收当前 DS 单 cell 上限 20
的这一轮自动回收，不证明更大 N、100k 存活、持续维护负载或节点
故障期间集中清理。

### 存活 FUSE 文件写入跨真实 etcd leader 重建

在 DS revision 3 上，公开创建沙盒 `sandbox-011f4so3jp` 并上传首个
55 字节文件。只读 `etcdctl endpoint status` 证明当时 leader 是
`etcd-1`，随后删除该 Pod，剩余成员选出了不同 leader。在旧成员
仍未 Ready 时，经正式 API Service 上传第二个文件返回 200、下载
返回 200 且字节一致；完成写入后再次核对旧成员仍未 Ready。
旧成员以新 Pod UID 恢复并 Ready 后，首文件下载返回 200，55 字节
哈希与原上传一致。公开 DELETE 返回 200，最终 GET 404；测试进程
退出码 0。三个 etcd 成员、两个 API、MinIO 最终均 Ready；受管
Pod、实例 NetworkPolicy、etcd FUSE record/uid-owner/state 索引和
active/session/workspace 相关键均为 0。

原始日志位于忽略目录 `reset-rebuild/fuse-file-etcd-leader-failover.log`，
SHA-256 为
`a978d227b9102747e5c7c3955cd0f69ecd8624f84929eeafd6a9a5bb4ae54c46`。
这证明当前三成员部署在**一次 leader Pod 重建**时有 quorum 的单条
FUSE 文件写入和安全清理；没有覆盖双成员故障、故障窗口的大并发
提交、长时间分区、节点 FUSE 故障或大量存活沙盒恢复。

### FUSE 自然 TTL 清理跨 API 滚动重启

正式 Service 创建一条 45 秒 TTL 的 FUSE 沙盒
`sandbox-8fpm35f271`，POST 返回 201，用时 5.29 秒，随后公开
GET 返回 200。在创建计时约 35.00 秒时滚动重启两个 API 副本；
Deployment 在约 57.48 秒时恢复就绪，重新连接正式 Service 后
公开 GET 在约 57.54 秒返回 404。客户端没有调用兜底 DELETE，
测试进程退出码 0。最终受管 Pod、实例 NetworkPolicy、etcd 池
record/uid-owner/state 索引及 active/session/workspace 键均为 0，
API 2/2、etcd 3/3 Ready。原始日志在忽略目录
`reset-rebuild/fuse-ttl-api-rollout-overlap.log`，SHA-256 为
`778f30c909d5bb44b43334d392ceaf56edae73dae6bc43600864f2500f519948`。

重启时间窗口覆盖了请求的 TTL 时间；公开结果证明最终安全回收，
但本轮没有逐事务证据区分清理由旧副本完成还是由新副本恢复执行。
它也不覆盖多条同时过期或重启时的节点 FUSE 故障。

### DS 当前部署现场核对（2026-10-25）

对 `ds-ai-research/sandbox-fuse-etcd` 只读查询：Helm release `sandbox`
revision 3 为 `deployed`；`sandbox-api` 2/2、etcd 3/3、MinIO 1/1，
所有对应 Pod Ready。经正式 `sandbox-api` ClusterIP Service 端口转发，
`/health` 和 `/ready` 均返回 200；未认证沙盒查询返回 401，带测试
API key 查询不存在的 ID 返回 404，未输出密钥。

随后经该正式 Service 运行一条当前默认 FUSE 业务烟测：公开 POST 201
创建 `sandbox-bw486aywlp`，GET 200 且 `workspace_mount_mode=fuse`；
上传 55 字节返回 200，下载返回 200 且字节哈希一致；公开 DELETE 200，
最终 GET 404。测试进程退出 0，之后按 `sandbox.managed=true` 标签
核对无残留 Pod 或实例 NetworkPolicy，API/etcd/MinIO 就绪副本数未变。
同一正式 Service 的公开一次性 Python 执行返回 200、退出码 0、预期输出；
其受管 Pod 和实例 NetworkPolicy 随后归零。
这证明当前 DS 测试部署的一条 FUSE 创建、文件读写和删除路径可用，
且一次性执行路径可用；不等于所有 HTTP 接口、真实生产集群、长期
运行或大存量容量验收。
本轮容量分支的 controller 优化尚未进入 DS revision 3 镜像。
