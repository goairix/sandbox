# 有界快照读取验证

2026-10-16，完整单元基准 `36a5dd13cc45d0287d0beeaa25d7f68ed8efa459`，最终源码 `1ce8e4a919d06f7ec9d8b2c9834cff5f43e5fdf9`。限定验证完成，唯一 fresh wholeTask 审查覆盖五次提交，结论为 Spec compliant / Approved，无 Critical 或 Important 项；Root已完整读取报告并接受本单元。本验收不表示生产切换完成。

唯一 Minor 项为既有 native fixture 的配置、取消和主动关闭诊断噪声。Root裁决保留原始日志，将分类展示延后到限定 collector 改进，不为清理日志重复测试。审查核对原始运行结果及接点安全性；来源归档的17类输入、逐成员和ELF校验由Root独立完成，二者不冒称重复执行或完整迁移分支审查。

## 实现与小提交

`9c5490e` 增加两个 coherent snapshot 读取入口、单 Backend 加权 LRU（默认64MiB，显式256KiB–64MiB）、16个共享 miss/256个读取调用硬上限与独立 capacity 错误。条目含保守开销和字符串/payload权重，返回独占副本；无每沙盒 ticker、Lease、扫描或常驻填充 goroutine。原 Backend Close 封准入、取消并 join 新调用/填充，之后清空缓存。

固定 snapshot65536B+placement4096B 两点预检共享原 view 的严格 dispatch/response 校验，先检查 flat 形状再计算 protobuf Size；旧4096B/4点/4pins预检不变。miss 验证 immutable/canonical/schema/digest/placement，命中仍重新读取并确认原 view。结果不授予 health、TTL 或 operation 权限。

`11a4699` 增加真实 TLS etcd 固定测试及 selector。`1ce8e4a` 将 plain cold 进行中恢复的分离读取替换为新接点，原 claim、capsule、workspace/generation及sandboxID检查保留；completed 历史 LoadSnapshot 不改。

## 验证事实

八项限定 host 与最终 race02 全 PASS、无 skip/race finding。初始 compile RED 为缺少新字段/方法；后续 deadline 行为 RED 实际观测 DeadlineExceeded 被覆盖为 Canceled，修正后验证调用者和独立填充 timeout 都保留原错误。原 view 最大命名空间预算仍14节点/request12569B/reply16273B/payload13312B。

metadata02 原运行 `3e87eb / session47944 / 6f65c7` 退出0，五组26子case全部通过：14类畸形响应、5类命中/填充 fence 变化、2类取消/关闭、5类原 view 严格性回归，以及正常缓存/clone/两 Backend 隔离/缺失查询。固定四次读取观察到 snapshot1次、metadata12次，实际响应总10750B；仅能说明同一 coherent 路径少取三次 immutable snapshot，不能据此声称比旧 service 的两次分离读取 RPC 更少或整体延迟/吞吐提升。

原 Backend ELF Wait=nil，etcd SIGTERM 后 join，attach/container Wait0、无OOM；精确容器 `61e4f6ea0bfe6de8267289328c628942645731cc5e3370ee9b9c4b2193dabbd1` 已删除，重新读取本Task inventory为空。

service01 原运行 `e742a6 / session67496 / 3c8085` 退出0，正常创建与 activation ACK 丢失后恢复两条真实成功路径通过。六个原 bridge Wait/relay/IO、两个正式 launcher exit0、两个 TLS etcd SIGTERM 均 join；attach/container Wait0、无OOM，精确容器 `4f4526ccf63d62adeac283f64c6c1212f692f4e2539d0086460da13089128d11` 已删除，inventory为空。原历史函数未改；本轮没有实际 Destroy/owner-release 的新增验收。

两项原 service host 回归 PASS；最终 etcd/provisioning/controlrunner 三包 Linuxarm64/CGO0 vet 退出0，原日志0B。Root核对各冻结版本实际17类 GoList输入、pre/post、归档每个常规成员、脚本/helper精确归档与真实ELF/buildinfo。host8932、正确race8937；metadata runner9112/Backend9020；最终service runner9113/launcher2005、vet9115、service host8891。最终service runner SHA `3061f3dae293767bad3a20c416c09ea54300ac3313dffcdb82351a4ea2821e81`，launcher SHA `73ebdecaaf550f5d3160454e1ed018c726829ec497e67e59d26fa03a68f48c4e`。

## 保留历史与限制

第一次 host GREEN 因无连接 NewCtxClient.Close 的测试预期错误而失败，按其真实 Canceled 行为修正 fixture；race01 执行通过但 collector 的 GoList 漏-race，仅作为执行证据，race02补齐真实变体来源。native01仅构建，修正异步测试回调中的 FatalNow 后用native02首次实际运行；未覆盖或改称成功。

原 raw 保留 etcd 单端口/simple-token、主动取消与实际 shutdown 的 warning/error，按配置、故障场景和已join退出分类，不声称日志 pristine。两次 fixture 均是单机固定样本，未完成大N/吞吐/进程RSS/生产容量验收。

本单元不包含完整业务codec、HTTP/Manager切换、freshhealth、永久TTL、调度、FUSE/collector、部署恢复或Redis移除。总迁移连续推进。
