# HCE 2.0 AppArmor parser 构建与验收记录

日期：2026-09-18，现场更新：2026-09-19。本文分开记录工具检查、真实构建与节点验收；任一阶段通过不能代替下一阶段。

本轮工具检查点：`b757efd`（构建/审计）、`33ca454`（完整 Helm 策略及严格告警检查）、`9e5df3c`（显式提供相同默认策略 ABI）、`6c4018f`（HCE 原生构建兼容修复）。这些提交不是生产就绪声明；实施计划中的完整部署交接及现场验收仍未完成。

## 当前资格

| 项目 | 状态 | 证据 / 剩余前提 |
| --- | --- | --- |
| AppArmor 4.1.7 源码来源 | 已校验 | 官方 GitLab 归档 SHA-256、独立公钥指纹与 GPG 签名一致 |
| 本地构建工具契约 | 已运行 | HCE 宏与 spec 兼容修复后，25 项测试、shell 语法及差异格式检查通过 |
| 最小 ABI 修复的最终独立复审 | 未完成 | 最终复审服务不可用；本地测试及源码检查不能代替该复审 |
| 原有 Helm AppArmor 回归 | 已运行 | `scripts/test-helm-apparmor-loader.sh` 通过，未修改业务 Chart 策略 |
| 完整策略严格编译 | 未通过验收 | 真正候选 parser 4.1.7 在目标 HCE 内核 feature 下复现：默认策略 ABI 无 `network_v8`，改用目标 ABI 后，`network unix stream` 因内核缺少扩展 AF_UNIX 能力触发降级告警；未放宽严格告警门禁 |
| amd64 HCE 原生节点构建 | RPM/SRPM 已生成 | 获准测试节点 HCE 2.0 x86_64 上构建，AppArmor 4.1.7 上游 `%check` 完成；产物哈希已记录，仍需独立容器审计 |
| 隔离 BuildKit HCE 构建与审计 | 未执行 | 尚无固定摘要的 HCE builder 镜像；原生节点构建不能替代独立容器审计 |
| amd64 节点 parser / CRI / FUSE enforce | 部分验证 | RPM 解包后的 parser 在目标 HCE 上报告 4.1.7、零诊断；完整策略严格编译失败。未安装 RPM、未修改 containerd、未做 FUSE enforce 测试 |
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

历史上采用 `-Q -K --config-file=/dev/null --Werror --warn=all` 和已验签源码中的 `features.all` 时，parser 因策略没有显式 ABI 声明而拒绝编译。AppArmor 4.1.7 的 `parser/parser_yacc.y` 同样在回退到 `default_features_abi` 时产生该告警，因此不能仅凭旧模拟测试通过就宣称构建可用。

最小修复从已验签源码的 `parser/default_features.c` 提取 `default_features_abi`，通过 `--policy-features` 显式提供同一默认值，并分别记录策略 ABI 与模拟内核能力的来源和摘要。完整策略保持不变、所有告警仍失败，没有使用 quiet、删除约束、较弱的 ABI 或 `--override-policy-abi`。该本地诊断不是 HCE 兼容性、4.1.7 实际构建或节点 enforce 证据。

进一步在同一现有 parser 4.1.0 上显式提供源码中相同的默认策略 ABI，严格检查仍报 `network rules not enforced`。单独将模拟内核能力替换为已签名源码的 `profiles/apparmor.d/abi/4.0` 后仍复现，因此不能把问题简单归结为旧测试能力文件。该替换仅存在于临时诊断进程，没有修改仓库中的能力文件选择或任何部署资源。

诊断用的原默认编译调用（非严格告警模式）虽然能生成二进制，但**不作为验收通过证据**。4.1.7 源码同时存在 network v8 规则生成和旧网络规则序列化路径；本地告警不能单独证明已部署节点上的网络约束失效。后续在真正候选 parser 和目标内核能力下查出的具体差异见下文；不得过滤告警或放宽现有 Chart 来获得绿色结果。

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

现场 `rpm -qp` 确认名称/版本/架构；二进制包仅列出 `/usr/sbin/apparmor_parser` 及两个许可证文件，没有脚本输出；`rpm --checksig` 只报告 `digests OK`，因此仍是未签名测试包。解包后在该 HCE 宿主机运行的真实 ELF 报告 `AppArmor parser version 4.1.7`，`readelf` 为 x86-64，动态依赖仅 `libm.so.6`、`libc.so.6` 和动态加载器。显式传入 `--config-file=/dev/null --version` 后，现场退出码为 0、stderr 为 0 字节；`ldd -r` 无未解析符号。

完整当前 Chart profile 已传到临时目录，并从目标内核复制了 29 个 feature 文件。首次手写 ABI 提取命令输出了空文件，已用仓库内的正式 `default_abi_bytes` 从同一锁定归档重新提取：1538 字节、SHA-256 `6e31eced95e2b7863cb8612a7090caf2a7362ebe41ff3b58300d5b3b46c027e3`。该精确文件已在节点核对，不再使用空文件。

本次补齐的原生构建产物摘要：

