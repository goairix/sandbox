# 工作空间 FUSE 历史实例恢复验证

日期：2026-10-08。集群 namespace：`aiadp-sandbox-fuse`。

## 故障证据

- 工作空间：`users/01965c2b-cd08-787f-8b3e-251bfef61133`。
- 旧沙盒：`sandbox-bhuudoo595`，runtime：`sandbox-pool-j92vzqpk6u`，Pod UID：`dce5ee34-7836-434b-8835-a40f39bc794e`。
- 三个 API 副本已经使用 `sandbox-api:v0.3.39`，新池使用 `sandbox-runtime:v0.3.39`；旧 runtime Pod 仍使用 `sandbox-runtime:v0.3.4`。更新镜像配置没有替换旧 Pod 内的探针。
- 旧 runtime 中 `truncate` PID 1458 为 `Z`、线程数为 1，父进程 `timeout` PID 1457 和 `sh` PID 1451 为 `T`。旧探针要求僵尸进程变为停止状态，quiesce 持续失败。
- 后续一次 quiesce 执行超时进入结果未知状态，持有该生命周期的 API 副本拒绝重放。旧 active record 仍为 `destroying`。
- 日志中另两个退役 Pod `sandbox-pool-nkk1nfnkte`、`sandbox-pool-7hdx2hnyzf` 在本次检查时均已不存在。

## 恢复操作

用户确认其他操作已经停止并授权接手恢复后：

1. 保存受限权限的旧 active record 快照，核对 runtime UID、工作空间和原配置。
2. 使用 pidfd，在核对 Pod UID、进程启动时间、UID、名称和状态后，取消两个停止的父进程。三个历史 PID 随后均消失；旧 runtime 中没有其他业务进程。
3. 重启单个 API 副本 `sandbox-fuse-api-7998d55fff-n5p7w`，让结果未知的本地控制状态退出并由正常生命周期流程继续回收。其余两个 API 副本持续 Ready，替代副本 `sandbox-fuse-api-7998d55fff-jnt47` 最终 Ready。
4. 旧 runtime Pod 经服务端回收后不存在，旧 active record 的 Redis EXISTS 为 0。没有人工清除工作空间 owner/lease，也没有强制删除旧 runtime Pod。
5. 按旧记录的原配置申请同一个 workspace：persistent、FUSE、timeout 2100 秒，保留原网络配置。

## 线上验证结果

| 验证 | 结果 |
| --- | --- |
| 同 workspace 创建 | HTTP 201，`sandbox-d9talq8w33`，`ready`，`reused=false` |
| 新 runtime | `sandbox-pool-h56mxyvjat`，UID `198a65fa-f66a-42e3-a3f5-0b9ebf650f6a`，镜像 `sandbox-runtime:v0.3.39` |
| 另一个 API 副本重复申请 | HTTP 201，同一 sandbox/runtime ID，`reused=true` |
| 原 API 副本重复申请 | HTTP 201，同一 sandbox/runtime ID，`reused=true` |
| 按 workspace 查回 | HTTP 200，同一 ID，`ready` |
| 实际命令与文件 I/O | HTTP 200，exit_code 0；在 `/workspace` 用唯一临时文件验证写入、fsync、读取、删除，临时文件已清理 |
| Redis 生命周期 | 新 active record 为 `active`；workspace owner 指向新 sandbox/runtime，lease generation 为 22 |
| API 副本 | 三个副本均 Ready |

新沙盒保留供业务线复用。旧 sandbox ID 已退役，调用方应从按 workspace 创建/查询的响应恢复映射。临时端口转发及包含旧状态快照的本地恢复文件在验证后清理。
