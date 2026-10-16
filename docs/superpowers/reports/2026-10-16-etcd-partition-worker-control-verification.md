# 分区 worker 控制单元验收记录

分支：`codex/etcd-state-management`。不可变 BASE：`afcb7bf96c99eb02fdc1e64bdf53c54623f454af`。实现、验证及原 reviewer 的 R1 限定复核已完成，结论 Approved；本记录仅验收下述控制单元。

## 交付范围

新增可选 worker control：只读固定 operator profile；原 session/同一 Lease/显式续租与单调 deadline；固定 256 分区归属；全 cell 固定 4 个 relist、20 个带宽、100 个 claim 份额；空桶、整数余数、固定 burst 与重复领取保留原对象；真实 callback、RPC 和取消回调的物理 join；接入原唯一 Backend.Close。该能力是后续调度控制基础，不授予 runtime/task 权限。

单 Backend 固定最多 32 个初始化/活跃 session，每 session 固定 256 个分区及 124 个预算槽记账，同时最多一个原生操作。多个份额可由同一 session 持有；不按 sandbox 数量建立常驻续租或全量镜像。profile/guard/partition/share 使用严格 canonical record、真实 key/Lease/revision 绑定；私有原比较把 base/profile/session/partition/share 放入实际控制事务。事务维持 64 节点、256 KiB，record≤2048 字节、key≤1024 字节。

持续持有全部速率份额时合计 20 MiB/s、100 次 claim/s，桶容量合计 5 MiB/100 次；新份额从成功 CAS 和 postguard 后的空桶开始，重复获取不重置额度。四个 leased relist 槽是当前归属份额，四个本地实际 owner 保持到真实回调退出；跨进程交接仍有旧读取尾部，不能称严格全局物理并发上限。持久的 32 个实际任务硬槽尚未实现。

## 小步提交

| 提交 | 内容 |
| --- | --- |
| `996975d` | 固定本控制单元设计与实施计划 |
| `9d15dc1` | 原 worker 会话、严格记录、分区 CAS、实际 RPC/Close owner；13 文件 |
| `75bc989` | 固定共享份额、整数桶、relist 与释放/关闭物理 join；11 文件 |
| `7cc7e87` | 独立三成员实际 native 和 operator 说明；2 文件 |
| `691944c` | review R1：取消后独立原 Lease 清理、profile 原响应校验、公开 callback 合同；6 文件 |

源码、测试、文档和格式均通过 apply_patch 修改。Root 独占 Go/Git/Docker 与格式化执行；无关 Sentinel 文档未读取、修改或提交。

## 行为测试与审查闭环

会话/分区和预算/relist 各自先实际运行 API 尚不存在的 RED，再实现。首次 host/race 分别覆盖 8 组、13 组顶层测试，无 skip；Linux/arm64 vet 通过。第二批的 13 组包括全部 11 个 worker 测试及两个实际共享 Close 的既有回归：snapshot admission 和 archive physical Close。host gRPC server 是协议模型，不能代替实际 etcd 证据。

唯一 fresh reviewer 首轮审阅完整 BASE→`7cc7e87`，发现 C0/I2/M1：I1 释放等待期间取消会把已取消 ctx 带入唯一 Revoke；I2 profile pin 丢失本次 native response 的完整 header/revision 校验；M1 公开 callback 文档缺少同步 self-join 禁止约定。

同一作者先补精确回归。实际 RED 明确复现 I1 的 canceled Revoke，以及 I2 四种异常 outer/nested/revision 响应被接受；合法省略 nested header 的正例通过。随后最小修复：已接纳 session Release 用独立、受原 requestTimeout 限制的 background cleanup；仅 worker profile 本地读取保留完整 response 并验证 outer/nested/KV revision，旧 domain reader 不变；公开 godoc/operator 说明 callback 不能同步释放/关闭会 join 自己的 owner。

修复后 host/race 全 13 组及新增子用例通过，无 skip、warning 或 race；Linux/arm64 CGO0 vet 通过。历史失败保留，未通过改测试规避原问题。

