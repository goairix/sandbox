# AppArmor 与 Sentinel 本轮实现复审

## 交付边界

这是可选 AppArmor 加载器及 Sentinel 认证/安全基础的代码批次，**不是两项生产功能的完整验收**。当前分支原地实施，不创建 worktree，不构建或推送发布镜像，不变更业务集群、release、宿主机 profile 或业务 PVC。

| 项目 | 当前结果 | 尚缺验收或实现 |
| --- | --- | --- |
| AppArmor 核心、CLI、Dockerfile、Chart | 已接入，默认关闭 | 发布者构建镜像；真实内核策略加载及完整挂载负例 |
| API 启动门禁、共享节点选择器、私有 enforce 检查 | 确定性及 client-fake 回归通过 | 目标集群实际覆盖、privileged mounter/子进程 enforce、性能采样 |
| 外部 Sentinel 独立认证 | 配置、API/drain、真实认证通过 | 不能据此代替 Kubernetes HA 验收 |
| 内置 Sentinel 安全基础 | 严格身份、保守恢复决策、不可覆盖标记、配置转义及 MAC 原语通过 | 可信远端身份获取、注册/防重放协议、完整启动协调器 |
| 内置 Sentinel Chart | **未完成，未开放可部署的 Sentinel 开关** | 三节点拓扑、初始化事务/Job、API phase gate、真实冷恢复与状态丢失矩阵 |

因此暂不要为这两项功能卸载生产重装。内置 Redis 仍沿用 standalone；不能将外部 Sentinel 支持写成 Chart 已搭建 HA 集群。

## 复审整改

- 策略名称使用完整 SHA-256；Kubernetes label 限长 63，完整摘要另存 annotation，并同时核验。修正原先 64 位 label 无法创建的错误。
- 实际 flush 使用 `/bin/sync`，策略补齐 `/bin/sync` 与 usr-merged 的 `/usr/bin/sync` 继承执行，不开放任意执行。
- 策略扫描修正 quoted `#`/转义边界，拒绝非 ix 执行模式、子 profile、外部 include 和 ABI 元数据，包含匹配摘要的回归，避免只因摘要正确就接受非自包含策略。
- Pod 门禁比较当前完整 defaulted 模板，只归一化固定 DaemonSet/admission 变更；旧 Ready Pod 的探针、安全字段、卷及 token 漂移均拒绝。
- 显式 `apparmorLoader.priorityClassName` 支持同名可信类的 admission priority/preemption 值；有 globalDefault 的集群必须填写既有类。未自动创建或授予 system-critical 优先级，空配置不宽泛信任注入类。
- YAML 标签保留大小写，结构键大小写兼容；无法安全复原的结构 merge 明确拒绝。有效节点选择器最多 64 项，启动超时 1..600 秒；Chart 同步类型与边界校验。
- FUSE 私有检查只读固定 PID 1 属性，读取前后复核 exact UID、零重启、固定安全模板及卷源，共享 5 秒预算；失败不构造授权凭据，不向公共租户 Exec 开放 mounter。
- Sentinel 数据端与管理端凭据独立，不回退替用；API 和两个 drain 路径 env 唯一，原 installed Deployment 的实际 env 保留。
- 固定 go-redis v9.18.0 按 masterName/地址数量推断客户端类型，原显式 Cluster 单 seed 被错误构造为普通 Client。增加 IsClusterMode，并在 Store/native config/Helm 一致拒绝非 Sentinel 残留 masterName。未改变 ACK/CAS/fencing；这不是实际 Redis Cluster 故障验收。
- 保守恢复决策修正同 epoch 主地址冲突，包括较低 epoch 的跨源冲突；更高单份状态不能被较旧多数覆盖，不允许包装器晋升副本，也不把 Pending/部分写标记视为重新引导许可。

以上关键修正均记录先 RED 后 GREEN。初始缺失 API 的编译 RED 与实施期间并行观察到的 RED 不作为最终结果；真实认证 fixture 不回退其它任务已经完成的实现来制造 RED。

## 主任务最终验证

以下命令在最终代码上执行并检查退出结果：

