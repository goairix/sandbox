# HCE / EulerOS 2.0 双架构 AppArmor parser 自行构建设计

日期：2026-09-18。用户已确认 FUSE 必选、没有可用供应商 AppArmor 包，并选择自行构建，同时覆盖 amd64 与 arm64。本文件将该路线落实为构建与验收边界；书面设计确认后再编写实施计划和代码。

## 目标与不变项

- 为实际部署的 Huawei Cloud EulerOS 2.0（HCE 2.0）提供可审计的真实宿主机 `apparmor_parser`，分别生成 `x86_64`、`aarch64` RPM；两架构采用同一上游源代码和构建规则，不共用二进制。
- FUSE 保持启用，生产 `workspace.allow_missing_lsm_for_kind` 保持 false。不得用 Unconfined、假 parser、占位链接、关闭 LSM 或更换存储模式绕过启动检查。
- Kubernetes 最低支持版本仍为 1.29。不修改当前 API、池、网络策略、Redis Sentinel、AppArmor loader 的功能或权限，不影响其它已验证集群。
- 构建、包验收与节点维护是独立阶段。构建命令不得安装宿主机软件、加载主机策略、修改 CRI 或重启服务；本轮不构建或推送业务镜像。
- 自维护包的兼容性与更新由本项目和平台管理员负责；不能将试验通过描述为双华云厂商已经认证或支持。

## 已知事实与适配依据

现有测试节点报告 HCE 2.0、amd64、5.10 厂商内核及 containerd 1.7.29 厂商版本。两节点内核 AppArmor 已启用，预期 FUSE profile 已加载为 enforce；宿主机 `/sbin/apparmor_parser` 入口不存在，静态 containerd 配置显式禁用 AppArmor。尚未查询运行中 CRI 有效配置或取得整改后的真实 FUSE/enforce 证据。详见[只读诊断与单节点维护授权记录](../../testing/2026-09-17-cce-euleros-apparmor-host-diagnostics.md)。

