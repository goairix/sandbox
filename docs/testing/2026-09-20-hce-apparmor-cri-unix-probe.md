# HCE 测试节点 AppArmor CRI 与 AF_UNIX 隔离探针

日期：2026-09-20。仅测试集群节点 `172.16.30.166`；没有操作生产节点或第二个 CCE 节点。

## 节点整改

- 节点 `cce-sandbox-fuse-05598-1batu`，HCE 2.0 x86_64，containerd `v1.7.29-14-g6d1d1601f`。节点 UID `69a79f2b-4f61-44c7-b46a-c46230c3530c`，`kube-system` namespace UID `cebeee78-9003-49bf-9442-3fea70515499`。
- 安装前：`/sbin/apparmor_parser` 不存在，运行中 `crictl info.config.disableApparmor=true`，containerd/kubelet active，节点 Ready。
- 测试 RPM `sandbox-apparmor-parser-4.1.7-1.x86_64.rpm` SHA-256 `766cb1ac45bd13e5643c2dd380c326ca2c6a111275bdd2937d2550baefa58ee3`；`rpm -K` 仅为 `digests OK`，**未签名，不是生产发布包**。包无 scriptlet，依赖和文件清单已核对；`rpm --test -i`、`rpm -i`、`rpm -V` 均返回 0。已安装的 `/usr/sbin/apparmor_parser` 报告版本 4.1.7，`/sbin` 为指向 `usr/sbin` 的链接，`/sbin/apparmor_parser` 可执行。
- 先将原 `/etc/containerd/config.toml` 原样备份到 `/var/tmp/containerd-config.toml.20260920T044743Z.bak`，备份与原文件当时 SHA-256 同为 `65be0570e6b27d512c6b6f987714651b01392ff69b0478a31442942ba78bb10f`。唯一内容差异是 CRI `disable_apparmor = true` 改为 `false`。用 `containerd --config /etc/containerd/config.toml config dump` 校验成功后重启 containerd。
- 重启返回 0，containerd/kubelet active，运行中 `crictl info.config.disableApparmor=false`，节点仍为 Ready。未执行回退；该测试节点当前保留已安装 parser 和启用的 CRI AppArmor。生产节点配置、RPM 和 Helm release 未改。

## 真实 CRI 和策略测试

在唯一临时 namespace `sandbox-aa-probe-20260920`，固定 `nodeName=172.16.30.166`：

1. `RuntimeDefault` 探针 Pod 启动，零重启，`/proc/1/attr/current` 为 `cri-containerd.apparmor.d (enforce)`。这证明实际 CRI 已能创建 AppArmor 容器，不能仅用静态配置推断。
2. 从当前 Chart 策略只删除一行 `network unix stream,`，得到唯一临时 profile `sandbox-fuse-unix-probe-4ecbfdd049a8f05aa9e034eb6464c25c899fa6c5241ef85dbe8d84e0df728eb7`，profile SHA-256 `acc2da01e1cf212684e7d1f04ec63a2cbc7914528df7febce1cb83bd1853f40e`。以目标内核 29 个非空 feature 文件同时作 policy/kernel 输入，执行 `-Q -K --config-file=/dev/null --Werror --warn=all` 严格编译，退出码 0，stdout/stderr 均为 0 字节，二进制 9681 字节。仅该临时名称被加载到内核，状态为 enforce；没有替换业务 profile。
3. 临时二进制通过允许执行的 `/usr/local/bin/workspace-mounter` 路径在候选 profile 下启动，读回的 `/proc/self/attr/current` 确认为该 profile `(enforce)`；`socket(AF_UNIX, SOCK_STREAM, 0)` 返回 `EACCES (13)`。宿主机 audit 有同一 profile 的 `apparmor="DENIED" operation="create" family="unix" sock_type="stream"`。这证明拒绝确由 AppArmor 生效，而非仅靠进程错误码推测。

## 真实 mounter 测试阻塞

准备使用测试 values 指向的 `registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-mounter:v0.3.30` 启动真实 prepared mounter，但 Pod 在执行程序前即 `Init:StartError`：`/usr/local/bin/workspace-mounter: no such file or directory`。随后构建的 `v0.3.34` 也重复了同一问题：节点成功拉取 manifest digest `sha256:29986641fde8570df196aeacc8e1afdb4298bdba0fd6ec6c31b3f85e22eb2f4c`，但镜像内容只有 `/app/sandbox`，`/usr/local/bin` 为空。独立用两个 tag 启动诊断容器均确认该文件不存在；CRI 镜像元数据的入口是 `/app/sandbox --config /etc/sandbox/config.yaml`，属于 API 镜像入口，而非仓库 `docker/images/workspace-mounter/Dockerfile` 定义的 mounter 入口。

因此**候选策略的真实 mounter/FUSE 正向链路尚未通过**，不能把 AF_UNIX 拒绝通过解释为 FUSE 可用，也不能修改正式 Chart 删除该规则。下一步须用正确 Dockerfile 构建新的不可变 mounter tag，在同架构环境对最终镜像执行 `/usr/local/bin/workspace-mounter health prepared --release-check-image`；更新测试 values 后重新做 prepared、实际 MinIO 挂载/读写/flush/卸载、子进程 enforce 和正常清理。不能复用错误的 `v0.3.30` tag 并声称已重新构建。

## 收尾

已精确删除临时 namespace（含所有探针 Pod 和 ConfigMap），从内核移除唯一临时 profile，并删除节点上的临时 profile、编译输出与探针二进制。复查临时 namespace/profile 不存在、无本轮测试挂载；原业务 `sandbox-fuse-37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d (enforce)` 仍在，目标节点 Ready，containerd/kubelet active，`disableApparmor=false`。
