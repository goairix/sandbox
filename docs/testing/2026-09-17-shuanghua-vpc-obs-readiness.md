# 双华云 VPC 网络与 OBS 存储：测试前准备核对

## 状态与范围

最新追加（2026-09-17 16:28）：内置 Sentinel 已恢复 1.29 版本下限，在单节点 CCE 1.31
隔离环境验证了三个独立 PVC 成员的初始化、鉴权、quorum、ACK 和单容器恢复，测试资源
已回收。完整 API 仍受 AppArmor loader/DNS 准入契约阻塞；网络策略、MinIO/FUSE 和 OBS
矩阵未执行，不把 Sentinel 通过写成云网络/存储通过。
见[本轮兼容整改报告](2026-09-17-sentinel-kubernetes-129-compatibility.md)。下文较早的
预检/准备状态作为历史记录保留。

新增第二节点后系统组件已全部就绪。仅禁用隔离 namespace 的 NodeLocal 注入不能消除
额外 DNS options 默认化；测试 DS 显式补齐 options 后，API 越过 loader 门禁但普通池
意图比较仍拒绝，错误诊断的正规化也不一致。此次测试资源已清理。后续兼容配置不得
改变此前集群的默认行为；网络/MinIO/FUSE/OBS 尚未验收，测试资源前缀统一 `sandbox-fuse`。

后续追加（2026-09-17 15:34）：已收到未启用DataPlane V2的单节点测试集群和私有values，
只读预检发现Kubernetes 1.31、无默认StorageClass、ENI-only且无可自动发现的完整CIDR、
AppArmor内核已启用。用户随后提供Pod `10.0.0.0/16`、Service `10.247.0.0/16`，地址库存
交叉核对一致；原values被Sentinel持久化/版本及生产HA门禁阻止。当前仍是MinIO，不是OBS，
没有安装或修改测试资源。见[首个集群预检](2026-09-17-shuanghua-no-dpv2-preflight.md)。

2026-09-17 已完成当前 `ds-ai-research` 的
[API v0.3.32 回归](2026-09-17-ds-ai-research-v0.3.32-validation.md)。本文记录新环境的已查证
前提、候选测试范围和待提供输入，不是已批准的执行设计或双华云通过报告。尚未在双华云创建
资源、读取 OBS 业务前缀或修改线上 values。

建议独立 namespace/release、独立 Redis 状态 scope、专用测试存储前缀。分阶段方案先验证
真实网络隔离、DNS/TLS 和普通 OBS sync，再运行三副本普通/FUSE 联合矩阵，便于定位失败层。
另一方案直接执行完整三副本矩阵，减少阶段启动次数，但首次失败的定位范围更大。目标环境、
隔离范围确认后，再形成正式设计和有界驱动；不直接复用当前业务池做网络策略实验。

## 已查证的云网络前提