```sh
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
golangci-lint run --new-from-rev=HEAD ./cmd/apparmor-loader/... ./cmd/sandbox/... \
  ./internal/apparmorloader/... ./internal/config/... ./internal/redisbootstrap/... \
  ./internal/runtime/kubernetes/... ./internal/storage/state/redis/...
scripts/test-helm-apparmor-loader.sh
scripts/test-helm-sentinel-auth.sh
scripts/test-helm-chart.sh
HELM_BACKEND_SWITCH_RUN_CLUSTER=0 scripts/test-helm-backend-switch.sh
scripts/test-inline-credential-config.sh
git diff --check
```

最终均 exit 0，增量 lint 为 0 issues；不声称既有代码全量 lint 已清零。最后一轮并行运行正常/race/Docker 编译时，既有 mounter 的有效 s3fs stub 版本探针一次达到 10 秒上限；停止重负载并行后，该原样用例连续三次 PASS（约 0.33..0.34 秒），再完整正常测试 PASS。没有加大生产超时或修改该用例掩盖结果，记录为一次未在串行复核复现的超时。

Linux CGO=0 的 loader amd64、loader/API arm64 编译到 `/dev/null` 均通过；没有构建发布镜像。候选策略两次通过隔离 Debian parser 的 `--skip-kernel-load --skip-cache` 语法检查；无 kernel interface 的 warning 不作为 enforce 证据。

## 主任务独立真实认证复核

除子任务报告的三次正常运行外，主任务实际运行两次 `scripts/test-redis-sentinel-auth.sh`：

| 自有 project | Go integration | 同连接实际 SET 后 WAIT 1 | 清理 |
| --- | --- | --- | --- |
| sandbox-sentinel-auth-20260913183659-37071-28257 | PASS 1.320s | 2 | 完整脚本 exit 0，六卷/网络/容器为零 |
| sandbox-sentinel-auth-20260913183937-38610-29167 | PASS 3.352s | 2 | 完整脚本 exit 0，再独立 inventory 确认零资源 |

实际 Redis 7.4.11，四条独立认证链路及特殊字符 dummy 密码收敛；六端点无认证/跨端密码均拒绝，生产 Store 两端错密码分别 WRONGPASS，正确 replica_ack Set/Get 与两个副本读回通过。

主任务另执行固定 `--verify-failure-cleanup`：project `sandbox-sentinel-auth-20260913183729-37458-4711` 自有 runner 刻意 exit 42，脚本保留 42 并完成零资源清理，不输出认证 PASS。只删除本次实验的六个精确卷和对应网络/容器、mktemp 配置；未删除业务数据或镜像。

普通 `go test ./...` 中该真实认证测试默认 SKIP，不能把这个 SKIP 当作真实认证通过。真实 fixture 内没有运行 race；本机 race 与真实网络验证分别记录。详细认证边界见 [真实认证报告](2026-09-13-external-sentinel-real-auth-results.md)。

## 剩余风险与下一批

既有 Kubernetes 授权 Exec 按 Pod 名称调用，没有 UID 前置条件。guard 的前后 UID 读取不可能为后续 Exec 消除全部替换窗口；可信 supervisor 再核对 UID 并拒绝，但不能宣称凭据绝不会进入同名可信 replacement。未用冗余 GET 假装消除这个 API 边界。

内核 profile 列表只证明同名 enforce，不证明该名称的实际规则内容；可信节点管理员须维护不可变命名约定。生产 AppArmor 仍缺真实挂载/flush/卸载、子进程继承、越权拒绝、节点重载及覆盖/延迟验收，策略不自动放宽或降级。

内置 Sentinel 下一批必须先定稿并实验可信 PVC 身份注册、独立认证的新鲜 challenge/response、可中断本地配置事务、初始化 monitor-enable 与 ConfigMap CAS 边界，再接 StatefulSet/Secret/NetworkPolicy/初始化 Job/API gate。现有 MAC v1 只绑定 PVC 与进程 run_id，不证明同进程拓扑新鲜性；pure decision 不是选主或初始化控制器。不得以固定 0 号主回退、共享 Redis 密码、两个空卷默认意见或 PING/offset=0 的 WAIT 伪造安全启动。
