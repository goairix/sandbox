# ds-ai-research 部署后 API 验证

日期：2026-09-13，约 11:31–11:52（Asia/Shanghai）。

结论：主要创建、执行、持久化和共享池复用路径通过，但不能判定为全量验收通过。工作区控制接口存在多副本状态一致性缺口，另有输入、资源、安全网络及错误返回问题。

## 环境与边界

- 集群 `ds-ai-research`，namespace `aiadp-sandbox-fuse`，release `sandbox-fuse`。
- API 镜像 `registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.3.18`，三个副本，测试结束均 Ready，重启数均为 0。
- 对 API Service 和三个具体 API Pod 分别进行 localhost port-forward。跨副本测试明确指定创建、查询、执行和销毁的不同副本，不依赖负载均衡碰巧命中。
- 后端为当前配置的 MinIO，测试均使用唯一、测试专属的工作区前缀。
- 没有重启/缩容 API，没有执行 upgrade、rollback、drain 或 uninstall，没有修改业务数据。
- 没有执行生产故障注入、Redis 中断、节点故障或滚动重启。没有验证外部 Ingress/TLS 链路或依赖包下载；这些不计为通过。
- 现有 FUSE 压力测试按生产环境有界执行：256 个小文件和 16 MiB 上传，不是默认 10,000 文件 / 1 GiB 压测。

## 生命周期矩阵

执行现有 `TestWorkspaceFUSE`，结果七条功能路径通过，一条压力测试在 flush 状态断言处失败；总耗时约 410 秒。

| 路径 | 结果 | 验证内容 |
| --- | --- | --- |
| ephemeral，无工作区 | 通过 | 创建、执行、workspace info 未挂载、销毁 |
| ephemeral，sync | 通过 | 写入、销毁自动同步、重新创建读取持久化数据 |
| persistent，sync | 通过 | 手动同步、销毁、重新创建读取数据 |
| ephemeral，FUSE | 通过 | 写入、销毁最终同步、重新创建读取数据 |
| persistent，FUSE | 通过 | 显式 sync、销毁、重新创建读取数据 |
| 相同前缀 sync/FUSE 冲突 | 通过 | 两种申请顺序均返回 409，释放后可以换模式 |
| 不同前缀 sync/FUSE 同时使用 | 通过 | 两个沙盒隔离写入与正常清理 |
| FUSE 文件系统压力 | 部分通过、最终失败 | 挂载类型、目录、覆盖、追加、truncate、rename、删除、Git clone、256 小文件、16 MiB 上传及大小校验通过；sync 后 info 未报告 flushed |

## 扩展验证

- 三副本 health、ready、鉴权及参数校验通过。
- 普通池：三个副本分别创建，任意副本 GET/exec，其他副本 DELETE，通过。
- FUSE：ephemeral、persistent 的任意副本 GET/exec/DELETE，通过。
- 同步执行 Python、Node.js、Bash；环境变量、stdout/stderr、非零退出码、exec SSE、超时返回 408、超时后再次执行，通过。Bash 同步 stdin 通过；Python/Node.js stdin 失败，见下文。
- 普通、sync、FUSE 三类沙盒的文件上传、文本读取、下载、列表、递归列表、glob、行读取、字符串编辑、行编辑、64 KiB 二进制 SHA256、零字节文件、非法路径拦截，通过；不存在文件下载的状态码失败。
- multipart：跨副本 init、分块、status、complete、cancel；不完整上传拒绝、取消后 404，通过（普通和 sync 实测）。
- skills：写入专属 SKILL.md、列表、正文、附属文件、缺失和路径越界校验，通过。
- 运行用户 UID 1000，NoNewPrivs=1，Seccomp=2，不能写入 `/etc`，通过。AppArmor/SELinux 未进行生产验收，见配置提示。
- 普通沙盒三并发创建、跨副本执行、销毁，通过；FUSE 三并发创建、跨副本执行、销毁，通过。
- 通过其他副本更新 TTL 后自动过期、API 不再返回该沙盒，通过。
- 明确设置 `tmp_disk` 走直接创建时，内存/CPU 限额与请求一致，执行和销毁通过。只指定内存/CPU 的路径失败。

首次脚本误将 `/files/read` 的文本响应当成 JSON；复测已按真实接口的文本响应验证。行编辑按实际行内容验证，不将尾部换行差异误报为业务失败。

## 共享池与性能

申请前保存 Pod UID，申请后比对 API 的 runtime_id 和 Kubernetes UID。普通及 FUSE 均实际领取了已有预热 Pod，不是每次冷建。三并发采样的六个 UID 均与申请前一致。

| 三并发采样，每类 n=3 | API 创建耗时 |
| --- | --- |
| 普通池 | 0.188、0.196、0.200 秒 |
| FUSE 池 | 0.870、8.178、9.951 秒 |

