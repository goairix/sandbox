# FUSE 终态暂停超时恢复与发布要求

日期：2026-10-09。namespace：`aiadp-sandbox-fuse`。

## 本次故障及线上恢复

工作空间 `users/01965c2b-cd08-787f-8b3e-251bfef61133` 的旧沙盒为 `sandbox-kpcd6t1meo`，runtime 为 `sandbox-pool-63blm5vzs2`，UID 为 `3a190aec-981f-424f-b0c8-7ac5027e6f2c`。API 和该 runtime 均为 v0.3.39，不能归因于旧 v0.3.4 runtime。

首次停止核验失败后，一次 Kubernetes exec 超时使本地 quiesce cycle 永久 poisoned。之后的定时回收仅返回 previous attempt failed，没有再次检查进程；workspace 的 creating 请求等待 cleanup 直至返回 503。检查时，业务 sh PID 385、python3 PID 391 都为 T，没有可写的 /workspace 文件描述符。最初 exec 超时的基础设施原因尚未证明。

在用户已有接手恢复授权下，核对 exact Pod UID 与 active record 后，正常重启单个 API `sandbox-fuse-api-7998d55fff-jnt47`。替代副本为 `sandbox-fuse-api-7998d55fff-5fzv7`。未杀业务进程、未强制删除 runtime、未人工清除 Redis owner/lease。旧 runtime 经现有服务正常回收后不存在，旧 active record 的 EXISTS 为 0。

按旧记录的原配置创建：persistent、FUSE、timeout 2100 秒、原网络配置。

| 验证 | 结果 |
| --- | --- |
| 同 workspace 创建 | HTTP 201，sandbox-eqn7nrhatq，ready，reused=false |
| 跨 API 副本重复申请 | HTTP 201，同一 ID，reused=true |
| 按 workspace 查回 | HTTP 200，同一 ID，ready |
| 实际文件 I/O | HTTP 200，exit_code=0；唯一临时文件写入、fsync、读取、删除成功 |
| 新 runtime | sandbox-pool-zbjmeeoplx，UID 6f672035-d1c3-4ac7-97d2-476164a40278，v0.3.39 |
| 生命周期 | 新 active record 为 active；lease generation 为 24 |
| API 副本 | 2mlcv、bm9ln、5fzv7 三个 API 均 Ready |

新沙盒保留供业务复用。以上只证明现有版本的这次线上故障恢复，不代表下面的代码补丁已在集群部署。

## 代码修复

- Kubernetes 新增可选终态 `QuiesceWorkspaceForCleanup` 能力，Manager 销毁时选择该能力；普通暂停/恢复继续保持未知结果不可重放。
- 探针固定命令 `quiesce --runtime-uid UID --generation GEN --cleanup` 先取得可信 PID1 的永久 cleanup fence 确认。fence 与 Resume 的所有 SIGCONT 共享锁，先前执行中的 Resume 必须结束，后续 register/resume 永久拒绝，同 UID/generation 的 fence 可以重复确认。
- 每次终态尝试重新检查完整进程集和 workspace 写句柄，产生新的不可恢复证明。超时不会建立必须查回的 resume broker；缺少确认时不得 flush。
- flush 失败或回包丢失时，Manager 清除本地 quiesced 标记，下一轮重新核验后再调用串行 sidecar flush。只有明确成功的 flush 才能写入 durable checkpoint 并进入原有删除/卸载/归属释放流程。
- 实际 sidecar flush 失败导致 StateUnhealthy 时仍保留 runtime 与归属，不能靠本补丁假报成功。

## 验证

- 针对 lost quiesce reply、lost flush reply、旧 token 失效、并发 quiesce/flush/resume、延迟 PID1 Resume、正在 SIGCONT 时 fence 等待、fence 未确认前禁止扫描，以及可写 workspace FD 的回归测试通过。
- Manager 故障注入：第一次 flush 未确认保留 runtime，第二次重新 quiesce 后正常回收，同 workspace 可重新创建。
- `TEST_REDIS_ADDR=127.0.0.1:16387 go test -race ./... -timeout=3m` 通过，独立测试 Redis 不接触线上状态。
- SDK 独立模块 `go test -race ./...` 通过，SDK/公共 HTTP 响应未修改。
- `golangci-lint run ./...`：0 issues。
- Linux arm64 全 workspaceprobe 测试通过。
- 使用现有基础镜像、绑定本地编译探针，实际 UID 1000 PID1 容器验证：旧普通 quiesce 已建立 cycle 后，cleanup 两次均成功，旧 Resume token 与后续普通 registration 均拒绝。没有构建或推送镜像。

## 发布要求

必须从同一源码构建并配套发布 sandbox-api 和 sandbox-runtime 新镜像。Kubernetes warm pool contract 已升至 v5，避免复用旧模板池；该变化不能升级已发布 active Pod 内的 PID1。

使用本次更新的 Helm chart 发布。cleanup protocol v3 和 runtime control v5 会触发升级前排空；hook 使用已安装 API 镜像、原 env/envFrom、挂载和 Redis scope，旧服务按旧控制协议正常完成 flush、卸载、runtime 删除及状态收尾，再应用新 API/runtime 镜像。不能只更新 Deployment 绕开 hook。首次跨协议升级会终止现有沙盒实例，workspace 文件仍按正常持久化流程保存。跨协议 rollback 会被 fingerprint guard 拒绝；维护步骤与限制见 [Helm 升级文档](../deployment/helm-deployment-upgrade.md)。

## 复审发现及补充修复

- PID1 的 register/cleanup 只接受服务端 Ed25519 授权，绑定命令、exact runtime UID 和 lease generation。未授权或签名不匹配在修改 ledger 前拒绝，不能用错误 generation 永久毒化回收。签名经私有 exec stdin 传递，探针读入前关闭 dump。
- 仅公钥进入 sandbox Pod 环境。每个 state scope 的签名 seed 由 Redis SetNX 持久化，跨 API 副本、重启及 API 密钥轮换保持一致；加载必须取得 durability ACK，异常或损坏不生成替代密钥。该记录属于控制 fencing 历史，无 TTL，release drain 不删除。
- proc 核验遍历每个 /proc/PID/task/TID 的 UID、starttime、state 与 FD 表，核验前后 task 集合及身份稳定。leader T、worker D 不能成为暂停证明；leader 已退出但 sibling 全部停止时可以继续；线程私有的 workspace 可写 FD 必须拒绝 flush。
- owner 已正常释放、session/active 删除仍未完成的合法 pending FUSE 记录会继续现有 checkpoint 恢复，在请求预算内等待收尾。缺少 owner 不能单独授权替代；持久化 flush、consumed/claimed identity、exact UID 终止证据及 controller fence 仍必须全部验证。
- checkpoint 恢复即使读到 owner/lease 都不存在，也必须取得两者 durable absence 确认才可跳过已完成的 Release。删除 ACK 丢失、确认失败会保留旧 active 记录；确认恢复后同 workspace 请求可以完成回收并创建。

补充验证：四项复现与 durable absence 回归均在旧代码 RED、新代码 GREEN；完整 root 与 SDK race、宿主及 Linux arm64 lint、Helm 渲染和脚本验证通过。独立 Redis 7.4 覆盖 state Lua 集成及跨客户端控制密钥恢复。实际 Linux UID 1000 的完整 workspaceprobe 测试包含 private FD/退出 leader，无 skip；PID1 容器验证伪造请求拒绝、合法普通 cycle、两次终态 fence、延迟 resume/register 拒绝。未构建、推送或部署镜像，未执行线上 Helm 升级。
