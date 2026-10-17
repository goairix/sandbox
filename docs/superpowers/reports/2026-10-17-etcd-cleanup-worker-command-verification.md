# 独立 cleanup worker 命令构建与控制流程验证

新增 `sandbox-cleanup-worker serve MANIFEST`。Linux 启动真实
`BootstrapWorkspaceSyncWorker`，再打开严格 mounted v2 自动部署；其它系统拒绝启动。
命令不接收 Task 引用、不生成凭证或登记身份。SIGINT/SIGTERM 先触发既有关闭路径并
实际等待 Close，原 worker 异常退出即失败；即使异常退出没有终态错误也不能报成功。
只有请求关闭且实际 join 后全部错误叶均为原 `context.Canceled` 时才能接受取消；
任何 unknown、deadline 或其它依赖错误均保留失败。stderr 的命令诊断为固定文本，
不打印参数、路径、凭证或原始错误；依赖库自身仍可能输出原有 RPC 告警。

真实 actor 合入后，新命令六组控制测试通过，含17个子测试：host 1.250s、race
2.347s，无 FAIL/SKIP/WARN 或数据竞争。控制替身只验证退出和 Close 顺序，不提供
KernelBoundary、会话、签名或物理执行证据。取消与 unknown 混合、其它关闭错误、
异常空错误退出、参数拒绝及诊断脱敏均覆盖；失败断言也释放并 join 测试 goroutine。

Linux arm64/CGO0 命令包 vet 和生产 ELF 构建通过。host/race/vet/product 分别
8985/8990/9127/9127份实际输入，各轮17类源码字段、前后源码/工具及全部归档成员
均核对；race 的列表与执行两侧均启用 -race。
host 日志 SHA256 `14d85991e151815714f46bea0e0bc7a95e792ed0aa1960300d4ffa36d5e1c07d`；
race 日志 SHA256 `91d4b3246fe7415e0b9cac69edb2e86ceb467a21f6966dc138255b98b11229bb`。
实际产物为62,752,552字节静态 AArch64 ELF，SHA256
`031592f65b17d882f71004cc487cbb14b330247d23852c89fb2d6123482b2bb8`。

Root 已完读六个命令文件。命令只组合已有 launcher 和 deployment，生命周期接口
保持私有控制边界，没有导出任意回调。真实进程初始化、原生自动清理、SIGTERM 实际
退出、session fence 失败退出及原生 P 故障仍待 Root 运行新 native 组合；本片没有
运行该 ELF，不将编译或控制测试算作物理验收、容量证明或默认后端切换。
