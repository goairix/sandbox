# Kubernetes enrolled management bridge

继续已批准的 etcd master 阶段一至五。本单元把已验收 Destination 的 Dial 接到实际 Kubernetes HTTP exec 协议：只有固定 `/usr/local/bin/sandbox-launcher bridge`，不经过用户 shell/旧 execInPod。交付可调用的真实网络客户端，不创建 Pod、不发业务命令、不发布 etcd、不改 main；下一创建/生命周期编排消费它。它不把点查或传输成功当 native authority、健康、死亡或释放证明。

## 选择

现有 remotecommand.NewSPDYExecutor 默认环境代理、协议回退及 redirect，不符合固定请求要求；StreamWithContext 在 ctx.Done 时可能早于原 streamer 返回。选择现有依赖 github.com/moby/spdystream v0.5.0 的固定 SPDY/3.1 + v4.channel.k8s.io，直接拥有原始 TCP/TLS、HTTP upgrade、Serve 和流；不升级依赖、不复制 SDK executor。相比 WebSocket-only，保留现有 Kubernetes 1.29 时代 exec 协议兼容目标；真正 kubelet/CRI 兼容另验收。

## 合同

`NewEnrolledBridgeClient(EnrolledBridgeOptions) (*EnrolledBridgeClient,error)`：options 为静态 `*rest.Config`、固定 Namespace、MaxActive(1..16)、RequestTimeout(>0..5s)。零值和复制 client 拒绝，constructor 不联网。`Dial(ctx context.Context,target EnrolledBridgeTarget) (net.Conn,error)` 只建立字节连接，供现有 DestinationOptions.Dial closure 或有限 bootstrap 使用；不接收 command/endpoint/path/callback。`Close() error` 封闭准入、取消、关闭并等待本 client 所有实际点查/dial/handshake/Serve/内部 reader 和已进入 Read/Write/CloseWrite 完成；达到上限立即背压，不排队、不按已有 sandbox N 保存对象。

target 为 Namespace 之外固定 attribution：PodName、PodUID、NodeName、NodeUID、NodeBootID、LogicalID、CreationIntentID、Generation、Authority(controlprotocol.TrustBinding)、ContractDigest、UID/GID、NetworkAllowed。字段必须有界、namespace/name/DNS、canonical nonzero UUID、generation>0、digest/canonical authority 符合原规则；复制 DTO 只能约束候选，原 Root-signed activation/runtime cert/management TLS 和 Backend fences 才授权操作。Runtime BootID 由原 birth/TLS 验证，不与 NodeBootID 混同。

每次 Dial 占用一个 slot，有限顺序 GET exact Pod、GET exact Node；无 list/watch/重试/redirect。校验 HTTP JSON≤256KiB、对象类型/namespace/name、Pod UID、无 deletionTimestamp、共享 `InspectEnrolledPlainPod`、完整 config/target tuple、exact node UID/boot/linux；初始 Pod 可能尚未有 ready 状态，此处不以 phase/ready 推断健康。然后只 POST `/api/v1/namespaces/{fixed}/pods/{fixed}/exec`，固定 container=sandbox、command 两项上述绝对路径和 bridge、stdin/stdout/stderr=true、tty=false；GET 与 POST 之间替换同名 Pod 不获权限，正式 birth/TLS exact runtime cert 必须继续拒绝错目标，不声称 Kubernetes exec 有 UID precondition。

静态配置拒绝自定义 transport/wrap/dial/proxy、exec/auth plugins、文件凭证/CA/cert/key、insecure、impersonation、rate limiter/warning callbacks；限制总字节≤256KiB，禁止 CR/LF，origin 固定 http/https 无 user/path/query/fragment，实际生产 TLS 验证。保留 http 仅用于受控 loopback fixture/明确 operator plain origin，报告不得宣称已部署 TLS。可窄提取原 cleanup configure 的纯静态校验/拥有的 TLS/凭证快照到 `static_api_config.go`，原 cleanup transport、预算、tracker、5s上下文和行为不变；禁止嵌入清理 client 共享额度或复制150行配置逻辑。

HTTP GET 用私有无代理、无keepalive、无压缩、无HTTP2、无重试 transport。exec 不用 http.Client 迁移连接所有权：对固定 origin 原始 DialContext/TLS HandshakeContext、限时/有界 HTTP headers(≤64KiB)与错误 body(≤256KiB)，严格101/Connection Upgrade/SPDY/3.1/唯一v4协商，保留 reader 已缓冲字节交给 SPDY，拒绝3xx/其他protocol/重发。每次连接独立、无共享 mutable roundtripper。只有静态 token/basic auth发送到同一固定 origin，不记录凭证。

创建固定四流 error/stdin/stdout/stderr；拒绝服务器主动新流。stdin CloseWrite 发送真实 FIN，保留 stdout；stderr 和 v4 status 分别≤4KiB/16KiB，溢出硬关闭，不无限缓存。status须空成功或严格成功 Status，非零退出/格式错误/超界作为失败。Read 不将中途断连或未验证状态的 stdout EOF 当成功；等待 actual bounded error/stderr readers 后判定。传输不解析/复制管理业务 payload，TLS close_notify/原响应验证保持。

Dial 的 ctx 只约束建立阶段，成功后不把 Destination.Open 的短上下文永久绑定到连接；请求取消、timeout、client Close 的回调必须实际关原 TCP，并等待原建立调用退出才退款。返回 net.Conn 有真实 Read/Write/CloseWrite、地址和 deadline；管理设置任何到期 deadline 可保守中止整条会话，不能以返回ctx.Err代替物理退出。Close幂等，先关闭原TCP以解阻写帧，再 reset/close SPDY、等待原 Serve 与全部本地IO/reader；实际退出前一直占slot。定时器/回调必须停用并join，限制在当前≤16活跃连接，不给idle sandbox常驻ticker。

## 验收和边界

新限定 host/race 和 affected vet，真实 loopback HTTP/TLS/SPDY 协议 server，不注入 transport/成功Dial factory。正例验证三次固定请求、全部query、四流、二进制字节、半关闭后响应与原实际 server/Serve join；取消建立、超时、redirect、未知protocol、bad upgrade、oversize metadata/status/stderr、同名UID/node/完整合同错配、constructor callbacks拒绝、16slot背压、client Close与blocked原IO实际join、成功Dial后原建立ctx取消仍可用。新增测试验证窄提取未改变原 cleanup static拒绝和实际GET。不要重放前Task矩阵。

这是实际 Kubernetes wire client 验收，尚非真实API server/kubelet/CRI、正式launcher跨SPDY的native组合、create/claim/quota/recovery或生产切换；明确记录这些后续项。没有当前真实集群凭证，不接触外部环境。

## 全局限制

仅 apply_patch 编辑源码/测试/文档/格式；不碰 Sentinel，不读 CLOSED OWN，不读真实 Redis/Kube/secrets/fullinspect、不升级/push/deploy。一个 fresh author/唯一 fresh complete reviewer，同pair限定修复，无 nested agent；Root-only expectedHEAD/exactpaths common→本OPEN OWN fcntl Git锁，小步验证提交。所有新host/race/vet按实际GOOS/CGO/race收集GoList全17输入字段，前后filename/SHA精确绑定，保留原RED/失败；不同epoch不冒充final same-source套件。无native fixture请求/运行，除非Root之后给出新增具体批准。用户已授权持续实施master，不重复审批其实现细节。
