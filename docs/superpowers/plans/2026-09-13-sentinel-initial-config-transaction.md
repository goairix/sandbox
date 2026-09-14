# Sentinel 已登记卷初始配置事务 Implementation Plan

> REQUIRED SUB-SKILL: subagent-driven-development / executing-plans、test-driven-development。原地执行，只有主 agent 提交，不调用集群或真实业务PVC。

**Goal:** 将已登记 Reserved marker 的初始配置可中断地持久安装，并且只在所有文件/目录同步成功后返回 Configured；该函数不授权启动或恢复主。

**Architecture:** 固定PVC文件、当前UID0600、同目录排他安装manifest/config、同marker phase原子更新；advisory flock只序列化本地文件事务，非选主锁。保留的Configured永不重写初始角色/epoch；首次配置只允许Pending登记授权。

仅新建 `internal/redisbootstrap/initial_transaction.go`、`initial_transaction_test.go` 和本计划证据。

```go
func ConfigureInitialLocalVolume(context.Context,string,BootstrapRegistration,[3]ed25519.PublicKey,Member,InitialConfigOptions) (LocalVolumeSnapshot,error)
```

## 前置与固定文件

调用者已在真正空卷上预约身份、协调器已登记三marker/公钥。函数验证registration、keydigest、member、本地private identity与该ordinal marker；不创建缺失identity、不替换marker、不根据Pending重新授权空卷，不访问网络、不执行Redis。不提供exportedwriter/step hook。

trusted稳定绝对非根PVC mount同ReadLocalVolume约束；所有state/config/manifest最终文件regular、当前UID、0600、nlink1且有界。固定文件 identity.json、redis.conf、sentinel.conf、initial-config.json、.bootstrap-lock。manifest至多64KiB，含Configured同marker identity、公钥摘要、两份配置bytes（JSON base64仍是机密），严格JSON/精确keys，不记录可重放“当前主”意见。

lock用root.OpenFile O_CREAT|O_RDWR|O_NOFOLLOW|O_NONBLOCK 0600，允许空文件，但须regular/0600/当前UID/nlink1；LOCK_EX|LOCK_NB等待最多5s且检查ctx，每次sleep<=10ms。创建lock后Reserved reader拒绝签名是预期（grant已登记）；重试事务自己读取private identity/manifest，而非把不完整reader当empty。退出unlock/close/rootclose失败保留失败/零snapshot；context与字节限制不保证强制中断内核I/O。

## Configured 路径

锁内重新读取身份；合法同marker Configured：用ReadLocalVolume检查实际配置，分别sync当前identity/redis.conf/sentinel.conf和目录，再ReadLocalVolume要求同一identity/完整snapshot/digest；变化只返回重试失败。绝不比较或覆盖旧initial manifest，也不调用初始生成器重置角色/epoch；允许已经通过Sentinel重写后的非0主配置。此路径不授权exec，不修复不一致的角色/monitor。

## Reserved 路径与续跑

必须registration.Cluster.Pending。使用RenderInitialMemberConfigs得到固定预期configs和Configured同marker身份，构造确定性manifest。

无manifest时根只允许identity、lock、空lost+found，以及该事务遗留的合法manifest临时文件；已有最终配置/AOF/未知目录/其它数据均拒绝。manifest临时文件只在prefix `.bootstrap-plan-`，且private bytes严格等于当前登记下的预期manifest时，才可删除精确该文件；不递归删除、不把未知文件忽略为空。清理后以同目录private fsynced temp + exclusive hardlink安装initial-config.json，移除本次temp并sync目录，不能覆盖已有manifest。任何非ErrExist安装/同步错误不得在同一次调用中通过reread变成功；真正ErrExist只接受预期manifest并自己sync确认。

有manifest时必须private且精确等于预期manifest（严格解析及identity/key/config验证）；凭据/预算改变导致不一致则阻断，不能自动重生成。Reserved根只允许上述固定文件，以及合法事务临时文件；所有其它数据拒绝。临时`.bootstrap-redis-` / `.bootstrap-sentinel-` / `.bootstrap-identity-`仅当内容分别精确等于当前预期redis/sentinel/configuredidentity private bytes时可删除；未知/错内容保留并失败。保留manifest供审计、不自动删除。

