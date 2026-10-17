# Detached no-effect writer 组件验证

代码提交：`58f38fd`（独立工作树 `7f738ab`）。仅接受 Task3 的原生 writer、
历史 reader 及原 attempt 仲裁组件；完整 Task3、自动消费者与生产切换仍未完成。

## 实现范围

原 registry job、Kernel、Claim、Run、E/S 与双排空是独立前提。真实 signed plain
来源与原 none 历史认证后，原 job 持久固定 Pending/attempt，再执行一次
60 比较、Put1、Else3 的 create-once settlement。Unknown 只仲裁原 draft；
不同原生值是 Conflict，原 Claim 失权后的 committed 结果只作附 error 的诊断。
原 none Claim 与后续 writer Claim 分别保留，不重建或覆盖历史计划。

准备入口在 Claim 锁外认证原 job/Kernel，锁内检查不反向获取生命周期锁。
Pending 只接受空 final-sync handoff 或本原 draft 的精确坐标。记录、原生读取、
writer 分文件，纯 codec 留在 controlprotocol；finish 尾部清除原 draft。

## 最终验证

Go1.25.6，Linux/arm64、CGO0；使用主仓库共享构建与模块缓存。
`writerfinalbuild01`、`writerfinalvet01` 均 exit0、9239 个输入，完整输入、
工具、源码与前后指纹审计通过。最终测试 ELF SHA256：
`59eec745865f4272e1d62647163c6dab05cf91e39410fb480e3d24243fb93427`。

真实原 TestMain 启动三个 etcd3.6.14 子进程。容器以非 root65532、零 capability、
NNP、只读 rootfs、network none 运行，仅绑定两个只读 ELF，工作数据在有界 tmpfs。
本轮没有拉取镜像、开放端口或访问生产数据。

| 真实 native 用例 | 结果 |
| --- | --- |
| OriginalWriter | PASS，实际 Query 重验、64 节点事务、复制 job/draft 拒绝、重复只读 |
| LostACKRetainsAttempt | PASS，服务端真实提交后丢 ACK，死 Claim 保留 committed 诊断且不重发 |
| AbsenceStaysUnknownUntilJoin | PASS，absence 保持 Unknown；原真实 join 推进 E.mod，迟到全事务及独立旧 E.mod CAS 均失败 |
| RejectsIncompatiblePending | PASS，archive、termination、外来 final-sync Stage、外来 detached attempt 四个子场景不被覆盖 |
| ClaimLockedLivenessAvoidsLifecycleLock | PASS，确定性持锁调度验证无反向获取生命周期锁，失败路径也会实际 join |
| UnknownForeignRawConflicts | PASS，真实外来 malformed KV 返回 Conflict，不重新发送 |

上述六个顶层用例及四个子场景全部通过，无 skip、FAIL 或 OOM。
原三个 etcd 子进程均实际 Wait/join，停止后仅删除精确自有容器。
运行 `run-d73ef09f849448efb6a4d9357757f7db` 日志129159字节，SHA256：
`350b936a4e73b664530dcb4d78e3174008982a1f47964a7d46946b2e682228d9`。

另一个真实三成员运行 `run-cb4d01f669f14b6aadd82b8d2a0310fc` 验证固定五点
transport 组件：32768/4096/8192 字节边界通过，各自加1拒绝。实际14节点，
请求6923字节、实际回复48606字节、最坏投影49001字节，符合128KiB/256KiB。
该组件值刻意没有 signed schema，不能把成功点读算作已认证的大 quiescence 历史。
最终两场运行的11项源码指纹及 ELF 哈希均保持不变。

## 反例、审查和限制

初次 native 在 fixture 共用活 actor session 时冲突，未进入 writer；改为独立真实
准备 session 完全释放，再由原 actor 创建 Run。等待原 owner 后还排队等待 actor
尾部完成，不把一次 barrier 当作后续独占。原失败日志保留。

三个严格性问题先由真实 native 断言复现，再最小修复。审查新增的16KiB限制先以
真实32KiB点读失败复现，再对齐原 quiescence codec 的32KiB上限；其他上限未放宽。
唯一原 Task3 审查员限定复核 Spec/Quality PASS，无阻断，未重复审查已关闭 codec。
原始命令、环境、manifest、指纹、Docker inspect、失败及成功日志保留在独立工作树
`.superpowers/sdd/2026-10-18-etcd-detached-noeffect/`。已审计的旧可重建二进制及时清理。

这是真实元数据 writer 验证。fixture 的 mTLS 签名只证明归属，不证明受保护 PID1、
cgroup 空集或 managed admission。CGO0 Kernel 原生路径没有被 Darwin race 测试
覆盖，不将 host skip 算作 race 通过。

冷新 Run 门禁及其实际接线、retained graph 内存核算、真实 signed 超16KiB输入、
v2 termination/observer 正向贯通、owner/terminal/quota、实际消费者与部署仍开放。
本轮不是32物理并发、大量已有 sandbox、性能、容量或生产切换验收。
