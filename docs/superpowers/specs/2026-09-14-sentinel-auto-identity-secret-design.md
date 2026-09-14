# 内置 Sentinel 身份 Secret 自动创建

## 已确认需求

用户确认：线上只配置 values 并执行 Helm，不再手动创建身份或认证 Secret；首次生成身份，升级和卸载重装保留复用，旧状态存在但身份丢失时拒绝重新生成。身份 Secret 手动引用仅作为可选兼容模式。认证 Secret 已有 values 自动创建能力，本轮不改变其认证语义。

本轮在仓库外临时 worktree、基于包含最新 Sentinel 修复的功能分支实施。主目录 main 用于用户打包，不改其文件、分支或工作区。镜像构建、线上部署仍由用户执行；不操作业务 Kubernetes 资源、不清空数据、不回滚。

## 方案选择

推荐普通一次性身份 Job：使用已有 Go Ed25519 生成器，通过 namespace API 创建或验证固定身份 Secret；它不挂载 Secret/PVC，不等待 Redis。与普通初始化 Job 同时创建，后者等待公钥 Secret，API 继续等待 Initialized。SA/Role/RoleBinding 为普通 Helm 资源，正常卸载清理；身份 Secret 刻意保留。

备选 pre-install/pre-upgrade Hook Job 可在普通资源创建前核验旧 PVC，但还需先以 Hook 创建 RBAC，并另外处理其卸载生命周期。纯模板随机生成则不复用现有可信公私钥生成/验证路径，且更容易在渲染中更换身份。本轮不增加这两种并行实现。

## values 与兼容性

- `redis.sentinel.identitySecretName: ""` 默认自动管理，实际名称固定为 `<release>-redis-sentinel-identity`。
- 非空值表示引用已有身份 Secret：验证而不创建、不修改。已有用户填写相同固定名称也按外部引用处理，保留原行为。
- 所有成员自身 seed、公钥、API/初始化 Job 公钥引用使用同一个 effective name helper，保留现有私钥隔离。
- `redis.sentinel.existingSecret: ""` 时，认证 Secret 继续使用 `redis.password` 与 `redis.sentinel.password` 创建。两个不同安全 token、32~256字符的限制不变。
- 不增加新开关；默认 standalone、external 不产生身份 Job、额外 RBAC 或 Secret 行为变化。AppArmor 本次保持关闭。

## 新身份创建授权

普通 Job 必须区分本轮新建 PVC 与先前保留 PVC，不能仅把 Pending 当生成新密钥的许可。

1. Chart 在渲染时读取固定状态 CM、身份 Secret、三个固定 PVC，不使用 list。任一旧状态 CM/PVC 已存在则不授予“首次生成”许可；Secret 缺失时提前拒绝，避免升级改写业务资源。显式外部身份不授予创建许可。
2. 状态 helper 一次计算并缓存本轮状态：保留已有完整状态，或生成新的 clusterID/Pending。CM 与身份 Job 使用完全相同的 clusterID。此缓存属于 Helm root context，不是用户可填写的 values 授权。
3. 仅当渲染时没有旧 CM/PVC、且自动身份模式时，Job 收到本次新 clusterID 的创建许可。它等待普通 CM 创建，固定预算两分钟，可取消；真实 GET 验证 UID/resourceVersion、clusterID、固定三个成员、Pending、无登记。
4. 新 PVC 模板标注本轮 clusterID。创建前 Job GET 三个固定 PVC：缺失允许继续；存在则必须对应本轮新 clusterID，未知/旧标记拒绝。不读取或修改卷数据。
5. 对 CM/PVC 做两轮一致性检查，锁定 CM UID/版本，创建前重新 GET Secret。出现旧登记、phase/UID/membership 变化时拒绝。
6. 使用既有 `GenerateIdentitySecret` 创建三个独立私钥及公钥 JSON。只调用 Secret Create，不 Update/Patch/Delete；AlreadyExists 或创建结果不确定时重新 GET 验证原服务端对象，不覆盖、不先删除、不用重新生成的密钥代替已有对象。
7. 创建后再次读取并验证服务端 Secret、CM 和 PVC。失败明确返回，不把不确定 Create 当成功；不回滚或删除已出现的对象。

