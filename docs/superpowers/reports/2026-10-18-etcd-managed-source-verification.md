# managed detached 来源接线验证

主提交 `4fd2a15`，辅助提交 `fcd2ef0`，共13文件。原 DueWorker 的 snapshot
阶段已实际调用完整 detached 来源认证；成功后停在 `detached-source-verified`，
尚未安装 wire inventory、申请清理 Claim 或执行物理销毁。

原40MiB动态额度内串行预留24MiB/20键，认证构造模型13.6875MiB，保留16个紧凑
来源 pin。automatic/cold 的两份4MiB额度不重复借用。Pending checkpoint 存在时
不改变缓存覆盖或游标；缓存超出缩减后的上限时完整失效并重建，不能丢尾后仍声明
覆盖完整。取消和 Stop 必须等待实际 native/source 调用返回、原 owner 退休后退款。

使用原 Task、snapshot、placement、CreationIntent、publication 及其完整签名历史；
普通 workspace 从 BackendStorageID 与原 RequestHash 重建。原 local/v2 descriptor
仍走原路径。source 分类和 pin 不授予 Claim、Run 或 effect 权限。

| 验证 | 结果 |
| --- | --- |
| `managedsourcevalidred02` | 基线实际 actor 在合法60s配置下未进入来源分支，RED |
| `managedsourcegreen02` | 5个顶层、2个子场景通过；含 Pending 真实新 session 恢复、缓存重建及取消保留 |
| `managedsourcerace01` | 18组 source/cache/automatic/cold 回归通过 |
| `managedsourcevet01` | Linux arm64 vet 通过 |
| `mainmanagedsource01` | 主分支 source5、wire2、JSON2 共9组通过 |

全部验证完成17类输入、工具、源码归档和前后指纹审计；共用缓存，不归档工具链或
模块缓存。最初非法 TTL/已失效 session 的 fixture 失败不算产品 RED，日志原样保留。
证据在辅助 D 工作树 `.superpowers/sdd/2026-10-18-etcd-detached-runtime-native`。

唯一审查员限定 Spec/Quality PASS，确认主/辅助13个 blob 一致及主分支新增验证。
注释覆盖额度归属、Pending 不变量、取消和退款顺序；存储事务与原 actor 所有权仍留
etcd 包，独立文件区分来源认证与内存模型，无新增导出权限或反向依赖。

下一步是完整 detached wire 预检、运行阶段内存准入和真实 managed Claim；配额历史
读取、writer、自动消费者与 HTTP 接线也尚未全部完成。本组件不提高生产切换进度，
不是大 N 容量、RSS 或吞吐验收。
