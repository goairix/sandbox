# Enrolled Kubernetes Profile Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 普通 enrolled Pod 的生产 builder、正式配置 provisioning 与 observer/cleanup 共用同一严格 profile。

**Architecture:** 一个完整可审查 Task。共用原 v2 config decoder、显式新 profile 和固定 CLI provisioning mode，真实 mount/PID1 fixture 验证消费者接线；不切 main 或旧 createPod。

**Tech Stack:** 现有 Go1.25.6、Kubernetes v1 客户端、Linux UDS/原 launcher；不加依赖。

**Spec:** docs/superpowers/specs/2026-10-16-enrolled-kubernetes-profile-design.md

## Global Constraints

- canonical v2 config≤16384、固定路径/模式、无 secret/fallback/任意 signer；共用原校验，不降低 caps/UUID/trust/kernel/EOF/clock 检查。
- 新 profile sandbox-enrolled-plain/v1，exact NodeName/UID/BootID、intent/generation/config/image digest；单 Never 容器，无 init/sidecar，五个固定 volumes，两 readonly host sockets/source，control Memory EmptyDir≤1MiB。
- PID1/root provisioning 只收紧原目录并 O_EXCL 新文件，不覆盖/删除旧出生数据；之后执行原 serve/BootstrapPID1。
- 旧 profile 拒绝不放宽；共享校验仅结构诊断，不是 native authority/健康/终止/owner释放；不改 main/旧 createPod 默认分支。
- 无按已有 N 的 map/timer/扫描；不读 CLOSED OWN、不碰 Sentinel、不取真实Redis/Kube/secret/fullinspect、不升级/push/deploy；只 apply_patch 写源码/测试/文档/格式。
- 每作者≤1/总≤2原批准私有512MiB/2CPU/256PID fixture，原 cap/隔离不扩张；原 Wait/Close/join/exactcleanup、全17字段实际GoList匹配环境前后 filename/SHA、ELF/source绑定。
- Git仅 Root expectedHEAD/exactpaths common→OPEN OWN fcntl锁窗口，小步验证提交；一个 fresh author/一个 fresh wholeTaskreview，同pair scoped修复，不重放已验收矩阵。

## Review Focus

1. 自称新 profile 不能绕过旧 strict 安全检查或引入可写 hostPath/任意 mount；builder 与两消费者必须真的消费同一校验。
2. config 公钥/节点身份/authority/命令镜像绑定不一致拒绝，JSON secret/重复/null/超界不成为启动配置。
3. provisioning 对已有文件、symlink、错误 owner/mode、写入中断保持失败，不覆盖旧 runtime 或调用弱 constructor。
4. readonly Unix socket mount 必须保留真实 SO_PEERCRED，不以普通 TCP proxy、注入成功 bool 或 fake kernel 代替。
5. fixture 原 Wait/serve/handler join 和精确 cleanup 不能由 context取消/diagnostic 缺失代替；真实 kubelet 部署限制如实记录。

---

### Task 1: Strict managed Pod profile and real provisioning entrypoint

**Create:** `internal/runtime/launcherconfig/config.go`, `config_test.go`；`internal/kubecontract/enrolled_plain.go`, `enrolled_plain_test.go`（按职责可拆 config/security/volumes 文件）；`internal/runtime/kubernetes/enrolled_plain_pod.go`, `enrolled_plain_pod_test.go`；`cmd/sandbox-launcher/provision.go`, `provision_linux.go`, `provision_unsupported.go`, `provision_test.go`；`internal/runtime/controlrunner/enrolled_profile_native_linux_test.go`；`scripts/test-enrolled-profile.sh`。

**Modify narrowly:** `cmd/sandbox-launcher/protected_config.go` 迁移 decoder为共用wrapper/薄调用、`main.go` 固定 mode分派；`internal/nodeobserver/kubernetes_guard.go` 新profile分支及临时providerRegistration.Binding，`enrollment.go`/`termination.go` 两处仅从原已验证activation.Binding赋值，不改persistedproviderObservation/journalwire；`internal/runtime/kubernetes/task_runtime_cleanup_profile.go` 新profile分支；对应两个消费者新增有限新测试文件，不重写已验收矩阵。其余旧 producer/validator 接点需要先报Root具体矛盾。

