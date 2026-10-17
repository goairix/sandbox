# Cleanup 公开32/33请求与关闭等待验证

本片只修改四个测试文件，产品代码、权限接口和原配置上限不变。用两类互补证据验证队列和关闭：真实Linux startup Kernel/profile下调用公开 `ExecuteCleanupLocal`，以及同包host测试扣住真实etcd调用的本地返回。远端gRPC handler由fixture负责Close/join，不能把客户端已返回之后的远端等待归给Backend。

Linux两个fresh子例分别调用Stop和Backend.Close。第一条真实Task停在原execution点读的真实响应返回屏障，随后并发32条不同、合法路由的语法请求，恰好一条ErrConflict，余31条加原任务共32条未返回；同一被拒ref立即重试仍拒绝，Task admission/Grant/Claim/归档计数不变。额外语法请求没有生产历史或物理权限声明；前片四Backend的33条真实生产Task持久槽仲裁为独立证据。q=3s、入队观察2s和原150s测试/180sattach上限均未放宽。

Host在真实三节点调用已经执行后延迟本地方法返回，锁内只读确认1个admitting、31个queued、原session/native owner/share一致。取消关闭后，owner、全部32个jobDone、workDone、finalDone及关闭调用必须仍等待；解除屏障后实际收齐。失败路径也先解除屏障再等待全部goroutine。已检查注释、逻辑分段、包职责及gofmt/diff-check。

## 验证结果与失败记录

- `close-host-01`：1组2子例通过，2.19s、包3.498s，9019输入；日志1806B，SHA256 `fc5c7140ee30e1f2d63e6facb48333878f98f5331f7e3e1abb78a6cdd57cd7c2`。两个关闭返回均nil，原owner、32个jobDone、workDone和finalDone实际joined。
- `close-race-01`：相同1/2通过，2.28s、包4.708s，无数据竞争，9024输入；日志1806B，SHA256 `4f1f0ba192d3e4a43685668b6fba32c0e233bd5628f2cd7220054467ea7bdd42`。host/race均无WARN/FAIL/SKIP。产品和host测试未再变化，之后未重放这些已通过单元。
- etcd Linux vet通过，9110输入；controlrunner Linux vet通过，9277输入。新runner/launcher构建通过，输入9277/2043，ELF64 AArch64 SHA256分别为 `601f5c0bb979af50e1b142f8f49cb82505ce6d9cf8a5e76689e0d10c8b2009e9`、`666749838c9e2aabe856578df755f89d2f0a6483ddb79047c5d7004d30d223de`。全部Go epoch的17类输入、源码/工具、前后/当前SHA及每个归档成员在后续Go/源码修改前独立核对。
- 首轮 `close-native-01` 整轮失败4.50s，Stop子例通过；Backend.Close子例已完成32请求返回、Close=nil和Done，但新测试错误地只允许stopped/unknown，实际为invalid。日志11350B，SHA256 `3667b072b28141f011875db53e4e620a2f6b200360f716cca27c6f6880cbe566`；完整运行、输入、工具、真实失败退出及清理先行审计。源码确认Backend.Close标记session lost，actor后续live检查返回GuardExpired，原finish映射为invalid；Close只汇总资源清理结果，不汇总worker历史错误。仅修正新测试，记录并校验实际Stop错误和原256字节LastError，仅Backend.Close的精确非Unknown GuardExpired允许invalid，未放宽其它错误或修改产品语义。
- **`close-native-02/close-join`通过，4.47s**：Stop/backend-close子例2.24s/2.23s。两者公开32/33及同ref重试均通过；只有session Grant、一次Task admission读，TaskClaim/归档写均0。Stop实际worker错误nil、phase stopped；Backend.Close返回nil，实际worker Stop保留 `ErrGuardExpired`、phase invalid、LastError精确一致。全部32个Execute返回，Done/status、重复Close、proxy handler/pumps实际join、两个worker实际Wait=nil。父进程独立确认无E/S/Claim/归档/provider/bridge效果，再由fixture SIGKILL并实际Wait仍存活的Supervisor；这不是清理完成或终止proof。日志10869B，SHA256 `27217550738e14089626219aed8c5529559425311b1e1932661ede4b131c0567`。
- native真实attach0/Wait0、无OOM/timeout、固定参数、工具归档、二进制/etcd输入、确切删除和空清单独立核对通过；16WARN/6ERROR，包含取消和fixture关闭监听，原始记录保留。关闭case有独立无效果与实际join断言，没有弱化原complete/recovery结算断言。
- close-01三节点host实际退出均0，无OOM，容器和网络删除、空清单、实际Compose/工具归档/生产者和全部host环境独立核对通过；成员WARN21/19/21、ERROR4/5/5（取消Range/关闭监听），记录保留。

## 剩余范围

部署loader、尚不支持的恢复阶段和唯一完整Task审查仍待完成。本片不是32个物理任务并发、大量存量沙盒容量或生产切换验收。
