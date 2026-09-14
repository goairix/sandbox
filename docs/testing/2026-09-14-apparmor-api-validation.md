# 2026-09-14 AppArmor 启用后的线上 API 验证

## 结论

目标为 `ds-ai-research`、`aiadp-sandbox-fuse`、release `sandbox-fuse`。2026-09-14 21:38 起（北京时间）检查用户完成升级后的部署；22:10 左右完成资源收尾。

AppArmor 启动门禁已恢复，普通、sync、FUSE 的主要功能及跨副本池复用通过。换本地网络后的 FUSE 文件压力专项也通过。**不能宣称全量通过：普通无工作区沙盒的跨副本并发 DELETE 稳定返回部分 `503 SANDBOX_CLEANUP_PENDING`。** 三个复测沙盒最终均销毁，未造成活动沙盒或策略遗留。

本轮仅通过 API 创建、操作和销毁隔离测试沙盒，辅以集群只读检查；未修改产品代码、values、release、业务沙盒或节点策略，也未手工修改 Redis 数据。没有 upgrade、rollback、uninstall、扩缩容或故障注入。

## 部署身份及健康

- Helm revision 4，状态 `deployed`。
- API 镜像 `sandbox-api:v0.3.24`，三个副本实际 digest 均为 `sha256:a5fc32055cd9e8bff17657831e5717a6f2a2143f58fa7e5461b655705dccf5a0`。
- 加载器镜像 `sandbox-apparmor-loader:v0.3.24`。仓库 manifest list 为 `sha256:d0fc6fe6f809d3cdc60a88a82751ef04230002f9f062e4a429927e1980d1bd9b`，其中 arm64 manifest 为此前节点验收的 `sha256:5a984dcb978f6634acee4446bb2f1ae5d2aca918ee53b189372deb688bcf908c`。四个节点的 imageID 报告前者，worker-2 报告后者；通过 `docker buildx imagetools inspect` 确认索引和 arm64 子 manifest 的关系，不把这种表示差异误判成镜像漂移。
- 集群节点均为 arm64，Kubernetes `v1.33.6`。加载器 DS 模板及五个实际 Pod 的 `enableServiceLinks` 均为 false，DS 5/5 Ready、零重启。
- API 三个 Pod UID 在测试前后不变，重启数均维持 7。这是修复前累计次数，本轮新增重启为零；最终各副本 `/health`、`/ready` 均 HTTP 200，受保护接口未认证检查返回 401。
- Redis Sentinel StatefulSet 3/3 Ready、零重启；收尾时 `redis-sentinel-1` 为 master，两个副本 online、lag 0，`min_slaves_good_slaves=2`。

## 已执行测试

| 覆盖项 | 结果及证据 |
| --- | --- |
| `TestDeployedAPIRemediation` | PASS，267.58 秒；三个 API 副本均参与 |
| Python/Node 标准输入、文件接口、SSE | PASS；空文件、缺失文件 404、文件内容校验、一次性 SSE 正常结束 |
| 动态 sync 挂载及跨副本生命周期 | PASS；挂载信息、同步、恢复及卸载 |
| CPU/内存与网络控制 | PASS；100m/128 MiB cgroup、指定 Service 通信、白名单撤销/恢复、15 次内部 CIDR/地址拒绝 |
| 普通、sync、FUSE 模式矩阵 | 7 个功能子项 PASS；无工作区、ephemeral/persistent sync、ephemeral/persistent FUSE、同前缀跨模式冲突、不同前缀并行 |
| 存储持久化与冲突 | PASS；写入后销毁、重建读回内容，FUSE flush 后状态及时间戳、冲突请求拒绝 |
| 跨副本共享池及文件访问 | PASS；3 普通 + 3 FUSE 并行申请，6/6 runtime UID 与申请前 prepared 池实例一致；三个副本读取相同 runtime ID，跨副本上传/下载内容一致 |
| AppArmor 实际使用 | 三个已申请 FUSE 沙盒的 mounter `/proc/1/attr/current` 均为下面的 exact profile `(enforce)`，实际覆盖 worker-1/worker-3 |
| 普通容器基础限制 | 三个网络切换后的普通沙盒均为 UID 1000、`NoNewPrivs: 1`、`Seccomp: 2` |
| FUSE 跨副本并发销毁 | PASS；3 个沙盒，每个向三个副本同时 DELETE，9/9 HTTP 200 |
| FUSE 文件压力专项，换网络后 | PASS，76.63 秒；256 个小文件、16 MiB 流式上传、目录、覆盖/追加/截断/重命名/删除、Git、真实 fuse.s3fs、flush、租约冲突与销毁 |
| 普通跨副本并发销毁，换网络后 | **未通过**；9 次请求仅 3 次 HTTP 200，另 6 次 HTTP 503；最终各副本查询均 404 |

