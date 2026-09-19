# HCE 2.0 AppArmor parser 构建与验收记录

日期：2026-09-18，现场更新：2026-09-19。本文分开记录工具检查、真实构建与节点验收；任一阶段通过不能代替下一阶段。

本轮工具检查点：`b757efd`（构建/审计）、`33ca454`（完整 Helm 策略及严格告警检查）、`9e5df3c`（显式提供相同默认策略 ABI）。这些提交不是生产就绪声明；实施计划中的目标内核能力检查、完整部署交接及现场验收仍未完成。

## 当前资格

| 项目 | 状态 | 证据 / 剩余前提 |
| --- | --- | --- |
| AppArmor 4.1.7 源码来源 | 已校验 | 官方 GitLab 归档 SHA-256、独立公钥指纹与 GPG 签名一致 |
| 本地构建工具契约 | 已运行 | HCE 宏与 spec 兼容修复后，25 项测试、shell 语法及差异格式检查通过 |
| 最小 ABI 修复的最终独立复审 | 未完成 | 最终复审服务不可用；本地测试及源码检查不能代替该复审 |
| 原有 Helm AppArmor 回归 | 已运行 | `scripts/test-helm-apparmor-loader.sh` 通过，未修改业务 Chart 策略 |
| 完整策略严格编译 | 未通过验收 | 非 HCE 的现有 parser 4.1.0 仍报告网络规则告警；真正候选 4.1.7/HCE 已构建，但尚未完成目标内核 feature 下的严格编译 |
| amd64 HCE 原生节点构建 | RPM/SRPM 已生成 | 获准测试节点 HCE 2.0 x86_64 上构建，AppArmor 4.1.7 上游 `%check` 完成；需补全构建产物哈希和独立审计 |
| 隔离 BuildKit HCE 构建与审计 | 未执行 | 尚无固定摘要的 HCE builder 镜像；原生节点构建不能替代独立容器审计 |
| amd64 节点 parser / CRI / FUSE enforce | 部分验证 | RPM 解包后的 parser 在目标 HCE 上报告 4.1.7，未安装 RPM、未修改 containerd、未做完整策略编译或 FUSE enforce 测试 |
| arm64 HCE 构建 | 未执行 | 尚无 HCE arm64 构建环境 |
| arm64 节点验收 | 未执行 | 尚无 HCE arm64 测试节点 |
| 双架构生产发布 | 不具备资格 | 两架构真实构建、签名发布和现场安全验收均未完成 |

补充回归已执行并通过：

```sh
bash scripts/test-helm-apparmor-loader.sh
go test -tags helmtests ./internal/helmtest -run AppArmor -count=1
go test ./internal/apparmorloader ./cmd/apparmor-loader -count=1
```

## 已核实源码

