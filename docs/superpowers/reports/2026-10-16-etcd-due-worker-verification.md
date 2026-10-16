# 常驻到期 worker 范围验收

2026-10-17：本单元已范围验收。显式启动的消费者能持续发现到期候选、恢复永久扫描进度、衔接有界 Watch，并经原权限协议创建永久 cleanup task。它尚未执行物理清理，也没有接入默认入口。功能代码约70%、整体交付约45%、生产切换0%的估算不因本单元测试数量上调。

## 实现与提交

| 提交 | 连贯改动 |
| --- | --- |
| `1f7f7b6` | 严格永久 checkpoint、固定 R 恢复、真实有限消费者和 Grant 前持久原请求身份 |
| `a22f5fc` | 原认证连接上的有界原生 Watch、解码前限制、缓存、原权限分组校验及实际 Send/Recv 等待 |
| `6463b5d` | 实际常驻循环、原 Lease 续租、多份额计费等待、窗口滚动和两阶段关闭 |
| `1017b66` | 新三成员组合用例及运维说明 |
| `688748f` | 明确有限轮询的调度限制 |
| `9a06de9` | 修复同步/pending 展示、饱和日期、已知中止重试和最终关闭登记，补回归并改善测试排版 |

仓库规范 `AGENTS.md` 已按用户要求独立提交 `a40d73e`，覆盖关键注释、逻辑分段和包职责审查。本单元的协议、缓存、actor、checkpoint 和生命周期按职责分文件；原权限/事务/锁的内部耦合使它们继续位于 etcd 包内合理，没有为拆包扩大权限导出。后续业务与物理执行编排仍需单独评估包边界。

## 已验证的行为与边界

- 每个消费者使用一个原 session，串行协调原生操作与续租；最多256个配置分区。没有按每个已有 sandbox 建立 goroutine、Lease 或 Watch。配置只获取指定份额，不扫描空闲份额或复用释放 tombstone。
- 永久 checkpoint 每分区一条，严格8192字节上限，精确 CAS 保留固定 R、窗口、已结算 frontier 和原 pending。替代 session 创建自己的原权限与 cursor，不采用旧句柄；永久 replay 基线保持 R+1。
- Grant 前必须确认 exact candidate、Stage reference 和 Task reference 已持久化。未知写入禁止 Grant；未知准入停止原消费者，替代 session 先真实仲裁同一 reference。明确 aborted 保留候选，后续独立计费重试；不把 nil error 当作已结算。
- 原分页最多118条/256KiB。先流式处理真实 overdue 前缀，再缓存未来尾部；超过10000条 overdue 不要求全量先入缓存。首个未来洞前冻结 frontier，真实最终页单独 CAS 完成 phase，Watch 准入不会跨洞推进 frontier。
- 原连接上的原生 stream 在 protobuf 解码前限制单帧118事件/256KiB、整批512帧/10000事件/8MiB；不使用无界 WatchChan。全局最多一份源页、10000个候选，保守自有对象预算64MiB；这不是测得的进程 RSS 上限。
- 真实事件/有效 progress 才形成覆盖，Created 不证明追赶。原参与者完整分组校验，PRE 作为追赶目标，POST 可观察更新尾部；历史 CoveredRevision 与当前同步发布分离。真实准入阻塞时显示非同步及确认持久的 exact pending。
- 采用有界轮询，单次 stream 最多500ms，原操作 context 受 `min(3s,TTL/3,原剩余期限)` 约束，随后可取消地等待100ms。每轮检查续租；尚未实现 earliest-expiry timer，不承诺到期时延。context 到期不能替代实际 Send/Recv、callback、原请求和清理等待。
- 同 cell 最多四个本消费者，因为每个终身占有一个固定 Relist share。20MiB/s Range、100claim/s 是原软份额预算，不是持久任务硬并发32，也不是无突发吞吐保证。其他消费者接入须共同分配这些原份额。
- Stop、原分区/份额/session 释放和 Backend.Close 经过实际工作、once-only 原清理、final done 三个阶段。controller 的登记保留到 final done 同锁发布，后进入的 Close 仍会等待；子释放先发布自己的完成信号再等待 worker，避免循环。

## 实际验证

以下组数是重叠的针对性验证范围，不能相加为独立用例总数，也不是全仓测试或性能验收。

