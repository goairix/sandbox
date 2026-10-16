# 清理持久槽协议与原 Claim 复用验证

本次交付清理执行计划 Step3 的第一个连贯切片：永久槽记录、占用与轮转 CAS，以及复用原 TaskClaim 流水线的私有准入。实际 cleanup 消费者、run/join 标记、真实终态结算和正向物理 Claim 准入尚未完成；本记录不代表完整持久32执行能力、容量验收或生产切换。

## 已实现

- 固定32个无 Lease 槽，永久 Task 执行记录保存原 Task/intent/初始 committed receipt 的共同出生版本及摘要；严格 canonical 编解码，单记录4096字节。
- 相同 Task 经执行记录的选择 CAS 仲裁占用与轮转；丢响应保留原选择，租约失效不会回收占用。E/S revision 偏序拒绝删除重建后的旧占用。
- 原 worker session、partition 和 Claim 份额控制每次有限操作，不退款；退出必须等待已开始的真实原生 RPC 返回。新私有路径要求 session TTL60秒、单请求不超过3秒及足够的剩余期限，原公开 ClaimTask 配置不变。
- 原公开与 managed Claim 共用准备、Grant、创建事务、postcheck 和失败 Revoke。创建时增加的 worker/E/S 比较不进入原八条效果 fences；缺少真实 KernelBoundary 与完整物理请求预检结果时，在 Grant 前拒绝。
- 对新协议12类具体 protobuf 请求及成功/失败响应进行字节测量和64节点检查。此处仅覆盖新协议；完整旧物理链的具体预检生产者仍待下一切片完成。

## 验证结果

Root 使用 Go1.25.6 和独立三成员 etcd3.6.14。实际 Docker Compose 创建项目，三个不同成员共享同一 cluster，服务端 max-txn-ops=64。原测试的 Compose 所有权检查保留，明确传入项目与精确 CID，没有改守卫或跳过集成测试。

| 检查 | 实际结果 |
| --- | --- |
| 11组新协议/准入与8组原 Task/Claim/Read回归，共106个子测试 | host PASS 12.315s；race PASS 20.345s |
| Linux/arm64、CGO0 go vet | 退出0，输出为空 |
| 最终跳过/失败/数据竞争 | 两轮均无 SKIP、FAIL 或 DATA RACE |
| 输入和工具 | 三个最终执行的17类输入、source/tools逐归档成员、执行前后及当前SHA均独立核对通过 |
| 排版 | gofmt-l 与 git diff-check 无输出 |
| 测试资源 | 三个成员 SIGTERM/实际 Wait 退出均0，无OOM；精确CID删除后NoSuch/[]，专用网络和标签清单均已清理，独立运行参数核对通过 |

| 执行 | 实际输入数 | 日志字节 | SHA256 |
| --- | --- | --- | --- |
| slot-host-02 | 8997 | 60887 | 1f435544e835e4a43c818883ba82889f890ce4847473eb3064717a9b3f66f7b3 |
| slot-race-01 | 9002 | 60877 | e98a548cdf8f0d3ee607af2e9650ad11bff7d6bc890b199c3aa6b858dcf2ff34 |
| slot-vet-02 | 9088 | 0 | e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 |

以下为两次运行测得的各项最大值，单位字节。已枚举32个候选槽及未来记录的合法最大标量宽度；并非所有生产配置的性能或全链尺寸验收。

| 具体模板 | 节点数 | 请求 | 成功响应 | 失败响应 |
| --- | --- | --- | --- | --- |
| point | 7 | 702 | 4344 | 563 |
| bundle | 16 | 1725 | 43082 | 563 |
| observe | 8 | 777 | 3410 | 563 |
| claim-observe | 9 | 914 | 3962 | 563 |
| initialize | 36 | 7815 | 96 | 563 |
| rotate | 27 | 5734 | 96 | 563 |
| reserve | 41 | 10132 | 145 | 563 |
| progress | 31 | 9329 | 96 | 563 |
| claim | 64 | 13053 | 145 | 563 |
| claim-postcheck | 46 | 9682 | 393 | 393 |
| joined | 15 | 6585 | 96 | 563 |
| settle | 48 | 21005 | 147 | 563 |

## 历史失败、日志与边界

初始 RED 因新API未实现而编译失败；首次产品验证又发现新增参数与原 WorkspaceOwnerRecord 局部变量重名，host/vet 均在编译阶段失败。保留原始记录并完成独立输入核对后，仅改新增参数名，未放宽测试。两次都不计作实际物理行为 RED。

最终 host/race 各10条客户端 warn：8条故障或主动撤销后的 LeaseNotFound，2条取消读取的 context-canceled。服务器完整日志分别有35/33/33条warn、各4条error；warn包括测试环境的共端口/简单token、lease-not-found、启动健康探测和停止时的peer断开，成员1停止时还发生失去quorum及leader-transfer超时。12条error均为SIGTERM期间http/mux/listener关闭。全部原始日志保留，不声称无告警或生产可用性已验证。

32槽测试直接建立合法占用元数据，再证明没有空槽时不能准入；ABA测试直接验证旧原生CAS与严格读取，终态样本也是元数据fixture。它们不等同33个真实Task并发、32个物理工作或真实安全释放。managed正向Grant需要真实Linux工作进程和完整物理请求预检，仍待下一阶段。全Task唯一完整审查尚未开始。

按 AGENTS.md 检查注释、逻辑分段和包职责：严格记录、读写CAS、wire测量与Claim准入分文件；共同事务与生命周期不变量保留在etcd包，没有为拆包导出内部权限。部署配置和物理传输实现继续属于各自原有边界。
