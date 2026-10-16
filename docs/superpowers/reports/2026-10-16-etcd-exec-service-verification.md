# etcd Exec service 验证

基线 `436bef88405406036fb5c2cb159edd98bc70e151`，实现到 `2616a367d22233098c25972f7d8bd4d2dd7ce24e`。唯一 fresh 任务审查：Spec compliant、Task quality Approved，Critical 0、Important 0、Minor 2。本记录验收实际 service 执行链；普通 HTTP/main 尚未切换，容量和生产迁移尚未验收。

| 提交 | 内容 |
| --- | --- |
| `2469576` | owned mounted start/renew signer、固定证书/purpose/binding/digest、只读 Open 和失败/Close 回收 |
| `6777519` | 已认证终态与真实续期失败的窄区分，保留 errors.Is ErrConflict 兼容 |
| `7f50806` | 实际 Exec consumer、统一 MaxActive/wg、长调用期限和原目标读取重用 |
| `2616a36` | 真实长执行、续期/失权/取消/Close/gate 测试及 operator 版本说明 |

Exec 接收完整 ExecutionRequest 和同步 stdout/stderr writer，先验证 descriptor、固定 UID/GID、TTY/network，再验证同部署 profile/history，获取新的原 operation capability 并比较完整 control。沿原 Prepare/Deliver/Wait/Close；单个串行 5 秒 worker 调用原 RenewExecDelivery，不重复续 Lease。停止新 tick 后 join 在途续期，保留真实错误，再关闭原 handle、清理原 Lease，最后确认 context/service Close。旧 Create/Lookup 仍为 20 秒，Exec 为 descriptor timeout+30 秒。

结果明确 rejected/unknown/local_terminal，并保留原 operation/effect ref 和已验证 receipt。RequestID 只做归属追踪，每次调用是新操作，unknown 不自动重试。签名 local terminal 不等 business End，撤 Lease 不证明目标停止。writer 必须返回，Close 等待其真实 owner，不伪装任意阻塞 writer 可取消。

## 结果

三个新 API 的最小 RED 均为预期缺失符号。固定 Go1.25.6 的 targeted host/race 全部通过：signer 3 项、service 6 项及 7 个非法输入子项、Backend 5 项，无 skip/race 报告。deployment/provisioning/etcd/controlrunner 的相关 Linux vet 均退出 0、原日志为空。

Root 分别核对实际 17 类 GoList 输入：signer host/race/vet 8925/8936/9016，service 8918/8929/9009，Backend 8940/8951/9034；每次 action 的实际平台/race、前后路径/SHA、source/tools archive 的每个普通成员均匹配。service host 测试后仅以 apply_patch 对齐测试 map key 的四行空格，原已测字节保留；没有冒称重跑。最终 Linux build/vet/native 捕获格式化后的当前源码，gofmt-d 为空。

最终单 selector `TestExecServiceNative` 实际 PASS 38.83 秒、无 skip。真实 TLS etcd、正式 launcher PID1、原 Kubernetes bridge 和同 mounted signer 构造路径执行：

- 32 秒命令（descriptor45秒）返回原 stdout/stderr、exit7、root wait/drain；已验证 receipt 的 AuthorityDeadline 大于原 NotAfter，证明越过原 30 秒权限窗后的目标实际续期。
- 原 mutation gate、错误 UID/profile 拒绝；原 control 同值改写触发真实 5 秒续期失权，返回 unknown/ref；实际启动后的调用取消、Close、原三入口共并发预算及目标 gate 关闭均通过。
- 11 个正常 bridge 原 Cmd.Wait=0/relay=nil；4 个预标 unknown bridge（3 个取消 killed，1 个目标拒绝 broken pipe）均完成原 handler/Wait/IO join。正式 PID1 Wait=0、原 etcd SIGTERM Wait 已 join。

runner/launcher 分别捕获 9163/2021 个当前输入，实际 ELF 为 Go1.25.6 Linux arm64 CGO0/nonrace，SHA256 为 `e0f1035967594033dcd8be5c700dbd20449cb1c869fdc795dc227cbe3d05a047` / `542b4c58363dd490bf91bbc6816ad3bc3d1efbf4ce0fb5c2723443dd12db38a0`。最终原日志 9465B，SHA256 `e9b81602d1af62cf082d23d8e593e0bd2ea065935d0b712ada88e71ebb4f74e7`。Root 核对新旧执行脚本归档、前后工具/源码/二进制、固定隔离及三个只读挂载；attach/container Wait 均0、无 OOM，精确容器 `74047cd355c8a3830824b31390527f65bac0d57ba311df0c0b91420a487ca759` 已删除，任务 inventory 为空。

## 保留的失败、裁定和 Minor

首轮使用新获取的 etcd3.6.5，在初始 Backend.Status 因 namespace 账号无管理员权限失败（1.11秒），尚未创建 runtime 或进入 Exec。原失败日志/二进制/来源及原 Wait1、清理均保留。Root 对照上游 [3.6.5 Status](https://github.com/etcd-io/etcd/blob/v3.6.5/server/etcdserver/api/v3rpc/maintenance.go#L324) 与 [3.6.14 Status](https://github.com/etcd-io/etcd/blob/v3.6.14/server/etcdserver/api/v3rpc/maintenance.go#L341)：前者检查管理员权限，后者检查认证信息。裁定使用与 client 一致的官方3.6.14，不提升 root、不移除 cluster pin，不宣称所有中间版本兼容。

新 etcd ELF 来源官方 gcr.io/etcd-development/etcd:v3.6.14，registry digest `a491baeaa0cb0c9cd89c0062ac44ece53886e3e5bddad18d2daf36678ce665b6`，实际 arm64 image `2d858271898f1ff44d29b520f4790fac429ff9b54c3b32f85d31a58aa24f5612`；从从未启动的 nonet/readonly/无挂载容器提取后精确删除。ELF SHA256 `9076ea9d5662fdade520a63aa7e312fcde5a4fb7d50a5515d37896204f1c0f15`。仅新 run wrapper/测试 server 改变，复用同一组已验证 runner/launcher，无重建或权限修改。外层缓存 kind 镜像仅提供文件系统，不构成生产镜像验收。

审查保留两项非阻断 Minor：fixture combined-port/simple-token/SIGTERM 及预期 unknown transport 噪声已分类且原日志保留；终态单测尚未定点覆盖在途 renewal 与 terminal/真实错误的两种交接，源代码审查未发现错误。裁定不扩大当前旧矩阵，交由最终整分支审查再评估确定性补测；若判断错误，成本是更早暴露的保守 unknown/竞态回归，不能因此放松真实错误或授权检查。

仍待后续：HTTP 全操作接入、分区调度、FUSE/collector、实际远端同步、历史回收、容量/故障/部署恢复、Redis 移除和整分支审查。本 native 不证明大量已有 sandbox 的性能或容量。
