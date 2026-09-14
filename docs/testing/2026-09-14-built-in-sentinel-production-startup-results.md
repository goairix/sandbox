# 内置 Sentinel 完整启动验收（2026-09-14）

## 完成范围

已补齐：可中断 Reserved→Configured 事务、本 Pod 权限/独立密钥准备、Redis/Sentinel 最终 exec 包装器、固定成员真实拓扑和同连接写入 ACK、Initialized 配对 CAS、API 初始化门禁，以及三成员 StatefulSet、普通初始化 Job、最小 RBAC、标准 NetworkPolicy 和中文部署配置。

Sentinel 仍是唯一稳态选主组件。身份核验不代理业务，启动包装器不晋升副本；无凭据回退、ACK 降级、自动清空 PVC 或重新生成已有身份。默认 standalone 和外部 Redis 保持兼容。

## 真实三成员测试

入口：`bash scripts/test-built-in-redis-sentinel.sh`。仅使用缓存 Redis 7 Alpine，编译当前 Linux 测试及命令程序；三个独立容器/网络命名空间、三个独立命名卷、唯一 owner 标签的 internal 网络，无宿主端口、不接业务。运行真实 UID/GID999 attestor、Redis、Sentinel 和 prepare/exec 命令。测试 token 非线上凭据。

两轮完整通过：三成员矩阵分别63.09s、55.90s；随后实际换 IP 恢复分别5.91s、6.83s。每轮均验证并清理到本轮容器、卷、网络为零。

- 全新卷预约、三个随机挑战签名、登记、私密配置落盘、实际 exec、复制及写入 ACK，最后才更新 Initialized。
- 初始化在登记后实际取消；使用原 marker 续跑，不提前开门、不重置登记。
- 无密码和交叉密码均拒绝；数据、Sentinel 独立密码分别认证成功。
- 原主成员的三个进程全部停止；实际 Sentinel 选出非零主节点，已复制键保留。Initialized 核验容忍一个真正不可达成员。
- 原主包装器恢复为从节点，不调用晋升命令；Redis 单进程重启保留角色和认证。
- 全组三类进程停止后冷恢复；逐个 Redis 认证 INFO/GET 确认保留原非零主、两个从和原键。namespace marker 和 resourceVersion 不变。
- 少数成员实际保留 epoch900，并取得新签名确认负例前提；较旧多数不能覆盖该可达状态。
- 同 DNS 的实际保留卷数据移出至本轮隔离备份，原卷成为真正空卷：已有登记阻止重新 seed；两份状态缺失同样阻断。仅恢复本轮故障夹具，不操作业务卷。
- 两个数据从节点不可用时，复制/ACK 核验明确拒绝。
- 全部成员的 Docker 网络 IP 实际变化，但 DNS、卷、密钥不变；实际恢复原非零角色、每个节点原键、复制及写入 ACK，namespace 身份不重置。

夹具使用生产默认10s down-after、60s failover timeout。较早的1s实验探测阈值会在故意中断的初始化期间过早切换，已修正夹具，而不是放宽生产角色或证明检查。Docker 停止端点可能返回“invalid IP”，地址现于运行时获取并验证后保留。

## 真实 Linux 权限和配置重写

入口：`bash scripts/test-redis-bootstrap-rewrite.sh`。单独 network-none 缓存容器，无卷/网络/宿主端口；运行真实 Redis CONFIG REWRITE、Sentinel FLUSHCONFIG、认证冷恢复和 root→UID999 文件准备。

最新重写测试0.48s通过。实际验证 PVC 根999/0700、自身 seed999/0400、固定工具root/0555；覆盖七个安装中断点、已验证双链接续跑、部分 final 拒绝、未知 UID/文件/替换 inode 保留。配置重写保留0600，未通过放宽 reader 权限绕过。一次空载 attestor VmRSS=26532kB，仅是采样，不是峰值或生产资源保证。清理确认零本轮容器。

## 复审与提交前回归

所有模块均完成独立 SPEC→QUALITY 复审；真实夹具也分别通过规格和质量复审。最终接口复审额外修正：API startupProbe 覆盖门禁和运行时余量；内置 Sentinel pre-rollback 静态拒绝历史 Pending manifest 重放，standalone/external 原行为保留。上述修正均先观察行为 RED 后验证 GREEN。

回归命令：

```sh
go test -p 2 ./... -count=1
go test -p 2 -race ./internal/redisbootstrap ./cmd/redis-bootstrap ./cmd/sandbox ./internal/config -count=1
go vet ./...
golangci-lint run ./internal/redisbootstrap/... ./cmd/redis-bootstrap/... ./cmd/sandbox/... ./internal/config/...
bash scripts/test-helm-chart.sh
bash scripts/test-helm-backend-switch.sh
bash scripts/test-helm-sentinel-auth.sh
bash scripts/test-helm-apparmor-loader.sh
git diff --check
```

最后一轮完整输出已读取：全仓测试退出0（redisbootstrap51.141s）；相关四包race全部通过，分别57.239s、4.331s、2.299s、2.209s；vet退出0、lint0issues；四个Helm脚本和结构化测试全部通过（结构测试1.042s），diff-check退出0。最终接口复审PASS，无Critical/Important；最后Chart探针/回滚delta亦独立SPEC/QUALITY PASS。`go run ./cmd/redis-bootstrap deny-rollback` 实际输出静态指导并退出1，不依赖集群。测试程序主动跳过的外部环境项不计为在线通过。

## 尚未代表的验收

本轮三成员 Redis、TCP/HTTP、身份签名、进程和卷数据操作是真实的；namespace ConfigMap API 由显式 UID/resourceVersion 的 typed fake reactor 仿真。尚未证明实际 Kubernetes RBAC/CAS、CSI、Secret subPath 隔离、CNI 策略执行、网络分区或生产节点故障能力；结构化 Helm 测试不替代这些在线验证。全仓默认跳过的外部集成也不作为在线通过。

可选 AppArmor 加载器代码、门禁和 Helm 回归可用，但随附 profile 仍须在获准的非生产 Linux 节点验证实际 enforce、FUSE/s3fs 挂载写入/flush/卸载及拒绝规则；本轮未加载业务节点内核策略。验收前保持加载器关闭，不能使用 kind 缺失 LSM 豁免冒充生产防护。

用户统一构建当前 sandbox-api 后，按 [内置 Sentinel 部署说明](../deployment/built-in-redis-sentinel.md) 安装新 Chart；AppArmor 启用另需构建加载器镜像并完成节点验收。本轮未构建/推送发布镜像，未操作线上、未回滚、未创建 worktree。
