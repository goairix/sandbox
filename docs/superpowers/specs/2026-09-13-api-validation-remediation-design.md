# 部署后 API 验证整改设计

状态：用户于 2026-09-13 确认；代码整改、独立复审与本机验证完成，部署后现场验收待执行。

## 目标与边界

消除 `docs/testing/2026-09-13-ds-ai-research-api-validation.md` 中可复现的九类缺陷，并定位 FUSE 预热申请长尾。保留已确认有效的普通/FUSE 共享池、跨副本数据直连和持久化行为。

- 在当前 checkout 工作，不创建 worktree。
- 延续已批准的 Kubernetes 无状态多副本架构：Redis 是权威控制面，当前 API 直接访问 runtime，不增加 API 间 HTTP/gRPC 转发或连接亲和。
- Docker 保留单进程生命周期，但执行、资源和文件公共接口的缺陷同步回归。
- 保留 UID、workspace generation、CAS 和 fail-closed；不以取消隔离、强制删除或盲目重放请求换取成功。
- 镜像仍由用户构建，Helm 仍由用户部署。本次先修改、验证仓库，不直接升级、重启或调整集群基础设施。
- 现有未提交的 Helm env 去重修复和测试保留；本次不自动提交。

## 方案选择

推荐：补齐现有无状态协议，按问题域逐项红绿测试、逐批验收。数据流保持直连，仅生命周期操作增加必要的短小 Redis 原子协调。

不采用两种替代方案：只补 handler 空值判断会掩盖 mount 的持久状态缺口，不能解决远端 sync/unmount 和清理遗留；HTTP 转发或粘性路由会重新绑定 API owner，违背既有设计与故障接替要求。

## A：工作区多副本与清理协议（最高优先级）

### 已复核根因

`internal/sandbox/workspace.go` 中 mount/unmount 读取本地 sandbox、workspace 和 gate；mount 仅发布本地状态及 session/ephemeral lifecycle，没有更新 authoritative active snapshot。sync 获取分布式 sandbox 后仍从本地 workspace map 读取 ScopedFS。FUSE flush 只更新本地快照和恢复记录，没有完成共享 active 更新。`internal/api/handler/workspace.go` 在 mount 后忽略 info error 并解引用可能为空的 info。

### 设计

1. Kubernetes 的 mount、unmount、sync 和 FUSE flush 从分布式 gate 返回的权威快照确定存在性、挂载模式和 runtime 身份；不要求请求副本已经保存本地 map。
2. 普通 get/exec/files 继续并发。工作区 mount/unmount/FUSE flush 使用有期限、可续期、generation-fenced 的分布式排他 admission：先阻止新冲突操作，再排空已进入的数据流，最后执行工作区副作用。现有 mutation token 只串行 mutation、未排空数据流，不能冒充这个排他能力。普通 TTL/network mutation 不因这次修复被全局串行化。
3. sync 重建请求级 ScopedFS，明确传递 sandbox/workspace 快照和时间戳，去除核心复制流程对本地 map 的隐式依赖。对象内容不经过 Redis，不整体载入内存。
4. mount 成功前关联 workspace owner、恢复记录、Config.WorkspacePath/mode 和 active snapshot。使用阶段记录与精确 readback 处理跨 key 的非原子发布；任一副作用之后的持久化不确定都必须保留可恢复证据，不能返回成功或直接回滚删除证据。
5. unmount 按排空、最终同步、持久阶段推进、精确停止对应 renewal、owner/lease 释放、发布未挂载状态的顺序执行。中途失败进入可恢复阶段，禁止留下已释放 owner 却仍可写入的挂载状态。
6. 生命周期 controller 只负责后台续租、检查和恢复，不转发用户请求。后台自动同步、请求同步与销毁使用同一协调边界；旧 controller/token 在每次副作用和共享写入前被校验。不要让每个请求副本都启动 owner renewal。
7. FUSE flush 在 quiesce/flush/resume 与 UID/generation 健康核验成功后，CAS 发布 flushed、last_flushed_at 和 last_synced_at。任何 pending/failed 状态不得报告成功，其他副本必须读到同一快照。
8. handler 不忽略 info error、不解引用 nil；防御性校验不能替代上述权威发布。

### 遗留状态整改

新增有界、默认只审计的工作区孤儿恢复路径，先区分活跃 owner、正在发布/清理的 owner、真正失去 runtime 的 owner。不新增裸 Redis DEL 操作。

- 对报告中的专属测试 owner，必须核对 scope、sandbox ID、Runtime ID/UID、workspace generation，确认无匹配 active/session/ephemeral 恢复记录，且原 runtime 的终止证据明确。
- 在新代码中让本地续租者发现权威关联消失后停止；仍被旧进程续租的 owner 不强抢。
- 可执行恢复使用精确 token/generation CAS，保留 fencing generation 计数；UID 被替换、状态损坏、数据最终同步无法证明时只报告阻塞与可能的数据损失，不伪造已完成同步。
- 仓库修复和用户部署之后，再单独核对并清理报告中的遗留 owner。当前阶段不修改线上 Redis。

### 验收

三个 Manager 共享真实 Redis，覆盖任意副本 mount/info/sync/unmount/delete、两种 sync 方向、FUSE 不可变返回 409、flush 时间戳一致、同前缀冲突、并发 streams 排空、TTL/自动 sync/destroy 竞争。覆盖发布失败、回复丢失、controller/operation lease 失效、旧 token 恢复、UID 替换与每阶段重试。清理成功后 active/session/lifecycle/owner/lease 均无遗留，generation 保留。

## B：普通库存与资源契约

