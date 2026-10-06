# 工作空间历史 owner 线上恢复

2026-10-06 13:19–13:27（Asia/Shanghai），使用用户更新的 ds-ai-research 凭据处理 aiadp-sandbox-fuse 的实际业务阻塞。线上 API 已由用户更新到 v0.3.36；此次未构建镜像或修改 Deployment。

## 故障与现场证据

工作空间 `users/01965c2b-cd08-787f-8b3e-251bfef61133` 的持久 owner 指向 `sandbox-jszyha0d59`，runtime 为 `sandbox-pool-blw6qdsldm`，UID 为 `bc6438d4-4248-48c2-abe9-9f6758b92f05`，generation 为 13。owner 最后更新时间为 2026-09-29T06:21:08.90057092Z。

实际 POST 创建请求复现 HTTP 503 / WORKSPACE_RECOVERY_REQUIRED / sandbox_record_missing。新版获取或创建流程没有消除这条升级前遗留的悬空归属；此前仅返回明确错误仍未解除业务阻塞。

重新核验：

- 完整集群 Pod 列表中，旧名称与旧 UID 均不存在；所有节点 Ready。
- workspace lease 不存在；按 sandbox ID 扫描所有生命周期、控制器及 session key 均无结果。
- FUSE pool 记录中没有旧 runtime 名称或 UID；集群 NetworkPolicy 中也没有其身份。
- namespace、state scope、backend bucket/subpath 与当前业务配置一致。
- generation 计数器仍为 13。

只读审计工具仍报告 runtime_scope_unconfirmed，因为旧 owner 不存储 namespace/release 绑定。此次通过集群级现场核验执行限定实例的运维恢复，没有放宽服务端通用归属验证规则。历史记录具体丢失原因尚无证据，不能断言是 Redis TTL、重启或某次人工清理造成。

## 恢复动作

现场证明及 owner 快照先写入本地权限 0600 的备份文件。Redis 原子脚本再次核对 owner 精确内容摘要、generation、lease、请求锁、生命周期及 pool 记录；任一变化均拒绝写入。该脚本已在隔离 Redis 验证 owner 变化、活跃租约、generation 变化、状态/池记录出现、请求占用、备份已存在等拒绝分支，以及成功分支保留 generation 和精确归档。

脚本将这一条 owner 原样归档到 `sandbox:workspace:recovery:20261006:dd802cb5526b98225c0b49a66dc0056776431f3f160465d650c12566aae15c37`，再清除其原 owner key。三台 Sentinel Redis 成员读回均确认归档与原快照完全一致、旧归属已解除。工作空间对象与 generation 计数器未被清除。

## 实际业务工作空间验证

带原 WorkspacePath 及 FUSE 模式调用创建接口成功，HTTP 201，返回 `sandbox-cjv542zoz3`，persistent、ready，使用运行实例 `sandbox-pool-lray20m7t6`，其 Pod 为 2/2 Ready。创建采用服务端默认 TTL 3600 秒，创建时间为 2026-10-06T05:23:34.374975119Z。新 owner 的 generation 为 14。

- 对三个 API 副本发起 9 个并发创建请求：全部 HTTP 201，全部返回同一 ID `sandbox-cjv542zoz3`。
- 每个 API 副本执行只读 bash 命令：全部 HTTP 200、exit_code=0，工作目录为 /workspace。
- workspace/info 返回 mounted=true、mount_type=fuse、mount_state=ready。
- 按 workspace 查询返回 HTTP 200 及同一 ID。
- 新 active 状态记录 TTL=-1，使用不自动过期的生命周期记录承接恢复；正常 session TTL 不会单独丢失这份 active 记录。

验证环境保留为该工作空间的可用实例，供业务后续申请直接复用。端口转发和隔离 Redis 验证容器在验证后停止。API key 仅在进程内存中读取，未写入命令行、日志或报告。
