# FUSE mounter 就绪远端 I/O 去重设计

## 背景与线上证据

第一阶段已经让 Kubernetes `WaitSandboxReady` 主动等待 exact mounter 就绪，不再依赖
kubelet 每 10 秒更新一次的 `PodReady`。`ds-ai-research` 集群更新后，旧的 0～10 秒周期性
等待已经消失，但 30 次轻量跨副本创建的 p95 仍为 3.695 秒，完整 API 生命周期测试中 30 次
FUSE 创建的 p95 为 5.652 秒，尚未达到热池创建 p95 不高于 3 秒的目标。

同请求分段观测显示，mounter-ready 本身通常占 0.598～2.150 秒，API 在其后还需要
0.381～1.074 秒完成强传播探针。代码检查确认这不是两个互补的安全证明，而是重复远端
对象存储 I/O：

1. `workspace-mounter health ready` 在确认 `fuse.s3fs` 挂载后执行一次
   `/bin/ls -U -- <mount>`；
2. API 随即在 sandbox 容器内执行绑定 exact RuntimeUID 和 Generation 的
   `workspace-probe write-read-delete`，实际创建、写入、`fsync`、关闭、重新打开、读取并删除
   探针对象，同时验证挂载类型和挂载身份没有变化。

kubelet readinessProbe 也会调用同一个 `health ready`，而 `ReadyStatus` 在整个调用期间持有
Supervisor 互斥锁。因此，远端 `/bin/ls` 不仅重复访问后端，还会让 kubelet 与 API 的就绪
检查串行竞争。

## 目标与边界

- 移除 mounter 就绪路径中重复的远端目录读取，降低热池领取延迟和长尾。
- mounter `ready` 仍严格证明 s3fs 进程、真实挂载、exact mount ID 和本地缓存状态健康。
- API 交付前仍必须通过 sandbox 内的 `write-read-delete` 强传播探针；任何失败均不得返回
  可用 sandbox。
- Docker 与 Kubernetes 两种 runtime 保持相同的端到端交付安全性。
- 不改变 HTTP API、Redis 状态、Helm values、网络策略、FUSE Pool 身份协议或超时配置。
- 不把本地测试通过视为性能通过；性能结论仍以用户部署新 mounter 镜像后的集群实测为准。

## 方案比较

### 方案 A：把 mounter ready 收敛为本地挂载健康（采用）

`ReadyStatus` 不再执行 `/bin/ls`。它仍等待真实 `fuse.s3fs` mount 出现、记录 exact mount ID、
再次校验该 mount 未被替换、检查 s3fs 子进程未退出，并扫描本地缓存及执行软限制保护。
Kubernetes 和 Docker runtime 随后继续执行已有的 sandbox `write-read-delete` 传播探针。

该方案消除重复远端 I/O 和长时间锁占用，不增加协议与配置，同时把两层就绪语义划分清楚：
mounter ready 表示本地挂载组件可工作，runtime propagation probe 表示 sandbox 可以安全交付。

### 方案 B：新增 `health mounted` 子命令（不采用）

API 调用轻量 `mounted`，kubelet 继续调用原 `ready`。这会增加 mounter 控制协议、兼容矩阵和
版本协商，而且 kubelet 的远端 `/bin/ls` 仍可能持锁并阻塞 API，因此没有消除主要竞争。

### 方案 C：保留现状并放宽 p95（不采用）

现状功能正确，但会为每次领取重复承担对象存储目录读取，延迟与后端抖动直接进入创建链路，
不符合热池用于稳定低延迟交付的设计目标。

## 就绪语义与详细流程

修改后，`ReadyStatus` 在同一把 Supervisor 锁和调用方 context 下执行以下流程：

1. 检查调用 context；已取消时立即退出。
2. 检查受监管 s3fs 进程是否已退出；退出时将状态置为 `unhealthy` 并返回安全诊断。
3. 只接受 `mounting` 或 `ready` 状态，且必须存在受监管进程和 `MountInfo` 检查器。
4. `mounting` 状态继续在原 mount deadline 内轮询，直到 exact workspace 路径出现真实
   `fuse.s3fs` mount；超时和取消语义保持不变。
