# Worker cold quota 分步恢复验证

新增私有恢复 cursor，借用已认证且不可变的原 history，以独立付费阶段读取已有 return、pending journal，仲裁原引用，再读取对应结果证明。每次 step 至多执行一次 W20 Range-only metadata read，或一次原 W24 Range+Claim Resolve；构造不读取额度、不发 RPC。已有 committed return 优先于 journal 和 mutable slot，恢复不申请新 UUID、Grant、Stage，也不读取 quota slot。

观察到 canonical journal 后，先保留原 journal KV 与完整 reference，下一轮才 Resolve。Aborted Outcome 必须另付费读取并认证原 aborted receipt；Committed Outcome 必须另读 return 和 receipt，实际完整引用须与原 journal 一致。额度拒绝或错误保留阶段与部分结果，Unknown 保留原引用并拒绝在已失权 session 上继续。

构造、各阶段和 cached done 均检查原 registered owner/context、session、partition、Range、Claim、history 指针和 Task/birth/Owner 绑定。非阻塞 busy 覆盖实际客户端返回、typed 解码和发布前复核。结果 wrapper 和 committed Entry 按值复制，原 journal/aborted KV 借用且保持不可变。结果只是历史数据，不能作为物理释放、重新调度或创建新 attempt 的权限。

## Root 验证

| 轮次 | 结果 | 输入数 / 原始日志字节 | 原始日志 SHA-256 |
| --- | --- | --- | --- |
| RED01 | 仅批准的新 state/result/constructor/step 缺失 | 9111 / 1451 | `3ab59ef57401a3c7fa8e681474c8454e55a616adb31387ca5f295fe082d0fed4` |
| host01 | 新恢复测试 PASS，2.579s | 9112 / 18170 | `b92000c9d25fe12bf10c3f67102662364d0c9e51cedcf494307ca28a25816261` |
| race01 | 恢复及 Resolve/postread/cohort/journal/public quota 定向回归 PASS，8.130s | 9117 / 76992 | `68e36865039fbe7b650de78ebeda7560b6f5944f8fb62253740572f0c52592c7` |
| Linux vet01 | etcd 包，Linux arm64、CGO=0，PASS | 9207 / 0 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

Root 在每轮后、后续 Go 或源码修改前核对全部 17 项编译输入、pre/post、工具、复用仓库快照及当时 current 源码，指纹一致。gofmt 与 diff check 干净。使用主仓库共享缓存，没有完整依赖或工具链归档。

测试覆盖先 replay 后 journal/slot、无 journal 的普通 fresh 数据、原 aborted 证明、committed 完整引用、每个读取阶段失败后重试、Resolve 预算原子性、Unknown 保留原 journal/reference、canonical journal 和 aborted receipt 出生拒绝、初始/中途/done 重绑定、业务 cap，以及 Release/Close 等待实际 native 返回。超大或损坏回复注入的预期 WARN 对应 fail-closed Unknown，不宣称无警告执行。

单文件内的 cursor、阶段和 binding 校验共同维护一个并发所有权与协议不变量，未扩大包接口；状态、失败行为、计费和借用约束均有注释和逻辑分段。此切片尚未接入全局单工作区、原 32 job registry、自动 quota consumer、P 删除 gate，也不构成真实 etcd、大 N 容量或生产迁移验收。
