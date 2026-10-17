# Cold quota worker 付费恢复仲裁验证

日期：2026-10-18。新增私有 `resolveColdRuntimeQuotaAttemptWorker`，只仲裁已认证 history 所绑定的原 canonical journal 引用。

本地解码、partition 绑定和完整 wire 预检先于支付。原 owner/session/partition/Range/Claim 在原生命周期锁中原子准入，支付 Range 262144 字节与 Claim 1。预检快照随后替换为实际付费 W24，并再次检查。实际只执行一个平坦 Txn：完整 W24 加 receipt 不存在比较，共25比较；Then写原引用的 canonical aborted receipt；Else按原顺序读取 identity、restore、原 receipt，合计29节点。

新增 phase 7 独立固定 cohort，与既有六阶段互斥；使用原连接、单次 Marshal、零重试缓存，实际 native 返回后才完成 call，owner 保留给后续阶段。phase7 使用 uint8 mask 最后一位，注释明确后续扩阶段需先扩 mask。新 request graph 由调用者保持不可变直至实际返回。

request 上限128KiB，reply固定16KiB，两者合计不超过原256KiB额度；worker receipt值另限4096字节，原其他阶段上限和 metadata 动态 allowance 保持原值。通用公共 receipt 解码最多接受64KiB，但更大的通用 receipt 在本 worker 中可产生 Unknown，不宣称完全兼容其宽上限。新方法在此窄界限内沿用原 `decodeReceipt` 语义。

成功 Then 返回 Aborted；Else 的原 receipt 可在 W fence 不成立时返回普通仲裁 Outcome，但不能授权调度、replacement、quota 或物理操作。未知 ACK、坏证据、未解析结果保留原引用并失效已进入 native 的 session。没有 Grant、新 UUID、Stage 重建、journal/slot修改、proof reread 或 owner.finish。后续调用者仍必须独立计费读取并验证 aborted receipt，或 committed return/receipt。

## Root 执行结果

| 轮次 | 结果 | 输入数 / 原始输出字节 | 原始输出 SHA-256 |
| --- | --- | --- | --- |
| RED01 | 真实失败，仅缺少两个批准的方法与 phase 常量 | 9107 / 1396 | `0c5fc98e2270e68ef67d37e68934b9f851a16d82bf2699f50f95aaf17ff18b13` |
| host01 | PASS，1.671s，新仲裁与固定 RPC 测试 | 9108 / 12715 | `ff0db1416a5f06a2f6d839beb6c791f8106c2d4162d9e3ccbe4c233ca21b934f` |
| race01 | PASS，5.993s，增加原 RPC、metadata、postread、纯校验及 journal 回归 | 9113 / 58290 | `668967323a75144210a96bb9bc8b3209297d246cb1b5688747390884dc6e94f5` |
| Linux vet01 | PASS，Linux arm64、CGO=0，etcd 包 | 9203 / 0 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

Root 在后续 Go 或源码修改前核对各轮全部17项输入指纹、pre/post、工具、复用仓库快照及当时 current 源码，均一致。host之后仅增加 mask 位数与借用 request 图的两条注释，再执行 race/Linux 检查；产品行为及测试断言未改变。gofmt及diff check干净。所有轮次共用一份构建/模块缓存，不复制整套依赖或工具链。

测试验证实际29节点单Txn、W24各字段、双预算及本地拒绝不扣费、原 receipt Outcome、固定上限、坏 ACK、已持久化但丢 ACK 不重发或读取 proof、所有旧 cohort 双向互斥、原生 gRPC retry policy 不重发 payload，以及取消后 Release/Close 等待实际 native 返回。

## 包职责与后续边界

新文件集中仲裁业务、预检及 ACK 解析；原 RPC/codec 文件只扩固定阶段，不修改公共 Resolve 或 codec wire schema。包内私有接口保留原事务、成员和并发所有权，不为拆包导出 authority 内部类型。关键注释、逻辑分段和依赖边界已检查。

本切片仍不是自动 quota 结算接线。完整 worker 历史认证、独立付费恢复 proof 读取、单共享工作区及32任务内存约束、actor/P删除 gate、v3配置、真实 etcd故障与大规模容量验收仍开放。host协议测试及Linux静态检查不代表生产切换或实际容量完成。
