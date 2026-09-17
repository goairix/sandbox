# 双华云未启用 DataPlane V2 集群：只读预检

## 结论与执行边界

2026-09-17 15:34（北京时间）完成目标身份、节点、存储、配置及镜像架构的只读复核。
集群可访问，但用户提供的 values 不能直接部署当前 Chart，已通过本地渲染复现门禁失败。
本次没有安装 Helm release、创建 namespace/Pod/NetworkPolicy/PVC、修改 values/kubeconfig、
改变节点/系统组件或访问 MinIO 对象。真实网络执行、API、FUSE、存储功能均尚未验收。

用户声明此环境未启用 DataPlane V2。本轮只记录此输入，没有以 CRD/Pod 状态替代云控制面
开关证据，也没有将“无 V2”直接写成已经实测流量不受控。

两份私有文件实际位于仓库的 `var/tmp/cce/`，不是系统 `/var/tmp/cce/`；已经被该目录的
`.gitignore` 忽略，未提交凭据。文件权限均为 0644，后续执行前应收紧；不复制其中的密钥、
证书私钥、密码或完整配置到报告/日志。

## 目标身份与资源

| 项目 | 实际证据 |
| --- | --- |
| kubeconfig 有效 context | `internal`；文件的 current-context 为不存在的 `cce-sandbox-fuse`，本轮显式指定 `internal`，没有修改全局默认 context |
| API server | HTTPS，CA 验证开启；无 exec 认证插件或外部密钥文件引用 |
| Kubernetes | `v1.31.14-r20-31.0.62.9-arm64`；版本字符串末尾不代表工作节点架构 |
| 唯一节点 | `172.16.30.166`，UID `69a79f2b-4f61-44c7-b46a-c46230c3530c`，Ready |
| 节点平台 | amd64；Huawei Cloud EulerOS 2.0；kernel `5.10.0-182.0.0.95.r3450_282.hce2.x86_64`；containerd 1.7.29 |
| 容量 | 4 CPU、约 8 GB RAM、40 Pod；可分配 CPU 3920m、内存约 6.24 GiB、临时存储约 8.77 GiB |
| 目标 namespace | `aiadp-sandbox-fuse` 尚不存在 |
| 存储 | 有17个StorageClass对象，无默认StorageClass，全集群PVC数为0；未验证供应成功，不把对象存在写成可用卷 |
| 当前后端配置 | MinIO，HTTPS，使用现有 bucket/subPath；不是 OBS |

四个系统组件的额外副本处于 Pending，PodScheduled 证据指向 required pod anti-affinity，
与只有一个节点相符。本轮不修改 CoreDNS、Everest、node-local-dns 或节点问题控制器副本数。

## 当前 values 的阻碍：逐项隔离复现

原配置为 `productionSafetyChecks=true`、`redis.mode=sentinel`、
`redis.persistence.enabled=false`。当前 Chart 的内置 Sentinel 还要求 Kubernetes ≥1.33，
并在 StatefulSet 中使用三个不同节点的 required pod anti-affinity。

全部是本地 `helm template`，输出清单仅在私有内存中检查，不连接集群，不保存含凭据的清单：

| 本地诊断变体 | 结果 |
| --- | --- |
| 原 values，KubeVersion 1.31.14 | exit 1：`built-in Sentinel requires persistence` |
| 只覆盖 persistence=true | exit 1：`built-in Sentinel requires Kubernetes >=1.33` |
| 只覆盖 mode=standalone | exit 1：`productionSafetyChecks requires external HA Redis or built-in Sentinel` |
| 仅诊断：standalone + productionSafetyChecks=false | exit 0；仅表示渲染成功，不表示部署/安全验收通过 |

因此不能用“启用持久化”或手工安装 StorageClass 来单独解决当前 Sentinel 部署问题，也不应
在单节点上通过放宽 Sentinel 反亲和假装三个成员是高可用。

API 配置还开启 HPA（3–10 副本）、普通/FUSE池各min=3、max=20。单节点验收需要限制
总容量并避免自动扩容；三个API进程可用于跨副本状态测试，但不代表节点级高可用。

## 网络范围与 DNS

- Cilium/Calico 分配 CRD 均未发现；可见 `nodenetworkconfigs.crd.yangtse.cni`；
- 唯一 Node 没有 `spec.podCIDRs`；ServiceCIDR API 不可用；
- NodeNetworkConfig 为 `eni-only`，没有完整集群 Pod/Service CIDR；这不能作为所有当前/
  未来分配范围的权威证据，也不单凭资源名称认定云控制面的网络模型；
