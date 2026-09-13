# AppArmor 实施前环境预检

## 范围与结果

本次按已确认规格进行只读预检，不加载策略、不安装 DaemonSet、不修改现有业务 Kubernetes 工作负载。用户随后允许选择现有节点，已在 ds-ai-research 的 ds-ai-worker-2 使用独立 namespace 完成节点预检。自定义策略的真实约束实验仍没有执行，不记为通过。

本地 Docker 输出：

```text
linux aarch64 ["name=seccomp,profile=builtin","name=cgroupns"]
```

`docker`、`helm`、`kubectl` 可用，macOS 宿主机没有 `apparmor_parser`。Docker SecurityOptions 未报告 AppArmor，本轮不能将该环境用于证明 Localhost profile 的真实 enforcement；这不是其它 Linux 节点不支持 AppArmor 的证据。

## Kubernetes 节点只读预检

- context：ds-ai-research；节点：ds-ai-worker-2，Ready、无 taint。
- Kubernetes v1.33.6，Debian 13，内核 6.12.57+deb13-arm64，nodeInfo 报告 containerd 2.2.0。
- 独立 namespace：`sandbox-apparmor-preflight-b1b4696`，UID `d918435b-746e-4a1a-bed2-6266984ecf4a`。
- Pod：`node-security-reader`，UID `7c40dd50-904c-462e-b7d5-540a53fd4cb0`，调度到明确节点，2026-09-13 09:52:26 UTC 退出，Succeeded/exit=0/零重启。
- 使用已有可信 mounter 镜像，只读挂载 securityfs；不挂载宿主机根/运行时 socket、不使用 hostPID/hostNetwork/API token、不执行 parser。

实际读回：

```text
AppArmor enabled: Y
LSM: lockdown,capability,landlock,yama,apparmor,tomoyo,bpf,ipe,ima,evm
Loaded profiles: 106
Default runtime profile: cri-containerd.apparmor.d (enforce)
Privileged diagnostic process: unconfined
```

另外仅对既有业务 Pod 做只读 `/proc/1/attr/current` 抽查：普通池 `sandbox-pool-i4wv2qpnta` 的 sandbox 为 `cri-containerd.apparmor.d (enforce)`；FUSE 池 `sandbox-pool-athl08r6kc` 的 workspace-mounter 为 `unconfined`。没有执行 API authorize、挂载、flush 或销毁。

结论：已确认该测试节点具备内核 AppArmor 和已加载 enforce 策略，可用于下一阶段加载器/自定义策略实验。不能推断所有节点或 privileged mounter 自定义 Localhost 已通过。只读路径没有验证宿主机 parser 版本，后续可信加载器应自带 parser 并验证语法与内核兼容。

预检夹具见 `testdata/apparmor-preflight/reader.yaml`。联合 namespace/Pod 的 server dry-run 中 namespace 不会真实落地，因此 Pod 验证曾返回 namespace NotFound；这不是节点或策略错误。确认 namespace 起初不存在后以 create 创建本轮独立资源，实际 Pod 执行成功。

## 未关闭的门槛

- 内核、LSM、securityfs 和 runtime 环境门槛已确认；parser、完整自定义策略及加载路径仍待实验。
- 真实 privileged mounter 和挂载子进程须读回 exact profile/enforce，并验证 mount/flush/unmount 与越权拒绝负例。
- 加载器 Ready 不代替进程证据；忽略 Localhost 的运行时不能按生产验收通过。
- 未完成策略、镜像、runtime 门禁或 Chart 实现；独立 Sentinel 基础实验已单独记录，不将本轮只读预检当作功能交付。

本轮创建了明确受信的 privileged 只读诊断容器，并只读挂载现有 securityfs；没有改变节点挂载/内核参数、加载或卸载策略、读取线上密钥。清理前确认 namespace 中没有业务资源，使用 UID 前置条件删除本轮 Pod/namespace，不 force、不清 finalizer。

删除完成等待返回成功，namespace 的 ignore-not-found 查询返回空；业务 namespace 仍为原有三个 API Ready/零重启及原有沙盒池。夹具语法/权限范围检查及 git diff --check 通过，本次没有运行自定义 profile 或 FUSE 约束测试。
