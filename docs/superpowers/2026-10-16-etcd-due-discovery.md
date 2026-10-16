# etcd 到期发现与任务准入

本单元代码与有限三成员原生验证已完成，整体单元 review 尚待完成；本说明不是生产启用记录。默认入口仍使用 Redis。

新 enrolled 发布采用独立 `runtime_publish_enrolled_v3` 协议，原有全部 54 个比较保持，成功分支 10 次写入同时发布 active control 和永久到期候选，失败分支为空，总计仍为 64 个节点。失败后仍须通过当前 identity/restore 下的真实读取仲裁；空失败分支本身不提供任何身份或结果证据。既有 v2 发布记录、receipt、原 digest 与重放保持兼容；签名 proof/enrollment 子记录不改变协议版本。

到期索引按分区、固定宽度 UTC 纳秒时间、sandbox、generation、intent 排序。候选只是发现线索，不能授权运行时操作。历史 v2 与未 enrolled 记录没有自动获得索引；补齐与覆盖率验收是生产迁移前置条件，不能静默忽略。

`ReadDuePage` 只读取 overdue 及首次窗口未来 60 秒。游标固定原 worker、分区、Range 份额、窗口和 revision，不能由替代会话接管或重复分叉。每页最多 118 条，按 key/value 各 1024 字节及 framing 上界预估，RPC 前收费 256 KiB；实际 protobuf 仍必须不超过 256 KiB。空页、失败或未知结果不退额度。历史读取响应的外层 revision 可以比固定 revision 新，记录本身必须属于原固定视图；compaction 使游标失效。

原生 Range 子响应允许合法的零值 cluster/member/term，外层仍严格校验 cluster，子响应 revision 与每个 KV 必须符合固定视图。实际请求保持 KEY/NONE 且没有 revision 筛选，避免额外排序取全范围。页长限制了返回值与客户端物化量；etcd 内部仍可能遍历窗口内的匹配索引计算 Count，所以集中到期积压的服务端 CPU 和恢复尾部仍须压测，不能把小页等同于恒定 CPU。[etcd3.6.14 原始实现](https://github.com/etcd-io/etcd/blob/v3.6.14/server/storage/mvcc/index.go#L101)

`AdmitExpiredTask` 在原 worker owner 内完成读取、准备、提交和原 Stage 清理，不把可延迟提交的 Stage 交给调用方。两次可信时间观察均使用 UTC 减 uncertainty 的保守下界，第二次紧邻提交且不得倒退。当前 active control、精确候选以及原 profile/session/partition/claim 份额比较位于实际提交事务内。未知结果保留原 attempt/reference，不能当作已中止后重试。

提交建立现有永久 metadata destroy task、关闭 metadata admission，并在同一事务删除本次精确到期候选，避免反复扫描已经入队的历史到期工作。失败或未知结果不允许另行删除候选；已存在的永久 task 是后续任务发现依据。它不证明实际 runtime gate 已关闭，不允许删除 Pod、释放 owner/quota 或绕过后续排空、归档、终止和安全释放协议。手工清理等遗留候选仍须验证当前 control，持久 32 个实际任务硬槽尚不由本单元提供。

固定 revision Watch、有界缓存、dirty/task 消费、实际 mounted worker、历史索引补齐/回收和生产启用仍须继续实施。小规模三成员验证只证明具体链路行为，不能替代大量已有 sandbox 的容量或恢复尾部验收。

2026-10-16 新建的 `TestDueDiscoveryNative` 在 etcd 3.6.14 三个真实成员上通过，耗时 7.82 秒。验证了 v3 发布与候选的同 revision 原子写入、正常 metadata CAS 冲突后原 worker 仍能续约、分区 fence 改变后旧 worker 被拒绝，以及新会话提交永久任务时原子删除精确候选。125 条受控候选按固定 revision 分为 118＋7 条，并验证并发插入/改写不混入旧视图；真实 physical compaction 后原游标失效。三个成员进程均由测试持有并终止等待，容器退出和清理已核对。

注入分页候选前，受控单沙盒 namespace 的最新键值序列化总量为 28,327 字节（32 个键），加入一个 metadata task 后为 32,732 字节（38 个键），包含固定 profile、租约键及历史。该样本不是生产每沙盒平均值，不能外推大规模容量，也未验证原先 8 KiB 的建模目标。固定 μ 下的 1 万、10 万、100 万已有沙盒和集中到期恢复仍待容量验收。
