# etcd 普通运行时执行 service

这是已批准 Redis→etcd 总方案的下一个业务消费单元。前置 Lookup 已验收 `436bef88405406036fb5c2cb159edd98bc70e151`；不重做旧单元，不访问已关闭的 SDD workspace/pair。当前普通 HTTP/main 仍依赖 Redis，本任务不把 service 实现称为生产切换。

## 目标

让同一部署的普通 service 真正经过现有 `BeginOperation → PrepareExecEffect → DeliverExecEffect → original handle.Wait/Close` 执行用户指令，并让 mounted deployment 提供实际可用的固定 start/renew signer。不能绕开可信 launcher、把 Lookup DTO 当权限、直接 Pod exec 用户 shell，或仅交付无人消费的 signer。

## 外部边界

- 新执行入口接收 sandbox ID、调用方稳定 request ID、已经完整解析的 `controlprotocol.ExecutionRequest` 和 stdout/stderr writer。具体 Go 名称/返回 DTO 经源代码预检冻结；保留 exact operation/effect reference 供不确定结果定位，不自动重发或创建新命令。
- 本层不猜测镜像默认环境，不继承管理进程环境，不把字符串 shell/HTTP 默认值映射混进底层 descriptor；后续 HTTP adapter 再明确解析旧业务参数。UID/GID 必须匹配固定 launcher 配置；network/TTY/workdir/timeout/输入大小遵循原 descriptor 和目标约束，非法值在危险写入/下发前拒绝。
- stdout/stderr 流由调用方同步 writer 消费，沿原 handle 有界通道背压，不累计全部输出、不用后台协程伪装任意 writer 可取消。公开说明 writer 必须返回；Close 不把尚未退出的 writer/物理 IO 当作已 join。

## 权限和结果

先验证当前 sandbox 属于同一固定部署/profile，并从原 coherent view/history 获取目标；实际执行必须重新获取原 Backend operation capability，核对其当前 control 与目标一致。旧历史记录、相同 hash、健康查询成功或 copied capability 都不授予执行权限。保留现有 Backend 每次下发前的全部 fencing；不放宽旧公开方法。

复用既有 fixed Destination/原 Kubernetes bridge，只进入原受保护入口。请求只有一个原 capability、prepared effect 和 delivery handle；不确定发送/应答、续期丢失和取消不回退 Redis、不 replay start、不以 reconstructed reference 恢复私有能力。

正常结果必须来自原 handle 验证的 `local_terminal`、精确命令归属、实际 root wait status 和 drain 标量；返回原 receipt/ref 及其明确的 local-terminal 语义。签名 local receipt 不是业务 End 或远端写入终态，本任务不新增假的 business End/历史 GC，也不把撤销 operation Lease 当目标已排空。不确定结果保留原 reference 并返回明确 unknown；无证据不返回 exit0/成功。

## 生命周期和预算

- 同一 service 的 MaxActive 覆盖 Create/Lookup/Exec 总和，不新增 N 常驻对象或 goroutine。只允许按实际已准入执行数分配有限续期/reader owner。
- 旧 Create/Lookup 的 20 秒期限保持。Exec 使用原 descriptor 的 1..3600 秒执行期限及明确有限清理余量，不能被旧 enter 的 20 秒误截断。
- 执行期间沿原 `RenewExecDelivery` 同时续原 operation Lease 和原目标命令；不另建 Lease/重复续原 operation、不延长原业务/descriptor 绝对期限。单个续期任务不得重叠；错误使当前执行失权，不乐观忽略。
- context cancel/Close 取消原调用、停止并 join 原续期任务，关闭/join 原 handle/bridge。撤原 Lease 仅做 metadata cleanup；失败/unknown 不宣称目标已停止。最终成功也须经过原 service/context 关闭检查。终态与续期竞态不能把确定成功错误扩权，也不能把真实续期失败转换成功。

## Mounted signer

复用已有 mounted issuer 私钥/证书和固定 binding，提供 concurrency-safe、context-honoring 的 Certificate/SignStart/SignRenew，仅签对应固定 purpose 与 Backend 选择的精确证书摘要。构造时验证 key/cert 对应且自持 owned copy，关闭必须等使用者完成后清理密钥；Backend 不持私钥。部署构造失败完整回收。`Deployment.Open` 保持只读：注册 issuer 仍由原实际 Prepare 的 lazy RegisterExecIssuer 执行，不在 startup check 写 metadata。不得新增网络 key 服务、环境变量私钥或把密钥放公共 etcd。

## 验证与完成

先对新入口/签名依赖做最小明确 RED，产品完成后 targeted host/race、相关 Linux vet 和一次固定真实 native。native 必须实际 service 创建/加载原 sandbox、原 Backend gate、实际 mounted signer 或与其同一构造路径、正式 launcher、TLS etcd、原 bridge，观察用户输出/非零退出及原 Wait/IO joins；包含执行跨原 20 秒期限并真实续期、错误/取消/Close 或 gate 变化下的拒绝/unknown。不得用假 session、伪 receipt 或 sleep 代替执行已开始证据。

Root 继续固定 Go1.25.6、apply_patch 唯一源码编辑、精确小步 Git；不碰 Sentinel。复用外部完整 module cache 避免重新下载。只对新源集合做匹配 action/platform/race 的实际来源捕获和必要审计，已验收旧矩阵不重放。Docker 每次具体命令 Root 预检，原始 Wait/IO join、有限隔离及精确清理。唯一 fresh task reviewer，修复沿同 pair；验收后连续推进 HTTP 全操作 gate、分区/FUSE/容量/部署/Redis 移除。
