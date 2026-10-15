# Local runtime 安全释放验证

状态：本地普通运行时安全释放单元已范围验收。唯一完整 Task 审查 C0/I1/M2，原作者以 `c20e586` 补齐 I1；同 reviewer 限定 R1 确认 I1 已解决、无新增 C/I/M，Task quality Approved。未归因 EOF、原 composite exit1 和日志限制继续保留。主程序仍使用 Redis。

## 实现范围

BASE `fbbd4dd354be01c635db20cea2300366777bbc44`。十二个连贯源码提交：`6160c36/7a06877/b03dbca/cb09c09/2895ffc/4831539/9c69b64/7baa4f7/f1e2440/fed06d2/55313ee/824c1a8`。独立 Kubernetes cleanup client 依赖另行验收，不重复计为本 Task 实现或重跑其旧矩阵。

支持 ordinary Kubernetes/local sync/enrolled plain no-mount；拒绝 legacy、Docker、FUSE、cloud、none 和未知资源合同。新增 public 原始 archive 本地绑定、TerminateTaskRuntimeLocal、ReleaseTaskWorkspaceOwnerLocal 和两个只读冷查询；真正 local complete、原 enrolled publication、独立 observer 物理证明、七槽精确 cleanup 与原子 owner 释放按顺序组合。没有以 copied diagnostics/bool/Pod404/EOF 代替权限或死亡证明。

固定32 active owner 上限，和原 archiveLife 共用 Close seal；原操作实际 join 后才退款。无按已有 sandbox 数量新增常驻 actor/map/timer/扫描。新增 termination/provider/release 三个固定 Task 键，payload 上限分别16/16/8 KiB；40 KiB 不含键、原有历史、MVCC/WAL/复制/RSS，不能据此称容量已验收。

完整50条保留屏障、whole operations-prefix 和 strict local complete4 均保留；两 enrollment 独立认证后 pin MOD，不提高64节点限额。实际 reserve/terminal/provider/release 为60/61/64节点；release 为60比较+3删除+1回执写。原 Current/birth/unknown 归属不覆写，失 ACK 不重发未知效果；release 后新 owner 已存在，cold receipt 仍独立验证原历史，不重建 ClaimTask。

## 已执行的有限验证

- full7：真实 enrolled Supervisor/public archive/空目录完整本地同步→实际 observer pidfd 死亡和 cgroup 排空证明→cleanup-complete→换 claim 保留同一 entry→64节点 release→新 owner→cold 原 receipt。最初四个 policy absent，只计 skipped；实际2PATCH+1DELETE。
- 六个 enrollment 变化：两条记录各同值 put、加 lease、delete/recreate，均拒绝、零 HTTP mutation、无三条终态记录、owner 保留。
- native ACK gaps：reserve、terminated、cleanup-complete、release，以及 provider 第一槽 intent，验证原 attempt 仅诊断、fresh claim 合法恢复或保留 unknown。它们不等同于 observer send-response gap。
- 七个实际 HTTP mutation ACK gaps：四个独立 owned policy，真实2PATCH+5DELETE；每次请求前检查持久 intent、紧邻 fresh native guard、原 UID/RV；实际效果后丢 ACK，同 claim/fresh claim 均不重发未知槽。raw 计数包含遭 UID/RV 拒绝的重发，不能仅数成功效果。
- 实际 blocked HTTP Backend.Close：取消原请求、等待真实 caller 返回后结束；远端 handler 另用 fixture 控制释放并实际 join，新 release 准入拒绝。
- 七个前置屏障负例：runtime/node UID、真实 claim Lease revoke、restore、complete 同值写、provider UID 替换、Observe→Apply间 resourceVersion 漂移，均零 HTTP mutation、不释放 owner。
- 最大 namespace512/key1024、4096-byte future proof、16/16/8 KiB 独立 record cap、request/两条 response 各262144预检。保守投影最大 reserve182338、terminal/provider198426、release193303请求字节；实际短值 native 请求约15–24 KiB。这是 wire/codec 边界，不是所有最大字段组合均可接受，也不是容量压测。

每个 native epoch 保留原 worker/Supervisor/observer/etcd、handler/Serve、Docker attach/containerWait，精确容器清理与 source/ELF/脚本 SHA。fixture cleanup SIGKILL 从不计为 observer 证明。Root 在各源码提交前独立核对当轮编译输入/产物；没有用后续新增源码伪称旧 ELF 已覆盖。

