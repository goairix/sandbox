# Cleanup：完整审查 I1/I2 修复验证（限定复核通过）

基线 `6519d280876f573df4450ebf0f3ec745226f8c70`。唯一完整 Task 审查提出两项
Important：完整 wire/profile 拒绝晚于永久占槽；目标文件检查占住串行 actor。
本报告记录修复和定向结果。原审查员已完成限定复核：I1/I2 均关闭，Spec Compliance
与 Task Quality 均 Approved，无新增 Critical、Important 或 Minor；不代表生产迁移完成。

## 改动

完整物理 schema/wire 预检现在位于原 issuer 认证后、initialize/reserve/new Run 前。
纯入口只使用认证元数据和未来字段尺寸，返回 error 后丢弃，不创建虚假 occupied
记录、Claim、Kernel 或可复用权限凭据。真实 Grant 前仍重读 E/S/Run，重新构造
一次性 inventory，并绑定原 Task/incarnation/Run/Claim/revision 和 genuine Kernel。
早期和实际入口共用完整 22 families、所有具体变体、原 canonical codecs 与容量上限。

snapshot actor 回合只做原 native 读取和元数据解析。原 owner 实际结束后，同一个
最多 32 个 registry jobs 中的唯一 goroutine 执行真实目标 open/check/Close，再通过
内部 admit continuation 回到原收费 actor，继续 publication/issuer/预检/slot/Claim。
physical 回合只回复该 goroutine，不另起执行者；目标入口拒绝非空 native owner 和
外来 registry 指针。准备失败、取消、Claim 错误、Stop/Backend.Close 均保留原实际等待。
无 Run 时不发布假 Joined，也不推断释放未知占用。

无新公开权限接口、可替换 opener/callback、不可变预算修改、schema 放宽或 runtime
权限扩张。存储协议和生命周期仍为同包私有；文件按准备、尺寸、执行职责组织。

## I1 修复前后实测

首次写入的不可变 recycling budget 即为 4096；worker 配置一致，不删除或替换该记录。
公开 Execute 应明确拒绝不支持的完整 profile，并保持 E 与全部 32 个 S 不存在。

- `preflight-refusal-red-01`：真实失败 2.76s。0 TaskClaim、0 effect、只有 session Grant，
  但 E 已 occupied，Run.Joined=true、confirmed=reserved、Pending.claim 保留；精确空值断言失败。
  原始日志 SHA `47f0f398321aed95b1d7cbcb7893552dbce24b6d91396a6e6fd70accee37e9fc`。
- `preflight-refusal-native-01`：修复后通过 2.41s。同一真实 Task 连续两次公开调用均拒绝，
  每次返回后 E/全部 32 槽为空，零 TaskClaim/close/quiescence/archive/termination/provider，
  原 Task/intent/link/receipt/预算和子历史完整 KV 不变；原 worker Wait=nil。
  原始日志 SHA `a00e087c77b16aa6ea6d8711e17b2ded3bb6728a78f2a5c815a4443fd338f45c`。

正确 profile 使用独立初始化的原完整执行场景验证；没有声称原 cell 支持在线改预算重试。

## 定向检查

host/race 均 13 组、43 子例，分别 24.649s、27.896s，无 DATA RACE 或 warn。
覆盖新 WirePreflight、TargetPreparationJoin、AdmissionContinuation、原五组 wire、
四组 actor 与 CloseCapacity；host SHA
`ceb870a539313a9b82f818dc11d14be2a71750855fbc7e6c709ea9bf3b9b8697`，race SHA
`fce84f33c6a1e38bff49a51fbed1fe103129b06cd29fa64667349ebe92b026c7`。

I2 的 test-owned metadata 准备返回屏障保持期间，真实第二 metadata job 返回，原
KeepAliveOnce 计数和 Status.Renewals 增长；只缩短本地 deadline，不延长服务端权限。
Stop/Backend.Close、job/work/worker Done 均等待解除屏障后的实际返回；admit 接受后取消
仍等待原 actor reply。该测试没有伪造目标成功、Kernel 或可赋权 callback。
它与下面 genuine native 实际目标 open/Close 构成组合证据，**不宣称模拟内核 syscall 挂死**。

| 最新真实场景 | 时间 | 原始日志 SHA-256 |
| --- | --- | --- |
| mounted 完整执行/partial startup/Close | 5.89s | `2a4e4e77979e5a7ce0f6ba821950aacb47b7df4747c4b3e1e8350890dd067e95` |
| bound archive 恢复 | 9.05s | `1610b7f369c300a7bd88d8a964a64480aad279155f7f272bdbbeb14097004c42` |
| observer ACK 丢失查询恢复 | 12.50s | `b5831448c56ab8e64ead83ddeb4aac2eecfeeaa659bcdec2f05673103114fa5e` |
| pending-close 早期恢复 | 9.37s | `c8dc1b9eb2852bfd3c6b9a581c897519bd01584adcc5d7fff03d6f8756317bed` |
| completed owner-held 释放 | 9.53s | `4e18796cba337470f7ebea6c4b040b59cde0f664da8be9e655bcfbca6d5ef05f` |
| 32/33 公开请求与 Stop/Backend.Close | 4.47s | `6f7a0951f87e35155c40447198270a10f2b328409840ff224759bf18c7a9d304` |

以上六项与拒绝场景均通过。Linux etcd/controlrunner vet 通过；gofmt、人工注释/分段/
包职责复查完成。最新 Go1.25.6/linux/arm64/CGO0/non-race runner SHA
`30206191e13e9a009ad8386716171d611b91ee749b1163a373e4f229ab88097d`，monitor SHA
`16306a9fa48ba5a5d8b75a8a005793be7a11c5c8e7ec8b1c1edbc5b7339c94f3`。

每个 Go epoch 的全部 17 类输入、源码/工具前后和每份 archive 均核对；每次 native 后
重核两份二进制输入。每场真实配置、工具归档、退出和清理独立审计，无 OOM/超时，
精确 CID 已删除且 label 清空。RED 审计通过仅表示证据与清理成立，native_pass=false。
三节点 host 环境实际 Wait=[0,0,0]、网络和容器删除、来源归档及两次 host 环境记录审计通过。
native 各场 3 条 error 为 etcd shutdown（close 两模式共 6 条）；warn 为启动、故障注入和
清理 lease-not-found，7/8 条（close 共 16 条），无 gRPC WARNING；未静默原错误。

限定复核范围为 `6519d280876f573df4450ebf0f3ec745226f8c70` 至
`3f5a72a8266780ac0212ee2043a6b26b7a73f611`，只检查 I1/I2 修复及其影响，
没有重复完整审查或重新执行测试。完整 Task 范围验收见 cleanup-executor-acceptance 报告。
自动发现、生产接线、FUSE/
collector、大规模容量、部署恢复及 Redis 移除仍是整体迁移剩余范围。
