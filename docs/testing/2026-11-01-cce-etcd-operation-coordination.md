# CCE 集群 etcd 请求协调优化实测

> 后续交付修订：用户确认尚未上生产、按全新安装交付，要求去除新增配置。当前代码在 Kubernetes + etcd 上自动使用共享协调，不再保留开关。本文记录的是 `76fe3252` 实测时的配置与结果，保留开关对照作为历史证据，不是当前安装要求；本轮没有重新部署后续默认行为修订。

日期：2026-11-01；API 代码：`76fe3252`。本轮只新增独立测试资源，未升级原 Helm release。

## 环境与镜像

- 使用仓库当前目录 `var/tmp/cce/cce-sandbox-fuse-v2-kubeconfig.yaml`，每次命令明确指定 `--context internal` 和命名空间 `sandbox-fuse-etcd`。文件内的 current-context 名称不匹配实际 context，不能直接依赖默认值；旧配置路径的内部地址不可达。
- 命名空间 UID：`e17f1d6b-426a-4f16-bd2f-49ae87b497f4`。
- 原 `sandbox-api` 两副本，镜像 `registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-api:v1-paged-owner-18dd9e90-arm64`。原 AppArmor loader、mounter、Secret、网络配置均沿用。
- 实际 state endpoints 是 `cce-pvc-etcd-0/1/2`，etcd `v3.6.14-arm64`，每成员各使用一块 Bound `csi-disk` PVC，前缀 `/sandbox/cce-pvc-20261026/`、scope `sandbox-fuse-etcd/sandbox`。本轮未操作另行保留的旧 `cce-test-etcd` emptyDir 集群。
- 对象存储是命名空间内的 `cce-test-minio:9000`，bucket `cce-test-bucket`、subpath `workspaces`。本轮没有重建 MinIO，不构成 MinIO 持久性验收。
- 临时 Deployment/Service/NetworkPolicy `sandbox-coord-20261101`，两个 API 副本，共用现有状态及存储后端；指标收集 sidecar 只在 Pod 内接收 OTLP。
- 测试 API 镜像固定为 `registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-api:test-coord-76fe3252-arm64-20261101`，复用 DS 已从干净 `76fe3252` 构建的 Linux arm64 镜像。
- 原 CCE 运行时没有可执行的 `sandbox-file-op`，因此仅测试 API 使用 `registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-runtime:v0.4.0-test-coord-76fe3252-cce-arm64-20261101`。该 tag 指向 DS 已验证的 v2 运行时内容，未重新构建运行时，不能将 tag 中的协调代码编号当作运行时源码版本证明。此镜像对应独立池版本，FUSE 最小准备数 1，普通池最小数 0；原 API 的池配置不变。
- 测试 API 和 runtime 的镜像引用均使用完整 tag。registry raw manifest 检查为单架构 Docker manifest，没有额外索引。测试前置准备首次使用非语义化 runtime tag，被 FUSE 固定镜像校验拒绝，临时 API CrashLoop；保留失败日志，改成上述预发布版本 tag 后启动成功，没有放宽校验。

## 功能与成本对照

两组均通过 Go SDK 跨副本工作空间复用、v2 Write/Read/Close/Stat/Grep、原 API 通过旧文件接口读取新 API 写入的内容，以及删除后所有入口返回 404。

对照使用相同测试 API/运行时，仅切换临时 Deployment 的 `SANDBOX_STORAGE_STATE_SHARED_DATA_OPERATIONS`。每组先准备文件和预热共享组，再由不持有该 sandbox controller ownership 的另一副本串行执行 201 次 List `/workspace`。计数取完整 OTLP 导出窗口前后差值。

| 指标 | 关闭共享协调 | 开启共享协调 |
| --- | ---: | ---: |
| 成功 List | 201 | 201 |
| 协调 RPC | 1206 | 207 |
| RPC / 请求（包含窗口内后台维护） | 6.000 | 1.030 |
| lease Grant | 201 | 0 |
| KV 写事务 | 402 | 0 |
| p50 / p95 / p99（ms） | 102.199 / 173.851 / 196.589 | 91.553 / 162.504 / 178.836 |

关闭时每请求 6 次协调 RPC、2 次 KV 写事务。开启时为 201 次只读校验、3 次后台续租前读取与 3 次 KeepAlive。共享组首次创建和最终释放不在该窗口内，仍有写成本；本表也没有包含准备池、状态扫描等其它模块的全部 etcd 流量。

