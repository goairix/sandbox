# API 测试报告整改与验证

日期：2026-09-13。对应现场报告：`2026-09-13-ds-ai-research-api-validation.md`。

## 已实施的代码整改

| 问题 | 整改 |
| --- | --- |
| 多副本 mount/sync/unmount 依赖本地缓存 | Kubernetes 请求直接读取 Redis active snapshot；不做 HTTP 转发。排他操作关闭新 admission，排空已有流，再核验 token 与 runtime UID。 |
| 工作区状态和 flush 信息不一致 | 共享 CAS 发布配置、owner、同步时间和 flush 状态；失败保留 transition journal，由 controller 接替清理。 |
| 卸载后旧副本继续续租 | 共享状态撤销旧 controller；旧 worker 精确退出，不销毁卸载后的沙盒或新 generation。 |
| 崩溃恢复卡住 | 保存请求级 excludes；最终同步完成在删除普通 Pod 前持久 checkpoint。FUSE quiesce 后可由 peer 重建仅清理的生命周期，不开放 admission。 |
| 普通 Pod 缺失与残留策略 | 真正 NotFound 转为 runtime 错误；清理绑定旧 UID 的 NP/CNP，包括 Pod 在两次检查之间消失的情况。UID 替换和不确定终止仍拒绝。 |
| 请求资源被默认池覆盖 | 与实际普通池资源比较；等价 quantity 可复用，不兼容请求直接创建并补齐默认资源。 |
| Python/Node stdin 空 | 使用 shell-quoted `-c` / `-e` 执行代码，stdin 独立输入。 |
| 文件不存在返回 500 | Kubernetes、Docker 普通及 FUSE 下载区分确认的文件缺失和 transport/权限错误；缺失 404，空文件 200。 |
| one-shot SSE 迟迟不结束 | 先持久关闭 admission，再由有界、tracked coordinator 清理；done 不等待 Pod 删除。持久化失败发 error 而非 done，HTTP 收尾清除 write deadline。 |
| Cilium 内部白名单返回成功但不可达 | 使用显式 `k8s-service://namespace/name` 转精确 namespace + Service selector；拒绝集群 CIDR 字面目标。allow/deny 联合核验 UID/attempt/readback。外部大网段覆盖私网 deny 的交集语义已补测。 |

保留 UID/generation/CAS、真实 mount/probe/PodReady 检查，不通过放宽这些安全条件解决故障。Docker 保持单进程模式。

## 网络配置变化

新增：

```yaml
config:
  runtime:
    kubernetes:
      podCIDRs: []
      serviceCIDRs: []
```

显式配置时两组必须完整、规范，IPv4/IPv6 地址族一致，包含未来分配范围。此模式跳过 inventory 扫描，适合大集群。留空则有界发现 Node/ServiceCIDR，Cilium IPAM 必要时读取 CiliumNode；缺失权威范围会拒绝不确定的目标。新版 Chart 包含所需只读 RBAC。详见 `../kubernetes-network-targets.md`。

不要把普通 HTTP URL 当作 Service 目标。例如 `k8s-service://backend/catalog` 只允许该 namespace 下匹配该 Service 全部 selector 的 Pod；拒绝无 selector、headless 和 ExternalName Service。

## 生产部署整改

新增 `productionSafetyChecks: false`，开发默认不变。生产可使用 `deploy/helm/sandbox/values-production.yaml` overlay：

- API 至少三副本、PDB 保留两副本。
- 外部 HA Redis、`requireHA: true`、带副本确认的 durability；示例 Sentinel 地址必须替换并通过现有安全方式配置凭据。
- 禁止 kind LSM bypass；禁止空、unconfined 和 label=disable profile。

Helm 只能校验配置，不能证明节点真实启用了 AppArmor/SELinux、Redis 故障切换达标。两者仍需部署验收。

`replica_ack` 的安全写入、Lua、独立同 slot 写屏障及 `WAIT` 使用同一主节点 TCP 连接；覆盖 active snapshot/controller/operation、workspace owner/lease/generation 和 FUSE pool 写入。屏障用于确认幂等无写重试，不修改业务 capability。确认失败保留清理证据，不以主节点读回代替副本确认。普通 Get/List 不增加副本等待，开发 `best_effort`/`native` 保留原路径。

Cluster 的安全写会按 key master 路由，只读审计遍历全部 master。遇 MOVED/READONLY/网络故障刷新拓扑并让当前操作保持 pending，不跨连接重试已写入操作；ASK 迁移期间保持 pending，直至迁移结束。当前 FUSE pool 的多 key Lua 使用既有非同 slot 键布局，不能据此声明整个后端支持 Redis Cluster；本次生产 overlay 使用 Sentinel，不应改为 Cluster 部署。真实 Sentinel 故障切换及丢失窗口仍待现场验收，副本 ACK 不等同于磁盘持久化或绝对无损故障切换。

