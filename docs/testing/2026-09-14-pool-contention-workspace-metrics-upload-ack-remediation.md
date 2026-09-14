# 普通池竞争、工作区销毁指标与上传确认整改

## 范围与状态

本次按 [批准规格](../superpowers/specs/2026-09-14-pool-contention-workspace-metrics-upload-ack-design.md) 和 [实施计划](../superpowers/plans/2026-09-14-pool-contention-workspace-metrics-upload-ack.md) 实施，代码基线 `f2b299c`，直接使用 `feat/workspace-fuse-mount` 工作区，不创建 worktree。

三项修复及本地回归已完成，各项独立规格、质量复审以及最终整体复审通过，无开放 Critical、Important 或 Minor 项。这不是新镜像线上验收记录。本轮未修改线上集群、Redis 数据、Helm 或 values。

## 普通池争用

可控回归确定性复现了两类旧错误：prepared 快照后真实 checkout CAS/业务身份迁移/消费确认与扫描交错产生 `ordinary pool record changed before cleanup claim`；claimed 退休快照与确认消费交错产生 `claimed ordinary record changed during retirement`。启动扫描和稳态 refill 在内存及独立 Redis 上均复现。

修复为：失败 CAS 先校验最新持久化记录，再触发记录索引与运行时库存整体刷新，最多 3 次并受 context 约束。不把失败清理当成功，不沿用旧索引继续 orphan 扫描；坏状态、存储失败、UID 变化、终止未确认和缺失 Pod 策略收尾错误仍保留恢复证据。

成功精确清理后的同轮旧库存也不再二次删除或重新收录。取池 CAS 不新增全局共享锁，稳态库存范围未扩大；容量回补和业务 UID 保留有明确断言。质量复审提出的重试预算断言已收紧为实际 3 次。

对应代码提交：`313eb5b`。

## 工作区销毁指标

内存与真实 Redis 的 sync/FUSE、ephemeral/persistent、同副本/跨副本共 16 个主场景先观察到重复扣减：sync 创建一次、三个请求成功后 active 为 -2；FUSE 为 -3。

FUSE 的负数还暴露了同一指标的一处遗漏：公共 `Create` 在 FUSE 分支提前返回，未经过普通/sync 成功创建的 active 加一。不能把 FUSE 验收目标降低为“负一但只扣一次”；本轮同时补齐成功创建加一和本地 FUSE 实际完成销毁减一，不新增创建计数维度或重构所有遥测。失败创建、prepared 池预热和 restore 不应重复添加本次创建量。

结果沿 sync/FUSE 生命周期、checkpoint 和 interrupted recovery 传播到现有销毁指标层：真正完成者返回 true；等待者、历史缺失确认和失败返回 false。正常并发请求仍可等待 owner 完成，后台扫描跳过 live owner；不新增指标锁或清理所有权机制。

最终质量复审又指出：删除调用可能在 Fence 后暂停，期间 controller 被释放/接管且 peer 完成删除；旧调用恢复后原仓储对缺失记录仍返回 nil。仅由 nil ACK 推断本次完成，会使两个存活请求计数两次，不能以“非崩溃事务 exactly-once”排除此边界。补充回归在内存/真实 Redis、两种生命周期模式的四个场景均先复现 active=-1、destroy=2。

修正是在原 Lua 调用中区分“本次删除”和“原本已缺失”，新增内部可选完成结果接口，只有实际删除且原 durability ACK 成功才返回 true。原 error-only 接口保留幂等行为；不新增 Redis 状态 key、结构或往返，原 replica ACK barrier/WAIT 不变。旧自定义仓储若只支持 error-only 接口，清理仍可成功，但指标保守地不猜测本次完成，可能少记而不会据此阻塞清理。

新增回归覆盖真正无缓存的 final-output/runtime-removed checkpoint 接管、历史完成、取消/超时等待者、上述原子删除交接窗口、旧仓储兼容和真实 Redis ACK 失败。故障注入包装器显式保留新接口行为，避免方法提升绕过原故障。指标仍是进程级 best-effort，不承诺崩溃后全局事务级 exactly-once；每个副本的 `sandbox.active` 增减量不是独立 release 总量，恢复/后台的既有遥测语义未在本轮重构。

对应代码提交：`0d5505c`。

## 上传助手

旧助手将空 200、204、缺字段、错误 path/size、非法/截断/尾随/超限 JSON 当成功；提前响应或请求失败后 pipe 仍可写。RED 回归已观察到这些错误行为。