| 产物 | SHA-256 |
| --- | --- |
| `sandbox-apparmor-parser-4.1.7-1.x86_64.rpm` | `766cb1ac45bd13e5643c2dd380c326ca2c6a111275bdd2937d2550baefa58ee3` |
| `sandbox-apparmor-parser-4.1.7-1.src.rpm` | `6092bc2c56f80956049862101a14e2500ddb86905c6559288599fd0764554c8a` |
| 完整 Chart profile | `962a729d73297c06ea9599b0766326f2ceb182f2707661960bf2a264c5dd5fcc` |

`rpmbuild.status` 为 0。目标 kernel feature 快照来自 `/sys/kernel/security/apparmor/features`，在复制后的 `features/` 目录中按相对路径排序，对各文件执行 `sha256sum` 后再汇总 `sha256sum`，所得摘要为 `f3f022275edb31107e7aa97267ba4f9aa544b1e0f6cd40fc8fe3a4ac5c977c8c`。这不是单一原始文件的哈希，复核时必须使用相同算法。

### 目标 HCE 能力下的严格编译诊断

使用解包后的真实 4.1.7 ELF，对未改动的完整 Chart profile 执行 `-Q -K --config-file=/dev/null --Werror --warn=all`，只输出测试二进制，不加载或更新内核策略：

1. `--policy-features` 为已验签源码的默认策略 ABI、`--kernel-features` 为目标内核快照：退出码 1，`network rules not enforced`。默认 ABI 只有旧 `network {af_unix ...}`，目标内核只有 `network_v8/af_mask`，4.1.7 `parser_main.c` 使用两者交集判断支持，因此两条路径都不满足。
2. 仅作定位，把策略 ABI 也设为目标内核快照；其余参数与 profile 不变：退出码 1，`downgrading extended network unix socket rule to generic network rule`。目标内核有 `network_v8`，但没有 `network/af_unix`；`parser_yacc.y` 会为 `network unix stream` 合成 `unix_rule`，`af_unix.cc` 因扩展 AF_UNIX 能力缺失而发出降级告警。
3. 进一步的隔离诊断通过进程替换从输入流排除 `network unix stream`，不修改 Chart 或节点策略：严格编译退出码 0、stderr 0 字节。此步骤只定位告警来源；删除该限制会改变安全边界，**不是修复方案**。
4. 保留完整 profile 和目标 ABI、仅临时取消 `--Werror` 时，可生成 9681 字节二进制，但仍输出上述降级告警。该二进制**不能作为验收通过产物**，没有加载到内核。

已修复隔离构建流程的用户态编译输入：现在使用已验签归档中的正式 `profiles/apparmor.d/abi/4.0`（SHA-256 `e510bb8f6788b45e48de2f859a6f94a7b8416cbac5a1051814cfce925fa911bd`）同时作为策略 ABI 和模拟内核特性。现场同一原生 4.1.7 parser 对完整策略严格编译退出 0，stdout/stderr 均为 0 字节，生成 11137 字节二进制。该门禁只证明候选 parser 在固定用户态 ABI 下可编译，**不是**目标 HCE 内核兼容证据；目标内核仍需单独使用 feature 快照验收。

当前结论是 parser 包构建和本机运行已经验证，但这台 HCE 内核的 AppArmor feature 集合与当前严格策略验收要求不匹配。不能通过静默告警、删除 `network unix stream` 或任意伪造 ABI 宣称完成。后续需对“仅 family/type 的 generic network_v8 回退”进行明确安全设计和受控拒绝测试，或取得具有所需扩展 AF_UNIX 能力的内核；在此之前，AppArmor 的目标节点 enforce 验收保持阻断。

截至此记录，RPM 尚未安装到宿主机，containerd 未改变，真实 FUSE enforce/拒绝测试未执行。节点 `/sys/kernel/security/apparmor/profiles` 中已存在同名 `sandbox-fuse-*` enforce profile，其来源不属于本轮操作，本轮未加载、替换或移除任何策略，也不能据此推断本次候选 RPM 通过测试。原生节点构建产物尚未完成完整 `hce_stage.audit` 审计，更不能用于生产发布。

## 维护入口检查（原记录）

通过私有 kubeconfig 的显式 `internal` context，只读确认目标仍为 `172.16.30.166`，节点 UID 为 `69a79f2b-4f61-44c7-b46a-c46230c3530c`，HCE 2.0、amd64。只读取了地址、身份和 OS/架构字段；没有修改 kubeconfig。

本地到节点网段有 VPN 路由，Kubernetes API 可访问，但到目标 SSH 22 端口连接超时，未到达认证阶段。凭据未写入文件、命令或仓库。需要确认 SSH 端口/堡垒机入口；不通过 privileged 安装 Pod 或挂载主机根目录替代维护通道。

## 后续现场记录要求

每个架构分别记录 builder digest、RPM/SRPM 摘要及签名状态、依赖检查、parser 版本、目标内核 features 来源/摘要、实际默认 CRI profile、项目 profile enforce、FUSE 挂载读写、预期拒绝、普通/FUSE 正常销毁、遗留资源数量和节点恢复状态。不得记录密码、私有 values、Secret、原始主机配置或未筛选日志。