来源：[AppArmor 发布页](https://www.apparmor.net/about/releases/)、[4.1.7 签名页](https://gitlab.com/apparmor/apparmor/-/wikis/4.1.7_Signatures)。

- 归档：`apparmor-v4.1.7.tar.gz`。
- SHA-256：`ded4cd419b8a05002a108a0912208e2098695752664c6a59464d2dda418e0452`。
- 归档签名主密钥及签名密钥指纹：`3ECDCBA5FB34D254961CC53F6689E64E3D3664BB`。
- 签名归档中的提交：`9676b7aa934408748de2aaebad8680e69d7c3912`。工具当前将此字段标为未自动验证的元数据，构建信任依据仍是固定摘要和归档签名。

## 本地可执行检查发现的阻断项

除单元测试外，使用已有 loader 镜像内的真实 parser 4.1.0，以无网络、只读根文件系统、drop ALL capabilities 的临时容器检查完整 Helm 策略。没有加载内核策略。

采用 `-Q -K --config-file=/dev/null --Werror --warn=all` 和已验签源码中的 `features.all` 时，parser 因策略没有显式 ABI 声明而拒绝编译。AppArmor 4.1.7 的 `parser/parser_yacc.y` 同样在回退到 `default_features_abi` 时产生该告警，因此不能仅凭模拟测试通过就宣称构建可用。

最小修复从已验签源码的 `parser/default_features.c` 提取 `default_features_abi`，通过 `--policy-features` 显式提供同一默认值，并分别记录策略 ABI 与模拟内核能力的来源和摘要。完整策略保持不变、所有告警仍失败，没有使用 quiet、删除约束、较弱的 ABI 或 `--override-policy-abi`。该本地诊断不是 HCE 兼容性、4.1.7 实际构建或节点 enforce 证据。

进一步在同一现有 parser 4.1.0 上显式提供源码中相同的默认策略 ABI，严格检查仍报 `network rules not enforced`。单独将模拟内核能力替换为已签名源码的 `profiles/apparmor.d/abi/4.0` 后仍复现，因此不能把问题简单归结为旧测试能力文件。该替换仅存在于临时诊断进程，没有修改仓库中的能力文件选择或任何部署资源。

诊断用的原默认编译调用（非严格告警模式）虽然能生成二进制，但**不作为验收通过证据**。4.1.7 源码同时存在 network v8 规则生成和旧网络规则序列化路径；本地告警不能单独证明已部署节点上的网络约束失效。须在真正候选 parser 和目标内核能力下查清楚，不得过滤该告警、切换到任意策略 ABI 或放宽现有 Chart 来获得绿色结果。完整策略严格编译仍待通过。

## 2026-09-19 HCE amd64 现场构建

使用 JumpServer 资产连接令牌进入同一获准测试节点 `172.16.30.166`（主机名 `cce-sandbox-fuse-05598-1batu`）。读到 `/etc/os-release` 为 Huawei Cloud EulerOS 2.0 x86_64，内核 `5.10.0-182.0.0.95.r3450_282.hce2.x86_64`。`/sys/module/apparmor/parameters/enabled` 为 `Y`，FUSE 模块和对应 `kernel-devel` 均存在。没有访问其它节点或生产集群。

HCE base/updates 软件源没有 `apparmor` 或 `autoconf-archive` 包。管理员已授权在该测试节点构建；实际安装了 GCC、RPM build、Autotools、DejaGNU、Perl 测试工具等 HCE 包，以及 `binutils-extra` 中的 `/usr/bin/ld`。DNF 事务还升级了该节点的 ca-certificates、curl/libcurl 和 Perl 相关包；这些宿主机包变更需计入后续节点恢复/验收，不能声称节点完全未改。未触碰 kubelet/containerd 配置或服务。

源码归档 SHA-256 与锁定值一致。用与 AppArmor 官方 4.1.7 签名页一致的完整指纹核验公钥，GPG 验证归档签名成功。`keys.openpgp.org` 返回的无 UID 精简公钥无法由该节点的 GPG 导入；改从 Ubuntu keyserver 获取同一完整指纹的公开密钥再验证，未降低指纹检查要求。公钥、归档、构建输入和日志位于该节点 `/var/tmp/sandbox-apparmor-hce-amd64/`，没有写入仓库凭据。

现场发现并修复三处构建兼容点：HCE 没有 `autoconf-archive` RPM，故把唯一用到的 `AX_CHECK_COMPILE_FLAG` 宏固定为仓库输入并写入 SRPM；HCE 将链接器放在 `binutils-extra`；libapparmor 的 `autogen.sh` 需要显式 `ACLOCAL_PATH` 才扫描源码树 `m4` 目录。另外将占位的 1970 changelog 日期改为合法日期。这些修复未修改上游 AppArmor 源码的安全规则或项目 Helm profile。

第三次原生 `rpmbuild -ba` 生成：

```text
/var/tmp/sandbox-apparmor-hce-amd64/rpmbuild/RPMS/x86_64/sandbox-apparmor-parser-4.1.7-1.x86_64.rpm
/var/tmp/sandbox-apparmor-hce-amd64/rpmbuild/SRPMS/sandbox-apparmor-parser-4.1.7-1.src.rpm
```

现场 `rpm -qp` 确认名称/版本/架构；二进制包仅列出 `/usr/sbin/apparmor_parser` 及两个许可证文件，没有脚本输出；`rpm --checksig` 只报告 `digests OK`，因此仍是未签名测试包。解包后在该 HCE 宿主机运行的真实 ELF 报告 `AppArmor parser version 4.1.7`，`readelf` 为 x86-64，动态依赖仅 `libm.so.6`、`libc.so.6` 和动态加载器。由于宿主机没有 `/etc/apparmor/parser.conf`，未显式设置 `--config-file=/dev/null` 的版本命令在 stderr 发出配置警告；构建工具已改成显式指定空配置，后续需在现场复测这个严格零诊断门禁。

已经将完整当前 Chart profile 传到临时目录，并从目标内核复制到 29 个 feature 文件。首次手写 ABI 提取命令输出了空文件，**不能据此判断策略是否通过编译**。本地用仓库内的正式 `default_abi_bytes` 解析同一锁定归档，得到 1538 字节、SHA-256 `6e31eced95e2b7863cb8612a7090caf2a7362ebe41ff3b58300d5b3b46c027e3`；还需把该精确文件带回目标节点做严格编译。堡垒机连接令牌为一次性使用，终端断开后需要新令牌才能继续现场检查。

截至此记录，RPM 尚未安装到宿主机，containerd 未改变，项目 AppArmor profile 未加载到内核，真实 FUSE enforce/拒绝测试未执行。原生节点构建产物尚未完成完整 `hce_stage.audit` 审计，更不能用于生产发布。

## 维护入口检查（原记录）

通过私有 kubeconfig 的显式 `internal` context，只读确认目标仍为 `172.16.30.166`，节点 UID 为 `69a79f2b-4f61-44c7-b46a-c46230c3530c`，HCE 2.0、amd64。只读取了地址、身份和 OS/架构字段；没有修改 kubeconfig。

本地到节点网段有 VPN 路由，Kubernetes API 可访问，但到目标 SSH 22 端口连接超时，未到达认证阶段。凭据未写入文件、命令或仓库。需要确认 SSH 端口/堡垒机入口；不通过 privileged 安装 Pod 或挂载主机根目录替代维护通道。

## 后续现场记录要求

每个架构分别记录 builder digest、RPM/SRPM 摘要及签名状态、依赖检查、parser 版本、目标内核 features 来源/摘要、实际默认 CRI profile、项目 profile enforce、FUSE 挂载读写、预期拒绝、普通/FUSE 正常销毁、遗留资源数量和节点恢复状态。不得记录密码、私有 values、Secret、原始主机配置或未筛选日志。
