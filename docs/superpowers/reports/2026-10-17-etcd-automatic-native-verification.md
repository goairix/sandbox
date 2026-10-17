# 自动 cleanup native 组合验证

Root 执行并独立审计四场 native，均 PASS；attach/容器实际退出均0，无 OOM、超时或
SKIP，逐一实际删除后精确名称清单为空。每场独立的单成员 TLS etcd、真实 Linux
PID1/runtime 和 observer；Kubernetes API 使用既有严格 fixture 转发真实 Supervisor，
没有连接生产 Kubernetes 集群。测试没有显式提交 Execute 候选。

| 最终 epoch | 测试耗时 | 原始日志字节 | WARN / ERROR |
| --- | --- | --- | --- |
| mounted-05 | 36.43s | 4856 | 7 / 3 |
| role-05 | 27.07s | 5022 | 7 / 3 |
| aba-01 | 63.20s | 7858 | 7 / 3 |
| ack-01 | 65.47s | 7856 | 8 / 3 |

mounted：启动前真实 Task/P 同 revision23，E 尚不存在；严格 v2 自动 worker 经原
两次 Claim rollover 完成非空归档及目录/文件模式验证，RunJoined、release、空 S；
正常轮询后 P 消失，结算观察后至少等待30s。Close/Wait 均 nil，借用 kernel 仍有效。
role：独立命令 PID39 自动完成物理链，SIGTERM 后实际 Wait0、无输出、原 lease 已
撤销；新命令 PID48 的同一原会话实际续租 TTL40→59 后被精确 fence，实际 Wait1、
仅固定失败诊断，原 lease 撤销。未为通过测试忽略 unknown 或其它关闭错误。
四场最终各观测 provider API fixture 1 PATCH/1 DELETE、真实 supervisor SIGKILL
及父进程实际 Wait；原 settled E/release 和无占用 S 的断言保留。

ABA：只拦截精确单 Success Delete(P) 的冷 CAS；实际上游删除并用原字节重建 P，
原 birth23 CAS 失败且新 birth128 保留。约30.52s 后正常新轮以新 birth/mod fences
删除，Deleted=1、revision131。两次均44 compares/47 nodes，请求10832/10834B。
ACK：真实 CAS Deleted=1、revision127，44 compares/47 nodes、10832B；扣留真实
响应至原2s RPC deadline 后，远端 handler 实际 join1。原 worker 因 outcome unknown
终止且 Close 保留 deadline 错误；P 在替代启动前已不存在。新原始 session 完成正常
30s 观察后关闭，E/release 字节及原归档/关闭/终止/provider 写计数不变，没有重放
物理效果。代理、所有接受的 stream/pump、原 session 与实际 Grant lease 均检查回收。

原始日志 SHA256：

- mounted-05：`c8ffe9c87f447560b7c4e8604c2d001168c92a13ce08711f0daf0ccd78eab857`
- role-05：`ed6e217b23cbb2c5401b71aa3ac46d87933f8f50627656bd0eb0f99d3bbff61b`
- aba-01：`c73b3e7a69aed539e73d46949e388021899229b0da26a717197083d65e766690`
- ack-01：`38735f6e5bba9038ba3cb9c60327acdd528f904a3c8cada2575126af0551b3f9`

实际环境为固定镜像 ID `sha256:2d858271898f1ff44d29b520f4790fac429ff9b54c3b32f85d31a58aa24f5612`，
network none/private cgroup namespace、只读根、512MiB/2CPU/256PID、GOMAXPROCS2，
/tmp 为128MiB tmpfs；额外 /workspace 为1MiB tmpfs、UID/GID1000、mode0700。
外层 fixture drop ALL 后仅加 KILL/SETUID/SETGID/SETPCAP/SYS_ADMIN/SYS_PTRACE，
AppArmor unconfined、NNP=true，使用 Docker 默认 seccomp；实际 worker 仍是真实
非 root 零 cap 边界。runner/monitor/etcd 三个只读 ELF bind，role 再加第四个命令 ELF。
Root 独立核对实际 command、前后 capabilities/限制/挂载、当前及运行前后二进制字节。
worker100s/parent120s context、worker测试进程110s、supervisor140s及协议时限均未
扩大；外层测试 timeout150s。

失败历史未删除：前三轮 Root fixture 的 /workspace 目标、NNP/default seccomp 配置
错误；第4轮三个新 helper 的 map[string]any 把 uint64 ClusterID 转为 float64，触发
正确身份拒绝。修为 RawMessage 保持整数字节；未弱化身份、关闭或超时守卫。
所有 WARN 保留，含 lease-not-found 与 ACK 的实际 deadline；每场3条 ERROR 来自
etcd 关闭监听/HTTP server teardown，真实 etcd 进程实际 Wait=SIGTERM，未伪装为0。

当前 runner 为71,144,591字节静态 AArch64 ELF，SHA256
`70c436252bfa05576f8a7ee697dd3e39da3e449909abd16f3803c8512381c68f`；生产命令 ELF
沿用 `031592f65b17d882f71004cc487cbb14b330247d23852c89fb2d6123482b2bb8`。
build-02 与当前 Linux vet-01 均通过，各9334份实际输入、空日志；17类字段、前后
源码/工具及全部归档均核对。Root 独立复核实际 native 证据；全部源码保持冻结。

本片每场仅一个实际 Task，不证明32/33物理并发、大 N 容量、生产部署、原生 compaction、
pending Watch R+1、默认后端切换或完整迁移完成；完整 Task 审查与其它未覆盖矩阵仍需完成。
