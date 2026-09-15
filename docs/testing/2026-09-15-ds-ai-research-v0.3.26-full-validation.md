# ds-ai-research v0.3.26 全量线上验证

## 结论与范围

2026-09-15 10:12–10:29（北京时间），在 `ds-ai-research` 集群、
`aiadp-sandbox-fuse` namespace、`sandbox-fuse` release 上验证用户更新后的
`sandbox-api:v0.3.26`。

**本轮约定的线上功能回归全部通过。** 三个 API 副本分别直连参与测试；普通沙盒
跨副本并发销毁连续三轮通过，综合 API/FUSE/网络回归通过，完整 workspace sync/FUSE
模式矩阵及有界文件压力通过。收尾时 API 3/3、Redis Sentinel 3/3、AppArmor Loader
5/5 Ready，普通池 3 个和 FUSE 池 3 个均为 prepared；没有活动测试 session、
ephemeral 生命周期、workspace lease/owner 或已销毁沙盒的托管资源遗留。

本轮只通过 API 创建、操作并销毁隔离测试沙盒，并进行集群与 Redis 只读检查。没有
执行 Helm upgrade/uninstall/rollback、扩缩容、Redis 切主、节点重启或网络分区，也
没有删除 Pod、NetworkPolicy、Redis key、PVC、Secret 或对象存储前缀。测试使用三个
Pod 直连 port-forward，不等同于外部 Ingress/TLS 链路验收。

## 部署身份与健康

- Helm revision 6，状态 `deployed`，部署时间为 2026-09-15 09:39:47（北京时间）。
- API Deployment 3/3 Ready、updated 和 available 均为 3；镜像为
  `sandbox-api:v0.3.26`，三个副本实际 imageID 均为
  `sha256:8ea94215d0e24235628b19ef24e4d38af447ad8599ee7ba3a64ee7248d414a52`。
- Redis Sentinel StatefulSet 3/3 Ready、current/updated 均为 3。三个 Sentinel 一致
  认定 ordinal 1 为 master；Redis 实际角色为 ordinal 1 master、ordinal 0/2 slave。
- AppArmor Loader DaemonSet 5/5 Ready、available/updated 均为 5。
- 三个 API 副本的 `/health`、`/ready` 均返回 HTTP 200。测试时段没有新的 API 容器
  重启，也未检索到 API `ERROR`、panic、fatal 或 `failed to` 日志。
- 最早启动的 API Pod 累计 5 次历史重启，均发生在本轮功能测试之前的升级启动阶段；
  原因及精确处置见 [API 升级触发内置 Redis Sentinel 重启记录](2026-09-15-sentinel-api-upgrade-restart-incident.md)。

API key 只从 Kubernetes Secret 读取到测试进程环境变量，未写入命令输出或本文。

## 已执行的回归

三个直接副本分别映射到独立的本地端口，`SANDBOX_API_TEST_URLS` 包含全部三个地址。

```sh
go test ./test/integration/api \
  -run '^TestDeployedOrdinaryConcurrentDestroy$' \
  -v -count=3 -timeout=10m

SANDBOX_API_TEST_FUSE=1 \
SANDBOX_API_FUSE_SAMPLES=3 \
go test ./test/integration/api \
  -run '^TestDeployedAPIRemediation$' \
  -v -count=1 -timeout=12m

WORKSPACE_FUSE_SMALL_FILE_COUNT=256 \
WORKSPACE_FUSE_LARGE_UPLOAD_BYTES=16777216 \
go test ./test/integration/workspacefuse \
  -run '^TestWorkspaceFUSE$' \
  -v -count=1 -timeout=20m \
  -args -runtime kubernetes -profile minio-sigv4-path-style-v1
```