双华云官方网络指南区分节点、容器和 Service 网络，VPC 路由网络与直接从 VPC 分配 Pod IP
的云原生网络 2.0 不是同一模型，不能只凭“VPC”名称判断 IPAM 或分配范围。
来源：[网络概述](https://docs.shuanghuayun.com/zh-cn/usermanual/cce/cce_10_0010.html)。

官方 NetworkPolicy 支持表说明：VPC/云原生网络 2.0 的策略能力默认关闭，需要创建集群时
开启 DataPlane V2，并受集群补丁版本和节点操作系统限制；VPC 模型不支持 IPv6 策略，
`ipBlock` 不能按普通外部地址直接配置容器/Service 网段内地址及节点 IP。准确支持范围必须
对照实际环境，策略创建成功和 Pod Ready 不能代替数据面执行证据。
来源：[网络策略支持表](https://docs.shuanghuayun.com/zh-cn/usermanual/cce/cce_10_0059.html)。

2026-09-17 通过官方页面引用的同路径 JSON 读取正文，两篇版本均为 26.4.1。该文档版本
不是用户测试集群的实际 Kubernetes 版本。DataPlane V2 默认关闭尤其需要创建环境时确认；
若未开启，先报告前提不满足，不通过修改沙盒默认安全策略来“适配”。

## 当前实现边界

- 非 Cilium 环境使用标准 NetworkPolicy，候选配置为
  `config.runtime.kubernetes.networkPolicyProvider: standard`，以实际插件核对为前提。
- Calico Kubernetes IPAM 的自动发现读取全部 IPPool，不是必须先手工创建新池。云 VPC/
  外部 IPAM 的 Node PodCIDR 不一定权威；需要集群控制面提供完整 Pod/Service 分配范围。
  现有显式 `podCIDRs` 和 `serviceCIDRs` 必须两者完整，不从几个 Pod IP 猜子网，不填全网。
- 标准 FUSE system egress 当前以精确 resolver host CIDR/53 与已解析后端地址/端口表达。
  双华云对集群地址 `ipBlock` 的限制可能影响 DNS/Service 规则，需要真实测试，不能预判兼容。
- 标准 NetworkPolicy 的多策略为允许集并集，而且同 Pod 容器共享网络命名空间。因此必须核对
  FUSE system egress 对租户容器的实际影响，不能承诺 mounter 的网络放行只作用于 mounter。
- 当前编译目录包含 `huawei-obs-private-2023-v1`：HTTPS、virtual-host、SigV2、`compat_dir`。
  普通 OBS sync 使用原生 OBS driver；FUSE 使用 s3fs。MinIO 的 path-style/SigV4 独立对象
  校验器不能冒充 OBS 校验器。
- endpoint 与 `bucket.endpoint` 的解析、TLS/SNI、CA 信任均须验证。保持证书验证，不用
  `no_check_certificate` 或临时凭据不受支持的模式绕过契约。
- FUSE profile YAML 的供应商 evidence bundle 仍标为 blocked、实际环境证据为空；编译目录
  已支持不等于新私有云版本已经通过。旧 OBS values 样例含旧镜像/profile 参数，不直接部署。
- 节点 LSM 必须根据实际 OS/kernel 验证 AppArmor 或 SELinux；当前集群五节点 AppArmor
  结果不能外推到云节点。生产不启用 `allow_missing_lsm_for_kind`，不为挂载开放 privileged。

实现入口：`internal/runtime/kubernetes/service_targets.go`、`network.go`、
`internal/mounter/profile.go`、`internal/storage/filesystem.go`、
`testdata/fuse/profiles/huawei-obs-private-2023-v1.yaml`。

## 目标环境需要提供的非敏感输入

- 每个测试环境的 kubectl context、可用测试 namespace/release，以及是否允许创建隔离 fixture；
- 实际 CCE/Kubernetes 版本、VPC 或云原生网络 2.0 模型、DataPlane V2 状态、节点 OS/架构/
  runtime/LSM，以及权威 Pod 和 Service 全分配范围；
- OBS bucket、region、HTTPS endpoint、OBS 服务版本、是否使用私有 CA、可用测试前缀范围；
- 测试节点的镜像仓库可达性、可用镜像版本/架构，以及 DNS 和对象存储的真实访问路径。

AK/SK/API key 不发聊天，不入报告/命令行参数；保留在测试环境自己的受保护配置。身份凭据
需要仅授权测试范围的 list/get/put/delete 及 multipart 收尾，具体 IAM 约束以该私有云实现为准。

## 候选验收矩阵（未执行）

1. 用可达的正向控制验证 NetworkPolicy 执行、Service selector 放行/撤销/恢复、同 namespace
   非匹配 Pod 与其它 namespace 同标签 Pod 拒绝、未许可私网/metadata 拒绝；普通与 FUSE
   均覆盖。验证实际 PodIP/ServiceIP 及相应 CIDR不能通过 literal 白名单绕过身份规则；
   云内部服务网段、DNS、SNAT/DNAT 和同节点流量也列入边界核对。
2. 普通 sync：上传、空文件、下载、内容 hash、同步/卸载/销毁，以及三副本状态与资源限额。
3. OBS FUSE：专属随机路径、prefix marker、热池实际 UID 领取、写入/fsync/flush；通过独立
   原生 OBS 客户端 TLS GET 校验 hash，再正常销毁、重新挂载验证持久数据。补目录标记、
   多文件和达到实际 multipart 门槛的大对象，逐项检查兼容行为。
4. 记录创建/销毁/flush/重挂载时间线及错误率。功能阶段的小样本不用于关闭长期性能 SLO；
   不把父子阶段相加，不因超时而删 fsync、租约、exact UID/generation 或终止确认。
5. 最终按本次 run 的精确 namespace UID、资源 UID 与存储随机前缀清理，并审计对象/目录
   marker/multipart 遗留、Pod/策略/会话/owner/lease。只移除测试资源，不卸载业务 loader 或
   清空业务 Redis；失败保留诊断证据并通过正常 drain 恢复，不 force、不裸删状态。

双栈按实际模型逐项标明通过、不支持或未测试；节点重启、网络分区、Sentinel 切主和全组
冷恢复需要另行授权的故障窗口，不在这一准备记录中隐含执行。
