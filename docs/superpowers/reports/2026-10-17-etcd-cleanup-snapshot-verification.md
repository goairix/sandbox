# 清理快照与真实创建入口验证

本次完成cleanup执行计划的Step2：显式local workspace的PlainCold创建入口，在原AcquireIntent事务中捕获清理描述。真实cleanup消费者、持久32槽及分区自动发现仍未完成，本记录不代表生产迁移或容量验收。

## 功能范围

- 新snapshot2将原业务payload、local workspace描述及同步策略共同绑定到版本化摘要；descriptor限8192字节，总snapshot仍限65536字节，没有增加事务节点。
- 历史snapshot1字节、摘要和业务capsule1/2保持兼容。自动捕获默认策略后的历史请求重试返回原配置，不改写历史快照；新策略冲突及降级请求拒绝。
- 严格检查必填、未知、重复、null和canonical表示。业务payload继续允许任意JSON，合法null参与摘要；缺少payload仍拒绝。
- 描述、payload、排除路径在原RPC前复制；缓存及共享miss返回独立副本，完整计入缓存预算，每位读取者重验当前placement关联。
- 实际PlainCold.Create显式local workspace捕获默认同步策略。remote、detached business与旧快照没有凭空补全清理描述；之后的本地消费者仍须验证当前实际权限。

## 实际验证

固定Go1.25.6。host及race使用同一独立三成员etcd3.6.14，逐地址验证三个不同member/同一cluster；服务端max-txn-ops=64。不是mock KV或跳过集成测试。

| 检查 | 实际结果 |
| --- | --- |
| 状态层56个测试组及157个子测试 | host PASS 2.978s；race PASS 3.936s |
| 创建入口12个测试组及32个子测试 | host PASS 1.445s；race PASS 2.524s |
| 两包Linux/arm64、CGO0 go vet | 均退出0，输出为空 |
| 跳过/失败/数据竞争 | 最终四轮host/race均无SKIP、FAIL或DATA RACE |
| 输入与工具 | 六轮全部17类Go输入、source/tools逐归档成员、执行前后及当前SHA独立核对通过 |
| 测试资源 | 三个etcd实际SIGTERM+Wait退出均0，无OOM；精确CID删除后NoSuch/[]，专用网络和标签清单均清理 |

原始记录保存在当前执行计划的工作目录内。最终日志SHA256：

| 执行 | 日志字节 | SHA256 |
| --- | --- | --- |
| snapshot-host-02 | 65254 | 10741a98ecbf5ae500e05700ffabfbc63ef5dd05dd455f0cec53aa71b27bc161 |
| snapshot-race-01 | 65254 | b4296326989b7729e95d37313ef38fd3336f476cae5ec668607e48f62a1c6163 |
| producer-host-02 | 6306 | a2c7d5bd623156c0b00b9c7c938ece6abe46c8f4a5825306e27c1cb30f140b2c |
| producer-race-01 | 6305 | eb3e075fea5f2ec2f9cd115f7346e088b5dca9e678d06f02d843f857a9fd7bc5 |

## 失败记录与限制

首次两包因缺少新增类型/字段编译失败，仅作compile-only RED。随后真实行为测试分别复现null写入后无法读取、缓存命中与共享miss绕过当前placement校验，修复后上述测试转绿。创建入口测试曾将json.RawMessage与[]byte作不同类型的Equal比较；只修正显式字节类型，保持逐字节相等，不将该失败描述为产品行为缺陷。

最终provisioning host与race各保留1条原有假服务关闭测试的context-canceled告警；状态层输出无WARN。三个etcd完整服务日志有21/22/19条warn、4/5/5条error：其中两条启动期storage-version missing-term错误，其余12条是SIGTERM期间mux/http/listener关闭错误。原始日志完整保留；不声称运行无告警，也未将这些日志转换为生产可用性结论。

首次专用internal bridge未实际发布host端口，测试前即失败；三个已启动成员均实际退出0并清理，失败记录保留。新一轮使用专用bridge、关闭IP masquerade、仅127.0.0.1发布端口；这不是network-none物理运行时测试，也不代表完整出口防火墙。

注释、排版和包职责已自查：新增codec/描述/摘要保持存储协议内聚，provisioning只做创建策略捕获；关联校验复用私有函数，没有为拆包导出权限。本Task的完整spec+quality review仍待真实执行消费者、持久槽和新物理组合完成后统一进行。
