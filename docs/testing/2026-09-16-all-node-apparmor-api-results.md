# 五节点 AppArmor 公共 API 验收

## 结论与边界

2026-09-17 续测完成：worker-1/2/3 于9月16日通过，worker-4/5 于9月17日完整通过，
五节点普通/FUSE 公共 API、真实 s3fs enforce、四类内核拒绝、自己的 loader Pod 重建和
正常 drain / 精确清理全部通过。SEC-01 的本集群五节点验收范围已关闭，不以 loader Ready 代替业务测试。

worker-4 此前保留的 runtime 已正常退出；续测先通过正常 drain、Namespace UID 删除和
inventory RBAC GC，再创建新实验。期间另有一轮因冷准备触发 API 重启而严格判失败，
失败轮次不计入通过结果。冷拉取/准备长尾的根因仍未查明，单列 COLD-01，不声称已经修复。

使用 `ds-ai-research` 五个 worker 的独立临时 namespace，逐节点启动同源 Chart API、自己的
standalone Redis/emptyDir 和 loader；普通/FUSE 池各 min1/max2，固定实际 nodeName。
没有 upgrade/uninstall 业务 release、修改业务 Redis/PVC、重启节点或改写/卸载 AppArmor 策略。
遥测关闭仅用于实验，不修改生产 exporter；缺失 LSM 豁免保持 false。

本轮测试当前已部署镜像，不是尚未构建的本地 lint/SSE 修复上线证明。不覆盖节点冷启动、
策略丢失后的加载、Redis/Sentinel 故障恢复、其它内核/运行时/后端、Calico/双华云/VPC，
也不将单节点单 API 实验扩展成多副本或持续负载验收。FUSE 性能优化按用户要求暂缓。

续测只读核验五节点均为 arm64、Linux `6.12.57+deb13-arm64`、containerd `2.2.0` 且 Ready；
业务 API/runtime/mounter/loader 与下表同版本。没有放宽零重启、UID、profile、持久化或清理门槛。

## 已核验镜像与策略

所有实验容器按 digest 固定，并读回实际 imageID，包括 native sidecar 的 initContainerStatuses。

| 容器 | 版本 | sha256 digest |
| --- | --- | --- |
| API | v0.3.31 | `1e8e95347f805417998d4555d6d107d37e0895929bed8d095b33eda79f15cd4a` |
| sandbox | v0.3.4 | `237bd749531b31eb6144b5ce2d4b6dc93232c39bae2989c6bb9a4797a575e8c8` |
| mounter | v0.3.30 | `56db3a0e82c36259ff5325cf5294a5c3b876c085b8ccde147c41c76bca2e8931` |
| loader（arm64） | v0.3.24 | `5a984dcb978f6634acee4446bb2f1ae5d2aca918ee53b189372deb688bcf908c` |
| 实验 Redis | 7.4.2 | `144902e12778c0b4ad2b1812948a05c56ffd3977299fbd201aa628844ff17495` |

策略未改，来自当前 Chart，完整名称：

```text
sandbox-fuse-37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d
```

## 逐节点结果

| 节点 | run / Namespace UID | 普通 API | FUSE 生命周期 | 四类内核拒绝 | loader Pod 重建 | 正常 drain / 精确清理 |
| --- | --- | --- | --- | --- | --- | --- |
| worker-1 | `e98d1ffb` / `e1a55c29-0e68-4043-80ff-70911e09688a` | PASS | PASS | PASS（2 轮缺失证据重试） | PASS | PASS |
| worker-2 | `c912a5c8` / `55e3f848-4677-4017-999e-661479c17f9e` | PASS | PASS | PASS | PASS | PASS |
| worker-3 | `58ebb6a0` / `d729ffaa-ce46-40ad-9a82-eaffa25908bd` | PASS | PASS | PASS（1轮缺失证据重试） | PASS | PASS |
| worker-4 | `e8416437` / `ba86f4f9-38dd-4996-9d10-fea389dbb3df` | PASS | PASS | PASS | PASS | PASS |
| worker-5 | `0f8a8482` / `1f644ac7-5c1b-4892-8f5e-661662f3761a` | PASS | PASS | PASS（1轮缺失 mount 证据重试） | PASS | PASS |

