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

同时保留 API `startupProbe.enabled: true`（内置 Sentinel 本身也要求它）。不用手工安装节点 parser，也不用自己创建 profile ConfigMap、加载器 ServiceAccount/DaemonSet 或 API 读取 Role/RoleBinding：启用后均由 Helm 自动创建，profile 名称由 Chart 计算，不需要手填 `lsmProfile`。内核必须已启用 AppArmor，命名空间准入须允许这个可信 privileged/hostPath 组件；Helm 不会替你改变这些集群安全前提。

真实验收完成后，生产 values 才合并相同启用配置，并将 nodeSelector 改成生产中全部已验收节点的共同标签；不要将单个测试节点 hostname 原样复制上线，也不要仅为部署成功临时清空选择器扩到未验收节点。该 selector 同时约束普通池、FUSE 池和加载器，而不是只限制加载器。

同步 Chart 时需要完整保留 `files/apparmor/workspace-mounter.profile`，不能只复制 `templates/`；同时同步 `values.schema.json`，继续保留自己的 values。启用会变更安全/池契约，沿用 backend-change drain，可能销毁并重建池内沙盒，须安排维护窗口；不要在存在活动任务时盲目打开开关。

启用时自动合入 `kubernetes.io/os: linux`，显式 windows 冲突报错。选择器键和值须为字符串；YAML 的数字或布尔值必须加引号才能作为标签值。原生 `runtime.kubernetes.node_selector` 使用相同配置，环境变量 `SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR` 使用 JSON 字符串对象。

启用时 `config.workspace.lsmProfile` 的手工名称不再生效，使用 `sandbox-fuse-<策略摘要>`。完整摘要留在 annotation；label 使用前 63 位并同时核验完整 annotation，不把截断标签当作完整身份。策略或有效选择器改变会改变 backend fingerprint/池契约，沿用既有精确身份和排空流程。关闭时仍沿用手工 profile。

## 权限与可用性

节点须已启用 AppArmor 并可访问 securityfs。加载器无权改变内核启动参数，不假设所有 Linux 节点都有可用 AppArmor。DaemonSet 是可信节点管理组件，使用 privileged 和两个必要 hostPath：securityfs（策略加载需写入）及只读 enabled 文件；不挂载主机根目录、运行时 socket 或业务目录，不使用 hostPID/hostNetwork，不携带 Kubernetes API token。命名空间的 Pod Security Admission 必须由管理员允许该可信组件，不能因此扩大租户沙盒权限。

API 新增本 release 指定 DaemonSet 的 namespace get；跨运行时命名空间时，为核验 loader Pod 另增加 release namespace Pod get/list。加载器本身没有 Role。API 门禁核验 exact UID、当前 generation/模板和至少一个当前策略 Ready 实例，不依赖全节点都 Ready。所有目标节点覆盖仍须单独验收。

集群若配置了 globalDefault PriorityClass，必须显式设置 `apparmorLoader.priorityClassName` 为管理员确认的已存在类。默认空配置不会自动信任 admission 注入的其它类，也不自动授予 system-critical 优先级；否则严格模板门禁会超时。显式类仅允许 admission 补入其 priority/preemption 值，类名漂移和模板中已指定值的漂移仍被拒绝。

若加载器已 Ready，但 API 报 `AppArmor loader startup gate incomplete`，须同时检查加载器 DaemonSet 模板与所属 Pod，而不是只看 Ready 数。旧模板未设置 `enableServiceLinks`，Kubernetes 1.33 给实际 Pod 默认补入 `true`，会被严格模板比较判为不匹配。新版模板固定 `enableServiceLinks: false`，生成的 Pod 也保持 false；无需新增 values 配置或重建现有兼容镜像，只需同步新版 Chart 后重新 upgrade。不要为此关闭安全校验、回滚 release 或删除内置 Sentinel 的身份 Secret/PVC。字段被准入组件改写等其它不匹配仍会被拒绝。

运行时在 prepared 入池及授权前增加一次私有 Kubernetes Exec，读取固定 `/proc/1/attr/current`，联合复核 UID、零重启及安全设置；有独立 5 秒上限，无凭据输入，不向租户 Exec 开放。该开销需要实际压测，不承诺零成本。

既有授权通道仍是按 Pod 名称发起 exec；读取前后的 UID 校验关闭的是读取窗口，并不能为后续 exec 提供 Kubernetes 不支持的 UID 前置条件。可信 supervisor 会再次核对授权 UID 并拒绝 replacement，但不能宣称凭据绝不会进入此窗口中的同名可信 replacement。

加载器退出、滚动更新或 uninstall 不卸载内核 profile；旧进程和其它 release 可能仍使用它。旧版本的清理由节点管理员另行审计，不用卸载 hook 自动回收。

语法与继承执行规则参考 [Debian AppArmor 策略手册](https://manpages.debian.org/trixie/apparmor/apparmor.d.5.en.html)。
