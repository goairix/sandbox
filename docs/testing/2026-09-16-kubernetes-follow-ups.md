# Kubernetes 当前未完成事项（2026-09-16）

## 范围与最新基线

按用户要求，将剩余 FUSE 创建性能优化暂缓，后续另做专项。本次只更新记录、复核当前代码与
已有验收证据，并只读检查 `ds-ai-research / aiadp-sandbox-fuse`。没有修改运行逻辑、values、
部署或节点策略，没有新建测试沙盒、故障注入或删除线上资源；不检查分支差异。

2026-09-16 16:45–16:47（北京时间）复核：API `v0.3.31` 3/3 Ready，内置 Redis Sentinel
3/3 Ready，AppArmor Loader 5/5 Ready。普通池与 FUSE 池各有三个 prepared Pod；Redis 普通池
只有一个 scope 的三条 prepared 记录，FUSE 池复核为三条 prepared 记录。active record、active
controller、session v2、ephemeral lifecycle、workspace owner 和 workspace lease 扫描均为 0。
最近 10 分钟 API 日志采样没有匹配 ERROR、panic、termination unconfirmed、reconciliation
failed 或 lease lost。

本记录取代 [2026-09-13 后续清单](2026-09-13-kubernetes-follow-ups.md) 作为当前状态入口；旧记录
保留历史证据，不将已修复、已部署的事项继续计为当前故障。

## 暂缓的性能优化：PERF-02

FUSE 创建功能已通过，但热池创建 p95 不高于 3 秒的目标尚未达到：

- 第二阶段隔离热池 30 次：100% 成功，p50 1.238 秒、p95 3.770 秒、p99/max 3.791 秒；
- 最新 API `v0.3.31` 功能回归的一个 FUSE 样本：创建 6.625 秒，写入、flush、三副本共享状态
  和销毁均通过。单样本只说明仍有慢请求，不用它估算长期 p95；
- 已移除 Kubernetes PodReady 的串行等待和 mounter readiness 中重复的远端 `/bin/ls`；
- 剩余耗时包含领取/租约、前缀 marker PUT+HEAD、绑定/授权、网络收敛、s3fs 真实挂载，以及
  sandbox 侧强传播探针与状态发布。外部 ready 时间线不能当作纯挂载耗时。

后续先取得固定阶段 `sandbox.workspace.stage.duration` 的线上分布，结合追踪或有界驱动保留请求时间线，区分
`pool_acquire`、`lease_acquire`、`prefix_prepare`、授权/网络、`mounter_ready_wait`、
`propagation_probe` 和 publish，再确定代码优化点。库存充足/耗尽、隔离/并发、正常/慢后端
分别基准。不得为了提速删除 fsync、读回、删除补偿、exact UID/generation、租约、网络收敛或
FUSE flush/unmount 门槛。

阶段 histogram 不携带 sandbox ID，不能单靠聚合分位数还原某个请求。Manager 的 `readiness`
包含 runtime 的 `mounter_ready_wait` 与 `propagation_probe`，不能把父子阶段直接相加；实际挂载
从授权后启动，并可能与网络更新重叠，读阶段指标时也必须保留这个边界。

证据见 [FUSE 延迟整改及分段观测](2026-09-16-kubernetes-fuse-readiness-latency-remediation.md)
和 [v0.3.31 线上验收](2026-09-16-kubernetes-ordinary-destroy-remediation.md)。

## 其它未完成事项

