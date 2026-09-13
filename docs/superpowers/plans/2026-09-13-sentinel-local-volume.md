# Sentinel 本地 PVC 检查 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use executing-plans or subagent-driven-development. 当前分支原地执行，不创建 worktree，不碰业务 PVC；主 agent 提交。

**Goal:** 实现核验器使用的本地私有文件读取，证明空卷与 Configured 实际持久配置，而不是把远端布尔声明当成本地检查。

**Architecture:** 以调用者提供的可信 PVC mount root 为根，拒绝符号链接/硬链接/非当前 UID/非私有状态文件及超限文件。身份和配置连续读取两次，任何差异拒绝；这只是有界一致性检查，不声称跨两个文件拥有 Redis 的原子事务。保留 identity 的排他创建，Reserved 不授权配置或启动。

**Tech Stack:** Go 1.25 os.Root、O_NOFOLLOW、SHA-256、现有解析器和排他身份创建函数。

---

## 文件契约

新建 `internal/redisbootstrap/local_volume.go`、`local_volume_test.go`。

```go
type LocalVolumeSnapshot struct {
    Volume VolumeState
    Snapshot *PersistentConfigSnapshot
    ConfigDigest string
}
func ReadLocalVolume(context.Context, string, ClusterState, Member, string) (LocalVolumeSnapshot, error)
func ReserveLocalVolume(context.Context, string, ClusterState, Member, string) (VolumeIdentity, error)
func ValidateBuiltinSentinelPassword(string) error
```

固定文件名 `identity.json`、`redis.conf`、`sentinel.conf`。configured identity 文件必须实际为 0600、当前 UID、regular、nlink=1；两份配置相同约束，identity 上限64KiB、每配置1MiB。路径/文件内容不出现在错误中。root 必须是调用者已解析的可信 PVC 挂载路径，禁止为空或 filesystem root，不新增 hostPath；os.Root 限制访问范围，最终文件 O_NOFOLLOW。文件名前后 Lstat 和打开文件 Stat 必须指向相同文件，读取两轮必须字节及 inode/mtime 相同。运行中的 CONFIG REWRITE 冲突只返回重试错误，不修复角色。

PVC mount 与父路径必须是可信、稳定的命名空间；打开 root 后另比较目录 inode，但不能授权恶意进程替换 mount。context 在每一步和读取后检查；常规文件的内核 I/O、fsync 不能由 Go context 强制中断，字节上限不是故障文件系统的 wall-clock 承诺。

没有 identity 时只有根真正为空才 Empty=true；仅容许空且非 symlink 的 `lost+found` 作为文件系统元数据，不能忽略任意子目录或旧数据。根入口数最多128，unknown entry 或存在配置/数据时不能预约身份。Reserved 必须只有 identity（和可接受的空 lost+found）；存在半份配置时返回不完整错误，不将其当 fresh，这一 reader 不实现事务续跑。Configured 不检查 Redis 数据内容、不宣称 AOF 无损，只验证身份及实际持久角色/monitor/epoch。

Reserve 调用 ReadLocalVolume；已有合法 Reserved/Configured 同 marker 原样返回（幂等），不改写 phase。真正空卷使用 NewVolumeIdentity + WriteVolumeIdentity 排他创建；并发输家再读并只接受同一固定本地身份，绝不覆盖。配置写入/Reserved→Configured 事务单独实施。

只将真正 os.ErrExist 的排他创建失败视为并发输家，其余 installed-but-fsync-uncertain 错误保留失败；所有成功路径（包括已有 marker 的幂等路径）都同步保留 identity 与目录，再读并绑定同一 expected identity，不能仅靠 reread 确认落盘。成功创建也重新读取并要求同一合法 Reserved identity，发现新增数据立即失败，保留 marker 供审计而不是删除重试。低层 writer/confirmation 依赖分离用于确定性覆盖安装后失败，不向外部开放任意 writer 配置。

密码限制只服务未来内置启动生成器：32..256字节 ASCII 字母、数字、`_`、`-`，拒绝会在 Redis Sentinel raw rewrite 中改变 token 的输入；不是密码熵验证，也不影响外部客户端。

## Task 1：RED

- [ ] 添加测试和临时 stub。初始测试创建 t.TempDir，用实际 WriteVolumeIdentity 创建 private identity，再 os.WriteFile 生成测试配置（测试代码写临时文件允许）；断言真正空、Reserved、非0主Configured均准确返回。

```go
got, err := ReadLocalVolume(ctx, dir, c, member, "main")
if err != nil || !got.Volume.Empty { t.Fatalf("fresh volume: %v", err) }
identity, err := ReserveLocalVolume(ctx, dir, c, member, "main")
if err != nil || identity.InitialConfig != Reserved { t.Fatalf("reservation: %v", err) }
```

- [ ] `go test ./internal/redisbootstrap -run 'TestLocalVolume|TestBuiltinSentinelPassword' -count=1`，确认行为 RED。

## Task 2：GREEN 与拒绝矩阵

- [ ] 实现上述固定文件检查、两轮读取、真实持久配置解释及摘要。
- [ ] 先增加错误测试后收紧：symlink外部/内部、hardlink、mode0644/非regular、旧cluster/member、缺文件/Reserved半配置/无身份数据、unknown非空目录、lost+found非空、超限、取消context、密码space/quote/Unicode/control/short/oversize。
- [ ] concurrent Reserve 仅产生同一个 marker；前后比较文件，重复 Reserve 不改 bytes/mode；configured不得重写。
- [ ] 验证 error redaction，成功 configured摘要变化会随合法配置变化而变化。

## Task 3：复审和验证

- [ ] focused/package/race/vet/lint、gofmt、diff-check；先规格后质量独立复审。
- [ ] 主 agent 提交精确文件。不以此 reader 宣称 role事务、远端证明、namespace CAS、完整 Sentinel Chart或冷恢复通过。

## 执行证据

- 初始空卷/预约/非0主节点观察测试：stub 行为 RED → 最小实现 GREEN。
- 复审发现的 special mode、安装后 fsync 错误被重读掩盖、创建期间新增数据、已有 marker 绕过同步均新增测试观察 RED → 修复 GREEN。
- 独立规格复审通过；独立质量复审最终通过（可信且稳定挂载命名空间、有界文件观察范围）。
- 最新 focused tests：PASS 1.755s；随后 package race：PASS 4.339s，package vet exit 0。提交前仍需主 agent 对稳定源代码运行整体验证。
