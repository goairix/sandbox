# Kubernetes 普通 Runtime 网络身份与恢复设计

## 背景

Docker runtime 修复文件上传和网络资源恢复后，对 Kubernetes runtime 的
同类路径进行审查，确认普通 sandbox 存在以下问题：

1. Pool Pod 创建时使用 pool runtime ID 作为 `sandbox.id` 和 NetworkPolicy
   selector；领取后 Pod 的 `sandbox.id` 被改成逻辑 sandbox ID，但策略没有
   同步迁移。
2. `UpdateNetwork` 和 `RemoveSandbox` 接收的是 Pod/runtime ID，却直接把它
   当成策略使用的逻辑 ID。
3. `RemoveSandbox` 丢弃 NetworkPolicy 与 CiliumNetworkPolicy 删除错误。
4. Pod 先创建并 Ready，普通 NetworkPolicy 后创建；没有 namespace-wide
   default-deny 的部署存在无策略窗口。
5. 普通 managed NetworkPolicy 没有启动期孤儿回收。
6. Pod exec 上传失败后，临时文件清理错误被丢弃。

FUSE Kubernetes runtime 已使用不可变 instance selector、Pod UID 绑定、精确
删除和启动孤儿恢复。本设计不修改 FUSE 行为。

## 目标

- 普通 Pod 的所有策略始终选择 Pod 当前的逻辑 `sandbox.id`。
- 新普通 Pod 出现前，所需标准 NetworkPolicy 以及可选 Cilium private-deny
  已经存在。
- Pool 领取期间，旧策略到新逻辑 ID 策略的切换不产生无策略窗口。
- 更新和删除均从 exact Pod 读取最终逻辑 ID，不再猜测。
- 所有清理失败向调用方返回，不报告虚假的成功。
- 启动时安全回收没有 managed Pod 与之对应的普通策略。
- 上传失败同时保留主错误与临时文件清理错误。

## 非目标

- 不修改 FUSE system/user policy 和 exact-UID teardown 协议。
- 不改变用户网络模式、CIDR/FQDN 解析或 Cilium private-range 规则。
- 不处理 CNI 自身的地址池耗尽；这不属于 sandbox 创建的 per-Pod 资源。
- 不要求 Helm default-deny 存在，尽管仍建议保留该纵深防御。

## 策略身份

普通 Pod 的逻辑策略 ID 按以下规则解析：

1. `spec.Labels["sandbox.id"]` 非空时使用该值；
2. 否则使用 Pod/runtime ID；
3. 值必须是规范的 DNS-1123 subdomain，且 Pod 必须同时具有
   `sandbox.managed=true`；
4. FUSE Pod 拒绝进入普通策略 helper。

普通标准策略保持名称 `sandbox-<logical-id>`，selector 精确为
`sandbox.id=<logical-id>`。Cilium private-deny 保持名称
`sandbox-private-deny-<logical-id>` 和相同 selector。两类策略都带
`sandbox.managed=true`、`sandbox.id=<logical-id>`，新增明确的
`sandbox.policy.role=ordinary` 或 `ordinary-private-deny` 标签，并用
`sandbox.runtime.id=<pod-name>` 标签记录不可变 runtime ID。Pod 创建成功后再把
Pod UID 写入 `sandbox.runtime.uid` annotation；更新和删除必须同时验证这些身份。

## 创建顺序

`CreateSandbox` 在任何 API 写入前先纯构造 Pod 和两类策略，并验证逻辑 ID。
执行顺序为：

1. 确认目标 Pod 名不存在，并确认没有其他 managed Pod 使用同一逻辑 ID；
2. 使用 Create-only 语义创建标准策略；不得更新或接管同名存量策略；
3. 需要 Cilium private deny 时，同样以 Create-only 语义创建；
4. 再次确认没有其他 managed Pod 使用同一逻辑 ID，关闭检查与策略创建之间的
   控制面竞态；
5. 创建 Pod；
6. 把两类策略绑定到 Create 返回的 Pod UID；
7. 等待 Pod Ready；
8. 返回 runtime identity。

策略先于 Pod 出现，selector 因此在 Pod 调度和启动前已经生效。任一步失败都
使用独立 cleanup context 清理本次创建的资源，并通过 `errors.Join` 保留主错误
与清理错误。Create 响应不确定时不得覆盖或删除无法证明属于本次 attempt 的
策略。

## Pool 领取迁移

只有普通、仍带 `sandbox.pool=true` 的 Pod 可以通过 generic `UpdateLabels`
同时删除 pool 标签并更换 `sandbox.id`。迁移顺序为：

1. GET exact Pod，记录 UID、resourceVersion 和旧逻辑 ID；
2. 确认新逻辑 ID 没有被其他 managed Pod 使用；
3. 读取旧标准策略并确认其 name、managed label 和 selector 与旧 ID 完全匹配；
4. 从旧策略复制现有 egress 语义，以新 ID 创建新策略；Pool 只会携带隔离策略，
   但复制已验证对象避免把该假设隐藏在迁移代码中；
