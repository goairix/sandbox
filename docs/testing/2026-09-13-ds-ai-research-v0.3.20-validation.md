# ds-ai-research v0.3.20 部署验证

## 范围与结论

2026-09-13 对 `ds-ai-research / aiadp-sandbox-fuse` 新部署进行三副本直连验证。本次网络整改的两个核心问题已经通过真实环境回归：实际 Cilium Pod 网段不会被 Node 的旧网段掩盖；普通池领取不再改变物理安全身份，Service 放行不再出现此前几十秒的身份切换等待。

这不是所有 CNI、生产 LSM 和高可用故障切换的验收。现场还有一条测试前已存在的孤儿 NetworkPolicy，旧普通池退休的超时清理路径仍需整改，不能写成整个 release 零遗留通过。

本次只创建、使用和正常销毁测试沙盒；未升级、回滚、重启、缩容部署，未强制删除 Pod，未修改业务 Redis，也未删除历史孤儿策略。

## 实际部署

- API 镜像：`registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.3.20`。
- 三副本实际镜像摘要一致：`sha256:496601923a87cc895c8cbc81d8b25b08f4c8b1643df79fa7c9536b9d10072d18`。
- 三个直连 Pod：`sandbox-fuse-api-744457dbb8-k7ln6`、`sandbox-fuse-api-744457dbb8-lt9qt`、`sandbox-fuse-api-744457dbb8-tnkj7`；分别使用本机 18081、18082、18083。
- `networkPolicyProvider=auto`，日志确认选择 Cilium；`podCIDRs` / `serviceCIDRs` 留空。
- Node 上报 `10.244.x.0/24`，CiliumNode 实际分配 `10.0.0.0/24` 至 `10.0.7.0/24`；ServiceCIDR 为 `10.96.0.0/16`。
- Redis 为 standalone / best_effort / requireHA=false；FUSE profile 为 minio-sigv4-path-style-v1。
- `workspace.allow_missing_lsm_for_kind=true`，本次不能验收生产 AppArmor/SELinux 强制执行。
- 实际 inventory ClusterRole 已包含 Calico IPPool list、CiliumNode list、Node list、ServiceCIDR list；runtime namespace Role 已包含 Cilium Endpoint get。当前集群没有 Calico IPPool API，不能据此声称 Calico 发现路径已在线验收。

## 已完成的功能回归

| 项目 | 结果与证据 |
| --- | --- |
| 三副本健康、就绪、认证 | health / ready 均 200；未认证访问受保护接口均 401 |
| 部署 API 整改套件 | `TestDeployedAPIRemediation` 通过，247.74 秒 |
| 跨副本执行及 stdin | 普通沙盒 Python / Node stdin、跨副本执行正确；一次性 Python stdin 三副本正确 |
| 工作区控制 | 跨副本动态挂载、双向同步、卸载及共享状态通过 |
| 资源限额 | 实际 cgroupv2 128 MiB、0.1 CPU 通过 |
| 文件与流式接口 | 缺失文件 404、空文件 200 空内容、普通/FUSE 扩展读写列举、分片合并/取消、TTL 更新、技能列表/不存在技能、已有沙盒 Exec 流及一次性 SSE 结束通过 |
| 扩展接口补测 | 普通/FUSE 补测 83.88 秒通过；对应两个原始 runtime UID 均已删除 |
| FUSE 文件系统压力 | 目录、覆盖、追加、截断、重命名、删除、Git、256 小文件、16 MiB 上传、flush、前缀互斥、销毁通过，81.51 秒 |
| 四种工作区持久化组合及切换 | ephemeral/persistent × sync/FUSE 全部通过，销毁后重新挂载读到 UUID 原内容；sync→FUSE、FUSE→sync 持有时均 409，销毁后可切换；该阶段 395.59 秒通过 |

## 核心修复的真实流量与身份验证

