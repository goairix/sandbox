# 到期发现与自动 metadata 任务准入：范围验收

验收范围为 BASE `f95d100f26ba6ba81f38ed2289090f36f0ca86f1` 至 `f8a9d1a13ba5180e3f54f50e8b3e24ec182ea0ae`。唯一完整 Task review 结论为 Spec compliant / Task quality Approved，无开放 C/I/M。不是整个迁移或生产切换验收。

| 实现提交 | 交付 |
| --- | --- |
| `d65c62b` | 新 enrolled v3 发布原子写入永久到期候选，保留54个比较；明确兼容旧v2协议、digest、记录与实际恢复/配额消费 |
| `ff649e8` | 原 worker/分区/Range 份额的118条、256KiB固定 revision 分页；真实原 owner 取消与Close等待 |
| `6350a18` | 两次可信时钟保守下界、原 worker/claim 份额/current control 实际提交验权；metadata task 与精确候选删除同事务 |
| `4957c9f` | 一个全新真实三成员原生链路及操作说明 |
| `f8a9d1a` | 已验证切片与精确协议约束记录 |

v3 事务为54比较＋10成功写＋0失败分支＝64，失败仍通过原 read-bearing 仲裁确定结果。自动到期业务组合44比较＋6写＋14保留＝64，实际提交52比较＋7成功操作＋4失败读取＝63。既有 generic Stage、手工销毁、物理终止和 owner/quota 释放协议未扩权。正常过时候选、metadata 冲突或时钟拒绝不会误退役健康 worker；实际 worker fence 丢失仍终止原会话。

Root 按切片实际运行 tests-first RED、host/race 和 Linuxarm64 vet，publication 与 provisioning 兼容路径均覆盖。最终到期切片21个顶层测试组通过，无skip/WARN/race。中间发现的真实 nested Range header 兼容及正常拒绝误退役问题均先复现，再修复、验证。每个源码切片在验证输入和原始输出核对后独立提交。

全新 `TestDueDiscoveryNative` 在 etcd3.6.14 三成员上实际 PASS，耗时7.82秒。观察到 v3 publication/control/due 同revision19；正常冲突后原worker仍可续约；真实分区fence改变拒绝旧会话，新原会话在revision39提交永久task并删除精确due。125条受控候选在固定R44分为118＋7条，并发修改将外层revision推进至46但未污染旧视图；真实physical compaction使原游标失效。成员进程、客户端和容器均实际关闭、Wait与清理，零丢弃输出。

最终 Linuxarm64/CGO0 二进制SHA256 `9ad93a133c945529449eb24e7a6e4a0a1a96150dd0150ab64c4ec7d4236c87a6`；native原始输出2912字节SHA256 `064fe50c28e80bbf47c5c9e979220e2decb392a829a3ab6e6a3dc0c2e2569a98`。编译输入17类、源码/工具归档与前后哈希、实际二进制及字面容器运行约束均经Root独立核对。运行限制为原批准Linuxarm64镜像、network none、私有PID/cgroup、只读root、1GiB/2CPU/256PIDs、dropALL/NNP、两个只读挂载；退出0且无OOM，删除后精确实例与label库存均为空。

保留的失败尝试：首次native编译/vet因Root建议了不存在的fixture日志方法失败，确认已安装API后只改为WithLogger；第一次容器create因Root给了Docker不支持的`--pid private`参数失败，未创建容器或运行native，只去掉该参数使用Docker默认私有PID。所有失败原始记录保留，未覆盖；实际通过运行使用新epoch。未修改生产日志、客户端权限或运行时隔离要求。

注入合成分页候选前，受控单沙盒namespace的最新键值为28,327字节/32键；增加一个metadata task后32,732字节/38键，包含固定profile、租约键和历史。这不是生产每沙盒平均值，不能验证8KiB建模目标或外推容量。Range payload有界不意味着etcd计算Count时CPU恒定；大量积压与恢复尾部仍待实测。

审查中的 Cannot verify 项已由Root逐项归入后续范围：历史v2真实滚动升级/全量索引覆盖；Watch/cache/dirty/task发现、持久frontier和实际worker；持久32实际任务槽及物理清理；大N性能容量、部署恢复和Redis移除。历史协议在本单元有明确源码与模型测试支持，没有宣称真实线上滚动升级实验。当前默认仍Redis，功能代码粗估70%（65%–75%）、整体交付45%、生产切换0%保持不变。