**Interfaces:** `launcherconfig.Decode([]byte) (Config,error)` 精确保留原v2字段顺序与规则；`kubecontract.EnrolledPlainProfile = "sandbox-enrolled-plain/v1"` 与 `ValidateEnrolledPlainPod(*corev1.Pod) error`，严格解析 config并核对profilemetadata，不授权；`kubernetes.EnrolledPlainPodRequest{Spec runtime.SandboxSpec, NodeName,NodeUID,NodeBootID,CreationIntentID string, Generation int64, Config []byte}` 与 `BuildEnrolledPlainPod(namespace string, request EnrolledPlainPodRequest) (*corev1.Pod,error)`。固定CLI `serve-provisioned` 从 SANDBOX_LAUNCHER_CONFIG_V2 消费公开配置并移除变量，完成固定受保护文件后调用原serve。

**Shared extraction:** `SelectEnrolledPlainPodProfile(*corev1.Pod)(bool,error)` 中央拒绝unknown或缺marker但有reserved迹象，两消费者无条件先调用；`InspectEnrolledPlainPod(*corev1.Pod)(EnrolledPlainPodIdentity,error)` 返回copy诊断DTO Config/LogicalID/CreationIntentID/NodeUID/NodeBootID/Generation，Validate薄调用。两个消费者再比对原verifiedactivation或cleanupbinding完整tuple。spec列明7受控annotation；PidLimit非零拒绝；cmd definedwrapper保持跨包私有方法而不复制decoder。

**Fixed shape:** `kubecontract.EnrolledPlainPodSpec(nodeName,image,config string, resources corev1.ResourceRequirements, volumes []corev1.Volume) corev1.PodSpec` 统一builder/validator固定模板，fresh结构且不修改caller对象；动态项仍独立检查。加入原普通Pod两个NoExecute300s；只在无PriorityClassName时校验copy允许Priority nil/0、PreemptionPolicy nil/PreemptLowerPriority，其他值拒绝，原Pod/hash不变。Step1新增唯一TestEnrolledProfileAdmissionDefaults先RED→GREEN/scopedrace/affectedvet，不重放其他已GREEN选择器。HostUsers ptrtrue暂保留部署限制；用户Env原monitor白名单隔离不变。

- [ ] Step1 读取实际 v2 config/原 Pod/两个消费者，写最小 config共享与profile RED；报告精确矛盾，不扩张原authority。迁移 config、实现共享严格profile及纯builder，有限host/race/vet通过后小提交。
- [ ] Step2 正式 provisioning mode 与真实 FD/NOFOLLOW/O_EXCL/root/mode/stale 检查；两个消费者新显式 profile分支，保持旧分支不变。对受控关键字段有限negative和原拒绝条件完成host/race与Linuxcompile/vet，及时小提交。
- [ ] Step3 新Linux fixture exec正式serve-provisioned PID1，真实readonly socket mounts/初始EmptyDir，原observer签名enrollment/ready认证；同真实GET Pod profile由cleanup Prepare接受。有限stale/foreign-entry拒绝和原Wait/join/cleanup、actualall17源码ELF绑定完成，小提交。
- [ ] Step4 冻结原始证据/报告，Root唯一fresh完整review，同pair修复、tracked验收并继续创建/发布生产编排。

## Root preflight and rulings

| Pair/task | Producer / consumer | Finding/ruling |
| --- | --- | --- |
| Step1 → Step2 | 共用config/profile → CLI/observer/cleanup | 单一decoder/validator；不得复制不同caps或仅凭annotation放行。 |
| Step2 → Step3 | 固定 mount/目录合同 → 实际PID1 | native必须使用生产builder所产profile，不能继续旧无mount弱fixture。 |
| Task1内部 | spec volume描述 → 校验 | 实际是五个volume，不是四个；workspace,tmp,control,clock,enrollment，精确校验全部五个。 |
| Task1 → 当前CLI Task | sharedconfig迁移 → 先前accepted源码 | 当前CLI完整review与tracked验收完成前，不写任何产品Go源/测试；先只读准备。新Task不得改写其历史证据。 |
| Task1 → 后续driver | 纯builder/provisioning → 创建/Inspect/bootstrap/Publish | 本Task不声称真实kubelet/main/创建未知仲裁；后续必须exactNode/PodUID重验。 |

Ruling: 复用已有功能分支和用户master持续执行授权；本次使用 brainstorming/writing-plans 细化已批准接口，不重新停下审批。保留失败关闭和旧profile拒绝边界，真实kubelet兼容性只能后续实测确认。
