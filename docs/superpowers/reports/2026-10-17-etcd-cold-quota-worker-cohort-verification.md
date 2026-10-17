# Cold quota worker Stage cohort 验证记录

日期：2026-10-17。本文记录 Root 提供的真实验证结果；实现作者未执行 Go、gofmt、Git 写操作、Docker 或 native etcd 测试。实现和测试已冻结；唯一完整 Task 审查已批准。

## 改动范围与协议边界

新增私有 `continueColdRuntimeQuotaReturnWorker`，从原 worker journal permit 进入一次独立付费的 Stage cohort。同一个原 worker call 连续执行 Grant、guard、Commit、原 identified lease Revoke，随后才发布 call 完成；不等待中途 refill，不续租原十秒 Stage lease。

两个 cohort 产品文件分别负责生命周期与原协议证据、事务组合与请求预算。新增 worker fences 文件仅共享 base4 精确组合不变量，原 journal helper 保留原形状检查后委托该 helper。新增 cohort 测试使用既有 decoded cold-history host fixture，并通过真实 worker journal ACK 获取原 permit。

共享 helper 要求 body 至少四项、worker 恰好 W24；body 和 worker 的前四个完整 protobuf comparison 均须匹配当前 backend 新生成的 identity Value/Lease、restore Value/Lease。只删除这一份完整相等的 base4，保留其余二十项 worker comparison 的值、目标及顺序。journal 的 body 30/37、cohort 的 body 6/34 入口约束分别保留；没有按 key 去重或扩大导出权限。原 journal 的十八种 base 字段变更矩阵随本轮回归执行。

额度不足时，两桶均不部分扣费，原 permit 不 consume，可等待后重入。成功原子取得 Range 262144 字节与 Claim 1 后，才 CAS consume；重复 loser 不能破坏 winner，也不能开始第二次 Grant。原 prepared mutation、locator、digest、reference、journal ACK 与 enrolled owner 指针保持绑定，不分配新 UUID，不重新认证另一份快照，不覆写 owner.context。

仅原可信 header、正 revision 与正 lease ID 可以授权原 lease 清理。已 identified 的坏 TTL、Grant.Error 或取消仍须清理同一 ID；清理使用独立有界 context 并实际 join。原 lease 已 missing 按既有 rpctypes 语义视为安全；清理结果未知则不给出 commit permit，保留原 journal/reference 供未来仲裁。call.finish 晚于所有 native client 返回及清理返回；enrolled owner 继续保留给后续独立付费 proof reads。

私有 commit permit 只保存原绑定、原 journal 指针和 ACK 标量值，表示原 Stage 已可信完成且原清理已 join。它没有完整 Stage 或 mutable native response 指针，也不是已验证 quotaCommitted、公开 Entry、物理运行时退出或 preparation 删除权限。

## Root 真实 RED 与验证结果

| 轮次 | Root 提供的结果 | 输入与原始输出 | 原始输出 SHA-256 |
| --- | --- | --- | --- |
| RED01 | 真实失败，仅缺少 `coldQuotaWorkerCommitPermit` 与 `continueColdRuntimeQuotaReturnWorker` | inputs=9096；raw=651 | `d5223886d7c20376bd3bb09332c33ff8b899d0cfaf2fca5bfcb2b977648d2985` |
| host01 | PASS，1.966s，12 个顶层测试：11 个新增 cohort 测试及原 journal composition 测试 | inputs=9099；raw=24853 | `01100a39f9c1f6bb29305f2cf5c5569538bde8b5170836e77c78e96d4683b05d` |
| race01 | PASS，4.449s，13 个顶层测试：host 范围再加原 public wire model prefixes 回归 | inputs=9104；raw=30978 | `c69c503f8ad57d3ea001d36a9c3e77160810ebf335294bfd13165ce7219b54b1` |
| Linux vet01 | PASS，Linux arm64、CGO=0，etcd 包 `go vet` | inputs=9194；raw=0 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |
| 最终 Task review | Spec/Quality PASS；M1 注释问题已关闭，无未解决 C/I/M | 唯一完整审查员定点核对最终证据 | 不适用 |

Root 报告上述 RED、host、race、Linux vet 各轮完整 17 项、源码、工具、pre/post、归档与当时 current 审计均 PASS；每轮审计后才开始下一轮或修改源码。依赖 A/B 在 main `8e745f9` 验收后，Root 合入 D，真实 RED 所用 D HEAD 为 `3adeff35b48e16797f146b8b3402ec00b04f6305`。本文没有把输入数量或输出校验和替代为测试行为证据；这些值用于对应 Root 保存的原始验证材料。

唯一审查员发现一项 Minor：原 prepared 字段注释仍把所有路径描述为先 consume，
与新增 worker 的额度不足可重入不符。完成 Linux vet 及完整审计后，仅同步该字段
四行注释，区分 public 先消费与 worker 付费准入后消费；没有行为、测试或工具修改。
最终 gofmt/diff check 干净，未为这项纯注释同步重复测试。

## 请求、响应及顺序证据

Root 的真实 worker 请求捕获后，只扩大有边界的字段进行 protobuf sizing，未把扩大后的帧提交为业务历史。完整 W24 保留时得到：

| 阶段 | 最大请求字节 | Txn 节点 | 最大 canonical 回复字节 |
| --- | ---: | ---: | ---: |
| guard | 45612 | 29 | 6523 |
| Commit | 120148 | 62 | 10844 |

Grant、guard、Commit 和原 Revoke 的请求合计 165772 字节；加固定四阶段 receive caps 37888 字节，总计 203660 字节，小于一次 Range 的 262144 字节。guard 和 Commit 各请求均小于 128 KiB，节点均不超过 64。receive caps 分别为 Grant 4096、guard 16384、Commit 16384、Revoke 1024 字节，也涵盖被接收的错误或 malformed ACK，而不只计算 canonical success。

这里计量的是应用 protobuf request/response envelope，不包含 HTTP/2、TLS、status/trailer 或超过 receive cap 而被拒收的流量；host fixture 与 sizing 不代表 native etcd 或生产容量证明。

成功 trace 精确为原连接、同一 paid call 的 `Grant → guard → Commit → Revoke`；没有隐藏 metadata/postread RPC。原 guard failure 的 identity/restore 两点，以及 Commit failure 的 identity/restore/receipt/guard 四点与原 receipt 仲裁均保留。原 public 路径另有回归，仍为 `journal → Grant → guard → refund → return-read → receipt-read → Revoke`。

测试独立变更 profile、session、partition、Range、Claim 的 Value/Create/Mod/Lease，在 guard 与 Commit 两阶段逐项拒绝；覆盖信用不足后重入、复制或错绑 permit、重复竞争、各阶段 ACK 丢失及 malformed 回复、原 receipt/guard 失败仲裁、坏 Grant 及取消后的同 ID 清理。Release/Close 在四阶段使用 post-invoker gate，检查实际 client 返回前不能发布完成，并区分 Stage lease 与 session lease 的各自清理。

## 尚未覆盖的后续范围

本改动不实现独立付费 return/receipt postreads、metadata reader、自动 actor、P gate、Resolve、retention 或 quotaCommitted 终态证据，也没有修改公开 API 或公开 RPC 顺序。物理释放、preparation 删除、native capacity 与端到端 worker 接线均不能由本次 host/race 通过推导。后续任务仍须基于原 reference 和注册 owner 完成对应协议。
