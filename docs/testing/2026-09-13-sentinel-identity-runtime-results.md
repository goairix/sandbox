# Sentinel 身份核验运行时阶段验收

本文保留2026-09-13身份核验阶段的历史证据。随后补齐的启动、初始化和 Chart 接线及最新验收范围见 [2026-09-14完整启动验收](2026-09-14-built-in-sentinel-production-startup-results.md)，以下“尚缺”不代表最新实现状态。

## 状态与边界

本阶段完成本地 PVC 检查、真实持久配置解释、三成员独立密钥、随机挑战签名、固定端点 HTTP 传输、namespace 身份登记 CAS、本地认证 INFO 和可执行 attestor。API 镜像 Dockerfile 额外包含 `/app/redis-bootstrap`，默认 API 入口不变。镜像未构建或推送，线上未升级；默认 Chart 仍是 standalone，**内置 Sentinel 尚未达到部署完成条件**。

尚缺完整可中断配置事务、最终 exec 启动/恢复包装器、真实拓扑与副本确认后 Initialized CAS、Chart/API/drain 一致接入，以及完整故障矩阵。没有以身份证明代替角色/复制检查，或以单成员实验代替三节点 HA 验收。

## 两级复审

以下模块均完成独立规格复审后独立质量复审；最终无 Critical/Important 遗留：持久解析器（含默认 ACL 重写修正）、PVC 预约/同步确认、挑战及传输、成员密钥、namespace 登记、本地 INFO observer、HTTP server、attestor 命令、纯初始配置生成器。夹具清理与过程验证的独立质量复审也通过；没有把该单成员实验当 peer-auth、ACK、磁盘持久性或 HA 验收。

## 真实隔离实验

入口：`bash scripts/test-redis-bootstrap-rewrite.sh`。只使用缓存 `redis:7-alpine`，编译当前源代码的 Linux 测试程序和真实 attestor 命令；不构建、拉取或推送镜像。一个独立命名/标签的临时容器，network none，无宿主端口，`/data` 为 64 MiB tmpfs；不创建卷或网络，不引用线上凭据。实验密码为公开测试 token。

- 初始生成配置实际启动 Redis/Sentinel；正确独立密码 PING 成功，无密码和交叉密码明确返回 NOAUTH/WRONGPASS。
- 实际 CONFIG REWRITE/FLUSHCONFIG 曾被解析器误拒绝：Redis 会自动补入默认 ACL。集成和单元回归均观察 RED，再实现仅接受同文件 requirepass SHA256 匹配的受保护 default 用户后 GREEN。额外用户、免密、selector、重复配置和外部 ACL 文件仍拒绝。
- 实际重写还将配置改为 0644。实验启动设置私有 umask 0077，保持 0600，使真实 PVC reader/observer 通过；**生产最终 exec 包装器必须同样设置，未通过放宽文件权限绕过**。
- Redis 停机时实际 attestor 命令能经新随机挑战签出 Configured PVC inventory；Redis 冷重启后签名绑定新的 run_id，核验端独立连接固定 DNS 认证 INFO，再验证公钥/marker/nonce/run_id。
- myid、持久角色和 epoch 在本次冷重启保持不变；真实 attestor 响应 SIGTERM 并正常退出。
- 最新实验 PASS（0.49s）。一次隔离空载采样 VmRSS=22840 kB，不是峰值、并发资源上界或 Kubernetes 生产压测。
- 每次成功及失败实验结束均确认自身容器为零，删除精确两个临时测试程序及其目录；没有卷/网络需要清理，未删除业务资源或缓存镜像。

本实验只有一个配置成员、两个 Redis/Sentinel 进程及一个 attestor；其它 DNS 使用文档保留地址，**没有证明复制、自动切换、同名换卷、全组冷恢复、网络分区或生产 HA**。namespace CAS 的当前证据是显式 UID/resourceVersion 的 fake API reactors，不是实际 Kubernetes RBAC/CAS。

## 主 agent 新鲜验证

- `go test ./... -count=1`：PASS；redisbootstrap 19.298s，所有包命令退出0。默认需要外部环境的 integration skip 不作为在线测试通过。
- `go test -race ./internal/redisbootstrap ./cmd/redis-bootstrap -count=1`：PASS，分别22.303s、8.105s。
- `go vet ./...`：退出0。
- `golangci-lint run ./internal/redisbootstrap/... ./cmd/redis-bootstrap/...`：0 issues。
- `bash scripts/test-helm-chart.sh`：PASS；默认 Chart lint 0 failed。
- `git diff --check`：退出0；提交前加入新文件后再检查 staged diff。

以上仅对应本阶段源代码，不宣称 AppArmor 真内核验收完成，也不提供生产 Sentinel 部署许可。

## 必须保留的安全门槛

Reserved→Configured 必须保留同 marker，并在配置/身份/目录同步成功后才启动 Redis；安装后 fsync 失败不能通过 reread 变成成功。已登记空卷 replacement 和密钥变化必须阻断，不能重复 seed。恢复只允许一致的既有主角色；高 epoch 少数状态、可达但无可信证明的成员和角色/monitor 不一致不能被无条件忽略后恢复旧主。HTTP 错误必须分辨真正端点不可达与可达却无法核验，避免掩盖较新状态。

稳态选主仍只属于 Sentinel；初始化只在三成员真实角色/复制/Sentinel 状态及同连接有实际写入的 WAIT 满足后 CAS 放行，不能自动降级 ACK。AppArmor 的最终 enforce 测试需要明确非生产节点加载授权和当前构建镜像，不能用单独 namespace 宣称隔离节点内核。
