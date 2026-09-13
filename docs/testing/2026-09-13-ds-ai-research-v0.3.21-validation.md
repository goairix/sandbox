# ds-ai-research v0.3.21 部署验证

## 结论与范围

本轮不是全量通过。API 跨副本整改套件通过，普通/FUSE 并发预备 UID 复用与精确收尾补测通过，FUSE 持久化和文件系统压力通过；工作区矩阵中三项普通 sync 销毁返回 `503 / SANDBOX_CLEANUP_PENDING / active sandbox stale token`，矩阵整体 FAIL。

仅通过 API 创建、使用和正常销毁测试沙盒，读取 Kubernetes/Redis 状态。没有升级、回滚、重启、缩容 API，没有强删 Pod 或手工删除 Redis/策略资源。没有实施新的生产代码修复。

## 实际部署及基线

- 集群/namespace：`ds-ai-research / aiadp-sandbox-fuse`。
- 三个 API：`sandbox-fuse-api-69ddd69ddc-f4n7l`、`sandbox-fuse-api-69ddd69ddc-lr7hw`、`sandbox-fuse-api-69ddd69ddc-w5wbc`，分别直连本机 18081/18082/18083。
- 镜像 `registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.3.21`，三副本摘要一致：`sha256:ca0ef716c5dc47971b088e262cf4e597b786c3fff706031da81226d5335dd89a`。
- 沙盒镜像仍为 `sandbox-runtime:v0.3.4`。API 更新不要求兼容的普通/FUSE 池 Pod 重建。
- 测试前为三个 API、一个 Redis、三个普通池和三个 FUSE 池 Pod；8 条 NetworkPolicy，没有此前的 `sandbox-sandbox-pool-udjrjjtses` 策略。这里只报告本轮基线，不推断历史策略消失的操作来源。
- Cilium 提供方、Redis standalone、缺失生产 LSM 的本地验证开关保持既有部署配置。本轮不代表其它 CNI、生产 LSM 或 Redis 高可用验收。

## 功能结果

| 项目 | 结果 |
| --- | --- |
| 三副本 health/ready/认证 | 各副本 health/ready 为 200，未认证访问受保护 API 为 401 |
| `TestDeployedAPIRemediation` | PASS，241.17 秒；跨副本 Python/Node stdin、缺失/空文件、挂载/双向同步/卸载、实际 cgroup 限额、Service 放行/移除/恢复、集群字面目标拒绝、一次性 SSE、FUSE flush/info/unmount 冲突及销毁 |
| `TestWorkspaceFUSE` | FAIL，296.15 秒；5 项子测试通过、3 项失败，见下文 |
| FUSE 文件系统压力 | PASS，70.35 秒；256 小文件、16 MiB 上传及既有覆盖/追加/截断/重命名/目录/Git/flush/销毁断言 |
| 三副本普通/FUSE 并发补测 | 6/6 请求前预备 UID 复用、跨副本文件写入/读取、第三副本销毁；原 Pod 不存在，相关 NP/CNP 不存在，按业务 ID 与 runtime ID 扫描匹配键为 0 |

两个 Go 套件有部分并行，补测与压力测试也有重叠；下述耗时是小样本观测，不是独占热池基准或长期 p95/p99 承诺。

普通并发创建为 0.175 / 0.186 / 0.196 秒，销毁为 31.113 / 31.546 / 32.355 秒。FUSE 并发创建为 1.853 / 3.894 / 8.486 秒，销毁为 5.488 / 5.096 / 2.088 秒。API 套件另有三次 FUSE 创建，中位数 4.942 秒、最大 5.676 秒。生命周期长尾仍存在，不能归为本批性能优化成果。

补测显式登记的六个业务 ID / 原 UID：

| 业务 ID | runtime Pod | 原 UID |
| --- | --- | --- |
| sandbox-hmyuzf47qt | sandbox-pool-iz9d57km9o | 0bc36f8a-38a6-4723-8412-e531e0323528 |
| sandbox-l8onys33ua | sandbox-pool-q4bjxiziid | 21273486-d38f-4ab2-b213-9355ce0f71ed |
| sandbox-rnw6b4mdue | sandbox-pool-7plc0d4qdw | bf1064ab-d8d0-4ec5-8025-4fc997e3d9da |
| sandbox-fpgtmddo61 | sandbox-pool-7pmuyyptz8 | 86e995d1-8839-489a-9d0c-0d8d3303d217 |
| sandbox-8s6o5qrlaq | sandbox-pool-o7vj4befgb | 03b7d5d7-5c0b-43f6-9d3d-01e89ca590eb |
| sandbox-uuekm2nrsn | sandbox-pool-d6sb3tuyk8 | d542d76d-2978-4360-9bb0-ffe032bcf5ee |

## 工作区矩阵失败

- ephemeral no workspace：PASS，34.41 秒。
- ephemeral sync finalization：FAIL，33.24 秒，`sandbox-k4994p9obm` 的 DELETE 返回上述 stale-token 503；该测试在销毁处退出，不能算持久化重开通过。
- persistent sync manual sync：FAIL，35.60 秒，`sandbox-2avlzbt5q1` 同样在 DELETE 失败，不能算重开验证通过。
- ephemeral FUSE finalization：PASS，17.77 秒，包含销毁后重开读取内容。
- persistent FUSE durable flush：PASS，21.80 秒，包含销毁后重开读取内容。
- same prefix cross-mode conflict both orders：FAIL，35.62 秒，sync→FUSE 的拒绝已验证，但 `sandbox-slixxhmmix` 销毁 503 后退出，不能算双方向互斥全部通过。
- different prefix cross-mode concurrency：PASS，47.37 秒；该原测试的 deferred DELETE 忽略错误，所以还需最终现场审计支持，不能仅凭子测试 PASS 判断清理成功。
- FUSE filesystem stress：PASS，70.35 秒。

