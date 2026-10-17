# 自动 cold quota 归还及 v3 入口验证

## 已提交范围

`2a6c46d` 增加真实签名/common-birth 夹具；`71aa9ad` 将原 paid
history/recovery/prepare/journal/cohort/postread 接入实际 DueWorker。
prephysical 先封原 job 来源；postphysical 等实际 effects 和原 Run joined 后归还
配额。私有原 job 验证结果约束 E 结算、S 释放和原 fenced P 删除。终态积压使用
同一 32-job registry 的 quota-only 路径，backfill 保留未完成配额验证的 P。

单个共享 metadata workspace 与所有紧凑 job 合计预留额外 4MiB，按实际保留图
计量；cold 模式 cache 降至 32MiB/9567 keys，新增 Range lane 限于显式 cold
模式。Unknown 在实际 native 返回后使原 session 失效；真实 typed conflict
结束当前 job 并释放 workspace。原 v1/v2 行为及完整 CAS 比较保持不变。

`00806a9` 接入严格 v3 mounted 入口及独立复制的 256 项预期配额配置；
`d16b720` 增加显式 `sandbox-cleanup-worker serve-cold-quota <manifest>`。
该入口不初始化库存，仍要求已有 ready/profile/restore 证据。

## 验证结果

| 范围 | 结果 |
| --- | --- |
| 新 actor 定向 host | 20 顶层、41 含子组通过，1.587s |
| actor/automatic/cache/lifetime/result/P/backfill race | 79 顶层、286 含子组通过，42.125s |
| actor Linux vet | 9221 输入，通过 |
| v3 配置 host | 两个互补选择器共 9 顶层通过 |
| deployment 全包 race | 34 顶层、190 含子组通过，2.020s |
| command host/race | 各 9 顶层、37 含子组通过 |
| deployment / command Linux vet 与 command ELF | 9230 / 9204 / 9204 输入，通过 |
| native runner Linux build/vet | 各 9419 输入，通过 |
| 真实 mounted v3 自动物理清理及配额归还 | 35.92s，通过 |
| 真实独立进程自动物理清理及配额归还 | 36.16s，通过 |

两场 native 均从真实冷创建、已支付 preparation 和启动前永久 P 积压开始，没有
直接调用 Execute 或公开 quota settle 代替自动消费。各完成非空 workspace
归档、observer/provider 终止及原 Run join，保留 Task birth36、quota slot birth10
和原 owner/epoch；实际 journal138、receipt140 均早于原 P 删除144。
各发生一次 provider PATCH、一次 DELETE、14 次 bridge upgrade；E 结算、S 不再占用。
mounted Close/Wait 均 nil；独立命令 PID64 的 SIGTERM/Wait 成功、stdout/stderr
为空，原 session 消失且原 lease TTL=-1。各原 worker、clock 和 TLS etcd 实际 join。

整个 native 测试 72.08s、容器 exit0、无 OOM；资源边界为无网络、只读根、私有
cgroup namespace、512MiB/2CPU/256PID、四个只读 ELF 和有限 setup capabilities。
原始日志 11733 字节，SHA256
`0ca66f1ff3fad4e95981c7fe900881875da0a806795bbb7583440fab5034f089`。
runner ELF SHA256
`fd1b807f8c7bccf240a7b3cc6744038793fe4a8267126ce9c4e800bcfec5f3cf`；
独立 command ELF SHA256
`e3c574766d39359f8c4a1693c7fd85b534c866d5b080b04717c99e09111306a8`。

## 保留的失败及边界

首次 host 发现的测试夹具问题、Unknown 未失效 session 和并发所有权问题已修复；
同一 Task 审查员限定复核关闭。扩大 host 选择器误纳入 13 个缺少真实三成员
TEST_ETCD_ENDPOINTS 的旧测试组，该整轮保持 FAIL，未计作集成验收。
最终命令逐项核对编译字段、源码/工具指纹及执行前后输入；原取消故障测试的
context canceled、fixture etcd 的启动/关闭和已不存在 lease 警告均保留。

monitor 复用历史已验证 ELF；新增 native 断言随后改变了 dependency TestGoFiles，
因此其 fresh 全清单一致性检查明确 FAIL。唯一差异不属于该 go build 的
GoFiles/CompiledGoFiles；保留新旧指纹并单独完成生产输入复用核对，没有冒称完整
当前清单匹配。新 runner 已编入最新断言。

native finish helper 在精确 rm 成功后因 Docker 小写 missing-object 文案返回1。
Root 对原完整 ID 再 inspect，确认 exit1/空列表/精确不存在对象；保留原日志，
只修正文案大小写归一并用该实际回复验证，没有重跑 native 场景。

这两场各一条 Task，不证明 32 物理并发、大量已有 sandbox 的容量或延迟，也未
覆盖全部 Unknown/替代 worker 故障组合。默认 Redis 入口、其余 API/Pool/Docker/
Upload、detached 消费、FUSE/collector、部署恢复和最终替代仍需完成。