新助手仅接受 HTTP 200，对最多 64 KiB 的完整单值 JSON 校验必填 `path`、`size` 与请求匹配，合法零大小使用显式字段存在性判定。响应读/关错误也失败。所有请求退出路径关闭 pipe 并等待 producer，继续流式 `io.CopyN`，没有按文件大小申请整块内存。

独立复审要求补强 producer 完成信号的断言和传输尚未返回时取消的覆盖，已补齐。随后五阶段敏感/超长错误回归均先观察 RED，再将错误显示改成固定阶段消息，`Unwrap()` 保留程序化错误及取消身份，避免远端响应头、GOAWAY DebugData 或 JSON 溢出数字被原样打印。

保留现有大文件实际 stat、FUSE flush、文件语义和销毁检查，未扩大压力规模。这里只加强测试验收，不将历史换网后未复现的文件缺失推断为已修复的服务端缺陷。

对应代码提交：`d0dfda4`。

## 本地验证条件

真实 Redis 使用本轮独立创建的 `redis:7.4-alpine` 临时容器、loopback 随机端口及随机测试 scope；不指向线上 Redis。普通池原子争用和 Redis 原子操作补充检查使用真实存储；上传边界使用本地 httptest/可控 transport。

三项均完成独立规格、质量复审，提出的问题均补回归并关闭。主流程在稳定最终补丁上重新执行以下验证，而非以修正交接窗口前的结果替代。

最终补丁上的主流程验证：

- `go test ./... -count=1`：通过，包括 API、sandbox、Docker/Kubernetes runtime、存储及各命令包；最长包 `internal/redisbootstrap` 54.685s。
- `TEST_REDIS_ADDR=127.0.0.1:56861 go test -race ./internal/sandbox -run 'TestOrdinaryPool|TestSharedOrdinary|TestOrdinaryRefill|TestOrdinaryRecovery|Test.*DestroyMetrics|TestWorkspaceDestroyLegacyRepository|TestFailedFUSECreate|TestDistributedOrdinaryCleanup|TestDistributedSyncCleanup|TestSyncFinalizer|TestDistributedFUSE' -count=10`：通过，157.551s，包含内存与真实 Redis 的普通池、工作区指标、交接、取消/历史完成和既有安全恢复回归。
- `TEST_REDIS_ADDR=127.0.0.1:56861 go test -race ./internal/storage/state/redis -run 'TestAtomic|TestActiveSandbox|TestActiveControllerAtomicCompletion|TestRedisStore' -count=3`：通过，2.718s，明确包含原子完成与 ACK 失败。
- `TEST_REDIS_ADDR=127.0.0.1:56861 go test -race ./internal/storage/state/redis -run 'TestSafety|TestFUSEPoolSafetyWritesRequireReplicaAck' -count=3`：通过，2.387s，保留连接绑定、barrier/WAIT 和故障不重放等原安全回归。
- `go test -race ./test/integration/workspacefuse -run 'TestUploadSized' -count=10`：通过，2.482s。
- `go build ./...`：通过，仅检查代码构建，不构建或推送镜像。
- `go vet ./internal/sandbox ./internal/storage/state/redis ./test/integration/workspacefuse`：通过。
- `golangci-lint run --timeout=3m --new-from-rev=f2b299c`：0 issues；只代表本次增量，不代表全仓历史 lint 清零。
- `git diff --check`：通过。

以上 Redis 地址只是本轮临时容器的实际端口，重复验收需重新创建独立测试 Redis 并设置新的地址。未启用的线上、大文件后端或故障注入 opt-in 用例仍会跳过；不把普通全仓测试中的 skip 当作这些场景的验收通过。

验证完成后已停止并删除本轮专用临时 Redis 容器 `sandbox-remediation-redis-20260914` 及其匿名数据卷，按精确名称复查均不存在；没有清理任何用户或线上 Redis。

## 部署与未关闭边界

产品修改涉及 API 二进制；上传修改仅属于测试助手。本轮未改 AppArmor loader、workspace mounter、Redis 启动辅助程序或 Chart，不需要增加 values 配置。镜像构建、推送和线上升级由用户执行；升级后再运行明确的有界线上专项验收。

普通销毁约 30 秒、FUSE 创建长尾继续后置；全仓历史 lint、生产 Redis/CNI 故障注入、Calico/双华云 VPC 现场验收、全节点完整 API AppArmor 挂载及外部 Ingress/长期负载验证不在本轮完成范围。本地测试通过不代表这些上线或高可用验收已完成。