| 最终 R1 检查 | 实际输入数 | 原始输出 |
| --- | --- | --- |
| 行为 RED（exit1） | 8964 | 6100 字节，SHA256 `1b6ea32ea33621f772fd286e59df3f5adb1dfe4d9fbd5cc689cf12f6832a5060` |
| host（exit0） | 8964 | 13932 字节，SHA256 `963a614990d07120f415d38815337ab9d859e59de619af992f6d05a1942307fd` |
| race（exit0） | 8969 | 13932 字节，SHA256 `03d8ade22ffd871015d3b394cd9d9376c3f96ea8a9b2b3beed54ce06c3404c22` |
| Linux vet（exit0） | 9053 | 空输出 |

## 真实三成员结果

本单元新建唯一 native selector `TestPartitionWorkerControlNative`。fixture 自己启动三个独立 etcd3.6.14 成员和一个真实 worker 子进程，逐个持有唯一 Start/Wait 与精确目录清理。未重放旧迁移故障矩阵。

首轮实际 PASS6.73 秒；R1 更改产品后，从新源码重新构建同一测试并实际 PASS7.25 秒。两轮各六个子场景、无 skip/warning/丢弃输出：两个 Backend 竞争同分区、原 Lease 续租/撤销和接管；四个 relist 槽及多份额；真实 worker SIGKILL+Wait 后原 TTL 过期才由新 incarnation 接管；guard/restore/identity 变化后即使恢复原字节，旧对象仍失权；operator profile 保持原记录。

最终 R1 cluster `61d4f4848f871a6`，实际 member ID 为 `55fc17f207108ccc`、`5bc077f63ac8967a`、`ac28794d69757657`。worker PID35 实际 SIGKILL+原 Wait，原 Grant TTL3 秒；三个 etcd PID12/13/14 也实际 Kill+Wait 后才清理目录。这里的 `signal: killed` 是预期进程退出断言，不能误读为未归因测试错误。

最终 runner：Go1.25.6 / Linux arm64 / CGO0 / non-race / ELF64 AArch64，原始 build0，实际 9053 个编译输入；SHA256 `7d53a7cd3f7ebc8984555047a38cbdbc4dfb3d3ddc584c7512d3cdf9a3578e12`。最终 native 原始输出3001字节，SHA256 `5bb4e3017e21f38a8777f5fdf76a74de36905e0ab04e348a5d45f733c66a0175`。

构建与执行逐项核对 Go list 的 17 类实际输入、每个源码/tool 的前后 SHA、归档中每个成员和当前源码；二进制、etcd fixture 与执行脚本 hash 前后保持一致。fixture 从独立批准的缓存新复制，未读取或采用其他已关闭计划的 workspace 产物。

固定容器 image `sha256:26ad49adcb6923b3faa00ee6654943de9bdda7cfcb1c42a9b9cef334a48dfb33`；network none、private PID/cgroup、readonly root、1 GiB/2 CPU/256 PID、drop ALL/no added caps、no-new-privileges、root0:0、/tmp768 MiB1777、两个 runner/etcd 只读挂载、两个显式加五个镜像环境变量。最终容器 `189e3feca6c30ab41e0f25ed36eb9a66e1367d67a33c4436f8a5074d154b2560` 的完整 created inspect 与 before/final inspect 符合该合同；attach Wait0/container Wait0、退出0、OOMfalse、精确移除且 run-label inventory 为空。

Root 第一轮 readonly 清理审计因 Docker 的 `no such object` 小写与审计脚本大写匹配不一致而失败；核对实际错误及空 inventory 后，仅修正内存中审计器的匹配并通过，未修改测试/产品/collector 或重跑该轮 native。R1 RED 首个拟用 epoch 名与已有 handoff 前缀冲突，被 collector 在执行 Go 前拒绝；随后换唯一 epoch 实际执行，无旧产物覆盖。

## 验收边界与后续

同 reviewer 的 R1 限定复核结论 Approved：I1 独立有界原 Lease 清理、I2 严格 profile 原响应校验、M1 公开 self-join 合同全部 ADDRESSED；没有新增 Critical、Important 或 Minor，也没有本范围内未验证的阻塞项。

即使本控制单元验收，真实 due/dirty/固定 revision list→Watch/compaction/有界缓存和 Range/claim/task consumer、持久32任务硬槽、mounted worker、FUSE/collector、远端同步、长期 GC/证书、容量、部署恢复和 Redis 移除仍未完成。默认 main 仍 Redis；小型 native 是控制正确性证据，不是吞吐、实际大 N 容量或生产迁移完成。整体口径暂保持功能代码约70%（65%–75%）、整体交付约45%、生产切换0%。