适配以实际 HCE 系统和软件 ABI 为准，不凭“继承 CentOS”选择 CentOS/RHEL 二进制包。[华为 HCE 说明](https://support.huaweicloud.com/hce_faq/hce_03_0003.html)将该产品的基础发行版说明为 openEuler；[HCE 软件仓库说明](https://support.huaweicloud.com/intl/en-us/usermanual-hce/hce_repo.html)提供 RPM/DNF/YUM 仓库机制。这些说明不能替代本项目 AppArmor 包的实际兼容性验证。

目前只有 HCE amd64 测试节点；没有 HCE arm64 节点或其维护入口。arm64 构建检查可以单独完成，但 HCE arm64 内核、CRI 与 FUSE 现场验收必须记录为未完成，不能由其它 OS 的 arm64 测试代替。

## 源码与构建环境

首个候选固定为上游 AppArmor **4.1.7**，不使用 master、latest、预发布版或现有旧版 2.13.4 spec。[官方发布说明](https://www.apparmor.net/about/releases/)记载 4.1 为长期稳定系列，4.1.7 包含 parser 权限表输出修复。候选版本不等于已经适配 HCE 内核；兼容性仍由下面的验收决定。

源码锁定清单必须记录版本、官方发布资产 URL、源代码 SHA-256、上游签名及核验用公钥指纹。编译前先验证签名和固定摘要；公钥信任必须来自另行核验的上游身份，不允许仅凭同一下载地址同时提供的包、摘要与陌生公钥建立信任。摘要不匹配、签名无效或信任未建立立即失败，不自动改版本、换镜像或关闭 TLS 校验。

两个架构在相应 HCE 2.0 隔离构建环境中分别编译。构建环境由管理员提供可信的 HCE VM 或 rootfs/容器基础，记录其版本、来源与固定摘要；项目不编造官方 HCE OCI 镜像名。入口核验 OS、执行架构及目标架构，并显式映射 `amd64 ↔ x86_64`、`arm64 ↔ aarch64`，拒绝其它或混淆值。

优先原生架构构建。如管理员使用现成仿真环境，产物必须标记仿真构建，不能据此获得原生 HCE 运行通过结论；脚本不安装 privileged 仿真器。禁止在集群业务节点现场编译，也不把 macOS、Debian、Ubuntu 或任意 openEuler 镜像当作已验证的 HCE ABI 环境。

按[上游构建说明](https://gitlab.com/apparmor/apparmor/-/raw/v4.1.7/README.md)先构建 libapparmor，再构建 parser，执行对应 `make check`。构建依赖只留在隔离环境，不安装到节点；记录依赖 RPM 版本、编译器、构建参数及测试结果。

## 最小 RPM 与产物契约

包统一命名为 `sandbox-apparmor-parser`，版本使用上游版本，Release 标记 HCE 打包修订；架构字段只能为 x86_64 或 aarch64。必须生成对应 SRPM、二进制 RPM、SHA-256 清单及机器可读构建/验收清单。保留许可证、上游源代码及完整重建所需脚本，清单包含组件与链接依赖，不提交私有签名密钥、认证信息或集群配置。

RPM 只安装真实 parser、运行所必需的配套文件、许可证及文档，不包含通用主机应用 profiles、AppArmor systemd/init 服务、Python 工具、选主逻辑或 loader。只打包默认 CRI/profile 编译确实需要的基础 include/ABI 数据；若需要，逐项列出并核验，不为方便安装整个 profiles 集合。配套数据进入本包专属目录，通过真实 parser 的编译时默认搜索路径使用；不得悄悄覆盖 `/etc/apparmor.d` 的现有主机配置。

parser 可静态链接本次构建的 libapparmor 和工具链支持库，但不能宣称默认构建就是完全静态。对最终 ELF 核验 machine、解释器、所有动态依赖和 GLIBC 等符号版本；动态系统依赖由 RPM 正确声明并在目标 HCE 验证。不得捆绑替换系统 glibc、覆盖系统动态库，或使用 `--nodeps`/忽略依赖检查安装。

安装路径需保证目标 CRI 实际查找的 `/sbin/apparmor_parser` 可达真实 ELF：HCE 的 `/sbin` 若链接到 `/usr/sbin`，使用正常发行版路径；若布局不同，包内提供指向真实 parser 的必要入口。入口不是占位文件、假版本脚本或只返回成功的 wrapper。RPM 安装前核验现有路径和包归属，有冲突则停止，不能强行覆盖。

RPM 不包含修改 containerd 配置、启动/重启服务、批量加载/卸载策略或执行节点维护的 scriptlet/trigger。包安装本身不得改变运行中容器行为；启用 CRI 的维护步骤由管理员独立执行。

内部生产发布需管理员用可信内部密钥签名并提供公钥分发机制。实验室未签名包必须明确标记，仅能在显式批准的隔离测试范围使用，不能标为生产就绪；脚本不自动执行 `--nogpgcheck`，也不生成或存储生产签名私钥。

## 构建与离线验收

新增的源码锁定、RPM spec、构建入口和检查工具放在独立的宿主机工具目录，与现有 `docker/images/apparmor-loader` 隔离。入口输出明确的包路径与每阶段状态，失败返回非零，不输出凭据，不从用户环境隐式选取集群或节点。

必须覆盖以下成功与失败场景：

- 两架构正确映射、错误 OS/架构拒绝，源码摘要/签名错误拒绝，构建或上游测试失败拒绝；不得输出误导的“全部通过”。
- 审计 RPM payload、依赖、文件属主/模式、入口链接与安装动作；多余服务/profile、越界路径、冲突文件、假 parser、缺失运行库或架构不符均拒绝。
- 在各自 HCE 隔离环境安装并运行真实 `apparmor_parser --version`，不能仅靠 `file`/`readelf` 或仿真得到运行兼容结论。
- 使用实际 Chart 模板生成的完整 FUSE profile，按现有替换、归一化、内容哈希规则生成名称，不能直接编译含占位符的模板。
- `-Q -K` 跳过内核加载与缓存，验证实际编译；语法输出不是编译、加载或 enforce 证据。离线使用经只读采集的目标内核 features 时必须记录其架构与来源；没有该数据就只报告用户态检查，不能推断目标内核兼容。
- 除本项目 profile 外，还验证目标 containerd 生成的实际默认 profile 及必要基础数据。所有策略降级、忽略规则和 ABI 不匹配告警必须审查；不得用 quiet、较弱 features/ABI 覆盖或删除安全规则获得通过。

## 节点验证与维护边界

后续首次变更仅限已获维护授权的 CCE 测试节点 `172.16.30.166`（UID `69a79f2b-4f61-44c7-b46a-c46230c3530c`），集群 kube-system namespace UID 必须仍为 `cebeee78-9003-49bf-9442-3fea70515499`。不自动操作第二节点、`ds-ai-research`、生产或尚未提供的 arm64 节点。

执行前确认可信兼容产物、管理员维护入口和恢复方案，再核验节点身份、健康、PDB selector、可迁移工作负载与剩余容量，按平台维护流程受控 cordon/drain。现有 kubeconfig 不等于已取得宿主机维护入口；构建工具不创建挂载主机根目录/runtime socket 的 privileged 安装 Pod 或扩权 loader 来解决入口问题。

安装真实 parser 后，分别记录静态配置与运行中 CRI AppArmor 状态，必要的配置修改只针对已审查的有效配置，不盲目批量替换文本。只有上述前提齐备才受控重启获准的 containerd，并等待节点、系统组件和 CSI 恢复；不重启 kubelet、不更换 OS、不修改内核启动参数、不自动回滚发行版或扩大节点范围。

首次现场验收使用独立临时 namespace，测试 loader、API 与沙盒均显式限定到已授权目标节点，不改变现有业务池或在第二节点加载策略。测试部署不得为了限定节点而静默放宽已有 Redis HA 安全约束；遇到资源或拓扑条件不足，记录具体条件并停止相应测试。

现场验收每个架构分别要求：

1. 宿主机真实 parser 可执行，目标 CRI 支持 AppArmor，API/loader 在原生产安全门禁下启动。
2. 真实 FUSE sandbox 创建、挂载和后端文件读写成功；验证 mounter 主进程及 s3fs 实际运行在预期 Localhost profile 的 enforce 模式下。
3. 使用独立临时测试资源触发无敏感数据的预期拒绝，取得对应内核策略拒绝证据；“profile 出现在列表”不能替代实际约束证据。测试失败不得放宽 profile。
4. 真实 API 普通/FUSE 创建、执行与正常销毁通过，并检查临时 Pod、策略、挂载、PVC/PV 无本轮遗留；确认节点和既有业务状态恢复。

构建通过、amd64 现场通过、arm64 现场通过、双架构生产发布资格分别记录。测试包及证据留在受控目录，公开报告仅输出架构、版本、摘要、状态和脱敏指标；不输出原始 host 配置、Secret、kubeconfig、私有 values 或未筛选日志。

## 文档、失败处理与范围外事项

实施阶段更新 `docs/deployment/apparmor-loader.md` 与 `docs/deployment/helm-deployment-upgrade.md`，补齐双架构构建、源码核验、签名、离线检查、节点维护和验收命令，并说明 Helm loader 只负责加载策略，不能代替宿主机 parser/CRI 整改。现有 Chart 和 values 不为此增加配置项，也不要求重新构建 API、bootstrap 或 loader 镜像。

依赖、ABI、内核、默认策略或真实 FUSE 检查失败就停止在对应阶段，记录可行动的失败条件；不得自动降级为无 AppArmor、同步存储或换 OS。维护中异常只按事先审查且在授权范围内的恢复方案处理，缺少方案则不进入服务重启阶段。

不选用不可取得的供应商 RPM；不直接复用 Debian parser 或旧 openEuler 打包 spec。SELinux 替代、内核重编译、主机 agent/安装 DaemonSet、所有节点自动分发及生产 rollout 都不在本设计中，必须分别评审授权。
