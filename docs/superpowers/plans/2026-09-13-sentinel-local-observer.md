# Sentinel 本地进程核验 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development or executing-plans；不创建 worktree、不访问集群、不提交。

**Goal:** 将签名协议接到真实本地 PVC 与已认证 Redis INFO，而非接收调用者伪造的 run_id。

**Architecture:** 固定本 Pod 的可信 PVC、cluster/member/masterName 和 Redis 127.0.0.1:6379；Inventory 不连接 Redis，Live 在两次相同 PVC 观察之间获取已认证 INFO server。该模块不预约身份、不改文件、不选主、不配置角色，也不开放任意 endpoint。

**Tech Stack:** 现有 ReadLocalVolume、go-redis v9 固定 RESP2/禁重试/有界超时、context。

---

仅新建 `internal/redisbootstrap/local_observer.go` 和 `local_observer_test.go`。

```go
type LocalObserverOptions struct {
    Directory string
    Cluster ClusterState
    Member Member
    MasterName string
    Password string
}
func NewLocalObserver(LocalObserverOptions) (ObservationProvider,error)
```

构造时验证固定 cluster/member、明确绝对非根 PVC 路径、masterName 为 1..128 ASCII alnum/underscore/hyphen（防止未来 raw Sentinel rewrite）、内置 password 已有校验器；不连接 Redis、不读取 PVC。无 endpoint/client/URL 的导出注入。

Inventory 调用一次 ReadLocalVolume，返回其 Volume/Snapshot/ConfigDigest，RunID 必须为空，即使配置已完成也不调用 INFO；Redis 未运行时可核验 Reserved。该函数不把 Empty/Reserved 升为 Configured。

Live 必须先读 Configured，之后连接固定 `127.0.0.1:6379` 用给定 password/default username 认证，读取 `INFO server`，再读 PVC；要求 identity、persisted snapshot、digest 完全相同才返回 run_id。任一失败、context 取消或不一致返回零 observation 和常量/取消错误，不暴露密码、响应或路径。INFO 响应至多64KiB；恰好一个 `run_id`，必须40lowerhex，重复/缺失/错误格式拒绝；可忽略正常 INFO 其它字段，非法 UTF8/控制字符（除 CR/LF）拒绝。Redis客户端 context timeout启用、Dial/Read/Write <=1s、MaxRetries=-1、PoolSize=1、RESP2、DisableIdentity=true；每次 Live 创建/关闭，关闭失败不能被当成功。新连接在 INFO 后断开，再读 PVC；不复用旧 process 的 run_id，不通过 Sentinel 选主。

unexported 依赖函数允许测试本地观察变化、认证/关闭/context 错误和 INFO 解析；public 使用真实 ReadLocalVolume 和真实 go-redis。测试不能只证明 fake bool；至少用真实本地 TCP RESP 服务核对 AUTH 正确/错误、INFO server命令、run_id读取以及实际客户端超时/关闭；只允许测试内部 endpoint seam，production 固定127.0.0.1。

- [x] 新测试 + stub，真实行为 RED。
- [x] GREEN 固定 provider /实际Redis客户端；补拒绝矩阵、两轮变化和 Inventory 无连接证明。
- [ ] focused/full/race/vet/lint，先规格后质量独立复审；不把此模块称为完整 sidecar/Chart/冷恢复验收。

## 执行证据（2026-09-13）

- 首个实际行为 RED：`TestLocalObserverInventoryReadsActualPVCWithoutRedis` 在 stub 上拒绝已配置 PVC，报 `local observer not implemented`；最小库存读取实现后 GREEN。
- 第二轮 RED：构造拒绝矩阵均被 stub 错误接纳；真实 AUTH/INFO 场景报 `local live observer not implemented`。另观察 INFO 有效响应在 parser stub 上失败、取消路径未保留 `context.Canceled`；实现固定 provider 和真实客户端后 GREEN。
- 关闭路径 RED：实际 TCP 连接测试在关闭 seam stub 上确认连接未关闭；接入真实客户端及关闭错误处理后 GREEN。后续拒绝矩阵/边界测试补的是已实现逻辑覆盖，不冒充每个分支均发生新 RED。
- `go test ./internal/redisbootstrap -run TestLocalObserver -count=1` PASS；`go test -race ./internal/redisbootstrap -run TestLocalObserver -count=1` PASS。
- `go test ./internal/redisbootstrap -count=1` PASS；`go test -race ./internal/redisbootstrap -count=1` PASS（5.869s）；`go vet ./internal/redisbootstrap` PASS；`golangci-lint run ./internal/redisbootstrap` 0 issues。整包可能受其它并行文件改变，以上是当次已读取的结果。
- focused coverage 中本模块 `local_observer.go` 114/120 statements = 95.0%；focused 整包仅约 35.6%，不将仅运行本模块的覆盖率当整包覆盖率。
- 实际本地 TCP RESP 服务覆盖正确 AUTH/错误密码、`INFO server`、run_id、客户端上下文超时与真实连接关闭失败；未运行 Redis 镜像、真实 Redis server、Sentinel 切换、sidecar、Chart 或集群。
- 在 go-redis 解码前限定 RESP2 bulk 宣告长度/帧总字节/数组/深度，避免仅在 `.Result()` 后检查 INFO 长度时已经分配巨大内存。此包装器没有任意导出 endpoint；production 固定 `127.0.0.1:6379`。
- 两轮 PVC 相等不是跨文件/进程原子事务；签名核验端还必须独立重连固定 Redis DNS 校验当前 run_id。故障文件系统的普通文件读可能不能被 context 即时中断，该模块没有新增绝对墙钟 I/O 保证。
- 独立规格和质量复审由父任务安排，尚未在此记录为通过。
