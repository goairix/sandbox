# 原始 workspace 本地 IO 与结算接缝

承接已批准的 `2026-10-06-etcd-state-management-design.md`，以及已验收的原始 source、永久 intent/slots、非特权 worker。这里交付真实 local regular-file IO，不代表完整同步、安全释放或生产迁移；主程序仍使用 Redis。云 provider、目录同步/删除、持久 terminal/退款与崩溃恢复分别继续实施。

## 两个可独立验收的单元

1. 原始 worker、只读打开的实际 local root FD、原始未读 body 同时绑定到永久意图；只有明确收到原始成功提交 ACK 的本地对象可进入写前门。原意图 Version 1 诊断行为保留，新增 Version 2 local profile；新字段仍包含在既有 MaxIntentBytes 内，不新增持久 key，也不隐含 terminal 预算。
2. 在该门后进行实际有界 regular-file 写入、文件 fsync/close 和目录 fsync；创建的 parent directories 同样属于该意图覆盖的有界 local syscall 序列。成功仅产生原始进程内 witness；永久 intent/slot 不删除、不退款。原 witness 未持久化之前，重启后仍按 unknown 保守收费。

## 权限与实际物理归属

- 入口私有于 etcd adapter，消费原始 `*launcher.KernelBoundary`、原始 `*PreparedTaskWorkspaceSyncSource`/body；引用、复制对象、配置 WorkerID、通用 Reader 和调用者成功回调不能取得效果权限。
- root 来自受信 operator local 配置，路径规范且最多 4096 bytes。构造仅只读打开，Linux openat2/no symlink/no magiclink，保留原始 FD、dev/inode、mount ID、filesystem type、非零 worker UID/GID 与原始 worker incarnation。root 必须 worker 所有且 0700；子路径最多 4096 bytes/128 components，用原始 root FD scoped openat2，禁止跨 mount、符号链接、特殊文件和 hardlink。
- filesystem type 采用明确 allowlist：ext4、xfs、btrfs、tmpfs；未知、NFS/CIFS/FUSE/overlay 等拒绝。tmpfs 仅证明当前进程的真实本地 IO 与 syscall 完成，不证明机器重启后的存储持久性。生产卷配置与 Deployment/凭证隔离另验收。
- target 配置关联由 operator 负责，实际 root 身份与 exact workspace/plan/path 在写前永久记录；复制的物理诊断不能重新打开 target 或重建权能。每次开始及结束效果检查原始 worker ValidateCurrent 与原始 target 身份，不通过提高 cap、dumpable 或宽松路径解决失败。

## 持久意图与写前门

Version 2 local profile 在原始 body 私有绑定完成后，原 reserve 路径保留全部 50 历史 pins、完整 operations prefix、budget pin 和原始 2-key CAS。序列化仍受原 B4096..16384、slot512、key1024 限制；所有实际 protobuf 保持 64 helper nodes/64KiB record/256KiB request/reply。超过限制拒绝，不删除历史 pins。

明确成功 ACK 后再经原始 local guard 才设置私有 acknowledged 状态；任何 unknown、冷 Load、arbitration，包括首次同字节配对 ABA，都永远不能设置该状态。后续已明确 ACK 的 draft 不因重复诊断读取得到第二次派发。

写前新只读 Txn：全部原始历史 pins + 完整 prefix + budget comparisons，成功分支同 revision point-read intent/slot；严格校验 canonical value、Version1/Lease0/同 birth 与原始 ACK revision。失败分支仅 bounded identity/restore/budget 诊断。不需要新增永久 dispatch key：Version 2 intent 已绑定这次 local syscall 序列的物理归属，原始进程内 one-shot 在任何效果前消耗。没有新 SDK retry/HTTP wire。

注册必须先于任何 mutating syscall。owner 加入 source Add/Wait seal gate；并发 Read/next/第二次 put/Close、claim lease 丢失和 Backend.Close 都不能绕过门。不能持 body/source/store/claim mutex 跨 native RPC 或 filesystem IO。取消不等于 syscall 返回；Close 必须实际等待原始 IO owner，未完成不产生 witness/退款。失败之后不重开、不重发、不切换 slot。

## 实际 IO 与 witness

使用固定 32KiB scratch，最多 128 parent components；不构造 workspace manifest map、全文件 byte slice、每 sandbox actor。单次 put 可创建缺失的私有 0700 parents，创建或覆盖 exact regular file（worker 所有、nlink1），保留归档 mode。direct write 失败可能留 partial file；这属于 unknown/failed charged intent，不能描述为 atomic replacement。此单元不引入额外 staging 布局。

bytes/hash 必须匹配原始 body descriptor；保留已有 source 当前权限检查。使用实际 fsync + Close 的返回值和原始 FD 身份，随后 fsync 被修改的目录；错误、取消、完整性失败、worker/target/pins 变化均无成功 witness。witness 私有 self/origin pin，仅本次原始 owner 可创建；其诊断明确 local syscall completion 与介质类别，不是完整 final-sync completion、durable etcd terminal、退款或 safe-release。

## 验证

host race 验证纯 codec、限额、复制/外来/unknown 拒绝，不对开发机执行 bootstrap。Linux arm64 CGO0 独立实际 worker/container 验证 openat2、root/文件身份与真实写/fsync/Close。fixture 使用已在本地的 pinned etcd image，pull=never、非零 UID/GID、caps0/NNP、network none，三个真实 etcd 子进程只监听 loopback，复用真实 native archive/claim/TLS fixture。必须实际 wait 三个 server 与 attach/container，失败保留原始句柄，exact owned inventories 为空。

真实用例涵盖正常、零长度、多 chunk、nested parents、覆盖、拒绝 symlink/hardlink/特殊文件/错误 root/worker/body、首次 ACK unknown 不派发、intent ABA/slot 损坏/restore/claim 失效、body hash 损坏、IO 进行时 Close 与取消。故障 seam 仅可暂停/报告真实调用完成后的边界，不能用 callback=true 代替 kernel/native 成功。测试数量、host SKIP、CGO0 非 race、tmpfs 非重启持久性分别报告。性能/容量与 HA 故障不在本次验收中。
