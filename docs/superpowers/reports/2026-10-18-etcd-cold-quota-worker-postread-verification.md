# Cold quota worker 分步确认验证

日期：2026-10-18。本切片实现原 worker Stage 完成后的两次独立付费 proof 读取，不接通自动清理入口。

## 行为与职责

原 Commit permit 增加 mint 时的 session、partition、Range 三个原始成员指针。新私有 postread 状态由该原 permit 构造，保留 owner/context、原引用及成员绑定；先前 Stage 的原 Revoke 已实际返回。它不能由普通诊断引用重建。

每次 step 最多执行一次 metadata 读取，支付原 Range 262144 字节、保留完整 W20、不使用 Claim。第一步读取 return（值上限 16384 字节）、验证并保存 compact stamp，然后返回 nil entry 让出执行；第二步读取 receipt（4096 字节），验证完整原 reference 相等后返回独立 Entry 副本。预算不足或失败保持原阶段，第二步重试不会重新执行第一步。完成态返回副本且仍检查原 owner、context、permit 和成员。

状态通过非阻塞 CAS 独占，独占跨度包含实际 native 返回和 typed decode。读取之后、发布下一阶段之前再次检查绑定和存活状态。原 consumed=1、journalAttempted=1、ACK/history revision、frozen mutation digest 等约束继续检查；不重新 Grant、不生成引用、不退回已经支付的额度。

新文件负责有限阶段和原始绑定；原 cohort 文件只新增三个成员字段及赋值。原 metadata/纯校验作为已验收依赖复用。包内私有边界保留原并发与事务约束，无新增导出权限。注释说明并发独占、失败后不推进、原对象生命周期和普通历史证明的权限边界；gofmt 与 diff check 干净。

## Root 执行结果

| 轮次 | 结果 | 输入数 / 原始输出字节 | 原始输出 SHA-256 |
| --- | --- | --- | --- |
| RED01 | 真实失败，仅缺少批准的新状态与两个方法 | 9105 / 1329 | `bd5e6ad696269dc86add5816b87c4cf39dc175f4fb0591829bf5c846932acad4` |
| host01 | FAIL，定位为新增测试的补额度辅助代码修改了 Claim 时间戳 | 9106 / 13374 | `e5ea0493c879f78d81ab883e79b642208ddfb96c9f2939c1f92b9b025a8bc5c5` |
| host02 | PASS，9 个顶层测试，1.559s | 9106 / 12168 | `0e6c8e4f4d56751bd5f7a3c603c0dd383f9dabbca9cb5c4ecc8f06372fc50ed7` |
| race01 | PASS，6.521s，包含 postread、cohort、metadata、纯校验与原 prepared/journal 回归 | 9111 / 57277 | `d7c4d2ebd94d0b20f4ccabd71941484d22b4af6fb53d6537a135bc5a7096d8ab` |
| Linux vet01 | PASS，Linux arm64、CGO=0，etcd 包 | 9201 / 0 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

各轮已在后续源码修改或 Go 运行前完成 compact 格式的完整 17 项源码输入、pre/post、工具、仓库快照与当时 current 审计。沿用共享缓存与压缩记录，未复制整个工具链或依赖缓存。

host01 中产品代码未修改 Claim；失败由测试自身调用原 cohort 的 `fund()` 同时更新两桶时间戳导致。仅将该处改为持原锁补充 Range tokens，保留已有 future earning origin、全部 Claim 字段及原严格断言，产品代码未变。失败日志保留，不将其记为通过。

新增测试验证两步各一次计费、Q2 额度拒绝后直接续跑且不重读 Q1、Claim 完全不变、返回副本、typed 失败不跳阶段、完整 reference/birth/canonical receipt 绑定、复制或替换原对象拒绝、值上限、未知 ACK，以及取消后 Release/Close 仍等待实际 native 返回。并发败者不能发送第二个读取。静态检查确认不新增保留 Q1 raw KV 或 history 副本，原 permit 图仍保留。

## 后续范围

Entry 仍是 quota-only 历史证明，不表示物理退出、E/S 终态或 P 删除权限。状态只增加 compact stamp，但仍通过 permit 保留原 prepared/history 图；32 个任务的完整保留内存上界尚未验收。

完整 worker 历史认证、未知 attempt 的付费 Resolve、自动 actor/删除 gate、v3 命令配置接线及真实 etcd/容量/恢复组合仍开放。本轮 host 协议测试与 Linux 静态检查不能替代这些交付。
