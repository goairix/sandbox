# ds-ai-research API v0.3.32 部署回归

## 结论与版本

2026-09-17 15:02–15:08（北京时间）在 `ds-ai-research / aiadp-sandbox-fuse` 完成新版三副本
公共 API 回归和只读收尾核对，全部已执行用例通过。没有更新 Helm、重启业务组件、修改节点
或删除 Redis 状态；没有执行故障注入、跨 CNI 验收或持续负载测试。

三个 API Pod 使用 `sandbox-api:v0.3.32`，实际 imageID 均为：

```text
registry.i.huaxisy.com/library/ai-infra/sandbox-api@sha256:f542f74b5efa78725ae41f288d4e844449ea1fc70cb9b3ba3cf4fb8b7bec9565
```

API Deployment UID 为 `845af585-f245-400f-9ebb-9ff307894eea`。三个副本分别在 worker-1、
worker-2、worker-3，全程基线核对为 3/3 Ready、零重启，Pod UID、imageID、PodTemplate 未变。
Redis Sentinel 为 3/3 Ready，AppArmor loader 为 5/5 Ready；本轮 API 更新后的 Redis/loader
模板及 Pod UID 与上一轮记录一致，本轮测试前后也未变化。此证据不等于故障期间零中断证明。

五节点隔离 AppArmor 验收仍以此前 `v0.3.31` 的
[独立结果](2026-09-16-all-node-apparmor-api-results.md) 为准；本轮未重复 `v0.3.32` 的五套
逐节点隔离实验，也未核验新镜像与构建源码的对应关系。

## 已执行矩阵

三个端点均为分别绑定精确 API Pod 的 loopback port-forward，不经 Service 随机分发。
认证凭据仅进入进程私有内存/子进程环境，不打印，不放入命令参数或报告。

- 跨三个副本执行 Python/Node stdin；
- 缺失文件返回 404/FILE_NOT_FOUND，空文件下载返回空内容；
- 动态 sync 工作区挂载、写入、从容器同步、移除本地文件、恢复到容器及内容校验、卸载；
- 三副本工作区挂载/卸载状态一致；
- 自定义 128 MiB 内存与 100m CPU，读取实际 cgroup v2 验证限额；
- 三个副本的一次性 SSE 正常结束，包含 done、不包含 error；
- 三个独立 FUSE 工作区：创建、写入、flush、三副本 flushed/同步时间一致、拒绝 FUSE
  动态卸载、正常销毁；
- 普通 ephemeral/persistent 各三轮，三个副本同时 DELETE，共 18/18 返回有效成功 ACK，
  随后各副本 GET 均为 404。

命令由受保护的临时编排器传入三个直连端点及凭据，Go 的原始响应/失败正文不对外输出：

```sh
go test ./test/integration/api -run '^(TestDeployedAPIRemediation|TestDeployedOrdinaryConcurrentDestroy)$' -count=1 -timeout=8m -json
go test ./test/integration/api -run '^TestDeployedOrdinaryConcurrentDestroy$' -count=2 -timeout=8m -json
```

两个命令退出码均为 0，包耗时分别为 66.822 秒、21.651 秒。网络目标相关可选环境变量本轮
明确留空，未执行内部 Service 放行/撤销、metadata、跨 namespace 或 CNI 流量矩阵。SSE 的
真实客户端断连/写失败没有现场故障注入；其可控失败 writer 回归证据仍见
[本地质量整改](2026-09-16-full-lint-remediation.md)。

## 性能边界

FUSE 创建本轮 n=3，nearest-rank p50 为 3.670181333 秒，p95/p99/max 为 5.338305583 秒。
样本不足以估计长期分位数，但已观察到高于 3 秒的请求，因此 PERF-02 仍保留，按用户要求暂缓。

并发销毁测试每个子用例耗时为 4.20–6.01 秒，包含创建、并发 DELETE、跨副本 GET 及测试
开销，不是单独的 DELETE 延迟；不据此重算普通销毁 SLO。

## 清理与只读收尾

- 通过正常 API DELETE 收尾本轮 11 个显式创建的沙盒，三个副本逐一确认 GET 404；一次性
  SSE 内部沙盒由正常 durable cleanup 收尾，随后全局活动状态扫描为零；
- 仅对本轮四个随机工作区路径下的 `validation.txt`、`empty.txt` 两个固定文件做独立认证
  TLS DELETE 与 GET 404，共八个精确 key。没有访问业务前缀，没有声称完成全前缀对象审计
  或删除所有目录 marker；
- 最终仅剩六个 managed Pool Pod：普通池三条、FUSE 池三条 Redis prepared 记录，均绑定
  当前运行中的 Pod UID，无 active 非池或 Terminating managed Pod；
- 普通池只有一个 scope；三个 FUSE mounter 的 prepared health 均为 exact UID、generation 0、
  cacheBytes 0、无 restart/cache exceeded。预备 FUSE Pod 的整体 Ready=false 是未授权挂载
  时的设计状态，不能据此判失败；
- Redis 仅 ROLE/SCAN/GET，active record、active controller、session v2、ephemeral lifecycle、
  workspace owner、workspace lease 均为 0；没有使用 KEYS/DEL；
- NetworkPolicy 共九条，其中六条 managed（普通三条、system 三条），全部关联当前 Pool
  Pod UID，没有 Terminating managed policy；CiliumNetworkPolicy 为 0；
- 分别核对三个精确 API Pod 最近十分钟日志（每 Pod 至多 3000 行），ERROR、panic、
  termination unconfirmed、reconciliation failed、lease lost 匹配数均为 0；
- 自己的三个直连 API port-forward 均已退出，业务 API/Redis/loader 的基线未变。

外部 Ingress/TLS、Sentinel 实际切主、全组冷恢复、节点故障、持续压测、双华云 VPC/OBS 仍
不属于本次通过范围。下一阶段的现场准备见
[双华云环境准备核对](2026-09-17-shuanghua-vpc-obs-readiness.md)。
