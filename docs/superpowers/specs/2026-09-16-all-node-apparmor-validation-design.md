# 五节点 AppArmor API 与拒绝规则验收设计

## 目标与边界

用户选择处理最新后续清单第 4 项。在 `ds-ai-research` 对 `ds-ai-worker-1` 至
`ds-ai-worker-5` 逐节点完成普通/FUSE 公共 API 生命周期、真实 s3fs 约束和拒绝负例，扩展已有
worker-2 组件级方案，不修改 AppArmor 规则或运行时安全契约。

五节点当前均为 Ready、无 taint，Debian 13 / Linux 6.12 arm64 / containerd 2.2。当前部署的
API v0.3.31、runtime v0.3.4、mounter v0.3.30 和 loader v0.3.24 镜像采用已读回的 digest；
测试不构建/推送镜像，不 upgrade/uninstall 当前 `sandbox-fuse` release。

## 方案比较与选择

采用逐节点、独立临时 namespace 的真实 Chart/API 验收。每轮通过同源 nodeSelector 将本轮
普通/FUSE Pool 和自己的 loader 固定到一个节点，再使用公共 API 领取和操作沙盒。只并行保持
一个节点的业务实验，控制资源与对象存储开销；测试 Pod 的 nodeName 必须读回匹配目标。

通过当前业务池反复领取较轻，但不能保证五节点覆盖；更改线上 nodeSelector 或强制迁移业务
池会影响当前 release。直接私有 authorize fixture 适合组件诊断，却不能关闭公共 API 验收
缺口。本次以隔离真实 API 方案为主，私有只读观测用于核验其实际状态。

## 测试资源与数据隔离

- 为每个节点创建本轮唯一 namespace，明确 run/purpose 标签，创建后记录 namespace UID。
  每个 namespace 只有一个测试 API、一只普通和一只 FUSE pristine Pool，以及自己的 loader。
  minSize 1、maxSize 2，限额有界。不同 API 的 runtime namespace 不共享，避免跨 release
  inventory 扫描相互干扰。
- 使用自己的 standalone Redis 和 emptyDir，不使用业务 Redis、PVC、Sentinel 身份或数据。
  测试场景不代表 standalone 已满足生产 HA。
- 实际存储仍使用当前部署已验证 TLS 的 MinIO 后端，所有 workspace 都位于随机本轮路径
  `validation/apparmor-all-nodes/<run>/<node>/...`，不读取或清理业务前缀。
- 当前存储凭据只在测试驱动安全内存/0600 临时配置和私有 API 配置中复用，不打印、不写入
  Git、不交给租户代码；继续通过原一次性 authorize 通道传递。测试 API key 使用本轮随机值。
  不增加 insecure TLS、自签 CA 绕过或改变生产镜像信任链来制造通过。
- loader 模板/profile 来自当前 Chart，内容与当前已加载 exact digest 名称一致。可以复用
  已加载内核策略，但必须读回 exact enforce；没有 profile 则按原加载器机制加载同版本。
  不替换、卸载或放宽任何业务正在使用的 profile。
- 仅创建本轮必要 namespaced Chart 资源。若需要网络分配资源读取的 inventory ClusterRole，
  只使用原只读规则，登记精确名称/UID，并绑定本轮 namespace 的归属用于清理；不授予 Node
  patch 或集群写权限。固定 CIDR 只在已取得完整分配证据时配置，不以不完整范围绕过发现。

## 每节点验收

1. 自己的 loader Ready、exact kernel profile enforce；普通与 FUSE Pool 发布时零重启，读取
   RuntimeUID 与实际 Pod UID、目标 nodeName 一致。
2. 通过公共 API 创建普通沙盒，验证 Python/Node/bash、UID 1000、NoNewPrivs、Seccomp 和默认
   AppArmor；真实写读及正常销毁，原 Pod UID 消失且所属策略收尾。
3. 通过公共 API 从 pristine Pool 创建 persistent FUSE sandbox；写入/fsync/读回、覆盖/追加、
   重命名与删除均验证内容；手动 flush 后跨副本信息语义保持正确。测试只有一个 API 时不
   虚构跨副本覆盖，已有三副本回归仍是独立证据。
4. 读取真实 supervisor PID 1 及真实 s3fs 子进程的 `/proc/<pid>/attr/current`，要求同一 exact
   profile `(enforce)`；联合真实 fuse.s3fs mount ID、generation 与零重启。不能用 shell
   子进程属性代替 s3fs 本身。
5. 在本轮精确 Pod、自己的路径上做允许/拒绝对照：允许读 /etc/hosts 和写 /run/s3fs；拒绝
   读 /etc/shadow、写可写 /dev/shm、从已受限进程再次 exec 任意 shell、在自己的 cache
   子目录挂载 tmpfs。所有非预期成功立即失败，不打印敏感文件内容。
6. 用自己的可信 loader 对照可写 tmpfs 与 tmpfs mount，严格筛选本轮 profile、随机测试
   路径/PID及时间窗的 kernel DENIED 记录，区分 AppArmor 拒绝与只读文件系统/utility 失败。
   不输出其它业务审计记录，不新增 hostPID、宿主根或 containerd socket 挂载。
7. FUSE flush 后用独立认证 TLS 对象读取严格校验测试内容，再删除本轮文件并 flush；通过
   API 正常销毁，确认原 UID 终止、策略收尾、Pool 自动恢复。清理失败保留证据并报告，
   不清 finalizer、force-delete 或裸删 Redis key。
8. 仅删除自己的 loader Pod，绑定原 UID 后等待同节点替代 Pod Ready/enforce；不删除线上
   loader、不卸载策略、不重启节点。因此结论是 loader Pod 重建恢复，不冒充节点冷启动。

## 失败与最终清理

驱动必须显式 opt-in，限定 context、五个节点、随机 namespace 前缀/标签/UID 和已核验镜像。
执行前核对业务 API/Redis/loader 基线；发现替代 UID、节点不匹配、profile 失配、控制读失败或
不可信身份时停止，不继续授权或扩大权限。日志只输出固定阶段、状态与非敏感身份。

所有测试请求都有总预算，等待采用有界状态轮询，工具调用等待不超过 60 秒。清理先正常销毁
自己的 sandbox，再按 namespace UID 前置条件移除自己的 namespace、并精确收尾测试 inventory
RBAC。内核 profile 保留。最后核对全部测试 namespace/策略/RBAC/转发和测试活动 Redis 状态
无残留，业务 release 元数据及健康不变。

五节点各项结果逐项保留成功/失败/未执行，不以总 Ready 或一次成功覆盖其它节点。本轮不涉及
Calico/双华云、节点重启、策略卸载恢复、其它运行时或其它存储组合；它们等待用户后续环境。

本设计是书面复核入口，尚未创建临时集群资源；用户确认后编写内联实施计划并执行。
