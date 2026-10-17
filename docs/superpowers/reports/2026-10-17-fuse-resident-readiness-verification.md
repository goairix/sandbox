# FUSE 驻留只读 readiness 组件验证（2026-10-17）

## 本次范围

本次仅新增 `internal/mounter/readiness.go`、`readiness_test.go`、`readiness_server.go`、`readiness_server_test.go`。复用已独立验证的增量缓存监控，不修改其源码。命令启动接线和 Kubernetes probe 切换属于后续独立提交；本组件本身尚未替换现有 kubelet exec probe。

驻留观察器只验证已经由管理路径建立的 ready mount，每次检查前先发布 unknown。一个原始 MountInfo 调用在 Supervisor 锁外占据唯一检查 worker，实际返回前不会因取消或超时释放后另起检查。返回后用 TryLock 再核对 generation、mount ID、进程完成通道和固定挂载/缓存身份；正向挂载及缓存样本超过 3 秒、来自未来或状态不明均拒绝。健康状态每秒检查一次；仅尚在工作的未知缓存样本使用 250 毫秒重试读取，避免两个一秒周期长期对齐未知窗口，不会据此并发启动挂载检查。

HTTP 仅提供 Pod 网络上的 `GET /readyz`（默认 TCP 9808），返回固定 readiness 文本，不调用 ReadyStatus、fork、遍历文件树或分发管理命令。调用方无须管理凭证，包括同 Pod 网络内用户也只能观察布尔状态；不会创建 Service 或签发能力。连接上限 8，读写/请求头超时均为 1 秒；配置请求头 1 KiB，另计 net/http 固定 4 KiB 解析余量，拒绝请求体及查询参数。

取消先关闭网络接入，再等待 HTTP Serve、实际连接退出和原始观察器完成。缓存 backend Close 的不确定错误向上传递；纯启动前取消正常结束，不把带其他后台错误的包装结果当成功。

## 可复核证据

证据目录为 `.superpowers/sdd/2026-10-17-fuse-resident-readiness/`。每个 epoch 保留原始 Go-list JSON（17 个源字段）、前后 source/tools 清单、命令/环境、原始退出码、日志及 source/tools archive；在后续 Go 调用或修改前审计通过。工具为 Go 1.25.6，`GOTOOLCHAIN=local`、`GOMAXPROCS=2`，独立 GOCACHE；宿主 darwin/arm64 启用 CGO，Linux/arm64 vet/build 关闭 CGO。

统一选择器：`^Test(Readiness|ResidentReadiness).*$`，包 `./internal/mounter`；测试参数 `-v -count=1 -timeout=120s`。race 模式在 go-list 与 go-test 两侧均显式启用 `-race`。

| epoch | 结果 | 输入数 | 日志/产物 SHA-256 |
| --- | --- | ---: | --- |
| resident-component-host-final | 12 组、17 叶测试通过，11.307s，无 skip | 2,097 | `4776b98ea48ffd735d80421700089a3c24f03d7dc2669f15a6694b87dbb07224` |
| resident-component-race-final | 同一 12 组通过，11.322s，无 race/skip | 2,104 | `35133ed3659c57a7793e94e8f84122906a7ea2d88640e5c3eac2a8a9ad44606c` |
| resident-component-vet-final | Linux/arm64 vet exit 0，空日志 | 2,129 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |
| resident-component-build-final | Linux/arm64 测试 runner 编译 exit 0 | 2,129 | runner：`c0b9b7b8c5f90b2b0a4cffed29cb9b2a4749a7df3d98dc142d2e6d83c1313efc` |

覆盖空闲挂载持续正向样本、旧/未来/终态/锁争用拒绝、身份与进程变更、缓存超限/unknown、阻塞原始检查持槽及关闭等待、周期相位对齐恢复、只读 HTTP 固定响应/管理路径拒绝、真实 TCP 空闲探测、16 KiB 请求头返回 431、慢请求 1 秒过期、连接准入上限、取消先关接入后等待观察器、后台关闭错误保留和启动取消。

测试先于实现：初始观察器、HTTP、缓存启动取消均保留实际 RED 记录；相位对齐回归 RED 中 480 次缓存正向观察仍无法得到挂载正向样本，修复后通过。首个相位模型因边界顺序通过的记录也保留，不冒充 RED。所有 4 个新增文件最终 `gofmt -d` 为空，已人工检查注释、阶段分隔、单向包职责与并发所有权。

## 冻结源码

| 文件 | SHA-256 |
| --- | --- |
| readiness.go | `fc54d339eaed78bd739273ec185ea6dc85141f44f5f028253e6bd16f94bb231f` |
| readiness_test.go | `632cfb393e5e8ac8882a3909f798914c875e611fc0f00c24f4e376ce5f7b172e` |
| readiness_server.go | `982a785423be0aca84600a467b4e780fb2f25c67f828eebda3c867b3ef712177` |
| readiness_server_test.go | `124ded2bc9f139737e9dd3a3ed4762412996801a338675211667b39773588646` |

## Linux 模型执行与限制

Root 使用相同哈希 runner 执行真实 Linux/inotify 与 TCP 测试：`resident-component-linux-model-01`，12 组、6 子例（17 叶）通过，exit 0、无 OOM/skip/race。原始输出 2,691 bytes，SHA-256 `5de6237c5b5b68912b75367a080d50a4e3f4fb5e7aacca45f92117a94f65bb6a`。镜像固定、network none、只读根、2 CPU、768 MiB、128 PID、128 MiB 临时盘，runner 前后哈希一致；精确测试容器已删除并核实 inventory 为空，cleanup 日志已保存。

宿主测试中的极小 kqueue fixture 只验证会计/生命周期；Linux 模型虽然使用真实 inotify 和 HTTP，MountInfo 仍为受控模型，未证明实际 FUSE 内核挂载、对象存储写入、真实 Kubernetes kubelet/CNI 路径或容量指标。节点 collector、既有 FUSE 管理轮询移除、生产规模成本与端到端 FUSE 验收仍未完成。