三个新普通池领取样本均复用已准备 Pod UID；领取前后的全部物理标签一致。`sandbox.claim.id` 与业务 ID 一致，`sandbox.claim.uid` 与原 Pod UID 一致。Cilium Endpoint 为 ready，身份标签始终使用物理池 ID；普通 NetworkPolicy UID 和 selector 在网络更新前后保持不变。

普通池物理标签 `sandbox.pool.state=preparing` 保持不变是稳定身份设计的一部分；prepared 发布由 `sandbox.pool.state=prepared` 注解与 `sandbox.pool.runtime.uid` 绑定。不能再仅凭旧标签统计空闲池；本次复用判断同时检查 UID、prepared 注解和未领取状态。

每个样本验证 API、Redis Service 的实际 TCP：放行两者、仅移除 Redis、恢复 Redis、关闭网络。移除目标之前已实际连接成功，避免把不可用后端误判为隔离成功。探测以单次 0.6 秒连接超时、总计约 5 秒的有界循环进行，不增加固定长等待。

- 放行/恢复后首个成功连接的进程内时间：7–54 毫秒；包含 API Exec 传输的请求约 0.26–0.94 秒。
- 移除 Redis 或关闭网络后的阻断探测：约 0.61–0.65 秒，主要为连接超时；不是精确的策略安装延迟测量。
- Network PUT 样本：0.172–1.814 秒。
- 三个样本分别从所有三个 API 副本提交实际 Pod IP、Pod CIDR、Service IP/CIDR、metadata/link-local 字面白名单，共 72 次，全部为 HTTP 400 / `NETWORK_TARGET_INVALID`。部署 Go 套件还验证了另外 18 次集群字面目标拒绝。

## 小样本性能与并发池复用

| 场景 | 样本结果 |
| --- | --- |
| 普通池三副本并发 | 3/3 复用请求前的预备 UID，三个 UID 不同；纯 HTTP 创建 0.182、0.189、0.213 秒 |
| FUSE 池三副本并发 | 3/3 复用请求前的预备 UID，三个 UID 不同；纯 HTTP 创建 3.157、6.372、10.052 秒 |
| Go 套件串行 FUSE 创建 | n=3，p50=5.439 秒，最大值=6.731 秒 |

FUSE 并发样本期间另有有界 FUSE 压力请求，存在共享准备/挂载路径的竞争；这不是纯热池独占性能基准。样本很小，不能承诺长期 p95/p99、持续负载容量或 Redis/API 故障切换 SLO。

持久化矩阵中的正常销毁：普通 sync 为 34.443 / 32.654 秒，ephemeral/FUSE 为 25.155 秒，persistent/FUSE 为 6.080 秒。均最终完成且可以重新领取相同前缀，但普通同步销毁和 FUSE finalization 仍有长尾；本次不把快速领取的改善解释为全部生命周期延迟已优化。

## 遗留清理问题

启动阶段三个 API 均出现 `obsolete ordinary pool retirement will retry / exact ordinary Pod deletion is unconfirmed: context deadline exceeded`。随后现场旧 Pod 已消失，后续采样未再出现该 ERROR；正常测试销毁等待期间仍会有 `cleanup already owned` WARN，不能直接当作泄漏。

测试前已经存在 `sandbox-sandbox-pool-udjrjjtses` NetworkPolicy：创建于 `2026-09-13T06:14:10Z`，绑定 runtime UID `361eddea-ad07-4731-8785-60969486021d`，对应 Pod 不存在。它比本次 v0.3.20 Pod 和测试都早，不能归为本次测试创建的残留，也不能据此判断具体是哪次历史中断产生的。

代码检查确认一个清理缺口：普通池退休使用 `fusePoolCleanupTimeout=5s`；精确 Pod 删除确认没有完成时，后续策略删除不会执行；再次退休遇到 Pod 已不存在，`cleanupRecord` 可能直接删除池记录而没有清理 UID 绑定策略。`RemoveOrdinarySandbox` 在指定 UID、Pod 不存在时也直接返回 NotFound。该机制可解释残留风险，但尚未新增中断窗口回归来证明这条历史策略的具体产生过程。

