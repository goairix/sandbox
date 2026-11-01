# DS 集群 etcd 请求协调优化实测

> 后续交付修订：用户确认尚未上生产、按全新安装交付，要求去除新增配置。当前代码在 Kubernetes + etcd 上自动使用共享协调，不再保留开关。本文记录的是 `76fe3252` 实测时的配置与结果，保留开关对照作为历史证据，不是当前安装要求；本轮没有重新部署后续默认行为修订。

日期：2026-11-01。代码：`76fe3252`，分支 `feat/etcd-state-management`。

## 环境与测试范围

- 集群：`ds-ai-research`；命名空间：`sandbox-fuse-etcd`（UID `f11d4019-3bf1-4ef8-aeb1-8f580fd94db8`）。
- 现有 Helm release `sandbox` 保留原配置、两个 API 副本及原镜像，没有执行 Helm upgrade。
- 新增独立测试 Deployment/Service/NetworkPolicy `sandbox-coord-20261101`，两个 API 副本，共用该命名空间已有的 state scope、三成员 etcd、工作空间存储和运行时池。
- 测试 API 镜像：`registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-api:test-coord-76fe3252-arm64-20261101`。从干净提交构建 Linux arm64 二进制；使用明确 tag，单架构 manifest，构建关闭 provenance/SBOM 索引。镜像内附临时指标收集程序，不作为生产发布镜像。
- 原 API 镜像：`registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-api:v2-stream-files-20e060b3-arm64`。
- etcd：3 个 Ready 成员，固定 `v3.6.14-arm64` 镜像，使用原有持久化 PVC。工作空间使用原配置的外部 MinIO，FUSE 运行时与 AppArmor loader 保持原版本。
- 仅测试 Deployment 切换 `SANDBOX_STORAGE_STATE_SHARED_DATA_OPERATIONS=false/true`；原 release 开关与镜像保持原状。临时 API 的两个池最小容量设为 0，原 API 继续维护已有池。
- Go SDK v2 客户端分别连接两个测试 Pod；旧 API 参与兼容性读验证。所有工作空间使用独立的 `validation/etcd-coord-20261101/<phase>-<timestamp>` 路径。

## 文件接口与协调成本

开关关闭、开启两组均通过：跨 API 副本创建同一工作空间复用同一 sandbox；v2 Write、Read/Close、Stat、Grep 内容校验；旧 API 读取新 API 写入的数据；删除后各 API 查询返回 404。

每组先完成文件准备与预热，再在不持有 controller ownership 的另一测试副本执行 **201 次串行 List `/workspace`**。通过 Pod 内 OTLP 接收器采集协调 stage 计数，取批次前后完整导出差值。

| 指标 | 关闭共享协调 | 开启共享协调 |
| --- | ---: | ---: |
| 成功 List 请求 | 201 | 201 |
| 协调 RPC 合计 | 1206 | 207 |
| RPC / 请求（含窗口内后台维护） | 6.000 | 1.030 |
| lease Grant | 201 | 0 |
| KV 写事务 | 402 | 0 |
| p50 / p95 / p99（ms） | 144.828 / 229.959 / 252.053 | 118.334 / 190.217 / 220.222 |

关闭时每请求为 open/read + grant + commit，以及 end/read 两次 + commit。开启时为 201 次 `read/read`，另有 3 次 `renew/read` 和 3 次 `renew/keepalive`。本批次在共享组已建立后测量，不包含首次建组与最终回收成本；这两项仍然需要写入。

该实测证明热请求的 etcd 写压力下降。样本包含真实 Kubernetes GET、SPDY exec 和 FUSE 操作，但它是单热点、小目录、串行短批次，不能推导集群最大 QPS、冷目录性能或业务延迟 SLA。两个批次之间发生测试 API rollout，节点与缓存差异也可能影响延迟。

## 真实运行时故障验证

每个流测试均创建 32 MiB 长度的上传，只发送 1 KiB 后保持输入打开；在 runtime Pod `/proc` 中核对实际 `sandbox-file-op write /workspace/cancelled.bin` PID，并绑定 runtime UID，避免把本地 HTTP 连接关闭当作远端 helper 已退出。

