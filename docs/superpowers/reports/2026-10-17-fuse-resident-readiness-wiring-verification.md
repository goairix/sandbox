# FUSE 驻留 readiness 启动与 probe 接线验证（2026-10-17）

## 行为与边界

`workspace-mounter supervise` 同时启动 Unix control、Docker reaper 和驻留只读 readiness；任一 Serve 退出会取消其他服务，等待全部原始 Serve 返回后才调用 Supervisor.Shutdown，包括部分启动失败路径。所有服务错误合并返回，不把取消当作阻塞本地检查已经退出。

Kubernetes mounter readiness 改为 HTTP `GET /readyz:9808`，保持 10 秒周期、1 秒超时、成功阈值 1、失败阈值 3；有限启动期的 prepared exec startupProbe 保持不变。端点不提供管理能力，不添加 Service 或认证凭证；管理命令仍走原有受信 control。需要使用包含新端点的 mounter 镜像。

共享端口与路径归入轻量 `internal/fuseprotocol/readiness.go`。mounter 现有导出常量仅作兼容别名；Pod 渲染不因这两个值新增对 filesystem/watcher 实现包的依赖。管理协议版本仍为 1。

预备 Pod 合同升级 `kubernetes-sandbox-pod/v6` → `v7`，防止带旧 exec probe 的预备池身份继续复用。共享版本同时进入普通池与 FUSE 池身份，因此普通池缓存也会失效；本改动不会直接改写或删除现有运行 Pod。现有 desired/current 完整 Pod.Spec 比较仍严格拒绝旧 exec readiness，新测试直接验证该差异；不修改通用 Pool 或 FUSE pool 实现。

## 验证证据

原始证据目录：`.superpowers/sdd/2026-10-17-fuse-resident-readiness/`。每个 epoch 的完整 Go-list JSON 覆盖 17 个规范源字段，保留 source/tools 前后清单、原始日志/退出码、精确命令/环境和 source/tools archive；在后续 Go 或修改前审计通过。Go 1.25.6，`GOTOOLCHAIN=local`、`GOMAXPROCS=2`、独立 GOCACHE，未改动 go.mod/go.sum。

| epoch | 结果 | 输入数 | 日志或 runner SHA-256 |
| --- | --- | ---: | --- |
| resident-wiring-command-host | 全包 13 组通过，1.022s，无 skip | 2,101 | `cb79c7f6cb0fe33c684e9d17bb5ded15b83f464337015392a23e69f75d41e35c` |
| resident-wiring-command-race | 全包 13 组通过，2.587s，无 race/skip | 2,108 | `71394473fb114624e8d1a631d8792cf262c3abfc323719fe1f8b6a7523e5e698` |
| resident-wiring-command-vet | Linux/arm64 vet exit 0，空日志 | 2,133 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |
| resident-wiring-command-build | Linux/arm64 测试 runner 编译 exit 0 | 2,133 | `6c19661e3cbf020a7211da5754c25a979c8f69fa10644cbcd5673d8b60ac5065` |
| resident-wiring-kube-host-final | 全包 324 组通过、1 个既有 Redis 集成测试跳过，8.158s | 7,551 | `5e0d29c69a469e82a524e67b0cbc1e8f2a7d6f2a5b705ed6e41baf1790407886` |
| resident-wiring-kube-race | 相关 34 组通过，4.219s，无 race/skip | 7,556 | `575ddbbc5322b2e3c450895d6becd9da496de2227075039c38746f829d00fa9b` |
| resident-wiring-kube-vet | Linux/arm64 vet exit 0，空日志 | 7,584 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |
| resident-wiring-kube-build | Linux/arm64 测试 runner 编译 exit 0 | 7,584 | `97b12a7a564cd28c7015ef93f72240970312ca27a4e975f008121cd92ba99a1f` |

command host/race 选择器均为 `^Test.*$`，包 `./cmd/workspace-mounter`；Kubernetes host 同选择器，包 `./internal/runtime/kubernetes`。Kubernetes race 选择器为 `^Test(DNSAdmission.*|CreatePod.*|PreparedFUSE.*|WarmPoolContract.*|ResolvePreparedSandbox.*|PreparedPodIntent.*)$`。race 在 Go-list 与测试命令两侧均启用 `-race`。

RED 记录保留：服务组缺失、渲染结果仍为 exec、v7 合同预期遇到 v6。首次 Kubernetes 全包运行只有 `TestDNSAdmissionDefaultPodBaseline/fuse` 因完整 Pod JSON 的 readiness 变化而失败；已仅更新这一 FUSE golden hash，普通 Pod baseline 未变。唯一跳过项为 `TestWorkspaceControlAuthorityWithRedisAcrossClients`，原因 `TEST_REDIS_ADDR not set`，不计为已验证的 Redis 集成。

## 冻结源码与审查

| 文件 | SHA-256 |
| --- | --- |
| cmd/workspace-mounter/main.go | `2bf32f1a428c6c1a5577d8a8ba164965a23c8e96b4bf058892c4e43e485d1942` |
| cmd/workspace-mounter/main_services_test.go | `f28ce55446def90c0b9cf26bc37337ee456519fb416f5e371d071aedf79b8fa7` |
| internal/fuseprotocol/readiness.go | `05e2229da8dd5b965b872568ce2c8a6ab8bc496cfe128bed2daf66f17a21fe01` |
| internal/mounter/readiness_server.go | `fd0ad3d7a5bf02e6c1640dc231bb890d1b7c7b570fa2a520d6f71da83663200d` |
| internal/runtime/kubernetes/pod.go | `8c7c73d0909822d34231403e1006d1d7625835e428c987fde8da6f2fa8c7ada0` |
| internal/runtime/kubernetes/pod_test.go | `41bb9ae411d02c1cac25339aa51e52c1e8e76e833927c2375f063a40894fb89a` |
| internal/runtime/kubernetes/dns_admission_test.go | `b5c55eac25b793cb9d55cb79f2a49866dab458e72aa4341c82b4b9b0c11c4f1f` |
| internal/runtime/kubernetes/runtime.go | `f41812bf9611f9c2fc8a565c4ca391676c5dc7a19f49c676976c8cc498225916` |

Root 已独立核对最终八个 epoch 的17类源码、工具、前后清单、全部归档成员和当前字节。
实际 Linux 服务组选择器 `^TestMounterServices.*$` 的3组通过，原始375字节输出 SHA256
`d55b80568fee9cf187a5f3304bf1b704e15606d3a503eb3642fa8043ae5d8986`。
实际 Linux 的 Pod 探针、旧探针拒绝、DNS 默认及 warm-pool 合同5组通过，原始1361字节
输出 SHA256 `6b2bbf73b11acba068760d1daa5dde73cb96f62a2a848472e2c50e0b92871020`。
两次均使用上述对应哈希 runner，容器退出0、无OOM/skip/race；固定镜像、network none、
只读根、2CPU/768MiB/128PID和128MiB临时盘均已核对，二进制前后哈希一致，精确容器
已删除并确认名称清单为空。首个服务组清理审计脚本错误地替换了PID上限字面量，原
断言失败保留；核对实际配置128后修正审计并通过，没有重跑或修改测试结果。

已检查注释、逻辑分段、服务取消/实际退出边界、测试失败时释放与 join，以及 protocol/runtime/mounter 包边界；最终上述 8 文件 `gofmt -d` 为空。Linux 运行仍是服务组/渲染模型；未验证实际 PID1/FUSE/s3fs/对象存储或 Kubernetes kubelet/CNI。节点 collector、原 FUSE 管理轮询移除及容量验收仍未完成。
