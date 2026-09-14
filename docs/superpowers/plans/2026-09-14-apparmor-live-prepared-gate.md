# AppArmor 真实加载与 prepared 门槛验收

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans。当前会话内联执行，不派生子代理、不创建 worktree。

**Goal:** 在用户指定的 ds-ai-worker-2 证明发布加载器能够加载候选策略，并验证真实 privileged supervisor 的 prepared 状态和 exact enforce 属性。

**Architecture:** 使用独立 namespace，直接渲染并创建 Chart 的加载器资源，不安装或改动线上 release。测试 Pod 使用现有 mounter/runtime 镜像、native sidecar 和 emptyDir 工作目录；bootstrap 不携带凭据、不连接对象存储。本阶段失败则停止真实存储授权，保留 fail-closed 结果，不扩大候选规则制造通过。

**Tech Stack:** Helm、kubectl、Kubernetes 1.33、containerd 2.2、AppArmor、安全元数据读取。

**执行状态：** Task 1/2/4 已完成；Task 3 已完成资源清理及核心回归，验收记录随本次提交保存。详细身份、失败排查与未验证项见 [实际结果](../../testing/2026-09-14-apparmor-live-prepared-results.md)。本轮挂载走真实 supervisor 私有协议，不计作公共 API 集成已通过。

## 权限与目标

- 最新用户明确要求在 ds-ai-worker-2 测试，并提供加载器镜像 v0.3.24-arm64；这取代旧预检计划“只读、不加载”的阶段边界。
- context ds-ai-research；namespace sandbox-apparmor-enforcement-20260914-a18c24；临时 release 渲染名 sandbox-apparmor-20260914。
- 仅创建自己的 namespace、Chart 渲染资源和 prepared-mounter Pod；不修改业务 namespace、现有 profile、节点标签、内核参数、运行时或 PVC。
- securityfs 读写权限仅给可信 Chart 加载器；mounter 仅挂载 /dev/fuse 和自己的 emptyDir。
- 加载器镜像按已核验 digest 固定，后续变更策略会生成新名称；不会替换或卸载旧名称。
- 清理前复核 namespace UID/测试标签/精确资源归属，通过 UID 前置条件删除自己的资源。不 force、不清业务 finalizer、不卸载内核 profile。

## Task 1：准备及渲染

**Files:** 创建 testdata/apparmor-enforcement/values.yaml 和 prepared-mounter.yaml；创建 docs/testing/2026-09-14-apparmor-live-prepared-results.md 记录实际结果。

- [x] 核对镜像 digest、节点 Ready/无 taint，确认 namespace 不存在。

```sh
docker buildx imagetools inspect registry.i.huaxisy.com/library/ai-infra/sandbox-apparmor-loader:v0.3.24-arm64
kubectl --context ds-ai-research get node ds-ai-worker-2
kubectl --context ds-ai-research get namespace sandbox-apparmor-enforcement-20260914-a18c24 --ignore-not-found -o name
```

- [x] 渲染 values，验证唯一选择器、策略摘要、token=false、仅规定 hostPath 及固定镜像。将 ConfigMap 的策略声明名称替换 Pod 的唯一 __APPARMOR_PROFILE__ 占位符；不重写策略规则。

```sh
helm template sandbox-apparmor-20260914 deploy/helm/sandbox \
  --namespace sandbox-apparmor-enforcement-20260914-a18c24 \
  -f testdata/apparmor-enforcement/values.yaml \
  --show-only templates/apparmor-loader.yaml
go test ./internal/apparmorloader ./internal/runtime/kubernetes -count=1
```

## Task 2：真实加载与 prepared

- [x] 确认目标 namespace 原先不存在后 create。namespace 设置明确实验标签和 privileged PSA；其它模板原样创建，不创建线上 Helm 记录。
- [x] 创建渲染出的加载器资源，最多等待 120 秒；每次工具等待不超过 60 秒。Ready 后读回 parser 版本、enabled=Y 和 exact profile(enforce)。禁止打印整份 kernel profiles 列表。

```sh
kubectl --context ds-ai-research -n sandbox-apparmor-enforcement-20260914-a18c24 get daemonset sandbox-apparmor-20260914-apparmor-loader
kubectl --context ds-ai-research -n sandbox-apparmor-enforcement-20260914-a18c24 get pods -l app=sandbox-apparmor-loader -o wide
```