| 节点 | 普通 Runtime UID | FUSE Runtime UID | 实际 s3fs PID / startTime / mount ID / generation |
| --- | --- | --- | --- |
| worker-1 | `45425f88-e70b-4a7e-b97e-3215caa27f38` | `2ab4c2c4-a530-4a04-99f3-03d621d79eac` | `203 / 494830297 / 5540 / 1` |
| worker-2 | `21fd3a21-e01a-48c8-b770-2b2fad9d9273` | `07bff2c0-17ca-4b14-84cd-b662724a7ade` | `217 / 2350321597 / 3076 / 1` |
| worker-3 | `467e9dc6-19cf-4eaa-a38e-f51032e785c7` | `0a9f2a41-4483-42ec-83bc-4e12accae43d` | `243 / 494851242 / 4791 / 1` |
| worker-4 | `a5f57485-1c92-4d27-afdb-6c36afa57159` | `173dfb32-ab7a-41a6-bba7-d1210ade2e1e` | `265 / 2352580178 / 4363 / 1` |
| worker-5 | `dec1ab0e-39ed-4120-828d-8b67bb8d43da` | `a6ef4eb2-837e-4059-814f-443a67135fcf` | `203 / 2188548347 / 2466 / 1` |

worker-4 在9月17日00:38:54、worker-5 在00:42:44（北京时间）完成全部阶段。
两轮自己的 loader 原/替代 UID 分别为：

| 节点 | 原 loader UID | 同节点替代 UID |
| --- | --- | --- |
| worker-4 | `92d5f478-8b92-4ac8-b187-83b03a8ecdbb` | `b655f6ab-e52e-4bbd-bd29-3d1d094489ab` |
| worker-5 | `7672505f-85ae-445f-8764-03766abf8c73` | `05ab48c2-118d-419a-b27c-3e92b5afc6f3` |

namespace 格式为 `sandbox-aa-api-<run>-<节点序号>`，所有 Pod UID、实际目标节点、零重启都在
每次操作前后核验。API 返回 runtime_id 必须为此前 pristine Pod 的名字，且 UID 不变。
普通池要求 Pod Ready；FUSE 预备池尚未挂载，因此不要求 PodReady，而是 exact UID、prepared、
generation0、空缓存。实际领取后要求 health ready/fuse/generation1，不能混淆这两种阶段。

## 每节点验收内容

- 普通沙盒：公共 create/exec/delete；Python/Node/bash 固定内容、文件写读/fsync、UID1000、
  NoNewPrivs=1、Seccomp=2、默认 `cri-containerd.apparmor.d (enforce)`。销毁后原 Pod UID 消失，
  对应策略收尾，普通 pristine pool 恢复。
- FUSE：公共 API 从预备池领取 persistent sandbox；真实写入、fsync、读回、覆盖、追加、rename、
  unlink，手动 sync/flush 与 workspace/info 的 mounted/fuse/flushed。使用当前可信 TLS MinIO，
  独立 SigV4 GET 内容严格等于 `verified-append`；删除最终文件并 flush 后独立 GET404。
- 读取 supervisor PID1 和真实 s3fs 的 attr/current，要求同一 exact profile enforce；联合
  s3fs ppid1/startTime、真实 fuse.s3fs mount ID、health RuntimeUID=实际 Pod UID、generation1。
  负例前后进程、挂载和代次不变，沙盒仍能读回文件；不是以测试 shell 代替 s3fs。
- 仅自己的 unconfined loader 做允许对照：shadow 读重定向 /dev/null、可写 /dev/shm 写删、
  自己 run 路径 tmpfs mount/umount/rmdir，均须成功。受限 mounter 的 hosts 读与 run 写须成功；
  shadow 读、自己 /dev/shm 写、已受限进程再次 exec shell、自己 cache 子目录 tmpfs mount 须失败。
- 内核拒绝必须匹配 exact profile、本轮随机 marker、测试前 dmesg 水位；shadow 与 exec 用
  同一 exec PID 的 marker 拒绝关联，mount 用唯一目录关联。必须有 open/mknod(or open create)/
  exec/mount 对应证据，utility 错误或只读 FS 提示不能单独算 AppArmor 通过。
- 仅 UID 前置条件删除自己的 loader Pod，同节点新 UID Ready、零重启、exact kernel enforce；
  证明 Pod 重建恢复，不冒充节点重启或策略丢失恢复。

## 夹具失败与修正

未通过轮次不计作业务故障或节点通过：

- `94c68040`：API 容器名称实际为 sandbox，最初用 api 查 imageID，导致误报。
- `5fc9a30d`：最初要求 FUSE pristine PodReady，因预备池尚未挂载而超时；改为读取严格
  prepared 健康，不放宽真实挂载 readiness。
