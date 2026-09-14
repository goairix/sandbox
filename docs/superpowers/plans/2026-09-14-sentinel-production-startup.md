# 内置 Sentinel 生产启动接线 Implementation Plan

> REQUIRED SUB-SKILL: subagent-driven-development / executing-plans、test-driven-development。原地执行；父任务提交；不改线上、不构建发布镜像。

**Goal:** 将已复审身份、登记和落盘事务接入实际进程启动、初始化 Job、API gate 及完整 Chart，最后用隔离真实三成员测试验收。

**Architecture:** Sentinel 唯一负责稳态选主。启动包装器保留角色，只允许新登记配置或恢复已有主；初始化协调器验证真实进程、复制与 ACK 后 CAS 开门。核验器不参与业务请求。

**Tech Stack:** Go 1.25、go-redis RESP2、Kubernetes typed ConfigMap API、Helm、缓存 Redis 7 Alpine。

## Task 1: 投影读取与 API 初始化 gate（父任务）

Create `internal/redisbootstrap/bootstrap_files.go`, `bootstrap_files_test.go`；Create `cmd/sandbox/redis_bootstrap_gate.go`, `redis_bootstrap_gate_test.go`；Modify `cmd/sandbox/main.go`, `internal/config/config.go`, `configs/config.yaml`。

```go
type BootstrapFilePaths struct { Cluster, Registration, PublicKeys string }
type BootstrapFiles struct { Cluster ClusterState; Registration *BootstrapRegistration; PublicKeys [3]ed25519.PublicKey }
func ReadBootstrapFiles(context.Context, BootstrapFilePaths) (BootstrapFiles,error)
func WaitBootstrapInitialized(context.Context, BootstrapFilePaths) error
```

- [x] 实际临时目录写 Pending cluster + 三公钥，stub Read 返回错误；测试应断言加载成功及 registration=nil 产生行为 RED。Configured/Initialized 文件必须匹配 cluster、keyDigest；默认 Pending registration 缺失只接受 ENOENT。
- [x] 固定显式绝对非根路径，regular、有界 cluster/reg64KiB、keys1KiB，允许可信 Kubernetes `..data` symlink；读取两轮 bytes 完全一致、各轮解析 cluster/reg 同 phase、公钥摘要一致；异常返回零对象及常量错误，ctx cancellation 保留。
- [x] 写实际有效 Initialized registration，Wait 成功；Pending 在有界 caller ctx 到期前不放行。固定250ms轮询、ctx可取消，不创建/更新文件、不请求私钥。
- [x] config 增加 `bootstrap_state_directory`、`bootstrap_public_keys_file`，默认均空；空目录禁用 gate，单字段/相对路径/非 sentinel/不满足 requireHA+replica_ack+ack1 均拒绝。不对默认 standalone/external 增加依赖。
- [x] main 在创建 runtime/store/manager 之前调用 gate，预算10m；drain/owner inspection 明确绕过首次初始化 gate 以便清理，但不绕过实际 Redis 原安全检查。新字段 YAML/ENV 测试真实解析。
- [x] `go test ./internal/config ./cmd/sandbox ./internal/redisbootstrap -count=1`、race focused、vet/lint；先 SPEC 后 QUALITY 独立复审。

## Task 2: 本地启动包装器与最终 exec

Create `internal/redisbootstrap/member_startup.go`, `member_startup_test.go`, `retained_parts.go`, `retained_parts_test.go`；Modify `persistent_config.go` 只提取原 Redis/Sentinel 独立 parser，保持既有严格联验；Create `cmd/redis-bootstrap/member_command.go`, `member_command_test.go`；Modify main dispatch 追加 `redis` / `sentinel` 两模式。

Create `internal/redisbootstrap/pod_preparation.go`, `pod_preparation_test.go` 和 `cmd/redis-bootstrap/prepare_command.go`, `prepare_command_test.go`；追加一次性 `prepare-pod` 模式，仅为本 Pod 挂载的 PVC/emptyDir 准备权限和当前静态 binary/自身 seed。

```go
type MemberStartupOptions struct { Files BootstrapFilePaths; Directory string; Ordinal int; Initial InitialConfigOptions; Timeout time.Duration }
func PrepareMemberLaunch(context.Context, MemberStartupOptions, bool /* sentinel */) (string,error)
```

