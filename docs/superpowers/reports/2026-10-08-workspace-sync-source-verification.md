# 原始 workspace 归档消费者验收

状态：本单元已验收。BASE `5a46ec729f9bb54dcb10cb799cc340b6f6f9c326`，功能提交 `61fe19afaa5f43ef1d2ecfceeda5fa902ca5f0ec`；精确13个源码/测试路径。用户指定的基分支修复已先合入 `7bca73a`。

此单元登记原始归档消费者，在读取前核对规范计划与当前 native 权限，提供私有有界 TAR/PAX 游标及单次 body。原始 source 只公开 Close；Close/Release/failed Renew/Backend 先取消、等待实际登记读取及后台监控退出，再清理文件、退款与关闭 client。它不发送 provider 请求，不提供远端结算、安全释放或生产迁移证据。

最终新 selector `^TestTaskWorkspaceSyncSource`：host race8个顶层 PASS（所有层级10PASS/24预期native SKIP），4.314s；真实 etcd/TLS native race13个顶层 PASS（所有层级34PASS/0SKIP），73.068s，未发现 race/etcd-client warning。原始45秒归档会话实际过期后继续读取、真实TTL3秒续约、当前权限失效与原始读取关闭等待均在新测试范围。不得把host中含SKIP子项的父PASS视为native验证。

Root核对13个文件的当前/Git内容、长度、SHA与blob ID共65项全部一致；22份保留日志的SHA/长度全部匹配。作者最终host/native/vet/Linuxarm64CGO0/gofmt/diffcheck命令exit均为0，暂存区为空。交叉产物实测为静态ELF ARM aarch64，Linux编译不是Linux运行时执行。唯一独立reviewer裁定Spec Compliant / Quality Approved，Critical0/Important0/Minor0；没有重复运行测试或扩大为整分支审查。

作者报告SHA256：`e4b4e7fb086ead0707d778b8bfc94fecad7c4635503258b63362a3665f9966ce`（5786bytes）；evidence.json SHA256：`ccf2a67b80a025278b9a92c13f33361dd4fea55af479a7051d9aa38f117ea139`（14950bytes）。最终host日志SHA：`0083ba8ad1235cc38ad64d495667a63b5a60fe3aa37caa9131f816e9615c648a`；native日志SHA：`4b3d6b9f60b9de36540951cefd28f2cde74b67bbe160d31f13f7f006ab687c3f`。实际叶子覆盖为5个host与24个native。

compiled RED仅覆盖实际 TLS/native 接收后 callable scaffold 拒绝 source admission；物理关闭测试首次运行在实现后，未宣称完整 preimplementation RED。独立 expiry 用受控发布的 monotonic deadline 与原生TTL30秒，实际TTL3秒续约另测。ReadAt seam执行原始文件读取后控制完成返回，证明已登记 IO 的物理等待，不能称阻塞内核系统调用。

先前测试因默认20秒父ctx提前取消45秒正例，以及旧setup30秒lease清理警告而未被接受；日志保留。canonical-only长ctx保持原始签名/session不变；在真实PublishRuntime/CommitStage后释放原始setup CreationClaim/Stage，后续仅避免已成功原始revoke重复。默认nil builder行为不变，不吞其他清理错误。

当前为保持强防篡改约束，会在描述符/body入口重复校验整份归档SHA。32KiB scratch有界但多文件总IO可能随entry数乘archive大小增长；不得称性能提升，容量验收前必须独立优化不可变source证据与避免重复扫描。接下来仍需每个真实provider请求持久意图/可信worker/物理目标与结算、运行时终止安全释放，以及生产接线、调度、FUSE/collector、完整容量和部署恢复、Redis移除。
