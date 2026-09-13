# ds-ai-research v0.3.22 部署验证

## 结论与范围

本轮已执行的部署 API 整改套件、完整工作区矩阵、单独普通 sync 回归和三副本并发 DELETE 补测全部通过。上轮普通 sync 销毁的 stale-token 503 未复现；补测的 27 个并发 DELETE 全部返回 200。

测试仅正常创建、使用和销毁专属沙盒，以及只读 Kubernetes/Redis 审计。未升级、回滚、重启/缩容 API，未强删 Pod、手工删除策略或业务 Redis 键。此处“通过”限于以下实际执行的场景，不代表所有 CNI、生产 LSM 或故障切换验收。

## 部署与端点

- 集群/namespace：`ds-ai-research / aiadp-sandbox-fuse`。
- 三个 API 都是 `registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.3.22`，摘要一致：`sha256:815a7f84f0c6f32fd19d4fe38bebc789f62fd0211dd9b6be7c0c10f761e2b906`。
- 三副本分别直连本机 18081/18082/18083：`sandbox-fuse-api-7997ffc49d-287rx`、`sandbox-fuse-api-7997ffc49d-crs4c`、`sandbox-fuse-api-7997ffc49d-mv9hv`。
- 沙盒 runtime 仍为 v0.3.4。此次 API-only 更新未改变池运行时契约；测试前存在的兼容普通/FUSE 池继续使用，随后正常消费并补池。
- 测试前后各副本 `/health`、`/ready` 都为 200，未认证访问受保护 API 为 401；三个 API 均 Ready、零重启。
- 仍是 Cilium 提供方、standalone Redis，启动日志仍有缺失生产 LSM 绕过警告；没有擅自修改这些部署配置。

## 套件结果

先单独执行此前稳定失败的普通 sync 场景，再并行执行两个完整套件。临时 UID/并发 DELETE 补测在两个套件全部结束后才运行，避免其它测试消费其预备库存。

| 项目 | 结果与耗时 |
| --- | --- |
| 单独 `ephemeral_sync_finalization` | PASS，65.95 秒；包含销毁后重开读取持久化内容 |
| `TestDeployedAPIRemediation` | PASS，272.17 秒 |
| 完整 `TestWorkspaceFUSE` | PASS，408.32 秒，8/8 子场景通过 |
| 三副本补测 | 9/9 预备 UID 复用、跨副本文件读写、27/27 并发 DELETE 为 200、精确 Pod/策略/匹配 Redis 键收尾通过 |

API 套件覆盖三副本 Python/Node stdin、缺失及空文件、动态挂载/双向同步/卸载、真实 cgroup 128 MiB/100m 限额、指定 Service 放行/撤销/恢复、15 次集群字面目标拒绝、一次性 SSE 正常结束、FUSE flush 时间戳一致和不可变卸载冲突，以及正常销毁。

完整矩阵各项：

| 子场景 | 结果 | 秒 |
| --- | --- | ---: |
| ephemeral no workspace | PASS | 32.30 |
| ephemeral sync finalization | PASS | 66.24 |
| persistent sync manual sync | PASS | 69.05 |
| ephemeral FUSE finalization | PASS | 18.20 |
| persistent FUSE durable flush | PASS | 21.43 |
| same prefix cross-mode conflict both orders | PASS | 78.41 |
| different prefix cross-mode concurrency | PASS | 41.77 |
| FUSE filesystem stress | PASS | 80.91 |

两种 sync 和两种 FUSE 生命周期均实际验证销毁后重开内容；同前缀互斥两种顺序都执行，不再在首次 sync DELETE 失败处退出。压力测试仍用 256 个小文件和 16 MiB 上传，覆盖目录、覆盖/追加/截断、重命名/删除、Git、真实 FUSE mount、flush 和销毁，不将它称为默认 10000 文件/1 GiB 压力测试。

## 精确 UID 与并发 DELETE 补测

每种模式先读取三条 prepared 库存的 runtime ID/UID，再让三个副本各创建一个独立前缀沙盒；在其它副本写文件并从三个副本读取；同时向三个副本 DELETE 同一个 ID，等待全部结果。随后确认所有副本 GET 为 404，原 runtime 名称不在 Pod 清单，对应 NP/CNP 不存在，按业务 ID/runtime ID 扫描匹配 Redis 键为空。deferred API cleanup 的返回状态也校验为 200/404。

