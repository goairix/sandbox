# CCE sandbox-api v0.3.33 现场验证

## 结论

2026-09-17 18:17–18:29（北京时间），未启用 DataPlane V2 的 CCE 隔离测试
**未通过完整 API/FUSE 验收**。v0.3.33 镜像已实际运行，普通池出现一个 Ready Pod，
但两个 FUSE mounter 被节点容器运行时拒绝：`apparmor is not supported`。
三 API 未同时 Ready，未进入业务 API 测试，不能宣称普通/FUSE 池命中、跨副本操作、
存储读写、网络隔离或销毁性能通过。

初次身份 Job 失败；相同模板的一次诊断重试成功，Sentinel 初始化随后完成。
该重试不等于原安装成功，也不等于已修复首次安装问题。
收尾时正常卸载第一次失败，经正常 UID 删除测试 FUSE Pod、取得 kubelet 终态后，
再次执行正常 hooks 卸载成功。测试 PVC/PV、namespace 和关联集群级 RBAC 均已清理。

本轮只做已批准范围内的测试、诊断和收尾；没有修改生产代码、业务/system 资源、
节点运行时配置或私有 values，没有构建镜像、回滚、创建 worktree、检查分支或委派子代理。

## 环境和范围

- kube-system UID：`cebeee78-9003-49bf-9442-3fea70515499`。
- 服务端：`v1.31.14-r20-31.0.62.9-arm64`；两个实际节点均为 amd64、Ready。
- 节点 containerd：`1.7.29-14-g6d1d1601f`；内核：
  `5.10.0-182.0.0.95.r3450_282.hce2.x86_64`。
- 随机 namespace：`sandbox-cce-sentinel-test-e623b98af4a6`；
  UID：`d2495735-0258-4a3d-9666-2d5d410ada43`；统一 release：`sandbox-fuse`。
- API：3 副本，`sandbox-api:v0.3.33`；观察到的实际 API/drain 镜像摘要：
  `sha256:16e630aed379ead417612da19f02e8a150e75b41a8fca38a77a60dae198b7de5`。
- 独立 3 成员 Redis/Sentinel、3 个 1Gi `csi-disk` PVC；测试 renderer 仅软化
  Sentinel hostname 反亲和，不修改 DNS、安全或生产默认反亲和。
- 保持 productionSafetyChecks、真实 AppArmor 和 `allowMissingLSMForKind=false`；
  loader 两节点均 Ready，bootstrap 镜像仍为 `sandbox-redis-bootstrap:v0.3.0`。
- 显式 NodeLocal opt-out 和两项 DNS options；标准 NetworkPolicy provider；
  管理员给定 Pod CIDR `10.0.0.0/16`、Service CIDR `10.247.0.0/16`。
- 存储仍是 TLS MinIO，不是 OBS；使用本轮唯一工作空间前缀，没有发起业务对象读写。

## 现场结果及证据边界

| 项目 | 结果 | 边界 |
| --- | --- | --- |
| 新镜像拉取/运行 | 已观察到 v0.3.33 和上述摘要 | 不是 API Ready 证明 |
| 身份 Job 首次运行 | 失败，exit 1、BackoffLimitExceeded | 只有身份错误类别，不能确定精确内部失败分支 |
| 身份 Job 一次原样诊断重试 | 成功 | 同 namespace、同镜像/模板、无额外 PVC；先确认新装授权和无旧身份 Secret |
| Sentinel 初始化 | Job 成功、状态 Initialized、registration 存在、3 成员 Ready | 没有节点故障、冷恢复或云环境 HA 验收 |
| AppArmor loader | 2/2 Ready、零重启 | 不等于 CRI 支持 AppArmor，也不是本轮 mounter enforce 证明 |
| 普通池 | 一个物理 Pod Running/Ready | 没有调用创建/exec/销毁 API，不能宣称普通沙盒完整可用 |
| FUSE 池 | 两个 Pod 的 mounter 为 CreateContainerError | CRI 明确报告 AppArmor 不支持，未开始挂载 |
| 三副本 API | 约 7 分钟测试等待窗口内未同时 Ready | 初期无日志，不能单凭无日志认定 Redis gate 故障；后期已出现池 Pod |
| 首次正常 uninstall | drain 失败 | 两个未启动 FUSE Pod 缺少终止证明，报 runtime termination is unconfirmed |
| 取得 kubelet 终态后的正常 uninstall | 成功 | 没有强删、跳过 hooks 或手工移除 finalizer |

FUSE 事件和 waiting 状态明确来自生成 AppArmor spec options 的失败，不是 DNS
意图差异或对象存储挂载耗时。本轮没有观察到上一轮的 DNS 契约拒绝；普通 Ready Pod
说明该路径至少已向前推进，但完整 DNS/loader/FUSE 验收仍受不同阻塞影响，不能打勾。

## 原因分析和待整改

