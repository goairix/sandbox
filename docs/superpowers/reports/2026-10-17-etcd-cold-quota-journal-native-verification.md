# etcd cold quota journal 原生 public 链验证

日期：2026-10-17。此次改动仅增加 native 测试与 fixture 接线，产品实现保持不变。

## 结果与覆盖范围

最终 Linux build02、Linux vet01 及各自独立输入审计通过。两个新场景分别在 fresh 容器中通过，容器均退出 0，无超时、无 OOM，并完成精确名称清理。

| 场景 | 最终结果 | Go 场景耗时 | 容器运行时间 | 原始日志 |
| --- | --- | --- | --- | --- |
| `quota-journal-success` | success02 PASS | 3.40 秒 | 约 3.56 秒 | 4570 字节 |
| `quota-journal-ACK` | ack02 PASS | 3.40 秒 | 约 3.56 秒 | 5142 字节 |

此次仅执行以上两个新增 case；没有重播旧 native 全矩阵，也不将旧矩阵列为本次新增验证结论。

这条 fixture 使用 public 元数据接口完成 cold 创建与发布，随后执行真实 runtime/observer 物理终止、真实 provider 客户端调用及本地 owner release。provider 的远端是带原始 UID/RV 前置条件检查的测试 HTTP fixture。它未覆盖生产业务 HTTP Create/DELETE 入口或真实 Kubernetes 集群，也未接入 automatic quota worker。

## 真实执行链与权限边界

两个场景复用 `runLocalRuntimeReleaseNativeMode(..., true)` 的完整 cold-create、Supervisor、observer、archive/local finalization、provider cleanup 和 original64 owner-release 链。quota 调用发生在原 runtime 已物理终止并完成安全 release 之后。

原 zero-cap worker 创建真实 `k.NewTaskRuntimeCleanupClient`。新 Backend 复制同一份完整 `state.Options`，保留 `LocalRuntimeRelease`、`TaskRuntimeTerminationIssuer`、`Clock`、`PublicationTrust`、`EnrolledPublication`、`TaskWorkspaceArchiveRecovery`、`TaskWorkspaceSyncRecyclingBudget` 和原 `PreparationInventory`；只切换测试代理 endpoint，并为每个 Backend 创建独立 0700 recovery 目录。

没有导出 Backend 内部 getter、扩张存储角色权限、新增 authority 注入接口或回调，也没有用 fake 签名、fake release、fake Run 或原始删除协议记录替代真实链。

Root 独立核对两次实际容器 inspect、命令与原始输出，确认：

- `Privileged=false`，网络 `none`，private cgroup namespace，只读根文件系统，内存 512 MiB、CPU 2、PID 上限 256。
- drop ALL 后仅加入 `KILL/SETUID/SETGID/SETPCAP/SYS_ADMIN/SYS_PTRACE`；启用 no-new-privileges，保留默认 seccomp，AppArmor 为明确配置的 unconfined。
- runner、monitor、etcd 三个 ELF 均只读挂载；实际挂载文件哈希与采集前后当前文件一致。
- 真正执行 public quota 调用的 worker 为 UID/GID 1000，Permitted、Effective、Inheritable、Ambient、Bounding 五组 capability 均为 0，NoNewPrivileges 为 true。
- worker 的真实 `Cmd.Wait=<nil>`；Supervisor 的真实退出信号为 observer 触发的 SIGKILL；provider 实际产生 2 PATCH、1 DELETE，原 pod 已不存在。
- TLS 代理 handler/Serve/upstream、observer Serve、HTTP fixture handler/server 均完成 join；原 TLS etcd 进程被终止并 Wait；两个精确容器名称清理后 inventory 为空。

## journal 与 public 恢复证据

代理仅在两个新模式启用，精确匹配当前 intent 的 `cold-runtime-quota-attempt` key。它转发真实 etcd 请求，只可丢弃一次已成功 CAS 的响应，不构造成功响应或修改请求。每次 quota Grant 转发前，通过相同存储角色的独立上游 KV 客户端线性读取 journal；该读不会递归经过代理或分配 lease。

成功场景实际 journal CAS 为 30 comparisons、1 success、3 failure，共 34 nodes，request 10933 字节。断言要求唯一实际 Grant 前，etcd 已存在对应原 candidate reference 的完整永久 journal KV；成功退款后 journal 保持不变。

ACK 场景实际执行以下顺序：

1. 首次 public `SettleColdRuntimeQuota` 的 journal CAS 已在真实 etcd 成功，代理丢失 ACK；返回 `OutcomeUnknown` 和原始非零 reference。首调用实际 Grant 数为 0，quota 仍占用，return 记录不存在。
2. 关闭并 join 首 Backend，使用独立 recovery 目录创建冷 Backend；直接再次调用 public `SettleColdRuntimeQuota`。fixture 没有先手动调用 `ResolveStage`。public 方法自己仲裁 journal 中的原 reference，取得原 aborted receipt，再构造 replacement。
3. public 调用成功后从真实 etcd 读取原 aborted receipt，核对原 reference 与最终 reference 不同，且满足 `oldJournal.ModRevision < abortedReceipt.CreateRevision < replacementJournal.ModRevision`。
4. replacement 实际 CAS 为 37 comparisons、1 success、3 failure，共 41 nodes，request 13373 字节。断言逐项核对它对原 journal 和原 aborted receipt 的 Value/CreateRevision/ModRevision/Lease 四项 fence。Version 仅用于记录校验，没有给产品增加第五项 compare。
5. 全场只允许一次实际 quota Grant，以及一次成功退款事务。Grant 前真实读取的 journal KV 必须精确对应被 ACK 的 replacement candidate。