失败 ID 的匹配 Redis 键随后为空；不等于请求时没有错误，也不等于整个 namespace 没有历史记录。

两套测试与并发补测结束后，单独复测 `ephemeral sync finalization` 仍 FAIL，32.97 秒：`sandbox-tlswz38l36` DELETE 同样返回 stale-token 503。该结果排除了必须依赖本轮两套测试并行才能触发的解释。复测匹配到的 fault matrix 因未配置驱动明确 SKIP，不计为通过。

## 竞争路径排查

日志确认普通 sync 的最终文件同步已经完成，之后多副本持续记录该 ID 的 cleanup pending。代码中存在能够解释本轮现象的接管路径：

1. `sync_lifecycle.go:246` 发布 `sync_final_output_done`，随后精确 runtime 删除等待正常终止；完成后还需再次 Fence/checkpoint。
2. `lifecycle_coordinator.go:280` 的检查点恢复分支早于本地 lifecycle 判断，直接进入 `destroyDistributedSandbox`。
3. `cleanup_distributed.go:100` 发现本地 controller 就调用 `retireLocalSyncController`/`Stop`，可能在原请求仍执行时取消并释放其 controller。
4. 原请求继续 Fence/checkpoint 就可能收到 stale token，恢复工作仍可完成清理。

这是代码路径与现场结果的对应分析，不包含每个失败请求当时 controller token 的完整取证，也尚未添加强制竞争的本地回归。本轮没有修改安全校验或实现修复。

后续应补充“最终同步已 checkpoint、原 worker 仍在删 Pod、后台扫描重叠”的确定性回归；本地活跃 finalizer 不得被恢复路径撤销。真正失效的 controller 仍须允许其它副本接管，并保留 UID/CAS fencing，不能简单禁用后台恢复或把任意 stale token 吞成成功。

## 最终收尾与历史状态

- 三个 API Ready、零重启；namespace 最终为三个 API、一个 Redis、三个普通 prepared 与三个 FUSE prepared Pod，没有本轮活动或 Terminating Pod。
- 最终 NetworkPolicy 为两条基础策略和六条当前池策略；CiliumNetworkPolicy 为 0，没有发现本轮测试策略遗留。
- 活动 record、session v2、ephemeral lifecycle 和 workspace lease 的 Redis 扫描均为空；四个失败 ID 的匹配键均为空。
- 当前 release scope `bf57c52e7a5af642` 的普通池记录为三个 prepared，与现场 Pod UID 对应；FUSE 记录同样为三个 prepared，没有 consumed/cleanup 记录。
- Redis 还有其它 scope `97358698362833fd` 的三个旧普通 prepared 记录，更新时间均为 `2026-09-12T13:08:xxZ`，早于本轮。未推定它们属于当前 release，也没有删除。
- 另有一个无 lease 的历史 sync owner，业务 ID `sandbox-8n2th3nl1q`，prefix `workspaces/validation/api-1789270566488168000/dynamic/`，不属于本轮 remediation/matrix/v0321-audit 前缀；本轮没有删除该 owner。因此不能声称 Redis 全局零遗留。
- 本机三个 port-forward 在验证完成后停止，临时补测驱动已移除。生产代码没有变化，本报告记录失败，不代表修复完成。

## 可复现命令

API 密钥从 Secret 注入环境变量，不写入报告或工具输出。

```sh
SANDBOX_API_TEST_URLS=http://127.0.0.1:18081,http://127.0.0.1:18082,http://127.0.0.1:18083 \
SANDBOX_API_TEST_FUSE=1 SANDBOX_API_FUSE_SAMPLES=3 \
SANDBOX_API_INTERNAL_TARGET=k8s-service://aiadp-sandbox-fuse/sandbox-fuse-api \
SANDBOX_API_INTERNAL_URL=http://10.96.14.111:8080/health \
SANDBOX_API_CLUSTER_LITERAL_TARGETS=10.0.0.0/24,10.0.7.0/24,10.96.14.111/32,10.96.0.0/16,169.254.169.254/32 \
go test ./test/integration/api -run '^TestDeployedAPIRemediation$' -v -count=1 -timeout=12m

WORKSPACE_FUSE_API_URL=http://127.0.0.1:18083 \
WORKSPACE_FUSE_SMALL_FILE_COUNT=256 WORKSPACE_FUSE_LARGE_UPLOAD_BYTES=16777216 \
go test ./test/integration/workspacefuse -run '^TestWorkspaceFUSE$' -v -count=1 -timeout=15m \
  -args -runtime kubernetes -profile minio-sigv4-path-style-v1

WORKSPACE_FUSE_API_URL=http://127.0.0.1:18083 \
go test ./test/integration/workspacefuse \
  -run '^TestWorkspaceFUSE/ephemeral_sync_finalization$' -v -count=1 -timeout=3m \
  -args -runtime kubernetes -profile minio-sigv4-path-style-v1
```

## 验收限制

此次沙盒模板指纹未变化，没有触发旧池跨代退休。因此正常流量收尾通过不能代替本批修复的“退休预算中断→Pod 已消失→按精确 UID 继续清策略”现场验收；该窗口已有提交中的本地回归。

没有配置 chaos driver、对象计数驱动或生产 LSM；不计为故障切换、无工作区对象零操作或 AppArmor/SELinux 验收。Calico / 双华云仍待真实环境。
