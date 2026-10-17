# 独立 cleanup worker 生命周期观察切片

`LocalCleanupDeployment.Done` 暴露原worker实际退出信号；`Wait(ctx)` 等待该信号后读取
原终态错误。取消Wait只结束观察，不停止worker或关闭依赖；调用者仍须Close。
未启动或nil部署不伪造已完成信号。接口不表示Task销毁成功或持久槽已归还。

新增两组测试覆盖nil/未启动部署和已取消观察；同时回归原严格manifest、边界和拒绝
测试，合计五组40叶例。host通过1.763s，race通过2.365s，无告警、跳过或数据竞争；
Linux arm64/CGO0包vet通过。缺接口的编译RED单独保留，未当作执行断言。
host/race/vet分别9047/9052/9136份实际输入，17类源码字段、源码与工具前后及归档
成员均核对，race输入列表和测试命令均带-race。

日志SHA256：host `1d8acf38c65b031d31ac2c68b18e2889a35605027544c7b4002b7f34eb83e67a`；
race `79e27730e6c7e1e914fe4658768d96c9fb16d2eaa69ad0da4954ac587bada652`。

Root完读两个新增文件，gofmt差异为空，并检查GoDoc、逻辑分段和包职责。该接口只观察
已有生命周期，不创建执行权限。没有执行容器、真实签名或物理清理；正向Wait终态、
独立进程信号/异常退出和实际关闭组合尚待后续验证。独立自动worker入口、v2配置及
调度接线仍在实现；本片不计为该完整功能、FUSE迁移、容量或生产切换完成。
