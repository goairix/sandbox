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

新部署方面，Chart 已有可选明文或 mTLS 的 etcd 模式，渲染不包含 Redis 资源或环境变量；尚未执行集群安装、真实运行时启动、物理 TTL 退款和 Redis 退役验收。

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

## 当前源码的 arm64 发布镜像打包检查

另以共享 Go 缓存编译 `-trimpath` 的 `./cmd/sandbox`，SHA-256 为
`cd7818a7f9d7faf712ff0fce620fcd1a2e9179e978fbe65db8fffc67064248d5`。
将该二进制与 `configs/config.yaml` 单独放入临时构建上下文，使用
`docker/Dockerfile.prebuilt` 打包，没有在 Docker 内重新下载 Go 工具链或
模块。镜像 `sandbox-etcd-reset:local-smoke` 的 ID 为
`sha256:a13b5797d82e7c263e481265b979472d5ab9332d359c4de094e2faacac340cc4`，
架构为 `linux/arm64`，大小 163,316,128 字节；容器内 `/app/sandbox`
的 SHA-256 与构建输入完全一致。

首次使用 Docker Hub `alpine:3.22` 时，匿名令牌请求连接被重置，构建失败。
核验内部镜像仓库的 `library/alpine:3.22` 同时提供 arm64 和 amd64
清单后，改用该明确基础镜像，完整构建和不带覆盖参数的重复构建均通过。
这仅验证镜像打包和内容，尚未运行该镜像的 Kubernetes Pod，也未推送
到发布仓库。
