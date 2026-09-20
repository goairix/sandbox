# 可选 AppArmor 加载器

## 当前发布边界

加载器核心、Chart、API 启动门禁与 FUSE 私有约束检查已接入，默认关闭。随 Chart 分发的策略已在下述 `ds-ai-research` 版本组合完成五节点真实验收；其它组合仍须单独验收，单测、Helm 渲染和 parser 不加载内核的语法检查不能代替生产验收。其它容器运行时可能忽略 privileged 容器的 profile，本项目会拒绝将这类 mounter 入池或授权，不自动降级为 unconfined。

启用前必须在隔离目标节点完成：加载器 enforce、mounter/PID 1 及 s3fs 子进程实际 profile、对象存储挂载、写入/flush/卸载、拒绝写入非允许路径、拒绝任意 mount、loader Pod 重建恢复及最终清理。未完成这些步骤时不要直接打开生产开关；节点冷启动或策略丢失后的恢复须另外验收，不能用 Pod 重建代替。

2026-09-14，`v0.3.24-arm64` 已在 `ds-ai-worker-2` 通过上述加载、MinIO TLS 挂载链路、拒绝测试及加载器 Pod 重建恢复，见 [真实验收记录](../testing/2026-09-14-apparmor-live-prepared-results.md)。该组件测试结束时线上 release 尚未启用；此结果不覆盖业务节点重启、其它节点/运行时、其它存储及启用后的公共 API/跨副本集成。现有加载器、API、mounter 镜像无需因这次测试记录重新构建。后续线上启用发现的启动门禁兼容问题另见 [门禁修复记录](../testing/2026-09-14-apparmor-loader-startup-gate-results.md)。

2026-09-17 续测完成：worker-1 至 worker-5 的普通/FUSE 公共 API、真实 s3fs enforce、四类内核拒绝、自己的 loader Pod 重建及正常 drain / 精确清理全部通过。worker-4 历史失败实验已正常清理，业务基线未变，见 [逐节点结果与续测记录](../testing/2026-09-16-all-node-apparmor-api-results.md)。没有修改业务 release 或策略，本次文档更新不要求重新构建镜像或升级 Chart。冷拉取/准备长尾仍未解决；结果不覆盖其它版本组合、节点冷启动、Redis HA 故障或其它 CNI。

## 构建及推送镜像

本功能新增 `sandbox-apparmor-loader` 镜像，并要求 `sandbox-api` 包含加载器启动门禁和私有 enforce 检查。如果已部署的 API 就是从包含这些实现的新版代码构建的，可以复用，不必再构建一次；否则按下面命令同时重建 API。mounter/probe 不因为这项功能重建，Redis 镜像也不用更换。

所有命令在仓库根目录执行，不能进入 `docker/images/apparmor-loader` 后将该目录作为构建上下文。先确认当前代码版本包含本功能；不要从尚未合入实现的旧 main 构建。下面使用当前仓库地址，版本号是示例：首次加载器可用 `v0.1.0`，后续有代码变更须使用新 tag，不能覆盖已发布版本。

### 1. 准备构建环境

```sh
# 在已检出目标代码版本的仓库中执行
cd "$(git rev-parse --show-toplevel)"
git branch --show-current
git rev-parse --short HEAD

# 登录构建/推送使用的镜像仓库；不要把密码写进命令或文档
docker login registry.i.huaxisy.com
docker buildx version

# 首次创建专用 builder；同名 builder 已存在时跳过 create，不要删除它
docker buildx create --name sandbox-apparmor-build --driver docker-container
docker buildx inspect sandbox-apparmor-build --bootstrap

# 后续命令在同一个 shell 中执行；API tag 按自己的发布版本替换
SANDBOX_BUILD_REGISTRY=registry.i.huaxisy.com/library/ai-infra
SANDBOX_APPARMOR_TAG=v0.1.0
SANDBOX_API_TAG=v0.3.24-apparmor.1
SANDBOX_BUILD_PLATFORMS=linux/amd64,linux/arm64
```