- [x] 创建自己的 prepared-mounter Pod，验证零重启、native sidecar started，以及以下固定命令结果；不以 sandbox 容器 Ready 代替 mounter prepared 成功。

```sh
kubectl --context ds-ai-research -n sandbox-apparmor-enforcement-20260914-a18c24 exec prepared-mounter -c workspace-mounter -- /bin/cat /proc/1/attr/current
kubectl --context ds-ai-research -n sandbox-apparmor-enforcement-20260914-a18c24 exec prepared-mounter -c workspace-mounter -- /usr/local/bin/workspace-mounter health prepared
kubectl --context ds-ai-research -n sandbox-apparmor-enforcement-20260914-a18c24 exec prepared-mounter -c workspace-mounter -- /usr/local/bin/workspace-mounter health prepared --self-check-image
```

- [x] 失败时读取本 namespace 的 Warning、短日志和脱敏终止原因；不授权、不连接线上存储。成功只证明此门槛，不记作 mount/flush/unmount、写入拒绝、重载和高可用已通过。

## Task 3：记录及清理

- [x] 记录时间、镜像 tag/digest、namespace/Pod UID、真实模式、健康结果及每项未执行验收。若运行时忽略 Localhost，明确禁止生产启用，不改成 unconfined。
- [x] 通过 UID 前置条件删除本轮精确 namespace，等待消失；再复核线上 API/Redis 仍 3/3 就绪。内核策略保留，并在报告中记录名称。
- [x] git diff --check 和 fixture 渲染/核心回归通过后提交已完成的验收记录，不推送或构建镜像。

## 后续门槛

真实 prepared/enforce 成功后继续规划独立测试对象存储及真实 API 的 mount/flush/unmount、s3fs 子进程、文件写入/mount 拒绝和重载实验。本阶段不将不存在的存储环境假定为可用，不使用业务凭据填补缺口。

### Task 4：prepared 成功后的隔离 TLS 挂载实验

- [x] 创建 testdata/apparmor-enforcement/minio-lab.js。仅接受固定 setup/seed/seed-prefix/verify 模式，setup 钉住本次 namespace UID，生成仅 1 天有效测试 TLS，使用已核验 arm64 MinIO digest、emptyDir（无 PVC）及本 namespace/DNS 网络白名单；seed/seed-prefix/verify 仅向本机专用端口转发连接发起验证 TLS 的 SigV4 请求。不读取业务 Secret 内容，不打印实验私钥或签名。

```sh
node testdata/apparmor-enforcement/minio-lab.js setup
kubectl --context ds-ai-research -n sandbox-apparmor-enforcement-20260914-a18c24 port-forward pod/minio --address=127.0.0.1 19000:9000
node testdata/apparmor-enforcement/minio-lab.js seed
```

- [x] 仅为新 minio-mounter Pod 在 prepared fixture 中更换名称/endpoint、添加只读测试 CA（Secret items 仅 public.crt，不挂载私钥）、延长本 Pod deadline 为 600s；使用固定 mount prefix workspace-test/、generation 1、唯一 UID、实验凭据进行私有 authorize。夹具目录标记修复后创建新 UID 的 minio-mounted-2 完成授权，不重用已消耗 supervisor。
- [x] 验证 ready，再从 tenant 容器写入固定实验文件并读回，通过 supervisor flush 的 accepted ACK 和 shutdown 的 graceful_unmount ACK 证明挂载/flush/卸载；读取 s3fs 子进程 exact enforce、mountinfo 及零重启元数据，卸载后以独立 TLS GET 严格核对内容。不使用 shell 执行子进程 profile 代替 s3fs 本身的属性。
- [x] 围绕可写运行目录/可写 shm 做允许与拒绝对照；再次启动 shell 的子进程应拒绝。初始 Kubernetes Exec 可直接设置 profile 后运行首进程，不把首进程的 exec 成功误判为 supervisor 子进程执行规则失效。非 FUSE tmpfs mount 在本 Pod 失败且可信 loader 的私有 emptyDir 对照成功；可信 loader 只读内核审计并严格筛选自己的 exact profile，确认 mount error=-13，而非只凭 utility 提示。
- [x] 仅停止自己的 loader Pod，重建后 exact enforce/Ready 应恢复；这不等于重启整台业务节点或“策略被卸载后重载”已验证。结束全部实验后按 Task 3 精确清理，保留内核 profile。
