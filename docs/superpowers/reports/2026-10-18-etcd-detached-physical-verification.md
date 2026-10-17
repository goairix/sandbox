# detached 真实 PID1 终止组件验证

源码 `1e4d836`（辅助分支 `8dd3d01`）。仅接受正式 detached 来源、原清理
writer、真实 PID1/cgroup 和生产七槽终止状态机的组合；完整 Task4A、自动消费者、
owner 释放、quota/终态、容量及生产部署均未完成。功能约70%、整体约45%、生产切换0%。

## 路径与信任边界

父测试使用正式 plain Pod builder、原 launcher BootstrapPID1、Supervisor enrollment
和 NodeObserver Serve。实际 Acquire/ClaimCreation/Dispatch 后，父按真实 birth 签来源
与 activation；运行时 ready 由原 Supervisor 签发。worker 完成受保护配置读取后，
实际降至 UID/GID1000、全部 capabilities 为零并设置 NNP，再通过原 worker Kernel。

原 registry/job/Run/Claim 经真实 close/Q、新鲜双 drain 和唯一 detached settlement
writer，调用私有生产 termination。NodeObserver 通过真实 pidfd 终止、PID1 死亡与
cgroup empty 后内部签发 terminal proof；父和 worker 分别根据实际 native intent
验证 proof。父不持有 observer terminal 签名私钥，不直接构造终止成功权限。

原七槽全部 complete。slot0 是实际 guarded no-op；HTTP 效果为 Pod DELETE 一次、
四个不同 policy DELETE、retainer PATCH 一次。provider 每次 mutation 前检查实际
持久 intent 的 UID/RV/effect/Claim；依赖 proof 的槽要求真实 terminated phase。
Kubernetes API 使用 TLS fixture，三成员 etcd 采用真实 client/peer TLS；这不是集群部署。

普通用户 probe 是真实 PID1 后代，验证 UID/GID1000、PPID1、NNP、同 cgroup 及所有
cap sets 为零；它在 ready/真实 quiescence 之前退出并由原 Cmd.Wait 回收。测试证明
observer 杀死实际 PID1，不声称杀死仍存活的用户后代。fixture 的 `f.park/startRun`
只控制原任务调度，不代表 managed admission。公开 v1 与生产实现均未修改。

## 实际 Linux 结果

显式父用例 `TestDetachedRuntimeTerminationPhysicalNative`，环境
`SANDBOX_DETACHED_RUNTIME_PHYSICAL_NATIVE=1`；原 LOCAL_NATIVE 环境不设置。
复用本机已有镜像，四个只读 ELF、私有 cgroup namespace、只读 root、network none、
NNP/default seccomp；父仅使用既有六项必要 capabilities，未使用 privileged 或拉取镜像。

| 运行 | 父/worker | 结果 |
| --- | --- | --- |
| 辅助分支 `run-89d9dc72c0fc47fda82181758f001f16` | 19.08s / 14.55s | PASS |
| 主分支 `run-b9dea8d97b6f4cd2adde565de93e1c1c` | 18.75s / 14.50s | PASS |

两场均有50次 Commit submissions、15次 provider 写、3次 termination 写、11次 guard；
Commit submissions 不是 transport dispatch 上限。实测投影55/56/59/60节点，最大请求
23539字节、回复273字节，符合64节点、请求128KiB/回复256KiB；仅属该 fixture。
issuer Certificate/Sign 各一次。原 PID1 Wait 为 SIGKILL，worker Wait 无错误，用户
probe 和三个原 etcd 子进程均完成原 Wait，Serve 关闭并 join 后检查 cgroup empty、
删除自有 cgroup 和卸载私有 mount。

两场无选择用例 SKIP/FAIL/OOM，自有容器按确切 ID 移除并确认不存在。辅助108份、
主分支109份源码 hash 及各自四个 ELF 前后稳定，均保留输入/命令/结果/日志。

## 构建、审查和失败记录

辅助 `detachedphysicalcontrolbuild04/controlvet02` 各9460输入、
`detachedphysicalstatebuild02/statevet01` 各9255输入及 monitor build2462输入通过；
主分支 `detachedmainphysicalcontrolbuild01/statebuild01/monitorbuild01` 分别
9461/9256/2463输入通过。每轮完整17项 Go 输入、工具、源码归档及前后指纹审计通过，
共用构建/依赖缓存，未重复归档工具链或模块缓存。记录位于辅助 D 工作树
`.superpowers/sdd/2026-10-18-etcd-detached-runtime-native`。

主分支 ELF SHA256：

- parent：`152b1f03ec4cde7baeb344d77125abc5a3c3bef17e4413b4458fb1229407ce76`
- state：`9bb0037696aaf612f76926ba17037111b4b4ea3b610cf50e89b6d981179e88d7`
- monitor：`e602a3ec65d9f926ce82be098b8d1c14b51505c1e447f6061d8e332ab1a72a07`
- etcd：`9076ea9d5662fdade520a63aa7e312fcde5a4fb7d50a5515d37896204f1c0f15`

唯一审查员完整检查三文件、直接调用者和实际记录，Spec/Quality PASS；主分支三个
Go blob 与已审辅助分支完全相同，并经同审查员限定确认整合。注释覆盖身份、签名、
调度与回收边界；物理父 fixture 留 controlrunner，私有状态入口测试留 etcd，无反向依赖。

首次 `run-fbaafdd8800b482787835625afdd22a7` 因 setup JSON 省略 RestoreEpoch 拒绝；
只在测试从 Binding 恢复该字段，New 仍独立检查真实永久 epoch。第二次
`run-7431544fb27c498c9b5e8381cebf062b` 因普通 probe 继承 CapInh=0xe0 被零 cap 断言
拒绝；测试沿用既有 archiveuser 初始化，在任何 probe IO 前清所有线程 cap，再保留
完整零值断言。两场日志、原进程 Wait、稳定输入和容器清理记录均保留；这些是测试
初始化失败，不作为产品 RED。没有放宽原权限校验、wire 上限或 quiescence 条件。

接续 owner 的独立 v2 seal、删除后仍存历史的认证 reader 及 Unknown 仲裁；再接付费
quota/终态消费者和全局 phase 内存准入。最大输入、32物理并发、大量已有 sandbox、
故障恢复、HTTP/FUSE、部署与 Redis 移除仍须实现或验收。
