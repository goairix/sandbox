# 本地终止票据的受控时钟余量修复

真实 mounted loader 首次运行到 local-complete 后，终止票据返回
`runtime termination ticket outside authentication window`。预算冲突后的部分
构造清理、同配置和同份额重新打开均已成功；这次物理清理未完成，原占用保留。

根因是终止签名用首次采样 `now1-1s` 作为 NotBefore，原认证校验又要求
`now2-1s >= NotBefore`，隐含两次独立受控时钟采样严格单调。真实 signed-clock
会计入每次调用耗时，合法的后一次估计可能略早。原直接时钟测试未覆盖这个边界。

修复只提取实际 x.sign 共用的私有窗口函数，并沿既有 close/quiescence 构造使用
`now-2s`：一秒认证边界和一秒采样余量。证书起点截断后新增前界检查，证书余量
不足仍拒绝。原 claim/context 剩余时间取最小值、20 秒未来上限、证书结束截断、
至少两秒未来余量及协议 30 秒总窗口上限保持原规则。没有修改签名协议、observer、
时钟传输或认证 verifier，也不保证任意幅度时钟回退可继续执行。

新增 13 个确定性子例调用实际原证书签名、票据签名与 verifier。后次采样早 100ms
时保留旧构造的明确失败 oracle，再验证新构造可用；超出一秒余量、到期、证书
起点/终点余量不足继续拒绝。覆盖 20 秒截断及较短剩余时间不延长后界。纯窗口
测试没有伪造 TaskClaim、KernelBoundary 或物理执行权限；claim/context 子例验证
已取最小值的输入，实际调用者仍由源码检查和后续 mounted 组合验证负责。

固定 Go1.25.6。host/race 为 darwin/arm64 CGO1，vet 为 linux/arm64 CGO0。
精确 selector 为
`^(TestLocalRuntimeTerminationClockWindow|TestLocalRuntimeReleaseProfile|TestLocalRuntimeReleaseOwners)$`。
每次后续 Go 或源码修改前独立核对全部 17 类依赖输入、前后 SHA、实际源码、工具
及每个归档；全部通过。人工检查注释、逻辑分段和包边界，gofmt/diff-check 通过。

| 运行 | 结果 | 输入数 | 原始日志字节与 SHA256 |
|---|---|---:|---|
| loader-clock-host-01 | 3 组、13 子例通过，包 1.441s | 9020 | 2310 / 9f831bb28c8c4087bdae0c0bb0a1b3924d64c58f7017278cbcdc0d27018cb947 |
| loader-clock-race-01 | 3 组、13 子例通过，包 2.550s，无数据竞争 | 9025 | 2310 / 01962a55d083c19154386b0da193db58bec8efe905469b3944ccc4379e525b08 |
| loader-clock-vet-01 | Linux vet 通过，无诊断 | 9111 | 0 / e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 |

首次 loader-native-01 为真实行为失败 5.34s，原始日志 5393 字节，SHA256
`f2c2d822c3be51aa140e0a1af89d08e8c002a3e8b009685a119979e7667c78d3`。
实际容器退出 1、worker Wait 1；签名时钟已实际 join，失败 fixture 的 supervisor
SIGKILL/Wait 和 TLS etcd SIGTERM/Wait 均有记录。无超时/OOM，确切容器已删除、
标签清单为空。独立环境、输入、工具归档、退出与清理核对通过；未将它记作物理
成功。失败后二进制全部 17 类输入也重新核对通过，才授权本次修复。

本提交时 mounted loader 的实现和测试仍是工作区中的后续独立切片，修复后的真实
组合验证尚待执行。本报告仅验收上述窗口修复及定向回归，不代表 loader、完整
cleanup Task、大规模存量容量或生产迁移完成。唯一完整 Task 审查仍待全部必需
恢复场景完成后进行。