`ds-ai-research` 当前节点为 arm64，镜像必须包含 `linux/arm64`，不能只构建 amd64。双架构镜像也适用于其它 amd64 集群；若这次只构建当前 arm64 集群，可将 `SANDBOX_BUILD_PLATFORMS` 改为 `linux/arm64`。先核对 builder 的 `Platforms` 支持所选架构；缺少跨架构执行支持时改用已配置的 builder/对应原生构建节点，不在业务节点临时运行 privileged 模拟器安装命令。构建机的 Docker 登录不等于 Kubernetes 节点已获得私有仓库的拉取权限，节点仍须具备原有仓库访问条件。

### 2. 构建并推送加载器

```sh
docker buildx build \
  --builder sandbox-apparmor-build \
  --platform "$SANDBOX_BUILD_PLATFORMS" \
  --file docker/images/apparmor-loader/Dockerfile \
  --tag "$SANDBOX_BUILD_REGISTRY/sandbox-apparmor-loader:$SANDBOX_APPARMOR_TAG" \
  --push .
```

加载器镜像包含 Debian `apparmor_parser` 和静态 Go 程序。这里的 `--push` 会直接推送镜像及架构清单到仓库，不需要再执行 `docker push`；不能只省略输出选项并以为构建结果已经能被集群拉取。多架构发布流程参考 [Docker 官方说明](https://docs.docker.com/build/building/multi-platform/)。

### 3. 按需构建并推送 API

仅当当前 API 尚未包含本功能，或本轮 API 代码又发生变化时执行：

```sh
docker buildx build \
  --builder sandbox-apparmor-build \
  --platform "$SANDBOX_BUILD_PLATFORMS" \
  --file docker/Dockerfile \
  --tag "$SANDBOX_BUILD_REGISTRY/sandbox-api:$SANDBOX_API_TAG" \
  --push .
```

当前此 Dockerfile 只打包 `/app/sandbox`；Redis helper 已拆到 `docker/images/redis-bootstrap/Dockerfile`，内置 Sentinel 应在 `redis.sentinel.bootstrapImage` 配置独立镜像，不再引用 API 镜像。复用 API 时，下面 values 中的 `image.tag` 保留该已验证镜像的版本，不填写示例新 tag。

### 4. 核对仓库中的镜像

```sh
docker buildx imagetools inspect \
  "$SANDBOX_BUILD_REGISTRY/sandbox-apparmor-loader:$SANDBOX_APPARMOR_TAG"

# 如果按第 3 步重建了 API，再核对它；复用时改为实际已有 tag
docker buildx imagetools inspect \
  "$SANDBOX_BUILD_REGISTRY/sandbox-api:$SANDBOX_API_TAG"
```

双架构构建的输出应包含 `linux/amd64` 和 `linux/arm64`；单架构构建应包含所选目标架构。记下实际 tag 和 digest，后续 values 必须与已推送镜像一致。以上是部署者执行的命令，不是本文已经构建、推送或完成真实内核验收的记录。

## values 配置

保留自定义 `values.yaml` 的部署不必补齐未启用组件的全部字段。关闭加载器只需：

```yaml
apparmorLoader:
  enabled: false
```

省略整个配置也默认关闭。模板会为缺失或 `null` 的字段补齐 Chart 默认值：检查周期 10 秒、parser 超时 10 秒、API 等待超时 180 秒；资源请求为 `25m/32Mi`，上限为 `250m/128Mi`。默认镜像与 Chart 的 `values.yaml` 一致，启用前仍应显式填写自己已构建、推送的可信镜像仓库和版本。显式配置不会被默认值覆盖；非法类型、0 超时、空镜像名称仍会在渲染阶段拒绝。同步更新时须同时同步 `templates/` 和 `values.schema.json`，无需用仓库的 `values.yaml` 覆盖线上密码及其它配置。

### 构建后先准备配置，不立即启用线上加载器

在自己的 `values.yaml` 中合并下面字段，不要重复增加第二个 `image` 或 `apparmorLoader` 顶层块。YAML 不会展开上面的 shell 变量，必须填写实际版本：

```yaml
image:
  repository: registry.i.huaxisy.com/library/ai-infra/sandbox-api
  tag: v0.3.24-apparmor.1 # 仅重建 API 时修改；复用则保留当前已验证 tag

apparmorLoader:
  enabled: false # 这里只准备镜像配置，尚不加载节点策略
  image:
    repository: registry.i.huaxisy.com/library/ai-infra/sandbox-apparmor-loader
    tag: v0.1.0 # 与实际推送版本一致
    pullPolicy: IfNotPresent
```

此阶段保留现有 workspace、Redis、网络、namespace 和 nodeSelector 配置，不要提前关闭测试环境已有的 `allowMissingLSMForKind` 或更改手工 `lsmProfile`，否则加载器仍关闭时可能因缺少已加载策略而启动失败。生产环境不得依赖该测试绕过开关。

### 获准的隔离环境：开启加载器并进行真实验收

下面是与隔离测试环境自身 values 合并的启用片段，不能直接作为完整 values 安装，也不能覆盖线上配置。对象存储、Secret、网络和运行时 namespace 必须指向该隔离环境，不能沿用线上业务目标。这里选择 `ds-ai-worker-2` 是当前测试节点示例，其它集群须换成已确认支持 AppArmor 的节点。

```yaml
apparmorLoader:
  enabled: true # 仅在获准的隔离节点先验收，线上暂不打开
  priorityClassName: "" # 有 globalDefault 时填写管理员确认的既有可信类
  image:
    repository: registry.i.huaxisy.com/library/ai-infra/sandbox-apparmor-loader
    tag: v0.1.0
    pullPolicy: IfNotPresent
  checkIntervalSeconds: 10
  parserTimeoutSeconds: 10
  startupTimeoutSeconds: 180
  resources:
    requests:
      cpu: 25m
      memory: 32Mi
    limits:
      cpu: 250m
      memory: 128Mi

config:
  runtime:
    type: kubernetes
    kubernetes:
      namespace: "" # 使用隔离 release 自己的命名空间，不填写线上 namespace
      nodeSelector:
        kubernetes.io/hostname: ds-ai-worker-2
  workspace:
    enabledMountModes: [sync, fuse]
    allowMissingLSMForKind: false # 加载器启用时必须为 false
```

同时保留 API `startupProbe.enabled: true`（内置 Sentinel 本身也要求它）。profile ConfigMap、加载器 ServiceAccount/DaemonSet 和 API 读取 Role/RoleBinding 均由 Helm 自动创建，profile 名称由 Chart 计算，不需要手填 `lsmProfile` 或手工创建项目资源。加载器使用**镜像内**的 parser 加载策略，不安装宿主机软件，也不修改节点 CRI 配置。内核启用、CRI 支持及命名空间准入是另外的集群前提，具体检查见下一节。

真实验收完成后，生产 values 才合并相同启用配置，并将 nodeSelector 改成生产中全部已验收节点的共同标签；不要将单个测试节点 hostname 原样复制上线，也不要仅为部署成功临时清空选择器扩到未验收节点。该 selector 同时约束普通池、FUSE 池和加载器，而不是只限制加载器。

同步 Chart 时需要完整保留 `files/apparmor/workspace-mounter.profile`，不能只复制 `templates/`；同时同步 `values.schema.json`，继续保留自己的 values。启用会变更安全/池契约，沿用 backend-change drain，可能销毁并重建池内沙盒，须安排维护窗口；不要在存在活动任务时盲目打开开关。

启用时自动合入 `kubernetes.io/os: linux`，显式 windows 冲突报错。选择器键和值须为字符串；YAML 的数字或布尔值必须加引号才能作为标签值。原生 `runtime.kubernetes.node_selector` 使用相同配置，环境变量 `SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR` 使用 JSON 字符串对象。

启用时 `config.workspace.lsmProfile` 的手工名称不再生效，使用 `sandbox-fuse-<策略摘要>`。完整摘要留在 annotation；label 使用前 63 位并同时核验完整 annotation，不把截断标签当作完整身份。策略或有效选择器改变会改变 backend fingerprint/池契约，沿用既有精确身份和排空流程。关闭时仍沿用手工 profile。

## 节点前提：loader Ready 不等于 CRI 支持

[Kubernetes 官方前提](https://kubernetes.io/docs/tutorials/security/apparmor/) 分别要求内核启用 AppArmor、容器运行时支持以及目标 profile 已加载。最低 Kubernetes 1.29 的注解兼容不改变这些节点前提；不能因为内核版本较新或 loader Ready 就判断真实 FUSE 已可用。

上游 [containerd 1.7.29 CRI 检查](https://github.com/containerd/containerd/blob/v1.7.29/pkg/cri/server/helpers_linux.go) 还检查 `DisableApparmor` 配置与 host 支持；[host 检查实现](https://github.com/containerd/containerd/blob/v1.7.29/pkg/apparmor/apparmor_linux.go) 检查宿主机 parser 路径、securityfs、enabled 状态等，并缓存首次结果。因此镜像内 parser 能成功加载内核策略，不代表宿主机 `/sbin/apparmor_parser` 已满足 CRI 的检查；这也不能直接证明 CCE 厂商补丁版本具体是哪一项失败。

平台管理员先只读核查：

- 节点内核 AppArmor 启用状态和 securityfs 是否可访问。
- 实际 CRI 版本、有效配置中的 AppArmor 禁用项，以及该版本要求的宿主机 parser 路径是否存在。
- kubelet/CRI 的能力或错误记录；Kubernetes feature gate、loader Ready 都不能代替 CRI 能力证明。
- profile 已加载并为 enforce；随后在获准隔离环境验证真实 mounter/PID 1 与 s3fs 子进程 profile、挂载、拒绝测试和清理。

CCE 现场曾在 loader Ready 后由 CRI 拒绝创建 mounter，见 [v0.3.33 现场报告](../testing/2026-09-17-cce-v033-live-validation.md)。这种节点前提缺口不是增加 Helm values 可以修复的。本项目不会安装宿主机 parser、修改 containerd 或自动重启节点服务；若涉及运行时能力缓存，须由平台方评估维护窗口，不要直接重启业务 CRI，也不要临时挂载 runtime socket/主机根目录或关闭 LSM 绕过验证。

2026-09-17 代码整改修复了身份 PVC 绑定竞态和未启动 FUSE Pod 的正常清理流程，但未解决 CCE 的 CRI 前提。本轮需重建 API 与 Redis bootstrap，加载器/mounter 可沿用，详见 [构建与部署说明](helm-deployment-upgrade.md#本次-cce-身份绑定与未启动-fuse-清理整改)。

用户提供 API `v0.3.34` 与 bootstrap `v0.3.1` 后，隔离现场确认首次身份/初始化和正常卸载通过，但已加载 enforce profile 的节点仍被 CRI 以 `apparmor is not supported` 拒绝创建真实 FUSE 容器；测试资源已清理。见 [最新现场报告](../testing/2026-09-17-cce-v034-live-validation.md)。这不是完整 API/AppArmor 验收通过，也不要求新增 Helm values。

### CCE 发行版与节点操作系统：另行核实，不能套用通用内核结论

截至 2026-09-17 查阅的 [华为公有云 CCE AppArmor 文档](https://support.huaweicloud.com/usermanual-cce/cce_10_1006.html) 将支持范围限定为集群 v1.31.6-r0 及以上、Ubuntu 节点，并要求 Everest 2.4.158 及以上；还要求在节点和容器运行时分别启用 AppArmor。该平台限制不改变本项目 Kubernetes 1.29 的最低支持，也不意味着通用 Kubernetes 1.29 不能使用 AppArmor。

当前双华云 CCE 测试节点实际为 Huawei Cloud EulerOS 2.0，Everest 镜像为 2.5.28。不能因 Kubernetes/插件版本达到上述门槛就忽略节点 OS，也不能未经私有云平台确认就把公有云支持范围等同于双华云发行版。项目不按 OS 名称自动关闭 LSM、切换 SELinux 或加入 Ubuntu 硬编码白名单；保持现有安全契约与其它已验证集群行为。

下一步是由平台确认该发行版/节点 OS 的支持范围，并读取**正在运行的 CRI** 的 AppArmor 禁用配置、宿主机 parser 和 host 检测前提。Kubelet configz 不包含 containerd 的有效配置，loader 镜像内的 parser 也不是宿主机 parser。不要只凭静态 config.toml 或 `containerd config dump` 推断运行中 CRI 已采用该值；导入配置、启动配置路径与能力缓存均可能影响结果。

若平台管理员已有节点访问方式、`crictl` 与 `jq`，可先在目标节点做以下只读检查；不要发送完整 `crictl info`、配置文件、环境或日志，其中可能包含私有连接信息。命令或字段不可用表示证据不足，不按 false/通过处理，也不要为诊断自行安装软件：

```bash
sudo crictl info | jq -e 'if (.config.disableApparmor | type) == "boolean" then {disableApparmor: .config.disableApparmor} else error("CRI AppArmor config unavailable") end'
test -e /sbin/apparmor_parser && printf 'host-parser-present\n' || printf 'host-parser-missing\n'
test -x /sbin/apparmor_parser && printf 'host-parser-executable\n' || printf 'host-parser-not-executable\n'
cat /sys/module/apparmor/parameters/enabled
```

2026-09-17 用户确认生产目标仍为 EulerOS 2.0，另行批准了两个非 privileged 临时只读诊断 Pod；已在两原测试节点核实：内核 enabled=Y、profile enforce，但宿主机 parser 缺失、静态 containerd 配置显式禁用 AppArmor。诊断资源已清理；运行中 CRI 的有效配置尚未查询。详细权限边界、实测与后续要求见 [宿主机诊断报告](../testing/2026-09-17-cce-euleros-apparmor-host-diagnostics.md)。不要求改为 Ubuntu，也不扩大已有 loader 权限。

该只读授权不包括安装宿主机软件、修改 CRI 配置或重启服务。临时诊断仅在获准的精确节点/挂载范围执行，不挂载主机根目录或 runtime socket；不能创建伪 parser 文件骗过检测，也不能直接把 Debian loader 镜像的 parser 复制到 EulerOS 上。EulerOS 包来源与 ABI/依赖兼容、私有云发行版支持和维护窗口须先确认，另获节点变更授权后才实施；之后仍需真实 FUSE enforce/拒绝及正常清理验收，不把静态配置或 loader Ready 当作通过。

### EulerOS 2.0 测试节点的受控整改步骤

下面是给平台管理员执行的**单节点测试 runbook**，不是 Helm 操作，也不是生产授权。现有 HCE amd64/arm64 包均是未签名的测试产物，只能用于已获准的测试节点；生产必须由平台方重新审计依赖、完成全部测试、用内部签名密钥签名，并通过双华云对 EulerOS 2.0/CRI 组合的支持确认。arm64 必须使用单独构建并审计的 `aarch64` RPM，不能把 amd64 包复制过去。arm64 节点原生构建与真实 API 验证见[现场记录](../testing/2026-09-20-hce-arm64-apparmor-validation.md)。

#### 1. 安装前检查和包校验

将经过制品库/签名流程发布的 RPM 放到节点临时目录。不要从其它节点直接复制已安装文件，也不要用 `--force`、`--nodeps` 或伪造 `/sbin/apparmor_parser`。

```bash
set -euo pipefail
pkg=/var/tmp/sandbox-apparmor-parser-4.1.7-1.$(uname -m).rpm
test -f "$pkg"

# 测试构建产物的摘要；生产应替换为签名制品的批准摘要
sha256sum "$pkg"
# 当前 x86_64 测试包：766cb1ac45bd13e5643c2dd380c326ca2c6a111275bdd2937d2550baefa58ee3
# 当前 aarch64 测试包：7ca6f7fa127d424a2e3a2089bf00da7cd60a49806b6e7744ed3608466301d912

rpm -K "$pkg"
rpm -qp --qf 'name=%{NAME} version=%{VERSION}-%{RELEASE} arch=%{ARCH}\n' "$pkg"
rpm -qp --requires "$pkg"
test "$(rpm -qp --qf '%{ARCH}' "$pkg")" = "$(uname -m)"
```

`rpm -K` 必须显示内部签名校验通过后才能进入生产；当前测试包只具备完整性摘要，不是生产签名包。依赖须先由平台管理员按 RPM 输出审核并从受信任 HCE 源解决。

#### 2. 安装真实宿主机 parser

```bash
sudo rpm --test -i "$pkg"
sudo rpm -i "$pkg"
sudo rpm -V sandbox-apparmor-parser
sudo test -x /usr/sbin/apparmor_parser
sudo /usr/sbin/apparmor_parser --config-file=/dev/null --version
test -L /sbin && readlink -f /sbin/apparmor_parser
```

该包只提供 `/usr/sbin/apparmor_parser` 和许可证文件；HCE 节点的 `/sbin` 应解析到 `/usr/sbin`。如果已有同名发行版包、路径不是该 RPM 提供的 ELF、`rpm -V` 不通过或版本命令失败，立即停止，不覆盖现有包。

#### 3. 备份并修改 containerd 的有效配置

先确认双华云节点配置是否由节点模板/配置管理系统托管。若是，必须在那个持久化入口修改；只改 `/etc/containerd/config.toml` 可能会被平台回写。

```bash
stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup="/var/tmp/containerd-config.toml.$stamp.bak"
sudo cp -a /etc/containerd/config.toml "$backup"
sudo sha256sum /etc/containerd/config.toml "$backup"
sudo vi /etc/containerd/config.toml
```

在 `[plugins."io.containerd.grpc.v1.cri"]` 段中，将唯一的配置项改为：

```toml
disable_apparmor = false
```

不要修改其它 CRI、CNI、sandbox image、registry、cgroup 或 snapshotter 配置。保存后先做语法检查（命令不可用或失败就停止）：

```bash
sudo containerd --config /etc/containerd/config.toml config dump >/dev/null
```

#### 4. 受控重启和运行态验证

containerd 重启会影响该节点上的容器创建/重启；生产必须先走节点维护、容量、PDB、排空和业务通知流程。单节点测试集群不要盲目执行 `kubectl drain`，先确认测试工作负载可以停机。

```bash
sudo systemctl restart containerd
sudo systemctl is-active --quiet containerd

# 必须看运行中的 CRI，不用静态文件或 containerd dump 代替
sudo crictl info | jq -e '.config.disableApparmor == false'
test "$(cat /sys/module/apparmor/parameters/enabled)" = Y
test -x /sbin/apparmor_parser
kubectl get node <目标节点> -o wide
```

containerd 的 AppArmor 能力检查在启动后缓存，因此只改配置而不重启不能作为通过。`crictl` 仍为 `true`、字段缺失、节点不 Ready、parser 路径失效或 kubelet/CRI 报错时，停止现场验收，执行回退。

#### 5. 回退

```bash
sudo cp -a "$backup" /etc/containerd/config.toml
sudo systemctl restart containerd
sudo systemctl is-active --quiet containerd
sudo crictl info | jq -e '.config.disableApparmor == true'
```

回退只恢复 containerd 配置，不自动卸载 RPM；parser 保留不会改变业务 profile。确认没有其它组件依赖该测试包后，平台管理员才可另行执行 `sudo rpm -e sandbox-apparmor-parser`，并再次确认 `/sbin/apparmor_parser` 的状态。若节点模板会重写配置，必须同步恢复模板，否则下次节点重启会再次改变结果。

#### 6. 通过前的真实验证

上述步骤只证明 CRI 前置条件具备，仍不能证明 FUSE 可用。之后必须在独立临时 namespace、固定测试节点和唯一 profile 名称下验证：profile enforce 身份、FUSE 挂载/读写/flush/卸载、预期 AF_UNIX 拒绝、普通和 FUSE 沙盒销毁，以及无 Pod/NetworkPolicy/PVC/挂载遗留。任一项失败，正式 Helm profile 和生产节点保持不变。

## 权限与可用性

节点须已启用 AppArmor 并可访问 securityfs。加载器无权改变内核启动参数，不假设所有 Linux 节点都有可用 AppArmor。DaemonSet 是可信节点管理组件，使用 privileged 和两个必要 hostPath：securityfs（策略加载需写入）及只读 enabled 文件；不挂载主机根目录、运行时 socket 或业务目录，不使用 hostPID/hostNetwork，不携带 Kubernetes API token。命名空间的 Pod Security Admission 必须由管理员允许该可信组件，不能因此扩大租户沙盒权限。

API 新增本 release 指定 DaemonSet 的 namespace get；跨运行时命名空间时，为核验 loader Pod 另增加 release namespace Pod get/list。加载器本身没有 Role。API 门禁核验 exact UID、当前 generation/模板和至少一个当前策略 Ready 实例，不依赖全节点都 Ready。所有目标节点覆盖仍须单独验收。

集群若配置了 globalDefault PriorityClass，必须显式设置 `apparmorLoader.priorityClassName` 为管理员确认的已存在类。默认空配置不会自动信任 admission 注入的其它类，也不自动授予 system-critical 优先级；否则严格模板门禁会超时。显式类仅允许 admission 补入其 priority/preemption 值，类名漂移和模板中已指定值的漂移仍被拒绝。

若加载器已 Ready，但 API 报 `AppArmor loader startup gate incomplete`，须同时检查加载器 DaemonSet 模板与所属 Pod，而不是只看 Ready 数。旧模板未设置 `enableServiceLinks`，Kubernetes 1.33 给实际 Pod 默认补入 `true`，会被严格模板比较判为不匹配。新版模板固定 `enableServiceLinks: false`，生成的 Pod 也保持 false；无需新增 values 配置或重建现有兼容镜像，只需同步新版 Chart 后重新 upgrade。不要为此关闭安全校验、回滚 release 或删除内置 Sentinel 的身份 Secret/PVC。字段被准入组件改写等其它不匹配仍会被拒绝。

运行时在 prepared 入池及授权前增加一次私有 Kubernetes Exec，读取固定 `/proc/1/attr/current`，联合复核 UID、零重启及安全设置；有独立 5 秒上限，无凭据输入，不向租户 Exec 开放。该开销需要实际压测，不承诺零成本。

既有授权通道仍是按 Pod 名称发起 exec；读取前后的 UID 校验关闭的是读取窗口，并不能为后续 exec 提供 Kubernetes 不支持的 UID 前置条件。可信 supervisor 会再次核对授权 UID 并拒绝 replacement，但不能宣称凭据绝不会进入此窗口中的同名可信 replacement。

加载器退出、滚动更新或 uninstall 不卸载内核 profile；旧进程和其它 release 可能仍使用它。旧版本的清理由节点管理员另行审计，不用卸载 hook 自动回收。

语法与继承执行规则参考 [Debian AppArmor 策略手册](https://manpages.debian.org/trixie/apparmor/apparmor.d.5.en.html)。
