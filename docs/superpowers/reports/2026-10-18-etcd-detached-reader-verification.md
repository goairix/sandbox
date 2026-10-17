# Detached 原生权限读取组件验证

2026-10-18；代码提交 `809838d`（observer 历史认证）和 `bc9dc8a`（etcd 权限加载）。
唯一 Task4A reviewer 对本组件 Spec/Quality PASS；完整 Task4A 仍开放。

## 实现范围

etcd loader 仅接受原 registry job、Kernel、当前 Run 和 Claim，通过实际永久
no-effect fact、source/q/none 历史及签名建立私有权限。共享八域 decoder 保留
原 local v1 行为；完整 detached variant 参与匹配，不伪造 archive 或 LocalComplete。
observer 独立读取固定 source/proof/enrollment/activation，验证历史签名、native
envelope、完整 stamp 和 plain contract，随后检查当前 Claim/Guard/TTL 与原生 guard。

审查发现公共 decoder 新增 c.live(child) 会将局部取消升级为父 Claim 失权。
真实 Claim 回归先失败，再改为非变异检查；detached wrapper 原取消失权策略保留。
CreationIntent 的 pending→published 原键更新另有真实反例与修复；仅该固定 source
允许 create<mod，其他 create-once source 约束保留。

## 验证结果

Linux/arm64、CGO=0，实际 etcd 3.6.14 三成员。最终三场均无 SKIP/FAIL/OOM，
三个原始子进程实际 Wait/join，精确自有容器删除；52 个 source 指纹和 ELF 稳定。

| 场景 | 实际结果 |
| --- | --- |
| 原 Claim 的 local/detached 取消对照 | 2 顶层 PASS；RED `run-f328e06cfdee472aa7f39ad2111039de`，GREEN `run-6c51b3f9ae824d37886134aec44e23e2` |
| 真 writer→loader；缺 fact/复制 owner；四类来源 ABA | 3 顶层及4子场景 PASS，`run-4ee580d0314a4d35af46311bfcac33b1` |
| 真 writer→loader→canonical intent→签名 ticket→observer；fact ABA 拒绝 | PASS 14.16s，`run-1db316b6676a44e3800e9da476f5c71d` |

实际 loader guard 54比较+1点=55节点，请求11054字节、回复299字节；observer
58比较+1点=59节点，请求15293字节、回复299字节。loader加一次重复guard观察到18次
Commit提交，observer成功bridge为9次、加拒绝ABA共11次；未测 transport dispatch/
retry 上限，也未以这些小输入证明所有合法最大值预算。

`detachedauthorityfinalbuild01`/`detachedauthorityfinalvet01`：9241输入，构建/vet通过；
`detachedobserverfinaltagbuild01`/`detachedobserverfinaltagvet01`：显式
`sandbox_detached_authority_test` 的9264输入，构建/vet通过。旧v1定向race16顶层
通过3.169s；未变 observer 输入的host13顶层2.195s、race15顶层5.355s通过。
每次 Go 的完整输入、工具、源码快照及前后指纹审计通过，共用缓存，无重复工具链归档。

首次新容器因256MiB临时盘不足，在 TestMain ready 前失败，没有测试开始；随后
测试 fixture 调整为512MiB tmpfs/1GiB内存。首次取消测试因使用全局故障fixture被跳过，
整场未算通过；改用已有真实native Claim fixture后取得上述真正RED/GREEN。

## 保留门槛

显式 Linux tag bridge 只返回 error，不授予权限、不发信号。组件用56节点真实CAS
写入 loader 构造的 intent，未执行生产 provider 的59节点 reservation。
实际七步provider/retained Query、后续Run正向恢复、owner/terminal/quota、managed
admission、PID1/cgroup、完整消费者和HTTP/部署仍待完成。真实signed大历史、最大合法
输入、transport调用预算和大N容量仍未验收。默认生产入口仍为Redis，生产切换0%。

原始证据和唯一审查记录在D工作树的
`.superpowers/sdd/2026-10-18-etcd-detached-runtime-native/`，失败记录也保留。