owner 运维入口：`-audit-workspace-owners` 默认只读；`-recover-workspace-owner <hash>` 才请求精确恢复。审计使用只读 runtime 构造和非迁移 session 查询。存在 lease、active/session/ephemeral 记录、同名新 UID 或不确定 termination 时拒绝恢复，永不重置 generation。历史 owner 不含 Kubernetes namespace/release 归属，因此当前工具拒绝恢复这类记录；普通 sync 缺少最终输出证明也只报告阻塞，不当作成功清理。正常有 journal/checkpoint 的生命周期接续不受此限制。不要强删业务 owner/lease；活跃旧 worker 必须先正常退出。

## 仓库验证

已运行本机独立 Redis（非业务 Redis）的真实 active operation/owner/lease 测试及三 Manager 回归，包含跨副本工作区控制、排他流排空、失效 token、UID 防护、崩溃接续和失败不提前释放证据。

已通过：

- `TEST_REDIS_ADDR=127.0.0.1:16380 go test -p 1 ./... -count=1 -timeout=120s`
- `TEST_REDIS_ADDR=127.0.0.1:16380 go test -race -p 1 ./... -count=1 -timeout=120s`
- `go build ./...`、`go vet ./...`
- `golangci-lint run --new-from-rev=HEAD ./...`：0 issues
- `bash scripts/test-helm-chart.sh`：lint/render/env 唯一性验证通过
- `bash scripts/test-helm-backend-switch.sh`：backend switch/drain render 验证通过

最终验证使用全新的本机隔离 Redis，并将 Go package 串行执行；不同 suite 共用 Redis 全局池索引的并行干扰不记为产品缺陷。强确认补测覆盖 13 条 FUSE pool 写入、Store 安全写/no-op、同 TCP/同 key master、连接中断/拓扑重选、全 master 审计，以及 lease/session/pool/active 的重试缺失确认。协议级 RESP 测试及无真实副本时的失败测试不替代现场故障切换验收。

新增低基数 `sandbox.workspace.stage.duration` 阶段观测，覆盖 acquire/prefix/lease/bind/authorize/network/readiness/probe/publish。未依据有限样本调低 readiness 安全检查。noop 观测基准为本机 Apple M5 约 236–262 ns/op、552 B/op、4 allocs/op；这不是 FUSE 创建耗时或线上性能结论。

## 构建、同步与部署后验收

本次需重建 **sandbox-api 镜像**并同步 **完整新版 Helm Chart**；没有修改 workspace-mounter/probe 可执行代码，无需因本次整改重建它们。使用新镜像 tag/digest，确保所有 API 副本运行同一版本。构建和部署由用户执行；整改及提交过程未部署、未操作线上 Redis。

仓库示例 `configs/config.yaml` 已同步 `runtime.kubernetes.pod_cidrs` / `service_cidrs` 及网络白名单语义，默认两项留空，Redis 保持 standalone / best_effort / require_ha=false。补充示例配置回归，避免配置项再次遗漏。

普通 Pod 模板增加 kubelet 注入的 UID 防护 env，模板版本升为 v2，池兼容性会刷新。旧活动普通 Pod 没有此 env 时工作区控制 fail-closed；不要把新旧模板混用当作已验证迁移。按新版 Chart 正常执行 release drain/部署，不强制移除 hook 或安全证据。

新增 `test/integration/api/remediation_test.go` 是显式启用的部署后定向整改回归，不是 HA/LSM/全部 API 压测。需设置三个不同的、直达 API 副本的 base URL（不是同一负载均衡 URL）：

```sh
SANDBOX_API_TEST_URLS='http://127.0.0.1:18081,http://127.0.0.1:18082,http://127.0.0.1:18083' \
SANDBOX_API_TEST_FUSE=1 SANDBOX_API_FUSE_SAMPLES=30 \
go test ./test/integration/api -run TestDeployedAPIRemediation -count=1 -v -timeout=12m
```

API key 通过 `SANDBOX_API_TEST_KEY` 的现有安全环境注入。可另设 `SANDBOX_API_INTERNAL_TARGET`、`SANDBOX_API_INTERNAL_URL` 和 `SANDBOX_API_DENIED_URL` 验证选定内部目标可达、其他目标阻断。资源检查要求 Linux cgroup v2。套件只创建/清理自身 sandbox 与独有工作区，具有请求、总时长及响应体上界。

尚未完成现场验收：真实 Cilium 连通性及负例；30 次创建的 p50/p95/p99、完整错误率、池 UID 复用/开销和并发性能；滚动升级与 API/Redis 故障切换；真实 LSM enforcement。套件串行成功耗时仅用于初始观测，不能替代这些验收。未设置 deployed URL 时测试会 skip，不能记为线上通过。
