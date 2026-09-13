# 可选 AppArmor Helm 集成实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将已验证的加载器核心及私有 enforcement 检查接入可选 Chart 配置，不改变默认调度集合。

**Architecture:** 一个 helper 对自包含策略模板执行 CRLF→LF、trim、final LF、SHA-256，生成不可变名称；API/drain/ConfigMap/DS/fingerprint 共用有效名称与有效节点选择器。独立 loader SA 无 token，API 仅新增 exact DS get；跨运行时命名空间时额外读取 release 命名空间的 Pod。

**Tech Stack:** Helm、Go 策略验证、Python YAML Chart 回归、AppArmor 语法。

---

### Task 1: Chart 回归

**Files:** Create `scripts/test-helm-apparmor-loader.sh`, `internal/apparmorloader/chart_policy_test.go`.

- [x] 渲染默认配置断言无 loader DS/CM；开启配置检查 DS、CM、SA、Role，断言 API 与 drain 有效 LSM/selector 一致、env 唯一、profile digest 与规范模板 SHA-256 一致、仅 securityfs+enabled-file hostPath、无 token/hostPID/hostNetwork/unload hook、跨 namespace Pod 只读。
- [x] Run `bash scripts/test-helm-apparmor-loader.sh`，预期 DS 不存在 RED。
- [x] Go 测试读取 `../../deploy/helm/sandbox/files/apparmor/workspace-mounter.profile`，canonical SHA-256 替换唯一 placeholder 后调用 ValidatePolicy，确保真实模板符合核心验证。

### Task 2: 有效配置及可信节点组件

**Files:** Modify `deploy/helm/sandbox/values.yaml`, `templates/_helpers.tpl`, `templates/deployment.yaml`; Create `files/apparmor/workspace-mounter.profile`, `templates/apparmor-loader.yaml`.

- [x] values 增加 `apparmorLoader.enabled=false`，image repository/tag/pullPolicy、resources、checkIntervalSeconds=10/parserTimeoutSeconds=10/startupTimeoutSeconds=180；runtime.kubernetes.nodeSelector 默认 `{}`。
- [x] helper `sandbox.apparmor.digest` 使用 `.Files.Get "files/apparmor/workspace-mounter.profile" | replace "\r\n" "\n" | trim` 后 `printf "%s\n"`→sha256sum；`sandbox.effectiveLSMProfile` 开启返回 `printf "sandbox-fuse-%s" digest`，关闭返回手工字段；`sandbox.effectiveNodeSelector` deepcopy map，开启合入 Linux 并拒绝冲突，输出 JSON。开启验证 FUSE/kubernetes/kindfalse/image/正数时间。
- [x] API 与 drain 的 LSM 改为 helper；非空 selector env 注入 JSON，API 开启注入 NAME/NAMESPACE/TIMEOUT_SECONDS；backend contract 增加有效 LSM 及 selector。
- [x] loader DS：同源 nodeSelector，独立无 token SA，privileged root/根只读，profile CM 只读、securityfs RW、module enabled 文件只读、run emptyDir；固定 args 包含 profile name/digest 及时间；readiness 运行 loader readiness subcommand max-age=3×interval。DS/current PodTemplate 标注 `sandbox.apparmor.profile-digest`，模板同时 label。不使用 hostPID/hostNetwork/Node 权限/卸载策略。
- [x] API Role release namespace exact daemonsets get，resourceNames release-loader；运行时 namespace 不同时另加 pods get/list。同 SA RoleBinding。无集群权限。
- [x] 策略仅固定可信 binaries `rix`、库 `mr`、必要只读配置/proc/随机源、三类受限写目录、必要 FUSE capability 与目标 workspace mount/umount。无 include、ux/Px/任意文件写或通配 mount；继承后端 network 仍受 Kubernetes 网络策略限制。
- [x] Run 新脚本及 `go test ./internal/apparmorloader -count=1`，预期 PASS；运行既有 Chart/backend-switch/auth 脚本，预期 PASS。

### Task 3: 接入复审与发布边界

**Files:** Create `docs/deployment/apparmor-loader.md`（中文部署和构建说明）；更新本计划测试记录。

- [x] 文档说明 loader 镜像单独构建、API 重建、mounter/probe 不重建；生产启用须通过真实 kernel enforce、mount/flush/unmount 和越权负例；PSA privileged/hostPath 管理员要求，退出不卸载旧 profile，非支持 runtime fail-closed。
- 该项实现及复审已完成；由父任务随本轮验证后的代码批次统一提交，不单独部署或构建镜像。

## 自审

Chart 可选集成可独立验证，但策略属于待真实挂载验收的候选，不把 parser/core/Chart 成功描述为生产 enforcement 通过。新增有效 LSM/selector 会进入 backend fingerprint，安全契约变更触发既有 drain 路径。

## 复审与最终验证

补充可选 `apparmorLoader.priorityClassName`，默认空，有 globalDefault 的集群须显式填写既有可信类；Chart schema 拒绝非字符串/非法 DNS/过长 label，不授予系统级优先级。脚本先因字段尚不存在 RED，再接入并 GREEN，运行时同名类 normalization 由门禁计划覆盖。

标签 64 位错误、遗漏 sync 执行、类型 coercion、Linux 合并后 65 项均有 RED/GREEN 记录。主任务最终全量 Go/race/vet、增量 lint 及新旧 Helm 脚本通过；parser 语法检查通过但未加载内核。完整结果与未完成能力见 `docs/testing/2026-09-13-apparmor-sentinel-implementation-review.md`。
