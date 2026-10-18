# 现有 cache 容量与即时 Status 账目验证

源码 `f32f5bd`（辅助 `2bf315d`），修复 worker cache 变化后、下次 actor 发布前，
`Status()` 仍返回旧 OwnedBytes/Queued 的问题。Status 在原 lifetime 锁内读取当前
cache 账；其它 phase/partition/error 字段保持原发布语义。

三个原产品文件共用现有容量公式：10000 keys/40MiB动态额度，automatic扣369 keys/
4MiB，cold再扣64 keys/4MiB；固定预留24MiB。初始化以prospective模式检查，成功
才安装原flags；put/replace、snapshot、publishStatus及即时Status共式。先检查
charge及减法操作数再算净额，避免回绕后错误准入；非法内部总量的诊断加法饱和，
不能显示成小值。不增加限额、token、执行路径或公开接口。

单个新回归通过原metadata fixture、合法due记录和原cache入口驱动：先发布一次
状态，再put/替换/settled但不再publish，Status应即时与cache.snapshot一致且phase
不变。原实现于第一put后失败，期望`0x1c00871`，实际仍`0x1c00000`；修复后完整
通过。RED `duecacheaccountingred01` exit1/9156输入，GREEN
`duecacheaccountinggreen01` exit0/9156；测试保持原样。

定向race八项通过（9161输入），包含原四个cache测试、automatic admission/bounds、
cold cache reservation和新回归；原10001 key拒绝/单page/expiry、369/64身份及4MiB
预留保持旧边界。这些是受控metadata回归，不是大量真实sandbox的性能或容量验收。
Linux vet（9265）通过；逐轮完整17项Go输入、源码/工具归档及前后指纹审计通过。
唯一审查员完整四文件、实际RED/GREEN/race/vet及冻结输入后限定Spec/Quality PASS，
注释、分段和既有cache/worker包职责均通过。

主分支 `duemaincacheaccounting01` 新Status及四项终态整合测试通过（9160输入），
raw1758字节，SHA256
`c65417113b4a35f6bfe85b685b1230ffe1a21cacd767f2a681a5c3b1ac355662`；完整证据
审计通过，四文件与辅助接受版本一致。证据位于B/D工作树相应本日SDD目录。

此片只关闭现有cache公式与状态报告问题。job token、重图phase的分配前准入和
真实join后退款仍未实现；不能据此宣称物理任务并发图、完整64MiB或RSS已经验收。
