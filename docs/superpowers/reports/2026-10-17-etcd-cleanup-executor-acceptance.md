# 真实 cleanup 执行器范围验收

本单元已验收：明确 Task 引用可以通过原 due actor，在全 cell 持久 32 槽约束下执行
真实关闭、排空、归档同步、终止和安全释放，并恢复已验证的 joined 历史。
默认 main 仍使用 Redis；这不是生产迁移完成或完整容量验收。

## 实现与审查范围

实施基线 `c6cee0f7b3a26145e00ad4fb4f61f13a485be71b`，代码终点
`3f5a72a8266780ac0212ee2043a6b26b7a73f611`，共 19 个连贯提交。
覆盖不可变 snapshot v2、持久槽与 Run/Claim 协议、完整 wire 预检、原 actor mailbox、
真实物理执行和生命周期、mounted deployment loader，以及确切历史恢复与结算。
验证细节分别保留在同日 cleanup snapshot、slot、wire、consumer、progress-join、terminal、
bound、observer、persistent32、close、clock、loader、early、pending、owner-held 和
preflight-fix 报告中，不把重叠测试组数累加为独立用例总数。

唯一完整 Task 审查覆盖基线至 `6519d28` 的 18 个提交，结果为 0 Critical、2 Important、
0 Minor。两项 Important 分别为 wire/profile 拒绝发生在永久占槽后，以及目标文件检查
持有串行 actor owner。`3f5a72a` 修复后，原审查员只复核该修复差异及交接风险；两项均
关闭，Spec Compliance 与 Task Quality 均 Approved，无新增或未关闭 C/I/M。
本验收不代替整个迁移完成后的分支审查。

## 最近修复及实际验证

- 完整纯尺寸预检先于 E/S/Run；真实 Grant 仍重读并绑定真实 Kernel、状态、Run 和 Claim。
  同一真实 Task 在不可变 4096 预算下连续两次公开调用，均明确拒绝且没有 E、32 个 S、
  TaskClaim 或物理效果，原持久历史不变；正确 profile 的独立 mounted 完整链通过。
- 同一 registry job 在原 owner finish 后做目标文件准备，再通过 admit continuation 返回
  原收费 actor；不增加第二个物理执行 goroutine。失败、取消、Stop 和 Backend.Close
  仍等待真实工作及资源交接，未知结果不会因租约消失或本地返回而被清除。
- 最近 host/race 均 13 组、43 子例通过，两包 Linux vet 通过；拒绝、mounted、bound、
  observer、pending-close、owner-held、close-join 七场真实 Linux 场景均通过。
  每场实际输入、工具归档、退出和资源清理均审计；原始 RED 和警告保留。

注释、逻辑分段与包职责均经作者、Root 和审查员检查。事务与并发所有权继续保持同包
私有，文件按尺寸预检、目标准备、执行与恢复划分；没有为了拆包扩大权限接口。

## 保留边界与后续

32 槽跨 Backend 仲裁和公开请求 32/33 边界已验证，尚未验证 32 个物理 Pod 并发或
10k/100k/1m 规模、长期运行、故障追赶及吞吐容量。准备等待测试是 metadata 返回屏障与
真实 native 文件操作的组合，没有模拟内核 syscall 挂死；实际 syscall 未返回时，关闭
仍等待其真实 join，但不再占住串行 actor。

冷启动未 joined、未知初始 Run 或无确切 archive 绑定的未知效果继续保留，不能自动证明
旧执行者死亡或手动删槽。已有 pre-PrepareAfterDualDrain EOF 限制保留，本 Task 未定位。
不可变 recycling 预算不支持在线替换；历史证明/回执 GC 尚未实现。

下一单元接通自动 pending 发现、原共享 actor 和 worker 角色。随后仍需完成主入口及
API/Pool/Docker/Upload/FUSE/collector/remote-sync 接线、索引与历史维护、容量与故障验证、
部署备份恢复和权限、默认切换及 Redis 移除。后续不得重复实施本已验收执行单元。
