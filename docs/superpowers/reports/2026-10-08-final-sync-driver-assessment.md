# Final-sync driver settlement assessment

日期：2026-10-08，基线53b8f9a。只读源码与官方协议调查，供后续可信 final-sync producer 使用；不是 provider 实测、生产接线或远端结算验收。此前接口盘点见 [inventory](2026-10-08-final-sync-release-interface-inventory.md)。本轮未更改依赖、对象布局或已有 FUSE profile 保证。

## 当前发送链的实际限制

仓库 `internal/storage/filesystem.go` 创建 github.com/goairix/fs v0.3.11 各 provider driver，`scoped_fs.go` 只负责路径收敛并转发 Create/Remove。final sync 的 `workspace.go` 依次写入、Close 和删除，以返回 error 决定本地结果。以下观察来自固定模块缓存源码，未打印凭证或请求头。

| 固定 provider / SDK | 命名检查的代码 | 观察 |
| --- | --- | --- |
| S3，aws-sdk-go-v2 v1.41.5 / service/s3 v1.97.3 | fs/driver/s3/writer.go:77、s3.go:38；aws/retry/standard.go:27 | writer 把完整内容缓存至 bytes.Buffer，Close 调 PutObject，只返回 error，丢弃 output。factory 不提供 task transport/retry observer；standard 的默认最大尝试数为3，实际可被 AWS 配置影响，不能把 factory 看成固定单次发送。 |
| MinIO，minio-go/v7 v7.0.91 | fs/driver/minio/writer.go:62/76、minio.go:35；SDK api.go:309/628/665、api-put-object.go:292 | buffer-backed Close 调 PutObject，没有 DisableMultipart 设置；SDK 可自动走 multipart。SDK MaxRetry 默认10，seekable body 可重试，retry loop 会在已发送请求错误后再发；factory 不提供 observer。SDK 的注释阈值不能当本项目固定实际 multipart 阈值，需用选定 options/版本行为测试。 |
| OSS，aliyun-oss-go-sdk v3.0.2 | fs/driver/alioss/writer.go:58/72、alioss.go:33；SDK oss/conf.go:260 | Close 用 WithContext 调 PutObject，只返回 error；SDK 配置 RetryTimes 默认5。此次没有读取所有 OSS 操作的 retry 分支，不能据默认配置断言每个 PutObject 必然发6次。 |
| OBS，huaweicloud-sdk-go-obs v3.25.4 | fs/driver/hwobs/writer.go:74、hwobs.go:31/205；SDK obs/conf.go:190/217/223、http.go:509 | driver Close 仅在入场前检查 ctx，之后调用 PutObject(input)；Remove 没有传入 per-call ctx。factory 未设置 WithRequestContext。SDK 支持自定义 http.Client、请求 context 和重试配置，但这几个设置没有通过 fs.Config 暴露。取消外层 ctx 不能在这里推导实际 HTTP 已停止。 |
| COS，cos-go-sdk-v5 v0.7.65 | fs/driver/txcos/writer.go:62、txcos.go:30；SDK cos.go:213/494 | Close 用 bytes.Reader 调 Object.Put(ctx)，只返回 error。SDK默认 RetryOpt.Count=3，但 doRetry 对 io.Reader body 直接调用 send；不能把所有 PUT 与 DELETE 一概说成三次重试。现 factory 也没有 task observer seam。 |

这些 writer 的 Close 没有本项目 task-scoped 单次消费/已关闭状态；再次调用 Close 可能再次调用 provider 写操作。包装层必须自己保证同一 effect 的唯一发送/Close，不以 io.WriteCloser 接口推导防重放。buffer 全量缓存的内存成本属于活动同步任务及文件大小，不是每个静止 sandbox 的固定开销；后续并发/容量阶段必须计入，不能给所有存活 N 各启动 worker。

AWS 明确区分 SDK 重试策略与单次 API 请求；PutObject 的成功响应说明该请求完整对象已存储，而 CompleteMultipartUpload 可先返回200再在响应体携带失败，必须完整解析结果。[Go v2 retries](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-retries-timeouts.html)、[PutObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html)、[CompleteMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CompleteMultipartUpload.html)。不能把 AWS 的这些保证自动推广到所有 S3-compatible/OBS/COS/OSS endpoint。

推论：只包住当前 ScopedFS.Create/Remove 的最终 error，无法归因 SDK 内的每一次已发送尝试。后一次成功响应也不能凭本地 error 消失证明此前结果未知的尝试不会晚到。nil error、worker 退出、context 取消、对象读回和 operations zero 都不能单独替代父方案要求的完整远端效果结算证明。

## 后续生产者需要落实的接线

1. 从已经关闭 USERS/DATA 的 exact runtime 获取可信只读同步源；普通用户 exec 能力不能越过关闭屏障。runtime source 身份、Boot、worker 身份和最终 effect intent 必须与 original task/current claimant/pins 绑定。
2. 在任何物理外部发送前持久记录不可复用 effect/attempt；原 claim、guard、restore、owner/control/generation/runtime fences 原子验证。不得从缺失记录推断“从未发送”。
3. 可信 producer 自己构造可观察的 provider 客户端/transport，或者增加经过审查的驱动 seam；当前封装不够。每次请求与隐藏 retry、redirect、multipart complete 都要归因，首次未知保持 sticky pending，后续 nil error 不能覆盖。不得修改全局 http.DefaultTransport 来串接。
4. 成功 settlement 由具体已完成/明确拒绝的 provider 结果产生；未发出的 effect 需要独立明确证据。失败/崩溃/失租后不重建原发送能力，不让新 generation 对同 prefix 写入；第一版保留当前对象布局，允许无法证明的远端未知保持 pending。
5. 原生元数据 Stage 的64-node预算必须在设计时算清；新增 effect fences 不能机械追加到已达上限的 quiescence Stage。发送授权、元数据事务 receipt、目标执行 receipt 是不同记录。

这些是父方案的实施约束与已观察的缺口，不是本报告定义的一套已完成新 API。下一单元须选定具体 producer、严格持久记录和可测故障边界后再实施。

## FUSE 保证与剩余范围

已读取 `internal/mounter/profile.go`、`supervisor.go:506–552` 和 `docker/images/workspace-mounter/profile-bundle.json`：四个 bundled profile 均声明 DurableFlushVerified；命令为固定 `/bin/sync -f -- MountPath`，Flush 在 mutex 下验证 Ready/UID/generation/mountID、执行有界 profile flush、复核 mountID 后返回 Accepted。bundle 的全零 s3fs hash 是既有 build-substitution placeholder，不能据此否定实际已发布镜像。

已命名读取私有 OBS rollout plan 的 Task2：其已记录25MiB multipart direct-API SHA读回、sync -f、普通 unmount 与 remount 读回；这里没有重新读取原始云端 spike 全部输出，也没有重放。保留既有 profile 正常 durable flush 契约，不把它降格为仅缓存刷新；新 task/restore/claim/Boot/mount attempt 的持久回执绑定与未知远端写分支仍需新 FUSE 单元接线和验证。此前正常 cloud readback 不能自动覆盖故障中迟到远端写。

未验证：实际各 provider 的故障/重试/延迟发送、可信 final-sync source/worker、已发送 effect 恢复、FUSE 新 task receipt、精确终止/owner 释放、已有 N 与并发容量、生产部署恢复/Redis 移除。无新外部部署或生产存储访问。
