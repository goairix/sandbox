# 可信 AppArmor 节点加载器

从仓库根目录构建两种 Linux 架构（本次实现没有构建或推送镜像）：

```sh
docker buildx build --platform linux/amd64,linux/arm64 -f docker/images/apparmor-loader/Dockerfile -t REGISTRY/sandbox-apparmor-loader:VERSION .
```

独立镜像包含 Go 加载器和 Debian `apparmor_parser`。它不是租户沙盒，也不访问 Kubernetes API。部署需可信 privileged 管理容器、`/sys/kernel/security` securityfs 的读写挂载、内核参数文件 `/sys/module/apparmor/parameters/enabled` 的只读挂载、只读 profile ConfigMap 和 `/run/apparmor-loader` 私有可写临时目录。不得挂载宿主根目录、宿主 `/proc`、containerd socket、业务 workspace 或密钥；不得启用 hostPID/hostNetwork；ServiceAccount 不挂载 API token、无集群写权限。Pod Security Admission 必须允许该专用管理组件的 privileged/hostPath。

启动参数为 `--profile-path`、`--profile-name`、`--profile-digest`、`--module-enabled-path`、`--profiles-path`、`--readiness-file`、`--check-interval`、`--parser-timeout`。`--proc-root` 默认 `/proc`，用于核对容器内加载器 PID 的启动身份，生产不要改为宿主 `/proc`。

摘要契约：将模板 CRLF 改为 LF、去掉两端空白、补一个末尾 LF。模板唯一声明为 `profile __SANDBOX_PROFILE_NAME__ flags=(attach_disconnected,mediate_deleted) {`；占位符只出现一次；整个自包含模板 SHA256 小写完整摘要生成 `sandbox-fuse-<64位摘要>`。最后替换名称，加载器按相同算法逆向核对。禁止外部 include、ABI 指令（例如 `abi <abi/4.0>,` 引用外部 feature metadata）、嵌套策略、complain 和策略切换。ABI 指令在策略声明前、内部及声明后均拒绝；注释和引号路径内的普通 ABI 文本不算指令。执行权限仅允许继承型 `ix`，不接受 unconfined、profile/subprofile 或 fallback 执行转换；该受限模板校验器也保守拒绝裸 `x`。引号路径和反斜杠转义按独立词法扫描核对，路径里的 `#` 不会隐藏后续规则。

启动首次检查执行固定 `apparmor_parser --add --skip-cache --skip-kernel-load` 语法检查；精确策略缺失时执行 `--add --skip-cache`，通过 stdin 传入已验证的不可变策略。已有同名 enforce 策略不调用替换或重新加载；已有 complain、内核未启用、读取失败、语法/加载失败保持 NotReady 并周期重试。稳定状态不重复执行 parser，但每次核对 ConfigMap 当前内容摘要和精确 kernel 名称/enforce 状态。所有 parser 调用有独立超时，不输出策略内容。

readinessProbe 应使用 exec，不直接检查文件存在：

```sh
/usr/local/bin/apparmor-loader readiness --readiness-file=/run/apparmor-loader/ready --max-age=30s
```

建议 `max-age` 略大于检查周期（默认周期 10s）。每次检查开始先删除旧成功标记，成功才原子发布带 PID/启动身份/时间的标记；失败、正常退出立即删除；即使 SIGKILL 留下标记，探针也拒绝已停止或 PID 复用的进程。退出和 Helm uninstall 不卸载策略。

信任边界：securityfs 的 profiles 列表只证明名称和模式，无法证明已有同名策略的规则内容；内容寻址命名依赖可信节点管理员不伪造或原地替换同名规则。加载器 Ready 不能证明 privileged mounter 实际受限，仍需 runtime exact profile/enforce 读回和真实 FUSE 操作、拒绝负例验收。本机 macOS 单元测试不是 Linux 内核 enforcement 验收。