后续整改应保留精确 UID、同名替换保护和正常销毁确认，补齐 Pod 已消失后的归属策略收尾；不能用强删 Pod、直接删除 Redis 记录或弱化确认来伪装清理成功。

## 清理与验收限制

- 身份/网络/并发补测显式登记 9 个沙盒；9 个原始 runtime UID 均不存在，63 个精确生命周期/操作锁/会话键 EXISTS 总计为 0，相关 NetworkPolicy/CiliumNetworkPolicy 无残留。
- 扩展文件补测的两个原始 runtime UID 均不存在。
- 持久化/互斥/二进制阶段显式登记 13 个沙盒，13 个原始 runtime UID 均不存在，91 个精确键 EXISTS 总计为 0，相关策略无残留。
- 联合审计上述三组共 24 个显式登记沙盒、168 个精确生命周期/操作锁/会话键，remaining_keys=0；验证前缀的 workspace owner / lease 均为 0；当前 release 活动生命周期记录为 0。这不包含一次性执行与被拒绝申请内部 ID 的逐键登记。
- 最终现场只有三个 Ready、零重启 API、一个 Redis、三个普通 prepared Pod 和三个 FUSE prepared Pod；没有本次测试的活动/Terminating Pod 或 CiliumNetworkPolicy。预备 FUSE Pod 空闲时 kubectl 显示 1/2，本次挂载和压力执行均正常，未以空闲 Pod 的全部容器 Ready 作为可领取的唯一依据。
- 策略只剩基础策略、当前六个池策略以及前述一条历史孤儿普通策略；不能将其写为全 namespace 无孤儿。
- 最后两分钟 API 日志未匹配 ERROR、unconfirmed、failed to start 或 obsolete ordinary 退休错误。所有本机 port-forward 在验证后停止；未遗留测试业务沙盒。
- 故障矩阵未配置故障驱动，结果为 SKIP，不计为通过；未验收 Redis 故障切换、API 重启/网络分区、持续大规模负载。
- Calico 和双华云真实网络尚未接入，待用户准备测试环境；当前 Cilium 结果不能代表其它网络插件。

## 可复现命令

API 密钥通过已部署 Secret 注入测试环境变量，不写入报告或命令输出。三个地址分别直连不同 API Pod。

```sh
SANDBOX_API_TEST_URLS=http://127.0.0.1:18081,http://127.0.0.1:18082,http://127.0.0.1:18083 \
SANDBOX_API_TEST_FUSE=1 SANDBOX_API_FUSE_SAMPLES=3 \
SANDBOX_API_INTERNAL_TARGET=k8s-service://aiadp-sandbox-fuse/sandbox-fuse-api \
SANDBOX_API_INTERNAL_URL=http://10.96.14.111:8080/health \
SANDBOX_API_CLUSTER_LITERAL_TARGETS=10.0.5.196/32,10.0.3.166/32,10.0.7.25/32,10.96.14.111/32,10.0.3.0/24,10.96.0.0/16 \
go test ./test/integration/api -run '^TestDeployedAPIRemediation$' -v -count=1 -timeout=12m

WORKSPACE_FUSE_API_URL=http://127.0.0.1:18083 \
WORKSPACE_FUSE_SMALL_FILE_COUNT=256 WORKSPACE_FUSE_LARGE_UPLOAD_BYTES=16777216 \
go test ./test/integration/workspacefuse \
  -run '^TestWorkspaceFUSE/FUSE_filesystem_stress$' -v -count=1 -timeout=6m \
  -args -runtime kubernetes -profile minio-sigv4-path-style-v1
```

稳定标签/UID、真实网络时序、持久化矩阵、并发及精确 Redis 审计由本机临时驱动补测；运行结果逐项断言，失败会返回非零退出码。本次没有新增生产实现代码。