| 覆盖项 | 结果 |
| --- | --- |
| 普通跨副本并发销毁 | PASS，包耗时 205.942 秒；连续 3 轮，每轮包含 ephemeral/persistent，每个沙盒由三个 API 副本同时 DELETE，共 18 次成功请求 |
| 综合 API 整改套件 | PASS，用例 288.50 秒、包 289.793 秒 |
| 跨副本执行与文件 | Python/Node stdin、文件上传下载、空文件、缺失文件、跨副本状态一致性通过 |
| 动态 sync、资源与 SSE | sync 挂载/同步/恢复/卸载、100m CPU/128 MiB 内存、一次性 SSE 正常结束均通过 |
| 网络控制 | 指定 Service 放行，撤销/恢复，以及集群 CIDR、Service 地址和 metadata 地址拒绝均通过 |
| FUSE 生命周期 | 3 个样本的创建、写入、flush、跨副本状态、拒绝动态卸载和销毁通过；创建 p50 11.185 秒、p95/p99 11.775 秒 |
| 完整 workspace 模式矩阵 | PASS，用例 418.21 秒、包 419.531 秒，8 个子场景全部通过 |
| sync 持久化 | ephemeral 销毁前落盘、persistent 手动同步和重建恢复通过 |
| FUSE 持久化 | ephemeral finalization、persistent durable flush 和重建恢复通过 |
| workspace 并发规则 | 同一前缀 sync/FUSE 双向冲突拒绝、不同前缀跨模式并发通过 |
| FUSE 文件压力 | PASS，73.92 秒；256 个小文件、16 MiB 大文件及目录、覆盖/追加/截断/重命名/删除、Git、真实 fuse.s3fs、租约、flush 和销毁通过 |

综合套件显式使用集群 Service
`k8s-service://aiadp-sandbox-fuse/sandbox-fuse-api`，可达控制地址为
`http://10.96.209.56:8080/health`；字面拒绝目标覆盖 Pod CIDR、Service CIDR、当前
Service 地址和 `169.254.169.254`。FUSE 延迟只有三个成功样本，仅作为本次回归观测，
不据此宣称生产 p95/p99 或并发吞吐。

## 池、状态与策略收尾

最终只剩 6 个 `sandbox.managed=true` Pod 和与其一一对应的 6 份 managed
NetworkPolicy：普通池 3 个、FUSE 池 3 个。没有 managed CiliumNetworkPolicy，也
没有 deletionTimestamp 中的托管 Pod。

普通池 Pod 的物理 CNI 标签仍显示 `sandbox.pool.state=preparing` 是当前安全设计：该
标签参与不可变网络身份，不能在准备完成后通用修改。权威业务状态由 UID 绑定的
annotation 发布。本轮三个普通池 Pod 均满足：

- `sandbox.pool.state` annotation 为 `prepared`；
- `sandbox.pool.runtime.uid` 与实际 Pod UID 完全一致；
- Pod Running、Ready 且无 deletionTimestamp。

三个 FUSE 池 Pod 的受保护 label 均为 `prepared`。最终 Redis 扫描结果为：

| 状态前缀 | 数量 |
| --- | ---: |
| `sandbox:session:v2:*` | 0 |
| `sandbox:ephemeral:v1:*` | 0 |
| `sandbox:workspace:lease:*` | 0 |
| `sandbox:workspace:owner:*` | 0 |

这四类为活动业务状态；普通/FUSE 池记录及 workspace generation fencing 状态属于
正常运行状态，不应按测试残留删除。

## AppArmor 实际状态

从当前普通池和 FUSE 池 Pod 内读取 `/proc/1/attr/current`：普通与 FUSE 的 sandbox
业务容器均处于 `cri-containerd.apparmor.d (enforce)`；FUSE mounter 使用精确加载的
专用 profile：

```text
sandbox-fuse-37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d (enforce)
```

三个当前 FUSE mounter 日志未检索到 error、panic 或 fatal。本轮没有强制把新业务
沙盒逐个调度到五个节点，Loader 5/5 Ready 与既有五节点加载验证不应被解释为本轮
又完成了一次五节点端到端挂载测试。

## 验证边界

- `TestWorkspaceFUSEFaultMatrix` 需要 chaos driver，本轮未执行；不宣称 Redis failover、
  API/节点故障、网络分区和运行中断恢复已通过。
- 未做对象存储全前缀零对象审计，不宣称所有历史目录标记或异常上传临时对象不存在。
- 旧 revision 和当前 revision 的 Redis identity/initialize Job 已成功完成，属于带
  24 小时 TTL 的部署审计资源，不是沙盒测试遗留，本轮未删除。
- 三个测试专用 port-forward 已精确停止，端口均确认不再监听。
