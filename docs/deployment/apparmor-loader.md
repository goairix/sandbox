# 可选 AppArmor 加载器

## 当前发布边界

加载器核心、Chart、API 启动门禁与 FUSE 私有约束检查已接入，默认关闭。随 Chart 分发的策略仍是待真实挂载验收的候选；单测、Helm 渲染和 parser 不加载内核的语法检查不能代替生产验收。其它容器运行时可能忽略 privileged 容器的 profile，本项目会拒绝将这类 mounter 入池或授权，不自动降级为 unconfined。

启用前必须在隔离目标节点完成：加载器 enforce、mounter/PID 1 及 s3fs 子进程实际 profile、对象存储挂载、写入/flush/卸载、拒绝写入非允许路径、拒绝任意 mount、重启重载及最终清理。未完成这些步骤时不要直接打开生产开关。

## 镜像

需要重建 sandbox-api，并新增 sandbox-apparmor-loader 镜像。mounter/probe 不因为这项功能重建。加载器包含固定 `apparmor_parser` 和静态 Go 程序，构建上下文为仓库根目录：

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -f docker/images/apparmor-loader/Dockerfile \
  -t YOUR_REGISTRY/sandbox-apparmor-loader:YOUR_VERSION .
```

镜像构建及推送由部署者统一执行；本文命令不是已执行记录。

## values 配置

```yaml
apparmorLoader:
  enabled: false # 隔离验收通过后再设为 true
  priorityClassName: "" # 有 globalDefault 时填写已存在的可信 PriorityClass
  image:
    repository: YOUR_REGISTRY/sandbox-apparmor-loader
    tag: YOUR_VERSION
    pullPolicy: IfNotPresent
  checkIntervalSeconds: 10
  parserTimeoutSeconds: 10
  startupTimeoutSeconds: 180

config:
  runtime:
    kubernetes:
      nodeSelector: {} # 普通/FUSE 沙盒与加载器共用，可指定经过验收的节点标签
  workspace:
    enabledMountModes: [sync, fuse]
    allowMissingLSMForKind: false
```

启用时自动合入 `kubernetes.io/os: linux`，显式 windows 冲突报错。选择器键和值须为字符串；YAML 的数字或布尔值必须加引号才能作为标签值。原生 `runtime.kubernetes.node_selector` 使用相同配置，环境变量 `SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR` 使用 JSON 字符串对象。

启用时 `config.workspace.lsmProfile` 的手工名称不再生效，使用 `sandbox-fuse-<策略摘要>`。完整摘要留在 annotation；label 使用前 63 位并同时核验完整 annotation，不把截断标签当作完整身份。策略或有效选择器改变会改变 backend fingerprint/池契约，沿用既有精确身份和排空流程。关闭时仍沿用手工 profile。

## 权限与可用性

节点须已启用 AppArmor 并可访问 securityfs。加载器无权改变内核启动参数，不假设所有 Linux 节点都有可用 AppArmor。DaemonSet 是可信节点管理组件，使用 privileged 和两个必要 hostPath：securityfs（策略加载需写入）及只读 enabled 文件；不挂载主机根目录、运行时 socket 或业务目录，不使用 hostPID/hostNetwork，不携带 Kubernetes API token。命名空间的 Pod Security Admission 必须由管理员允许该可信组件，不能因此扩大租户沙盒权限。

API 新增本 release 指定 DaemonSet 的 namespace get；跨运行时命名空间时，为核验 loader Pod 另增加 release namespace Pod get/list。加载器本身没有 Role。API 门禁核验 exact UID、当前 generation/模板和至少一个当前策略 Ready 实例，不依赖全节点都 Ready。所有目标节点覆盖仍须单独验收。

集群若配置了 globalDefault PriorityClass，必须显式设置 `apparmorLoader.priorityClassName` 为管理员确认的已存在类。默认空配置不会自动信任 admission 注入的其它类，也不自动授予 system-critical 优先级；否则严格模板门禁会超时。显式类仅允许 admission 补入其 priority/preemption 值，类名漂移和模板中已指定值的漂移仍被拒绝。

运行时在 prepared 入池及授权前增加一次私有 Kubernetes Exec，读取固定 `/proc/1/attr/current`，联合复核 UID、零重启及安全设置；有独立 5 秒上限，无凭据输入，不向租户 Exec 开放。该开销需要实际压测，不承诺零成本。

既有授权通道仍是按 Pod 名称发起 exec；读取前后的 UID 校验关闭的是读取窗口，并不能为后续 exec 提供 Kubernetes 不支持的 UID 前置条件。可信 supervisor 会再次核对授权 UID 并拒绝 replacement，但不能宣称凭据绝不会进入此窗口中的同名可信 replacement。

加载器退出、滚动更新或 uninstall 不卸载内核 profile；旧进程和其它 release 可能仍使用它。旧版本的清理由节点管理员另行审计，不用卸载 hook 自动回收。

语法与继承执行规则参考 [Debian AppArmor 策略手册](https://manpages.debian.org/trixie/apparmor/apparmor.d.5.en.html)。
