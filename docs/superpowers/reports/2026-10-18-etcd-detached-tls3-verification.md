# Detached NodeObserver 三成员 TLS fixture 验证

代码 `5f540a2`（辅助分支 `0943753`），2026-10-18。此片只准备真实物理终止
链路需要的存储 fixture，没有创建 PID1、authority carrier 或 termination proof。

原单成员入口保留名称、数据路径、client TLS、HTTP peer 和 observer 只读角色。
显式三成员入口先启动全部进程，再检查就绪；client 和 peer 均使用证书认证。
每个成功启动的原 Cmd 立即登记清理，部分启动失败也会实际 Signal/Wait。

`TestNodeObserverTLS3NativeFixture` 在 Linux/arm64、CGO=0、etcd3.6.14 的自有
容器中实际 PASS，4.95秒，记录 `run-469b840da84e47de99acf8cfb8f7f28e`：

- MemberList 与各 endpoint Status 核对三个独立 member 和相同 cluster。
- 固定连接分别向三个成员写入；三个独立 observer 连接均线性读到每次实际 ACK
  的值和 ModRevision，共九次跨成员读取。
- 每个 observer 连接拒绝 Put 和前缀外 Get；最终记录未被越权写入覆盖。
- 三个原 etcd Cmd 均实际 Wait，日志分别记录退出；无 SKIP/FAIL/OOM。

容器使用原已存在镜像，非 root、capabilities=0、NNP、只读根、无外网、私有
cgroup namespace；仅挂载两个只读 ELF，临时文件位于有界 tmpfs。未拉取镜像。
精确自有容器已删除，42项源码和两个 ELF 前后指纹稳定。测试 ELF SHA-256：
`6c6c285672479db4b2b57b3df1098c9fbe4085c75aa33c95ace5ef6a70241fb9`。

`detachedtls3build01`、`detachedtls3vet01` 各9458项输入，命令及完整输入、工具、
源码前后指纹审计通过，复用原共享缓存。唯一 reviewer 对两个测试文件限定
Spec/Quality PASS，核对注释、排版、包职责及单 endpoint resolver。

原旧物理测试镜像 `684aa844…` 已不存在；metadata 镜像仍可用。下一片恢复真实
PID1/observer 资产并验证七槽终止，不能把本 fixture 通过计为物理证明或 Task4A
完成。消费者、owner/terminal/quota 接线、容量与生产切换仍开放。

原始命令、输入、日志及独立审查记录保留于 D 工作树
`.superpowers/sdd/2026-10-18-etcd-detached-runtime-native/`。
