# Pending 真实三成员验证：首切片

本片只验证真实 etcd 元数据发现与恢复，不提供 Kernel、物理 job 或 queued 权限，
也不构成整个 pending Task 或大 N 容量验收。唯一新增测试文件为
`internal/storage/state/etcd/due_worker_pending_real_linux_test.go`。

## 实际覆盖

同一个真实3成员 etcd3.6.14 cell，四场独立 namespace 串行执行：

1. 原创建/签名发布/PrepareDestroy/CommitStage 产生真实 Task/P；80条原始坏P仅为
   不可信负向输入。固定R两页拒绝80条并发现可信尾项的原出生与摘要，未创建物理job。
2. 真实 Compact(R+1) 导致续页原RPC返回 ErrCompacted，actor清空读取状态并退避；
   原session仍有效。已消费cursor和失权session不能发送新RPC，cursor也不能转交新session。
3. 包装器仅扣留原成功 CommitStage 的一次真实ACK；Task/intent/P/receipt同出生。
   原Backend/session关闭后，新Backend及新capability/cache从P发现原Task，不显式提交引用。
4. 原串行loop在单Range份额和正常earned credit下交错pending与due；实际lane顺序
   `[pending due pending pending]`，仅拒绝160/400坏P时due已成功读取，未手填token/时钟。

## 命令与结果

- 精确入口 `SANDBOX_PENDING_REAL_NATIVE=1 /fixture/runner -test.v -test.count=1
  -test.timeout=100s -test.run=^TestDueWorkerPendingRealNative$`；测试内部90s总deadline，
  各场15s及原启动20s上限不变。
- 最终 Linux arm64 build-02/vet-02 均exit0，9165完整输入；各轮后独立核对17类输入、
  前后集合/hash、source/tool每个归档成员及当前字节，下一Go/修改前完成审计。
- runner SHA256 `8b5c2a609ce4f513a7063bfa79826511837a6b748f8e158f627d196160af2b61`；
  etcd ELF SHA256 `9076ea9d5662fdade520a63aa7e312fcde5a4fb7d50a5515d37896204f1c0f15`。
- `pending-real-native-03` 四场PASS，9.30s；实际attach/container exit0，无OOM、SKIP
  或race。原始日志36770B，SHA256
  `5d58606d71e163f478cce3c56dcc1f740746172ea62b6d0f34f9272d5b38c243`。
- Root独立核对实际3个distinct member、同cluster、命令、前后镜像/limits/caps/只读
  ELF及hash、完整退出结果和精确清理清单；3个etcd原Cmd.Wait均实际join，容器已移除。
- 容器固定network none、只读root、private cgroupns、1GiB/2CPU/128PID、512MiB tmpfs，
  drop ALL capabilities、NNP与默认seccomp；仅两个只读ELF bind，没有Kernel权限。

## 失败及边界

- native-01 在业务前失败：Root使用单成员的128MiB临时盘，三个WAL报no space。
  只修测试容器磁盘/内存预算，没有改业务限额、产品、断言或测试超时。
- native-02 真实集群启动后，新fixture漏配EnrolledPublication，原发布helper解引用nil。
  仅补齐原due native相同配置；旧helper及所有断言不变，随后重新构建并真实执行。
- 两次失败的命令、hash、日志、退出及精确清理记录均保留。取消/NotFound/客户端关闭
  告警完整保留，包括原helper清理已关闭Backend时的有界重试，未静音或裁剪日志。
- 未重跑已验收矩阵；新测试在原etcd事务包内复用私有协议与原受控进程helper，未新增
  产品API。人工检查注释、分段及职责，gofmt-d为空。
- 持久32槽与第33项安全释放恢复仍开放。唯一S删除需要原registered job、真实Joined
  Run及原签名terminal/release证据；不存在合法无Run捷径，不能用rawDelete或内存32队列
  冒充。同一原审查员已认可首批声明的元数据范围、无新增可行动问题；I3的完整恢复、
  负载去重/物理进展/续租公平性及持久32/33/实际Close仍开放，未整体关闭。
