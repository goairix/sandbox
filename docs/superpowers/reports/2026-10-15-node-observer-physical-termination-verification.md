# Node observer 物理终止生产者验证

状态：独立物理终止生产者已范围验收。原始完整 Task review 为 C0/I2/M1；同作者完成 R1、R2，同 reviewer 最终确认全部发现已解决，R2 无新增 C/I/M。既有物理验证缺口和生产集成限制仍保留。

## 源码与范围

BASE `ff989b805c7b6f5bb11d67cb76cd7d6fde4a98d0`。原始十个小步源码提交：`a1bc3cc`、`ea9a927`、`29500a6`、`15efe90`、`5a96a03`、`89c371c`、`a5d14a2`、`4ce4394`、`8c484ef`、`2b2e55d`。完整审查包仅包含这十个提交的35个路径，排除交错的其他 Task。

实现独立 node observer 的真实 UDS peer 登记、root-certified runtime/observer 身份、保留 pidfd/namespace/cgroup 句柄、固定原生 etcd 权限检查、单次 SIGKILL、实际死亡及原 cgroup 排空、fsync 后签名结果查询，以及独立 mTLS 和命令构建。没有实现主程序切换、生产部署、provider 清理或 owner 安全释放。

## 物理验证与证据边界

在具名隔离 Linux arm64 Docker fixture 中使用 genuine Supervisor 私有运行时 key、private PID/mount namespace、真实非特权后代、TLS 原生 etcd 与只提供精确 Pod/Node 读取的 HTTP fixture。fixture 仅操作自己的进程和新建私有 cgroup；没有真实 Kubernetes/Redis 或部署操作。

- epoch4 证明真实登记、固定59比较+1读取、原始 pidfd SIGKILL、死亡与原 cgroup 空、持久签名 proof、重复返回原 bytes。其 Close→New 只是同进程实例重开。
- epoch7 四个选中 parent case 通过，Go SKIP 为0：包含独立新 OS 进程读取原 terminal，以及 signal-issued 已持久后撤销真实 claim Lease、原 child 仍存活、同命令不重发、新 OS 进程查询仍为 unknown 的独立路径。
- epoch7 原始 Supervisor、foreign child、user probe、cold observer、etcd 共九条 Cmd.Wait 记录保留；外层原始 attach/container Wait 均0，精确容器删除和 label inventory 为空。
- Root 对 epoch7 两个 ELF、编译所选文件名及前后 SHA、23个实际选中的本 Task Go 路径与该轮冻结源码一致性进行了核对。此绑定不覆盖所有35路径；daemon 只有单独构建证据，不是已原生执行或部署。

原始失败均保留：早期 proc 重复挂载、fixture activation 窗口、实际 capability 验证 bug、持有 FD 导致卸载失败、非法 fixture history reseed，以及受限 cgroup rename EPERM。epoch4 外层 Wait 的原始证据仅在工具 session68871/chunk0a3a6d，未用后补 Wait 伪造磁盘记录。

## 完整审查与 R1

唯一 fresh reviewer 对冻结源码进行一次完整 Task 审查，发现：

1. 新受控时钟观测未用于收紧原 ticket 截止时间，旧 monotonic cutoff 尚有效时可能忽略已知过期或不确定性越界。
2. Close 保留了 producer 显式拥有的 etcd TLS 私钥和 Kubernetes 凭据副本/引用。

R1 要求每次新观测只收紧原 ticket/issuer/certificate cutoff，在 syscall/sign 前检查并保留 sign 后检查；已合法签名、之后 fsync 跨 cutoff 的历史结果规则保持。凭据修复仅清理显式拥有的 mutable material、释放配置引用，并实际等待原 signer；保留调用方输入，不声称擦除不可达 SDK 内部副本或 Go immutable strings。