两个场景的实际退款事务均通过 34 comparisons、4 success、4 failure、42 nodes 断言。原 quota slot、free index、exact return 与 committed receipt 的写入 revision 相同；return/receipt 为无 lease 的不可变记录，free slot 不再 occupied，保留原 quota reference。该检查发生在下一次 allocator 复用之前。

## epoch 复用与旧 public replay

退款前，原 workspace 已通过真实 `AcquireIntent` 存在新 owner，并通过真实 `ReserveColdPreparation` 复用了 preparation ID、epoch 加 1。退款后，再通过实际 `AcquireIntent`、`ClaimCreation`、`ReserveColdPreparation` 分配原 quota 分区的唯一可用 slot，要求 quota ID 相同、epoch 恰好加 1。

随后重放原 intent/task 的 public `SettleColdRuntimeQuota`，要求返回与原成功结果相同的 reference 和 Entry。代理记录该调用的真实 Range/Txn 读，并检查点读及区间覆盖，禁止读取旧 quota slot、free index 或 pending journal；也不允许再次提交 journal CAS 或退款事务。

重放前后，新 quota slot 的完整 KV、新 quota workspace owner KV、原 workspace 新 owner KV、已复用 preparation 记录、journal 和 exact return KV 均保持不变；新 epoch 的 free index 仍不存在。

## 构建、静态检查与证据定位

本 Task 证据根目录为 `.superpowers/sdd/2026-10-17-etcd-cold-quota-journal-native/`。Root 执行真实 Go/Docker；实现者仅编辑源码及报告，并进行只读检查。

Go 工具链为 `/Users/dysodeng/go/go1.25.6/bin/go`，环境包含 `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 GOTOOLCHAIN=local GOMAXPROCS=2`。实际命令记录于本 Task 的 `quota-journal-native-build-02-command.json` 与 `quota-journal-native-vet-01-command.json`：

```text
go test -c -o <本 Task OWN>/quota-journal-native-build-02-runner.test ./internal/runtime/controlrunner
go vet ./internal/runtime/controlrunner
```

两条命令退出码均为 0。各自通过 `go list -deps -test -json` 收集完整 17 类输入字段，共 9363 个输入文件；Root 独立重算输入全集、前后清单、当前源码、source/tools 归档逐成员、归档哈希和工具清单，build02 与 vet01 均 PASS。

最终 runner SHA256 为 `f7c4b876b4313c1cc5eee8d305010f5d8e4d8d308c82be5004e88c03738ea3c4`。两个真实容器使用该同一二进制和以下精确 selector，均带 `-test.count=1 -test.timeout=150s`：

```text
^TestColdRuntimeQuotaReturnNativeFullchain$/^quota-journal-success$
^TestColdRuntimeQuotaReturnNativeFullchain$/^quota-journal-ACK$
```

最终原始日志与结果分别为 `quota-journal-native-success-02-raw.log` / `-result.json`、`quota-journal-native-ack-02-raw.log` / `-result.json`；对应 `-command.json`、`-inspect-before.json`、`-inspect-after.json`、`-cleanup.log` 保留实际命令、资源及退出、清理证据。

| 原始日志 | 字节数 | SHA256 |
| --- | ---: | --- |
| success02 | 4570 | `31efbb3c9d5c45bf2bc7feb3701227ef85dfbd367af40a68fd637187b57e3910` |
| ack02 | 5142 | `4f1720c14d5a23f4a1ac27fd3f9b37ec0b2c4ad748567f5773593acbf6c6a40b` |

## 保留的首次失败

build01 编译及独立审计通过，但 native success01、ack01 整场均为 FAIL、容器退出 1。内部 worker 虽已通过 journal/refund/reuse 断言并正常退出，不能据此将整场记为 PASS。

唯一失败原因为父 fixture 的旧成功分类没有包含两个新 fault 名称：最终 HTTP 检查误走拒绝分支，期望 0 PATCH，而真实成功 cleanup 已产生 2 PATCH；Supervisor 日志也被误标为 fixture cleanup。修复仅在两处原成功分类中加入两个精确新名称，保留 SIGKILL、2 PATCH/1 DELETE/pod absent 的严格要求，其他旧 fault 分支、产品和已通过 helper/proxy 未修改。Root 对失败容器的资源、ELF、原始输出、退出 1、无 OOM 和清理也完成独立核对。

| 首次失败原始日志 | 字节数 | SHA256 |
| --- | ---: | --- |
| success01 | 4930 | `9de757e457079e15af0fed8b905265151d6dc9e63b6ff5415519e02ffa4280e8` |
| ack01 | 5498 | `6f27da5405cb92cf00f677329b7a2e03c20e4a2392b3acd23de2359ed3bd44b6` |

对应 `-raw.log` 和 `-result.json` 保留在同一 Task OWN 中；修复后重新构建 build02，再分别创建 fresh success02/ack02 容器取得最终通过证据。

## 仍未完成的范围

默认 Redis 切换、GC 策略与 automatic quota 接线仍为 open。生产业务 HTTP Create/DELETE、真实 Kubernetes 集群集成及旧 native 全矩阵此次均未验证。此报告证明上述 public 元数据协议与真实物理 release fixture 下的 journal/退款/重放行为，不据此宣称 quota 全生产链路完成。
