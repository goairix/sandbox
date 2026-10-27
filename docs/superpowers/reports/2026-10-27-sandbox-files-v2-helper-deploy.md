# Files v2 helper CCE 隔离部署验证（2026-10-27）

## 范围

- 源码：`feat/etcd-state-management` 的 `4ac00899`；本次只验证 `sandbox-file-op` 第一片。
- 集群：`var/tmp/k8s-test/sandbox-fuse-v2-kubeconfig.yaml`，context `internal`，CCE `sandbox-fuse-etcd`。
- 只创建测试 Pod `files-v2-helper-smoke-4ac00899`，UID `3acb4dd9-6354-45de-aee6-c7a68afb6ba8`，节点 `172.16.30.29`。未修改现有 API Deployment、池、etcd 或业务 Pod。
- Pod 使用当时已运行的 runtime 镜像 digest `sha256:ec108aeee183c1cb0f3a5c481bf38038626fde8da96ae5a4624df396d5f6d7d8`；将本次静态 Linux/ARM64 ELF 临时复制到该隔离 Pod 的 `/tmp/sandbox-file-op`。本地与 Pod 内 SHA-256 均为 `3900aab383fef9c809f28b6f426772bf3a9e79af61af5837df3f048fe3c20dab`。

## 结果

| 检查 | 实际结果 |
| --- | --- |
| Pod 准备及身份 | `Ready`；`uid=1000(sandbox) gid=1000(sandbox)` |
| 原始字节写入、读回 | 向 `/workspace/hello.txt` 写入 5 字节 `hello`，随后读回 `hello` |
| 元数据和目录 | `stat` 返回路径与大小 5；`list /workspace` 返回该文件 |
| 越界链接 | `/workspace/escape -> /etc` 后读取 `escape/passwd` 被 `openat2` 以 `EXDEV` 拒绝，退出码 1，无文件内容输出 |
| 资源清理 | 测试 Pod 已删除并等待完成；按测试标签查询结果为空 |

本地 Linux 容器内六个 `filev2` 顶层测试也通过；amd64 与 arm64 静态编译通过。Docker 构建过滤文件原本漏掉新 helper 目录，已在 `d7228dd7` 修复；BuildKit 用现有本地基础镜像验证 `COPY cmd/sandbox-file-op` 和 `COPY internal/filev2` 均实际通过。该替代基础镜像没有 Go，故随后的构建步骤按预期以 `go: not found` 退出，**不能把这次检查记作完整镜像构建成功**。

## 尚未验证

- 完整沙盒镜像构建、推送与基于新镜像的 Pod 启动。Docker Hub 的 `golang:1.25-alpine` 授权请求被连接重置；本机从私有仓库拉取当前 runtime digest 长时间停滞后取消。没有修改现有线上镜像引用。
- Files v2 Docker/Kubernetes transport、Manager、HTTP 路由、SDK、Eino 契约、FUSE 文件同步。这些后续任务尚未接线，因此本次结果不能表示公开 Files v2 已可用。

隔离测试产物位于此计划的忽略目录 `.superpowers/sdd/2026-10-26-sandbox-files-v2-core/deploy-smoke/`；不包含凭据。
