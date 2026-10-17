# Worker cold quota 分步 prepare 验证

新增私有 P1/P2 cursor，每次至多一次 W20、Range262144 读取，不消耗 Claim。P1 用共享纯 decoder 验证原 immutable ready，P2 单独读取原 slot/free，各限制 16KiB；额度不足和 typed 失败保留当前阶段及原 ready，不重读 P1。合法槽位及返回后的原绑定检查均通过后，才生成一个 UUID 并复用原 mutation/digest 冻结逻辑。Done 返回同一个未使用的 prepared 指针，复核原 reference/mutation 和一次性 consumed/journalAttempted 标志。

构造和每阶段绑定原 registered owner/context、history 指针、worker session/partition/Range、Task/birth/Owner 和完整 quota。历史保持借用不可变，P2 完成后丢弃 raw ready；未新增 heavy history 副本或跨包可变状态。状态机和纯 slot/freeze 逻辑按文件职责分离，注释说明并发所有权、计费、失败、借用和 UUID 边界，Root 已检查排版及包职责。

公开 coldReady/prepare 共享纯 ready、slot 和 freeze 校验，保留 profile → ready read → slot/free read → validation → UUID → freeze 的原顺序、错误及 RPC。此 cursor 仍需先完成历史认证与恢复，不授予物理清理、调度或新 Stage 权限。

## Root 执行记录

| 轮次 | 结果 | 输入数 / 原始日志字节 | 原始日志 SHA-256 |
| --- | --- | --- | --- |
| RED01 | 仅批准的新 cursor 类型/构造/step 缺失 | 9111 / 1074 | `19f686c238375c5ff289f10f85ce6f2927ebdc9a56d4017756d680d9e298b351` |
| host01 | FAIL，新增分区等值限制错误拒绝合法输入 | 9113 / 54085 | `ae776a5fd75046be7c7b399744967785b56e4122d601d98fa6e49f1ed9c1403f` |
| host02 | 修正后全部新测试 PASS，1.506s | 9113 / 12472 | `5f85670d37bad4b8b9b61c07101ac91b3b8e2560c935b805f5839581251010ad` |
| race01 | 新 cursor 及公开 prepared/journal/quota/inventory 定向回归 PASS，4.611s | 9118 / 20538 | `d450e64a51703f81f44c007904e9bf0a318114d13b24018ac50949fd122ca9ff` |
| Linux vet01 | etcd 包，Linux arm64、CGO=0，PASS | 9208 / 0 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

各轮在后续 Go 或源码修改前完成全部 17 项编译输入、pre/post、工具、仓库快照和当时 current 源码核对。共享构建/模块缓存与仓库输入快照，未归档完整依赖或工具链。gofmt/diff check 干净。

host01 暴露的额外 `Quota.Partition == Owner.Partition` 限制违反既有独立 quota 分区协议。Root 沿原 reservation/slot 校验定位根因，作者只删除该额外条件并补充注释，原完整 quota/槽位/worker 绑定以及测试、断言均不变。独立 Task 审查限定复核确认该 Important 问题已解决，无其他发现。

测试验证 Claim 全字段不变、原 ready 保留、严格当前组重试、共享 mutation/reference、合法 P2 前零 UUID 字节与成功后一次 UUID、canonical ready/slot/free 拒绝、三个 value cap、初始/中途/done 重绑定、prepared 被使用或修改后拒绝、返回后 context 更换，以及 Close 等待真实客户端返回。host02/race01 无失败、跳过或警告。

这只是分步 prepare 的 host 协议验收。全局共享工作区、原 32 job 保留内存核算、job 正向来源、自动 quota consumer/P 删除 gate、生产配置、真实 etcd 故障和大 N 容量仍未完成。