5. 用 UID 与 resourceVersion 前置条件 patch Pod labels；
6. 删除旧策略，并返回任何删除失败。

如果新策略创建后 Pod patch 失败，只删除带本次迁移 attempt 标识且 UID 匹配的
新策略。旧策略和旧 Pod label 保持有效。Manager 不再忽略 `UpdateLabels`
错误；迁移失败时销毁已领取 runtime、通知 Pool 补充，并返回创建失败。

普通 Pool 不创建 Cilium private-deny，因此迁移不涉及 Cilium 对象。

## 动态更新

`UpdateNetwork(runtimeID, ...)` 必须先 GET exact Pod，验证 managed ordinary
身份并读取当前 `sandbox.id`，随后更新同 logical ID 的标准策略与可选 Cilium
deny。标准策略更新成功而 Cilium 操作失败时返回明确的状态不确定错误；Manager
不得保存新的网络配置。

更新现有策略前必须验证 name、managed label、role 和 selector。缺少新 role
标签的历史普通策略只有在其他所有身份字段完全匹配时才允许升级；不接管第三方
或形状不兼容的对象。

## 删除与错误传播

`RemoveSandbox(runtimeID)` 执行：

1. GET Pod，读取 UID 和逻辑 ID；NotFound 视为 Pod 已消失；
2. 对存在的 Pod 使用 UID precondition 删除，并观察该 UID 到 NotFound 或被替换；
3. 仅在旧 Pod 已确认消失后删除 logical ID 对应的 Cilium 和标准策略；
4. 兼容删除 runtime ID 命名且身份匹配的历史策略；
5. 用 `errors.Join` 返回 Pod、标准策略和 Cilium 策略的所有错误。

这样不会因为先删 allow/deny 策略而扩大仍运行 Pod 的权限，也不会再把策略清理
失败报告为成功。Manager 的启动期普通 Pool 清理只对实际成功删除计数，失败项
记录 runtime ID 与错误。

若 GET Pod 已经返回 NotFound，则按 `sandbox.runtime.id=<runtimeID>` 列出新格式
策略，并在 name、role、selector、runtime ID 与 runtime UID 绑定形状均合法且
确认 Pod 仍不存在时删除。缺少 runtime ID 标签的历史逻辑 ID 策略留给启动扫描，
不根据调用参数猜测其归属。

## 启动恢复

Kubernetes runtime 初始化后扫描 `sandbox.managed=true` 的标准与 Cilium
策略。只处理满足以下条件的普通策略：

- name 与 logical ID 派生名称完全一致；
- selector 精确为 `sandbox.id=<logical-id>`；
- 新格式策略的 runtime ID/UID 绑定与同名 exact Pod 一致；
- role 为新的 ordinary role，或是其余身份完全匹配的历史普通策略；
- 当前不存在具有 `sandbox.managed=true,sandbox.id=<logical-id>` 的 Pod。

仅满足全部条件才删除。存在 Pod、身份畸形、第三方策略或无法确认 Pod 是否存在时
一律保留。List/Get/Delete 失败使 runtime 初始化失败，避免在资源状态未知时继续
启动。FUSE role 由现有 FUSE reconciler 处理，启动扫描跳过。

## 上传清理

`removePartialPodUpload` 改为返回 error，并验证 cleanup exec 的 transport error、
nil result 与非零 exit code。上传主路径对 body 长度、tar stream 或容器消费错误
仍保持原错误类型，同时通过 `errors.Join` 附加 `cleanup partial upload` 错误。

## 测试

- 新建 Pod 前已经存在匹配的标准策略；Cilium open 模式 deny 也先于 Pod。
- 同名存量策略或同 logical ID Pod 不被覆盖。
- Pool 领取按“新策略 → label patch → 旧策略删除”迁移，失败时精确补偿。
- 动态网络更新从 Pod label 使用 logical ID，策略确实选择当前 Pod。
- RemoveSandbox 等待 exact Pod 消失，再删除 logical/legacy 策略并传播所有错误。
- 启动扫描只回收无 Pod 的 managed ordinary 策略，保留 FUSE、第三方、畸形和有
  Pod 策略。
- Helm default-deny 缺失时，创建顺序仍不存在 Pod 先于策略的窗口。
- 上传失败同时返回主错误和 cleanup 错误，成功 cleanup 不改变原错误。
- 运行 Kubernetes runtime、sandbox manager 定向测试、race 测试和仓库全量测试。

## 部署影响

只需重新构建并部署 `sandbox-api`。Helm default-deny 继续保留。升级后启动扫描会
回收可以证明无对应 Pod 的普通遗留策略；无法证明安全删除的对象保留并阻止启动，
由运维检查，而不是猜测性清理。
