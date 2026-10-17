# Worker cold quota 历史分步认证验证

新增私有历史 cursor，以七次固定、分别计费的 W20 metadata 读取认证原 publication、settlement 和 signed release。每次 step 至多执行一次 Range262144 读取，不消费 Claim；额度不足和校验错误保留当前阶段，下一轮只重试尚未完成的组。A4/A5 共用一份完整认证的 settlement receipt，公开 loader 的原 RPC 顺序保持不变。

原 registered owner、context、session、partition、Range 和 cursor self 身份在构造、每阶段及 cached done 检查；busy 覆盖实际客户端返回、解码和发布前复核。A2 完成真实签名验证后丢弃 proof/enrollment 临时图，A8 完成 signed release 和原来源绑定后丢弃九点 snapshot，只返回借用的原六个 CAS source KV。调用者仍须在单共享工作区中保持该 history 不可变。本组件没有完成全局工作区、32 job 内存约束、原 job 正向来源或自动 consumer 接线。

生产文件按 cursor 调度与 typed phase 拆分，保持 etcd 包内私有事务职责；没有扩大导出接口、跨包共享可变状态。并发所有权、重试、保留范围和权限边界均有注释，Root 已检查逻辑分段、gofmt 和 diff check。

## Root 执行与证据

| 轮次 | 结果 | 输入数 / 原始日志字节 | 原始日志 SHA-256 |
| --- | --- | --- | --- |
| fixture02 | 原公开历史 loader 基线 PASS，1.409s | 9109 / 208 | `939e7503e50d1834968235e82c8038886be650c9a7c1ecaf4e4c8d8db7c8dba3` |
| RED01 | 仅批准的新 cursor 类型、构造及 step 缺失 | 9112 / 1191 | `54270a474e09f4992956e2fabf7c71b4fcef4215ec6736b79fae3192c65a1d52` |
| host03 | 全部新历史测试 PASS，2.010s | 9114 / 9962 | `5834f923028f8584e7e4ac6524bbef23665b3977c5f04b81eae397bf48f7ac38` |
| race01 | 新历史及 metadata/settlement/quota return 定向回归 PASS，8.286s | 9119 / 31925 | `3f4f82063f7cf802731152b7a6c0835226cea0946cdd57e09629ea7e5f701c52` |
| Linux vet01 | etcd 包，Linux arm64、CGO=0，PASS | 9209 / 0 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

各轮在后续 Go 或源码修改前完成全部 17 项输入、pre/post、工具、仓库快照及当时 current 源码核对。使用共享构建和模块缓存，仓库输入快照复用，未重复归档依赖或工具链。

早期 fixture01 的 Task/claim 出生顺序错误，以及 host01/02 两个独立客户端测试的 RestoreEpoch、loopback opt-in 配置错误均保留失败日志；只修复测试配置，没有放松业务、计费、签名或关闭断言。超大 protobuf 回复注入产生预期 WARN，对应 fail-closed Unknown 和 session 失效，不是无警告执行。

测试覆盖精确组/cap/计费、每阶段拒绝后恢复、公开结果及原 KV 等价、located/full 变化拒绝、真实终止签名、初始/中途/done 成员重绑定、返回后 context 更换及 Close 等待实际 native 返回。它们是有界 host 协议验证，不能替代真实 etcd、物理清理、大 N 容量或生产切换验收。