| 阶段 | 实际结果 |
| --- | --- |
| checkpoint | 25组 host/race、Linux vet 通过；每份故障日志1条 warning |
| 有界 Watch/cache | 52组 host/race、Linux vet 通过；每份2条 warning |
| 初始 resident actor | 67组 host/race、Linux vet 通过；每份4条 warning |
| review R1 最终 | 71组 host36.164s、race41.778s通过；Linux vet/build通过；无SKIP/FAIL/race，各4条故障/取消 warning |
| 初始新 native02 | 三成员 etcd3.6.14实际通过10.91s |
| 修复后新 native03 | 新构建源码/二进制，三成员实际通过9.98s；无SKIP/WARN/server-error行 |

R1 四组新回归分别覆盖真实 pending 后阻塞 Grant 的状态、year9999最后60秒已到期/只推进可信时钟两条路径、真实 ResolveStage 建立 aborted 后无新 Range 的独立计费重试，以及真实 session cleanup 后进入 Close 的 final publication 交错；既有多页 early-tail 用例新增永久 phase 断言。均先观察目标行为失败，再修改产品，最终转绿。

Root 独立执行并核对17类 Go-list 实际输入、前后源码/工具清单、每个归档成员与当前内容。最终 host/race 分别8989/8994个输入，Linux vet/build9080个输入。原始日志SHA256：host `1b25b0451b920922136f12e6e2091b84724fb8a7dfc0eea01d52470af756228e`；race `7be85a51d4f5c9d1134a39f368edccf7df18370d11de0a754868c521b46fe3d0`。

native03 二进制SHA256 `84ecfadbae0a3d6cb25e5259cd5636273531e67dbb9739bd489bc9767ca66fe0`；原始日志1279字节，SHA256 `343fed934d7b7dc2c61c22d025720650e5aff3db4cea61b79359018dd19ed545`。实际验证118＋2页、固定R63、frontier以下late PUT71、Covered/Observed76、三条永久任务、丢commit ACK后同请求恢复、原fence失权，以及三组原session/租约/实际工作退出。

正式容器保持 Linux arm64/CGO0、只读根目录、network none、私有PID/cgroup、drop ALL、no-new-privileges、1GiB/2CPU/256PID、768MiB临时目录及两个只读挂载。Root核对实际创建/运行前/退出配置、工具与二进制hash、attach/wait/exit0、无OOM、精确移除、同CID不存在和标签清单为空，没有强制停止。三个归属etcd子进程为Kill后实际各一次Cmd.Wait(signal:killed)，输出丢弃0；没有宣称优雅退出。

## 独立审查及保留的失败

唯一完整 Task 审查范围为 `1802da5`→`8855923`，发现C0/I4/M4。原作者修复后，同一审查员仅复核 `8855923`→`9a06de9` 的发现与修复风险，最终 Approved、无新增或未关闭C/I/M。I1–I4、phase与排版已修复；告警事实保留，earliest timer按明确的有界轮询策略调整关闭，没有把延期功能描述为已实现。

首次 native01 实际失败于新测试选择错误 Task 解码器，后续链未执行。Root采集脚本把空 AssertionError 字符串误当成功，已保存原脚本/失败并修正为明确校验实际attach和failure状态；只修正新测试解码器后重新构建native02。R1首次极限时间测试调用fixture未实现的DeleteRange，不计作目标RED；只改为既有真实Txn OpDelete后，第二次RED才复现目标行为。原始失败、告警和工具问题均保留，后续PASS没有覆盖失败证据。

原认证连接及固定etcd3.6.14 progress语义沿用本Task的原连接检查、官方服务端源码依据和新native回放证据；审查没有扩大到重做旧组件验收。

## 尚未交付

本单元没有物理 cleanup 执行器、持久32任务硬槽、默认main接线、历史ACTIVE索引回填/长期GC、FUSE/dirty/collector、完整Pool/Docker/Upload、生产部署恢复或Redis移除。小规模组合测试不能证明大量已有sandbox的MVCC/WAL、RSS、空闲流成本、故障追赶或吞吐容量；Range payload有界也不证明服务器Count扫描CPU与N无关。继续完整迁移时从真实任务执行和共享调度权限接线推进，不重做本已验收单元。