凭据修复已小步提交 `ec60d6fa41139618d3f9852bf48390acb7f355a5`；时钟修复提交 `2bbcb1288516369dfb57f0d8b415bcdd5155989f`。原始 Linux GREEN 包括六个选中 unit parent case，以及有效时钟正例和三个 fresh-clock 拒绝阶段；凭据单测包括 Ed25519/RSA/ECDSA、constructor failure 保留 caller、阻塞 signer 的实际 Close join。最后一个过期边界在 UTC 中点尚未过期、750ms 不确定性已越界时拒绝签 terminal。

Root 独立核对最终 native 的5625个编译输入、daemon 的4267个输入文件名及前后 SHA 一致，三个 binary SHA 与实物一致；六个选中 R1 Go 路径匹配最终源码，cmd/main.go 确实出现在 daemon closure。最终 raw 有十二条原始 child Wait，外层 attach/container Wait 均0，精确清理完成。host 针对性 race、host/Linux vet、daemon 构建、格式与脚本检查通过；Darwin race 不覆盖 Linux-only 凭据单测。

同 reviewer 对仅含两个提交、七个路径的 R1 包确认时钟修复，同时发现 owned-secret 修复引入的共享 TLS 配置容器并发修改问题：SDK 取消请求不保证内部握手已退出，不能据此清零其仍可能读取的 Certificate/Transport 字段。

## R2 与最终审查

`b76ebb8c4650ff57622a6a7dffe283b53b934dd3` 只修改 server 和对应 Linux 单测：保持已发布容器不可变，只关闭受锁保护的自有 signer 并释放 observer 引用。浅克隆配置与 Transport public 字段的确定性 RED→GREEN、四个凭据 parent case、原有实际阻塞 Sign join/caller/三种 key 单测通过，原始外层退出1→0和精确清理均保留。Linux race 未运行；没有重跑物理终止矩阵。

Root 对 R2 的5625个 unit 编译输入和4267个 daemon 输入文件名/前后 SHA、两个修改 Go 路径、三个实际 binary SHA 和 cmd/main.go 选中情况独立核对。R2 只覆盖关闭凭据路径；物理行为来自前述具名历史源码 epoch，并不声称最终整包重新执行了全部物理矩阵。affected Linux vet、daemon 构建、格式和 diff 检查通过。

同一 reviewer 只审 R2 两路径修复包，确认原 I2 完整解决、无新增问题；I1/M1 保持解决。该轮 unit-only script 的额外编译 runner 使用空 selector，产生 `testing: warning: no tests to run`，不计覆盖率。未增加 review seat 或重建丢失的历史 Wait。后续组件可以依赖这一生产者，但其部署和未强制物理场景仍须在对应环境验证。

## 固定界限与未证明部分

原生 guard 固定60节点，每个请求/分支响应独立≤256KiB；intent/frame≤16KiB，ticket≤8KiB，proof≤4KiB。journal slots1..1024、每槽≤32KiB、两代 payload≤64MiB；unknown 继续占槽，active≤4、每槽六个持有 FD。它们是逻辑资源界限，不是整体 RSS、MVCC/WAL 或大 N 容量结果。

实际 cgroup replacement 因 fixture rename EPERM 未执行，不能记为物理成功。未强制数值 PID 复用、已有活 peer 的 PID namespace 替换或不可中断内核任务退出；claim-loss unknown 不能替代这些证明。Kubernetes fixture 不是 kubelet/真实 API rollout 验收。

原生 PASS 日志保留两类配置告警：HTTP/gRPC 同端口、simple token 未签名。显式 SIGTERM teardown 对应 `http: Server closed`、2379/2380 `use of closed network connection` 及其 secure-gRPC 停止告警。只有这些已知内容按配置/停机分类；其他错误不概括为正常。刻意触发的 observer 拒绝属于具名 negative 结果，PASS 不能称零告警。

主程序仍使用 Redis。后续须完成注册发布组合链路、etcd termination composer、精确 provider 清理与原子 owner release，再完成生产接线、分区调度、FUSE/collector、大 N 验证、部署恢复和 Redis 移除。
