# Cold quota worker 固定 unary protobuf transport

## 范围与结果

新增五个私有 typed RPC 方法，供后续 paid journal 和原始四 RPC continuation
使用：Grant、guard Txn、Commit Txn、Revoke、独立 journal Txn。它们复用原 Backend
的 ActiveConnection 和原 workerCall，不创建 client、Stage、业务权限或新 owner，
也没有启用自动 quota worker。调用者仍负责原 owner/nonce、完整业务比较、可信 ACK
语义，以及持有原 call 到实际 native return 后再 finish。

原 archiveLife.mu 同时保护 call/session 指针成员关系、cohort 类型、phase 消费、
busy、累计请求字节及可信 Grant ID。复制 call、重建诊断信息、重复 phase、并发
phase 和倒序调用均不能重用该原始调用。Journal 独占一个 paid call，不能与四 RPC
混用；stage 按 Grant→guard→Commit 执行，失败时只保留原 lease 的 Revoke 路径。
本层不退还任何已支付 credit。

## protobuf 边界

每个请求至多 131072 字节；Txn 至多 64 个 comparison/success/failure 节点，
只允许 flat point Range/Put/Delete。预检先拒绝 nested/recursive Txn，再调用生成的
Size/Marshal，避免递归对象图在尺寸预检中越界。原调用者必须保持请求图不可变直至
实际返回；codec 再次校验 Marshal 的尺寸与原 admission charge 相同。

| 固定 phase | response envelope 上限（字节） |
| --- | ---: |
| Grant | 4096 |
| guard | 16384 |
| Commit | 16384 |
| Revoke | 1024 |
| 独立 journal | 16384 |

四 RPC response cap 总计 37888，因此原 stage cohort 的累计请求硬限为
224256 = 262144 − 37888。Guard/Commit admission 为尚未执行的原 Revoke 预留
10 字节；Grant 固定 TTL=10 秒，请求为 2 字节。超过剩余预算会在发送前烧毁该
phase，但保留原 lease 的 cleanup。独立 journal 请求与 response cap 合计也小于
一个 262144 字节 Range charge。

每个 phase 独占 ForceCodec。生成 Unmarshal 前的有限 wire walk 拒绝 unknown
field、重复 singleton、重复 oneof、nested Txn、PrevKv/PrevKvs、超过 64 个
ResponseOp、每个 Range 超过一个 KV，以及超过 258 个消息对象。Grant.Error
单独限为 1024 字节；生成解码前的拒绝不会改写 destination。transport 还检查根
header 的 cluster/revision；journal/continuation 仍须验证完整业务响应及 nested
header。

这些限制仅覆盖应用 protobuf envelope，不包括 HTTP/2、TLS、gRPC status/trailer，
也不把超过 receive cap 后拒收的网络流量算作已接受的 envelope。

## one-shot 与 cleanup

同一 codec 只允许一次 Marshal，包含 clientv3 unary interceptor 再次 invoker 的
情形；每次实际调用同时设置 MaxRetryRPCBufferSize(0)。已核对真实 grpc-go
v1.82.1 源码：unary sendMsg 把 Write 错误转成 nil，随后发送路径以正数五字节
gRPC header 加 payload 超过零 retry buffer，commit 原 attempt；因此之后不能
透明重放已缓存的 payload。建流前仍可能选择/重试 transport，此时尚未发送 payload。

host fixture 为 LeaseGrant 配置了实际 native Unavailable retry policy
（maxAttempts=3，1–2ms backoff），并通过 GetMethodConfig 确认 policy 生效，
使“只有 Marshal 一次但仍重放缓存 payload”的缺陷可由 server send count 发现。
Auth-old-revision、learner、Unavailable、deadline、cancel 场景均通过真实 host
gRPC 边界计数。现有 Backend 的 clientv3.Config 只配置 Endpoints/DialTimeout/TLS，
不配置 username/password；真实 etcd v3.6.14 的 nil authTokenBundle 不触发 Auth
RPC。host 注册 Auth 服务，使零 Auth send 的断言确实可观察。