这是 localhost port-forward 链路的有界样本，不是持续压测或分位数统计。FUSE 在库存已预热时仍有明显长尾，需要单独定位；不能用普通池结果推断 FUSE 性能已达标。普通销毁多次约 31–33 秒，需结合 Pod graceful termination 评估，不与创建延迟混算。

## 已确认问题

1. **工作区控制接口未完整适配多副本。** 动态 mount 在创建副本执行后日志显示成功，但 handler 再读取 workspace info 时发生 nil 指针 panic，返回 500。持有已创建的 sync 沙盒，向其他副本执行 sync，返回 400 `NO_WORKSPACE_MOUNTED`。FUSE unmount 打到其他副本时返回 404 `SANDBOX_NOT_FOUND`，而不是 409 不可变工作区。相关入口仍读取进程本地 sandbox/workspace 状态。
2. **动态挂载清理留下 owner/lease。** 沙盒 `sandbox-8n2th3nl1q` 的 runtime Pod `sandbox-pool-q9aqkmmqfx` 已删除，active/session 记录已清理，但测试专属 `workspaces/validation/api-1789270566488168000/dynamic/` 仍有 persistent owner 和持续存在的 TTL lease。这是上面控制状态缺口的清理后果；未强删 Redis capability。
3. **FUSE flush 信息未更新到可读取的共享状态。** 创建副本 sync 成功后，三个副本的 info 都返回 mounted=true、mount_state=ready，但没有 flushed=true/last_flushed_at，last_synced_at 仍为零时间。现有压力测试因此失败。前面的重新打开读数据通过，不等同于这条状态契约通过。
4. **旧普通库存退役持续重试不存在的 Pod。** 三副本反复报 `obsolete ordinary pool retirement will retry: pods "sandbox-pool-fsxdpy3odp" not found`，测试开始前就已出现，测试结束仍存在。Kubernetes GetSandbox 的 Pod NotFound 没有在这一调用链统一映射为 runtime.ErrNotFound，导致记录退役被阻断。
5. **Python/Node.js stdin 被脚本启动方式占用。** 传入 `stdin-fixture`，程序读取 stdin 得到空值，退出码仍为 0；Bash 同样请求能读到输入。buildCommand 使用 here-document 通过 stdin 传程序文本，与用户 stdin 冲突。
6. **只指定内存/CPU 时请求未生效。** 请求 memory=128Mi、cpu=100m，实际领取的普通 Pod limits 仍为 memory=512Mi、cpu=500m。加 tmp_disk=64Mi 进入直接创建路径后，limits 正确为 128Mi/100m。
7. **不存在文件下载返回 500/EOF。** 普通、sync、FUSE 三类均复现，预期为明确的 404 FILE_NOT_FOUND。已有文件和二进制下载通过。
8. **内部网络白名单连接失败。** 网络关闭和 private range 拦截通过；开启网络并设置 API Service IP 或具体 API Pod IP 的 `/32` 白名单，接口返回 200，NetworkPolicy 包含该 CIDR，但连接超时。具体 Pod IP 连续五轮、约 17 秒重试仍失败。需要排查 Cilium 对集群身份/CIDR 的匹配及实际策略，不能仅凭 HTTP 200 判定网络更新生效。
9. **one-shot SSE HTTP 收尾不完整。** stdout 与 done(exit_code=0) 都已送达，但约 33 秒后读取 HTTP chunk 终结时抛 IncompleteRead。handler 有 20 秒写 deadline，返回前的同步 Destroy 常超过 30 秒；这是收尾路径需要检查的边界，不是代码未执行成功。

## 配置与验收提示

- 启动日志提示 `workspace.allow_missing_lsm_for_kind` 已开启，不视为生产 AppArmor/SELinux 强制隔离验证通过。
- Redis 当前为 standalone 单 Pod，多副本 API 不等于状态存储已经高可用；本次没有进行状态存储故障验收。
- namespace 中已有约 14 小时的 NetworkPolicy `sandbox-sandbox-pool-ovykiqgexb`，对应 Pod 当前不存在。它在本次测试前已经存在，未将其作为测试资源删除。

## 清理结果

- 本次测试成功创建的 sandbox runtime Pods 均已删除。结束时 managed Pods 仅剩普通 prepared 池 3 个、FUSE prepared 池 3 个。
- 当前 Redis 无 active sandbox / session 记录；workspace generation fencing 计数保留，不作为需要清零的业务会话。
- 四个唯一 validation 测试前缀及矩阵前缀 `matrix/minio-sigv4-path-style-v1/kubernetes/1789270370447262000/` 的测试对象已删除，并重新列表确认为空。矩阵删除前核对了全部对象名及四条生命周期文件的固定测试内容。
- 上述动态 mount 留下的一组 owner/lease 未进行裸 Redis 删除，需按修复后的安全生命周期协议处理。不能把“Pod 已清理”描述成“所有逻辑状态已清空”。
- 已关闭本次测试的四个 localhost port-forward，删除两个测试专属的临时 MinIO CLI 配置目录。
- 没有调整线上部署；本报告记录测试与问题，不包含产品代码修复。
