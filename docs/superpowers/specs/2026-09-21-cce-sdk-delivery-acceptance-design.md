# CCE 交付文档与 Go SDK 验收设计

## 目标与边界

在已部署的双华云 CCE DataPlane V2 测试集群上，通过仓库现有 Go SDK 验证普通沙盒和 FUSE 工作空间完整生命周期，并让仅持有 `tools/` 交付包的运维人员能依照文档完成同类验收。修正现有 Helm 文档中内置 Sentinel 与 `--atomic` 的矛盾。此次不改变服务端 HTTP 契约、不扩展 SDK 公共 API，也不修改集群网络、Redis 身份或 AppArmor 安全门禁。

## 现状

`sdk/go` 已支持 `WorkspaceMountFUSE`、`WorkspacePath`、普通/FUSE 创建、exec、sync、workspace info 和销毁，现有单元测试通过。缺口是缺少调用真实部署服务的可重复 SDK 验收入口，运维 runbook 只有手写 `curl`，且其 Secret 名称和 FUSE 样例需要与当前 Chart 实际输出核对。Helm 部署文档写明内置 Sentinel 不得用 `--atomic`，但安装、升级示例仍包含该参数。

## 方案

1. 在 `sdk/go` 模块内提供基于公开 SDK 接口的可编译验收命令及测试。发布人员从已锁定的代码版本分别构建 Linux amd64/arm64 单文件程序，连同 `tools/` 交给运维；运维不需要仓库业务代码或 Go 构建环境。程序从环境变量读取 API 地址和 API key；不打印凭据、响应头或完整 Secret。运维通过 `kubectl port-forward` 接入目标 release。程序不直接拼 HTTP 请求。
2. runbook 先用 `curl` 检查 `/health`、`/ready`，SDK 程序再依次执行普通沙盒创建与 exec、FUSE 沙盒创建与写读、`Sync`、`WorkspaceInfo`，并分别销毁。FUSE `WorkspacePath` 使用运行时生成的唯一测试前缀，避免碰撞已有工作空间。每一步设置有界 context；只在创建返回有效 ID 后注册清理，失败时尽力使用独立有界 context 销毁已创建实例，并明确报告需要人工核查的 ID。销毁 API 成功不单独证明 Pod/策略零残留，runbook 还须要求核对目标实例和池恢复。
3. 本仓库当前集群测试从 `sdk/go` 模块直接运行同一验收命令。运维交付包只新增构建产物与使用说明；不复制 SDK 源码，不引入隐藏或浮动版本依赖。构建命令和产物摘要由发布人员交付。
4. 更新 `tools/apparmor-hce/deploy-runbook.md`：给出 SDK 验收命令、环境变量、期望结果、失败留证与清理检查；修正当前 API Secret 名称和 FUSE 申请样例；记录 CCE NodeLocal DNS 的显式 opt-out/options，以及 `csi-disk` Sentinel PVC 配置应从实际集群确认，不能照搬网段或存储类。
5. 更新 `docs/deployment/helm-deployment-upgrade.md`：Sentinel 的安装/升级示例不再含 `--atomic`；保留 standalone/external 的条件化说明，避免误导。只改受影响命令，不重写整篇历史说明。

## 验证与通过标准

- Go SDK 模块单测、验收程序单测和构建通过；文档命令经过静态检查。
- 对当前 CCE 集群运行 SDK 程序：普通创建/exec/销毁及 FUSE 创建/写读/sync/info/销毁均成功，`WorkspaceInfo` 显示 `mounted=true`、`mount_type=fuse`、`flushed=true`。
- 集群复核 API、loader、Sentinel 均为期望副本；对应测试沙盒和 NetworkPolicy 无遗留。FUSE 空闲池 `1/2` 的未授权 mounter 不作为失败；HPA 缺 `metrics.k8s.io` 单独记录，不降低安全验收。
- 不运行 Helm rollback/`--atomic`，不删除 Redis 身份 Secret/PVC，不更改现有业务工作空间。测试产生的对象存储唯一前缀按运维数据保留策略处理，程序不自动递归删除对象。

## 非目标

不验证 Sentinel 故障切换、节点重启后的 containerd 配置持久性、Cilium 策略的所有拒绝路径或其它云存储后端。这些需要独立演练，不以本次 SDK smoke test 代替。
