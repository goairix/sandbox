# detached owner 释放基础片验证

源码 `0c6a009`（辅助 `936429b`）只实现独立 v2 seal 编解码、纯历史归属匹配及
事务投影。没有 live writer、原生历史认证 reader、删除权限或消费者接线；公开 v1
保持严格，功能约70%、整体约45%、生产切换0%。

v2 seal 绑定 Task/birth、实际释放者 Claim、runtime/workspace/generation/node、完整
termination stamp、CleanupDigest、DetachedEffects 及有序三键和原 stamps。释放者
可以是 cleanup-complete 后的新 Claim；历史终止发送者 Current 不作为当前权限。
canonical 重新编码逐字节相等、不可变 lease0 envelope 和确切派生键拒绝 v1、外来
字段/重复字段/null、错误 stamp/身份/顺序与非规范输入。seal 出生必须晚于终止
mod 和释放 Claim 出生。纯 matcher 重构整个可比记录，仍不认证 terminal 签名。

固定 schema 在 root512、restore/sandbox128、runtime 三个128字节值各六倍 JSON
转义、int64 最大宽度及精确三派生键下为7643字节，符合8KiB。任意独立1024字节
删除键不满足该证明，codec 必须匹配实际派生的598/654/592字节上界。

投影保留 base4+Claim40+source8，再 prefix-empty1、settlement Mod1、termination
四项和 seal-absent1，共59比较；Then仅三个单键 Delete 和永久 seal Put，共63节点，
无 Else。最大维度模型使用原 Claim 构造器，实际 protobuf 请求75170字节、success249、
failure47，核原请求128KiB/回复256KiB。模型中的 unsigned terminal 绝不送 native
writer；投影自身也不认证输入的权限。

| 检查 | 结果 | 实际输入数 |
| --- | --- | --- |
| `detachedownerfoundationred01` Host五项 | 全部到达原 scaffold 断言失败，前置模型成功 | 9152 |
| `detachedownerfoundationgreen01` Host同五项 | PASS | 9152 |
| `detachedownerfoundationrace01` Darwin arm64 race | PASS | 9157 |
| `detachedownerfoundationvet01` Linux arm64 vet | PASS | 9260 |
| `detachedmainownerfoundation01` 主分支整合同五项 | PASS | 9152 |

五项覆盖新释放者与历史 Claim 区别、严格 codec、foreign variant 历史归属、最大
schema、完整63节点及独立字节预算拒绝。各轮完整17项 Go 输入、工具/源码归档和
前后指纹审计通过；审计 PASS 不把 RED 改写成测试通过。证据复用辅助 B 工作树
`.superpowers/sdd/2026-10-18-etcd-detached-noeffect`，共享缓存且仅归档仓库源码。

唯一审查员完整检查四文件与直接依赖，核实际结果与输入指纹后限定 Spec/Quality
PASS。注释解释 schema 上界、历史归属和权限边界，校验/编码/匹配有清晰分段；
codec 与 protobuf 分文件，继续共享 etcd 私有事务不变量，没有扩大导出接口或依赖。
主分支四个 Go blob 与辅助分支相同，主分支整合也通过实际 Host 和指纹审计。

下一片需要原 job/Claim/Kernel 的 live writer、删除 index 后仍存13来源的认证读取、
原七槽/proof 校验、同 Claim 丢 ACK 只读仲裁及新 Claim 恢复。quota/终态、统一 phase
准入、32物理并发、大量已有 sandbox、部署和 Redis 移除仍开放。
