# etcd 自动物理任务结果保留：核心验证（2026-10-17）

## 范围与结果

- 修复 I2：自动入队未等待的 job，其实际完成错误原先随 registry 移除而丢失；现在原 worker 终态保留固定历史摘要。
- 摘要只在原物理/准备 owner、transport、Claim、caller callback 实际返回后，与 registry 移除及 job.done 同锁发布。
- 自动模式聚合该 worker 的全部 job（包括显式提交）；显式调用仍取得原始 per-job result/error，非自动模式不聚合 job 错误。
- 成功恢复不清除历史失败；单个失败不取消 actor，不阻塞后续 reconciliation，不改变永久 E/S/P 的归还条件。
- 只有全部实际叶均为 context.Canceled 才作为正常取消；混合 unknown、deadline、dependency 不能被 errors.Is(Canceled) 吞掉。
- DueWorker.Stop 返回固定摘要，现有 deployment.Wait/Close 与命令消费链无需产品修改；直接 Backend.Close 保持原来的 session 清理错误契约，只承诺实际 join。
- CleanupFailures 为饱和 uint64 诊断计数，不授予执行、重试、释放槽或完成的权限。

## 有界保留

- 一个 [256]byte 首个诊断、uint16 长度/固定 sentinel 位图、uint64 计数；只保留一个历史对象和一个不可变终态副本。
- 不保留历史 job/Task/任意 error 图，不为本功能增加逐 Task goroutine，也不累计 errors.Join 历史。
- 11 个静态 sentinel 包含 canceled、deadline、unknown 与本包校验/冲突类；任意其他依赖错误仍为非 nil、非纯取消结果。
- 额外保守计账 1KiB，包含在原 automatic 4MiB 预留中；3,670,016+1,024=3,671,040B < 4MiB。
- 原 64MiB/10000 identity 上限、原 1MiB 候选证据预留及原 32 active job 不扩容；不宣称 RSS/容量实测。

## 验证证据

- 新 OWN：`.superpowers/sdd/2026-10-17-etcd-automatic-result/`；Root 配置的 check.sh/audit.py 未修改。
- result-red-01：新增行为测试编译成功并以 exit 1 复现丢失错误；完整17字段/源与工具归档/current 审计在修改产品前 PASS（9066 输入）。
- RED 原始日志 4326B，SHA256 `46a836f0e5a47f4915d5435a13e62a7710903404172f2ae1bb6b2a223a87145d`；失败证据保留。
- result-host-01 为首次 GREEN，审计后仅修 gofmt 对齐并追加计数/actor 存活断言；以下为冻结最终源的验证。
- 最终 host/race selector：`^TestDueWorker(CleanupResult.*|Lifetime.*)$`，覆盖 8 个结果组及 6 个原生命周期组。
- result-host-02：exit 0，9067 输入，日志 5382B，SHA256 `3221b6768423325986980780c67cb4223b6266e955b51d57541e9dff47a9e7c2`。
- result-race-01：exit 0，9072 输入，日志 5380B，SHA256 `926e69add599e1c9512f24d1e043a199be244d5edd1c0ebb778db7828c64892e`；go-list 与 test 均带 -race。
- result-vet-01：Linux arm64 vet exit 0，9159 输入，原始日志为空。
- result-build-01：Linux arm64 test -c exit 0，9159 输入，原始日志为空；仅构建，没有执行 native。
- 构建 ELF SHA256 `7b3983ba9573f1a621b41e5312fb53b206a07808c290ff94a0386d3470521b1d`。
- 四个最终 epoch 均在下一 Go 或修改前完成 full17、前后 exact set/hash、源/工具实际绝对成员归档及 current 审计，全部 PASS。
- 使用 Go 1.25.6、GOTOOLCHAIN=local、GOMAXPROCS=2、独立 OWN GOCACHE、既定 GOMODCACHE/GOPROXY；完整命令及环境在每个 command.json。
- gofmt -d 对五个修改/新增 Go 文件输出为空；未改模块依赖、automatic/birth/backfill/DELETE 产品文件或旧 native 断言。

## 覆盖边界

- host 的原生成 KV/Lease 连接与完成 callback barrier 仅证明生命周期/错误发布；不冒充物理 Kernel/外部副作用证据。
- 覆盖已移除 job、纯/混合取消、全部固定 cause、任意依赖、10000 次同 error 完成、饱和计数、不可变副本、32 job 并发与实际 callback join。
- 真实自动物理 job 的 bound-ACK unknown、后续正常自动恢复及 deployment/独立进程非零关闭，留待单独 native 组合与 Root 执行；本报告未将其记为 PASS。
