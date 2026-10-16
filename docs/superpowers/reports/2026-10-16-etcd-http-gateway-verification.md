# etcd HTTP gateway 范围验证记录

本单元已范围验收。同一 reviewer 的 R1 限定复核确认 I1/I2/I3 全部解决，无新增 Critical/Important，spec 与质量 Approved。基线 `4cb308b6629b6621367d8e1b312241c5bbd228c6`；初审完整 HEAD `fea854d4f46d81bb56d26029ad4d22c70858bc1a`，R1 HEAD `cdcc8594f894153acd8b1a493e71a69fa744bbe3`。本单元提供显式、受限的 etcd HTTP 模式，不代表整个 Redis 迁移或生产部署完成。

## 已实现范围与提交

| 提交 | 内容 |
| --- | --- |
| `e2868a8` | 本单元 spec/plan |
| `bfb4ee5` | 可选有界 HTTP 配置、mounted API key 所有权、共享原语言命令构建器 |
| `c4d7498` | 创建/Lookup/同步执行/SSE、严格 JSON、全局限流与总并发、真实 socket 期限及关闭等待 |
| `532e9c6` | `--serve-etcd` 提前分支、OpenHTTP 必需配置检查、原只读启动检查兼容 |
| `4085642` | unknown 的实际 typed error 原因，无字符串泄露或自动重试 |
| `f9b9bb1` | 从认证原申请重放路径返回 Reused，独立于完成阶段 |
| `fea854d` | 真实 HTTP native、原 fixture 最小扩展及 operator 文档 |
| `cdcc859` | 唯一审查 R1：已知 SSE 拒绝、joined causal reason、创建接纳失败的映射及精确回归 |

一个原 Deployment/Service/Backend/signer，固定 principal、资源/network/launcher profile。创建保留原幂等键、IDs 和配置；成功响应须 fresh Lookup。Exec 每次创建新原 operation，拒绝 Idempotency-Key，包括空 header。只有原 verified local_terminal/drain 才生成真实退出码；unknown 保留原引用，不伪 exit0/done，不重发命令。

HTTP 最多32个请求，body2MiB；执行 stdin/metadata 保留原协议更小上限。同步 stdout+stderr 合计1MiB；SSE 一个16项队列、每项最多32KiB，实际 ResponseController 写期限和原 writer/Exec owner join。无每已有 sandbox 的 API map、IP map 或后台清理协程。这里只确认结构界限，没有测出大量已有 sandbox 的性能提升。

## 测试与证据

Root 唯一执行 Go/Git/Docker，固定 Go1.25.6；所有源码/测试/格式修改通过 apply_patch。每个 action 记录匹配平台/race 的 GoList17、源码及工具前后 SHA、执行前归档、逐 regular member 校验和原始日志；Root 在源码改变前核对实际当前输入。历史版本的成功记录不冒充后续字节。

- 配置/命令构建器、gateway、CLI、OpenHTTP 均先出现预期缺失声明 RED，再实现。配置 host/race4项顶层、共享命令1项及3语言分支、旧 handler2项及实际 Python/Node4分支通过，无跳过。
- Gateway 初始9项 host/race通过，包含真实 socket 背压/断连、关闭等待、认证/严格输入/容量/输出上限；相关 Linux vet通过。后补 unknown 原因的5组同步/SSE wire 与 Reused 的 false/true wire host/race通过。
- CLI 新/旧早期入口共6项 host/race通过，实际运行原测试二进制子进程；OpenHTTP/配置5项 host/race通过，无跳过。Linux CLI/provisioning vet通过；没有额外 go run。
- 初始 native build `http-01` 成功但未执行。`http-02` 实际暴露 Reused 推断错误，保留失败；修复后 `http-03` 实际全场景通过。没有重跑旧 Lookup/32秒 Exec 矩阵。

| 实际 native | 结果 | 原始日志 SHA256 | 原始日志字节 |
| --- | --- | --- | --- |
| `http-02` | FAIL1.38s，create/replay201同ID后 Reused 断言失败；Exec尚未到达 | `120e86cee104e997fdae42143a69bfbf7581e24d128a010a3677ab351521e549` | 4303 |
| `http-03` | PASS1.77s，无skip；为初审前源码 | `a89f4846bf8c7f9352707b72db10da9df082dae9ccdf17bc9a2e338e299e04b9` | 7058 |

