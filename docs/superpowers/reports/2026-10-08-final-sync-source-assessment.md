# Final-sync trusted source assessment

日期：2026-10-08，源码基线a8ab6a4。只读调查下一步生产者入口，不是实现或物理验证；不重放已验收的 runtime gate。当前 final-sync reservation 单元只登记永久归属，不提供文件源或发送能力。

## 已命名检查

| 现有接口 | 实际观察 | 后续接线约束 |
| --- | --- | --- |
| internal/sandbox/workspace.go:942–1011 | fullSyncFromContainer 调 Runtime.DownloadDir("/workspace")，遍历 tar，Create/copy/Close，最终 error 只决定本地返回 | 新链路不能直接用这一个返回值作为 task-scoped 远端结算。文件内容/路径和 plan digest 仍需可信来源绑定。 |
| internal/runtime/docker/file.go:216–229 | DownloadDir 先 isFUSEContainer，FUSE 分支转 downloadFile；普通分支直接 daemon CopyFromContainer，参数仅 id/path | 普通分支无需新 tar 子进程，具备候选管理读取入口，但当前函数没有强制新的 task/claim/Boot/双排空证据。此次未重新审查 downloadFile 的完整调用链，不能据此认可 FUSE 分支。 |
| internal/runtime/kubernetes/file.go:364–405；exact_exec.go:10–23 | downloadDirFromPod 经 exactPodCommand 生成 tar 命令，再对 sandbox container 建 SPDY exec；exact context 存在时检查环境 SANDBOX_POD_UID，缺少时直接返回原命令 | 必須防止在 USERS 关闭后重新走普通用户 exec。UID shell guard 不是新 task 管理授权，也不证明 launcher Boot。尚未实测已关闭 launcher 下此命令的行为。 |
| internal/runtime/kubernetes/file.go:407–420 | pipeReadCloser.Close 同步 drain 到EOF并等待 Stream goroutine结束 | 不能仅取消调用ctx就推断 source worker 已退出；后续请求需明确有限生命周期及占槽。此次不是对该旧路径的修复或回归验收。 |
| internal/runtime/controlrunner/supervisor_transport.go:88–149 | 激活后TLS handshake，由私有dispatcher消费 Exec/CloseData/Quiesce；入口初始5秒deadline | 可信 source 应是独立管理 purpose，不能塞入用户 Exec 或无条件延长所有handler。实际流的期限/续权与结果需明确设计。 |
| internal/runtime/controltransport/frame.go:18–28；task_close_frame.go:111–141 | 现有事件单帧最大32768bytes；RuntimeRequest现分支没有archive管理purpose | 流式 source 需要显式协议路由、大小/次数/总字节预算与终态，不把 stdout EOF 当完整snapshot证明。 |
| cmd/sandbox-launcher/serve.go:17–63 | PID1/root校验、protected config/socket、BootstrapPID1、原管理TLS/journal，由Supervisor.Serve持有listener | 候选可信来源仍在这个原进程边界；不得借用外部用户 tar、重启launcher恢复旧能力或新增nsenter/CRI特权。 |

以上是逐项源码事实。Docker 的普通读取候选没有自动被认可为可用的生产授权路径；Kubernetes 必须增加独立受信管理读取，不能将旧 Pod exec 当成已实现的安全替代。

## 下一步设计输入

管理读取需固定 `/workspace`，绑定 exact runtime UID、当前 Boot、original task/current claim、永久 producer attempt 和已确认 USERS/DATA closed。必须在返回数据前验证当前身份，并限制活动任务的读取/缓存/队列，普通空闲 sandbox 不启动常驻源worker。

归档实现必须明确路径归一化、symlink/special-file处理、可信文件打开和完整结束摘要；不跟随用户路径进入protected credentials/journal，不把部分流或被取消的读当完整文件源。同步模式需要可信源；FUSE/none模式应走各自证明链，不能无条件读取后声称无远端写。

真实实现与验证仍未完成：受信source purpose/签票/target journal、Linux PID1下读取和迟到请求隔离、Docker/Kubernetes生产绑定、逐请求远端ledger/provider结算、最终释放。该报告不扩大父方案授权、不定义新的API完成项，也不引入新的对象布局或部署动作。
