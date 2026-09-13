# AppArmor 实施前环境预检

## 范围与结果

本次按已确认规格进行只读本地预检，不加载策略、不安装 DaemonSet、不修改现有 Kubernetes 工作负载。尚未获得明确的隔离 Linux 宿主机目标，因此真实约束实验没有执行，不记为通过。

本地 Docker 输出：

```text
linux aarch64 ["name=seccomp,profile=builtin","name=cgroupns"]
```

`docker`、`helm`、`kubectl` 可用，macOS 宿主机没有 `apparmor_parser`。Docker SecurityOptions 未报告 AppArmor，本轮不能将该环境用于证明 Localhost profile 的真实 enforcement；这不是其它 Linux 节点不支持 AppArmor 的证据。

## 未关闭的门槛

- 需要明确隔离 Linux 环境，读取内核 AppArmor 启用状态、LSM 列表、securityfs、parser 与容器运行时版本。
- 真实 privileged mounter 和挂载子进程须读回 exact profile/enforce，并验证 mount/flush/unmount 与越权拒绝负例。
- 加载器 Ready 不代替进程证据；忽略 Localhost 的运行时不能按生产验收通过。
- 当前只解除本地工具可用性检查，未完成策略、镜像、runtime 门禁或 Chart 实现。独立 Sentinel 实验可以先推进。

本轮没有修改内核参数、挂载 securityfs、读取线上密钥或部署 privileged 管理容器。