- [x] actual PVC Reserved+注册文件使包装器完成同marker事务并准备 config 路径；stub 行为 RED。两包装器并发配置由已复审 flock 串行。Pending未登记只在真正空卷预约 Reserved，等待登记；已有非空未知数据不得预约。Initialized/已登记空卷拒绝自动新身份。
- [x] 独立读取本地 Sentinel monitor/configEpoch/globalEpoch/myid；独立 Redis priorRole/priorTarget；普通 Inventory 仍拒绝不一致。可信当前 UID 私有文件、同marker、key digest、固定公钥派生 Sentinel myid、前后 inode/bytes 一致；所有可执行扩展/非固定成员拒绝。
- [x] Sentinel 包装器等待 durable Configured，允许 local Redis/monitor 暂不一致后直接启动保留 Sentinel 配置，不等任何 Redis/quorum，绝不改 monitor/epoch。Redis priorReplica 保留其配置启动，允许 Sentinel 随后收敛，不依据多数报告晋升副本。
- [x] priorPrimary 必须取得另两成员的新 Inventory proofs，原注册 marker、公钥、configured一致，结合自身 Sentinel证据 SelectEvidence最高epoch且>=2；任何已连接但Rejected 阻断，未完成dial不当离线；最多并行两个有界RPC，未证明时等待预算。自身更高 epoch 或同epoch不同映射拒绝旧证据。
- [x] matching priorPrimary+本地 monitor/epoch 完全匹配selected且target=self，仅重启该已保留主。不使用 REPLICAOF NO ONE。若selected!=self，等待本地 Sentinel persisted monitor/epoch 与selected一致，再在本地锁内仅把 Redis配置修改为 replicaof selected；保留其它配置/身份/密码/epoch，原子600 temp sync/rename/dirsync；前后实际文件一致，绝不写运行中的 Sentinel配置。
- [x] 初始化0号也走保留证据检查：等待两个Configured replicas即可形成初始epoch0证据，replicas/Sentinel不等主，避免循环；首次安装函数成功不代表恢复任意旧主权限。
- [x] actual-files 与签名测试覆盖非0主恢复、旧主降为从、Sentinel不依赖Redis、缺两状态/高单份epoch/拒绝端点/空同名replacement/预算取消/密码日志redaction。等待间隔250ms，可取消；仅首次进程启动检查，无持续控制循环。
- [x] cmd最终 `syscall.Umask(0077)` 然后 `syscall.Exec("/usr/local/bin/redis-server", []string{"redis-server", path, optional "--sentinel"}, os.Environ())`；测试私有exec seam核对 argv 无密码、没有 shell/sleep、exit和ctx信号行为。容器路径固定，不能由输入选择任意可执行文件。
- [x] 一次性prepare init以root运行但仅本Pod mounts，dropALL并只加CHOWN/FOWNER/DAC_OVERRIDE（仅用于中断重试时读取已变为非root owner的自身emptyDir文件），无hostPath/hostPID/hostNetwork/SA token；不承担核验服务/选主。PVC root UID只能root或所配置共同非rootUID，nofollow稳定mount、只chmod/chown root及已确认真正为空的lost+found；保留文件不递归chown、不chmod、不删除。未知UID/异常root fail closed。所有长驻容器统一非rootUID/GID默认999，Pod不设fsGroup，避免kubelet把600保留配置改660。
- [x] ownseed source mount仅pod-name subPathExpr文件，0400 root-owned，prepare读取bounded32并用publickey+ordinal验证，再仅复制至新private emptyDir own seed0400/nonrootowner（sidecar唯一挂载）；不打印/输出seed，不写PVCidentity。tools emptyDir复制固定可信 `/app/redis-bootstrap` 至 `/redis-tools/redis-bootstrap`，最终目录/文件0555 root-owned，其它容器只读挂载；代码source不是用户指定任意路径。实际FILES tests验证未知UID/非空lostfound/失败不改retainedconfigs、seed错误不安装、permissions与独立用户读取；ROOT专属行为用隔离cachedRedisfixture核对，不把mock当实际chown证明。
- [x] focused/race/vet/lint，SPEC/QUALITY。主任务运行真实 cached Redis 持久rewrite回归。

