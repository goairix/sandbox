# Kubernetes 沙盒服务环境变量清理

普通和 FUSE Pod 已有 `enableServiceLinks: false`。Kubelet 对 default namespace 的
`kubernetes` Service 仍生成环境变量，不受该开关影响，见
[Kubernetes v1.33 源码](https://github.com/kubernetes/kubernetes/blob/v1.33.0/pkg/kubelet/kubelet_pods.go#L634-L643)。
原实现用八个显式空值覆盖服务地址，所以没有暴露对应地址值，但变量名仍可被业务进程读取。

## 修复范围

- 保留 Pod 中的空值覆盖，避免 kubelet 将真实 Service 地址补回 CRI 容器配置。
- 普通 PID1 的 sleep、FUSE PID1 的 probe，以及同步/流式命令、文件读写、tar/pipe 和
  私有 sandbox probe 执行入口，统一经过 `/usr/bin/env -u ... --` 再 exec 原目标。
  既移除变量名，也清理目标的初始 `/proc/self/environ`；保留 PATH、业务变量、runtime UID、
  控制公钥、argv、stdin 和退出状态。workspace-mounter 的控制执行入口不改变。
- 普通/FUSE 预热 Pod 模板版本升至 v6，避免把旧启动模板作为新池复用。
  控制 wire/cleanup protocol 没有变化，Helm cleanup v3/control v5 保持兼容。

直接由集群管理员执行的原生 `kubectl exec POD -- env` 仍可能读到 CRI 配置中的八个空变量。
它不是从已清理 PID1 派生的进程。应用进程环境清理不能修改 kubelet/CRI 的这份配置，
也不代表完全隐藏所有 Kubernetes 运行痕迹。

## 发布与复测

只需重建 sandbox-api 并更新其镜像。当前 sandbox-runtime 已提供所需的 `/usr/bin/env -u`，
本次没有修改 runtime Dockerfile、probe 二进制、SDK 或 Helm 模板。
自定义 runtime 镜像须同样提供该命令。

更新 API 后，平台执行入口立即使用清理前缀。现有 active Pod 的 PID1 不会被原地修改或强制删除；
完整启动环境修复在沙盒正常销毁、重新申请后生效。未绑定旧空壳按既有池契约正常淘汰。

通过沙盒业务执行接口运行以下脚本，应输出空列表；需分别验证同步和流式接口：

```python
import os
names = [
    'KUBERNETES_SERVICE_HOST', 'KUBERNETES_SERVICE_PORT',
    'KUBERNETES_PORT', 'KUBERNETES_PORT_443_TCP_ADDR',
    'KUBERNETES_PORT_443_TCP_PORT', 'KUBERNETES_PORT_443_TCP_PROTO',
    'KUBERNETES_PORT_443_TCP', 'KUBERNETES_SERVICE_PORT_HTTPS',
]
remaining = [name for name in names if name in os.environ]
print(remaining)
assert not remaining
```

回归测试对新 Pod 启动前缀和 client-go 实际 exec 请求执行验证：继承值为空、非空时均删除
上述八个变量，同时保留 PATH、业务变量和 UID。覆盖同步、TTY/非 TTY 流式、文件上传/下载、
archive、pipe、glob 和私有 probe；workspace-mounter 保留原命令。

验证结果：新增启动/exec wire 回归在旧代码中失败，修复后通过；`go test ./...`、
Kubernetes runtime/Manager/workspaceprobe 的 race、Linux amd64/arm64 编译、宿主及 Linux arm64
lint 均通过。使用现有 v0.3.40-arm64 runtime 镜像验证了两个 PID1 的实际 `/proc/1/environ`；
FUSE 签名暂停、重复终态回收和旧 resume 拒绝通过；Linux 镜像中的针对测试通过。
没有构建、推送或部署镜像，没有操作线上沙盒。