本轮 mounter 实际 profile：

```text
sandbox-fuse-37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d (enforce)
```

三个 FUSE 创建样本中位数 9.930 秒、最大 10.545 秒；换网络后三个普通创建样本为 0.220、0.240、0.278 秒。样本很小，不据此宣称生产 p95/p99 或压力下吞吐。

## 网络切换前后的区别

首轮 `TestWorkspaceFUSE` 用时 421.68 秒，7 个功能子项通过，压力子项失败：上传调用检查到 2xx 后，后续 stat 未找到 `large.bin`。该上传测试助手只检查状态码，没有读取上传响应 JSON，不能用这一条证据确认服务端完成了上传。

之后专项复跑还遇到 HTTP connection reset、上传 BrokenPipe、Kubernetes Exec TLS handshake timeout、port-forward 流建立超时。普通容器的大文件探测也遇到 BrokenPipe，不能仅归因于 FUSE/AppArmor。用户切换本地网络后，重新建立三个独立 Pod port-forward；压力专项通过，普通并发 DELETE 的 503 则仍可复现。

因此，前述压力失败在换网络后未复现，但原始失败的精确原因没有完全确定；不据此做产品改动，也不把服务端明确返回的并发 DELETE 503 归因于网络。模式矩阵与压力专项分轮通过，不声称同一轮完整套件连续全绿。

## 普通并发销毁：根因与待修复方案

复测沙盒为 `sandbox-bq61ga44am`、`sandbox-gzn3hmeqij`、`sandbox-kfous68vsn`。每个沙盒同时向三个 API 副本 DELETE，结果都是一份 200、两份 503；503 约 0.27 秒返回：

```json
{"code":"SANDBOX_CLEANUP_PENDING","message":"sandbox cleanup pending: cleanup already owned for sandbox-ID"}
```

成功请求分别耗时 32.383、32.598、33.385 秒；随后三个副本 GET 及最终 DELETE 均为 404。

源码 `internal/sandbox/active_store.go` 的 `destroyDistributedSandbox` 普通无工作区分支，在 `acquireActiveController` 未取得销毁权时直接返回 `ErrSandboxCleanupPending`，handler 映射为 503。相邻的 `destroyDistributedWorkspace` 分支在同样情况下调用 `waitForActiveCleanup`，等待记录消失并确认收尾。因此，本次差异是普通分支的并发收尾语义，不是 AppArmor 门禁、池未复用或双重删除故障。

建议下一轮修复：普通分支复用经过最终清理确认的等待路径；仍仅由持有销毁权的控制器删除运行时，其他请求等待结果。真实超时、取消、运行时身份不确定或持久化未确认仍返回失败，不直接吞掉 503。先补跨副本同时 DELETE 的失败回归，以及等待超时、所有者失败/接管、资源删除确认等边界，再修代码，最后在用户更新镜像后复测。此次只定位和记录，未实施这一改动。

## 收尾及验证边界

- 测试沙盒及后续清理助手通过 API 正常销毁；四份模式矩阵 `lifecycle-value` 经核对固定测试内容后删除并 flush，四个助手最终 GET 均为 404。
- 最终 Redis 扫描 active records/controllers、persistent sessions、ephemeral lifecycle、workspace owners/leases 均为 0。
- 普通及 FUSE Redis 池记录各 3 份、全部 prepared；runtime UID 与剩余六个池 Pod 对应，未发现 Terminating 的托管沙盒 Pod。
- 最终 NetworkPolicy 为 3 份 Chart 策略及 6 份当前池实例策略，CiliumNetworkPolicy 为 0；不存在已销毁测试 runtime 的策略。
- 本轮三个本地 port-forward 已停止。未手工删除 Redis key、策略、Pod 或存储前缀；未做对象存储全前缀零对象审计，不宣称测试目录标记或所有异常上传临时对象均不存在。
- 未进行 Redis failover、节点重启、网络分区、API 副本故障或长期负载测试；健康状态不等于故障切换验收。
- 本轮 API 实际 mounter 覆盖 worker-1/worker-3；加载器 5/5 Ready 不等价于五个节点均完成 API 挂载验收。worker-2 的独立组件级真实内核验收见 [历史记录](2026-09-14-apparmor-live-prepared-results.md)。
- 请求直接经本地 Pod port-forward，不将本次结果等同于外部 Ingress/TLS 链路验收；本轮也没有重做真实 s3fs 子进程的 profile 探测或所有越权负例。