这是实际 Kubernetes GET、SPDY exec 与 FUSE 路径上的单热点、小目录、短批次测试，不能据此推导持续吞吐、容量上限、首次冷目录耗时或业务 SLA。两组之间发生临时 API rollout，延迟可能受节点及缓存差异影响。

## 故障与恢复

阻塞上传测试声明 32 MiB，只发送 1 KiB 后保持输入打开。通过 runtime Pod `/proc` 核对实际 `sandbox-file-op write /workspace/cancelled.bin`，绑定 runtime UID，验证远端 helper 消失，而非仅检查客户端请求取消。

| 场景 | 结果 |
| --- | --- |
| 上传取消与并发 Destroy | 流仍持有时 Destroy 未完成；客户端取消后 helper 约 2.903 秒内退出，之后销毁及 404 校验通过。 |
| 临时 API 崩溃 | 仅对临时 API 的 PID 1 发 SIGABRT，确认容器重启；helper 约 2.345 秒内退出。旧 operation key 到期消失，另一副本仍能找到原 runtime 并写文件，重启 API 恢复 Ready。 |
| 临时 API→etcd 断连 | 选择性 NetworkPolicy 在约 38.404 秒完成生效确认，3 个 etcd 新连接失败而 Kubernetes API/MinIO 仍可连接。新 List 返回 503 / `SANDBOX_NOT_READY`；确认断连后约 0.906 秒内观察到上传 helper 已退出，从策略应用计总计约 39.309 秒。旧 operation key 到期消失，原 runtime 状态保留，另一副本可继续写文件；删除策略后故障 API 恢复 Ready，沙箱销毁与 404 校验通过。 |
| PVC etcd follower 重建 | 读取 leader 后选择 follower `cce-pvc-etcd-2`，正常删除 Pod，由 StatefulSet 使用原磁盘 PVC 重建；约 21.412 秒恢复，期间 37 次 List、0 失败。Pod UID 改变，3 个 member ID 一致。 |

故障注入前置检查保留了两次未通过的记录：首次策略遗漏 CCE 集群内 Kubernetes API 的实际 5444 端口，`partition_preserve_kubernetes_access` 未通过；策略撤销后 API 恢复 Ready，该沙箱经独立清理确认各入口 404、runtime 已删除。第二次补上端口后，20 秒内未观察到 etcd 端口被阻断，故未计为断连测试通过。随后独立诊断验证：selector 与 Pod 标签匹配、admission 未改写策略，在策略应用约 31.26 秒时，3 个 etcd 新连接均失败，Kubernetes API 与 MinIO 新连接仍成功；撤销后全部恢复。这说明前述第二次测试的策略等待上限不足，本轮测试器将上限调整为 60 秒，并在验证 TCP 断连后才开始评价故障行为。没有修改集群 CNI 或协调实现。

全节点分区、同时失去 Kubernetes API、任意远端命令的持续副作用，以及大存量和持续高并发仍未覆盖。原部署开关保持原状，仓库默认仍为关闭；本报告不等于全面生产开启批准。

## 清理与证据

最终清理校验通过：

- 原 `sandbox-api` Deployment 的完整 spec 与 UID 和测试前快照一致，2 个副本 Ready/Available；原镜像、开关和池配置保持原状。
- 原 `cce-pvc-etcd` StatefulSet 的 spec 与 UID 未变，3 个 Pod Ready、3 个 client endpoint 健康；重建前后 member ID 一致。
- 3 块 `csi-disk` PVC 的 UID、绑定 PV 和 Bound 状态与测试前一致。
- 8 个独立测试 sandbox 均完成公开销毁、各 API 返回 404；对应 active record/operation 与 runtime Pod 均已不存在。首轮隔离测试清理曾报告客户端失败，随后独立重新检查/清理通过，原始失败记录保留。
- 只删除这 8 个确切工作空间前缀下的 9 个当前对象，复查各前缀当前对象数为 0。没有清理业务工作空间、整个 bucket 或手工删除 etcd owner/operation key。
- 临时 Deployment、Service、ReplicaSet、API Pod、故障与诊断 NetworkPolicy 均已删除；独立版本测试池的准备运行时及其网络策略也已回收，端口转发进程已退出。
- 测试镜像 tag 和本地私有证据保留，便于追溯；未发布正式版本、未升级原 Helm release、未修改产品代码。

本报告及本地验收报告补充执行 `git diff --check`，通过。

原始逐阶段结果、指标、私有基线配置和一次性驱动保存于忽略目录 `var/tmp/cce/etcd-coordination-20261101-76fe3252/`。认证从 Secret 在内存中读取，构建上下文、镜像和本文不包含密钥；不要整体提交或公开该目录。