| 场景 | 结果 |
| --- | --- |
| 上传取消与并发 Destroy | Destroy 在流持有期间不能完成；取消上传后实际 helper 约 4.130 秒内退出，之后销毁成功，查询返回 404。 |
| 测试 API 进程崩溃 | 对单个临时 API 的 PID 1 发 SIGABRT，确认容器 restartCount 增加；实际 helper 约 3.430 秒内退出。旧共享 operation key 随 lease 到期消失，另一 API 仍能找到同一 runtime 并写文件，重启 API 恢复 Ready。 |
| 单个测试 API 到 etcd 断连 | 临时 NetworkPolicy 仅选中指定测试 Pod，允许 DNS、443/6443，阻断 2379。确认到 3 个 etcd 成员的新连接均失败后，List 返回 HTTP 503 / `SANDBOX_NOT_READY`；实际上传 helper 约 22.031 秒内退出，旧 operation key 到期消失。另一 API 可继续读状态、写文件；删除策略后故障 API 恢复 Ready。 |
| etcd follower 重建 | 查询当前 leader 后选择 follower `etcd-2`，正常删除 Pod，由 StatefulSet 用原 PVC 重建；约 15.354 秒恢复，期间 20 次 List、0 失败。Pod UID 改变，3 个 member ID 保持一致。 |

测试方法修正及未计为通过的首次尝试：

- 首次流取消测试的 helper 退出检查失败，已保留原始结果；因检查器可能与 Pod 删除并发，未获取足够证据确定原因。独立复测实际观察到 helper 消失并完成清理。不将首次失败隐藏为通过。
- 首次 API 崩溃尝试使用容器内 `kill -9 1`，容器并未重启，故该次没有形成有效故障；改为 SIGABRT 并观察实际 restartCount 增加后才计入结果。
- 首次 NetworkPolicy 应用后立即发请求，没有先确认策略生效，未得到预期拒绝。复测先检查 3 个 etcd 端口的新连接均失败，再验证 503 和远端退出；首轮不作为隔离证明。

上述结果覆盖当前运行时版本的阻塞文件上传、调用方取消、API 进程退出与 API→etcd 选择性断连。**尚未验证整个节点与 Kubernetes API 同时失联、任意远端命令持续产生副作用、旧 runtime UID 替换等全部隔离场景**，因此不能将本报告视为全面生产开启批准。全局默认开关继续关闭。

## 清理与证据

最终校验通过：

- 原 `sandbox-api` Deployment 的完整 spec 与 UID 等于测试前快照，2 个副本 Ready/Available；未改原 API 镜像或开关。
- etcd 3 个 Pod Ready，3 个 client endpoint 健康；follower 重建前后的 3 个 member ID 一致。
- 3 个 etcd PVC 与 1 个 MinIO PVC 的 UID、绑定 PV 和 Bound 状态保持一致。
- 9 个独立测试 sandbox 均经 API 销毁并校验 404，对应 runtime Pod、active record/operation 均已不存在。
- 只删除这 9 个确切工作空间前缀下的 11 个当前对象，复查当前对象数为 0；没有扫描删除业务工作空间或整个存储前缀，也未手工删除 etcd owner/operation key。
- 临时 Deployment、Service、NetworkPolicy、ReplicaSet、Pod 均已移除，故障策略与标签未遗留，端口转发进程已退出。
- 测试镜像 tag 与本地私有原始记录保留，便于追溯；没有发布正式版本或升级原 Helm release。

代码行为未在本轮修改；新增本报告并补充本地验收记录，执行 `git diff --check` 通过。

原始指标、逐阶段结果、私有基线 Deployment 与一次性驱动位于仓库忽略目录 `var/tmp/k8s-test/etcd-coordination-20261101-76fe3252/`。该目录包含环境配置，禁止整体提交或公开；文档只记录脱敏结论。认证从已有 Secret 在内存中读取，未写入文档、构建上下文或镜像。
