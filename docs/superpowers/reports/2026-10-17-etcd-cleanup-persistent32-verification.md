# Cleanup 全 cell 32 槽跨 Backend 仲裁验证

本片仅新增 `TestCleanupPersistent32RealTasks`，没有修改产品代码或既有测试。一个 cell 内通过原 Acquire、creation Claim、dispatch、Bind、mount、Publish、PrepareDestroy、Commit 流程产生33条独立Task，再由四个Backend、四个真实session、不重叠partition和原100份Claim预算争用32个持久槽。预算使用真实流逝时间，不回拨bucket或修改token；每Backend只有一个调度goroutine，每个native owner至多两次RPC，实际finish后才开始下一轮。

四方准备barrier后并发占满32槽，E/S均由原initialize/reserve/rotate协议写入。第33条任务遍历32个候选后保持原selecting记录，原64条E/S完整内容与版本保持。成功reserve响应后丢ACK的任务只读取原候选恢复；同Task旧选择不能再次reserve或rotate。最后明确执行一次同字节S删除重建的ABA破坏实验，旧occupation不能接受新版本；实际释放全部session后，持久记录仍保留且Lease=0，旧owner不能继续写。

这验证全cell永久槽仲裁；测试没有调用TaskClaim Grant，因此路径零Grant不等于验证了managed第33次Grant gate。普通签名runtime元数据不是enrolled物理证明，本片没有启动32个Pod，也没有新增Kernel、inventory或权限DTO。公共mailbox的32注册/第33拒绝及Close实际等待仍须单独验证。已检查注释、并发barrier与失败join、逻辑分段、包/文件职责及gofmt/diff-check。

## 验证结果

- `capacity-host-01`：用例通过8.30s、包9.625s，9018输入，日志1084B，SHA256 `6127bce503f0c42ce279953bec6ec8bccc17e7c3a1ac319456fa51e71a8f3939`。四Backend收费turn分别116/88/74/110，实际Txn170/128/107/161，冲突36/24/17/35，实际成功写后扣ACK一次。
- `capacity-race-01`：用例通过10.78s、包13.205s，无数据竞争，9023输入，日志1081B，SHA256 `7ad55751658623a6a0673992ff509f76a758b82607eb0e68f9978d614a99f320`。四Backend收费turn180/58/74/46，实际Txn266/83/107/65，冲突68/9/17/3，实际成功写后扣ACK一次。调度顺序不同而不变量保持；两轮均无WARN/FAIL/SKIP。
- Linux arm64/CGO0 vet通过，9109输入。每个Go epoch全部17类输入、源码/工具、前后及当前SHA、每个归档成员，在后续Go或源码修改前独立核对通过。产品未修改，没有重复已验收的52组恢复矩阵。
- 所有四个goroutine在成功或失败时均cancel、解除barrier并实际Wait；创建Claim、Stage、session、Backend/client使用原释放和Close路径。session建立后Grant和TaskClaim写计数均0，仅作为本reserve路径观测。
- capacity-01真实三节点退出均0，无OOM；容器和专用网络删除、空清单、实际Compose/工具归档/生产者、全部host测试环境独立复核通过。成员WARN19/17/19，ERROR各4条关闭监听，日志保留。

## 剩余范围

公共32/33请求队列和关闭等待、部署loader、尚不支持的恢复阶段与完整Task审查仍未完成。本片不代表10k/100k/1m存量沙盒容量、32个物理任务并发、生产切换或Redis移除验收。
