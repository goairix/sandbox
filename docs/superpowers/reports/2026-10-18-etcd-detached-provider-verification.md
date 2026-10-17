# Detached provider 首次发送组件验证

2026-10-18，代码提交 `54e3d08`，唯一 reviewer 组件 Spec/Quality PASS。
此片接通原 detached job 的生产 reservation/provider 首次发送和
同 Claim 只读诊断；完整七槽终止、terminal proof、后续 Run Query 与消费者未完成。

## 行为与审查修复

私有入口认证原 job/Claim/Kernel 后复用原 bounded runtime owner，使用原真实
v2 loader。固定三点读取和七槽执行提取为共有实现，原 v1 入口仍走 strict local
权限。v2 证书与 contract 只取自已认证来源，reservation 匹配完整 variant 与
实际 create-once envelope。同 Claim 重入只读原记录，不重新签票或进入 provider。

唯一 reviewer 发现 P1：loader 捕获的 Claim cutoff 没有传给后续 context，原 Claim
续租可延长已接收操作的外部调用窗口。真实 RED
`run-2ef273fa0e16454eb3f5a6d67cadc7bf` 中，Certificate/Sign context 截止时间
晚于捕获窗口。修复只在两个 detached wrapper 成功 load 后用原 cutoff 派生
context，供 pair RPC、execute 和诊断读取使用；独立诊断仍捕获自己的当前窗口。
原 v1、guard、Claim TTL 与生产容量限制未放宽。

## 实际 provider 结果

Linux/arm64、CGO=0，实际 etcd3.6.14 三成员，正式 plain Pod builder 和实际
TLS API client。Unknown observer 验证真实签票后只返回 Unknown，没有产生终止证明。

`run-7ec0bd70f49b4400b1353bb6968e2229` 两顶层及两个负向子例通过：

- 原 provider key 最初为空；生产 writer 实际写入五次，reservation 为58比较+
  1写入=59节点，请求15359字节、回复37字节。
- TLS API 实际收到一次 Pod DELETE，零 finalizer PATCH；observer 一次 Terminate、
  零 Query，issuer 一次 Sign。原 durable slot1 intent 在 DELETE 前已可读取。
- 同 Claim 重入后 intent/provider MVCC 不变，provider/issuer/observer 接收数不增。
- 缺永久 fact、复制 job 均拒绝，零 effect，transition keys 保持为空。

首次调用含诊断读观察29次 Commit 提交，加重复调用共48次。这不是 transport
dispatch/retry 上限。上述组件只完成 slot0/1，slots2..6 保持 unstarted。

截止时间 GREEN `run-a8e3b42553ef46d08aedf4f83efba9a9` 的 execute 和 diagnose
两项通过。原生 Renew 延长真实 Claim；Certificate/Sign callback context 仍精确
等于先前捕获 deadline。诊断 pair 的原1秒 RPC timeout 更早，仅作为短窗口控制，
不冒称其独立复现外层 cutoff 缺陷；诊断自身捕获新的合法窗口且无 effect 重放。

两场均无 SKIP/FAIL/OOM，原三个 etcd 子进程实际 Wait/join、精确自有容器删除，
各56项 source 指纹和 ELF 稳定。provider 场 ELF 为
`887b775e6a0e0150b8663b352d2d74e02e1a4efbc21b383da1b85488e4f23578`；
截止时间场 ELF 为 `7059368756d5d09b1d612f5436e30f66b3518a3faf94ce8847f67ef7be97ae12`。
二者产品相同，后者只在短租约测试的首次操作前统一走真实 Renew。

`detachedproviderfinalbuild02`、`detachedproviderfinalvet02` 各9246输入通过；
`detachedproviderfinalcompat01` 旧 v1 定向 race16顶层通过，9149输入。
完整输入、工具、快照及前后指纹审计通过，未重复归档工具链/依赖缓存。

## 保留门槛与原始记录

尚未验收：全部 slots2..6、真实 PID1/cgroup 与 terminal proof、fresh Claim/new Run
retained Query、受阻 in-flight Close、provider 最大合法输入预算、owner/terminal/
quota、managed consumer、HTTP/部署及大 N 容量。默认入口仍 Redis，生产切换0%。

首次 cutoff 用例尝试将 fixture RequestTimeout 设为15秒，被原 worker≤3秒约束
拒绝，没有到达目标分支；已恢复原1秒。后续10秒 Claim 用例另在长时间准备后、
首次操作之前 guard 过期，没有冒充 cutoff 缺陷证据。测试现只通过原 RenewTaskClaim
补充当前合法窗口，不赋值 deadline、不 sleep、不放宽 guard。失败日志全部保留。

原始命令、输入、日志和唯一 review 位于 D 工作树
`.superpowers/sdd/2026-10-18-etcd-detached-runtime-native/`。