- 在 Kubernetes runtime 的 GetSandbox 边界统一将真正的 API NotFound 映射为 `runtime.ErrNotFound`；403、网络错误或超时不能当作不存在。旧库存仅在 owner-free、record CAS 与精确 UID 校验成立时退役；不存在 Pod 的记录也应能完成退役，不因单条旧记录永久阻塞其他记录。
- 普通池采用与 FUSE 一致的有效资源契约比较。未指定或与池实际资源相同的请求继续复用；memory/CPU/disk/tmp_disk 与池不兼容的请求直接创建。直接创建补齐未指定的池默认限额/requests，不能只在 tmp_disk 请求时补默认值。
- 验收请求值与真实 Pod/container limits；覆盖默认请求、等价 Kubernetes quantity 表达、不兼容的单字段与组合字段、Docker 默认值，以及 claimed/UID 替换不可误删。

## C：执行与文件接口

- Python/Node 使用经过严格 shell quoting 的解释器参数传递代码，不占用用户 stdin、不使用固定 heredoc 分隔符。覆盖单引号、换行、Unicode、shell 元字符、超长输入、stdout/stderr 和同步/stream/one-shot 三种入口；Bash 语义保持。
- 下载在提交 HTTP 成功响应前区分不存在文件、目录/不可读、runtime 不存在、权限和 transport 故障。不存在的普通/sync/FUSE 文件返回 404 `FILE_NOT_FOUND`，不能把所有 EOF、所有 exec 失败都当作不存在。保持零字节和大文件流式传输、关闭与取消正确，避免重复整个文件读取。
- one-shot SSE 的执行响应生命周期与耗时清理分离。返回前先持久关闭 sandbox admission 并留下可接替的 cleanup 状态；清理由有界、受跟踪的后台流程完成，不新增无证据的 fire-and-forget goroutine。协调器覆盖普通、sync、FUSE 的 cleanup 恢复。SSE 完成时正确处理写 deadline，HTTP chunk 终结、客户端断开和服务停止均要测试。普通/FUSE graceful termination 不直接改为强删。

## D：Cilium 网络与性能

集群当前使用 Cilium v1.18.4。该版本官方策略说明明确指出：CIDR/ipBlock 不能选择双方均为 Cilium 管理的 Pod，内部流量需使用 endpoint/service/entity 策略。因此目前只生成 /32 ipBlock 的内部白名单不能视为正确实现。[Cilium v1.18.4 策略说明](https://github.com/cilium/cilium/blob/v1.18.4/Documentation/security/policy/language.rst)

- 外部白名单继续使用经验证的 CIDR。内部目的地需要显式的 Service/endpoint 语义与 namespace 范围，禁止自动将一个 IP 放大为整个集群、namespace 或任意相同宽泛标签的访问权。
- 保留现有字符串白名单兼容性；若字面 Pod/Service IP 在当前 CNI 下不能安全表示，应明确拒绝并提示可支持的内部目标格式，而不是返回 200 但永远不通。内部目标的公开表达、RBAC 和 Cilium allow/deny 更新必须一并提供测试与迁移说明。
- 核对 Cilium deny 优先级和例外，更新失败 fail-closed。集群验收既测目标可达，也测同 namespace 非目标、其他 Service、private range、link-local 仍不可达；不能只检查 manifest。
- FUSE 长尾先新增分阶段观测：pool acquire、prefix preparation、lease binding、authorize、network、Pod readiness、probe 与 active publish。当前 readiness probe 周期 10 秒、WaitSandboxReady 等待 PodReady，是与 8–10 秒现象吻合的候选原因，尚未证明是唯一根因。
- 依据观测优化就绪等待；不跳过 Pod/UID/generation、真实挂载读写 probe 或隔离检查，不默认缩短集群所有探针。性能验收使用相同预热库存、至少 30 次串行与有界并发采样，比较 p50/p95/p99 和出错率，同时核对 UID 复用、池恢复与资源开销；不给三个样本承诺性能达标。

## E：部署配置与验证交付

- Helm 默认 `allowMissingLSMForKind=false` 已存在，不重复增加同名配置。提供生产 values 示例与校验，要求明确、有效的 AppArmor/SELinux profile；不自动关闭线上绕过开关导致未准备节点无法启动。
- 当前 standalone Redis 的非 HA 风险属于部署整改。沿用已有 external sentinel/cluster、durability、requireHA 配置，提供生产配置与持久性约束说明；不在仓库修复中擅自迁移在线 Redis。
- 报告中的旧 NetworkPolicy 按精确受管归属和无活跃 runtime 证据审计，不能按年龄或名称前缀批量删除。
- 每个问题先观察回归测试失败，再实施最小修复；完成针对性单测、真实 Redis、多副本/race、Docker/Kubernetes runtime、handler、全量 `go test ./...`、build/vet、Helm lint/render/env 唯一性测试。
- 将临时集群测试固化为仓库中的可重复有界验证，覆盖报告的失败项；为压力与故障注入设置显式 opt-in，不把未执行项计为通过。
- 交付改动清单、配置变化、已执行测试及限制、需要用户构建的镜像/同步的 Chart。用户部署后再复跑现场报告矩阵；产品代码本地通过不等于线上整改完成。

## 推荐实施批次

1. A：权威工作区操作、持久发布和安全恢复。
2. B/C：资源、库存 NotFound、stdin、下载及 one-shot cleanup。
3. D：内部网络目标契约、Cilium 回归和 FUSE 分阶段性能优化。
4. E：配置校验、集群验证固化、整体复审与交付；部署后另行验收线上遗留状态。

每批具有独立的测试和验收结果；不能用普通池高性能或生命周期矩阵通过代替网络、跨副本工作区、清理和故障接替验收。
