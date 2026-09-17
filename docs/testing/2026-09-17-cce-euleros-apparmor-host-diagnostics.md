# CCE EulerOS 2.0：AppArmor 宿主机只读诊断

时间：2026-09-17 23:27–23:28（北京时间）；23:29 独立复核清理。

## 结论与授权

用户明确生产环境也是 EulerOS 2.0，并允许两个临时非 privileged 只读诊断 Pod。**本轮发现两节点均缺少宿主机 `/sbin/apparmor_parser`，静态 containerd 配置均显式设置 `disable_apparmor = true`；内核已启用 AppArmor，预期 profile 已加载为 enforce。**

这些实际宿主机前提缺口与 [v0.3.34 现场 CRI 拒绝](2026-09-17-cce-v034-live-validation.md) 一致。没有读取运行中 CRI 的有效配置，因此不能将静态配置视为对运行中配置的查询结果，也不能确定两个缺口中哪一个是厂商二进制当时首先命中的条件。未取得修复后的真实 FUSE/enforce 证据，完整 API/FUSE 验收仍受阻。

生产目标保持 EulerOS 2.0，不把迁移 Ubuntu 当作本轮方案，不自动切换 SELinux、不关闭 LSM、不按 OS 添加硬编码白名单。项目最低支持 Kubernetes 1.29，不改变其它已验证集群功能。

## 范围与隔离

- 仅操作原双华云 CCE 测试集群，kube-system namespace UID `cebeee78-9003-49bf-9442-3fea70515499`；不修改 `ds-ai-research` 或生产环境。
- 随机临时 namespace `sandbox-cce-node-check-ce0a2a0f8769`，UID `030516e1-f5a5-4f58-8ed2-a05738aa3ee9`。
- 节点 `172.16.30.166`（UID `69a79f2b-4f61-44c7-b46a-c46230c3530c`）、`172.16.30.193`（UID `b5ada28b-b127-4851-a0f2-a574ae572f13`）逐次核验身份与 Ready 状态。
- 两个普通 Pod，各绑定一个精确节点；无 Job/DaemonSet/Service/Secret/RBAC/PVC，最多五分钟 active deadline。
- 复用用户 bootstrap `v0.3.1` 的固定 manifest list digest：`sha256:f3ee05dfa6bd9f3e3b2c827ce6f4c8d371ce6a4d297aed2b4ecb5e88c62c0636`。两个现场 imageID 均匹配、零重启，没有构建/推送镜像。
- privileged=false、allowPrivilegeEscalation=false、readOnlyRootFilesystem=true、capabilities drop ALL、Seccomp RuntimeDefault；无 hostPID/hostIPC/hostNetwork/shareProcessNamespace，无 API token、env、envFrom、额外 sidecar、runtime socket 或主机根目录挂载。
- 唯一四个 hostPath 为 `/etc/containerd`、`/sbin`、`/sys/module/apparmor`、`/sys/kernel/security`，均 Directory、只读、无 mount propagation；不执行宿主机二进制。
- 读取仅限固定 `config.toml`、parser 元数据、AppArmor enabled/profiles；不遍历/跟随配置 imports，不读取主机环境、systemd 或其它目录。配置原文只在进程内解析为白名单结果，不写入报告、终端或仓库。
- 诊断 exec 进程实际确认 CapEff 全零、NoNewPrivs=1、Seccomp=2；宿主机读取前后核验同 Pod UID/节点及隔离契约。

## 两节点实测

| 检查 | 172.16.30.166 | 172.16.30.193 |
| --- | --- | --- |
| AppArmor enabled | Y | Y |
| securityfs profiles 可读 | 是 | 是 |
| 预期 profile enforce | 是 | 是 |
| `/sbin/apparmor_parser` 入口存在（含符号链接检查） | 否 | 否 |
| config.toml 可读、version | 是、2 | 是、2 |
| `plugins."io.containerd.grpc.v1.cri"` 存在 | 是 | 是 |
| 静态 `disable_apparmor` | true | true |
| 静态 `enable_selinux` | false | false |
| 静态 imports 数量 | 0 | 0 |
| 运行中 CRI 配置查询 | 未执行 | 未执行 |

预期 profile 为 `sandbox-fuse-37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d`。没有再次加载、卸载或修改 profile；只读取前轮加载器已留在内核中的状态。

## 与实现的对应关系

上游 [containerd 1.7.29 的 apparmorEnabled](https://github.com/containerd/containerd/blob/v1.7.29/pkg/cri/server/helpers_linux.go) 在 DisableApparmor 为 true 时直接返回 false；其 [HostSupports](https://github.com/containerd/containerd/blob/v1.7.29/pkg/apparmor/apparmor_linux.go) 还检查宿主机 parser 路径、内核/securityfs 等，并缓存首次结果。现场是厂商补丁版本，不把上游源码等同于已核验厂商二进制。

项目 loader 使用镜像内 parser 加载 profile，无法填补宿主机 parser 或替容器运行时改变有效配置。仅更新 API/bootstrap/values、增加重试或移除 AppArmor 请求不能修复上述节点前提。禁止创建空 parser 文件、假脚本或占位链接来欺骗能力检测；不能仅为启动成功以 Unconfined/关闭 LSM 替代真实 enforce。

现有 loader Dockerfile 的 parser 来自 Debian 13 镜像。未验证 EulerOS ABI/依赖前不能直接复制它到宿主机。查阅 [HCE 2.0 x86_64 OS 包索引](https://repo.huaweicloud.com/hce/2.0/os/x86_64/Packages/) 与 [updates 包索引](https://repo.huaweicloud.com/hce/2.0/updates/x86_64/Packages/)，本次未找到名称包含 apparmor 的 RPM；这不证明所有私有/其它仓库都无可用包，也不能编造 `yum install` 包名或以第三方发行版包盲目替代。

## 清理与本地校验

23:27:57，正常 UID precondition 删除两 Pod、保留原五秒宽限期并等待消失，再以 namespace UID precondition 删除本轮 namespace，确认清理完成。未强制删除、手工移除 finalizer、跳过 hooks 或创建云卷。

23:29 独立只读复核：本轮 namespace 不存在、所有 `sandbox-cce-` 临时 namespace 为零、无 PV claimRef 指向本轮 namespace；两原节点 Ready，kube-system 无非终态异常未就绪 Pod。

诊断执行退出码 0；本地检查脚本语法、TOML true/false 及凭据白名单过滤，并用八个负向 fixture 验证 privileged、主机根目录、可写挂载、token、env、hostPID、额外容器与 runtime socket 均拒绝。这些不是生产节点变更、实际 CRI 重新配置或业务性能/HA 验收。

## 下一阶段：另行授权节点整改

本次只读授权已执行完毕。尚未安装软件、修改配置、重启 containerd/kubelet、变更节点 OS、构建镜像或修改业务代码/Chart。生产仍以 EulerOS 2.0 为目标。

后续须先确认双华云厂商对该 EulerOS/CRI 组合的支持，准备可信且兼容的宿主机 parser 与依赖；不直接套用仅列 Ubuntu 的公有云安装命令。节点变更只在另获授权后，按平台维护方式先选一个测试节点验证：安装真实 parser、启用有效 CRI AppArmor，并评估能力缓存及受控服务重启的影响。静态配置、CRI 有效配置、实际 mounter/PID 1 与 s3fs enforce、内核拒绝、挂载/存储、正常清理均取得证据后，再覆盖另一测试节点及完整 API/网络测试。不能将授权只读诊断理解为生产节点写入或服务重启许可。
