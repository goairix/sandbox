# ds-ai-worker-2 AppArmor 真实内核验收

## 结论与范围

2026-09-14 18:06–18:22（北京时间），用户提供的加载器镜像在指定节点通过真实加载、privileged mounter prepared、TLS MinIO 挂载、租户写入、flush、正常卸载、卸载后对象持久化校验、越权拒绝及加载器 Pod 重建恢复。本轮没有修改生产代码或扩大候选策略权限。

这是该节点、该镜像与 MinIO 挂载组合的组件级验收，**不是线上 release 已启用 AppArmor或全部 API 集成/高可用已通过**。线上 release 未被修改；启用后的普通/FUSE API、跨副本生命周期以及其它目标节点覆盖仍须验证。没有重启业务节点、卸载内核策略或模拟内核策略丢失。

## 环境与身份

| 项目 | 实际值 |
| --- | --- |
| context / 节点 | `ds-ai-research` / `ds-ai-worker-2` |
| 节点 | Debian 13、`6.12.57+deb13-arm64`、arm64 |
| Kubernetes / 运行时 | `v1.33.6` / `containerd://2.2.0` |
| 加载器 | `registry.i.huaxisy.com/library/ai-infra/sandbox-apparmor-loader:v0.3.24-arm64` |
| 加载器拉取 digest | `sha256:5a984dcb978f6634acee4446bb2f1ae5d2aca918ee53b189372deb688bcf908c` |
| parser / 内核 enabled | `4.1.0` / `Y` |
| mounter / 普通容器镜像 | `sandbox-fuse-mounter:v0.3.4` / `sandbox-runtime:v0.3.4`，使用现有仓库地址 |
| 临时 namespace | `sandbox-apparmor-enforcement-20260914-a18c24` |
| namespace UID | `d88ea2e2-c80d-4bdb-bc77-50bf2bfb6f55` |
| prepared Pod UID | `7b92a53b-6a46-4e8f-8624-5dd885b124a8` |
| 成功挂载 Pod / UID | `minio-mounted-2` / `997e91f8-f22c-489f-83e0-1ed2350e123e` |

本轮策略直接来自 Chart 文件，未改动规则，完整名称为：

```text
sandbox-fuse-37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d
```

## 加载及真实挂载结果

- 仅创建 Chart 加载器模板的 ServiceAccount、ConfigMap、DaemonSet、Role、RoleBinding；没有安装完整 Helm release、没有生成实验 Helm 历史。
- 加载器实际拉取上述 digest，节点内核存在 exact profile `(enforce)`，初次 Pod Ready、零重启。
- native privileged mounter 的 `/proc/1/attr/current` 为 exact profile `(enforce)`，`health prepared` 为 prepared/generation 0/cache 0/restart false，`health prepared --self-check-image` 返回 0。
- 独立 MinIO 使用固定 arm64 digest、emptyDir，无 PVC。实验生成 1 天自签 TLS，bootstrap 只挂只读公共 CA，未挂私钥；使用公开的实验凭据，未读取业务 Secret 内容。网络策略只允许本 namespace 内通信及 DNS。
- 新 Pod 的一次性 authorize 返回 `accepted=true` / generation 1；`health ready` 为 ready/fuse/generation 1/restart false/cache 0，mountinfo 显示 `/workspace` 的 `fuse.s3fs` 挂载。
- 根据真实 s3fs 进程的 `/proc/<PID>/stat` 定位子进程，再读取其 `attr/current`，结果同样为 exact profile `(enforce)`，不是用 shell 的 profile 冒充 s3fs。
- uid/gid 1000 的普通容器写入并读回 `accepted.txt`，内容为固定实验文本。
- supervisor `flush` 返回 `accepted=true`；strong `shutdown` 返回 `graceful_unmount=true`；后续 mountinfo 无 FUSE workspace 挂载。
- 通过带 SigV4 认证且验证 TLS 的独立 GET，卸载后的 `workspace-test/accepted.txt` 内容与写入内容严格一致。

本实验直接测试真实 supervisor 私有协议，不经过公共 API。prepared Pod 设置 300 秒主动截止，之后因 DeadlineExceeded 结束，是实验边界，不是策略启动故障。shutdown 后 `health ready` 返回 s3fs-exited 是正常退出后的 fail-closed 结果，不应继续把该一次性 supervisor 当可用池实例。

## 拒绝行为与审计证据

| 操作 | 实际结果 |
| --- | --- |
| 读取允许的 `/etc/hosts` | 返回 0 |
| 写自己的 `/run/s3fs` | 返回 0 |
| 读取 `/etc/shadow` | Permission denied，内核 `DENIED operation=open requested_mask=r` |
| 写可写 tmpfs `/dev/shm/apparmor-deny-test` | Permission denied，内核 `DENIED operation=mknod requested_mask=c` |
| 已受限进程再执行 `/bin/sh -c true` | 返回 126，内核 `DENIED operation=exec name=/usr/bin/dash requested_mask=x` |
| 向自己的 cache 目录挂载 tmpfs | 返回 32，内核 `DENIED operation=mount fstype=tmpfs error=-13` |
| 可信 unconfined loader 在自己的 run emptyDir 挂载/卸载 tmpfs | 返回 0，实验目录随后移除 |

