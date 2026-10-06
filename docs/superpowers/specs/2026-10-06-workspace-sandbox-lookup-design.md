# 按工作空间查回沙盒

## 目标

App 丢失 workspace → sandbox ID 映射后，可以从 sandbox 服务查回已有的可用沙盒，恢复映射并继续使用。租约冲突不能触发已有沙盒删除、owner 清除或强制接管。

用户已于 2026-10-06 确认按推荐的查询方案实施。

## 已确认的现状

- 管理 API 和 Go SDK 目前仅提供按 sandbox ID 查询。
- `WorkspaceOwner` 已持久化 sandbox ID、存储身份哈希、bucket、规范化 prefix、runtime ID/UID 和 generation，可直接作为已有沙盒的查找入口。
- Kubernetes active record 是生命周期权威记录；非分布式运行还有本地内存和持久化 session。
- owner 的存在不等于沙盒已创建完成或可执行，需要结合生命周期和 runtime 验证。
- 服务鉴权是单个服务 API key，没有用户或租户 principal。因此本次归属校验是工作空间与沙盒的关系校验；不能声称提供用户或租户授权。
- 当前创建失败会回收本次创建或借出的 runtime；这与删除工作空间已经存在的 owner 沙盒应明确区分。

## 方案比较

1. **按工作空间查询（推荐）**：新增查询 API，App 明确恢复映射后复用；不改变创建语义，满足当前映射丢失问题。
2. **创建时自动复用**：减少 App 的查询步骤，但需要定义配置兼容、并发创建和等待发布的语义，不能默默忽略新请求中的资源、网络或挂载要求。
3. **同时支持查询与可选创建复用**：能力完整，但本次范围和并发验证更多。可在查询能力稳定后增加显式复用选项。

## 推荐接口

新增 `GET /api/v1/sandboxes/by-workspace?workspace_path=...`，遵循现有 API key 鉴权。Go SDK 新增 `GetSandboxByWorkspace(ctx, workspacePath)`。

成功返回 HTTP 200，包含现有 `SandboxResponse` 字段以及 `workspace_path`、`workspace_mount_mode`，供 App 验证并恢复映射。成功意味着返回时沙盒可用；后续请求仍走现有生命周期准入检查。

路径使用现有工作空间校验规则和 `BuildWorkspacePrefix`。存储 provider、identity、bucket 和 subpath 来自服务配置，不能由请求任意指定。

## 查找与校验

1. 对启用工作空间协调器的对象存储部署，直接读取完整存储身份对应的 owner key，不依赖 App 映射，也不要求新建索引。
2. 用 owner 中的 sandbox ID 读取权威 active record；非分布式运行读取本地快照或只读 session，包括旧格式 session。
3. 对未使用协调器的历史沙盒，通过已有生命周期记录和只读 session 查找已挂载的工作空间。存在多个候选时返回歧义冲突，不能任意选择。旧 Docker local session 通过实际可写 `/workspace` bind mount 验证当前物理目录；没有 owner 的旧对象存储 sync session 没有可证明后端存储身份的元数据，返回 409。
4. 校验工作空间完整存储身份、规范化 prefix、sandbox ID、runtime ID/UID、owner generation 和挂载信息一致。历史记录缺少某些字段时，仅在现有元数据足以证明关系和 runtime 身份的情况下返回。旧 Docker local session 缺少独立 UID 时，可用实际检查所得的完整 immutable container ID 验证；Kubernetes pod UID 不能从名称推测。
5. 检查生命周期允许访问、工作空间没有未完成转换、沙盒未过期，以及精确 runtime 仍运行。FUSE 使用现有可用性规则。
6. 对协调工作空间，在返回前复读 owner，若身份或 generation 变化，返回可重试冲突。查回不迁移 session、不更新配置、不续 TTL、不接管租约。

## 错误与 App 行为

- 400：路径无效。
- 404：没有可查回的工作空间沙盒，也没有阻塞该工作空间的 owner/lease。
- 409：owner 已存在但尚未发布、销毁中、租约状态不能确认、记录不一致、归属不能证明或存在多个候选。使用稳定错误码区分原因。
- 503：状态存储或 runtime 检查不可用；不能当成不存在。

App 映射丢失时先调用查回接口。200 时校验 workspace 后恢复映射并复用；404 才尝试创建。如果创建发生租约或 owner 冲突，再次查回并按错误码重试。409/503 都不能触发删除已有沙盒。

服务端记录与 owner 都丢失、仅有孤立 runtime 的情况，不通过猜测恢复归属；该情况由现有审计和恢复机制处理。

## 验证

- App 映射不存在、服务端 owner 和 active record 仍存在时，跨 Manager 查回同一个 ID。
- 持久化旧格式 session 能被查到，查回过程不迁移或删除原记录。
- 相同路径、不同存储身份或 bucket 不能互相查回。
- sandbox ID、runtime UID、generation 不一致时拒绝返回。
- 发布中、销毁中、过期、runtime 缺失和多个历史候选均不能作为可用沙盒返回。
- 租约冲突、Redis/runtime 故障和查询失败不会调用已有沙盒销毁或清除 owner。
- HTTP 路由、错误码、响应字段和 SDK URL 编码保持一致。
- 对相关 Go 包运行单元测试和 race 检查。

## 2026-10-06 用户补充确认

用户要求同一个 workspace 申请直接返回已有可用环境，不把正常租约争用、映射丢失和生命周期恢复处理交给调用方。创建 HTTP 入口增加服务端获取或创建语义，原 Create 作为内部创建原语保留。已有配置和 TTL 保留；只有首次创建使用创建参数。查询接口仍只读。申请在请求期限内等待并发创建及正常状态转换，执行必要的正常生命周期恢复；缺失归属证据不清除旧 owner 或强制删除实例。
