# Sentinel 核验服务运行时 Implementation Plan

> REQUIRED SUB-SKILL: subagent-driven-development；原地执行、不访问集群、不提交。

**Goal:** 为原生 sidecar 提供真正可运行且有服务端连接/生命周期限制的只读核验服务。

**Architecture:** 固定监听 TCP :18080；使用 NewLocalObserver 取得实际 PVC/认证 INFO，再交给 NewIdentityHandler 签名。GET /healthz 只检查服务本身，不读 PVC、不连 Redis/远端、不等待 quorum；不预约/改写身份、不初始化、不选主。启动文件读取、Secret绑定、命令和Chart另行集成。

仅新建 `internal/redisbootstrap/identity_server.go` 和 `identity_server_test.go`。

```go
type IdentityServerOptions struct {
    Observer LocalObserverOptions
    PrivateKey ed25519.PrivateKey
}
func RunIdentityServer(context.Context,IdentityServerOptions) error
```

生产入口不能传入 endpoint/provider/listener：固定端口和真正本地 provider。先校验 observer/私钥，生成每次启动随机 session，再listen。unexported server/listener 函数供 httptest/真实 TCP测试，避免测试占固定端口。

net/http.Server ReadHeaderTimeout1s、ReadTimeout3s、WriteTimeout3s、IdleTimeout1s、MaxHeaderBytes4096；禁keepalive。listener 最多32个accepted连接，满时立即Close新连接，不排队等slot。每个connection.Close仅归还一次slot。已有handler8请求并发、2s provider预算仍保留。不得因未初始化或Redis不可用使healthz失败；healthz只允许精确GET路径、无query/RawPath，不通过请求泄露配置。

取消ctx后先用新background context给Shutdown最多3s，超时强制Close所有HTTP连接，取消request BaseContext；返回ctx.Err。服务异常返回常量错误，不返回地址/私钥/原始网络错误。关闭listener/serve goroutine有界收尾，证明signal能结束服务；不声称强制中断内核PVC I/O。server.ErrorLog写io.Discard避免原始request诊断泄漏。

- [x] 行为RED→GREEN：实际TCP服务healthz与签名proof可读、ctx取消停止、健康不依赖provider/Redis。
- [x] 拒绝/界限：异常path/method/query、invalidoptions/key、slowheader/body、32连接上限与slot仅回收一次、shutdown异常请求不hang。
- [ ] focused/full/race/vet/lint，规格后质量独立复审；主 agent 提交前验证。不是完整Sentinel引导/Chart验收。

## 实施证据（2026-09-13）

- 第一轮实际行为 RED：空实现提前返回，`TestIdentityServerActualTCPHealthAndProof` 明确失败 `server returned before serving: identity server failed`；实现后实际 TCP 健康与签名往返、取消退出 GREEN。
- 第二轮 RED：无连接限额时第 33 个连接读取超时，失败 `saturated listener queued instead of closing new connection`；加入 listener admission 后 GREEN。32 个持有连接，额外连接立即关闭；同一连接两次 Close 只释放一次容量，再补一个连接后仍拒绝额外连接；关闭 listener 能结束 Accept。
- 空 context 的拒绝测试先观测到真实 panic，再将其转成明确的断言 RED `nil context panicked instead of failing closed`，校验修复后 GREEN。
- 第三轮实际 PVC RED：生产 observer 绑定构造的空实现失败 `production handler constructor must bind actual local observer`；实现后签名证明读取保留 PVC 中非 0 号主的真实配置/摘要，不依赖 Redis；两个服务启动 session 不同，本地 identity/config 摘要保持不变。
- 在上述 GREEN 后补拒绝/网络界限覆盖：精确 GET healthz、HEAD/POST/query/encoded RawPath 拒绝、无 provider/quorum 依赖、无配置回显、禁 keepalive，慢 header/body、阻塞 response write、超大 header、异常 Serve 错误常量化、已取消生产启动不创建/读取 PVC。补覆盖用例初次已 GREEN，不冒称新的行为 RED。
- 最新 `go test ./internal/redisbootstrap -count=1` PASS（12.909s）；全包 `go test -race ./internal/redisbootstrap -count=1` PASS（15.440s）；`go vet ./internal/redisbootstrap` PASS；`golangci-lint run ./internal/redisbootstrap/...` 输出 `0 issues.`。
- focused `go test ./internal/redisbootstrap -run '^TestIdentityServer' -count=1 -coverprofile=...` PASS（10.444s）。coverage 以拥有文件单独统计，不能将仅跑 server 用例后的全包百分比当全功能覆盖。
- 关闭用例实际等待一个不协作的 handler：3s Shutdown 超时后 server Close 关闭 HTTP 连接，Serve goroutine 与入口结束并返回 context.Canceled；测试最后主动释放该操作。不声称 Go 能强制停止不协作的 PVC 内核 I/O，也不声称完整 Sentinel 引导/Chart/生产 HA 已验收。
- 没有访问集群、Docker、构建镜像或提交；两级独立复审由主 agent 安排，尚待完成。