- `9f4896b8`、`d5d82a98`：FUSE 功能已通过，但审计关联未通过。现场观察到 uptime 与 dmesg
  时间戳约 82 秒偏移，且 kauditd_printk_skb 存在 callbacks suppressed；前者误过滤新记录，
  后者丢弃部分 printk 审计。改为同一内核日志时间域的前置水位，以及只重试缺失负例的有界
  间隔收集，最多4轮/120秒启动预算。没有修改内核限流参数、策略规则或用历史业务审计顶替。
- worker-4 的 `7b0ab854`：API Pod 在拉取当前 index digest 的镜像期间未启动，240秒启动预算
  耗尽；尚未进入公共 API/AppArmor 验收，因此不计成功。注册表只读核对 arm64 child digest 为
  `a376b2eeb3e5d1c41fad536009bffccd0ca1a5b6e1e02a6cc8cbf59ca6c16caa`，压缩层共60,676,203字节。
  没有改镜像或超时；同 Chart drain Job 在 worker-1 运行，自己 API 缩零后旧 Pod 正常退出，
  drain、namespace/RBAC 清理和业务不变均通过。新 namespace 再试，结果以独立完成轮次为准。
- worker-4 的 `17eaec31`：API 镜像已读回相同 index imageID，但普通 runtime Pod 持续
  Pending/ContainerCreating（没有 imageID）；API 重启一次，上一轮日志匹配启动失败、超时和
  termination unconfirmed，未匹配 security contract/AppArmor 门禁失败。240秒启动预算耗尽。
  drain Job 在 worker-3 运行，自己 API 缩零并退出；该 Job 在18:19以 BackoffLimitExceeded失败。
  普通 runtime Pod 已在18:14:12进入删除，18:19:36仍 Pending，UID仍为
  `ded0c286-e1d0-47fa-bef0-e372d8a1368b`。因此无法确认 runtime终止，不能裸删状态或 namespace。
- worker-4 的 `af9f8f5f`（9月17日续测）：普通 runtime 已就绪，mounter 在00:30:07开始拉取，
  后续观察仍为 PodInitializing、没有 imageID。API 上一轮日志匹配启动失败、超时和
  termination unconfirmed；未匹配 security contract 或 loader gate 失败。API 后来 Ready，
  但 restartCount=1，因此驱动于00:33:24以 `lab Pod identity mismatch` 拒绝进入验收。
  00:34:08正常 drain、namespace/RBAC 清理和业务基线不变全部确认。随后独立 run
  `e8416437` 零重启完整通过，没有重用失败 Pod 或延长准备/启动预算。

前四个夹具失败和 worker-4 的首次失败 namespace 均经正常 release drain、exact Namespace UID 删除与 inventory RBAC GC
确认清理；两个失败 FUSE run 的固定三份测试文件在各自随机前缀内独立 DELETE，并认证 GET404。
只清理本轮固定测试文件，不声称完成业务存储全前缀对象审计；目录 marker 也不当成泄漏证明。
间隔重试能取得本次四类拒绝证据，但不证明生产集中审计零丢失，dmesg 不是完整审计输送渠道。

### 历史保留现场与续测收尾

9月16日失败时只保留 `sandbox-aa-api-17eaec31-4`，Namespace UID
`d3010735-8934-44df-8e42-269750fb0589`。自己的 API Deployment 已缩零；一个尚未确认终止的
普通 runtime、自己的 Redis/emptyDir、loader、失败 drain Job 和 inventory RBAC 保留。
没有创建 FUSE sandbox 或租户测试文件。私有本地 values已移除，端口转发未启动；报告不含凭据。

只读核对 worker-4 的 kubelet configz：serializeImagePulls=true，enableSystemLogHandler=true、
enableSystemLogQuery=false。现有日志接口不能查询系统 unit日志；没有启用节点日志查询或新增
nodes/proxy权限。使用严格 host key校验/BatchMode尝试已有 SSH 主机名，因主机名无法解析未连接。
进一步确定拉取与删除不完成的根因仍需要节点 SSH入口或脱敏 kubelet/containerd日志。
本轮事件和应用日志只证明卡在未启动 runtime的拉取/终止链路，不推断为 DNS、磁盘或 AppArmor故障。

最终追加只读核验时，本机 kubectl连接集群入口 `172.16.20.15:443` 已发生 i/o timeout，10秒请求/
15秒进程预算的检查也失败。业务 UID/template/imageID/restart/Ready不变的最后确认来自18:19:12
该轮 baseline比对，不声称网络中断后的即时健康或资源状态。自己的超时只读查询已停止。
当时须恢复本机到集群入口的网络，再重新核对保留现场；没有因连接失败去改集群或本机网络设置。

