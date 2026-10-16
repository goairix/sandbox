# Partition Worker Control Implementation Plan

> REQUIRED SUB-SKILL subagent-driven-development。一个完整控制单元，一个fresh author和一个fresh reviewer，小步验证提交；不等待重复人工批准。

**Goal:** 实现原生worker单Lease会话、256逻辑partition归属及跨副本固定软预算载体，供后续真正worker/索引消费；不宣称完整scheduler或生产迁移完成。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-partition-worker-control-design.md

**Immutable BASE:** afcb7bf96c99eb02fdc1e64bdf53c54623f454af

**Architecture:** backend原生opaque session→原session guard/Lease下partition与有限budget-slot CAS→单调本地bucket/有界原owner→后续consumer同原Txn fence。失权永久lost，Backend.Close先停止并实际join，再关client。

## Global Constraints

固定256partition；单backend最多32初始化/活跃session、每session一Lease与至多256partition；New不自动建会话；全cell软预算4relist、20×1MiB/s、100×1claim/s，不能变每副本额度。所有新record/key/profile/header/Lease严格校验，最坏wire/64节点预检在Grant前，unknown保守且无自动重试复活。ctx取消不等于原RPC/工作结束。新预算/partition不是runtime/task权限；不删除owner/quota，不把leased软slot充当持久32task硬槽。不改已满64节点的publication/原TaskClaim权限，也不偷偷加松散due写。Root唯一Go/Git/Docker/gofmt，apply_patch唯一编辑；Sentinel与所有旧OWN/pair禁止。

## Review Focus

原Lease/guard/identity/restore/partition/预算身份是否同事务固定；初始化/续期/Close实际join与锁顺序；有界记账、重复获取/bucket/burst/失权重连是否复制份额；relist物理owner与软Lease交接是否正确区分；预算载体不被冒充为后续task授权；实际三member/进程退出/Wait与旧矩阵不重放。

### Task 1: Native worker sessions, partition ownership and fixed cell budget shares

**Files:** 新internal/storage/state/etcd/worker_{records,keys,session,partition,budget}.go及focusedtests，按责任再拆有限nativeRPC/bucket/lifetime文件；修改client.go及实际closeTaskArchiveBackend生命周期接点，必要新backend worker life owner；新partition_worker_native_linux_test.go，复用tracked integrationBackend仅作三member验证，不改旧fixture合同；新operator说明docs/superpowers/2026-10-16-etcd-partition-worker-control.md。确切文件/API由Step1预检冻结，避免把会话/预算大块塞入client.go。

**Consumes:** namespace.Key/Partition、backend.baseComparisons/requestContext/clusterID、既有record/wire预检、实际Backend.Close/client关闭owner。TaskClaim仅作只读权限边界参考，不导出/改写其安全比较。

**Produces:** opaque原worker会话与诊断ref；exact partition CAS归属；fixed profile软预算借用与单调bucket；后续consumer可同Txn验证的原fence接口；有界初始化/操作/Close物理生命周期。具体签名、重复领取/未知ACK/失权与relist guard合同预检冻结。

- [ ] Step1 作者只读预检：exact API/record/key/profile/TTL、guard/fence消费者、Close实际owner、bucket/burst/handoff、finite三member native。Root对照spec裁定冻结并记录共享接点。
- [ ] Step2 tests-only worker会话/partition/records原契约RED，Root实际执行审计后实现；host/race/Linuxvet，连贯可用切片即时提交。
- [ ] Step3 tests-only跨副本软预算与bucket/relist owner RED，Root执行后实现；双owner/重复份额/失权、ctx与Close join、每槽限额和边界验证，小步提交。
- [ ] Step4 Root fresh collector新sole三member实际native，实际进程失效/原Lease/竞争/份额/身份/Wait与精确清理；operator说明及新native小提交，不重放旧矩阵。
- [ ] Step5 唯一fresh full review BASE→HEAD，同pair限定fix；tracked范围验收明确完整due/dirty/Watch/task硬槽与生产consumer未完成，关闭OWN/pair，继续总迁移。

## Root 初检与裁定

| producer→consumer | 初检发现/处理 |
| --- | --- |
| worker session→partition/budget | 一原Lease、原guard/actualrevision/identity/restore，不能borrow reference构造权限 |
| session初始化/renew→Backend.Close | 必须接真实client owner并停止接纳/取消/实际join，不另造Close权威或关闭原未join RPC |
| fixed slots→localbucket/consumer | 全cell固定份额+单调本地bucket，必须说明burst/尾部；后续实际consumer同Txn重验fence |
| relist owner→失租/新session | ctx/Lease失效不冒充本地工作已退出，旧结果不能发布到新缓存；明确接管尾部 |
| task硬槽→软budget | 32个实际任务硬槽不是leased份额，本单元不伪造完成或提前归还 |
| native→integrationBackend | 实际3不同member；新controlled collector/sole selector/childWait；无旧OWN artifact或旧全矩阵 |
| publication→future due | 实际54cmp+9success+1failure已满64；当前单元不加写、不放大cap、不省略安全比较，后续须独立改造原发布流程 |
| Task内部 | 控制单元完整可验证，生产消费与大N容量仍有明确后续，不按组件/测试数上调百分比 |

Ruling: 用户已批准完整迁移且要求持续加速，本单元是原分区调度方案的必要接点，不重开方案批准。先实现全cell控制与原native权限边界，后续同Txn索引/真正consumer接入不可跳过；成本是范围验收不等于主程序已使用全cell背压。完整生产功能/容量百分比暂不因本组件增加。