## Task 3: 初始化协调器与真实拓扑

Create `internal/redisbootstrap/topology.go`, `topology_test.go`, `initialize.go`, `initialize_test.go`；Create `cmd/redis-bootstrap/initialize_command.go`, `initialize_command_test.go`、`identity_secret_command.go`, `identity_secret_command_test.go`；Modify command dispatch。

```go
type TopologyOptions struct { Registration BootstrapRegistration; PublicKeys [3]ed25519.PublicKey; MasterName,DataPassword,SentinelPassword string; AckTimeout time.Duration }
func VerifyTopology(context.Context,TopologyOptions,bool /* require all three */) error
```

- [x] 真实 RESP TCP fixture验证 AUTH 后 INFO server+replication、Sentinel MASTER/SENTINELS/CKQUORUM、Live签名及 independently current run_id；未具备已复制 offset 的 WAIT0 不能过关，先观察 RED。
- [x] fixed各DNS6379/26379，密码ENV，RESP2预分配前沿用 bounded frame decoder、单请求超时/总预算、关闭连接、无 retry/redirect。验证一个实际 primary、其它运行实例 replica 指向它且 linkup/sync0、至少1健康从，同一 replid；Sentinel quorum/固定myid/已知成员/current monitor+mapping epoch一致，Live proof的角色/持久配置与 INFO一致，二次 INFO 防 Redis replacement。
- [x] 同一个 primary `Conn`执行随机短TTL `SET sandbox:bootstrap-barrier:<nonce> 1 PX60000` 再 `WAIT 1` timeout1..10s；ACK失败明确失败、不自动 best_effort；不在常规 readiness 反复写。
- [x] initialize Job从 scopedtypedCM GET锁定UID/RV；无登记必须3真实Reserved证明 RegisterNamespaceBootstrap；已有登记不改marker；等待3实际Live拓扑，成功后一次PinnedCAS同时将 cluster.json和registration.Cluster.Phase 改Initialized。冲突重新GET校验相同UID且完整流程重核验；UID replacement拒绝。不create/delete任意CM。
- [x] 已Initialized模式验证至少2实际健康成员+ACK1后退出，正常单节点故障不要求3才能升级。较高reachable保留epoch/不可信成员不能被较旧2忽略。诊断不输出密码/配置/原Redis网络payload。
- [x] manual `identity-secret -namespace ... -statefulset ...` 只输出既有 GenerateIdentitySecret 的JSON object（含v1/Secret TypeMeta，main stdout）；安装文档用户明确pipe `kubectl create -f -`，不使用apply覆盖旧身份，工具自身不创建 Secret，也不更新已有身份。help无secret读写。
- [x] 实际命令/typedCAS/RESP行为测试，SPEC/QUALITY和full验证。

## Task 4: 原生 Chart 完整接线

Modify `deploy/helm/sandbox/values.yaml`, `values.schema.json`, `templates/_helpers.tpl`, `deployment.yaml`, `secret.yaml`, `redis.yaml`, drains；Create `templates/redis-sentinel.yaml`（将普通初始化Job和状态资源同文件接线，不拆第二模板）；Modify `scripts/test-helm-chart.sh` 及结构化断言、安装文档/生产示例。

