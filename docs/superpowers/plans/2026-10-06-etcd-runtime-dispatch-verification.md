# etcd runtime dispatch 验证记录

分支：`codex/etcd-state-management`。本批实现到 `e3579dc`：将 runtime 调用的原 operation 与输入持久化，并在原 creation claim 下原子声明。该接口返回元数据证据，实际 runtime 调用、可信 target 回执、Publish、GC 和生产接线仍属于后续单元。

## 分单元提交与审查

| 提交 | 功能单元 | 审查结果 |
| --- | --- | --- |
| `52ea97f` | 内部新 attempt locator 与一次性 Stage builder | spec 合规、quality approved |
| `d532ece` | 永久声明/input 与固定点恢复读取 | 独立审查发现 receipt 响应错误分类偏差 |
| `4356d6b` | 修复公开 loader 的 receipt 点响应错误分类 | scoped re-review：已修复、spec 合规、quality approved |
| `e3579dc` | 原 claim 下原子声明、重放与故障仲裁 | spec 合规、quality approved |

实施计划、scope 与上下文补充分别提交为 `7a49381`、`8c51d72`。每个功能单元完成实际验证和自审即提交，再进行独立审查；发现的偏差另作小修复提交。

## 行为证据

- 同一 intent 的 16 个并发声明只提交一个 operation；失败者返回自己的 unknown attempt，不能伪称 declared。后续重放读取获胜 operation，不修改原 creator、输入或期限。
- 声明、input、committed receipt 同首次 revision，永久 Lease0；恢复者只点读固定 key，严格校验身份、归属、schema、摘要、revision 和 receipt，再从实际回执取得完整 StageReference。
- 原 creation claim 的 24 条比较保持不变；加两条 absence 比较、两条永久写和 Stage 预留，总预算 42，原 64 上限未放宽。声明 Stage Lease 固定 30 秒。
- 同输入重放和不同输入冲突均不 Grant；完整编码与输入校验在 Grant 前完成。caller 和返回对象的修改不会污染永久记录。
- 真实提交成功但丢回复：返回 unknown/nil Entry，经 exact receipt 仲裁 committed，新 Backend 可读原 operation。
- 暂停完整提交事务，由 resolver 先写 aborted：迟到事务不能写声明/input。claim 原 Lease 消失、同值重建、control/restore 变化也阻止旧事务。
- guard 初始化未知时保留 exact reference；本地 lost 不代替服务端仲裁。Stage 清理失败独立报告，不降级 known commit，也不释放 workspace owner 或 creation claim。

## 验证命令与结果

控制器对 `e3579dc` 执行：

```sh
bash scripts/test-etcd-state.sh -v
go build ./...
go vet ./...
```

新建 owned 三成员 etcd 3.6.15 fixture：**race exit 0，140 项顶层 PASS、0 SKIP，21.732s**。包含真实 NOSPACE 与 leader pause/unpause；fixture 及其卷由脚本 trap 清理。

控制器独立 build、vet 均 exit 0、无输出。实施者的最终 `go test ./...` 带真实开发集群 endpoints，全部有测试包通过，etcd 包 16.470s；定向 race 12.468s。gofmt 与 diff 检查通过。

故障 fixture 有 5 条预期 etcd-client warn：3 条 LeaseNotFound、2 条 NOSPACE；相应测试均通过，不表述为无警告输出。完整仓库 race 未执行，本批完整 race 范围为 etcd package。全包 lint 未作为通过项；前一批已记录的 14 项 lint baseline 未在本批修复。

## 大量存量记录的字节预算

通过真实声明后读取实际三条 etcd value 计数。以下 payload 是合成 JSON 字符串，ID 长度取测试夹具，**不是实际 Pod 请求分布或吞吐压测**。

| compact payload | 声明 | 完整 input | receipt | 三条 value 合计 | 保留 10 万份 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 19 bytes 夹具输入 | 1176 | 412 | 347 | 1935 bytes | 193.5 MB |
| 4 KiB 合成输入 | 1176 | 4489 | 347 | 6012 bytes | 601.2 MB |
| 8 KiB 合成输入 | 1176 | 8585 | 347 | 10108 bytes | 1010.8 MB |

MB 使用十进制。上述未计 key、MVCC、WAL、索引、碎片、复制副本、其他域记录和快照，不能把设计中的 8 KiB `B_live` 建模目标直接当作这批恢复日志的总成本。终态日志保留量应按创建速率、保留时间和 GC 服务率另算；未决日志不能按年龄删除。

这一条声明/恢复路径的 RPC 和内存工作集不随存量 N 扫描增长，字节存储仍随保留记录数增长。后续 runtime producer 应优先引用已有 immutable snapshot/version/digest，避免机械复制整份配置；snapshot GC 必须尊重未决引用。实际生产输入分布、日志保留预算和后端放大率需在接入后重新采样。

## 本批决策

在 claim 的私有 request/control 上下文之外，保留原 immutable WorkspaceIdentity。原因是既有 LoadRuntimeDispatch 要验证存储 authority；不能从裸 hash 重建身份或增加绕过。成本是每个活跃 claim 多保留少量不可变字段；若该选择不合适，需要重做上下文传递，不增加每沙盒常驻 controller 或 Lease。

实施计划的提交顺序已同步为“完成实际验证和自审即提交，独立审查的修复另提交”。它遵循用户分单元及时提交的要求，也让审查包能指向实际提交范围；代价是本地历史可包含随后修正的提交，尚未合并或部署。

## 后续边界

最终 whole-branch 独立审查通过：完整审阅从 `b99b823` 到 `e3579dc` 的 58 文件、10,581 行 diff，无需修复的 P0–P2。两项非阻断建议明确接受：补强通用 Stage builder 的 Grant 边界复制测试，以及分类保留预期故障告警。完整报告见同目录 `2026-10-06-etcd-runtime-dispatch-final-review.md`。本批不能据此切换生产后端；可信 target publication 还需绑定 exact runtime UID/BootID、原 operation/input digest、当前 task epoch、业务期限、gate 与一次 mount attempt。可信 helper 的实际证据来源成立后，组合 Publish 才可将 control 置为 active。