tmpfs utility 的提示包含“只读/写保护”，不能单凭该文字归因为 AppArmor；后来通过可信 loader 只读 `dmesg`、严格筛选本轮 exact profile，确认上述 mount 确实被 AppArmor 拒绝。未读取其它 profile 的业务审计日志，没有新增主机 root/socket/hostPID 挂载。

直接 Kubernetes Exec 的首进程可先被置入指定 profile 再运行，因此首进程 `/bin/sh` 的执行成功不是 supervisor 能执行任意 shell 的证据。其自身 profile 读回仍是 enforce；正确负例是该已受限进程继续启动 shell 子进程，结果被拒绝。公共协议没有增加通用 shell 或 mounter Exec 能力。

## 测试夹具故障排查

首次受限挂载失败，完全相同的 unconfined 对照 `minio-control` 也失败。直接 s3fs 诊断确认：实验只创建 `.seed`，没有创建 `workspace-test/` 的规范目录标记，s3fs 返回 Bucket or directory not found。补齐实验对象标记后，使用新 Pod/新 UID 的一次性授权成功；没有修改 s3fs 生产选项或通过放宽策略掩盖故障。

实验 TLS 生成还遇到完整 DNS 作为 CN 太长，已将 CN 改成短实验名称、保留完整 DNS SAN。手工诊断曾尝试 s3fs 1.95 不支持的 `ssl_ca_cert` 选项；改用生产 runner 已使用的 `CURL_CA_BUNDLE` 验证后通过。以上均是实验夹具/诊断错误，不是线上配置建议。

内核审计仍记录 Go/curl/ls 的可选只读探测被拒绝，包括 hugepage 大小、cgroup、somaxconn、时区、host.conf、filesystems/mounts；本轮功能链路不受阻。没有为了消除审计噪声盲目扩大规则。容器 CPU 适配及读回探测的性能影响未压测，不能声称额外约束开销为零。

## 加载器恢复与清理

仅以 UID 前置条件删除自己的初始 loader Pod：

```text
旧 UID 80219922-bb46-4028-b819-8df3a56ca808
新 Pod sandbox-apparmor-20260914-apparmor-loader-k6rf2
新 UID c136f842-548a-4b4b-af19-b03d0591ff06
```

新 Pod 在同一节点 Ready、零重启，拉取 digest 未变，readiness 返回 0，exact 内核 profile 仍为 enforce。该测试证明 Pod 重建能恢复服务，不等价于节点重启后的缺失策略重载；没有执行危险的节点重启或卸载实验。

结束前核对全部 namespace 资源及 UID/实验标签，使用 DeleteOptions UID 前置条件删除整个实验 namespace，不 force、不修改 finalizer。实验 MinIO、TLS Secret/CA、网络策略、全部测试 Pod、加载器及 RBAC 均随 namespace 清理；没有实验 PVC。自动注入的 registry Secret 只读取元数据，其 namespace 内副本随实验 namespace 删除，未改其它 namespace 的凭据。端口转发与临时 kubectl proxy 均已停止。

18:21:44 最终复核 namespace 已不存在，线上 `sandbox-fuse-api` Ready/Available 3/3，`sandbox-fuse-redis-sentinel` Ready 3/3。只核对元数据，未重启线上 Pod或修改 release。

节点内核保留上述新 profile，**不自动卸载**，避免误伤仍引用它的进程或共享 release。

## 本地回归与复现文件

```sh
go test ./internal/apparmorloader ./cmd/apparmor-loader ./internal/runtime/kubernetes \
  ./internal/config ./cmd/sandbox ./internal/mounter ./internal/fuseprotocol -count=1
go test -tags helmtests ./internal/helmtest -count=1
bash scripts/test-helm-apparmor-loader.sh
node --check testdata/apparmor-enforcement/minio-lab.js
node testdata/apparmor-enforcement/minio-lab.test.js
git diff --check
```

上述回归、语法与 diff 检查通过。夹具安全回归通过：四种模式都在同名 namespace UID 被替换时、发生任何 Kubernetes/S3 写入之前拒绝执行。最初将受 `helmtests` build tag 约束的包直接放入普通 go test，命令因该包无可构建文件失败；已按正确 tag 独立重跑通过，不计作产品故障。

实验 values、prepared Pod 与 TLS/SigV4 辅助脚本见 [实验夹具](../../testdata/apparmor-enforcement/values.yaml)、[prepared Pod](../../testdata/apparmor-enforcement/prepared-mounter.yaml)、[MinIO 辅助脚本](../../testdata/apparmor-enforcement/minio-lab.js)。辅助脚本固定本轮 namespace UID，是历史验收夹具，不得直接复用于新 namespace，更不能用于生产凭据。过程见 [执行计划](../superpowers/plans/2026-09-14-apparmor-live-prepared-gate.md)。