配置分别排他安装，存在时只接受exact expected private bytes并sync，不truncate/overwrite配置；安装后的非ErrExist/Sync不确定错误保留失败。完成两份配置后同步两文件和目录，再验证旧Reserved identity与原expected marker unchanged。

Configured identity只改变InitialConfig，marker/cluster/member保持；新private temp fsync后root.Rename原子替换identity.json，sync目录；任何失败返回零snapshot，不以身份读得到推断成功。最后ReadLocalVolume必须完整合法Configured、同marker、预期snapshot/digest；锁内前后identity/files变动拒绝。启动包装器必须在函数成功之后另做冷恢复证据/最终exec，不能仅看到Configured绕过同步失败。

为确定性中断/错误测试可有unexported事务checkpoint hook（manifest安装后、每配置安装后、phase前/phase安装后），public始终nil；hook只允许中断/注入失败，不能把失败改成成功，不对远端开放。

### 排他安装 hardlink 的精确中断恢复

同目录 Link 成功但 temp 尚未移除时，最终文件和该 temp 的 nlink 都为2。仅在已持有本地 private flock 时，事务可窄化修复这一状态：恰好一个固定 final 与恰好一个对应合法 prefix temp，均 nofollow、regular、当前 UID、0600，同 inode，完整 bytes 等于本次已登记的预期目标，实际 nlink 恰好2。只能移除这个已核验 temp，sync 目录并重新确认 final nlink1 后回到普通 private 校验；任何内容、链接数、对应关系或额外 temp 不匹配均失败并保留。通用 ReadLocalVolume 仍要求 nlink1，不增加宽泛例外。须用 manifest、redis、sentinel 三种实际同 inode 双链接文件测试先 RED 后 GREEN。此修复仍不构成任意指令 SIGKILL、断电或 CSI 持久性证明。

## TDD与验收

- [x] actual temp PVC+真实登记/publickeys/Reserved marker，stub行为RED→GREEN，返回Configured同时文件0600/相同marker/可被reader解释。
- [x] 初始身份缺失/错marker/keydigest/Initialized Reserved、olddata/unknown目录、symlink/hardlink/mode/超限manifest、cfg不同/phase改变/ctx取消均拒绝且保留原数据。
- [x] 每个checkpoint中断返回失败，随后幂等续跑同manifest/marker；已经Configured（包括实际文件中的非0主重写模拟）的bytes/role/epoch不被初始化重置。并发调用同marker最终一致；flock超时/锁错误验证。
- [x] 合法/非法临时文件续跑，fsync/phase安装不确定错误不能同调用launder为成功；错误不含credentials/内容/path。
- [x] focused/full/race/vet/lint和先规格后质量独立复审。明确这里只是实际文件+确定性中断，不称SIGKILL/断电/CSI持久性或生产启动/HA验收。

## 本地执行证据（2026-09-14）

