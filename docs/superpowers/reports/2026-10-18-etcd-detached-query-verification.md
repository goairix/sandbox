# Detached 新 Run 查询与 owner 回收验证

2026-10-18；代码 `9c5ad2e`，唯一 reviewer 对五文件组件 Spec/Quality PASS。
范围为真实元数据恢复和原请求 Query，不代表 physical proof、七槽完整终止或消费者完成。

## 改动

首次 Unknown 返回后，旧 Claim 的原 attempt owner 仍保留 caller context/cancel，
使实际冷恢复对象图校验拒绝继续。修复保留原 owner 指针与不可逆 attempt 标记：
finish 在生命周期锁下移除活动槽、释放 ctx/cancel，再关闭原 done。Close 在同锁内
复制 cancel 函数值及 done，解锁后只使用副本，避免另一 owner 提前 finish 后读取
已清字段；取消仍不能代替 join。原 v1 purpose 禁重入、gate 和容量上限均保留。

## 验证

真实接合 RED `run-d32fa57626be4bc581ec496c74381df6`：首次 Unknown 已返回、
原 owner/TLS 已返回，真实 Joined E/S 精确匹配后，cold preflight 因旧对象图拒绝。
另两项 host 回归先真实失败，再覆盖字段释放、原 marker 身份，以及 Close 快照后
另一 owner finish 的确定性交错；用 channel 控制，没有 sleep。

最终实际 etcd3.6.14 三成员、Linux/arm64、CGO=0：

| 场景 | 结果 |
| --- | --- |
| 原 Run joined→原51节点 actor CAS→真实新 Claim/fresh drain→原 reservation Query | `run-95c78ab5b66e44a787accf9776b825c3` PASS16.16秒 |
| 续租后原 cutoff 保持；独立诊断新窗口 | `run-d4d5327f96f54463aee7a6f97a7c7118` execute/diagnose PASS |

新 Run 仅1次 Put；后续 Query 操作零 Put。累计 Terminate1、Sign1、Pod DELETE1、
Query1；原 intent/provider/fact/none 完整 MVCC 不变，旧 authority 本地拒绝。
两个 live guard 使用真实新 Claim，55节点，请求11065字节、回复300/299字节。
Query 操作观察19次 Commit 提交，不是 transport dispatch/retry 上限。

两场各65项 source 与 ELF 指纹稳定，无 SKIP/FAIL/OOM，原三个子进程实际 join，
精确自有容器删除。ELF SHA-256：
`610ece085ad6aa4d736ce2ea32363ac82f9cbc08b092a60c716b4ee46c652556`。

`detachedownerretirementgreen01`：18顶层 race PASS2.697秒，9153输入；
`detachedquerygreenbuild01`/`detachedquerygreenvet01`：各9253输入，通过。
完整命令、输入、工具、源码快照及前后指纹审计通过，共用缓存。

完整 slots2..6、terminal proof/PID1/cgroup、managed consumer、owner/terminal/quota
全链路、HTTP/部署和大N容量仍开放。原始证据及 `query-review.md` 位于 D 工作树
`.superpowers/sdd/2026-10-18-etcd-detached-runtime-native/`。
