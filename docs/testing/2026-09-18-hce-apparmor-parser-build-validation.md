# HCE 2.0 AppArmor parser 构建与验收记录

日期：2026-09-18。本文分开记录工具检查、真实构建与节点验收；任一阶段通过不能代替下一阶段。

本轮工具检查点：`b757efd`（构建/审计）、`33ca454`（完整 Helm 策略及严格告警检查）、`9e5df3c`（显式提供相同默认策略 ABI）。这些提交不是生产就绪声明；实施计划中的目标内核能力检查、完整部署交接及现场验收仍未完成。

## 当前资格

| 项目 | 状态 | 证据 / 剩余前提 |
| --- | --- | --- |
| AppArmor 4.1.7 源码来源 | 已校验 | 官方 GitLab 归档 SHA-256、独立公钥指纹与 GPG 签名一致 |
| 本地构建工具契约 | 已运行 | 增补精确默认 ABI 检查后，24 项测试、shell 语法及差异格式检查通过；不代表真实 RPM 已构建 |
| 最小 ABI 修复的最终独立复审 | 未完成 | 最终复审服务不可用；本地测试及源码检查不能代替该复审 |
| 原有 Helm AppArmor 回归 | 已运行 | `scripts/test-helm-apparmor-loader.sh` 通过，未修改业务 Chart 策略 |
| 完整策略严格编译 | 未通过验收 | 非 HCE 的现有 parser 4.1.0 仍报告网络规则告警；真正候选 4.1.7/HCE 尚未构建 |
| amd64 HCE 构建 | 未执行 | 尚未取得可用 HCE builder；获准测试节点 SSH 连接超时 |
| amd64 节点 parser / CRI / FUSE enforce | 未执行 | 尚未安装 RPM、修改 containerd 配置或重启服务 |
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

## 维护入口检查

通过私有 kubeconfig 的显式 `internal` context，只读确认目标仍为 `172.16.30.166`，节点 UID 为 `69a79f2b-4f61-44c7-b46a-c46230c3530c`，HCE 2.0、amd64。只读取了地址、身份和 OS/架构字段；没有修改 kubeconfig。

本地到节点网段有 VPN 路由，Kubernetes API 可访问，但到目标 SSH 22 端口连接超时，未到达认证阶段。凭据未写入文件、命令或仓库。需要确认 SSH 端口/堡垒机入口；不通过 privileged 安装 Pod 或挂载主机根目录替代维护通道。

## 后续现场记录要求

每个架构分别记录 builder digest、RPM/SRPM 摘要及签名状态、依赖检查、parser 版本、目标内核 features 来源/摘要、实际默认 CRI profile、项目 profile enforce、FUSE 挂载读写、预期拒绝、普通/FUSE 正常销毁、遗留资源数量和节点恢复状态。不得记录密码、私有 values、Secret、原始主机配置或未筛选日志。