最终 host17个 `^TestLocalRuntime` 顶层 race PASS（2.316s）、host vet 通过，实际 darwin/arm64/CGO1/race GoList 全17字段8894路径 pre/post filename+SHA 相同。原 composite 脚本随后在 Linux collector 因 Bash3/set-u empty-array 退出1；不能称整脚本全绿。独立 pure Linux continuation 原退出0，linux/arm64/CGO0 全17字段8972路径匹配，etcd ELF 编译及 etcd/controlrunner vet通过，未重复 native 执行。Root 独立审计所有当前文件SHA及产物绑定相同。

最终 host manifest SHA256 `28e24bb618e9e45d909857d01c05e9d57581614c18a826be051d7d066f477926`，Linux manifest `9c9377e0cc10abfdcd486dc4502c4ac8b643060db81d034122d32a76b404c1a5`，Linux ELF `728dac217c8d5dbd19eb1511a955ef8939d42e9e975530433d87dd942002e068`。

## 审查与未关闭项

唯一 fresh 完整 reviewer 审查全部12 source commits/26paths；共享 provider records 只纳入本作者初始接口 hunk，已经范围验收的 provider 实现不重复完整审查。未确认产品代码安全缺陷。Important I1：必须真实丢弃 observer 已完成 Terminate 的响应，在 native termination仍reserved、两槽complete时，证明同attempt不重发、新claim只Query取得原proof，原Current/birth保持，继续cleanup/release/newowner coldreceipt，并数实际Terminate总数1。

R1 `c20e586d945f096a9eb8b1aec224acf32e4c11c4` 只改三个 fixture 路径，新增真实 TLS 双向帧转发器，不改产品。代理从实际 observer 收到并独立验证真实 terminal proof 后才丢第一次 Terminate 响应；后续 Query 原样转发。透明代理 RED 原 build42541退出0/run1744退出1，正确失败于“预期 unknown 但得到成功”；仅启用一次响应丢弃后 GREEN 原 build19427退出0/run44232退出0，顶层1.92s/worker0.71s。实际 Terminate1/Query1、两次真实 terminal proof 响应、只丢一次，原 Current/TaskBirth/CreateRevision 保持，cleanup/release/newowner coldreceipt 完成。两轮原 Wait/attach/container/exactcleanup 均保留，RED容器退出1、GREEN退出0，不能合称整轮无失败。

Root 在释放共享源码 quiet 前独立核对 R1 GREEN runner993packages/9049inputs、monitor171packages/1984inputs 的全17字段 pre/post filename+SHA、当前仓库701/253inputs、实际 ELF/sourcearchive/script SHA、final exit0/noOOM（4f456e退出0）；受影响 Linux controlrunner vet/格式/diff检查 f5ef1c退出0。原 reviewer 只对 c20e586 修复包和原证据作限定复审，确认 I1 ADDRESSED、无新增 Critical/Important/Minor，Task quality Approved；没有新建完整 review seat 或重放旧矩阵。外部依赖和进程/源码证明的原⚠项分别由已验收 provider tracked report 与 Root 的逐 epoch 审计覆盖，master 之外的范围没有被算作本单元交付。

Minor M1：recovery1 在 PrepareAfterDualDrain 前置查询返回 EOF，未进入新的 ACK 注入。现有日志只能定位调用，不能确定是两次原查询中的哪次或底层原因；worker约0.50s退出，不是日志证明的整体timeout。same-source recovery2通过不能消除原失败，尚无产品修复或根因声明，保留为前置链路可靠性调查项。不得把后续 etcd 关闭日志当此前 EOF 的根因。

Minor M2：原 composite exit1 与独立 Linux continuation exit0 分别归档，当前摘要已如实保留。历史早期 host GoList 未带race、测试带race和部分输入记录不足仍明确披露；最终匹配gate只证明最终源码状态，不反向重建历史证据。full1/3–6、proxy handler join 前一轮、HTTP raw计数前一轮、Close 控制fixture前一轮、最大值fixture前一轮等实际失败/限制均保留在当前任务原始记录，不称 pristine。

R1非阻断 O1：原始 embedded etcd 有单端口/simple-token setup告警，以及显式关闭 HTTP/gRPC listener 的 warning/error，之后原etcd Cmd.Wait=SIGTERM。它们来自未修改的 fixture setup/shutdown，保留原日志，不过滤后声称零告警。M1的前置EOF调查仍开放；M2/O1仅确认记录准确，未声称消除原失败或告警。

## 后续边界

本单元不能代表真实 kubelet、生产 main/Manager/API/allocator、其他 runtime/provider/mount、observer1024槽认证回收/长 idle 证书、分区 due/dirty/Watch/FUSE/collector、10k/100k/1m实际存量、MVCC/历史GC、部署备份恢复或 Redis 移除完成。下一独立单元已开始正式 launcher enrollment/签名 ready transport 接线；普通 Pod 创建和安全提供 protected config/clock/node UDS 仍需真实 driver/profile 整体验证。
