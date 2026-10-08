# 可信 workspace 同步接线准备

这是后端归档交付之后的实现约束与源码研究记录，不是 provider、安全释放、生产迁移或容量验收。主方案仍为 `2026-10-06-etcd-state-management-design.md` 的阶段一至五。研究期间未访问云端、输出凭证、改变依赖或部署。

## 已确认的输入边界

归档接收结果仅公开 Close，没有路径、FD、通用 Reader 或 provider authority。后续消费者必须先登记原始 body/consumer owner，再读取已验证 spool；结果 Close 与 Backend.Close 必须封闭新读取、取消并物理等待现有 owner，之后才能删除文件或归还额度。provider 工作可以晚于历史归档会话完成，但须使用新的当前 claim/native intent 授权，不能延长归档票据或复活已失效的发送窗口。

`TaskFinalSyncPlan` 当前只有 Mode 和 Digest；原始不可变记录携带 task、claim、历史排空归属和 AttemptID。实际同步计划还须规范绑定 WorkspaceIdentity、provider、物理 storage identity、bucket、canonical prefix、排除规则与删除策略，并核对 Task.WorkspaceHash 与预留 Plan.Digest。目标 URL 或调用者填写的成功摘要不能授予权限。etcd 已依赖 internal/storage，接线避免反向循环。

样例 `configs/config.yaml` 是 local/sync；现有 factory 还支持 S3、COS、OSS、OBS、MinIO，不能猜测线上 provider 或只实现远端对象存储后宣称全部接线完成。local 需要受信 scoped directory、身份校验、有界流式写入和物理/fsync 证据；bind/no-sync 不能单独证明安全释放，也不能把网络/FUSE 文件系统当作已结算的本地磁盘。

## 原有语义与有界资源

`internal/service/workspace.go` 原同步按 runtime/storage manifest 差异上传，并删除源中不存在、且未被排除的对象。新链路保持现有对象布局、排除和删除语义，禁止排空后再次普通 exec/DownloadTAR。原有全量 map、ScopedFS 全文件 buffer 不适用于大 workspace；采用受限归档索引、已知长度的流式 body、有界分页 listing 和有限分片并发。Listing 只用于观察，不是陈旧 DELETE 的授权。保留 workspace root marker；目录、排除和索引哈希碰撞处理需明确。

每个真实 wire 请求都要有不可变描述符、原始受信 worker UID/incarnation、当前 fences、持久意图和结算结果。SDK 重试、跳转、region discovery、分页、分片及 CompleteMultipart 都是独立请求边界。逻辑 entry 的长度/hash 与签名封装后的 wire 字节可以不同，描述符不能记录认证秘密。全 cell/partition 的意图数量与字节须背压，unknown 不随 lease、年龄或 worker 退出自动回收。

## 已安装 SDK 与 Go 的实际边界

- MinIO v7.0.91 `api.go`：seekable body 可重试；MaxRetries=0 保留默认，1 才是单次 SDK attempt。未指定 region 时存在附带 discovery 请求。错误 body 的 io.ReadAll 与忽略 drain/Close 错误需要独立限额与终结观察。已知 size/DisableMultipart 的单 PUT 也必须遵守实际 provider object 上限；大文件采用有界 multipart。
- MinIO `api-put-object-streaming.go` 的 HTTP 200/UploadInfo 与 deferred `utils.go` closeResponse，不能单独证明实际 response framing EOF 和物理 Close 成功。限额溢出不能伪造 EOF；响应头、ETag 或 SDK nil 也不能代替原始结算证据。
- OBS v3.25.4 有原生 context；MaxRetryCount=0 仍可能跳转。旧 ScopedFS 接线忽略 context。AWS S3 CompleteMultipartUpload 的 HTTP 200 可能包含业务错误，必须验证实际协议结果。
- Go 1.25.6 net/http Transport 可在 RoundTripper 内部重试，包括 HTTP/2 cached-connection fallback。若采用私有 HTTP/1 实现，需显式限制协议、路由与重用，并用真实 TCP/TLS fixture 验证 wire 边界，不能只数 wrapper 调用。
- Request.write 的 WroteRequest 可发生在外层 bufio Flush 之前；提前响应也可能先于 writer 完成。body EOF、SDK nil、DisableKeepAlives 或 CloseIdleConnections 都不能通用证明全部 writer 已物理退出。需要私有原始连接/body IO owner，封闭新 IO、取消/关闭原始 socket 并物理等待实际 in-flight Read/Write。无需 fork net/http。

## 结算、故障恢复与后续验收

原始 provider witness 必须由私有 adapter、原始 request、已验证 body/hash、持久 intent 与物理目标共同产生。任意 callback、io.Writer.Close(nil)、新 HEAD/list 或相同内容都不能证明旧未知写不会晚到。明确区分远端语义终态与 etcd 终态提交结果；终态写响应丢失仅可仲裁元数据，不能重新发送 bytes。

杀死 worker/runtime 只能阻止未来请求，不能结算已发 PUT/DELETE/CompleteMultipart。未知保留 owner/pending，不启动同 prefix 的新写入。无 dispatch 阶段的恢复也需要旧实际 worker 隔离及精确无待发请求证明，不能只看 lease 过期。第一版不引入 gateway/staging，不改变当前对象布局。

当前原始 final-sync/command/发送能力在 manager 重启后不能由记录重建。它们证明保守拒绝，并未完成主方案承诺的正常 manager 故障恢复；后续必须分别验收无外部效果的重启恢复和有未知远端效果的 sticky pending。长期空闲 runtime 的 query/TLS/issuer 证书协调更新同样未完成，不能放松旧证书 Match 或为每个已有 sandbox 增加常驻 actor。

下一实现先规范绑定实际同步计划与原始归档消费身份，再落地持久 per-wire intent、原生 provider/本地 IO 结算、实际基础设施终止及安全释放。之后仍须生产 API/Pool/Docker 接线、分区调度、FUSE/collector、实际大 N 容量/性能、部署恢复与 Redis 移除。