上述为可信 Kubernetes/Helm 控制面的正常安装保护，不提供跨 CM/PVC/Secret 原子事务，不防御管理员同时伪造或恢复整套相同身份。实际数据为空仍由已有本地 reader、Reserved marker、三签名登记和配置事务决定；身份 Job 不授权 Redis 晋升或业务写入。

## 已有身份核验

已有 Secret 必须名称/namespace 完全一致，类型 Opaque、immutable=true、只有三个固定成员32字节 seed 和 public-keys.json，公钥严格解析且逐个验证对应私钥。输入有界，不记录原文。存在登记时 keyDigest 必须匹配；CM/membership 异常拒绝。验证不更新 Secret metadata/data、登记、marker、角色或 epoch。

旧 CM/PVC 存在而 Secret 丢失：报“保留身份缺失，需恢复原 Secret”，不自动创建。CM 丢失而旧 PVC 保留也拒绝继续初始化。重新安装不等于新数据库，不因为 release revision 或 Pending 重新生成密钥。

## 权限、生命周期与性能

- 独立 namespace SA；get 限定指定 Secret、状态 CM 和三个固定 PVC。自动模式额外拥有 namespace Secret create；外部模式没有 create。
- Kubernetes 顶层 create 不能通过 resourceNames 限定名称，须如实记录此边界。代码只创建固定名称；没有 list/update/patch/delete、ClusterRole、Pod Exec 或 Node 权限。依据：[Kubernetes RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)。
- Job 使用当前 sandbox-api 的固定 `/app/redis-bootstrap` 子命令，非root、drop ALL、read-only root、seccomp；仅挂载必要的投影 SA token，没有身份/认证/PVC 挂载、无密码 ENV/argv。
- 两分钟总预算，API 单请求五秒、QPS2/Burst3，正常 transient 缺失/RBAC 创建次序在有界预算内重试；已有对象格式/身份错误立即失败。Job 名包含 revision 且长度受63字符限制。
- 公私钥只在 Job 内存和 Kubernetes Secret 中，不通过 stdout/日志或 Helm values/release manifest 输出。错误使用固定分类；CLI 成功不输出密钥。Go 临时 seed 仅做 best-effort 清零，不宣称整个 transport/内存零残留。
- 身份 Secret 标注 keep、安装范围和版本，无指向 Job 的 ownerReference；Job/SA/RBAC 为普通 release 资源，Secret 在 uninstall 后仍保留，不进行自动密钥垃圾回收。依据：[Helm 生命周期](https://docs.helm.sh/docs/topics/charts_hooks/)。
- Job 仅安装/升级时运行，无常驻循环、无业务请求转发；不改变 Sentinel 唯一选主权、ACK、原有恢复包装器或 API 请求性能路径。
- 身份 Job 不作为 post-install Hook，避免 API gate 与 Helm --wait 的死锁；初始化 Job、API gate、探针等待余量继续保留。

## 验收与最终交付

- typed fake/实际 HTTP API 夹具：首次创建、公私钥有效、重复/升级/重装原bytes保留；显式外部只读；旧PVC/旧CM/登记/Initialized但缺Secret不创建；缺CM保留PVC、不匹配/可变/错seed/多键Secret、UID/版本变化、AlreadyExists、提交不确定/预算取消、错误不含私钥或API原文。
- 结构化 Helm：身份Job/普通最小RBAC/无凭据挂载、有效名称一致、无手动Secret需求、同一新clusterID、PVC标记、预算/长度、standalone/external不变、env无重复。
- 真实三成员矩阵接入自动身份创建结果，继续覆盖复制/ACK、非零主切换、旧主恢复、冷启动、新IP、空卷/较高少数epoch阻断。
- 全仓 Go、相关race、vet/lint、Helm与真实隔离夹具、diff-check，独立复审修复后提交。默认跳过和 fake API 不宣称生产部署通过。
- 更新中文默认values/示例/部署说明，提供最终线上覆盖配置；用户仅设置新API镜像、存储类、两个密码及sentinel mode，不需手动 kubectl create Secret。

## 自审

边界已明确：只自动生成真正新安装身份；保留与全新安装分离；普通Job无启动依赖循环；create RBAC限制不夸大；不记录密钥；不递归删数据；外部引用只读；不改main；不构建发布镜像。本文件为实施设计，不是完成或生产验收声明。
