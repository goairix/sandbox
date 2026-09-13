# Portable Kubernetes Network Identity Implementation Plan

> **For agentic workers:** 按用户要求在当前目录顺序实施，使用 executing-plans 检查点；不创建 worktree。收尾按 requesting-code-review 技能做独立只读复审，不并行修改代码。

**Goal:** 修复实际 Pod 网段校验遗漏和领取引发的 CNI 身份变化，保持多副本安全及跨插件可移植性。

**Architecture:** 独立普通 Pod 业务注解视图与不可变安全标签；权威网段发现按能力组合，显式配置优先、缺失 fail closed。

**Tech Stack:** Go、client-go typed/dynamic clients、Kubernetes NetworkPolicy、CiliumNode、Calico IPPool、Helm。

### Task 1: 权威网段矩阵

Files: `internal/runtime/kubernetes/service_targets.go`、`network_range_authority_test.go`、新增 `portable_network_ranges_test.go`。

- [x] 添加 Node=10.244/CiliumNode=10.0 的实际 /32 必须拒绝测试；Calico IPPool=172.30 与 Node 不一致测试；缺失证据必须错误、显式 VPC 配置不 list inventory 测试。
- [x] 新的遗漏/缺证据场景经历红绿循环，Calico 用例也在实现前确认失败。
- [x] Runtime 的范围选项新增 Calico 能力和严格完整性；CiliumNode 每次发现均读取；Calico IPPool 有界分页读取全部 pool spec.cidr；通用严格路径 Node/ServiceCIDR 缺失返回配置指引。
- [x] 同组及全包测试通过；保留取消、分页、family 和 configured 快路径测试。

### Task 2: 稳定身份领取

Files: 新增 `ordinary_identity.go`、`ordinary_identity_test.go`；修改 `ordinary_network.go`、`runtime.go`、`runtime_test.go`。

- [x] 添加真实普通池领取后 `require.Equal(t, before.Labels, after.Labels)`、原始 policy selector 不变、业务视图 ID 已更新的失败测试。
- [x] UID/resourceVersion CAS patch 仅写 claim ID/UID 注解；丢响应读回只认可相同 UID、ID 和完整标签快照，其他 owner 不重入。
- [x] prepared publication 使用 UID 绑定注解，runtime List 按有效业务视图过滤；claimed Pod 不出现在普通池 inventory，仍能按稳定身份清理策略。
- [x] 调整原迁移测试为新契约，保留所有失败注入覆盖，不删除错误路径测试。
- [x] Kubernetes 全包和 race 通过。补充版本号在网络就绪等待中变化、恢复不得跳过 prepared 发布的红绿回归。
- [x] 独立复审整改：exact OrdinaryPoolClaimer 将 acquired UID 与领取/失败清理绑定；不新增业务 ID claim 对象，最终唯一性由共享 active repository NX 发布保证。

### Task 3: 部署与性能边界

Files: `deploy/helm/sandbox/templates/runtime-network-inventory.yaml`、`values.yaml`、`configs/config.yaml`、相关 Kubernetes 文档。

- [x] 增加 Calico IPPool 只读 list 权限，检测能力时不把 CRD 存在描述为插件确实启用；说明未知 VPC 显式配置和策略执行先决条件。Cilium Endpoint get 仅在 runtime namespace。
- [x] 更新 warm-pool contract，旧预备池不误混入新安全模板。稳态 refill 按已记录 runtime 检查，不扫描活动业务沙盒。
- [x] 增强部署测试：在不同副本创建/更新网络/实际 TCP 检查，不增加固定长 sleep 或放宽断言。UID/物理标签及清理审计仍需部署后外部核对。
- [x] 独立复审整改：新增 networkPolicyProvider（auto/standard/cilium），配置/环境变量/API及 drain hook 同步，显式 standard 不等待遗留 API 对应的 Endpoint；保留旧自有 CNP 清理，缺失可选资源 404 不阻塞。

### Task 4: 全量验证与部署复测交接

- [x] 使用两个隔离临时 Redis，各套件内 -p 1 顺序运行全量与 race；build、vet、本次改动 lint（0 issues）、两组 Helm 脚本、diff 检查通过。全仓 lint 仍有历史问题，不作为全部 lint 通过。
- [x] 自审及独立只读复审完成，两个 Important 已整改并复核，未发现剩余 Critical/Important；不把 opt-in 未接入的真实部署测试当现场 pass。
- [x] 写中文整改结果，明确线上新镜像验证需用户构建部署；不替用户构建推送或 upgrade。
- [ ] 用户部署后复测三副本 API、Service 网络与完整生命周期，再做清理审计和性能采样。
- [ ] Git 提交由用户明确请求后执行。
