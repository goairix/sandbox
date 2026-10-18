# detached quota-return 编码组件验证

源码 `0bd7852`（辅助 `d6bb0da`），仅新增独立 v2 record/codec 与测试两个文件。
原 v1 类型、字节和调用者保持原样；没有接到 Grant、Stage、journal、quota writer、
自动消费者或终态读取。功能约70%、整体约45%、生产切换0%。

v2 使用独立 domain/version，内嵌已验收的 `detachedOwnerReleaseRecord`；校验可由
记录表达的 Task/namespace/epoch/partition、workspace/generation/runtime/node、
slot/ref/stamp 及时间顺序。quota partition 保留独立含义。canonical 重编码和递归
类型校验拒绝重复、未知、缺失、null 和混合 v1/LocalComplete/archive 字段；native
decoder 另查 lease0/version1/create=mod 及出生晚于来源。它只验证结构，不能认证
创建 IntentID 来源、真实释放历史或产生退款权限；这些仍须后续私有 proof 消费者。

独立复算外层7717字节，加已接受 nested seal7643字节，共15360字节，保留原16KiB
上限。Go 同时最大模型实际得到15360字节，包含 root512、外层合法128字节字段、
三组 runtime 转义和最大标量；是 codec 模型，不宣称该组合有实际签名来源。实际
单记录 protobuf 使用679字节派生键，PutRequest16045、RangeResponse16117字节。
这些只测单记录；完整事务、native quota、32并发和大 N 容量均未验收。

| 检查 | 结果 | 输入数 |
| --- | --- | --- |
| `detachedquotacodecred01` Host六项 | 三正向到原 stub 失败；v1/foreign/strict通过 | 9154 |
| `detachedquotacodecgreen01` Host六项 | PASS | 9154 |
| `detachedquotacodecrace01` Darwin arm64 race六项 | PASS | 9159 |
| `detachedquotacodecvet01` Linux arm64 vet | PASS | 9262 |
| `detachedmainquotacodec01` 主分支整合六项 | PASS | 9154 |

每轮完整17项 Go 输入、工具/源码归档、前后指纹审计通过，原 RED 保留；共享构建和
依赖缓存，仅归档仓库源码。证据在辅助 D 工作树
`.superpowers/sdd/2026-10-18-etcd-detached-runtime-native`。唯一审查员核对两完整
文件、十个直接依赖、实际结果与冻结指纹后 Spec/Quality PASS；同审查员确认主分支
两 blob 相同及整合证据。注释说明数据与权限边界，校验按职责分段；存储 codec 留
etcd 包、复用纯 validator，无新导出能力、传输依赖或反向依赖。

后续接 owner 的真实释放/幸存历史、独立付费 quota reader 和 v2 终态传输 profiles；
现有 quota 执行底层不重写。全局 phase 内存准入、容量、部署恢复和 Redis 移除仍开放。

## 完整 W20 付费历史封包补记

`38ec47d`使用原20比较请求生成器及实际 Task 派生的13键，覆盖 namespace/root、
restore/worker、snapshot与locator合法长度和最大标量，逐点计入全部比较键和回复。
Head请求25034、回复44430字节；历史请求37638、回复223702字节，合计261340，
保留原262144门限，仅余804字节。未增加上限、删伴随点或增加隐藏读取。

`detachedquotawireprobe02`、`detachedquotawirerace01`、Linux vet及主分支
`mainquotawire01`通过，完整17输入和归档指纹审计通过，唯一限定审查接受。
超限实际 reader 在 credit/native 准入前拒绝；该运输模型不能证明完整 paid cursor、
实际签名来源、quota writer、退款或容量。首次 locator/stage 测试编译错误保留，
不当作产品 RED。
