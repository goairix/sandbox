# 无 workspace 业务申请验收

2026-10-16。审查范围：`36ec725e2610609ce043d0e47bded0b91b1f88cb` → `ecb5d4b5f1de8701708801d463926c93deb7825d`，19个文件。独立全任务审查结论：Spec compliant / Approved，无Critical或Important问题。

## 实现与提交

- `101082b`：资源数量在解析前检查十进制指数的计算预算，保留E/Ei单位。
- `ef92972`：独立detached身份，绑定原Backend存储身份与principal/key，保留原workspace哈希并拒绝进入workspace同步。
- `ea1845d`：CreateBusiness支持可选幂等键、生成键回传、原请求/配置恢复、一次准入与Close取消/join；首次并发竞争仅有限仲裁，结果未知时保留原参考，不重复创建。
- `4864e09`：真实Linux运行时的同键重放、并发首次申请与激活ACK丢失后重建恢复。
- `ecb5d4b`：冻结仲裁和验证边界。

## 已验证

固定Go1.25.6。资源codec九组、身份五组、业务入口七组最终host与race测试均退出0，无skip或race报告；最终四包Linuxarm64 vet退出0。生命周期测试实际取消Backend查询并等待原RPC、handler及服务关闭，不冒充创建成功。

Linuxarm64原生测试三个场景全部PASS：生成键/同键重放/默认TTL变化保持原配置且变更请求冲突；强制两次首次查询均不存在后只创建一次；激活ACK丢失后通过同一业务键恢复原IDs和运行时。三个正式launcher、九组bridge/IO、三个TLS etcd进程均完成原始join，容器退出0、无OOM，精确删除本次容器并确认清空。

Root独立核对实际17类GoList输入、平台/race参数、构建前后清单、源归档所有常规成员、脚本/helper、真实ELF与实际Go buildinfo。保留原失败：指数行为RED、缺失API编译RED，以及旧测试位置参数literal的编译兼容失败；修复后使用新的最终验证记录。

## 边界与审查小项

原始日志保留预期查询取消、fixture配置及关闭诊断，不将其描述为无噪声输出。native日志实际13,100字节、84物理行；此前报告的105行计数更正为84行，完整日志和进程退出证据均已读取。

此次没有新增真实Destroy/owner释放场景，也没有逐一确定性注入所有ResolveStage传输分支；历史publication寻址与有限仲裁另经源码审查。配置冲突验证没有额外POST或launcher启动，未测量LeaseGrant计数。固定样本不构成大N容量或性能验收。

主进程仍使用Redis。当前完成业务创建入口，尚未完成HTTP/Manager生产接线、全功能运行时操作、分区调度、FUSE/collector、容量与部署恢复验收及Redis移除。