| 类型/创建副本 | 业务 ID | runtime Pod | 请求前 prepared UID |
| --- | --- | --- | --- |
| ephemeral sync / 0 | sandbox-d5zjftkuk8 | sandbox-pool-sucnysc0ug | abce05c7-1d57-44bb-830d-8625cb141c09 |
| ephemeral sync / 1 | sandbox-0ddaboq5ab | sandbox-pool-y9muxeb7k8 | 97f4a92f-b840-46f8-acfa-eb3166e2fb28 |
| ephemeral sync / 2 | sandbox-6tas4dtqg5 | sandbox-pool-pahuubjawd | 40551c38-77f2-46db-aeaf-48a1cc353e19 |
| persistent sync / 0 | sandbox-qvbfqxcg1e | sandbox-pool-5eks7iwva6 | bca26464-cf70-43b0-8016-eb0859bd0384 |
| persistent sync / 1 | sandbox-lzi5fci2c2 | sandbox-pool-a3lvivuwi5 | 51b896bb-eba7-4f43-aa8b-3bbee34dc021 |
| persistent sync / 2 | sandbox-9n138lvbyu | sandbox-pool-8dd1hn05it | 2e5ceb83-a896-435e-a5d2-b1bd3b1d6d48 |
| persistent FUSE / 0 | sandbox-3eyvwxxexy | sandbox-pool-xdmztfudlq | 33283440-f9c7-4b0d-bfdd-b26c0e4102ce |
| persistent FUSE / 1 | sandbox-tr43azcxzo | sandbox-pool-phqc4i2jw4 | 77e61766-9d4a-4629-a7e2-59b3389c8c9a |
| persistent FUSE / 2 | sandbox-465vmbh9dz | sandbox-pool-wpeb09sh95 | 4965f6de-79f6-407a-a166-8ee9d3141225 |

每行三个 DELETE 都为 `[200,200,200]`，无 stale-token 或 cleanup pending。普通 sync 创建为约 0.099–0.215 秒，并发销毁等待约 31.0–32.5 秒；FUSE 创建约 5.94–7.55 秒，销毁约 1.02–3.67 秒。

API 套件另外三次 FUSE 创建中位数 8.843 秒、最大 11.424 秒，其间有工作区矩阵并行。这些均为小样本耗时，不是长期 p95/p99 或公平独占性能对比。普通销毁仍遵守约 30 秒 graceful termination；本批解决竞争 503，不宣称已消除该等待。

## 最终现场审计

- 三 API、一 Redis、三普通池和三 FUSE 池 Pod；没有本轮活动/Terminating 沙盒。三个 API UID 未变化，全部 Ready、零重启。
- NP 共 8 条：两条基础策略与六条当前池策略；CNP 为 0，无本轮业务策略遗留。
- active record/controller、session v2、ephemeral lifecycle 和 workspace lease 的扫描为空。
- 当前普通 scope `bf57c52e7a5af642` 库存为三个 prepared，分别绑定 `sandbox-pool-i4wv2qpnta`、`sandbox-pool-au4hmty01z`、`sandbox-pool-a66n0c2y8o`；runtime UID 与现场对应。
- FUSE 库存为三个 prepared，绑定 `sandbox-pool-omed7oqnnm`、`sandbox-pool-b8cpu7voyt`、`sandbox-pool-athl08r6kc`；没有本轮 consumed/cleanup 库存。
- 汇总测试时段三 API 日志没有匹配到 ERROR、stale-token、reconciliation failed 或 cleanup deferred。
- 与上轮相同，其它普通 scope `97358698362833fd` 仍有三个历史 prepared 记录，更新时间为 2026-09-12T13:08:xxZ；没有推定属于当前 release 或手工删除。
- 仍有历史无 lease 的 sync owner `sandbox-8n2th3nl1q`，runtime `sandbox-pool-q9aqkmmqfx`、UID `dc230072-e992-41db-bed1-017dd00187b0`、generation 1。它早于本轮、不属于本轮测试前缀，未删除。因此“无本轮遗留”不等于 Redis 全局零历史记录。
- 测试完成后停止三个本机 port-forward，移除本轮临时补测源码与二进制；没有保留 API 密钥到文件。

## 可重复命令

启动对上述三个 Pod 的 port-forward，密钥从 Secret 注入 `SANDBOX_API_TEST_KEY` / `WORKSPACE_FUSE_API_KEY`，不得打印或写入报告：

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
  -run '^TestWorkspaceFUSE$/^ephemeral_sync_finalization$' -v -count=1 -timeout=4m \
  -args -runtime kubernetes -profile minio-sigv4-path-style-v1
```

## 未执行的验收

没有配置 chaos/object-counter driver，不计为 API/Redis 故障切换、对象零操作或真实复制 ACK 故障验收；没有生产 AppArmor/SELinux，Calico/双华云仍待对应环境。此次模板契约未变化，不代表“跨代退休删除预算中断→Pod 不存在→继续精确清策略”的现场故障注入通过，该路径仍由已提交本地回归覆盖。