| 编号 | 当前状态及证据 | 后续关闭条件 |
| --- | --- | --- |
| QUALITY-01 | 已确认代码质量欠账：本次全仓 golangci-lint 退出 1，共 81 项：errcheck 50、ineffassign 2、staticcheck 20、unused 9。并非 81 个已复现的线上故障，也不全是排版建议。 | 按类别复核并整改，保留错误传播、资源释放和安全状态机语义，补对应回归，最后全仓 lint 通过。增量 lint 通过不能替代。 |
| HA/FAULT-01 | 内置 Sentinel 已部署，quorum/复制及正常 API 回归通过；本地真实三成员 fixture 已覆盖多类故障，45 秒 refill lease 接管已在部署集群验证。但没有完成目标 Kubernetes/CSI 上的实际 Redis 切主、API/节点故障、网络分区、全组保留卷冷恢复，以及旧池跨代退休预算中断的完整现场故障矩阵。 | 在明确授权的隔离窗口执行有界故障驱动，验证唯一 writer、ACK 不确定时 fail closed、失效 owner 接管、精确 UID/策略收尾、身份保留和服务恢复；同时完成备份恢复演练。不能以 Ready 或 WAIT 1 代表零数据丢失。 |
| NET-01 | 可移植网络发现/策略代码及当前 Cilium Service 放行、撤销、私网/metadata 拒绝已有通过记录；Calico 和双华云/VPC 的真实环境仍未完成验收。 | 两个双华云测试环境就绪后，验证实际 Pod/Service 分配范围、白名单撤销/恢复、metadata、跨 namespace、双栈及云网络策略执行；在各环境验证实际对象存储后端挂载/flush/恢复，不能用 Cilium/MinIO 结果替代。 |
| SEC-01 | AppArmor Loader 五节点 Ready，生产缺失 LSM 豁免为 false；worker-2 组件级挂载/拒绝/重载测试，以及线上已有节点的 API FUSE 链路通过。未完成五个节点各自的端到端 API 挂载和完整拒绝规则覆盖，也未覆盖其它内核、运行时和存储组合。 | 在目标调度范围逐节点验证 mounter 与 s3fs 子进程 exact enforce profile、真实读写/flush/unmount、越权负例和重建恢复。节点加载成功不等于该节点完整业务验收。 |
| OBS-01 | tracer/metrics/log OTLP 当前均已启用，不再属于“exporter 未配置”。但本轮没有从 collector/后端取得固定阶段分布和可关联的请求追踪，也未验证遥测丢失率与开销。 | 核对 collector 实际接收与存储，取得慢请求的固定阶段证据，检查低基数、脱敏、采样及开销；作为后续 PERF-02 专项的输入。 |
| LOAD/EDGE-01 | 当前已通过有界功能矩阵、256 小文件/16 MiB 压力，以及普通/FUSE 的多副本回归；尚无持续大负载的吞吐/错误率/p95/p99 SLO，也没有完整外部 Ingress/TLS 链路和对象存储全前缀审计。 | 在容量可控环境验证持续并发、池耗尽与补池、慢后端、FUSE 创建及 flush/unmount 长尾、资源开销和最终清理；补外部认证/Ingress/TLS 测试及精确测试前缀对象审计。不能把单次健康结果或目录标记当作全局零遗留证明。 |

QUALITY-01 可先做本地整改；HA/FAULT-01 需要单独的隔离环境或明确故障窗口，不在本次只读复核中
直接执行。PERF-02 按本次用户要求暂缓。

## 已关闭或不再作为当前故障的事项

- 普通沙盒约 32 秒销毁：最短非零优雅期与 exact UID 终止确认已部署；10/10 热池样本成功，
  p50 3.415 秒、p95/max 3.891 秒，达到 5 秒验收线。原有 30 秒 Pod spec 无需主动重建。
- 普通多副本并发 DELETE 的立即 503：最新三轮 ephemeral/persistent 共 18/18 DELETE 成功，
  所有副本随后 GET 404；共享普通池、正常策略收尾及工作区生命周期已有回归。
- 普通池 CAS 竞争、工作区并发销毁重复计数、上传助手错误 ACK：已有实现和本地回归；后续
  正常部署 API/工作区矩阵通过，不继续列作未实施修复。遥测仍保留原进程级 best-effort 语义。
- Redis API 镜像耦合滚动：helper 已拆为独立镜像。本次执行升级比较器，对
  `/tmp/sandbox-fuse-sentinel-v0.3.29.json` 的 revision 10 后置基线与当前 revision 13 比较为
  PASS：Redis revision、PodTemplate、Pod UID/restartCount/imageID 均未变，未新增 Sentinel
  一次性 Job。该区间也发生过 mounter 升级，因此这里只证明这段升级区间 Redis 元数据稳定，
  不将比较器结果扩展成故障期间零中断证明。
- identity/initialize Job 每次升级累积：Initialized 后不再创建，一次性资源生命周期已有
  Chart 状态矩阵与现场记录。45 秒 refill lease 到期接管也已有真实部署验证。
- 旧清单的 standalone 单点、生产缺失 LSM 豁免、OTLP disabled：当前分别为内置三成员
  Sentinel、`allow_missing_lsm_for_kind=false` 和三个 OTLP exporter enabled，不再保留旧结论。
- 历史 workspace owner/其它普通 pool scope：当前只读扫描 owner/lease 为 0，普通池只有当前
  一个 scope 的三条记录。旧记录不再是本次现场存在的问题；本次未删除状态，也不推断它们
  具体由哪次清理或重装消失。正常 pool/generation fencing 状态不应按残留删除。

## 本次检查命令

```text
golangci-lint run --timeout=3m --output.text.path=stdout --output.text.print-issued-lines=false --output.text.print-linter-name=true
bash scripts/verify-sentinel-api-only-upgrade.sh verify --context ds-ai-research --namespace aiadp-sandbox-fuse --release sandbox-fuse --snapshot /tmp/sandbox-fuse-sentinel-v0.3.29.json
```

前者为 81 issues、exit 1，后者 PASS、exit 0。其它检查为 Kubernetes 元数据/非敏感 env 和
Redis SCAN/只输出白名单状态字段的 GET；凭据和 token 不打印，不使用 DEL 或 KEYS。本次没有
重新运行沙盒功能、故障或容量套件；功能数字引用已提交的线上验收记录。
