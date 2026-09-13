# AppArmor 私有运行时约束检查实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 可选检查在 FUSE prepared 健康及授权前证明 exact mounter PID 1 处于预期 enforce profile，失败不传递凭据。

**Architecture:** Kubernetes 私有控制通道只额外允许固定双参数 cat 且禁止 stdin；不修改公共 fuseprotocol 语法。检查前后 get exact UID，校验 native sidecar 固定命令、根只读/零重启和显式 Localhost profile，防止读取期间发生替换或重启。默认关闭，完整启动门禁和 Chart 接入属于下一独立计划。

**Tech Stack:** Go、client-go fake、现有 bounded SPDY executor。

---

### Task 1: 私有读取语法

**Files:** Modify `internal/runtime/kubernetes/control.go`; Create `internal/runtime/kubernetes/apparmor_test.go`.

- [x] 测试 `allowedMounterCommand([]string{"/bin/cat", "/proc/1/attr/current"})` 为 true，而 fuseprotocol.AllowedMounterCommand 相同输入为 false；额外路径、参数、shell 都为 false。execControl 对该命令任何非空 stdin 都返回 ErrInvalidControlCommand。
- [x] Run `go test ./internal/runtime/kubernetes -run TestAppArmorPrivate -count=1`，预期固定读取拒绝 RED。
- [x] 实现 `isAppArmorReadCommand(argv []string) bool { return len(argv)==2 && argv[0]=="/bin/cat" && argv[1]=="/proc/1/attr/current" }`，Kubernetes allowed helper 特判它；execControl 拒绝该命令非空 stdin，公共协议不变。
- [x] 同命令预期 PASS。

### Task 2: 入池与授权检查

**Files:** Create `internal/runtime/kubernetes/apparmor.go`; Modify `internal/runtime/kubernetes/runtime.go`; Test `internal/runtime/kubernetes/apparmor_test.go`.

- [x] 对已准备 exact FUSE Pod 打开 `WithAppArmorEnforcement("sandbox-fuse-"+strings.Repeat("a",64))`，更新其 Localhost 注解/字段，控制脚本分别返回 exact enforce、unconfined、complain、不同名称、超长输出、读取错误，以及读取期间 UID/RestartCount 变化。健康及授权只能接受 exact enforce；失败时脚本不得见 authorize 或凭据 stdin。
- [x] Run `go test ./internal/runtime/kubernetes -run TestAppArmor -count=1`，预期缺少 option/guard RED。
- [x] option 保存预期 profile；非空开启时 WarmPoolContract 附加 `:apparmor=<profile>`。检查仅接受 `sandbox-fuse-<64 lowercase hex>`，在 5 秒 context 下读取，输出最多 512 bytes，`strings.TrimSpace(raw)==profile+" (enforce)"`。先后 getExactPod 并验证 `exactFUSEPodBootstrap`、`validatePreparedContainerState`、单个 native mounter command/restart/security profile 及固定安全设置；错误只输出脱敏边界说明。
- [x] PreparedSandboxHealth 静态验证后调用 guard，AuthorizeWorkspaceMount 在构造凭据 request 前调用 guard；PrepareSandbox 非空启用时拒绝 spec LSMProfile 不匹配，保持原有补偿删除和 UID fencing。
- [x] 同聚焦命令预期 PASS；执行 `go test ./internal/runtime/kubernetes ./internal/fuseprotocol -count=1`、`go test -race ./internal/runtime/kubernetes -count=1`，预期 PASS。
- 该项实现及复审已完成；由父任务随本轮验证后的代码批次统一提交，不单独部署或构建镜像。

## 自审范围

本计划只实现不可绕过的运行时边界，不取代真实内核拒绝测试。加载器启用的配置入口尚由后续启动门禁提供；默认配置不额外产生 Kubernetes Exec。读取权限不进入公共 mounter grammar。