9月17日00:26重新连接成功，核验 Namespace UID 仍一致、worker-4 Ready、原 runtime Pod
已不存在、自己的 API replicas=0。失败 drain Job 的 UID 为
`744616a9-d126-432e-ac9b-bcda9617ce45`，确认终态且无 active 实例后，使用其同源模板
创建唯一恢复 Job `aa-api-17eaec31-api-drain-resume-faf29e`，核验自己的 namespace、
Deployment、ServiceAccount、state scope 和无重复 env；没有覆盖/删除旧 Job 或改镜像。
恢复 Job Complete、managed runtime/两类策略均为0后，才按原 Namespace UID 删除。
00:27:23确认 namespace 与 inventory RBAC 均消失，业务 UID/template/imageID/restart/Ready 不变。

使用读回的 worker-4 InternalIP 和现有 SSH 用户配置，只读查询 kubelet/containerd日志仍因
SSH连接超时失败；没有更换账号、跳过 host key、修改节点或扩大日志访问权限。继续验收的依据
是原 Pod 正常退出及 drain/精确清理恢复，不是节点冷拉取根因已修复。该根因保留为 COLD-01。

worker-5 本轮冷拉取事件记录：API 1.903秒、runtime 56.717秒、mounter 4.68秒；API 零重启。
这些单轮事件不构成长尾 SLO，也不能反推 worker-4 的 DNS、磁盘或注册表网络根因。

### 最终全局核验（9月17日00:43:40）

- 五份完成报告均包含全部必要 PASS 阶段、四类 exact profile 内核拒绝，且 cleaned=true；
  各成功轮次自己的私有 values 文件均已移除，报告不含凭据。
- `sandbox-aa-api-*` namespace 为0，对应 network-inventory ClusterRole/Binding 为0，
  自己的 kubectl port-forward 为0。失败轮次也已正常收尾，没有遗留实验 namespace。
- 业务 API 3/3、Sentinel 3/3、loader 5/5 Ready，所有容器 restartCount=0；各轮实际基线比对
  均确认 UID/template/imageID/restart/Ready 不变。最终业务对象 UID 如下：

| 业务对象 | UID |
| --- | --- |
| API Deployment | `845af585-f245-400f-9ebb-9ff307894eea` |
| Redis Sentinel StatefulSet | `374b274b-52ee-44fa-bb2a-bdd8f0e39693` |
| AppArmor loader DaemonSet | `8a87e7d5-b494-4772-b93e-ae1bc0e5fcd8` |

没有 force、手工清 finalizer、裸删状态、节点/业务重启或策略卸载。固定实验文件删除及独立
认证 GET404 均已确认，内核 profile 按设计保留。其它 CNI、HA 故障和 FUSE性能仍是独立未完成事项。

## 驱动与安全回归

主驱动默认 dry-run，不读 Secret或写集群。真实模式必须显式 context、execute、五节点之一；
不能接受业务 namespace。每个 namespace 带 run/purpose，记录 API 返回 UID，所有集群写入前
核验归属；Pod delete 和 namespace delete 使用 UID precondition。只创建必要 Chart 对象，
cluster inventory 仅原 get/list 权限且绑定 Namespace UID，由 GC 收尾。不 force 或手工剥除 finalizer。

临时配置0600、目录0700；AK/SK/API key 和错误 body不打印、不进Git，测试文件只在本轮随机
存储前缀。失败时先正常 API DELETE/drain；清理不确认则保留身份/阶段并停止下一节点。
成功清理后移除私有配置并停止自己的127.0.0.1端口转发，只保留非敏感 report。

```sh
node testdata/apparmor-enforcement/all-node-api.test.js
node --check testdata/apparmor-enforcement/all-node-api.js
node testdata/apparmor-enforcement/all-node-api.js
bash scripts/test-helm-apparmor-loader.sh
git diff --check

# 获准后逐节点执行，前一轮清理未确认不得继续
node testdata/apparmor-enforcement/all-node-api.js --execute --context ds-ai-research --node ds-ai-worker-1
```

安全回归先观察 RED，再 GREEN：错误 Namespace UID/run/purpose/业务命名空间、replacement Pod UID、
目标 nodeName 不符、越界 kind/delete URI 均在实际 create/exec/delete 入口拒绝，stub 下零副作用。
另验证 TLS/随机前缀、init sidecar digest、错误 profile/旧时间/不相关 PID、只读 open 不能算写拒绝、
内核日志水位和 S3 方法/path 越界拒绝。9月17日重新运行这些本地命令通过；现场通过依据为
上述逐节点真实 API、进程/挂载身份、持久化及内核证据，不以本地测试代替。