- [x] render测试新Sentinel mode结构先 RED；standalone默认快照不变，external无内置资源。
- [x] 三个Parallel StatefulSet Pods各Redis/Sentinel+nativeattestor initContainer restartPolicyAlways；Linux+Kube1.33；headlesspublishNotReady端口6379/26379/18080；PVC独立、AOF、requiredhostnameantiAffinity、PDBmin2。
- [x] APIimage当前静态bootstrapbinary：一次性prepare init复制固定binary和自己的seed至emptyDir并准备当前PVCroot权限，之后nativeattestor常驻；Redis/Sentinel用该静态binary最终exec自身镜像Redis。source ownseed采用projected Secret key pod-name subPathExpr隔离，仅prepare挂载source，native只挂preparedownseed，所有其它容器只能publicJSON。POD_ORDINAL直接来自1.33 StatefulSet `apps.kubernetes.io/pod-index` downward label，无任意shell提取/密码拼接。
- [x] immutable身份Secret必须用户提供，stateCM新clusterID仅fresh随机且lookup保留cluster/reg、keep卸载保留匹配PVC；区别旧单实例名称。初始化Job作为普通资源与STS一起创建，名称带 release revision，禁止 pre-install 或 post-install hook：Helm `--wait` 会先等待 API Ready，再执行 post-install，和 API gate 构成死锁。SA仅namedstateCM get/update；创建次序造成短暂CM/RBAC/DNS缺失只在固定预算中重试，UID一旦读取即锁定。文档要求 `--wait --wait-for-jobs --timeout 15m`；原drain hooks保持。
- [x] NP限定同release/API/drain/init身份端口，允许DNS及所有插件通用NetworkPolicy；密码同一existingSecret明确datakey/sentinelkey；避免env重复，effectiveRedishelper统一API/init/wait/drain，preupgrade保持lookup旧actualenv。
- [x] readiness只本地服务角色/复制/CKQUORUM，liveness只PING或healthz，init等待不会触发重启风暴；预算启动10m/Job12m/Helm建议15m以上；资源初始observerrequest64Mi/limit256Mi，实测RSS不是峰值承诺。
- [x] 最终接口复审修正：内置Sentinel必须启用API startupProbe，period/timeout1..60、failureThreshold1..maxint正整数；自动等待至少max(600秒门禁,Job预算)+180秒，ceil后增加首次立即probe余量，并保留用户更长预算。实际结构化测试先RED后GREEN，standalone/external不变。
- [x] 最终接口复审修正：内置Sentinel pre-rollback在任何状态patch前执行静态`deny-rollback`模式，始终非零、不读ENV/文件/API且不挂token，防止历史Pending manifest覆盖登记。禁止`--atomic`、只能forward upgrade；默认standalone/external原fingerprint守卫不变。命令及实际Helm结构行为RED→GREEN。
- [x] Sentinel参数masterName/down1000..60000/failover>=2down<=600000/parallel1..2/persistence/auth/HAreplica_ack1严格渲染验证；密码inline校验安全32..256token，existingSecret运行时校验不泄露。两密码值必须不同以验证独立认证链路，仍允许同Secret不同键；external兼容不变。productionChecks接受满足要求builtinSentinel，不默认降级standalone。
- [x] 中文新增values/schema/config/docs一致；`bash scripts/test-helm-chart.sh` default+sentinel+external结构通过、envunique、chartlint。
- [x] SPEC/QUALITY完整chart复审。

## Task 5: 真实隔离三成员验收与提交

Create `scripts/test-built-in-redis-sentinel.sh`、`internal/redisbootstrap/sentinel_runtime_integration_test.go`；Modify `docs/testing/2026-09-13-sentinel-identity-runtime-results.md`。

- [x] 只使用已缓存Redis镜像，编译当前Linux test/command二进制，创建唯一标签的隔离网络和3卷/进程组；当前发布镜像不构建、不拉取、不接业务，trap只清理本次精确资源。
- [x] actual freshreserve/registration/config/wrapperexec/initialize/SET+ACK，四认证链路、断主切换非0、旧主恢复Replica、Redis-onlyrestart、完整冷恢复、Pod新IP、初始化中断、emptyreplacement/两份状态丢失/高单份epoch阻断、ACK不足不放行。每项以实际进程/命令/响应判定，不能只断言script文本。
- [x] `go test ./... -count=1`、race相关包、`go vet ./...`、lint、Helm测试、真实fixture、`git diff --check`，全部输出读完并如实区分SKIP。
- [x] 全实现独立最终复审，修复到无Critical/Important，再由父任务提交；记录真实通过范围和线上需用户当前镜像部署后的验证。AppArmor真实profile加载仍需用户授权非生产测试节点，不冒称已验收。

## 最终执行证据

相关模块及夹具均SPEC→QUALITY PASS，最终接口复审PASS；最后探针预算和回滚禁令delta亦分别SPEC/QUALITY PASS。全仓、race、vet、lint、四Helm脚本及结构测试通过；真实三成员矩阵和新IP恢复连续两轮通过，Linux root权限/中断/rewrite实测通过。本次不构建/推送发布镜像、不改线上、不回滚、不创建worktree。详细范围和未宣称的在线/内核验收见 [验收报告](../../testing/2026-09-14-built-in-sentinel-production-startup-results.md)。