`http-03` 使用实际 TLS etcd3.6.14、正式 launcher PID1、原管理 bridge 和实际 HTTP listener。覆盖401不创建Pod、Ready、创建201/原key重放/409冲突、GET原时间与expiry、同步和SSE stdout/stderr/exit7、拒绝Exec幂等header、501、实际启动后断连、物理丢bridge后的unknown原refs、server取消与关闭。一个原Pod/launcher，8个健康bridge Wait0、3个事先标记故障bridge被kill；全部11个原 Wait/IO owner 已join，正式PID1 Wait0，etcd原SIGTERM已消费。

最终初审前 runner actual17=9174，SHA `159ebb360c1fd9dbc25cec340db564e0df9a142358ee6ddc27483fcc250c2357`；launcher actual17=2022，SHA `6c99f53eee5b0e53d205b82777b898a2970968bff7ff07673dc212a68686dae1`。两者真实 ELF64 AArch64、Go1.25.6/linux/arm64/CGO0/nonrace，构建日志0B；Root 在运行前后核对源码、工具、归档每项、binary/buildinfo。Fixture etcd SHA `9076ea9d5662fdade520a63aa7e312fcde5a4fb7d50a5515d37896204f1c0f15`。

成功容器 `4a5c51d73a95df0e5690a23f4c406ff3b2f4b7bb86847d0e77f89cff8cb979f8` attach/container Wait0、无OOM，已精确删除、原label inventory为空；networknone/privatecgroup/readonly/512MiB/2CPU/256PIDs/固定cap及NNP、3个只读bind已核对。失败容器 `4685fc5861b5b5a54b8541cb5d0becf5510e6fb0e0da9118a702f6e7ba338e26` 同样精确回收、Wait1/无OOM；失败 wrapper 没有到达末尾 postchecks，后续校验明确属于 Root 手工检查。

## 审查闭环（R1 Approved）

唯一 fresh author/reviewer；初审 C0/I3/M1，Needs fixes。三个 Important：SSE将执行前拒绝当unknown；joined取消掩盖真实guard/冲突原因；未被service接纳的创建当202pending。新增精确 wire 用例已实际 RED：`review-r1-red-01` exit1、actual17=8918、11089B，SHA `0e49cab6690a0e0165ee814679a56dbbb647e297126fe0019174abed5c37b63c`。原真实pending、取消/截止时间/opaque控制用例仍符合原约定。

R1 使用共享 service-error classifier 保留已知 SSE rejected 事件和有限 retry_after；已提交HTTP200不伪改404/429/503。Guard/Conflict 优先于伴随取消；无原创建IDs的 Pending+Conflict 503/RetryAfter1/actualkey，实际pendingunknown仍202。没有添加 Lookup、第二次 Exec、索引或原生权限。

修复后 `^TestGateway` host/race 各14项顶层PASS、无skip，含真实socket、旧输入/输出/关闭、4组已知拒绝、7组causal reason（两种join顺序和同步/SSE）及busy/pending控制。`review-r1-host-01` actual17=8918、3715B SHA `c733450e858fd26b7a949a2a83b527260ef469bd0a8b5eefa9361d379464aaa4`；race actual17=8923、3715B SHA `f36d6e2c236c4d3065bbf76647d3cce2dfaf16a4ff0e9ca0cd40f3918df2e640`。`review-r1-vet-01` linux/arm64 actual17=9003 exit0/raw0B。Root 完整匹配当前/前后输入、工具及逐归档成员，格式诊断为空，然后精确5路径提交。R1仅修改wire分类与文档，运行时/transport/fixture未变；没有再次执行native，不把初审前http-03当R1字节执行证据。

同一 reviewer 已逐项复核修复 diff 和实际 RED/GREEN 日志，确认全部三项 ADDRESSED、无新增 Critical/Important，Task spec/质量 Approved；未重新执行测试。M1 为已保留的 fixture common-port/simple-token 警告及 SIGTERM teardown 日志，延期至最终整分支审查整理；事先标记的负例 bridge 日志是有效故障证据，不能隐藏。原始日志不宣称 pristine。

## 实际限制

默认启动仍走 legacy/Redis；显式 etcd 模式只提供四条业务路由，其余 workspace/file/upload/network/TTL/destroy/oneshot 明确501。KubeAPI为受控fixture，未验证真实kubelet/CRI或生产 rollout；TLS终止/部署恢复、长期证书/历史GC、分区/FUSE/collector、大N容量与最终Redis移除尚未交付。上述短命令与小规模验证不等同容量、性能或整个生产迁移验收。

Root另记部署前接续项：当前health共用HTTP入口预算，业务满载时也会503；下一HTTP接入单元应提供独立有限liveness/control预算并覆盖满载行为。本项在R1改动之外，转入后续接线/最终整分支审查，未宣称生产probe验收完成。
