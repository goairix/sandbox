# Kubernetes enrolled management bridge

继续已批准的 etcd master 阶段一至五。本单元把已验收 Destination 的 Dial 接到实际 Kubernetes HTTP exec 协议：只有固定 `/usr/local/bin/sandbox-launcher bridge`，不经过用户 shell/旧 execInPod。交付可调用的真实网络客户端，不创建 Pod、不发业务命令、不发布 etcd、不改 main；下一创建/生命周期编排消费它。它不把点查或传输成功当 native authority、健康、死亡或释放证明。

## 选择

现有 remotecommand executor 的环境代理、回退和原streamer未join返回不符合要求。初选moby SPDY经作者只读预检否决：NewConnection无条件monitor及drainer无法对外join，SPDY解压header/SETTINGS长度会未经cap分配，CloseWait错误可提前返回。最终选择现有gorilla/websocket依赖的唯一`v5.channel.k8s.io`，直接拥有TCP/TLS/HTTP upgrade和本地唯一reader，不用SDK executor或moby连接。无依赖升级、不自造SPDY parser、不提供fallback。

服务端必须支持exec WebSocket v5及stream close translation：Kubernetes1.29起可显式启用TranslateStreamCloseWebsocketRequests，1.31起默认启用，仍需真实部署时验证协商；不提高现有其他功能的最低版本，不保证任意1.29默认配置可用。依据[Kubernetes官方协议说明](https://kubernetes.io/blog/2024/08/20/websockets-transition/)，本地安装client-go websocket.go/v5.go为wire接口依据。初选只读阶段未编写/运行SPDY实现，无废弃产品代码。

## 合同

`NewEnrolledBridgeClient(EnrolledBridgeOptions) (*EnrolledBridgeClient,error)`：options 为静态 `*rest.Config`、固定 Namespace、MaxActive(1..16)、RequestTimeout(>0..5s)。零值和复制 client 拒绝，constructor 不联网。`Dial(ctx context.Context,target EnrolledBridgeTarget) (net.Conn,error)` 只建立字节连接，供现有 DestinationOptions.Dial closure 或有限 bootstrap 使用；不接收 command/endpoint/path/callback。`Close() error` 封闭准入、取消、关闭并等待本 client 所有实际点查/dial/handshake/Serve/内部 reader 和已进入 Read/Write/CloseWrite 完成；达到上限立即背压，不排队、不按已有 sandbox N 保存对象。

target 为 Namespace 之外固定 attribution：PodName、PodUID、NodeName、NodeUID、NodeBootID、LogicalID、CreationIntentID、Generation、Authority(controlprotocol.TrustBinding)、ContractDigest、UID/GID、NetworkAllowed。字段必须有界、namespace/name/DNS、canonical nonzero UUID、generation>0、digest/canonical authority 符合原规则；复制 DTO 只能约束候选，原 Root-signed activation/runtime cert/management TLS 和 Backend fences 才授权操作。Runtime BootID 由原 birth/TLS 验证，不与 NodeBootID 混同。

每次 Dial 占用一个 slot，有限顺序 GET exact Pod、GET exact Node；无 list/watch/重试/redirect。校验 HTTP JSON≤256KiB、对象类型/namespace/name、Pod UID、无 deletionTimestamp、共享 `InspectEnrolledPlainPod`、完整 config/target tuple、exact node UID/boot/linux；初始 Pod 可能尚未有 ready 状态，此处不以 phase/ready 推断健康。然后只 GET upgrade `/api/v1/namespaces/{fixed}/pods/{fixed}/exec`，固定 container=sandbox、command 两项上述绝对路径和 bridge、stdin/stdout/stderr=true、tty=false；GET与upgrade之间替换同名Pod不获权限，正式birth/TLS exact runtime cert必须继续拒绝错目标，不声称Kubernetes exec有UID precondition。

静态配置拒绝自定义 transport/wrap/dial/proxy、exec/auth plugins、文件凭证/CA/cert/key、insecure、impersonation、rate limiter/warning callbacks；限制总字节≤256KiB，禁止 CR/LF，origin 固定 http/https 无 user/path/query/fragment，实际生产 TLS 验证。保留 http 仅用于受控 loopback fixture/明确 operator plain origin，报告不得宣称已部署 TLS。可窄提取原 cleanup configure 的纯静态校验/拥有的 TLS/凭证快照到 `static_api_config.go`，原 cleanup transport、预算、tracker、5s上下文和行为不变；禁止嵌入清理 client 共享额度或复制150行配置逻辑。

HTTP GET用私有无代理、无keepalive、无压缩、无HTTP2、无重试transport。exec使用私有gorilla Dialer，Proxy=nil、压缩关闭、固定v5、内部拥有的NetDialContext/TLS快照；其内部callback不向外暴露。原始TCP/TLS建立与upgrade受同一短期限，原TCP包装在HTTP头解析前限制≤64KiB，错误body≤256KiB；严格101/Connection Upgrade/WebSocket/唯一v5协商、拒绝3xx/其他protocol/扩展压缩/重发。Gorilla保留HTTP已缓冲字节，禁止另绕库重写WebSocket认证。每个连接独立，无SDK后台streamer/心跳；只有静态token/basic auth发送同一固定origin，不记录凭证。

固定binary channels：stdin0/stdout1/stderr2/status3，禁止TTY/resize/其他channel/text/压缩。每条入站WebSocket message≤64KiB（先SetReadLimit再读），stdout待消费队列最多16条，出站stdin按≤32KiB拆message；用户payload不整体缓存。CloseWrite仅一次发送`[255,0]`的真实v5 stdin半关闭并保留stdout。stderr/status总量≤4KiB/16KiB，溢出硬关闭；status沿原v4规则空成功或合法Success Status，非零退出/格式错误拒绝。只有实际WebSocket正常关闭1000且状态成功、stderr为空才可在已交付stdout后返回EOF；裸TCP EOF、1006/其他close或协议错误均返回错误，不能仅凭status流EOF。唯一reader本身收集bounded诊断，无独立SDKheartbeat，Close等待它和本地实际IO。原TLS close_notify/响应认证继续由Destination执行，wire正常关闭不是native证明。

Dial的ctx只约束建立阶段，成功后不把Destination.Open短上下文永久绑定到连接；请求取消、timeout、client Close的回调必须实际关原TCP，并等待原建立调用退出才退款。返回net.Conn有真实Read/Write/CloseWrite、地址和deadline；管理设置任何到期deadline可保守中止整条会话，不能以返回ctx.Err代替物理退出。Close幂等，先关闭原TCP解阻写帧，再关WebSocket、等待唯一reader与全部本地IO；实际退出前一直占slot。定时器/回调必须停用并join，仅当前≤16活跃连接使用，不给idle sandbox常驻ticker。

## 验收和边界

新限定host/race和affectedvet，真实loopback HTTP/TLS/WebSocket协议server，不注入transport/成功Dial factory。正例验证三次固定请求、全部query、四channel、二进制字节、半关闭后响应与原实际server/readerjoin；取消建立、超时、redirect、未知protocol、badupgrade、oversize headers/metadata/message/status/stderr、裸断连/非法close、同名UID/node/完整合同错配、constructor callbacks拒绝、16slot背压、clientClose与blocked原IO实际join、成功Dial后原建立ctx取消仍可用。新增测试验证窄提取未改变原cleanup static拒绝和实际GET。不要重放前Task矩阵。

这是实际Kubernetes wire client验收，尚非真实API server/kubelet/CRI、正式launcher跨WebSocket的native组合、create/claim/quota/recovery或生产切换；明确记录这些后续项。没有当前真实集群凭证，不接触外部环境。

## 全局限制

仅 apply_patch 编辑源码/测试/文档/格式；不碰 Sentinel，不读 CLOSED OWN，不读真实 Redis/Kube/secrets/fullinspect、不升级/push/deploy。一个 fresh author/唯一 fresh complete reviewer，同pair限定修复，无 nested agent；Root-only expectedHEAD/exactpaths common→本OPEN OWN fcntl Git锁，小步验证提交。所有新host/race/vet按实际GOOS/CGO/race收集GoList全17输入字段，前后filename/SHA精确绑定，保留原RED/失败；不同epoch不冒充final same-source套件。无native fixture请求/运行，除非Root之后给出新增具体批准。用户已授权持续实施master，不重复审批其实现细节。
