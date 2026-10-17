# 历史回填可选状态损坏：索引隔离修复

本片修复完整 pending Task 审查的 I1：首条历史 Task 的可选 execution 状态损坏时，
严格 birth 读取仍拒绝，但后续可信历史任务不再永久卡在同一回填 frontier。

## 实现及约束

- 新增独立私有 index evidence，只包含同次读取的 Task/intent/link/初始 committed
  receipt 与原始 pending 观察；不含 execution、release 或物理执行权限。
- 纯 decoder 只能构造 draft。原真实 worker postguard 与 owner postcheck 都成功后
  才 seal；复制、外来、未 seal、失权或传输不确定证据不能推进。
- 仅历史回填消费这种可选拒绝。普通 pending/终态/物理路径保持严格错误及原证据要求。
- 原子 CAS 只补建不存在的规范 pending 或保留已有规范 pending，并推进同一连续历史
  前缀。损坏、带 lease 或被重写的 pending 不能修补后跳过；不修改 E/S/release。
- 共用原 mandatory 校验与连续页计划，保留原事务和并发所有权边界，避免复制两套协议。
- 实际原生节点 45/47，上界请求 91136B、成功 4096B、失败 14592B；原 64 节点、
  128KiB/256KiB 不变。索引证据最多 188416B，替代正常 birth/release，仍在原候选
  1MiB 和 automatic 4MiB 预算内；没有增加逐 Task 缓存或 goroutine。

## 验证

- `automatic-index-r1-red-01`：旧接口行为测试真实复现后续 Task 无 pending，exit1；
  `automatic-index-api-red-01` 是后续独立接口缺失 compile RED，不能混称行为复现。
- `automatic-index-host-01`：exit0，9072 输入，日志4132B，SHA256
  `940a566c86c3d76b8666aa47e52685a7c5791a059f0906866c3efbe736ff7c70`。
- `automatic-index-race-01`：exit0，9077 输入，日志17390B，SHA256
  `659816b291455d993b9704c626ca928557b90542ce021b662d50324db686f0c1`；
  包含新索引测试及既有 birth/advance 回归，list/test 均使用 race。
- `automatic-index-vet-01`：Linux arm64 vet exit0，9164 输入，原始日志为空。
- 每轮后在下一 Go/源码变化前独立核对完整17类输入、前后集合/hash、每个 source/tool
  archive member 和当前字节。最终三轮再次统一核对；原始失败和工具记录保留在 OWN。
- 人工检查 GoDoc、状态/授权边界、逻辑分段和包职责；gofmt-d 无输出。存储事务逻辑
  留在同一 etcd 包，新文件按出生观察与索引推进职责拆分，没有扩大公开接口。

这里的 host/race 使用真实生成 RPC 的受控模型，不是三成员 native 或大 N 验收。
原 Task 创建丢 ACK 后替代发现、真实 compaction/公平性和持久32/33恢复仍在 I3 中。
本片尚待原唯一完整 Task 审查员限定复核，不能将整个 pending Task 标记完成。