### 1. CCE 节点运行时 AppArmor 支持：环境阻塞

loader 的内核 profile 加载/就绪与 containerd 的 CRI AppArmor 能力是不同条件。
上游 [containerd AppArmor 生成逻辑](https://github.com/containerd/containerd/blob/v1.7.20/pkg/cri/server/container_create_linux.go)
在 AppArmor 能力不可用、请求非 Unconfined profile 时返回同名错误；
[能力检查](https://github.com/containerd/containerd/blob/v1.7.20/pkg/cri/server/helpers_linux.go)
同时涉及 runtime 的禁用配置和 host 支持检测。
这些上游参考用于解释错误类别，不代表已审计 CCE 的厂商补丁二进制。

尚未读取宿主机 containerd 配置，不能直接认定 `disable_apparmor=true`、缺少 parser
或其它 host 检测条件中的哪一个。需要平台侧确认/启用 CRI AppArmor 支持，或另行设计
并验收受支持的 SELinux 方案；本次不通过关闭 LSM 或放宽安全契约绕过。

### 2. PVC resourceVersion 变化：本地已复现的身份校验竞态

检查当前工作区 `createFreshIdentity` 的两次 PVC 快照校验时，发现它比较包含
resourceVersion 的整个记录。正常 PVC 绑定更新即使不改变 UID/身份注解，也会拒绝。
使用 fake client 最小复现（随机测试密钥不输出）得到：

```text
bindingRVChanged=false success=true rejectedAsIdentityInvalid=false secretCreates=1
bindingRVChanged=true success=false rejectedAsIdentityInvalid=true secretCreates=0
```

两组使用相同 namespace、PVC 名称、UID 和 clusterID 注解，第二组仅让两次读取间
resourceVersion 从 1 变为 2；没有真实集群写入。
这证明当前本地代码存在正常控制器更新被当作身份错误的路径，**不是证明本轮已发布
bootstrap 镜像的首次失败一定发生在该路径**。现场缺少原请求追踪，不合并两个结论。
后续应设计有界重新校验，保留 UID replacement、删除、身份注解/授权改变的拒绝，
不能直接忽略全部快照变化或为保留 PVC 重建身份。

### 3. 未启动 FUSE 的清理收敛：代码待整改

当前清理在非终态 Pod 上尝试 mounter shutdown；CreateContainerError 时没有可执行的
mounter，且 Pod 尚未进入 deleting/terminal，无法取得终止证明，因此闭环停住。
两 Pod 正常 UID 删除后，kubelet 将其转为 Failed：mounter terminated、sandbox
provably never started。现有 drain 随后取得证明并自行完成 finalizer/策略/状态收尾。

后续需要覆盖“准备失败且 mounter 未启动”的正常删除→等待 kubelet 终态→精确证明路径，
保留活进程、状态缺失、节点失联及 UID replacement 的失败关闭；不能把 Pending 或
ContainerID 为空直接当成安全销毁证明。本轮没有改这段生产行为。

## 收尾与复核

两个测试 FUSE Pod UID：`57a4f246-8a65-4be1-be3d-d54b384bf7ce`、
`b531025a-a944-4cdd-9467-276de4f35f3d`。正常删除前核对全部声明的容器状态完整，
均 waiting、零重启、无 ContainerID/started/历史运行记录，且 API Deployment 已为 0。
删除使用 UID 前置条件，未更改 finalizer 或强制缩短删除期限；等待 kubelet 终态后
重新执行普通 `helm uninstall`，由应用自己的 hooks 完成清理。

三组卷绑定在删除前验证 claimRef namespace/UID 及 Delete 回收策略：

| PVC UID（PV 名称为 pvc-加此 UID） | PV UID |
| --- | --- |
| `aa6ba886-7d94-40da-bf7e-ab40a0a713b2` | `8cc6077d-2fa4-466e-b0df-6c0d252b3f84` |
| `69ffca42-06f6-43d5-bc62-664fdabd0a15` | `0b6c1685-f9b6-4d19-85cd-01b0f80cd921` |
| `c030ac75-5dca-47c7-bea6-cc7d6c1edbff` | `b6805ed6-4241-4e42-a7de-42d898777cb6` |

18:29:14 清理脚本确认 PVC/PV/namespace 删除完成；其后独立只读复核确认：
测试 namespace、关联 PV、ClusterRole/ClusterRoleBinding 均为零，kube-system
非终态未 Ready Pod 为零，两节点 Ready。没有卸载内核 profile。
PV 对象消失是 Kubernetes 回收证据，不代表云账单已核销。

原 ds-ai-research 仅只读查看：仍是 v0.3.32，三个 API Ready、零重启，没有部署或修改。
它不是 v0.3.33 默认路径回归证明。最低 1.29 生命周期、VPC NetworkPolicy 实际执行、
DataPlane V2、OBS、节点级 Sentinel HA 和全部业务 API 仍未由本轮验收完成。