- CoreDNS Service 为 `10.247.3.10`，node-local-dns 配置包含 `169.254.20.10`；
- 需要 CCE 控制台的完整容器/Pod分配子网和Service网段，不能从上述IP猜网段；
- 后续探针必须同时保留可达正向控制和施加策略后的阻断/撤销/恢复，覆盖实际DNS及Service
  DNAT边界。策略对象创建成功不能写成真实数据面已经执行。

15:36追加：用户提供控制台容器网段 `10.0.0.0/16`、IPv4 Service网段 `10.247.0.0/16`。
只读交叉核对四个已分配地址的非hostNetwork Pod及八个ClusterIP Service，没有地址在对应
范围之外。完整/未来分配的权威来源仍是用户控制台输入，不是这十二个地址样本。

## 节点 LSM 与镜像

使用已经存在的 NPD Pod 做只读文件读取，前后守护其精确 UID；未启动特权诊断 Pod、未
修改内核。读取 `/host/sys/kernel/security/lsm` 得到 `lockdown,capability,yama,apparmor`，
内核 AppArmor enabled=Y。尚未运行本项目 loader、加载或验证本项目 exact profile，不能
把内核启用等同于 FUSE 的实际 enforce 已通过。

注册表 manifest index 核对结果：以下引用均包含 linux/amd64，另含 arm64；没有拉取
或启动镜像，也没有取得本节点实际运行中的 child imageID：

```text
sandbox-api:v0.3.32
sandbox-runtime:v0.3.4
sandbox-fuse-mounter:v0.3.30
sandbox-apparmor-loader:v0.3.24
redis:7.4.2
```

以上均来自 `registry.i.huaxisy.com/library/`，前四个在 `ai-infra/` 下。API server 的版本
后缀不能用来要求本节点改用 arm64 镜像。

## 待确认的测试专用方案

推荐先用无凭据、无卷的最小网络 fixture 验证实际能力，再在专用隔离 namespace/release中
使用 standalone Redis（不申请PVC）、固定三API进程、关闭HPA和小规模普通/FUSE池做可信
测试代码的MinIO功能矩阵。保持HTTPS、真实AppArmor、`allowMissingLSMForKind=false`、
exact UID/generation及正常drain；明确此环境不具备节点/Redis HA，不接入不可信租户流量。

这需要用户确认测试专用 `productionSafetyChecks=false` 的配置例外，不能改Chart门禁或
以此让生产环境“通过”。该开关同时包含生产HA与LSM配置检查，不能称为只关闭HA；测试
驱动仍需独立断言真实LSM/精确profile、缺失LSM豁免为false及正常清理。另一选择是本环境
只做网络能力负向探针，完整API/存储矩阵留给
后续满足前提的环境，资源开销较低但无法取得本环境的MinIO/FUSE兼容证据。

15:36之后对候选配置做本地渲染及结构断言，exit 0：standalone Redis为一个副本、无PVC；
API固定三个副本、没有HPA；ordinary池min/max=1/3、FUSE池min/max=1/2；提供方standard，
两个网段正确进入API env；loader保留、缺失LSM豁免为false。没有打印/保存完整清单、修改
原values或连接集群执行部署。这不是已部署/网络隔离/LSM enforce的通过证据。

完整网段已收到，但测试配置例外和执行方案尚未确认，因此本轮不修改配置或部署测试资源。
正式执行设计、逐步预算、资源/对象精确收尾及网络失效结论需在用户确认后形成。

## 后续追加：版本整改与真实 Sentinel 子集

2026-09-17 16:28：用户确认单节点测试三个成员和有界 PVC 开销后，已按独立设计恢复
Sentinel 1.29 版本下限，完成单卷供应/挂载/回收及 CCE 1.31 三成员初始化、认证、
quorum、ACK 与单 Redis 容器重启恢复。原私有 values 内容未改，文件权限已收紧到 0600。
完整 Helm/API 尚未通过：AppArmor loader 的 CCE DNS 准入改写与严格模板门禁冲突。
所有隔离测试资源已收尾；上述历史预检结果保留原样，不追溯改写成通过。
详见[本轮兼容整改与真实测试报告](2026-09-17-sentinel-kubernetes-129-compatibility.md)。

用户补齐第二节点后，系统组件全部就绪；再次隔离部署证明禁用 NodeLocal 注入仍有额外
DNS options 默认化。测试 DS 显式补齐 options 后 API 越过 loader 门禁，但普通池意图
校验仍拒绝。诊断 helper 的正规化与实际比较也不一致，当前完整 API/网络/FUSE 仍未通过。
第二轮资源已正常清理；后续测试统一 `sandbox-fuse` 前缀，并保持原集群默认行为不变。
