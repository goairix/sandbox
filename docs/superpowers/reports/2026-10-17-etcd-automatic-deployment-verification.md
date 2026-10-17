# Mounted v2 自动 cleanup 入口切片验证

新增 `OpenAutomaticLocalCleanup` 只接受 v2 且显式
`worker.automatic_cleanup=true`；原 `OpenLocalCleanup` 固定 v1，提供该字段即拒绝。
版本决定原 DueWorker 的真实 `AutomaticCleanup` 选项，不接受调用者另传模式开关。
严格解析继续拒绝重复、未知、大小写别名、null 和缺失字段；只允许这个精确的兼容
bool 指针字段在 v1 缺省，其余 required-field 检查不放宽。原挂载凭证和关闭顺序复用。

真实 actor 合入后，六组配置、边界及拒绝测试通过，包含51个子测试；host 1.412s、
race 2.617s，均无 FAIL/SKIP/WARN 或数据竞争。Linux arm64/CGO0 deployment 包 vet
通过。host/race/vet 分别9065/9070/9154份实际输入；每轮17类源码字段、前后输入与
工具清单及全部源码/工具归档成员均核对，race 列表和执行均启用 -race。

host 日志7003字节，SHA256 `a3557cc0813612781c1ff3e91a6e5f932c99f49e08b0235616b84e385db352fa`；
race 日志7003字节，SHA256 `634c9d55bfa892b17927e1261a9f86a7bcdbfb5161a20fa14112dda418f487a8`。
严格 v2 合法配置被原 v1 parser 拒绝的行为 RED、随后新入口缺失的编译 RED 均保留。

本片只验证严格配置、入口版本约束、拒绝行为和真实选项接线的编译检查；host 测试
没有成功构造实际 KernelBoundary、连接原生 etcd 或执行物理清理。自动发现物理链、
独立进程、P 的 ABA/ACK 丢失及完整 Task 审查仍待 native 组合，不计作已经通过。
Root 已完读这三个文件；注释说明版本兼容、必填边界和借用生命周期，部署仍只负责
挂载配置组合，不扩展存储权限、不生成身份，不切换默认后端。
