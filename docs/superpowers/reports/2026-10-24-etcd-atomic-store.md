# etcd 普通状态适配器验收

基线：`feat/etcd-state-management`，从 `feat/workspace-fuse-mount` 独立切出。此报告仅验收 `state.AtomicStore`，不代表活跃沙盒、FUSE 池、业务接线或生产迁移完成。

## 已验证行为

- etcd v3.6.14 单节点、本地隔离命名空间下，两客户端 Set/Get、SetNX、CAS、条件删除与缺席栅栏通过。
- 一秒租约到期后，旧值不能覆盖新 owner；递增的跨 goroutine 冲突、溢出和保留原 TTL 通过。
- 注入“服务端提交成功、客户端丢失事务回包”后，SetNX 和 Increment 返回不确定错误且不在适配器内重复写入；调用者能读回结果。
- `Keys` 只接受尾部 `*` 前缀；每次请求最多 256 个 key，并固定首批 revision。跨页插入不能混入结果；内嵌 glob 明确失败。
- 取消的缺席读返回错误和 `false`；空值与不存在仍可区分。

真实测试命令：`TEST_ETCD_ENDPOINT=127.0.0.1:64104 go test -race -v ./internal/storage/state/etcd -count=1`，11 PASS；日志 `/tmp/etcd-reset-atomic-store-20261024.log`，SHA-256 `0654721ef649d1a12053476ffa725d8dd584fa55c4c81b14821adb799ae05f9c`。另有 `go vet ./internal/storage/state/etcd`、`go mod verify`、`go test ./internal/storage/state/...` 与三个受影响的 `internal/sandbox` 基线测试通过。后两组未传 etcd 地址，只是编译和原行为回归，不计 etcd 集成证明。

## 明确限制

- etcd 租约只有整秒精度。正 TTL 向上取整，避免提前删除；实际过期可能晚于请求时长不足一秒。`SessionStore.Save` 使用动态剩余时长，因此不能直接拒绝小数秒。上线前须在业务整链复核这段偏差。
- `Keys` 虽按页读取，但返回接口仍汇集全部匹配 key；大量存活沙盒的调用点必须在后续任务改为有界遍历。原 `ordinarypool:v1:*:<pool>:owner:*` 无法直接映射，当前明确报错。
- 本轮是一个单节点明文 etcd 的状态接口实验，没有三节点故障、mTLS、活跃沙盒 Lua 转换、池索引、HTTP/FUSE 或 TTL 退款验收。
- 模块下载时公共 sumdb TLS 握手超时，使用已有共享模块缓存与仓库 `go.sum` 校验；随后 `go mod verify` 返回 `all modules verified`。
