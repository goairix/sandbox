# AppArmor 实施前环境门槛 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 本轮同会话内联执行，不默认派生子代理，不创建 worktree。

**Goal:** 明确真实 AppArmor 策略验证的可用环境，避免用未开启 AppArmor 的本地 Docker 证明生产加固有效。

**Architecture:** 本计划仅进行环境预检和证据记录，不加载 profile，不部署 DaemonSet。若本地环境不支持，先推进独立 Sentinel 实验；AppArmor 的策略/加载器实施须在获得明确隔离 Linux 环境后另行制定可执行计划。

**Tech Stack:** Docker、Linux AppArmor/securityfs、containerd、中文预检报告。

---

## 文件及权限边界

- 依据：`docs/superpowers/specs/2026-09-13-optional-apparmor-loader-design.md`。
- 不修改节点内核参数、现有策略或业务 Pod；不通过当前生产 namespace 临时加载安全策略。
- 仅创建 `docs/testing/2026-09-13-apparmor-environment-preflight.md`。
- 本计划不代替受限 profile、trusted private reader、准备/授权门禁、调度契约及 Chart 的生产实施计划。

### Task 1: 本地环境预检

- [x] 读取 Docker 能力并核对本地命令，当前输出为 linux/aarch64，仅 seccomp/cgroupns，没有 AppArmor：

```bash
docker info --format '{{.OSType}} {{.Architecture}} {{json .SecurityOptions}}'
command -v docker
command -v helm
command -v kubectl
command -v apparmor_parser
```

结论：本地 Docker 不能作为真实 AppArmor enforcement 证据。host macOS 上没有 parser 不能推断其它 Linux 节点也没有 AppArmor；不承诺 Docker Desktop/kind 等同原生节点验证。

### Task 2: 独立 Linux 环境的只读预检

- [ ] 获得明确隔离环境目标后，在该 Linux 宿主机读取以下信息，失败项记录原始错误；不自行扩大到生产集群节点。

```bash
uname -a
cat /sys/module/apparmor/parameters/enabled
cat /sys/kernel/security/lsm
command -v apparmor_parser
apparmor_parser --version
containerd --version
```

预期：AppArmor enabled=Y，LSM 列表包含 apparmor，可用 parser 和明确运行时版本。权限不足与内核不支持是不同结果；缺失 securityfs 不允许直接改挂载或启动参数。

- [ ] 若管理员明确提供 profile 状态读取权限，再执行只读检查；此处不授予加载器或 API 相同的宿主机权限。

```bash
sudo -n cat /sys/kernel/security/apparmor/profiles
```

预期：可读 enforce/complain 状态；`sudo -n` 失败则记录权限限制，不申请额外节点写权限来掩盖。

### Task 3: 环境结论和后续门槛

- [x] 用 apply_patch 写预检报告，准确区分本地证据、已提供隔离目标与尚缺权限。见 `docs/testing/2026-09-13-apparmor-environment-preflight.md`，独立 Linux 目标尚未提供。
- [ ] 只有隔离环境可用后才制定策略及加载器实施计划。必须包含 parser 语法、镜像固定路径、FUSE mount/flush/unmount、实际 mounter/子进程 exact profile、拒绝负例，以及 runtime 忽略 Localhost 时拒绝授权。
- [ ] 真实约束实验失败则保持功能不可按生产验收，不删除 privileged 挂载传播契约，不以 allow-all profile 或 unconfined 回退制造通过。
- [x] 读回报告、执行 `git diff --check` 后提交本轮预检记录；不部署、不卸载、不构建发布镜像。