- 首轮 stub 的真实行为 RED：registered volume 返回 unconfirmed，manifest/config/phase 安装与同步 checkpoint 尚未触发，Configured 非0主路径拒绝；实现后同一 focused 测试 GREEN。
- 后续真实 RED→GREEN：Configured 同 bytes 的 identity inode 在另一个文件 sync 时被替换；Reserved phase 前出现 dump.rdb；Configured 私密 manifest 被 chmod 0644；三个 final/temp 同 inode nlink2 安装窗口。修复后分别拒绝变化/不安全文件，或仅修复已登记的精确双链接对。
- 8 个调用者的实际文件并发测试暴露并复现 fixed lock 的 O_CREATE open 短暂 ENOENT。只对 lock open 的 ErrNotExist 做受同一 5s 总预算限制的重试，随后重新校验 lock 私密属性和获取 flock；不把这一重试用于 identity/config 安装或 fsync。两轮 `TestInitialTransactionConcurrent -count=20` 均 PASS。日志阶段诊断临时点已移除。
- focused `go test ./internal/redisbootstrap -run '^TestInitialTransaction' -count=1` PASS；包级 `go test ./internal/redisbootstrap -race -count=1` PASS；`go vet ./internal/redisbootstrap` PASS；`golangci-lint run ./internal/redisbootstrap/...` 为 0 issues。独立规格/质量复审由主 agent 安排，尚不在此宣称已获审批。
- 最终 fixture 增强：三个实际 temp PVC 的 Reserved marker 均由本地 reader 读取、独立 Ed25519 key 和随机 nonce 签名，再经 `RegisterFreshVolumes` 生成登记。增强后的 focused PASS（11.653s）、focused race PASS（12.395s）、全仓库 `go test ./... -count=1` PASS（Redis bootstrap 包 31.575s）；最终 vet PASS、lint 0 issues。安装/sync 中断矩阵扩展为14个 checkpoint。计数为正常/确定性文件测试，不把默认跳过的外部集成视为真实部署通过。
- 所有 checkpoint 都操作实际 tempdir/private files。错误注入在同步边界返回失败，包括 identity 已安装后的失败；不是强制让操作系统 fsync syscall 实际失败，也不是 SIGKILL、断电、真实 CSI 或生产冷恢复实验。非0主/epoch 保留为实际持久文件重写模拟，不冒称由真实 Redis failover 产生。
- 此函数只完成本地配置事务，不验证当前主恢复证据、不启动 Redis、不修改 namespace 阶段，也不替代初始化 Job 的 live 拓扑/同连接 ACK 验收。通用 reader 仍无 nlink2 例外。

### 规格复审修正

- 复审确认 Reserved 路径原先没有保留安装/sync 实际检查的 manifest/config inode。实际文件测试 RED 复现：manifest 同 bytes 换 inode 会成功；redis/sentinel 同 bytes 换 inode 会成功，改 bytes 则先推进 Configured 后才拒绝。
- installer 现在返回刚刚成功 sync 并复查的 file info，事务保留三个文件的 inode/mode/size/mtime。phase temp 完成 fsync 后、identity rename 前重新检查 identity、manifest 和两份 config 的精确 bytes/metadata/inode，任何变化保持 Reserved。回归矩阵覆盖三文件、两种变化，并分别在 before-phase 与 phase-temp 同步边界施加变化。
- 更晚边界的实际 RED 还复现 phase temp 同步期间新增 dump.rdb、以及 phase temp pathname 被不同 inode 替换。rename 前验证根恰好只有五个固定文件与本次 phase temp；temp 写入/fsync 自身检查同一 inode 与完整 bytes。失败清理只删除本次原始写入 inode 且 bytes 完全匹配的 temp，不删除占用该名字的未知 replacement。
- 以上均为 tempdir 实际文件变更与确定性 hook 测试，继续不声称 hostile 同 UID 的无锁任意指令竞态、SIGKILL、断电或 CSI 持久性证明。待主 agent 安排同一规格 reviewer 重新验收，再进入独立质量复审。
- 后续复审还确认 post-phase 原有 fresh reader 会采用换 inode 的 core 文件作为新 baseline。实际 RED 覆盖 identity/manifest/redis/sentinel 的同 bytes 新 inode，以及 identity 改 bytes；改后所有四文件的同步记录贯穿安装、rename、phase 后目录 sync 和 Configured helper 成功后最终返回。phase temp 返回实际完成 fsync 的 file info，rename 后的 identity 对比这个原始 inode，不再从 pathname 重新授权。共享 `initialRetainedFile`/validator 统一检查安装记录；已经 Configured 的独立 re-sync 路径复用同一验证器，不重置角色。
- post-phase 变化只返回零 snapshot 失败，不回滚已经提交的 phase，不删除未知 replacement。最新 focused regression `TestInitialTransactionPostPhase` GREEN；完整 focused PASS 10.919s、focused race PASS 13.521s、vet PASS、lint 0 issues。待主 agent 收取输出并安排再次规格验收，不能据本地 GREEN 宣称生产冷恢复已通过。

最终独立 SPEC/QUALITY 均 PASS；完整接线及真实运行验收记录见 [2026-09-14验收报告](../../testing/2026-09-14-built-in-sentinel-production-startup-results.md)。上文阶段性待复审描述保留为历史执行记录。