只有原 Grant 的可信 header 和正 ID 能绑定 cleanup。即使 TTL/Error 或之后的
取消使 Grant 不可用于 guard，该可信 ID 仍可用于一次 Revoke；无可信 ID 时不
猜测或重新 Grant，交由原十秒 lease 自然到期。原 call 仍注册时，即使 publication、
session 或 Close 已取消，也可用原 requestTimeout 有界地完成该 ID 的 Revoke；
外来 ID、复制 call 或提前 finish 均不能获得 cleanup 权限。helper 不主动 finish
原 call，原调用者必须 join 真实 native return。

## Root 验证记录

作者仅使用 apply_patch 修改源码；Go、Git、gofmt 检查与验证 epoch 均由 Root
执行。依赖使用独立 go-mod-private 中的 etcd v3.6.14 与 grpc-go v1.82.1；未使用
sharedLibrary cache，未重播旧矩阵或 Sentinel。

命令为 `go test -v -count=1 -timeout=120s -run '^TestColdQuotaWorkerRPC.*$'
./internal/storage/state/etcd`，Go 1.25.6 / darwin arm64，GOMAXPROCS=2。

- `quota-worker-rpc-red-01`：预期编译 RED，仅缺少批准的 fixed RPC API；9090
  inputs，2123 字节日志，SHA-256
  `6a3c26dae4ec2a7550e8f2fa1e66ddeafd7360a59d99939ec58c8c953b49cde7`。
- `quota-worker-rpc-host-01`：PASS，1.579s；9093 inputs，12243 字节日志，
  SHA-256 `4405bdb2d6657eb8c8c1d0544b233c2e554640fcaf8ffd966ccd9b6caf37591e`。

Root 已完成上述 epoch 的 17 类输入、源码、工具、pre/post/archive/current hashes
完整审计；产品 GoDoc 补齐前的 gofmt -d 与 diff check 均干净。host01 之后只补齐
codec 三段 GoDoc，并修正注释中的“At most 64”，不改变行为或测试。

- `quota-worker-rpc-race-01`：最终注释版本 PASS，3.381s；选择器为
  `^TestColdQuotaWorker(RPC|Admission).*$`，16 个顶层测试包含原双预算准入回归；
  9098 inputs，19855 字节日志，SHA-256
  `1a5839b31b6024613bf50d2f9b8b7f28831b72272e8334fcbc62060887f24410`。
- `quota-worker-rpc-vet-01`：Linux arm64、CGO=0，`go vet
  ./internal/storage/state/etcd` 通过；9188 inputs，日志为空。

每轮检查结束后，Root 均独立核对全部 17 类输入、源码和工具归档、执行前后及
当时当前文件的哈希，再开始下一轮。race 没有数据竞争、失败或跳过；原客户端
在刻意的故障测试中输出的 WARN 完整保留，包含第二次 Marshal 被拒绝的信息。
唯一独立完整 Task 审查已核对最终证据，Spec/Quality 均通过，无 C/I/M 问题。

## 注释、排版与包边界

新增代码按 transport/state、protobuf codec/request shape、有限 wire schema 分成
三个文件。原 workerCall 只增加一份由原生命周期锁拥有的私有状态，不新增全局
codec、可注入 callback 或通用 transport 框架。它保留在 etcd 包内，因为成员关系、
session 取消、Close drain 和 call 注册共享同一并发不变量；未扩大导出接口或改变
依赖方向。注释解释了 phase 失败、重试边界、预算预留和 cleanup 生命周期。

此变更不证明真实 etcd 故障语义、物理 cleanup、自动 settlement、retained-memory
容量、终态 pending gate 或生产规模，也未启用 Kubernetes/FUSE/Redis 默认切换。
