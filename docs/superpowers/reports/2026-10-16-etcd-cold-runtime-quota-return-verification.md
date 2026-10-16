# 冷建 runtime quota 安全归还验证

2026-10-16。完整单元基准 `956536ea29efbc00a75a87dd8f6c4b3a570dd890`，最终源码 `8555578c14db5c424432d96047d7975577642fc6`。唯一 fresh wholeTask reviewer 审查全部四个提交后给出 spec compliant / Approved，无 Critical 或 Important；Root独立核对实际来源、运行与退出证据后验收本单元。此处不表示生产迁移完成。

## 实现与提交

`3f304f8` 增加独立配额归还协议：重新认证原 publication、双槽 reservation、preparation settlement、签名终止、provider cleanup 和 owner release；仅在原 quota epoch 仍被占用时归还。公开 DTO、任务 locator、404、取消和失租均不提供退款权限。原 owner release 的 64 节点事务保持不变。

归还采用独立 42 节点 Stage（34 compares、4 success、4 failure）。第一次 Grant 前保存 exact reference；未知结果用原引用仲裁。重放认证不可变记录及原 receipt，不读写已经复用的 quota/preparation slot，不影响新 workspace owner。调用纳入原 Backend Close 屏障；没有新增随存量 N 增长的 map、ticker、Lease 或扫描。

`8555578` 增加原真实安全释放 fixture 的两条配额链路。新分支默认关闭，原 selector 不变；测试故障代理只转发或故障化实际元数据响应，不制造死亡或 provider 成功证明。

## 实际验证

限定 host 五项 contract、race、Linux vet 通过；首次 RED 是缺少新方法和 codec 的编译失败。Bash 兼容问题发生在 Go 运行前，未计业务 RED。实际 protobuf 上界投影为 request 189806B、success 243B、failure 8763B，均在 262144B 内；这不是容量测量。

`metadata-01` 六个真实 TLS etcd 负例通过：未发布、prep 未结算、缺失 release、异 workspace/task/restore 均保留原配额且不产生归还记录。该测试不提供物理终止正例。

`fullchain-04` 原运行 `c4bbfc / session39532 / d974f0` 退出 0，两条 case 通过：

- `quota-guard-commit-ACK`：实际 Grant、guard、配额提交响应依次丢失，两 Backend 按原 reference 仲裁，未确认时保持配额，提交后只归还一次。
- `release-ACK-quota-postread`：原 64 节点 release 已提交但丢 ACK；配额提交后读取失败仍保留 exact reference，另一 Backend 恢复原 committed 结果。

每条链路均经过原 archive/fullsync、物理 observer SIGKILL/cgroup 证明、精确 UID/RV provider 清理及 owner release。prep 和 workspace owner 已复用后才归还原 quota；quota 再被真实 allocator 以 epoch+1 占用后，旧重放不读取这些 slot，也不改变新记录。原历史、return record 与 receipt 的响应篡改被拒绝。实际 quota 事务 42 节点，request 16302B、response 61B；这些仅为固定 fixture 样本。

两个 worker Wait=nil，两个原 PID1 由 observer SIGKILL 后 Wait；observer、HTTP、TLS proxy、upstream transport 均 join，etcd SIGTERM 后 Wait。attach/container Wait=0、无 OOM；精确容器 `0577bfa8ffa8353042593e1e87fd60dc95bb59a573eb9ce90460828f7c6a6e07` 已删除，重新读取所持 inventory 为空。

最终 runner 来源 9106 项、launcher 2004 项；Root 核对实际 17 类输入、前后 SHA、归档每个常规成员、当前源码、ELF64/AArch64 Go1.25.6/Linux/CGO0 和 wrapper/helper 精确两成员归档。runner SHA `a59b3677ce29ec8cb62f273085a328687cd71b7daae7aa0d3cdd4b31d653193f`，launcher SHA `9ccee69ff15970f4241adcf5d060ff54d8bda787a67c38260ff0f472d836732d`。最后修改后限定 Linux vet 原日志为空。

## 失败历史与边界

`fullchain-01` 仅构建，补齐篡改负例后未运行；02 因新 Backend 复用原恢复目录锁而 setup 失败；03 因测试临时目录权限不满足生产 0700 契约而 setup 失败。04 仅隔离新测试目录并设置 0700，未放宽生产校验或锁。失败记录保留，未改称算法 RED 或成功。

原日志保留 etcd 单端口/simple-token 配置诊断、故障注入导致的 PermissionDenied/Unavailable，以及实际退出后的 ServerClosed/closed-listener 诊断；不声称日志无警告。原 selector 和旧矩阵未重放。

reviewer 的唯一 Minor 是成功测试日志含这些已分类诊断，建议未来 collector 保留 raw log 同时给出独立摘要。当前报告已逐类说明，Root将 collector 展示改进留待其后续维护，不为展示问题重跑真实终止测试。reviewer 的跨单元核对项由前置验收 `956536e` 和本轮 Root 的 actual17/归档/实际 Wait 与清理审计承接，无未决阻塞项。

本单元是 Backend 安全归还接点，尚未完成 full Manager/main 接线、永久沙盒、未发布出生回收、warm/FUSE/Docker、业务认证、分区调度、证书维护、容量与部署恢复、Redis 移除；少量真实 runtime 不证明大 N 或吞吐容量。
