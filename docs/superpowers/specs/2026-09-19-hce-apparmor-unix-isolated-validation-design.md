# HCE AppArmor AF_UNIX 隔离验收设计

日期：2026-09-19。用户已同意优先在授权测试节点使用独立临时 profile 验证更严格的策略；本设计只覆盖该验证，不授权上线变更或扩大节点范围。

## 背景与决策

HCE 2.0 测试节点 `172.16.30.166` 的 AppArmor feature 快照有 `network_v8`，没有 `network/af_unix`。使用目标 feature 同时作为策略和内核 ABI 时，原 Helm profile 唯一严格编译告警来自 `network unix stream` 被降级为粗粒度规则。AppArmor [规则文档](https://www.apparmor.net/man/4.0/apparmor.d/)说明，粗粒度 network 与细粒度 UNIX socket 规则的能力不同；不能把产生二进制视为原约束已等价执行。

优先方案是在**仅用于隔离测试的候选 profile**中删除这一个 AF_UNIX 允许规则，验证 FUSE 工作链是否确实不需要它。这是权限收紧，但可能影响可用性，故必须先做真实工作负载测试。若失败，保留现有生产策略和严格告警门禁，再单独评审精确降级例外；不自动改为接受告警。内核 backport/升级是另一条路径，不属于本轮。

## 隔离边界

- 固定目标为获准的 CCE HCE 2.0 amd64 测试节点 `172.16.30.166`；重新核验节点 UID `69a79f2b-4f61-44c7-b46a-c46230c3530c` 与集群 kube-system namespace UID `cebeee78-9003-49bf-9442-3fea70515499`。不操作第二节点、`ds-ai-research`、生产或 arm64 节点。
- 候选 profile 从当前 Chart 文件逐字生成，只删除一整行 `network unix stream,`，并绑定独立测试名称与内容摘要。保持其它网络、文件、能力、mount、signal、ptrace 和执行继承规则完全不变；生成过程必须拒绝原文件与预期结构不符或出现多处替换。
- 不直接修改当前 Chart、values、业务 profile、业务池或已有 loader。候选测试的策略、工作负载、后端测试对象均有唯一任务标识；清理只针对已登记的这些临时对象，不使用宽泛标签或通配删除。
- 现有未签名 RPM 只能在原授权测试节点且单独获准的维护步骤中使用。先通过包审计、节点身份/健康及恢复准备；本设计不是立即安装 RPM、启用 CRI AppArmor 或重启 containerd 的授权。缺少维护入口、权限、容量、测试凭据或恢复条件即停在只读/离线阶段。

## 验证顺序与通过条件

1. 离线绑定：记录原 profile 与候选 profile 摘要，校验候选唯一差异、目标 feature 来源/架构/摘要及 parser 4.1.7 的真实 ELF 与 RPM 来源。使用 `-Q -K --config-file=/dev/null --Werror --warn=all`，分别将目标 feature 快照作为 `--policy-features` 和 `--kernel-features`。退出码为 0、stdout/stderr 均为空、二进制非空才允许进入现场阶段；不加载内核。
2. 现场前门禁：由管理员确认测试节点维护窗口、获准连接、RPM 完整审计、宿主机 parser 安装/冲突检查、运行中 CRI AppArmor 能力以及恢复方案。任何项未满足时，不通过特权 Pod 或宿主机根目录挂载绕过。
3. 独立临时 profile 与工作负载：只在获准节点加载唯一名称的候选 profile，部署临时 namespace/工作负载并明确指定该 profile；不覆盖同名业务 profile，不让调度器落到其它节点。用独立的非敏感 MinIO 测试前缀验证真实 FUSE 创建、挂载、读写、flush、卸载和销毁，并检查 mounter 与 s3fs 的 `/proc/*/attr/current` 均指向候选 profile 的 enforce 模式。
4. 预期拒绝：在同一候选 profile 下尝试 `AF_UNIX/SOCK_STREAM` 创建，必须失败且取得与候选 profile 对应的内核拒绝记录；同时确认该拒绝不是 seccomp、容器权限或测试程序本身造成。测试还要记录业务所需的 IPv4/IPv6 网络、FUSE 与存储操作是否成功。仅看 profile 列表、Pod Ready 或一次文件读取都不足以通过。
5. 收尾复核：精确清理临时 profile、namespace、工作负载、测试前缀及本轮挂载；核对没有临时 Pod、NetworkPolicy、挂载、PVC/PV 或异常进程遗留。原业务 profile、节点状态和既有业务池须与测试前一致。删除测试前缀前先确认其唯一任务标识，不能清理其它数据。

任一正向操作失败或拒绝证据不完整，结论为“候选不通过”，正式 Chart 不变。全部通过仍只说明该节点上“无需 AF_UNIX 允许规则”的候选可用；之后须独立复审正式策略变更及多架构/生产发布资格，不能把单节点结果外推为全环境通过。

## 记录与隐私

验收记录包括命令版本、目标身份、策略/feature/RPM 摘要、严格编译退出码和诊断字节数、正向操作结果、实际 enforce 身份、拒绝事件、销毁/清理结果和恢复状态。日志应脱敏；不写入堡垒机口令、kubeconfig、Secret、私有 values、测试后端密钥或原始主机配置。现场不可达时只报告已验证的离线结果和具体缺口，不伪造通过。