5. `ready` 状态继续验证已记录的 exact mount ID，拒绝卸载、替换或类型变化。
6. 对本轮获得的 mount ID 再执行一次 exact active-mount 校验，然后才记录 mount ID 并把
   Supervisor 状态切换为 `ready`。
7. 扫描本地 cache 状态；扫描失败或达到 cache soft limit 时保持现有 fail-closed 行为，必要时
   终止 s3fs 进程。
8. 返回完整 `MounterStatus`，不执行任何远端目录读取。

Kubernetes `WaitSandboxReady` 与 Docker `WaitSandboxReady` 仍必须在 mounter ready 后立即执行
既有 `workspace-probe write-read-delete`。该探针继续承担 exact runtime/generation 绑定、真实
`fuse.s3fs` 可见性以及端到端读写删除传播证明，只有它成功后 runtime 才能交付 sandbox。

Kubernetes readinessProbe 也会因此变为轻量本地挂载健康检查，Pod 可能在强传播探针完成前的
短暂窗口进入 `Ready`。这些受管 runtime Pod 不是面向流量的 Service endpoint，且 API 不使用
PodReady 作为交付授权，所以这不会形成交付旁路。`PodReady` 表达组件运行健康；API 的传播
探针表达客户可用，两者职责不再重复。

## 安全、可用性与可观测性

- 不放宽 RuntimeUID、PoolKey、Generation、Pod UID、mount type、mount ID 或容器重启校验。
- 不删除传播探针，也不将其失败降级为告警；失败继续触发领取失败和现有补偿清理。
- s3fs 早退、挂载超时、挂载被替换、cache 扫描失败和 cache 超限继续 fail closed。
- kubelet readinessProbe、API 主动检查和本地 cache 检查继续存在，但不再产生重复对象存储请求。
- 第一阶段已有的 `mounter_ready_wait` 与 `propagation_probe` 指标阶段保持不变，可继续分辨
  本地挂载耗时和远端传播耗时。
- 不修改 `/bin/ls` 的镜像系统检查契约；本次只消除每次 ready 调用中的执行，避免扩大镜像
  兼容性变更范围。

## 测试策略

按测试驱动方式实施：

1. 先修改 Supervisor 测试，要求首次从 `mounting` 进入 `ready` 时会轮询真实 mount、记录
   exact mount ID，但 `Runner.Run` 调用数保持为零；旧实现必须因执行 `/bin/ls` 而失败。
2. 保留并运行调用方取消不污染 Supervisor、mount deadline、s3fs 早退、mount identity 变化、
   cache 统计失败和 cache soft-limit 等现有测试。
3. 检查 Kubernetes 与 Docker runtime 测试仍明确断言：mounter ready 成功后必须执行
   `workspace-probe write-read-delete`，且传播失败时不交付 sandbox。
4. 运行 `internal/mounter`、Kubernetes runtime、Docker runtime 的定向测试与 race 测试，随后
   运行全仓 Go 测试和 `go vet ./...`。

## 部署与验收

本阶段只修改 `workspace-mounter` 二进制，因此只需要重新构建并发布
`sandbox-fuse-mounter` 镜像。sandbox-api、普通 sandbox、AppArmor loader、Redis/Sentinel 和
Chat 镜像无需因本阶段重新构建；Helm values 只需把 mounter 镜像 tag 更新为新版本。

mounter 镜像变化会进入 FUSE Pool key/backend fingerprint。Helm 更新时应按现有 drain 与重建
流程替换旧 FUSE Pool，不修改普通 sandbox Pool。

部署后在 `ds-ai-research`、`aiadp-sandbox-fuse` 重新执行：

- 不少于 30 次轻量跨 API 副本的 FUSE 创建、写入和销毁；
- 完整 deployed API remediation suite，并单独统计 FUSE 创建分位数；
- 创建成功率 100%，热池创建 p95 目标不高于 3 秒，且无 10 秒周期长尾；
- 所有请求继续通过真实读写、flush 和销毁，Pool 自动回补到目标容量；
- 三个 API 副本无关键错误，集群无 active sandbox、Redis 租约、Pod 或 NetworkPolicy 残留。

若 p95 仍高于 3 秒，再依据 `mounter_ready_wait` 与 `propagation_probe` 分段数据定位后端挂载或
传播探针本身，不继续通过削弱身份、挂载或端到端 I/O 校验换取延迟。
