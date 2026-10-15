# Enrolled launcher / publication transport 验证记录

当前状态：本单元已范围验收；唯一完整审查的 I1 经原作者修复和同reviewer限定复审闭环，Approved。主程序仍使用 Redis，整体迁移未完成。

## 交付范围

正式 `sandbox-launcher serve` 仅接受 canonical v2 protected 配置，固定节点 UID/boot 与 enrollment UDS，执行真实 BootstrapPID1 后只构造 NewEnrolledSupervisor。新增只读 `runtime_enrolled_publication` 管理 TLS 请求；request≤32768、response≤65536、certificate≤4096，原嵌套证据上限不变。Destination 保持原 origin seal、一次 send/read、总5秒和 authenticated TLS EOF，Supervisor 只编排原 Enrollment/EnrolledPublicationProof；返回副本仍需 Backend 原 verifier/native guards，不能作为新副作用权限。

源码 BASE `f618f314d647ec0d6f4782df165b7efd756f70bc`。四个连贯源码提交：

- `2976b1688afd50d2adf3f7256f94440d711fd658`：v2配置、正式serve接点、bounded frame。
- `3f6dfde9e5a29a72e006a59241fcc5b62b04f6a5`：真实管理TLS client/server只读查询。
- `7e2001f95c7ef37412e0e6cf09613c1aa3e85de0`：正式CLI PID1真实enrollment/publication native fixture。
- `78f53ab0205e995d5010d1889c07bb3891fb8c8a`：进程存活且gate关闭后的查询拒绝补测。

## 已核对结果

作者 config/frame/TLS 三个新增选择器均保留预期 undefined-symbol compile RED；对应有限 host/race GREEN 与 affected vet 已完成。Root独立有限第一组2top race及2包vet退出0，实际race/CGO1全17字段2285输入pre/post文件名与SHA一致；第二组4top race及2包vet退出0，2284输入一致。它们是各自源码快照，不称一次最终同源全套host测试。

真实正式CLI `native-v1`：原build退出0、run原session97774退出0。子进程仅设置批准私有隔离后 exec 本次构建的 `/fixture/launcher serve` PID1，实际 signed clock/独立 observer/SO_PEERCRED/cgroup登记、bootstrap activate和新TLS查询，原PublicationVerifier验证真实ready（1863字节，digest `722469a29f9b3d41bceaa85dfec7de91a0ec3327047138ae710fd8a733650c25`）。错误publication certificate/runtime/issuer拒绝，第二wrong-node子例在原登记身份校验拒绝。原两launcher Wait0，observer/clock原Serve join，etcd原SIGTERM，attach/container原Wait0/0。exact container `a28236c73c30c3e4964b63cc4a382b66127719b49070fdb6c4a7abdc42da202f` removed，精确inventory为空，final exit0/noOOM。

`native-v1` 的关闭案例先SIGTERM及原Wait，只证明进程关闭；Root要求另加 `live-gate-v1` 单独新选择器，未重放旧两个案例。新build原session25418退出0，run40896退出0：真实认证ready（1869字节，digest `8196b34709fd7d3175936abd191d20ab127469fc1ed2b1db6a0d1dd5ec0fe463`），原认证issuer经真实TaskCloseData TLS获得原verifier验证的signed data_closed receipt；Signal0证明原进程存活，新publication拒绝，随后相同TLS关闭历史仍可认证且/proc/exe仍是正式CLI，最后才SIGTERM原Wait0。observer/clock原join、etcdSIGTERM、attach/container Wait0/0；exact container `d58985ca8df4df891109c4de874bec03d95e38ddd12a73f69bc4a1a68480491f` removed/inventory为空，final exit0/noOOM。fixture签名关闭仅收紧gate，不声称Backend/native task claim准入验收。

两epoch均是批准的networknone/private namespaces/readonlyroot/NNP/512MiB/2CPU/256PID及原cap白名单fixture；内部private /run≤1MiB tmpfs，不挂host/run。每轮实际LinuxCGO0 GoList全17字段runner993包/9056inputs，正式launcher171包/1991inputs，before/independent-after文件名与SHA匹配。Root独立核对每个实际source archive成员、当前源码、ELF和script绑定。最后三affected包Linux vet退出0，全17字段9063input pre/post匹配；最后仅Linux fixture新增后的Root controlrunner Linuxvet/format/bash-n/scoped diff/index检查也退出0。

## 审查与保留限制

唯一fresh完整Taskreview消费四源码commit/17paths的Task-only92579字节diff，SHA `2eb39c5be156d22c03606b1933e91d2964c076f322b69276ba4db275a17c6a82`，排除interleaved其他Task修复/docs。结论 C0/I1/M1、Needs fixes：

- I1：RawMessage预检只看到重复字段最后值，后续typed解码可能先分配早先超nestedcap blob、最后canonical才拒绝。outer cap仍有界、无成功认证绕过，但不满足明确的分配前nested cap合同。原作者修复commit `9336fc00c6cdb70fa7b436aff45874eb5c99bb1f` 仅改frame/test两个文件：typed解码前先执行固定schema的有序raw预检，拒绝duplicate/unknown/casealias/escaped字段并保持原nested/aggregate caps。8个直接raw-validator回归先RED（原9236退出1），host18879/race7897/transportvet54ee8d均退出0；Root独立1bd992审计四epoch全部17字段pre/postfilename/SHA，RED2114仅核历史前后、GREENhost/vet2114及race2125匹配最终当前源码，format/diff/index也通过。仅affected选择器，无native/旧矩阵重放。原reviewer scoped R1确认ADDRESSED，无新增C/I/M，Approved；原M1继续保留，不清洗历史日志。
- M1：原始私有etcd单端口/simple-token启动警告、正常关闭的HTTP/gRPC/closed-network日志和wrong-node EOF拒绝警告保留；不称pristine。原Wait与实际PASS支持非阻断fixture归因，不过滤原日志。

Root第一次archive审计退出1，原因是错误要求归档成员数等于输入数；macOS bsdtar多带8525/1780个与真实源一一对应的AppleDouble `._` metadata。只修正审计计数假设后，实际每个源成员SHA和全部绑定通过；未重写archive、未重跑native。这个审计失败记录保留。此前Root还在提交前纠正显式request32768上限和TLS测试原raw.Close之后的worker join；均已新affected验证，不隐藏为初版正确。

Git由Root exactpaths/expectedHEAD窗口执行common→OPEN OWN fcntl锁、branch/HEAD/初始indexempty/exactstaged/diffcheck/commit/postempty。R1原5cae49退出0并归还FREE/indexempty。历史实际输入/原Wait/Git操作由Root原始核对支持，源码review不替代这些证据。R1 scoped diff17658字节/SHA `70720adcd169ed2646c7ee4296fca5fa523351a6077055afd74559a4961fb037` 已交原reviewer；修复后的transport/native执行分别属于不同明确源码快照，不能拼成一次最终同源native全轮。

Root最终2cc06c核对17条最终source SHA与冻结作者报告SHA全部匹配。Task acceptance只覆盖本记录中的正式CLI/只读transport及已列出故障边界，之后继续受控Kubernetes profile/真实生产driver，不重启已验收单元。

不包含普通Pod provisioning/创建、真实kubelet/CRI、main/API生产切换、Redis移除、Backend claim admission、大N性能容量、长空闲证书维护或observer slot回收。现有正式CLI路径可用于后续生产driver；两个本地隔离fixture不证明这些剩余工作。
